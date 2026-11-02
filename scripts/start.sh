#!/usr/bin/env bash
# Start the assistant in the background and wait until /livez answers.
# Idempotent: exits 0 if already running. Used by the watchdog and Termux:Boot.
set -euo pipefail
# shellcheck source=env.sh
. "$(dirname "$0")/env.sh"

if pid="$(pa_pid)"; then
  echo "assistant already running (pid $pid)"
  exit 0
fi

if [ ! -x "$PA_BIN" ]; then
  echo "binary not found at $PA_BIN — run: make build" >&2
  exit 1
fi

pa_load_secrets

# Validate config in the foreground so errors are visible, not buried in logs.
if ! "$PA_BIN" -config "$PA_CONFIG" -check-config >/dev/null; then
  echo "config invalid: $PA_CONFIG" >&2
  exit 2
fi

mkdir -p "$PA_RUN_DIR" "$(dirname "$PA_STDOUT_LOG")"

if [ "$PA_WAKE_LOCK" = "1" ] && command -v termux-wake-lock >/dev/null 2>&1; then
  termux-wake-lock
fi

# Keep stdout.log bounded: it only captures panics and pre-logger output.
if [ -f "$PA_STDOUT_LOG" ] && [ "$(wc -c <"$PA_STDOUT_LOG")" -gt 5242880 ]; then
  mv -f "$PA_STDOUT_LOG" "$PA_STDOUT_LOG.1"
fi

# A SIGKILLed assistant can leave its llama-server child running and holding
# the port; clear orphans that serve models from this installation.
if command -v pkill >/dev/null 2>&1; then
  pkill -f "llama-server -m $PA_HOME/models/" 2>/dev/null && sleep 1 || true
fi

cd "$PA_HOME"
nohup "$PA_BIN" -config "$PA_CONFIG" >>"$PA_STDOUT_LOG" 2>&1 </dev/null &
pid=$!
echo "$pid" >"$PA_PIDFILE"

addr="$(pa_addr)"
url="http://${addr}/livez"
deadline=$((SECONDS + PA_START_TIMEOUT))
while [ "$SECONDS" -lt "$deadline" ]; do
  if ! kill -0 "$pid" 2>/dev/null; then
    echo "assistant exited during startup; last output:" >&2
    tail -n 20 "$PA_STDOUT_LOG" >&2 || true
    rm -f "$PA_PIDFILE"
    exit 1
  fi
  if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then
    echo "assistant started (pid $pid) on http://$addr"
    exit 0
  fi
  sleep 0.5
done

echo "assistant (pid $pid) did not become live within ${PA_START_TIMEOUT}s at $url" >&2
exit 1
