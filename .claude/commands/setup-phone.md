---
description: Set up (or update) the personal assistant on a USB-connected Android phone. Everything except creating the Telegram bot
argument-hint: "[device-serial]"
---

# Set up the personal assistant on an Android phone

You are setting up this repository's local-first assistant on the user's
Android phone, which is connected to this computer over USB. You run on the
computer and drive the phone with `adb` and `scripts/adb-termux.sh`. Read
`README.md`, `docs/ANDROID.md` and `scripts/adb-termux.sh` once before you start.

Device serial requested by the user (may be empty): `$ARGUMENTS`

## Ground rules

- **Ask before anything destructive or disruptive:** uninstalling apps (it
  deletes their data), rebooting the phone, overwriting an existing
  `~/personal-assistant` that has local changes, installing software on this
  computer. Settings and steps that this guide marks as safe need no extra
  confirmation; say what you are applying as you go.
- **Never ask for or handle secrets in the chat.** The Telegram token goes into
  the phone through `scripts/adb-termux.sh secret`, which the user runs in a
  separate terminal (step 10). Never print `configs/secrets.env`.
- **Tell the user exactly when to look at or tap the phone** (USB-debugging
  prompt, install confirmations, unlocking after reboot). Keep the phone
  unlocked and the screen on during setup if possible.
- Use `scripts/adb-termux.sh run '…'` / `sh` for anything inside Termux. Run
  long steps (package install, model download, build) in the background and
  follow their output. Do not use fixed sleeps; poll for the condition you need.
- If a step fails, read the error, fix the cause, and re-run the step. Every
  step is idempotent. Do not skip verification.
- On Windows, run the scripts from Git Bash or WSL.
- Keep a short checklist of the steps below and report progress as you go.

## 1. Computer prerequisites

- `git`, `curl`, `tar` and `adb` must be available. `scripts/adb-termux.sh`
  finds `adb` on `PATH` or in the usual Android SDK locations.
- If `adb` is missing, offer to install Android platform-tools and ask before
  installing: macOS `brew install --cask android-platform-tools`;
  Debian/Ubuntu `sudo apt install adb`; Fedora `sudo dnf install android-tools`;
  Windows `winget install Google.PlatformTools`. Alternatively the user can
  download it from https://developer.android.com/tools/releases/platform-tools
  and point `ADB=/path/to/adb` at it.

## 2. Select the device

- Run `adb devices -l`.
  - **No device:** tell the user to enable Developer options (Settings →
    About phone → tap *Build number* 7 times), turn on **USB debugging**, use a
    data-capable cable, and set the USB mode to *File transfer* if nothing shows up.
  - **`unauthorized`:** ask them to unlock the phone and tap **Allow** on the
    *Allow USB debugging?* prompt (tick *Always allow from this computer*).
  - **More than one device:** use `$ARGUMENTS` if it names one; otherwise
    ask the user which serial to use. Then `export ANDROID_SERIAL=<serial>`
    for every following command.
- Run `scripts/adb-termux.sh check` and note: manufacturer, model, Android
  version / `sdk`, `abi`, `mem_total_kb`, and which Termux packages exist.
- Check free space: `adb shell df -h /data`. Free space must be ≥ 5 GB
  (≥ 4 GB on phones that will get the small model).
- Requirements (stop and explain if they are not met):
  - `sdk` ≥ 24 (Android 7).
  - `abi` is `arm64-v8a` or `x86_64`. On `armeabi-v7a`/`x86`, warn that only
    the small model may load, and continue only if the user agrees.
  - RAM: `mem_total_kb` ≥ 5,000,000 gets the default model. Between about
    3,300,000 and 5,000,000 it gets the small model. Below that, warn that it
    will probably not work well, and continue only if the user agrees.

## 3. Install Termux and Termux:Boot (GitHub builds)

The setup needs the **GitHub (debuggable) builds**, because only those allow
`adb run-as com.termux`.

- If `check` shows `com.termux=installed` and `termux_run_as=ok`, skip to the
  Termux:Boot part.
- If Termux is installed but `termux_run_as=refused` (an F-Droid or Play
  build): explain that it must be replaced with the GitHub build, and that
  **uninstalling deletes everything inside Termux**. Ask before you
  uninstall. With the user's consent: `adb uninstall com.termux.boot`,
  `adb uninstall com.termux.api` (only the ones present, since add-ons share
  the signing key), then `adb uninstall com.termux`. If they decline, stop and
  point them to *Quick start B* in the README.
- Download into a temporary directory on this computer:
  - Termux: the latest release from `https://api.github.com/repos/termux/termux-app/releases/latest`.
    Pick the asset matching the abi: `…+github-debug_<abi>.apk` (`arm64-v8a`,
    `armeabi-v7a`, `x86_64`, `x86`), falling back to `…_universal.apk`.
    Also download the `…_sha256sums` asset.
  - Termux:Boot: the latest release from `https://api.github.com/repos/termux/termux-boot/releases/latest`
    (`termux-boot-app_…+github.debug.apk` plus `checksums-sha256.txt`).
  - **Verify each APK's SHA-256 against the published checksum file** before
    installing. Never install an APK that does not match.
- Install with `adb install -r <apk>` (Termux first, then Termux:Boot). Some
  phones (Xiaomi, OPPO, Vivo, realme…) show an on-screen confirmation for USB
  installs, or need *Install via USB* enabled in Developer options. Tell the
  user to watch the screen. A signature-mismatch error means an old
  non-GitHub copy is still installed; go back to the uninstall step.

## 4. First start of Termux and Termux:Boot

- On Android 13+ (`sdk` ≥ 33): `adb shell pm grant com.termux android.permission.POST_NOTIFICATIONS`.
- Launch Termux: `adb shell monkey -p com.termux -c android.intent.category.LAUNCHER 1`.
  It installs its base system on first start, which needs internet. Poll
  `scripts/adb-termux.sh check` until `termux_bootstrapped=yes` (usually
  under 2 minutes). Ask the user to keep the screen on, and check the
  phone's network if it takes much longer.
- Launch Termux:Boot once so Android delivers the boot broadcast to it:
  `adb shell monkey -p com.termux.boot -c android.intent.category.LAUNCHER 1`.
  Then bring Termux back to the foreground with the first `monkey` command.
- Sanity check: `scripts/adb-termux.sh run 'echo ok; uname -m'` prints `ok`.

## 5. Background-execution settings (adb; safe)

Apply these and tell the user you are doing so. They are reversible system
settings, documented in `docs/ANDROID.md`:

```sh
adb shell dumpsys deviceidle whitelist +com.termux
adb shell dumpsys deviceidle whitelist +com.termux.boot
adb shell cmd appops set com.termux RUN_ANY_IN_BACKGROUND allow
adb shell am set-standby-bucket com.termux active
```

Phantom process killer: on `sdk` ≥ 32 run
`adb shell settings put global settings_enable_monitor_phantom_procs false`.
On `sdk` = 31 run `adb shell device_config set_sync_disabled_for_tests persistent`
and `adb shell device_config put activity_manager max_phantom_processes 2147483647`.
Verify each one using the table in `docs/ANDROID.md` (*Verifying the setup*).
A failing command on some OEM build is not fatal; note it and continue.

## 6. Push the code

- If `~/personal-assistant` already exists on the phone (`scripts/adb-termux.sh run 'ls personal-assistant'`),
  this is an update. `push` keeps `configs/config.json`, `configs/secrets.env`,
  `data/`, `models/` and the logs on the phone, and overwrites the source
  files. Proceed unless the user says they edited source files on the phone.
- Run `scripts/adb-termux.sh push` (target: `~/personal-assistant`).

## 7. Install packages, model, config and build (long; run in the background)

Tell the user the download size first: about 3.35 GB on phones with ≥ 5 GB
RAM, or 2.19 GB for the small model. The phone should be on Wi-Fi and
ideally charging. Then run:

```sh
scripts/adb-termux.sh run 'cd personal-assistant && bash scripts/termux-setup.sh --with-model --install-boot'
```

This takes 10–40 minutes, mostly the download, then tests and build. It
picks the model and thread counts from the phone's RAM and cores, creates
`configs/config.json` and `configs/secrets.env`, runs the tests, builds
`bin/assistant`, and installs the Termux:Boot hook with the correct path.
If it is interrupted (USB unplugged, network drop), simply run it again: the
model download resumes. On success it prints `Setup complete`.

## 8. Manufacturer-specific settings (on the phone; recommended)

Look up the phone's manufacturer from step 2 in the per-brand table in
`docs/ANDROID.md`. Give the user only the instructions for their brand.
Step 5 already covers Pixel and near-stock phones. Also suggest locking
Termux in Recents. Continue without waiting; these settings improve
reliability but are not needed to finish the setup.

## 9. Start and verify

Recommend a reboot, because it proves auto-start works. **Ask first.**

- **Reboot (recommended):** `adb reboot`, then `adb wait-for-device` and poll
  `adb shell getprop sys.boot_completed` until it prints `1`. **Ask the user
  to unlock the phone once.** On encrypted phones Termux:Boot runs only after
  the first unlock. Then poll for health, up to ~3 minutes:
  `adb forward tcp:8787 tcp:8787` and `curl -s http://127.0.0.1:8787/healthz`.
- **No reboot:** `scripts/adb-termux.sh run 'cd personal-assistant && bash scripts/start.sh && bash scripts/watchdog.sh --daemon'`,
  then the same health check. Say that the boot hook takes over from the next reboot.

If the health check fails, read `logs/boot.log`, `logs/stdout.log`,
`logs/assistant.log` and `logs/llama-server.log` in `~/personal-assistant`,
and `bash scripts/status.sh`. Fix the cause and retry. Low free memory at
llama-server start is the usual cause (`min_free_mem_mb_to_start`): close
other apps, or switch to the small model with
`PA_MODEL=small bash scripts/termux-setup.sh --with-model` and update
`llm.server.model_path`.

When healthy, tell the user the dashboard is at `http://127.0.0.1:8787`, both
in the phone's browser and on this computer while the `adb forward` is active.

## 10. Telegram (the user creates the bot; you finish the config)

1. Ask the user to open Telegram, message **@BotFather**, send `/newbot`, and
   follow the prompts. **They must not paste the token into this chat.**
2. Ask them to store it from a **separate terminal** in this repository
   (it prompts without echoing):

   ```sh
   scripts/adb-termux.sh secret PA_TELEGRAM_TOKEN
   ```

   (Add `ANDROID_SERIAL=<serial>` in front if several devices are attached.)
   Alternatively they can type it in Termux:
   `nano ~/personal-assistant/configs/secrets.env`. Wait until they say it is done.
3. Verify without revealing it:
   `scripts/adb-termux.sh run "grep -c '^PA_TELEGRAM_TOKEN=..*' personal-assistant/configs/secrets.env"` → `1`.
4. Enable Telegram in `configs/config.json`, which holds no secrets. Read it
   with `scripts/adb-termux.sh run 'cat personal-assistant/configs/config.json'`,
   set `telegram.enabled` to `true` on this computer (JSON-aware, e.g.
   `python3` or `jq`), write it back via `scripts/adb-termux.sh sh` (heredoc
   into the file), and validate with
   `scripts/adb-termux.sh run 'cd personal-assistant && ./bin/assistant -config configs/config.json -check-config >/dev/null && echo valid'`.
5. Restart: `scripts/adb-termux.sh run 'cd personal-assistant && bash scripts/stop.sh; bash scripts/start.sh && bash scripts/watchdog.sh --daemon'`.
   Check `/healthz` again; the Telegram section should show the bot running.
6. Ask the user to send any message to their bot. It answers with their
   **numeric Telegram user ID**, which is not a secret. Ask them for it, add it to
   `telegram.allowed_user_ids` the same way as in step 4, validate, and restart.
7. Ask them to send `/status`, then `/help`, to their bot to confirm it works.

## 11. Finish

Summarise:
- Device, Android version, chosen model, and thread settings.
- Which background settings were applied, and which per-brand settings the
  user still has to do on the phone.
- How to use it: Telegram, the dashboard, and the `/commands` from the README.
- How to update later: `git pull` here, then run `/setup-phone` again. It
  skips finished steps and keeps config, data and the model.
- Where the logs are, and that after any reboot the phone must be unlocked
  once for the assistant to start.
