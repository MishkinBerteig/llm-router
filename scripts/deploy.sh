#!/usr/bin/env bash
# Build llm-router for Apple silicon and deploy it to a macOS host where it
# runs as a launchd LaunchAgent. Atomic swap, health check, automatic rollback.
#
# Settings come from the environment or from a gitignored .deploy.env at the
# repo root:
#
#   DEPLOY_HOST    ssh destination, e.g. user@studio.local          (required)
#   DEPLOY_PATH    binary path on the host, relative to its $HOME    (default: bin/llm-router)
#   LAUNCHD_LABEL  the LaunchAgent's label                           (default: local.llm-router)
#   HEALTH_URL     checked on the host after restart                 (default: http://127.0.0.1:12345/healthz)
#
# The LaunchAgent's ProgramArguments must point at $HOME/$DEPLOY_PATH.
# The config file (tiers.json) lives on the host and is never touched here.
set -euo pipefail

cd "$(dirname "$0")/.."
if [[ -f .deploy.env ]]; then
  # shellcheck disable=SC1091
  source ./.deploy.env
fi

: "${DEPLOY_HOST:?set DEPLOY_HOST, e.g. in .deploy.env}"
DEPLOY_PATH="${DEPLOY_PATH:-bin/llm-router}"
LAUNCHD_LABEL="${LAUNCHD_LABEL:-local.llm-router}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:12345/healthz}"

if [[ -n "$(git status --porcelain)" ]]; then
  echo "warning: working tree has uncommitted changes; deploying them anyway" >&2
fi

echo "==> vet and build (darwin/arm64)"
go vet ./...
mkdir -p build
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o build/llm-router-darwin-arm64 .

echo "==> copy to ${DEPLOY_HOST}:~/${DEPLOY_PATH}.new"
ssh "$DEPLOY_HOST" "mkdir -p \"\$(dirname '$DEPLOY_PATH')\""
scp -q build/llm-router-darwin-arm64 "${DEPLOY_HOST}:${DEPLOY_PATH}.new"

echo "==> swap, restart ${LAUNCHD_LABEL}, health check"
ssh "$DEPLOY_HOST" bash -s -- "$DEPLOY_PATH" "$LAUNCHD_LABEL" "$HEALTH_URL" <<'REMOTE'
set -euo pipefail
path="$1"; label="$2"; health="$3"
cd "$HOME"
chmod 755 "$path.new"
[[ -f "$path" ]] && cp -p "$path" "$path.prev"
mv "$path.new" "$path"

healthy() {
  for _ in $(seq 1 30); do
    curl -fsS --max-time 2 "$health" >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  return 1
}

launchctl kickstart -k "gui/$(id -u)/$label"
if healthy; then
  echo "healthy: $health"
  exit 0
fi

echo "health check FAILED; rolling back" >&2
if [[ -f "$path.prev" ]]; then
  mv "$path.prev" "$path"
  launchctl kickstart -k "gui/$(id -u)/$label"
  healthy && echo "rolled back to previous binary; healthy" >&2
fi
exit 1
REMOTE

echo "==> deployed $(git rev-parse --short HEAD) to ${DEPLOY_HOST}"
