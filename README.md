# cpa-plugin-zen

CLIProxyAPI native suite plugin for OpenCode Zen: client conversation
session forwarding plus direct (paid-key) providers.

Phase 1 merges two previously separate plugins with zero behavior change:

- `opencode-session-mapper` — `request_interceptor` mapping downstream
  session headers (`Session-Id`, `Thread-Id`, `X-Claude-Code-Session-Id`,
  `X-DeepSeek-Harness-Session-Id`, …) plus the host-computed
  `canonical_session_id` fallback to `X-Opencode-Session` /
  `X-Opencode-Client` for Zen sticky routing.
- `cpa-plugin-systemone` — `model_provider` + `model_router` + `executor`
  exposing `jev-1.13` (`jev`) and `jev-1.13-free` (`jev-free`) as
  chat-compatible models over `/zen/v1/systemone` with a weighted key pool.

Free-tier pools arrive in Phase 2 and never share the paid keys below.

## Capabilities

- `request_interceptor` — session mapping before and after credential
  selection (narrow header/metadata projection; 64 MiB envelope gate).
- `model_provider` — advertises `zen/<upstream-name>` models.
- `model_router` — hijacks configured aliases to this executor; every other
  model returns `Handled: false` (e.g. `deepseek-flash` keeps its native
  channels untouched).
- `executor` — SystemOne `{model, state, questions}` calls through the host
  HTTP client with pool failover; answers render as standard OpenAI chat
  completions (single chunk on streams).
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

```bash
./build.sh
```

Runs `go vet`, `go test`, then `go build -buildmode=c-shared` for
`./cmd/zen`, emitting `zen-v<version>.so` into `plugins/linux/amd64/`.

## Test

```bash
go vet ./... && go test ./...
```

Covers session priority/fallback/fail-closed behavior, config defaults,
router ownership (including `deepseek-flash` passthrough), the SystemOne
envelope mapping, and OpenAI response rendering.

## License

MIT
