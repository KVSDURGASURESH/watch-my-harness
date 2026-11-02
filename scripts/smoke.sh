#!/usr/bin/env bash
# Smoke test for a built binary on a clean machine: fresh HOME, fixture transcripts,
# start the server, check health, setup, scan, overview and the archive round-trip.
# Usage: scripts/smoke.sh /path/to/watch-my-harness[.exe] [fixtures-dir]
set -euo pipefail
BIN="$1"; FIX="${2:-contract/fixtures/claude}"
T="$(mktemp -d)"; export HOME="$T/home" USERPROFILE="$T/home"; mkdir -p "$HOME/.claude/projects"
cp -R "$FIX"/. "$HOME/.claude/projects/"
PORT=$((20000 + RANDOM % 20000))
"$BIN" version
"$BIN" dashboard --no-open --port "$PORT" >"$T/server.log" 2>&1 &
PID=$!; trap 'kill $PID 2>/dev/null || true' EXIT
for i in $(seq 1 40); do curl -fs "http://127.0.0.1:$PORT/healthz" >/dev/null && break; sleep 0.5; done
curl -fs "http://127.0.0.1:$PORT/healthz" >/dev/null || { cat "$T/server.log"; echo "server did not start"; exit 1; }
J='-H Content-Type:application/json'
curl -fs "http://127.0.0.1:$PORT/api/sources" | grep -q '"setup_complete":false' || { echo "setup not pending on fresh home"; exit 1; }
curl -fs $J -X POST -d '{}' "http://127.0.0.1:$PORT/api/setup/complete" >/dev/null
for i in $(seq 1 60); do
  n=$(curl -fs "http://127.0.0.1:$PORT/api/overview" | grep -o '"turns":[0-9]*' | head -1 | cut -d: -f2 || true)
  [ "${n:-0}" -gt 0 ] && break; sleep 0.5
done
[ "${n:-0}" -gt 0 ] || { cat "$T/server.log"; echo "no messages after scan"; exit 1; }
echo "scanned messages: $n"
curl -fs "http://127.0.0.1:$PORT/" | grep -qi "WATCH MY HARNESS" || { echo "UI not served"; exit 1; }
ARC="$T/archive"; if command -v cygpath >/dev/null 2>&1; then ARC="$(cygpath -m "$ARC")"; fi   # Windows: Go needs a drive path
curl -fs $J -X POST -d "{\"dir\":\"$(printf '%s' "$ARC" | sed 's#\\#/#g')\"}" "http://127.0.0.1:$PORT/api/archive/settings" >/dev/null
curl -fs $J -X POST -d '{}' "http://127.0.0.1:$PORT/api/archive/snapshot" | grep -q '"ok":true'
ls "$ARC" | grep -q '\.db\.gz$'
curl -fs "http://127.0.0.1:$PORT/api/archive/export?format=csv" | head -2 | grep -q '^timestamp,'
echo "SMOKE OK"
