#!/usr/bin/env bash
# Manual SQLite backup / restore. The running assistant also writes
# automatic backups (backup.interval, default daily, keeps backup.keep).
#
#   scripts/backup.sh                 create backups/assistant-manual-<ts>.db
#   scripts/backup.sh --list          list backups
#   scripts/backup.sh --restore FILE  stop assistant, restore FILE, start again
set -euo pipefail
# shellcheck source=env.sh
. "$(dirname "$0")/env.sh"

DB="${PA_DB:-$PA_HOME/data/assistant.db}"
DIR="${PA_BACKUP_DIR:-$PA_HOME/backups}"
mkdir -p "$DIR"

case "${1:-}" in
  --list)
    ls -lh "$DIR"/assistant-*.db 2>/dev/null || echo "no backups in $DIR"
    ;;
  --restore)
    src="${2:?usage: backup.sh --restore FILE}"
    [ -f "$src" ] || { echo "no such file: $src" >&2; exit 1; }
    command -v sqlite3 >/dev/null || { echo "sqlite3 required (pkg install sqlite)" >&2; exit 1; }
    if [ "$(sqlite3 "$src" 'PRAGMA integrity_check;')" != "ok" ]; then
      echo "integrity check failed for $src; not restoring" >&2
      exit 1
    fi
    was_running=0
    pa_pid >/dev/null && was_running=1
    [ "$was_running" = 1 ] && bash "$PA_HOME/scripts/stop.sh"
    ts="$(date +%Y%m%d-%H%M%S)"
    if [ -f "$DB" ]; then
      mv "$DB" "$DIR/assistant-before-restore-$ts.db"
      rm -f "$DB-wal" "$DB-shm"
    fi
    cp "$src" "$DB"
    chmod 600 "$DB"
    echo "restored $src -> $DB (previous database saved as $DIR/assistant-before-restore-$ts.db)"
    [ "$was_running" = 1 ] && bash "$PA_HOME/scripts/start.sh"
    ;;
  "")
    command -v sqlite3 >/dev/null || { echo "sqlite3 required (pkg install sqlite)" >&2; exit 1; }
    [ -f "$DB" ] || { echo "no database at $DB" >&2; exit 1; }
    dest="$DIR/assistant-manual-$(date +%Y%m%d-%H%M%S).db"
    # .backup uses SQLite's online backup API: consistent while the app runs.
    sqlite3 "$DB" ".backup '$dest'"
    chmod 600 "$dest"
    echo "backup written: $dest ($(du -h "$dest" | cut -f1))"
    ;;
  *)
    sed -n '2,9p' "$0"
    exit 2
    ;;
esac
