# Running on Android: keeping the assistant alive

Termux is an ordinary Android app. Nothing guarantees that it, or the
processes it starts, keep running. This document lists what can stop the
assistant, the settings that reduce the risk on any Android phone, and how to
verify them. Menu names differ between manufacturers and Android versions;
[dontkillmyapp.com](https://dontkillmyapp.com) tracks the details per brand.

## What can kill the assistant

| Mechanism | Effect | Mitigation |
|---|---|---|
| Low-memory killer (lmkd) | SIGKILL with no warning when RAM runs low. **Biggest risk while the model is loaded.** | Model sized to RAM, small context, `runtime.memory_limit_mb`, watchdog + supervisor restart |
| Phantom process killer (Android 12+) | Kills child processes of apps (every Termux process is one) above 32 system-wide, or ones using a lot of CPU in the background | Disable it (below) |
| Battery optimization / app standby | Restricts or kills apps it considers unused | Battery "Unrestricted" / exemption (below) |
| Manufacturer task killers | Many OEM skins (Samsung, Xiaomi, OPPO, Vivo, Huawei…) add their own "sleeping apps" or auto-start blocking | Per-brand settings (below) |
| Doze / CPU suspend | Timers and network stall while the screen is off | `termux-wake-lock` (the boot hook takes one; costs a little battery) |
| User taps **Exit** on the Termux notification | Kills all Termux sessions and processes | Don't, or use `scripts/stop.sh` |
| Reboot / OS update | Everything stops | Termux:Boot hook (`scripts/boot/10-personal-assistant`) |

Because SIGKILL cannot be caught, **graceful shutdown is best-effort**. The
design assumes a crash can happen at any point:

- `scripts/start.sh` / `status.sh` detect stale pidfiles (they check
  `kill -0` and `/proc/<pid>/cmdline`), so a killed process does not block a
  restart.
- SQLite runs in WAL mode with transactions, so a kill cannot corrupt data.
- `scripts/watchdog.sh --daemon` restarts the assistant when `/livez` stops
  answering (at most 6 restarts/hour, with backoff); the assistant's supervisor
  restarts llama-server. At startup, interrupted runs and approvals are marked
  failed (approvals are never re-executed automatically).

On a reference phone, after `kill -9`, the supervisor restarted llama-server
in about 20 s and the watchdog restarted the assistant in under a minute. A
reminder that fell due during the outage fired right after recovery.

## Settings that can be applied over USB (adb, no root)

The `/setup-phone` Claude Code command applies these; you can also run them
yourself from a computer with USB debugging enabled:

```sh
# Battery-optimization exemption ("Unrestricted" on most phones)
adb shell dumpsys deviceidle whitelist +com.termux
adb shell dumpsys deviceidle whitelist +com.termux.boot
# Allow running in the background and keep the app out of restricted standby buckets
adb shell cmd appops set com.termux RUN_ANY_IN_BACKGROUND allow
adb shell am set-standby-bucket com.termux active
# Android 13+: let Termux show its foreground notification
adb shell pm grant com.termux android.permission.POST_NOTIFICATIONS
```

**Phantom process killer** (Android 12 and later):

```sh
# Android 12L / 13 and later
adb shell settings put global settings_enable_monitor_phantom_procs false
# Android 12 (API 31) only
adb shell device_config set_sync_disabled_for_tests persistent
adb shell device_config put activity_manager max_phantom_processes 2147483647
```

On Android 14+ the same switch is in **Settings → Developer options →
Disable child process restrictions**. These settings persist across reboots,
but an OS update may reset them, so re-check after updates.

## Settings you must change on the phone (per manufacturer)

Set these for **Termux** and **Termux:Boot**. Also lock Termux in the
recent-apps view where the launcher supports it (open Recents → Termux icon or
long-press → **Lock** / **Keep open**).

| Brand / skin | Settings |
|---|---|
| **Google Pixel**, Android One, Motorola, Nokia, Nothing (near-stock) | App info → **App battery usage → Unrestricted**. Usually enough on its own. |
| **Samsung** (One UI) | App info → Battery → **Unrestricted**. Settings → Battery → **Background usage limits**: turn off *Put unused apps to sleep*, or add both apps to **Never sleeping apps**. |
| **Xiaomi / Redmi / POCO** (MIUI / HyperOS) | App info → **Autostart: on**; Battery saver → **No restrictions**; lock Termux in Recents. |
| **OnePlus / OPPO / realme** (OxygenOS / ColorOS / realme UI) | App info → Battery → **Allow background activity** (or *Unrestricted*) and **Allow auto launch**; lock Termux in Recents. |
| **Vivo / iQOO** (Funtouch / OriginOS) | Battery → Background power consumption → **Allow**; i Manager → App manager → **Autostart: on**. |
| **Huawei / Honor** (EMUI / MagicOS) | Battery → App launch → Termux → **Manage manually**: enable auto-launch, secondary launch and run in background. |

Optional, for a phone that stays on a charger permanently: enable the battery
protection / charge-limit option if your phone has one (often named *Protect
battery*, *Optimized charging* or *Battery health*). It caps charge at 80–85%.

## Verifying the setup

From a computer (`adb shell …`):

| Check | Command | Expected |
|---|---|---|
| Battery exemption | `dumpsys deviceidle whitelist \| grep termux` | lines for `com.termux` (and `com.termux.boot`) |
| Standby bucket | `am get-standby-bucket com.termux` | `5` (exempted) or `10` (active) |
| Background app-op | `cmd appops get com.termux RUN_ANY_IN_BACKGROUND` | `allow` |
| Phantom process monitor | `settings get global settings_enable_monitor_phantom_procs` | `false` |
| Boot hook | `run-as com.termux ls files/home/.termux/boot` (GitHub build) | `10-personal-assistant` |

The real test is a reboot. **Unlock the phone once after it boots.** On
encrypted phones (all modern ones) Android delivers the boot event, and
Termux's storage becomes readable, only after the first unlock. Afterwards `logs/boot.log` should show the hook
running, and `curl -s http://127.0.0.1:8787/healthz` (or `scripts/status.sh`)
should report healthy without you opening Termux.

## Termux notes

- **Install source.** Termux and its add-ons (Termux:Boot, Termux:API) must
  come from the same source (all GitHub releases or all F-Droid) because they
  share a signing key. The Google Play build is outdated. The guided setup
  uses the GitHub builds because they are debuggable, which lets
  `adb run-as com.termux` drive Termux from a computer.
- **Notification.** Termux keeps a foreground-service notification while it
  has sessions or a wake lock. That is what keeps it alive, so leave Termux's
  notifications enabled. Its **Acquire wakelock** button is equivalent to
  `termux-wake-lock`.
- **Termux:Boot.** Open the Termux:Boot app once after installing it, because
  Android does not deliver the boot broadcast to apps that have never been
  launched. Termux:Boot runs hooks *without* `termux-exec`, the library that
  makes `#!/usr/bin/env bash` work on Android, so the hook sets up the
  environment itself and calls every script through `bash`.
- **16 KB page-size warning.** Recent Android versions may warn that Termux
  "isn't 16 KB compatible". On devices with 4 KB pages (`getconf PAGE_SIZE`
  → `4096`, most phones today) this is informational.

## Monitoring limits inside Termux

Recent Android versions use SELinux to block apps from reading `/proc/stat`,
`/proc/loadavg`, `/proc/uptime`, `/sys/class/thermal/*` and
`/sys/class/power_supply/*`. For temperature and battery data:

- **Termux:API**: install the Termux:API app (same source as Termux) plus
  `pkg install termux-api`, then run `termux-battery-status`.
  `scripts/benchmark.sh` uses it automatically when present.
- **From a USB-connected computer**: `adb shell dumpsys battery` (temperature in
  tenths of °C) and `adb shell dumpsys thermalservice`.

`uptime(1)` still reports load averages because it uses the `sysinfo(2)`
syscall. Android's load average is normally high (often 20–30 at idle)
because it counts many kernel threads, so use it only for relative comparisons.

## Networking notes

- The assistant binds `127.0.0.1:8787` only. To view it from a computer over USB,
  run `adb forward tcp:8787 tcp:8787` and open `http://127.0.0.1:8787`.
- If you use Termux `sshd` for development, set `ListenAddress 127.0.0.1` in
  `$PREFIX/etc/ssh/sshd_config`, use key authentication only, and reach it with
  `adb forward tcp:8022 tcp:8022`. See [SECURITY.md](SECURITY.md).
- Go binaries must be built **in Termux** for DNS to work (see the README).

## Time zone

Termux does not export `TZ`, and **Go's Android runtime ignores `TZ` anyway
and sets `time.Local` to UTC**. The scripts export `TZ` from
`getprop persist.sys.timezone`. The assistant loads that zone explicitly with
`time.LoadLocation` (Android's tzdata works) and sets `time.Local`, so
reminders such as "6pm" mean 6 pm local time. You can also set `"timezone"`
in `config.json`.

See also: [LLM.md](LLM.md) for the memory and thermal behaviour of the model.
