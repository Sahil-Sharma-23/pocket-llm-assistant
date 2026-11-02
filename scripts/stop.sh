#!/usr/bin/env bash
# Gracefully stop the assistant: SIGTERM, wait, then SIGKILL as a last resort.
# A user-initiated stop also stops the watchdog (otherwise it would restart
# the assistant); the watchdog itself calls this with PA_FROM_WATCHDOG=1.
set -euo pipefail
# shellcheck source=env.sh
. "$(dirname "$0")/env.sh"

if [ "${PA_FROM_WATCHDOG:-0}" != "1" ]; then
  bash "$PA_HOME/scripts/watchdog.sh" --stop >/dev/null 2>&1 || true
fi

release_wake_lock() {
  if [ "$PA_WAKE_LOCK" = "1" ] && command -v termux-wake-unlock >/dev/null 2>&1; then
    termux-wake-unlock
  fi
}

if ! pid="$(pa_pid)"; then
  echo "assistant not running"
  rm -f "$PA_PIDFILE"
  release_wake_lock
  exit 0
fi

kill -TERM "$pid"
deadline=$((SECONDS + PA_STOP_TIMEOUT))
while kill -0 "$pid" 2>/dev/null; do
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "assistant (pid $pid) did not stop within ${PA_STOP_TIMEOUT}s; sending SIGKILL" >&2
    kill -KILL "$pid" 2>/dev/null || true
    break
  fi
  sleep 0.2
done

rm -f "$PA_PIDFILE"
release_wake_lock
echo "assistant stopped (pid $pid)"
