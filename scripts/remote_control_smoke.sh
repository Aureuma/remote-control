#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 3 ]]; then
  echo "usage: $0 <remote-control-bin> [session-id] [port]" >&2
  exit 64
fi

binary=$1
session_id=${2:-gha-smoke}
port=${3:-18182}
bind=127.0.0.1
tmpdir=$(mktemp -d)
pid=""

cleanup() {
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
  rm -rf "$tmpdir"
}

trap cleanup EXIT

export SI_REMOTE_CONTROL_HOME="$tmpdir"
export SI_REMOTE_CONTROL_RUNTIME_DIR="$tmpdir"

"$binary" start \
  --cmd cat \
  --bind "$bind" \
  --port "$port" \
  --id "$session_id" \
  --no-tunnel \
  --no-caffeinate \
  >"$tmpdir/start.log" 2>&1 &
pid=$!

for _ in $(seq 1 80); do
  if curl -fsS "http://$bind:$port/healthz" >"$tmpdir/health.json"; then
    break
  fi
  if ! kill -0 "$pid" 2>/dev/null; then
    echo "remote-control exited before health check" >&2
    cat "$tmpdir/start.log" >&2 || true
    exit 1
  fi
  sleep 0.25
done

if [[ ! -s "$tmpdir/health.json" ]]; then
  echo "health check did not succeed" >&2
  cat "$tmpdir/start.log" >&2 || true
  exit 1
fi

grep -q "\"id\":\"$session_id\"" "$tmpdir/health.json"

status_output=$("$binary" status 2>&1)
printf '%s\n' "$status_output" >"$tmpdir/status.txt"
grep -q "$session_id" "$tmpdir/status.txt"
grep -q "clients=0" "$tmpdir/status.txt"

stop_output=$("$binary" stop --id "$session_id" 2>&1)
printf '%s\n' "$stop_output" >"$tmpdir/stop.txt"
grep -q "$session_id" "$tmpdir/stop.txt"

wait_status=0
if ! wait "$pid"; then
  wait_status=$?
fi
if [[ $wait_status -ne 0 && $wait_status -ne 143 ]]; then
  echo "remote-control exited unexpectedly with status $wait_status" >&2
  cat "$tmpdir/start.log" >&2 || true
  exit "$wait_status"
fi
pid=""
