# llm-router: agent notes

One OpenAI-compatible endpoint that routes capability tiers (`amazing`, `great`,
`fast`, `instant`) to local LLM backends. Single file (`main.go`), Go standard
library only. Keep it that way: no third-party dependencies.

**This is a public repo.** Never commit hostnames, IP addresses, domains, keys,
usernames or real people's names. Host-specific settings live in the gitignored
`.deploy.env` and in the host's own `tiers.json`.

## Develop
- `go vet ./... && go build .`
- No unit tests yet. To smoke-test, run against a stub backend that echoes
  `model`. Check auth (401), the tier rewrite, `model_substituted`, and
  `backend_unreachable`.

## Deploy
- `scripts/deploy.sh` builds for darwin/arm64, copies the binary to
  `$DEPLOY_HOST:~/$DEPLOY_PATH`, restarts the LaunchAgent, health-checks it,
  and rolls back on failure. Settings are in `.deploy.env` (gitignored).
- The live config (`~/.config/llm-router/tiers.json` on the host) is never
  touched by a deploy. The tier map hot-reloads. The timeouts need a restart.

## Commits
- Commit straight to `main` and push. No branches.
- End every commit message with this exact line (no `Co-Authored-By`):
  `Development supported by LLMs including both frontier lab and open weight models. All work is carefully reviewed and tested by humans.`
