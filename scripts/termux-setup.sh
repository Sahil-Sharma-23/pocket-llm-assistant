#!/usr/bin/env bash
# One-time (idempotent) Termux setup: packages, model, config, build, tests.
# Run from the project root inside Termux:
#
#   bash scripts/termux-setup.sh [--with-model] [--install-boot]
#
#   --with-model     also download + verify a GGUF model sized for this phone
#   --install-boot   install the Termux:Boot hook (auto-start after reboot)
#
# Model choice (override with PA_MODEL=default|small, or set all three of
# PA_MODEL_FILE / PA_MODEL_URL / PA_MODEL_SHA256 for a custom GGUF):
#   default  Gemma 4 E2B QAT Q4_0 (official, 3.35 GB)  - phones with >= 5 GB RAM
#   small    Gemma 4 E2B QAT UD-Q2_K_XL (2.19 GB)      - phones with less RAM
#
# Safe to re-run: existing configs/config.json and configs/secrets.env are kept.
set -euo pipefail

if [ -z "${TERMUX_VERSION:-}" ] && [ ! -d /data/data/com.termux ]; then
  echo "This script is meant to run inside Termux." >&2
  exit 1
fi
cd "$(dirname "$0")/.."
PA_HOME="$(pwd)"

WITH_MODEL=0
INSTALL_BOOT=0
for arg in "$@"; do
  case "$arg" in
    --with-model) WITH_MODEL=1 ;;
    --install-boot) INSTALL_BOOT=1 ;;
    -h|--help) sed -n '2,16p' "$0"; exit 0 ;;
    *) echo "unknown option: $arg" >&2; exit 2 ;;
  esac
done

# ---------------------------------------------------------------- device ---
ARCH="$(uname -m)"
CORES="$(nproc 2>/dev/null || echo 4)"
MEM_KB="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)"
MEM_MB=$((MEM_KB / 1024))
echo "==> Device: arch=$ARCH cores=$CORES ram=${MEM_MB} MB android=$(getprop ro.build.version.release 2>/dev/null || echo '?')"

case "$ARCH" in
  aarch64|x86_64) ;;
  *) echo "WARNING: $ARCH is a 32-bit userspace; models over ~2 GB may fail to load. Use PA_MODEL=small." >&2 ;;
esac

# Generation stops scaling beyond ~4 threads on phone CPUs (memory bound);
# prompt processing benefits from more. Config validation allows 1..8.
THREADS=$((CORES < 4 ? CORES : 4))
THREADS_BATCH=$((CORES < 8 ? CORES : 8))

if [ -z "${PA_MODEL:-}" ]; then
  # Phones sold as "6 GB" report ~5.4-5.8 GB MemTotal; "4 GB" ~3.6 GB.
  if [ "$MEM_MB" -ge 5000 ]; then PA_MODEL=default; else PA_MODEL=small; fi
fi
case "$PA_MODEL" in
  default)
    MODEL_FILE="gemma-4-E2B_q4_0-it.gguf"
    MODEL_URL="https://huggingface.co/google/gemma-4-E2B-it-qat-q4_0-gguf/resolve/main/$MODEL_FILE"
    MODEL_SHA256="fa401b55b07ee70a54c6dae3903c783a6e65064312529ea57175cb5f8dec6634"
    MODEL_SIZE="3.35 GB" ;;
  small)
    MODEL_FILE="gemma-4-E2B-it-qat-UD-Q2_K_XL.gguf"
    MODEL_URL="https://huggingface.co/unsloth/gemma-4-E2B-it-qat-mobile-GGUF/resolve/main/$MODEL_FILE"
    MODEL_SHA256="0a5bbc20f91f92da96ab4870fa71b356c45b8500a7b8b9c3e0eb48359b72da28"
    MODEL_SIZE="2.19 GB" ;;
  *) echo "PA_MODEL must be 'default' or 'small'" >&2; exit 2 ;;
esac
MODEL_FILE="${PA_MODEL_FILE:-$MODEL_FILE}"
MODEL_URL="${PA_MODEL_URL:-$MODEL_URL}"
MODEL_SHA256="${PA_MODEL_SHA256:-$MODEL_SHA256}"
echo "==> Model: $MODEL_FILE ($PA_MODEL)  threads=$THREADS threads_batch=$THREADS_BATCH"

# -------------------------------------------------------------- packages ---
echo "==> Installing packages"
# Non-interactive so the script also works when driven over adb (no TTY).
export DEBIAN_FRONTEND=noninteractive
APT_OPTS=(-y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold)
pkg update "${APT_OPTS[@]}"
pkg install "${APT_OPTS[@]}" golang sqlite git make curl llama-cpp termux-tools

echo "==> Toolchain"
go version
sqlite3 --version
llama-server --version 2>&1 | head -n1 || true

echo "==> Verifying SQLite FTS5"
if sqlite3 :memory: "CREATE VIRTUAL TABLE t USING fts5(x);" 2>/dev/null; then
  echo "FTS5: available"
else
  echo "FTS5: NOT available (knowledge search will fail)" >&2
fi

# ----------------------------------------------------------------- model ---
if [ "$WITH_MODEL" = 1 ]; then
  mkdir -p models
  if [ ! -f "models/$MODEL_FILE" ] || ! echo "$MODEL_SHA256  models/$MODEL_FILE" | sha256sum -c --status; then
    echo "==> Downloading $MODEL_FILE ($MODEL_SIZE, resumable; re-run to resume)"
    curl -fL -C - --retry 5 -o "models/$MODEL_FILE" "$MODEL_URL"
  fi
  echo "$MODEL_SHA256  models/$MODEL_FILE" | sha256sum -c
fi

# ---------------------------------------------------------------- config ---
if [ ! -f configs/config.json ]; then
  echo "==> Creating configs/config.json (tuned for this device)"
  sed -e "s/\"threads\": [0-9]*/\"threads\": $THREADS/" \
      -e "s/\"threads_batch\": [0-9]*/\"threads_batch\": $THREADS_BATCH/" \
      -e "s/\"model_path\": \"[^\"]*\"/\"model_path\": \"$MODEL_FILE\"/" \
      configs/config.example.json >configs/config.json
elif ! grep -q "\"model_path\": \"$MODEL_FILE\"" configs/config.json; then
  echo "NOTE: configs/config.json already exists and does not point at models/$MODEL_FILE;" >&2
  echo "      edit llm.server.model_path if you want to use it." >&2
fi
if [ ! -f configs/secrets.env ]; then
  echo "==> Creating configs/secrets.env (mode 600) - add your Telegram token there"
  cp configs/secrets.env.example configs/secrets.env
fi
chmod 600 configs/secrets.env

# ---------------------------------------------------------- test + build ---
echo "==> Tests (GOFLAGS=-p=2 keeps compile memory bounded)"
GOFLAGS=-p=2 make test

echo "==> Build"
GOFLAGS=-p=2 make build
./bin/assistant -version
./bin/assistant -config configs/config.json -check-config >/dev/null && echo "config: ok"

# ------------------------------------------------------------------ boot ---
if [ "$INSTALL_BOOT" = 1 ]; then
  echo "==> Installing Termux:Boot hook"
  mkdir -p "$HOME/.termux/boot"
  sed "s|^PA_HOME=.*|PA_HOME=\"$PA_HOME\"|" scripts/boot/10-personal-assistant \
    >"$HOME/.termux/boot/10-personal-assistant"
  chmod 700 "$HOME/.termux/boot/10-personal-assistant"
  echo "Installed ~/.termux/boot/10-personal-assistant (open the Termux:Boot app once)."
fi

echo
echo "Setup complete. Start with: bash scripts/start.sh && bash scripts/watchdog.sh --daemon"
