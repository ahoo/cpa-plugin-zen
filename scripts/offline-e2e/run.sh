#!/usr/bin/env bash
# Offline exact-Host protocol gate; NEVER run scripts/e2e/run.sh or
# scripts/mint_sessions.py for this test: they contact real services.
# Usage: ./run.sh --build                 (Go 1.26 glibc, cached Go modules)
#        ./run.sh --plugin /tmp/staged/zen.so
# Requires local Docker images with IDs pinned below, Docker network support,
# bash, Go 1.26 for --build, and only Python stdlib in the cached Python image.
# No production config, auth, plugin discovery, host ports, or daemon restarts.
# Starts an --internal Docker network, synthetic mock and exact unmodified Host
# image; routes every upstream URL to http://mock:9999, tests through a third
# container on that network, then removes all resources, even on failure.
# No real Zen network traffic or identity/provider-gate claim is made here.
set -euo pipefail
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GATE="$SRC/scripts/offline-e2e"
HOST_IMAGE='sha256:793691b3817afff2679999bfd173f23e63b0cc05517859f78d8adcc5c5345ea1'
PY_IMAGE='sha256:d10dc7bdd3eafc178ccc7672fb3cb319a4c6c34d332a4234832777868e1e18f3'
GO='/home/ubuntu/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.26.0.linux-amd64/bin/go'
PLUGIN=''
BUILD=0
while (($#)); do
  case "$1" in
    --build) BUILD=1; shift ;;
    --plugin) [[ $# -ge 2 ]] || { printf 'missing --plugin argument\n' >&2; exit 2; }; PLUGIN="$2"; shift 2 ;;
    *) printf 'usage: %s [--build | --plugin /absolute/staged/zen.so]\n' "$0" >&2; exit 2 ;;
  esac
done
if ((BUILD == 1)) && [[ -n "$PLUGIN" ]] || ((BUILD == 0)) && [[ -z "$PLUGIN" ]]; then
  printf 'specify exactly one of --build or --plugin /absolute/staged/zen.so\n' >&2
  exit 2
fi
[[ "$(docker image inspect "$HOST_IMAGE" --format '{{.Id}}')" == "$HOST_IMAGE" ]] || { printf 'pinned Host image mismatch\n' >&2; exit 1; }
[[ "$(docker image inspect "$PY_IMAGE" --format '{{.Id}}')" == "$PY_IMAGE" ]] || { printf 'offline Python image unavailable\n' >&2; exit 1; }
# An explicit staged plugin must not come from production discovery or this source tree.
if ((BUILD == 0)); then
  [[ "$PLUGIN" = /* && -f "$PLUGIN" ]] || { printf 'plugin must be an absolute path to an existing staged .so\n' >&2; exit 2; }
  case "$(realpath "$PLUGIN")" in "$SRC"/*|/home/ubuntu/workspace/cliproxyapi/plugins/*)
    printf 'refusing source or production-discovery plugin artifact\n' >&2; exit 2 ;;
  esac
fi
TMP="$(mktemp -d "${TMPDIR:-/tmp}/zen-offline-e2e.XXXXXXXX")"
ID="$(basename "$TMP" | tr -cd 'a-zA-Z0-9')"
NET="zen-e2e-${ID}"
MOCK="zen-e2e-mock-${ID}"
HOST="zen-e2e-host-${ID}"
cleanup() {
  docker rm -f "$HOST" "$MOCK" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  # Older Host versions may have created root-owned error logs in this
  # temporary bind. Remove only our own tmp logs using the cached Python image.
  if [[ -d "$TMP" ]]; then
    docker run --rm --network none -v "$TMP:/offline:rw" "$PY_IMAGE" \
      python3 -c 'import shutil; shutil.rmtree("/offline/auth/logs", ignore_errors=True)' >/dev/null 2>&1 || true
    rm -rf -- "$TMP"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -p "$TMP/plugins/linux/amd64" "$TMP/auth"
if ((BUILD == 1)); then
  [[ -x "$GO" ]] || { printf 'Go 1.26 toolchain unavailable\n' >&2; exit 1; }
  printf '[offline] building plugin into temporary staging (no build.sh, no go mod tidy)\n'
  (cd "$SRC" && env CGO_ENABLED=1 GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local "$GO" build -mod=readonly -buildvcs=false -buildmode=c-shared \
      -ldflags '-X main.pluginVersion=0.2.0 -X github.com/ahoo/cpa-plugin-zen.pluginVersion=0.2.0' \
      -o "$TMP/plugins/linux/amd64/zen-v0.2.0.so" ./cmd/zen)
else
  cp -- "$PLUGIN" "$TMP/plugins/linux/amd64/zen-v0.2.0.so"
fi
# No production config/credentials/volumes. Every upstream URL resolves ONLY inside this private network.
# The session ID is synthetic; tests here exercise protocol mapping, not upstream identity gates.
printf '{"sessions":[{"id":"offline-session-only","fails":0,"cooldown_until":0}]}\n' > "$TMP/zen-sessions.json"
cat > "$TMP/config.yaml" <<'YAML'
host: "0.0.0.0"
port: 8317
auth-dir: "/work/auth"
api-keys:
  - "offline-client-only"
request-retry: 0
proxy-url: ""
plugins:
  enabled: true
  dir: "/work/plugins"
  configs:
    zen:
      enabled: true
      paid:
        base_url: "http://mock:9999"
        api_key: "offline-paid-only"
        models:
          - alias: offline-paid
            name: offline-systemone
            questions:
              q1:
                type: noul
                instructions: "Synthetic protocol test only"
      free:
        enabled: true
        session_pool: "/work/zen-sessions.json"
        members:
          - key: public
        quota:
          cooldown: 1
          failover_paid: true
          paid_fallback: offline-fallback
        models:
          - {alias: offline-chat, name: offline-chat, endpoint: chat}
          - {alias: offline-chat-tool, name: offline-chat-tool, endpoint: chat}
          - {alias: offline-responses, name: offline-responses, endpoint: responses}
          - {alias: offline-responses-done, name: offline-responses-done, endpoint: responses}
          - {alias: offline-responses-tool, name: offline-responses-tool, endpoint: responses}
          - {alias: offline-fail, name: offline-fail, endpoint: chat}
          - {alias: offline-bad, name: offline-bad, endpoint: chat}
          - {alias: offline-event-error, name: offline-event-error, endpoint: responses}
YAML
# Docker's internal bridge has no external route. Neither container mounts the host socket,
# production config, auth files, or existing plugin discovery directory.
docker network create --internal "$NET" >/dev/null
docker run -d --name "$MOCK" --network "$NET" --network-alias mock \
  --env HTTP_PROXY= --env HTTPS_PROXY= --env ALL_PROXY= --env NO_PROXY='*' \
  -v "$GATE/mock.py:/mock.py:ro" "$PY_IMAGE" python3 /mock.py >/dev/null
docker run -d --name "$HOST" --network "$NET" --network-alias host \
  --user "$(id -u):$(id -g)" \
  --env HTTP_PROXY= --env HTTPS_PROXY= --env ALL_PROXY= --env NO_PROXY='*' \
  -v "$TMP:/work" "$HOST_IMAGE" ./CLIProxyAPI -config /work/config.yaml -local-model >/dev/null
printf '[offline] Host=%s on internal network only; waiting for plugin registration\n' "$HOST_IMAGE"
docker run --rm --network "$NET" --env HTTP_PROXY= --env HTTPS_PROXY= --env ALL_PROXY= --env NO_PROXY='*' \
  -v "$GATE/cases.py:/cases.py:ro" "$PY_IMAGE" python3 /cases.py --mode ready || {
    docker logs "$HOST" 2>&1 | grep -Ei 'plugin|zen|error|fail|listening' | cut -c1-240 || true
    exit 1
  }
docker run --rm --network "$NET" --env HTTP_PROXY= --env HTTPS_PROXY= --env ALL_PROXY= --env NO_PROXY='*' \
  -v "$GATE/cases.py:/cases.py:ro" "$PY_IMAGE" python3 /cases.py --mode test
printf '[offline] gate PASS; temporary containers, network and staged files will be removed\n'
