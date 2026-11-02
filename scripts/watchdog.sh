#!/usr/bin/env bash
# Keep the assistant alive: every PA_WATCHDOG_INTERVAL seconds, check /livez
# and restart the assistant if it is down or hung. (The assistant itself
# supervises llama-server.) Restarts back off exponentially and are capped per
# hour so a crash loop cannot drain the battery.
#
#   scripts/watchdog.sh          run in the foreground
#   scripts/watchdog.sh --daemon run in the background (pidfile run/watchdog.pid)
#   scripts/watchdog.sh --stop   stop the background watchdog
set -uo pipefail
# shellcheck source=env.sh
. "$(dirname "$0")/env.sh"

INTERVAL="${PA_WATCHDOG_INTERVAL:-60}"
MAX_RESTARTS_PER_HOUR="${PA_WATCHDOG_MAX_RESTARTS:-6}"
LOG="$PA_HOME/logs/watchdog.log"
mkdir -p "$PA_RUN_DIR" "$PA_HOME/logs"

case "${1:-}" in
  --daemon)
    if pid="$(pa_pid_alive "$PA_WATCHDOG_PIDFILE" watchdog)"; then
      echo "watchdog already running (pid $pid)"
      exit 0
    fi
    nohup bash "$0" >>"$LOG" 2>&1 </dev/null &
    echo "watchdog started (pid $!), log: $LOG"
    exit 0
    ;;
  --stop)
    if pid="$(pa_pid_alive "$PA_WATCHDOG_PIDFILE" watchdog)"; then
      kill "$pid" && echo "watchdog stopped (pid $pid)"
    else
      echo "watchdog not running"
    fi
    rm -f "$PA_WATCHDOG_PIDFILE"
    exit 0
    ;;
esac

echo "$$" >"$PA_WATCHDOG_PIDFILE"
trap 'rm -f "$PA_WATCHDOG_PIDFILE"; exit 0' INT TERM

log() { printf '%s watchdog: %s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "$*"; }

restart_times=""
backoff=0
failures=0
log "started (interval ${INTERVAL}s, max ${MAX_RESTARTS_PER_HOUR} restarts/hour)"
while true; do
  addr="$(pa_addr)"
  if [ -n "$addr" ] && curl -fsS --max-time 10 "http://$addr/livez" >/dev/null 2>&1; then
    failures=0
    backoff=0
  else
    failures=$((failures + 1))
    # Tolerate one missed probe (long GC, CPU throttling) unless the process is gone.
    if [ "$failures" -ge 2 ] || ! pa_pid >/dev/null; then
      now=$(date +%s)
      kept=""
      count=0
      for t in $restart_times; do
        if [ $((now - t)) -lt 3600 ]; then
          kept="$kept $t"
          count=$((count + 1))
        fi
      done
      restart_times="$kept"
      if [ "$count" -ge "$MAX_RESTARTS_PER_HOUR" ]; then
        log "restart limit reached ($count in the last hour); waiting"
      else
        if pid="$(pa_pid)"; then
          log "assistant (pid $pid) not responding; stopping it"
          PA_FROM_WATCHDOG=1 bash "$PA_HOME/scripts/stop.sh" >/dev/null 2>&1
        fi
        log "starting assistant (restart #$((count + 1)) this hour)"
        if bash "$PA_HOME/scripts/start.sh"; then
          failures=0
        else
          log "start failed"
        fi
        restart_times="$restart_times $now"
        if [ "$backoff" -eq 0 ]; then backoff=30; else backoff=$((backoff * 2)); fi
        [ "$backoff" -gt 900 ] && backoff=900
        sleep "$backoff"
        continue
      fi
    fi
  fi
  sleep "$INTERVAL"
done
