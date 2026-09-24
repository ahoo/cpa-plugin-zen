#!/usr/bin/env bash
# Offline exact-Host + native ABI gate; NEVER run scripts/e2e/run.sh or
# scripts/mint_sessions.py for this test: they contact real services.
# Usage: ./run.sh --plugin /tmp/staged/zen-v0.2.5.so
#        ./run.sh --plugin /tmp/staged/zen-v0.2.5.so --previous-plugin /tmp/staged/zen-v0.2.4.so
#        ./run.sh --build                 (isolated Go 1.26.8 glibc image only)
# Pinned cached images, internal Docker network, synthetic credentials/URLs.
# Never mount production config/auth/plugin discovery, publish ports, or restart
# the production Host. An Alpine Python client tests HTTP; the glibc Go image
# runs the native ctypes probe (Alpine cannot dlopen the shared plugin).
set -euo pipefail
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GATE="$SRC/scripts/offline-e2e"
HOST_IMAGE='sha256:793691b3817afff2679999bfd173f23e63b0cc05517859f78d8adcc5c5345ea1'
PY_IMAGE='sha256:d10dc7bdd3eafc178ccc7672fb3cb319a4c6c34d332a4234832777868e1e18f3'
GO_IMAGE='sha256:2166f13e96959cfe4bfe3d2bb208f4e91736a488a1fb7f3a194663dfb5840226'
VERSION='0.2.5'
PREVIOUS_VERSION='0.2.4'
ARTIFACT="zen-v${VERSION}.so"
PREVIOUS_ARTIFACT="zen-v${PREVIOUS_VERSION}.so"
PREVIOUS_SHA256='72dbefb794a9dde35ccf502b79a6892ef7488eb9bbd401ef4200040748e90794'
PLUGIN=''
PREVIOUS=''
BUILD=0
while (($#)); do
  case "$1" in
    --build) BUILD=1; shift ;;
    --plugin) [[ $# -ge 2 ]] || { printf 'missing --plugin argument\n' >&2; exit 2; }; PLUGIN="$2"; shift 2 ;;
    --previous-plugin) [[ $# -ge 2 ]] || { printf 'missing --previous-plugin argument\n' >&2; exit 2; }; PREVIOUS="$2"; shift 2 ;;
    *) printf 'usage: %s [--build | --plugin /absolute/staged/%s [--previous-plugin /absolute/staged/%s]]\n' "$0" "$ARTIFACT" "$PREVIOUS_ARTIFACT" >&2; exit 2 ;;
  esac
done
if ((BUILD == 1)) && [[ -n "$PLUGIN" || -n "$PREVIOUS" ]] || ((BUILD == 0)) && [[ -z "$PLUGIN" ]]; then
  printf 'specify --build alone or --plugin with optional --previous-plugin\n' >&2
  exit 2
fi
[[ "$(docker image inspect "$HOST_IMAGE" --format '{{.Id}}')" == "$HOST_IMAGE" ]] || { printf 'pinned Host image mismatch\n' >&2; exit 1; }
[[ "$(docker image inspect "$PY_IMAGE" --format '{{.Id}}')" == "$PY_IMAGE" ]] || { printf 'offline Python image unavailable\n' >&2; exit 1; }
[[ "$(docker image inspect "$GO_IMAGE" --format '{{.Id}}')" == "$GO_IMAGE" ]] || { printf 'offline glibc Go image unavailable\n' >&2; exit 1; }
# An explicit staged plugin must not come from production discovery or this source tree.
if ((BUILD == 0)); then
  candidates=("$PLUGIN")
  [[ -z "$PREVIOUS" ]] || candidates+=("$PREVIOUS")
  for candidate in "${candidates[@]}"; do
    [[ "$candidate" = /* && -f "$candidate" ]] || { printf 'plugin must be an absolute path to an existing staged .so\n' >&2; exit 2; }
    case "$(realpath "$candidate")" in "$SRC"/*|/home/ubuntu/workspace/cliproxyapi/plugins/*)
      printf 'refusing source or production-discovery plugin artifact\n' >&2; exit 2 ;;
    esac
  done
  [[ "$(basename "$PLUGIN")" == "$ARTIFACT" ]] || { printf 'candidate must be staged as %s\n' "$ARTIFACT" >&2; exit 2; }
  if [[ -n "$PREVIOUS" ]]; then
    [[ "$(basename "$PREVIOUS")" == "$PREVIOUS_ARTIFACT" ]] || { printf 'previous artifact must be staged as %s\n' "$PREVIOUS_ARTIFACT" >&2; exit 2; }
    [[ "$(sha256sum "$PREVIOUS" | cut -d' ' -f1)" == "$PREVIOUS_SHA256" ]] || {
      printf 'previous staged artifact checksum mismatch\n' >&2; exit 1;
    }
  fi
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
  printf '[offline] building only a temporary plugin with pinned Go 1.26.8 (no network, no build.sh)\n'
  docker run --rm --network none -v "$SRC:/src:ro" -v "/home/ubuntu/go/pkg/mod:/go/pkg/mod:ro" \
    -v "$TMP:/stage:rw" -w /src -e CGO_ENABLED=1 -e GOPROXY=off -e GOSUMDB=off -e GOTOOLCHAIN=local \
    "$GO_IMAGE" go build -mod=readonly -buildvcs=false -buildmode=c-shared \
      -ldflags "-X main.pluginVersion=$VERSION -X github.com/ahoo/cpa-plugin-zen.pluginVersion=$VERSION" \
      -o "/stage/plugins/linux/amd64/$ARTIFACT" ./cmd/zen
else
  cp -- "$PLUGIN" "$TMP/plugins/linux/amd64/$ARTIFACT"
fi
printf '[offline] probing staged native ABI twice in pinned glibc Go image (no network)\n'
for first_mode in nonstream stream; do
  docker run --rm --network none -v "$SRC/scripts/abi_probe.py:/abi_probe.py:ro" \
    -v "$TMP/plugins/linux/amd64:/artifacts:ro" "$GO_IMAGE" \
    python3 /abi_probe.py "/artifacts/$ARTIFACT" "$VERSION" "$first_mode"
done
# No production configuration or credentials: write separate synthetic pool
# fixtures before each fresh, otherwise unmodified, pinned Host container.
write_config() {
  local mode="$1" fallback=false cooldown=1 member='          - key: public' store=''
  case "$mode" in
    hot) store=$(printf '      store:\n        version: "%s"' "$PREVIOUS_VERSION") ;;
    test|policy-paid|geo-paid|exhausted-paid) fallback=true ;;
    cooldown) cooldown=3 ;;
    proxy|proxy-exhausted) member=$'          - key: public\n            proxy_url: "http://mock:9998"' ;;
    no-member) member=$'          - key: public\n            disabled: true' ;;
  esac
  case "$mode" in
    exhausted|exhausted-paid|proxy-exhausted) printf '{"sessions":[{"id":"offline-session-only","fails":5,"cooldown_until":0}]}\n' > "$TMP/zen-sessions.json" ;;
    bad) printf 'not-json\n' > "$TMP/zen-sessions.json" ;;
    empty) printf '{"sessions":[]}\n' > "$TMP/zen-sessions.json" ;;
    missing) rm -f -- "$TMP/zen-sessions.json" ;;
    *) printf '{"sessions":[{"id":"offline-session-only","fails":0,"cooldown_until":0}]}\n' > "$TMP/zen-sessions.json" ;;
  esac
  mkdir -p "$TMP/auth-$mode"
  cat > "$TMP/config.yaml" <<YAML
host: "0.0.0.0"
port: 8317
auth-dir: "/work/auth-$mode"
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
$store
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
          - alias: jev
            name: offline-jev
            questions:
              q1:
                type: noul
                instructions: "Synthetic protocol test only"
      free:
        enabled: true
        session_pool: "/work/zen-sessions.json"
        members:
$member
        quota:
          cooldown: $cooldown
          failover_paid: $fallback
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
          - {alias: offline-tail-ping, name: offline-tail-ping, endpoint: responses}
          - {alias: offline-tail-null, name: offline-tail-null, endpoint: responses}
          - {alias: offline-tail-extra-delta, name: offline-tail-extra-delta, endpoint: responses}
          - {alias: offline-tail-extra-response, name: offline-tail-extra-response, endpoint: responses}
          - {alias: offline-tail-no-cost, name: offline-tail-no-cost, endpoint: responses}
          - {alias: offline-tail-unknown, name: offline-tail-unknown, endpoint: responses}
          - {alias: offline-tail-late-output, name: offline-tail-late-output, endpoint: responses}
          - {alias: offline-tail-late-error, name: offline-tail-late-error, endpoint: responses}
          - {alias: offline-tail-late-failed, name: offline-tail-late-failed, endpoint: responses}
          - {alias: offline-policy, name: offline-policy, endpoint: chat}
          - {alias: offline-geo-chat, name: offline-geo-chat, endpoint: chat}
          - {alias: offline-geo-mixed, name: offline-geo-mixed, endpoint: chat}
          - {alias: offline-geo-responses, name: offline-geo-responses, endpoint: responses}
          - {alias: mimo-free, name: mimo-v2.6-flash-free, endpoint: chat}
          - {alias: offline-chat-tail-other, name: offline-chat-tail-other, endpoint: chat}
          - alias: offline-free-systemone
            name: offline-free-systemone
            endpoint: systemone
            questions:
              q1:
                type: noul
                instructions: "Synthetic protocol test only"
YAML
}
# Docker's internal bridge has no external route. Neither container mounts the
# host socket, production config, auth files, or plugin discovery directory.
docker network create --internal "$NET" >/dev/null
docker run -d --name "$MOCK" --network "$NET" --network-alias mock \
  --env HTTP_PROXY= --env HTTPS_PROXY= --env ALL_PROXY= --env NO_PROXY='*' \
  -v "$GATE/mock.py:/mock.py:ro" "$PY_IMAGE" python3 /mock.py >/dev/null
failures=()
for mode in test exhausted exhausted-paid bad empty missing no-member policy policy-paid geo geo-paid proxy proxy-exhausted cooldown tail mimo-chat-n mimo-chat-s mimo-responses-n mimo-responses-s mimo-messages-n mimo-messages-s; do
  docker rm -f "$HOST" >/dev/null 2>&1 || true
  write_config "$mode"
  docker run -d --name "$HOST" --network "$NET" --network-alias host \
    --user "$(id -u):$(id -g)" \
    --env HTTP_PROXY= --env HTTPS_PROXY= --env ALL_PROXY= --env NO_PROXY='*' \
    -v "$TMP:/work" "$HOST_IMAGE" ./CLIProxyAPI -config /work/config.yaml -local-model >/dev/null
  printf '[offline] pinned Host phase=%s on private internal network\n' "$mode"
  docker run --rm --network "$NET" --env HTTP_PROXY= --env HTTPS_PROXY= --env ALL_PROXY= --env NO_PROXY='*' \
    -v "$GATE/cases.py:/cases.py:ro" "$PY_IMAGE" python3 /cases.py --mode ready || {
      docker logs "$HOST" 2>&1 | grep -Ei 'plugin|zen|error|fail|listening' | cut -c1-240 || true
      exit 1
    }
  snapshot_mount=()
  if [[ "$mode" == geo || "$mode" == geo-paid ]]; then
    # Directory bind tracks atomic writes to our SYNTHETIC session pool. The
    # client reads only this generated fixture to prove 403 health neutrality.
    snapshot_mount=(-v "$TMP:/synthetic:ro")
  fi
  if ! docker run --rm --network "$NET" --env HTTP_PROXY= --env HTTPS_PROXY= --env ALL_PROXY= --env NO_PROXY='*' \
    "${snapshot_mount[@]}" -v "$GATE/cases.py:/cases.py:ro" "$PY_IMAGE" python3 /cases.py --mode "$mode"; then
    failures+=("$mode")
  fi
done
if ((${#failures[@]})); then
  printf '[offline] gate FAIL phases: %s\n' "${failures[*]}" >&2
  exit 1
fi
if [[ -n "$PREVIOUS" ]]; then
  # Reproduce the actual order: old Host starts with only the previous version;
  # the candidate is added to a distinct, unused path while it remains running.
  docker rm -f "$HOST" >/dev/null 2>&1 || true
  rm -f -- "$TMP/plugins/linux/amd64/$ARTIFACT"  # Our test copy, no Host running.
  cp -- "$PREVIOUS" "$TMP/plugins/linux/amd64/$PREVIOUS_ARTIFACT"
  write_config hot
  docker run -d --name "$HOST" --network "$NET" --network-alias host \
    --user "$(id -u):$(id -g)" \
    --env HTTP_PROXY= --env HTTPS_PROXY= --env ALL_PROXY= --env NO_PROXY='*' \
    -v "$TMP:/work" "$HOST_IMAGE" ./CLIProxyAPI -config /work/config.yaml -local-model >/dev/null
  printf '[offline] hot switch: pinned synthetic Host starts with only store.version=%s\n' "$PREVIOUS_VERSION"
  docker run --rm --network "$NET" --env HTTP_PROXY= --env HTTPS_PROXY= --env ALL_PROXY= --env NO_PROXY='*' \
    -v "$GATE/cases.py:/cases.py:ro" "$PY_IMAGE" python3 /cases.py --mode ready
  old_log="$(docker logs "$HOST" 2>&1 | grep -F 'pluginhost: plugin loaded' | grep -F 'plugin_id=zen' | grep -F "version=$PREVIOUS_VERSION" | grep -F "$PREVIOUS_ARTIFACT" || true)"
  [[ -n "$old_log" ]] || { printf '[offline] missing pinned old plugin load log\n' >&2; docker logs "$HOST" 2>&1 | grep -Ei 'plugin|zen|version|error' | cut -c1-300 || true; exit 1; }
  printf '[offline] %s\n' "$old_log"
  [[ ! -e "$TMP/plugins/linux/amd64/$ARTIFACT" && -f "$TMP/plugins/linux/amd64/$PREVIOUS_ARTIFACT" ]] || {
    printf '[offline] old-only Host unexpectedly has a candidate or removed its own plugin\n' >&2
    docker logs "$HOST" 2>&1 | grep -Ei 'plugin|zen|version|error' | cut -c1-300 || true
    exit 1
  }
  docker run --rm --network "$NET" --env HTTP_PROXY= --env HTTPS_PROXY= --env ALL_PROXY= --env NO_PROXY='*' \
    -v "$GATE/cases.py:/cases.py:ro" "$PY_IMAGE" python3 /cases.py --mode hot-old
  before="$(docker inspect --format '{{.State.Pid}}|{{.State.StartedAt}}|{{.RestartCount}}' "$HOST")"
  [[ ! -e "$TMP/plugins/linux/amd64/$ARTIFACT" ]] || { printf '[offline] refusing to overwrite an existing plugin path\n' >&2; exit 1; }
  cp -- "$PLUGIN" "$TMP/plugins/linux/amd64/$ARTIFACT"
  cmp -s -- "$PLUGIN" "$TMP/plugins/linux/amd64/$ARTIFACT" || { printf '[offline] staged candidate differs from source\n' >&2; exit 1; }
  printf '[offline] added %s to new plugin path while old Host PID=%s remained running\n' "$VERSION" "${before%%|*}"
  inode="$(stat -c %i "$TMP/config.yaml")"
  # Change only one version byte in place: same config inode, no partial YAML
  # or .so replacement. This is ONLY our generated /work/config.yaml.
  python3 - "$TMP/config.yaml" "$PREVIOUS_VERSION" "$VERSION" <<'PY'
import os
import sys
old, new = (arg.encode("ascii") for arg in sys.argv[2:])
changes = [i for i, (a, b) in enumerate(zip(old, new)) if a != b]
assert len(old) == len(new) and len(changes) == 1, "hot selector must change one byte"
with open(sys.argv[1], "r+b", buffering=0) as config:
    data = config.read()
    needle = b'version: "' + old + b'"'
    assert data.count(needle) == 1, "synthetic store.version selector missing"
    index = data.index(needle) + len(b'version: "') + changes[0]
    config.seek(index)
    assert config.write(new[changes[0]:changes[0] + 1]) == 1
    os.fsync(config.fileno())
PY
  [[ "$(stat -c %i "$TMP/config.yaml")" == "$inode" ]] || { printf '[offline] synthetic config inode changed\n' >&2; exit 1; }
  reloaded=''
  for ((i=0; i<60; i++)); do
    reloaded="$(docker logs "$HOST" 2>&1 | grep -F 'pluginhost: plugin hot reloaded' | grep -F 'plugin_id=zen' | grep -F "active_version=$VERSION" | grep -F "retired_version=$PREVIOUS_VERSION" | grep -F "$ARTIFACT" | grep -F "$PREVIOUS_ARTIFACT" || true)"
    [[ -z "$reloaded" ]] || break
    sleep 0.5
  done
  [[ -n "$reloaded" ]] || { printf '[offline] missing old→new active/retired hot reload log\n' >&2; docker logs "$HOST" 2>&1 | grep -Ei 'plugin|zen|version|error|config' | cut -c1-300 || true; exit 1; }
  printf '[offline] %s\n' "$reloaded"
  after="$(docker inspect --format '{{.State.Pid}}|{{.State.StartedAt}}|{{.RestartCount}}' "$HOST")"
  [[ "$before" == "$after" && "$after" == *'|0' ]] || { printf '[offline] isolated Host restarted during hot switch: before=%s after=%s\n' "$before" "$after" >&2; exit 1; }
  docker run --rm --network "$NET" --env HTTP_PROXY= --env HTTPS_PROXY= --env ALL_PROXY= --env NO_PROXY='*' \
    -v "$GATE/cases.py:/cases.py:ro" "$PY_IMAGE" python3 /cases.py --mode hot-new
  printf '[offline] hot switch PASS; same synthetic Host PID/start/restarts=%s; no rollback assumed\n' "$after"
fi
printf '[offline] gate PASS; temporary containers, network and staged files will be removed\n'
