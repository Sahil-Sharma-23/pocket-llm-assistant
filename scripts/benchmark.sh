#!/usr/bin/env bash
# Collect CPU, RAM, storage, thermal/battery and runtime information.
#
# Usage: scripts/benchmark.sh [--cpu-test]
#   --cpu-test  also time a short single-core hashing workload (~5-20 s) to
#               get a comparable CPU baseline (useful before/after thermal load)
#
# Output is printed and saved to logs/benchmark-<timestamp>.txt.
# Every probe is guarded: Android 16 SELinux denies apps access to several
# /proc and /sys files (e.g. /proc/stat, /proc/loadavg, thermal zones), and
# those are reported as "unavailable" instead of failing the script.
set -uo pipefail

PA_HOME="${PA_HOME:-$(cd "$(dirname "$0")/.." && pwd)}"
CPU_TEST=0
for arg in "$@"; do
  case "$arg" in
    --cpu-test) CPU_TEST=1 ;;
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

mkdir -p "$PA_HOME/logs"
OUT="$PA_HOME/logs/benchmark-$(date +%Y%m%d-%H%M%S).txt"
exec > >(tee "$OUT") 2>&1

have() { command -v "$1" >/dev/null 2>&1; }
section() { printf '\n=== %s ===\n' "$1"; }
kv() { printf '%-24s %s\n' "$1" "$2"; }
readf() { # readf <file> -> contents or "unavailable"
  local v
  if v="$(cat "$1" 2>/dev/null)" && [ -n "$v" ]; then echo "$v"; else echo "unavailable"; fi
}
prop() { have getprop && getprop "$1" 2>/dev/null || echo "n/a"; }

section "Run"
kv "timestamp" "$(date -Iseconds 2>/dev/null || date)"
kv "host" "$(uname -n)"
kv "termux_version" "${TERMUX_VERSION:-not termux}"
kv "prefix" "${PREFIX:-n/a}"

section "Device"
kv "model" "$(prop ro.product.model)"
kv "manufacturer" "$(prop ro.product.manufacturer)"
kv "soc" "$(prop ro.soc.manufacturer) $(prop ro.soc.model)"
kv "android" "$(prop ro.build.version.release) (sdk $(prop ro.build.version.sdk))"
kv "abi" "$(prop ro.product.cpu.abi)"
kv "kernel" "$(uname -r)"
kv "arch" "$(uname -m)"
if have getconf; then kv "page_size" "$(getconf PAGE_SIZE 2>/dev/null || echo n/a)"; fi

section "CPU"
kv "cores (nproc)" "$(nproc 2>/dev/null || echo n/a)"
kv "online" "$(readf /sys/devices/system/cpu/online)"
for d in /sys/devices/system/cpu/cpu[0-9]*; do
  [ -d "$d/cpufreq" ] || continue
  c="${d##*/}"
  max="$(readf "$d/cpufreq/cpuinfo_max_freq")"
  cur="$(readf "$d/cpufreq/scaling_cur_freq")"
  gov="$(readf "$d/cpufreq/scaling_governor")"
  [ "$max" != "unavailable" ] && max="$((max / 1000)) MHz"
  [ "$cur" != "unavailable" ] && cur="$((cur / 1000)) MHz"
  kv "$c" "max=$max cur=$cur gov=$gov"
done
# uptime(1) uses sysinfo(2), which works even when /proc/loadavg is denied.
kv "uptime/load" "$(uptime 2>/dev/null | sed 's/^ *//' || echo unavailable)"
kv "/proc/loadavg" "$(readf /proc/loadavg)"

section "Memory"
if [ -r /proc/meminfo ]; then
  grep -E '^(MemTotal|MemFree|MemAvailable|Cached|SwapTotal|SwapFree|Committed_AS):' /proc/meminfo |
    awk '{printf "%-24s %8.0f MB\n", $1, $2/1024}'
else
  echo "/proc/meminfo unavailable"
fi

section "Storage"
df -h "$HOME" "${PREFIX:-/}" 2>/dev/null | awk 'NR==1 || !seen[$0]++'
for d in data logs models bin; do
  [ -d "$PA_HOME/$d" ] && kv "$d/" "$(du -sh "$PA_HOME/$d" 2>/dev/null | cut -f1)"
done

section "Thermal / Battery"
zones=0
for z in /sys/class/thermal/thermal_zone*; do
  [ -r "$z/temp" ] || continue
  t="$(cat "$z/type" 2>/dev/null)"; v="$(cat "$z/temp" 2>/dev/null)"
  [ -n "$v" ] && kv "$t" "$((v / 1000)) C" && zones=$((zones + 1))
done
[ "$zones" -eq 0 ] && kv "thermal zones" "unavailable (SELinux); use termux-battery-status or adb"
if have termux-battery-status; then
  # Requires the Termux:API app; times out if it is not installed.
  bat="$(timeout 10 termux-battery-status 2>/dev/null | tr -d '\n ' || true)"
  kv "battery" "${bat:-unavailable (install the Termux:API app)}"
else
  kv "battery" "unavailable (pkg install termux-api + Termux:API app)"
fi

section "Toolchain"
kv "go" "$(have go && go version || echo missing)"
kv "sqlite3" "$(have sqlite3 && sqlite3 --version | cut -d' ' -f1 || echo missing)"
if have sqlite3; then
  if sqlite3 :memory: "CREATE VIRTUAL TABLE t USING fts5(x);" >/dev/null 2>&1; then
    kv "sqlite fts5" "available"
  else
    kv "sqlite fts5" "NOT available"
  fi
fi
kv "git" "$(have git && git --version | cut -d' ' -f3 || echo missing)"
kv "make" "$(have make && make --version | head -n1 || echo missing)"
kv "curl" "$(have curl && curl --version | head -n1 | cut -d' ' -f1-2 || echo missing)"

section "Assistant"
BIN="$PA_HOME/bin/assistant"
if [ -x "$BIN" ]; then
  kv "binary" "$("$BIN" -version 2>/dev/null)"
  kv "binary size" "$(du -h "$BIN" | cut -f1)"
else
  kv "binary" "not built (make build)"
fi
PIDFILE="$PA_HOME/run/assistant.pid"
if [ -f "$PIDFILE" ] && pid="$(cat "$PIDFILE")" && kill -0 "$pid" 2>/dev/null; then
  if [ -r "/proc/$pid/status" ]; then
    awk '/^(VmRSS|VmHWM|VmSwap|Threads):/ {
           if ($1=="Threads:") printf "%-24s %s\n", "proc " $1, $2;
           else printf "%-24s %.1f MB\n", "proc " $1, $2/1024 }' "/proc/$pid/status"
  fi
  addr="$("$BIN" -config "${PA_CONFIG:-$PA_HOME/configs/config.json}" -check-config 2>/dev/null |
    sed -n 's/^ *"addr": *"\([^"]*\)".*/\1/p' | head -n1)"
  addr="${addr:-127.0.0.1:8787}"
  start_ns="$(date +%s%N)"
  body="$(curl -sS --max-time 5 "http://$addr/healthz" 2>&1)"
  end_ns="$(date +%s%N)"
  kv "healthz latency" "$(((end_ns - start_ns) / 1000000)) ms"
  echo "$body"
else
  kv "process" "not running"
fi

if [ "$CPU_TEST" = "1" ]; then
  section "CPU test (single core)"
  # 256 MB through sha256: deterministic, no disk I/O, single-threaded.
  if have sha256sum; then
    start_ns="$(date +%s%N)"
    head -c 268435456 /dev/zero | sha256sum >/dev/null
    end_ns="$(date +%s%N)"
    ms=$(((end_ns - start_ns) / 1000000))
    kv "sha256 256MB" "${ms} ms ($((256000 / (ms > 0 ? ms : 1))) MB/s)"
  else
    kv "sha256sum" "missing"
  fi
fi

printf '\nSaved to %s\n' "$OUT"
