# Zen e2e

Production-only probes against the live gateway (`:8317`). Every prompt is
prefixed `e2e-probe` so upstream spend is identifiable.

## Run

```bash
./run.sh --quick          # cases 1 2 6, ~2 min, pre/post-restart smoke
./run.sh                  # full 1..11, ~16 min
BASE=http://host:8317 ./run.sh --quick
```

## Cases

| # | What | Assert |
|---|------|--------|
| 1 | paid direct (`deepseek-flash`) | 200 + non-empty |
| 2 | free-first (`mimo-free`) | 200 + `X-Zen-Pool-Session` echo (needs plugin ≥ `0bc9bec`) |
| 3 | SystemOne paid (`jev`) | 200 + content |
| 4 | `jev-free` free-first + fallback | 200 + content |
| 5 | `muse-free` responses mapping (stream) | SSE text non-empty |
| 6 | paid isolation (`deepseek-flash` x2) | no pool echo |
| 7 | unknown model | 4xx fail-closed |
| 8 | stickiness x3 (same pinned session) | 3x200 (echo stripped by host, see note) |
| 9 | chat SSE (`mimo-free` stream) | chunks + `[DONE]` + text |
| 10 | tools passthrough paid (`deepseek-flash` + tool) | 200, tool_calls or text |
| 11 | tools on free tier (`mimo-free` + tool, cloak merge) | 200, shape valid |

> Host strips custom plugin response headers (proven 2026-09-23), so cases
> 2/8 assert availability; stickiness is unit-tested. Cases 1..8 ~12 min,
> full 1..11 ~16 min.

## Verdicts

- `PASS` — green first try.
- `FLAKE-WARN` — recovered on retry. Expected on free tier; not actionable
  unless the same case flakes 3 runs in a row.
- `FAIL` — dead on all 3 attempts. Actionable: check upstream status and
  `docker logs`, then file/assign.
- Container `RestartCount` is compared before/after; any restart mid-run is a
  hard fail regardless of case results.

## Cost ceiling

`max_tokens=32` per call (128 for tool cases), ≤ 33 calls full suite. Worst case is cents; paid
fallback may answer free-tier cases and that is by design (still PASS).
