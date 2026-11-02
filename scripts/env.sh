# shellcheck shell=bash
# Shared environment for start/stop/status/watchdog. Source, do not execute.
# Every value can be overridden by exporting it before calling a script.

_script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export PA_HOME="${PA_HOME:-$(cd "$_script_dir/.." && pwd)}"
if [ -z "${PA_CONFIG:-}" ]; then
  if [ -f "$PA_HOME/configs/config.json" ]; then
    PA_CONFIG="$PA_HOME/configs/config.json"
  else
    PA_CONFIG="$PA_HOME/configs/config.example.json"
  fi
fi
export PA_CONFIG

PA_BIN="${PA_BIN:-$PA_HOME/bin/assistant}"
PA_RUN_DIR="${PA_RUN_DIR:-$PA_HOME/run}"
PA_PIDFILE="${PA_PIDFILE:-$PA_RUN_DIR/assistant.pid}"
PA_WATCHDOG_PIDFILE="${PA_WATCHDOG_PIDFILE:-$PA_RUN_DIR/watchdog.pid}"
PA_STDOUT_LOG="${PA_STDOUT_LOG:-$PA_HOME/logs/stdout.log}"
PA_SECRETS="${PA_SECRETS:-$PA_HOME/configs/secrets.env}"
PA_START_TIMEOUT="${PA_START_TIMEOUT:-30}"
PA_STOP_TIMEOUT="${PA_STOP_TIMEOUT:-30}"
# 1 = hold a Termux wake lock while running (keeps CPU awake; costs battery).
PA_WAKE_LOCK="${PA_WAKE_LOCK:-0}"

# Android does not export TZ to Termux; Go would otherwise use UTC.
if [ -z "${TZ:-}" ] && command -v getprop >/dev/null 2>&1; then
  _tz="$(getprop persist.sys.timezone 2>/dev/null)"
  [ -n "$_tz" ] && export TZ="$_tz"
fi

# pa_load_secrets exports variables from configs/secrets.env, refusing files
# that other users could read.
pa_load_secrets() {
  [ -f "$PA_SECRETS" ] || return 0
  local mode
  mode="$(stat -c %a "$PA_SECRETS" 2>/dev/null || stat -f %Lp "$PA_SECRETS")"
  if [ "$mode" != "600" ] && [ "$mode" != "400" ]; then
    echo "refusing $PA_SECRETS: permissions $mode (run: chmod 600 $PA_SECRETS)" >&2
    return 1
  fi
  set -a
  # shellcheck disable=SC1090
  . "$PA_SECRETS"
  set +a
}

# pa_addr prints the configured listen address (host:port) using the binary's
# own resolved config, so env overrides are honoured. No jq dependency.
pa_addr() {
  "$PA_BIN" -config "$PA_CONFIG" -check-config 2>/dev/null |
    sed -n 's/^ *"addr": *"\([^"]*\)".*/\1/p' | head -n1
}

# pa_pid_alive <pidfile> <name> prints the pid if that process is alive.
pa_pid_alive() {
  local file="$1" name="$2" pid
  [ -f "$file" ] || return 1
  pid="$(cat "$file" 2>/dev/null)"
  [ -n "$pid" ] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  # Guard against PID reuse after Android killed the process.
  if [ -r "/proc/$pid/cmdline" ] && ! tr '\0' ' ' <"/proc/$pid/cmdline" | grep -q "$name"; then
    return 1
  fi
  echo "$pid"
}

# pa_pid prints the assistant PID if alive.
pa_pid() { pa_pid_alive "$PA_PIDFILE" "assistant"; }
