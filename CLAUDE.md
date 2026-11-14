# llm-router: agent notes

One OpenAI-compatible endpoint that routes capability tiers (`amazing`, `great`,
`fast`, `instant`) to local LLM backends. Single file (`main.go`), Go standard
library only. Keep it that way: no third-party dependencies.

**This is a public repo.** Never commit hostnames, IP addresses, domains, keys,
usernames or real people's names. Host-specific settings live in the gitignored
`.deploy.env` and in the host's own `tiers.json`.

## Develop
- `gofmt -l .` (must print nothing), `go vet ./... && go build . && go test ./...`
- `main_test.go` drives the real mux against `httptest` stub backends. It covers
  multipart forwarding, kind rules, `response_format`, streaming, audit records
  and the chat-path regressions. The tests share the package globals, so do not
  add `t.Parallel()`.
- Smoke-test the built binary too, against a stub backend that echoes `model`.
  Check auth (401), the tier rewrite, `model_substituted`, `backend_unreachable`,
  and a multipart upload (`curl -F model=<tier> -F file=@clip.wav` at a
  `"kind": "transcription"` tier): the stub should report the rewritten `model`
  and the file's sha256 unchanged, and the audit line should have `files`
  metadata and no audio.

## Deploy
- `scripts/deploy.sh` builds for darwin/arm64, copies the binary to
  `$DEPLOY_HOST:~/$DEPLOY_PATH`, restarts the LaunchAgent, health-checks it,
  and rolls back on failure. Settings are in `.deploy.env` (gitignored).
- The live config (`~/.config/llm-router/tiers.json` on the host) is never
  touched by a deploy. The tier map hot-reloads. The timeouts need a restart.
- **Ordering rule.** An old binary rejects unknown config fields, and at startup
  that is fatal (a `KeepAlive` crash loop that takes every tier down). So deploy
  the new binary **before** adding `kind`, tier `log_bodies` or `max_upload_bytes` to the live
  config, and remove them from the live config **before** rolling the binary
  back. On a hot reload an old binary only refuses the edit, which is harmless.

## Commits
- Commit straight to `main` and push. No branches.
- End every commit message with this exact line (no `Co-Authored-By`):
  `Development supported by LLMs including both frontier lab and open weight models. All work is carefully reviewed and tested by humans.`
