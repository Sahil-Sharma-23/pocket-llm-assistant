#!/usr/bin/env bash
# Host-side helper (macOS / Linux / Git Bash): drive Termux on a USB-connected
# phone through `adb run-as com.termux`. Used by the /setup-phone Claude Code
# command, and handy for development.
#
# Requires the GitHub (debuggable) build of Termux; F-Droid / Play builds are
# not debuggable, so run-as is refused for them.
#
#   scripts/adb-termux.sh check           device + Termux readiness (key=value)
#   scripts/adb-termux.sh sh  < script    run a bash script inside Termux (cwd: ~)
#   scripts/adb-termux.sh run 'cmd ...'   run one command line inside Termux
#   scripts/adb-termux.sh push [dir]      copy this source tree to ~/<dir>
#                                         (default: personal-assistant); runtime
#                                         state and configs/config.json /
#                                         secrets.env on the phone are kept
#   scripts/adb-termux.sh secret KEY [dir]
#                                         set KEY=value in ~/<dir>/configs/secrets.env
#                                         (prompts without echo; or value on stdin)
#
# Select a device with ANDROID_SERIAL=<serial> when several are attached.
# Override the adb binary with ADB=/path/to/adb.
set -euo pipefail

cd "$(dirname "$0")/.."

find_adb() {
  if [ -n "${ADB:-}" ]; then echo "$ADB"; return; fi
  if command -v adb >/dev/null 2>&1; then command -v adb; return; fi
  for c in "${ANDROID_HOME:-}/platform-tools/adb" "${ANDROID_SDK_ROOT:-}/platform-tools/adb" \
           "$HOME/Library/Android/sdk/platform-tools/adb" "$HOME/Android/Sdk/platform-tools/adb" \
           "${LOCALAPPDATA:-}/Android/Sdk/platform-tools/adb.exe"; do
    if [ -x "$c" ]; then echo "$c"; return; fi
  done
  echo "adb not found: install Android platform-tools or set ADB=/path/to/adb" >&2
  exit 1
}
ADB="$(find_adb)"

T_PREFIX=/data/data/com.termux/files/usr
T_HOME=/data/data/com.termux/files/home

# Environment of an interactive Termux session (minus the terminal).
prelude() {
  cat <<EOF
export PREFIX=$T_PREFIX HOME=$T_HOME
export PATH=$T_PREFIX/bin:/system/bin TMPDIR=$T_PREFIX/tmp LANG=en_US.UTF-8
for l in libtermux-exec-ld-preload.so libtermux-exec.so; do
  if [ -f "\$PREFIX/lib/\$l" ]; then export LD_PRELOAD="\$PREFIX/lib/\$l"; break; fi
done
cd "\$HOME"
EOF
}

# -T: no pty, so stdin/stdout are binary-safe and the exit code propagates.
termux_sh() { "$ADB" shell -T run-as com.termux "$T_PREFIX/bin/bash" -s; }

prop() { "$ADB" shell getprop "$1" | tr -d '\r'; }

cmd_check() {
  local state pkgs
  state="$("$ADB" get-state 2>/dev/null || true)"
  echo "adb=$ADB"
  echo "device_state=${state:-none}"
  if [ "$state" != "device" ]; then
    echo "hint=connect one phone with USB debugging on and accept the RSA prompt (or set ANDROID_SERIAL)"
    exit 1
  fi
  echo "manufacturer=$(prop ro.product.manufacturer)"
  echo "model=$(prop ro.product.model)"
  echo "android=$(prop ro.build.version.release)"
  echo "sdk=$(prop ro.build.version.sdk)"
  echo "abi=$(prop ro.product.cpu.abi)"
  echo "mem_total_kb=$("$ADB" shell "awk '/^MemTotal:/ {print \$2}' /proc/meminfo" | tr -d '\r')"
  pkgs="$("$ADB" shell pm list packages | tr -d '\r')"
  for p in com.termux com.termux.boot com.termux.api; do
    if grep -qx "package:$p" <<<"$pkgs"; then echo "$p=installed"; else echo "$p=missing"; fi
  done
  grep -qx "package:com.termux" <<<"$pkgs" || return 0
  if ! "$ADB" shell run-as com.termux id >/dev/null 2>&1; then
    echo "termux_run_as=refused"
    echo "hint=Termux is not the debuggable GitHub build; reinstall Termux and its add-ons from GitHub releases"
    return 0
  fi
  echo "termux_run_as=ok"
  if "$ADB" shell run-as com.termux ls "$T_PREFIX/bin/bash" >/dev/null 2>&1; then
    echo "termux_bootstrapped=yes"
  else
    echo "termux_bootstrapped=no"
    echo "hint=open the Termux app once and wait for its first-run install to finish"
  fi
}

cmd_push() {
  local dir="${1:-personal-assistant}" list
  case "$dir" in /*|*..*|*\'*|*' '*) echo "push: dir must be a simple path relative to the Termux home" >&2; exit 2 ;; esac
  list="$(mktemp)"
  trap 'rm -f "$list"' EXIT
  # git's view of the tree (tracked + untracked, minus .gitignore); outside a
  # checkout fall back to an explicit exclude list.
  if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    git ls-files -z -co --exclude-standard >"$list"
  else
    find . -type f \
      -not -path './.git/*' -not -path './bin/*' -not -path './data/*' -not -path './logs/*' \
      -not -path './models/*' -not -path './run/*' -not -path './backups/*' \
      -not -name 'config.json' -not -name 'secrets.env' -not -name '.DS_Store' -print0 >"$list"
  fi
  echo "pushing $(tr -cd '\0' <"$list" | wc -c | tr -d ' ') files to ~/$dir on the phone" >&2
  COPYFILE_DISABLE=1 tar --null -T "$list" -czf - |
    "$ADB" shell -T run-as com.termux "$T_PREFIX/bin/sh" -c \
      "'export PATH=$T_PREFIX/bin; mkdir -p $T_HOME/$dir && exec tar -xzf - -C $T_HOME/$dir'"
  echo "done" >&2
}

cmd_secret() {
  local key="${1:-}" dir="${2:-personal-assistant}" val
  [[ "$key" =~ ^[A-Z_][A-Z0-9_]*$ ]] || { echo "secret: KEY must look like PA_TELEGRAM_TOKEN" >&2; exit 2; }
  case "$dir" in /*|*..*|*\'*|*' '*) echo "secret: bad dir" >&2; exit 2 ;; esac
  if [ -t 0 ]; then
    read -rsp "$key (input hidden): " val; echo >&2
  else
    IFS= read -r val || true
  fi
  [ -n "$val" ] || { echo "secret: empty value, nothing changed" >&2; exit 1; }
  case "$val" in *[[:space:]\'\"\\\$\`]*) echo "secret: value contains unsupported characters" >&2; exit 1 ;; esac
  # The remote bash parses main() completely before running it, so its `read`
  # consumes the line *after* the script: the value never appears in argv.
  {
    prelude
    cat <<EOF
main() {
  IFS= read -r v
  f="\$HOME/$dir/configs/secrets.env"
  [ -d "\$HOME/$dir/configs" ] || { echo "~/$dir/configs not found (push the code first)" >&2; exit 1; }
  umask 077
  touch "\$f" && chmod 600 "\$f"
  { grep -v '^$key=' "\$f"; printf '%s=%s\\n' "$key" "\$v"; } >"\$f.tmp" && mv "\$f.tmp" "\$f"
  echo "$key set in ~/$dir/configs/secrets.env (mode 600)"
}
main
EOF
    printf '%s\n' "$val"
  } | termux_sh
}

case "${1:-}" in
  check) cmd_check ;;
  sh) { prelude; cat; } | termux_sh ;;
  run) shift; { prelude; printf '%s\n' "$*"; } | termux_sh ;;
  push) shift; cmd_push "$@" ;;
  secret) shift; cmd_secret "$@" ;;
  *) sed -n '2,22p' "$0"; exit 2 ;;
esac
