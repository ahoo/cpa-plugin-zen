# cpa-plugin-zen

CLIProxyAPI native suite plugin for OpenCode Zen: client conversation
session forwarding, paid-key SystemOne models and configured free-model
Chat/Responses/SystemOne adapters with optional paid fallback.

The suite includes the capabilities of two previously separate plugins:

- `opencode-session-mapper` — `request_interceptor` mapping downstream
  session headers (`Session-Id`, `Thread-Id`, `X-Claude-Code-Session-Id`,
  `X-DeepSeek-Harness-Session-Id`, …) plus the host-computed
  `canonical_session_id` fallback to `X-Opencode-Session` /
  `X-Opencode-Client` for Zen sticky routing.
- `cpa-plugin-systemone` — `model_provider` + `model_router` + `executor`
  exposing `jev-1.13` (`jev`) and `jev-1.13-free` (`jev-free`) as
  chat-compatible models over `/zen/v1/systemone` with a weighted key pool.

Free-tier pools have separate member configuration. Optional paid fallback uses
`paid.api_keys` (or the existing paid credential fallback), and can incur charges.
This plugin does not grant access to models or regions an upstream disallows.

## Capabilities

- `request_interceptor` — session mapping before and after credential
  selection (narrow header/metadata projection; 64 MiB envelope gate).
- `model_provider` — advertises `zen/<upstream-name>` models.
- `model_router` — hijacks configured aliases to this executor; every other
  model returns `Handled: false` (e.g. `deepseek-flash` keeps its native
  channels untouched).
- `executor` — SystemOne `{model, state, questions}` calls through the host
  HTTP client with pool failover; answers render as standard OpenAI chat
  completions (content chunks followed by terminal usage on streams).
- `request_translator` / `response_translator` — model-name normalization
  and pass-through.

## Install

```yaml
plugins:
  enabled: true
  configs:
    zen:
      enabled: true
      priority: 100
      session_mapping:
        enabled: true
      paid:
        api_keys:
          - key: sk-YOUR_ZEN_KEY
            weight: 10
            # proxy_url: http://127.0.0.1:18080  # optional per-key proxy
        # models:  # optional override (defaults: jev, jev-free)
        #   - alias: jev
        #     name: jev-1.13
```

Then restart CLIProxyAPI (plugins load at startup):

```bash
docker restart cli-proxy-api
docker logs cli-proxy-api | grep -E "plugin_id=zen"
# pluginhost: plugin registered plugin_id=zen plugin_name=Zen Suite
```

Disable the superseded stanzas (`deepseek-harness-session`,
`opencode-session-mapper`, `systemone`) — `zen` covers their behavior.
Keep their `.so` files until the switch is verified, then remove them.

## Request mapping (paid executor)

First match wins:

1. **Native envelope** — last user message parses as JSON with a
   `questions` object: `{"state": ..., "questions": {...}}` used directly.
2. **Configured questions** — plain chat text becomes `state`, `questions`
   from `paid.models[].questions` for the requested alias.

## Build

Debian/glibc toolchain only. Requires Go >= 1.26:

Always stage development artifacts outside the host's plugin discovery directory:

```bash
STAGING="$(mktemp -d)"
CGO_ENABLED=1 go build -mod=readonly -buildmode=c-shared -o "$STAGING/zen.so" ./cmd/zen
python3 scripts/abi_probe.py "$STAGING/zen.so"
```

`build.sh` additionally runs module tidy, vet and tests in Docker. Its default
output is the adjacent `plugins/linux/amd64/` directory, which may be a live
plugin mount. For development, explicitly override it with
`PLUGIN_OUT_DIR="$STAGING" ./build.sh`. Neither build command restarts the host;
building and deploying are separate operations.

## Fallback and response semantics

- `free.models[].endpoint` accepts `chat` (also the empty default), `responses`,
  or `systemone`. Invalid values and overflowing pool weight totals fail at
  registration instead of producing an empty response or panic.
- When enabled by `free.quota.failover_paid`, one logical request starts at
  most one paid fallback sequence. That sequence retains normal paid-member
  failover, requests non-streaming chat JSON, and renders the result in the
  requested downstream format. Its output carries `X-Zen-Fallback: paid`.
- Cancellation and deterministic request errors do not trigger fallback.
  Upstream/fallback errors are returned as errors, not synthetic successful
  `stop` frames. Empty-result detection ignores role/usage-only frames.
- Text, tool calls and usage survive response conversion; Responses text
  `done` events do not duplicate preceding deltas. Only `/v1/messages` receives
  executor-level `data:` framing; other paths remain bare JSON for the host.
- Native buffers are checked before conversion to `C.int`. Stream pump failures
  cancel plugin producers; shutdown drains asynchronous workers and in-flight
  callbacks before the host can free its C callback table. Cancellation of a synchronous host call still
  depends on the host callback context; a plugin-local context alone cannot
  interrupt an arbitrary C callback.

## Test

```bash
go test -mod=readonly ./...
go vet -mod=readonly ./...
go test -mod=readonly -race ./...
```

Tests use synthetic credentials and local/in-memory HTTP fixtures. They cover
session mapping, model ownership, fallback call counts, text/tool/usage
conversion, malformed/truncated streams, transport errors, pool concurrency,
configuration boundaries and ABI cancellation. Native pointer/length boundaries
are checked separately by `scripts/abi_probe.py` against a staged shared library.

For isolated official-host compatibility tests, see `scripts/offline-e2e/`.
That gate uses temporary containers, an internal network and a mock upstream;
it must not mount production configuration or credentials. The older
`scripts/e2e/` runner is **production-only** and can consume real upstream quota
or incur fallback charges. `scripts/mint_sessions.py` is an operational tool
that writes session state, not an offline test. Do not run either as part of
local regression testing.

## License

MIT
