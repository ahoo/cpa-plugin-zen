#!/usr/bin/env bash
# Zen plugin e2e runner. Quick (cases 1 2 6, ~2min) or full (1..11, ~16min).
# Per-case failure retries 2x with 30s interval; outcome is tri-state:
#   PASS       - green first try
#   FLAKE-WARN - failed then recovered on retry (free tier flakiness, expected)
#   FAIL       - dead on all 3 attempts -> actionable signal
# Usage: ./run.sh [--quick] [--base URL] [--cases "1 2 3"]
set -u
BASE="${BASE:-http://127.0.0.1:8317}"
# Gateway auth: env E2E_API_KEY wins, else first api-key from the live config
# (runtime read, never committed).
if [[ -z "${E2E_API_KEY:-}" ]]; then
  CFG="${E2E_CONFIG:-/home/ubuntu/workspace/cliproxyapi/config.yaml}"
  E2E_API_KEY="$(grep -m1 -A30 '^api-keys:' "$CFG" 2>/dev/null | grep -m1 -o 'sk-[A-Za-z0-9_-]*' || true)"
  export E2E_API_KEY
fi
QUICK=0
CASES=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --quick) QUICK=1; shift ;;
    --base) BASE="$2"; shift 2 ;;
    --cases) CASES="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
if [[ -z "$CASES" ]]; then
  if [[ "$QUICK" == "1" ]]; then CASES="1 2 6"; else CASES="1 2 3 4 5 6 7 8 9 10 11"; fi
fi
DIR="$(cd "$(dirname "$0")" && pwd)"
RESTARTS_BEFORE=""
if command -v docker >/dev/null 2>&1; then
  RESTARTS_BEFORE="$(docker inspect -f '{{.RestartCount}}' cliproxyapi 2>/dev/null || true)"
fi

pass=0; flake=0; fail=0
declare -a ROWS
for c in $CASES; do
  attempt=0; ok=0; first_fail=""
  tmp="$(mktemp)"
  while [[ $attempt -lt 3 ]]; do
    attempt=$((attempt+1))
    python3 "$DIR/cases.py" --base "$BASE" --case "$c" >"$tmp" 2>&1
    out="$(tail -n 1 "$tmp")"
    if python3 -c "import json,sys; sys.exit(0 if json.load(open('$tmp'))['ok'] else 1)" 2>/dev/null; then
      ok=1; break
    fi
    [[ -z "$first_fail" ]] && first_fail="$out"
    [[ $attempt -lt 3 ]] && sleep 30
  done
  if [[ $attempt -eq 1 && $ok -eq 1 ]]; then
    pass=$((pass+1)); ROWS+=("case $c  PASS")
  elif [[ $ok -eq 1 ]]; then
    flake=$((flake+1)); ROWS+=("case $c  FLAKE-WARN (recovered try $attempt; first: $first_fail)")
  else
    fail=$((fail+1)); ROWS+=("case $c  FAIL x3 (last: $out)")
  fi
  rm -f "$tmp"
done

echo "=== zen e2e ($BASE) cases: $CASES ==="
printf '%s\n' "${ROWS[@]}"
echo "---"
echo "PASS=$pass FLAKE-WARN=$flake FAIL=$fail"
if [[ -n "$RESTARTS_BEFORE" ]] && command -v docker >/dev/null 2>&1; then
  NOW="$(docker inspect -f '{{.RestartCount}}' cliproxyapi 2>/dev/null || true)"
  echo "restarts: before=$RESTARTS_BEFORE after=$NOW"
  if [[ "$NOW" != "$RESTARTS_BEFORE" ]]; then echo "HARD-FAIL: container restarted mid-run"; exit 1; fi
fi
[[ $fail -gt 0 ]] && exit 1
exit 0
