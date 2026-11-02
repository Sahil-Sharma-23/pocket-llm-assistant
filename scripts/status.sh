#!/usr/bin/env bash
# Report process and health status.
# Exit codes: 0 healthy, 1 not running, 2 running but unhealthy.
set -uo pipefail
# shellcheck source=env.sh
. "$(dirname "$0")/env.sh"

if ! pid="$(pa_pid)"; then
  echo "assistant: not running"
  exit 1
fi

rss="unknown"
if [ -r "/proc/$pid/status" ]; then
  rss="$(awk '/^VmRSS:/ {printf "%.1f MB", $2/1024}' "/proc/$pid/status")"
fi
echo "assistant: running (pid $pid, rss $rss)"

addr="$(pa_addr)"
if body="$(curl -sS --max-time 5 -w '\n%{http_code}' "http://${addr}/healthz" 2>&1)"; then
  code="${body##*$'\n'}"
  echo "${body%$'\n'*}"
  [ "$code" = "200" ] && exit 0
fi
echo "health: FAILED" >&2
exit 2
