# llm-router

One OpenAI-compatible endpoint in front of every LLM backend on a host.

Clients name a **tier**, such as `amazing`, `great`, `fast` or `instant`, and
never a model. The router decides which backend serves each tier and what that
backend calls the model. Swapping the model behind a tier is a one-line config
edit that takes effect on the next request. No client, agent or chat UI needs
to change.

- **Single Go file, standard library only.** No dependencies to audit.
- **Hot-reloading tier map.** A bad edit is refused and the last good routing
  is kept.
- **Backend names never leak.** Replies are rewritten so clients only ever see
  the tier name. Streaming is preserved.
- **Substitution guard.** A reply from a model other than the one the tier is
  configured for is refused, not silently passed on.
- **One front door for every backend.** Constant-time API keys, plus optional
  delegation of key checks to a backend such as LM Studio.
- **Audit log.** JSON Lines, rolling retention, key IDs only. Secrets are never
  written.
- **Fails fast and streams unbuffered.** Long generations are not mistaken for
  hung ones.

It was built to serve open-weight models from a Mac Studio (M3 Ultra, 512 GB)
to a small team's agents and chat UI. See [Example deployment](#example-deployment).

## Why tiers?

Open-weight models change every few weeks. If every agent config, chat UI and
script names a model directly, each upgrade means hunting down every client.

With tiers, clients ask for a capability and speed class (`amazing` is the most
capable and `instant` the fastest). The operator decides, and re-decides, which
model and engine fill each class. A model swap becomes a config change on one
host.

## Build and run

```sh
go build -o llm-router .
./llm-router --config ~/.config/llm-router/tiers.json
```

## Config

See [`tiers.example.json`](tiers.example.json). Keep the real file at mode
`600`, because it may contain keys. The file is the router's only state:
editing it re-points a tier on the next request, and because nothing else is
stored, the change survives a restart.

```json
{
  "listen": "0.0.0.0:12345",
  "dial_timeout_ms": 2000,
  "response_header_timeout_ms": 3600000,
  "client_api_keys": ["sk-lm-AbCdEfGh:..."],
  "tiers": {
    "amazing": { "base_url": "http://127.0.0.1:8201/v1", "model": "some-org/Big-Model-122B" },
    "great":   { "base_url": "http://127.0.0.1:12346/v1", "model": "great", "api_key_env": "LMSTUDIO_API_KEY" },
    "fast":    { "base_url": "http://127.0.0.1:8123/v1", "model": "some-org/Mid-Model-27B" },
    "instant": { "base_url": "http://127.0.0.1:8200/v1", "model": "some-org/Small-MoE-35B-A3B" }
  }
}
```

- **`base_url`** includes the version prefix. A request to `/v1/chat/completions`
  on the router becomes `<base_url>/chat/completions` at the backend.
- **`model`** is what the **backend** calls the model. This rewrite is what
  makes tiers work: a client asks for `fast`, the backend receives its own model
  identifier, and the reply comes back saying `fast` again.
- **`api_key` or `api_key_env`** can be set per tier, so backends with different
  credentials can sit behind one front door. A tier may have no key at all.
  Some local model servers have no auth of their own, in which case the
  router's front door is the only thing protecting them.
- **`listen`** must be `0.0.0.0` if a containerised client, such as Open WebUI in
  Docker, reaches the router via `host.docker.internal`. Bind every backend to
  loopback, so the router is the only LLM port exposed beyond the host.
- **`dial_timeout_ms` and `response_header_timeout_ms`** are read once at
  startup, unlike the tier map, so changing them needs a restart.
  `response_header_timeout_ms` has to cover the slowest backend's prefill. A
  large model serving a 512k context can take 20–40 minutes to prefill a
  full-window prompt before its first byte.
- **`verify_response_model`** defaults to on. See [Substitution guard](#substitution-guard).

Unknown JSON fields are rejected on load, so a mistyped key fails loudly
instead of being silently ignored.

## The backend's name never reaches a client

Rewriting the request is not enough. Backends echo the model name in their
replies, so a client asking for `fast` would otherwise see the real model
identifier come back.

The router rewrites the name back on the way out, wherever it appears as a
`model` or `system_fingerprint` value:

- **Non-streamed JSON** is buffered, rewritten and sent with a corrected
  `Content-Length`. Nothing is being streamed, so nothing is delayed.
- **Event streams** are rewritten a line at a time and flushed per line, so
  streaming stays streaming. It uses a `bufio.Reader` rather than a `Scanner`,
  because one chunk can exceed any token limit a Scanner imposes.
- **Only exact matches on those two keys** are replaced, so generated text is
  never touched. Ask `fast` to print its backend model's name and it still
  will, while the surrounding metadata says `fast`. The router rewrites
  metadata, never content.
- **Anything that is not parseable JSON passes through untouched.** A relay must
  not corrupt a body it does not understand.

This covers `/v1/chat/completions` and `/v1/responses`, streamed and not. Some
backends open a stream with a synthetic frame (oMLX sends `"model":"keepalive"`
first). Those names are recognised and rewritten too.

## Substitution guard

A backend that answers as a *different* model is worse than one that errors,
because nothing downstream notices. This happened in production: the model
behind one tier crashed, and its requests were quietly answered by another
model loaded on the same server.

So for every successful, parseable reply, the router checks that the model
named is either the tier or the tier's configured backend model. Anything else
fails with `502` and is recorded in the audit log as `model_substituted`.

For streams, the check happens before the status line is sent, because a `200`
cannot be turned into an error afterwards. The stream is peeked only as far as
the first event that names a real model. The peeked bytes are then replayed,
so streaming is unaffected.

`"verify_response_model": false` disables the guard without a rebuild, because
the config hot-reloads.

## Front-door authentication

There are two independent mechanisms. Both are checked **before** a tier is
chosen, so the key requirement applies equally to every backend, including
ones with no auth of their own.

```json
"client_api_keys": ["sk-lm-...", "sk-lm-..."],
"client_key_verifier": {
  "base_url": "http://127.0.0.1:12346/v1",
  "ttl_seconds": 300,
  "negative_ttl_seconds": 30
}
```

- **`client_api_keys`** is a static list, compared in constant time. Give each
  client its own key, so one machine can be revoked without reissuing keys to
  everyone. The static list also keeps known clients working when the verifier
  is down. `client_api_key` and `client_api_key_env` are single-key
  alternatives.
- **`client_key_verifier`** lets a backend that owns keys decide. LM Studio
  does not store its API keys anywhere a file parser can read. So for an
  unrecognised key, the router makes one `GET /models` call to the verifier
  with that bearer token, and a `200` means the key is genuine. Results are
  cached by key hash, never in plaintext. The cache is bounded, and it is
  cleared all at once when full, so an internet brute force cannot grow it.
  - A key **revoked** at the verifier keeps working until its cache entry
    expires (`ttl_seconds`).
  - The verifier decides who may use the router, so it must be loopback or
    otherwise trusted.

Set at least one of these whenever the router is reachable from outside the
host, for example through a tunnel. With neither, the router is
unauthenticated.

## Endpoints

| Endpoint | Purpose |
|---|---|
| `GET /v1/models` | The tier names, synthesized. Chat UIs build their model list from this, so it advertises tiers, not backend model names. |
| `POST /v1/*` | Any request with a `"model"` field is routed and rewritten. |
| `GET /_tiers` | What the router believes right now. Credentials are never echoed. |
| `GET /healthz` | `ok` |

## Audit log

Every LLM call on the host passes through the router, which makes it the one
place a complete record can be kept.

```json
"audit": {
  "dir": "/var/log/llm-router-audit",
  "retain_days": 7,
  "log_bodies": true,
  "max_body_bytes": 262144,
  "log_responses": false
},
"client_labels": {
  "sk-lm-AbCdEfGh": "alice (laptop agent)"
}
```

The log writes one JSON object per line and one file per day
(`audit-YYYY-MM-DD.jsonl`). The directory is `0700` and the files are `0600`,
because with bodies turned on the log holds everything everyone typed.

```json
{"ts":"2026-10-09T07:37:48-04:00","method":"POST","path":"/v1/chat/completions",
 "remote":"127.0.0.1:58588","key":"sk-lm-AbCdEfGh","label":"alice (laptop agent)",
 "user_agent":"curl/8.7.1","req_bytes":150,"tier":"fast",
 "backend":"http://127.0.0.1:8123/v1","backend_model":"some-org/Mid-Model-27B",
 "resp_bytes":6855,"status":200,"outcome":"ok","ms":18454,
 "request":{"model":"fast","messages":[{"role":"user","content":"..."}]}}
```

- **`key` is the key ID only:** the non-secret half of `sk-lm-<id>:<secret>`.
  Other key formats are reduced to a short hash. **The secret is never
  written.**
- **`forwarded_for`** is recorded beside `remote`, because behind a tunnel every
  caller looks like loopback.
- **Rejections are recorded** with the key ID that was tried and where it came
  from (`"outcome":"denied"`). That is usually the most interesting line in the
  file.
- **`outcome`** is one of `ok`, `denied`, `not_routed`, `unknown_tier`,
  `bad_json`, `no_model`, `unreadable_body`, `backend_unreachable`,
  `backend_error`, `model_substituted`, `encode_failed`, `build_failed`.
- **`log_responses`** is off by default. Capturing a stream means buffering the
  whole completion, and the prompt is the part that shows intent.

Retention is a rolling window, pruned by the date in the filename, so a clock
change cannot strand a file.

Useful queries:

```sh
jq -r 'select(.outcome=="denied") | "\(.ts) \(.key) \(.remote) \(.forwarded_for // "-")"' audit-*.jsonl
jq -r 'select(.outcome=="ok") | .label // .key' audit-*.jsonl | sort | uniq -c | sort -rn
jq -s 'group_by(.tier)[] | {tier: .[0].tier, calls: length, ms: (map(.ms)|add/length|floor)}' audit-*.jsonl
```

## Operational notes

- **A bad edit is refused.** The router logs `reload REFUSED, keeping previous
  routing` and carries on with the last good table.
- **Dead backends fail fast,** in milliseconds, with an error naming the tier
  and the backend. The alternative, a caller sitting through its own full
  timeout, is the failure this is built to avoid.
- **Streaming passes through unbuffered.** Every chunk is flushed, compression
  is not requested, and `X-Accel-Buffering: no` is set for anything in front.
- **There is no overall request deadline.** A long generation is not a stuck
  one. `dial_timeout_ms` and `response_header_timeout_ms` are the guards
  instead.

## Example deployment

This is the production setup it was written for: a Mac Studio (M3 Ultra,
512 GB unified memory) serving a small team over the LAN and through a tunnel.

```
                 :12345 (all interfaces, keys enforced)
  clients ----->  llm-router  --+-->  127.0.0.1:8201   oMLX       amazing  (122B MoE, 512k context via YaRN)
                                +-->  127.0.0.1:12346  LM Studio  great    (also the key verifier)
                                +-->  127.0.0.1:8123   Splash     fast     (27B)
                                +-->  127.0.0.1:8200   oMLX       instant  (35B-A3B MoE)
```

The clients are:
- OpenClaw agents on Discord;
- OpenClaude coding sessions on laptops;
- Open WebUI in Docker;
- evaluation harnesses.

Each client has its own key and its own label in the audit log.

Notes from running it:

- **Bind every model server to loopback.** Several local servers have no auth.
  If one listens on `0.0.0.0`, it sits beside the router and skips the key
  check.
- **Give each oMLX instance its own model directory.** oMLX loads any model it
  finds under `--model-dir`, so a shared parent directory lets a request to one
  port pull a second model into memory.
- **Keep the verifier running** even when it has no model loaded. If it is
  down, only the static keys work.

On macOS, run it as a LaunchAgent with `KeepAlive`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>local.llm-router</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/llm-router</string>
    <string>--config</string>
    <string>/Users/YOU/.config/llm-router/tiers.json</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/Users/YOU/Library/Logs/llm-router.out.log</string>
  <key>StandardErrorPath</key><string>/Users/YOU/Library/Logs/llm-router.err.log</string>
</dict>
</plist>
```

## License

MIT. See [LICENSE](LICENSE).

Development supported by LLMs including both frontier lab and open weight
models. All work is carefully reviewed and tested by humans.
