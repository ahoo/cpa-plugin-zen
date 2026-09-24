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

For a new installation, load the plugin when starting CLIProxyAPI. For an
already-running host that supports versioned native hot reload, stage and verify
a **new** `zen-v<version>.so`, then select it using
`plugins.configs.zen.store.version`. The filename and embedded plugin version
must agree. Verify `plugin hot reloaded` / `active_version` in the host log and
confirm the process has not restarted. Add the new artifact **after** the old
host is running: the reference host's startup cleanup removes unselected
version files, even if their version number is newer.

Replacing the bytes at an already-loaded path or touching the configuration is
not a code reload. Retain the previous artifact and configuration backup; do not
assume switching back to a previously loaded path safely reinitializes its
native state. For a single-file configuration bind mount, write the validated
configuration in place to preserve its inode. If hot reload is unavailable,
stop and arrange a separately authorized maintenance window instead of
restarting a live host as part of installation.

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

Debian/glibc toolchain only. Requires Go >= 1.26; this revision's compatibility
gate uses Go 1.26.8, matching the reference host and prior production artifact.
Run the native Python probe in a glibc environment too, not Alpine/musl.

Always stage development artifacts outside the host's plugin discovery directory:

```bash
STAGING="$(mktemp -d)"
VERSION=0.2.5
GOTOOLCHAIN=local CGO_ENABLED=1 go build -mod=readonly -buildmode=c-shared \
  -ldflags "-X main.pluginVersion=$VERSION -X github.com/ahoo/cpa-plugin-zen.pluginVersion=$VERSION" \
  -o "$STAGING/zen-v$VERSION.so" ./cmd/zen
python3 scripts/abi_probe.py "$STAGING/zen-v$VERSION.so"
```

Check `go version` before building, and select an unused version when deploying;
never overwrite a loaded version's artifact.

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
  requested downstream format. Its executor result carries `X-Zen-Fallback: paid`;
  the host may not forward that header to HTTP clients. Absence of the header
  is not proof that no paid request occurred.
- Cancellation and deterministic request errors do not trigger fallback.
  Upstream/fallback errors are returned as errors, not synthetic successful
  `stop` frames. Empty-result detection ignores role/usage-only frames.
- HTTP 403 with the explicit message `OpenCode's free tier can only be used from
  within OpenCode` ends free attempts immediately. It does not penalize sessions
  or members, rotate through the pool, or become an empty successful completion.
  The explicit geographic denial `This model is not available in your country`
  has the same health-neutral, single-free-attempt behavior. Console wrapping
  and letter case are accepted; unknown 403 and 429 retain their existing
  handling. Neither denial proves a session is bad, and returning it does not
  remove the upstream restriction. Explicitly configured paid fallback retains
  its separate resolver contract; no denial enables it automatically.
- Chat/Responses with an unavailable session pool, or any free endpoint with no
  eligible member, fail locally with HTTP 503 before upstream dispatch. This
  neither rewrites the pool nor extends cooldowns. `earliest_local_retry_at` in
  the error message is a UTC timestamp or `unknown`, excluding retired sessions
  (`fails >= 5`) and accounting for member cooldowns. It is only a local
  availability estimate, not an upstream-access guarantee or a `Retry-After`
  response header.
- **Session-pool compatibility:** only an unset `free.session_pool` whose default
  `plugins/zen-sessions.json` does not exist retains legacy sessionless operation.
  Explicitly configured missing files, unreadable/invalid files, empty pools,
  and exhausted pools fail closed; this also applies to existing default files.
  A healthy sticky assignment remains preferred. SystemOne retains its existing
  no-session behavior and does not require a usable session pool.
- Explicit `free.quota.failover_paid: true` still permits one paid fallback
  sequence for local unavailability or a policy 403. With fallback disabled,
  local unavailability makes no free or paid HTTP requests, and a policy 403
  makes just one free attempt. No error automatically enables paid fallback.
- Text, tool calls and usage survive response conversion; Responses text
  `done` events do not duplicate preceding deltas. Only `/v1/messages` receives
  executor-level `data:` framing; other paths remain bare JSON for the host.
- After a successful Responses `response.completed`, Zen's observed accounting
  tail `{"type":"ping","cost":...}` is ignored without emitting another chunk
  or changing text, tools, finish or usage. It must contain exactly those two
  field names; `cost` is opaque JSON, including `null`. Missing/extra fields,
  unknown tail events, duplicate completion and late output still fail. Parsing
  continues through the tail, so a later error cannot become a successful reply.
  Both EOF and a final `[DONE]` remain supported.
- **Temporary 0.2.5 diagnostic:** the first post-`[DONE]` framing failure for the
  configured `mimo-free` Chat entry, per executor, retains HTTP 502 and appends
  `; tail_structure_v2=`. The JSON envelope has `schema: 2`, the unchanged
  14-field `summary`, `detail_complete`, and ordered `frames`. Each frame has
  a fixed `kind`, ordered `[field, shape]` pairs, and an `unknown_fields` count.
  Only a fixed whitelist of protocol field names and shape categories can
  appear: unknown names and event types are never echoed. Empty, null and
  nonempty containers remain distinguishable; `cost` is always opaque.
  No upstream text, tool arguments, IDs, headers, credentials, proxy URLs or
  cost values are recorded. The entire suffix is at most 1024 UTF-8 bytes;
  detail overflow produces `detail_complete: false` and an empty frame list,
  not truncated JSON. Scan and detail completeness are both required as evidence.
  All post-DONE data remains rejected; a summary is not successful compatibility.
  Ordinary traffic may consume the one-shot observation before a manual probe.
  Missing, incomplete or conflicting evidence must not justify accepting a tail
  or spending another probe. No extra HTTP request or health write is performed
  by the observer.
- Buffered SSE is parsed and validated once before the first output chunk;
  streaming reuses independently owned, validated events instead of parsing them
  again. Text and tool arguments use growable buffers. Prepared chunks are queued
  synchronously in a closed, fully buffered channel, so an abandoned consumer
  cannot strand a producer. This is not incremental network streaming or a fixed
  total-memory bound.
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

Offline allocation/CPU benchmarks (fixed in-memory responses, no real quota):

```bash
go test -mod=readonly -run '^$' -bench '^BenchmarkFree' -benchmem -benchtime=150ms -count=5 .
```

They cover Chat/Responses streams with 256/1024 deltas, 64 KiB text/tool
aggregation, and completion chunking. Compare repeated samples on the same
machine; `B/op` measures cumulative allocation, not peak live memory or RSS.

For isolated official-host compatibility tests, see `scripts/offline-e2e/`.
That gate uses temporary containers, an internal network and a mock upstream;
it must not mount production configuration or credentials. The older
`scripts/e2e/` runner is **production-only** and can consume real upstream quota
or incur fallback charges. `scripts/mint_sessions.py` is an operational tool
that writes session state, not an offline test. Do not run either as part of
local regression testing.

## License

MIT
