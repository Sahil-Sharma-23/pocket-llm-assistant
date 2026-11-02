# Personal Assistant: a local-first AI agent for Android (Termux)

A personal AI assistant written in Go that runs **entirely on an Android
phone**. No root, no cloud LLM, and no fallback to one. A spare or old phone
that stays plugged in is ideal. Inference uses **Gemma 4 E2B** through a local
**llama.cpp** server inside [Termux](https://termux.dev).

- **Interfaces:** Telegram bot (long polling) and a local web dashboard at
  `http://127.0.0.1:8787`. Both use the same Go backend.
- **Agents:** Personal Assistant (routing and small talk), Productivity,
  Research, Developer, Knowledge. They all run in one Go process.
- **Tools:** a typed registry with READ / WRITE / SENSITIVE permissions and an
  approval workflow the model cannot bypass.
- **Memory:** SQLite (pure Go, WAL) with migrations and FTS5 full-text search.
- **Operations:** supervised llama-server, scheduler, watchdog, Termux:Boot
  auto-start, daily backups, log rotation, resource monitoring.

| Document | Contents |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Components, request flow, permission model, failure handling |
| [docs/LLM.md](docs/LLM.md) | Model choice, reference benchmarks, memory behaviour, limitations |
| [docs/SECURITY.md](docs/SECURITY.md) | Threat model, controls, residual risks |
| [docs/ANDROID.md](docs/ANDROID.md) | Keeping it alive on Android: background limits and per-manufacturer battery settings |

## Requirements

| | Minimum | Recommended |
|---|---|---|
| Android | 7.0 (Termux's minimum) | 10+ |
| CPU | 64-bit ARM (`arm64-v8a`) or x86_64 | 8 cores |
| RAM | 4 GB (uses the small 2.19 GB model) | 6 GB+ (default 3.35 GB model) |
| Free storage | 4 GB | 6 GB |
| Power | | Kept on a charger |

The setup script picks the model and CPU thread count from the phone's RAM and
core count. 32-bit-only phones are not supported for the bundled models.

**What to expect.** Measured on a mid-range 6 GB phone (MediaTek Dimensity 6300).
Faster SoCs do better:

- Prompt processing runs at about **8 tok/s** and generation at about **4.6 tok/s**.
  Natural-language requests take **~20–60 s**; web research takes 1–3 min.
- **`/commands` are instant and need no AI**, so use them for routine actions.
- The assistant uses about 17 MB RSS. llama-server uses about 2.5 GB RSS, but
  almost all of it is memory-mapped weights the kernel can reclaim.

---

## Quick start A: guided setup with Claude Code (recommended)

[Claude Code](https://claude.com/claude-code) runs on your computer and sets up
the phone over USB. It checks the device, installs Termux and Termux:Boot,
applies the background-execution settings that can be applied over adb, pushes
the code, installs packages, downloads and verifies the model, builds, runs the
tests, and installs the auto-start hook. **The only manual part is creating
the Telegram bot** (it needs your Telegram account).

1. **On the phone:** enable *Developer options* (Settings → About phone → tap
   *Build number* 7 times), then turn on **USB debugging**.
2. **On your computer:** install [Android platform-tools](https://developer.android.com/tools/releases/platform-tools)
   (`adb`), [Git](https://git-scm.com) and Claude Code.
3. Connect the phone by USB and accept the *Allow USB debugging?* prompt.
4. Clone this repository and start Claude Code in it:

   ```sh
   git clone https://github.com/Sahil-Sharma-23/pocket-llm-assistant.git
   cd pocket-llm-assistant
   claude
   ```

5. Run the setup command:

   ```
   /setup-phone
   ```

   Or paste this prompt:

   > Set up this project on my USB-connected Android phone by following
   > `.claude/commands/setup-phone.md` step by step. Ask me before anything
   > destructive, and leave the Telegram bot for me to do at the end.

Claude asks before anything that changes or reboots the phone, and it tells
you when to tap something on the screen (install prompts, opening Termux once).
You can run it again later to update the code.

## Quick start B: manual setup on the phone

1. Install **Termux** and **Termux:Boot** from the *same source*, either both
   from GitHub releases ([Termux](https://github.com/termux/termux-app/releases),
   [Termux:Boot](https://github.com/termux/termux-boot/releases)) or both from
   F-Droid. **Do not use the Google Play build.** Open Termux once and let it
   finish installing.
2. In Termux:

   ```sh
   pkg install -y git
   git clone https://github.com/Sahil-Sharma-23/pocket-llm-assistant.git ~/personal-assistant
   cd ~/personal-assistant
   bash scripts/termux-setup.sh --with-model --install-boot
   ```

   This installs `golang sqlite git make curl llama-cpp`, checks SQLite FTS5,
   downloads the model and checks its SHA-256, creates `configs/config.json`
   (tuned for the device) and `configs/secrets.env` (mode 600), runs the
   tests, builds `bin/assistant`, and installs the Termux:Boot hook.
3. Open the **Termux:Boot** app once. Android only delivers the boot event to
   apps that have been opened.
4. Apply the battery settings for your phone's brand in [docs/ANDROID.md](docs/ANDROID.md).
5. Start it:

   ```sh
   bash scripts/start.sh && bash scripts/watchdog.sh --daemon
   ```

## Telegram bot (manual, both paths)

1. In Telegram, talk to **@BotFather** → `/newbot` and copy the token.
2. Store the token on the phone using **one** of these:
   - in Termux: `nano ~/personal-assistant/configs/secrets.env` and set
     `PA_TELEGRAM_TOKEN=...`
   - from your computer, in a normal terminal (it prompts without echoing, so
     the token never appears in a chat or your shell history):
     `scripts/adb-termux.sh secret PA_TELEGRAM_TOKEN`
3. Set `"telegram": {"enabled": true, ...}` in `configs/config.json`, then
   restart: `bash scripts/stop.sh && bash scripts/start.sh`.
4. Message your bot. It replies with **your Telegram user ID** and answers no
   one until that ID is allow-listed. Put the ID in `configs/config.json`
   (`"allowed_user_ids": [123456789]`) and restart.

With the guided setup, Claude Code does steps 3–4 for you once the token is stored.

The bot shows "typing…" while the model works. Approvals arrive with
✅ Approve / ❌ Reject buttons. Reminders, monitor alerts and the daily plan
are pushed to you.

---

## Running

```sh
scripts/start.sh               # start (also starts and supervises llama-server)
scripts/status.sh              # pid, RSS, /healthz
scripts/watchdog.sh --daemon   # restart automatically if it dies or hangs
scripts/stop.sh                # graceful stop (also stops the watchdog)
```

- Dashboard: open `http://127.0.0.1:8787` in the phone's browser, or forward it
  to your computer with `adb forward tcp:8787 tcp:8787`.
- Health: `curl -s http://127.0.0.1:8787/healthz` returns checks plus LLM,
  memory, scheduler and Telegram details.
- The first model load takes 10–20 s. The model unloads after 15 minutes idle
  and reloads on the next request.
- After a reboot, **unlock the phone once**. The Termux:Boot hook then takes a
  wake lock and starts the assistant and the watchdog (log: `logs/boot.log`).

## Using it

Natural language works, for example "remind me to call mom tomorrow at 6pm",
"remember my locker code is 4412", "is example.com up?", "search for …".
Commands are faster:

| Command | |
|---|---|
| `/tasks`, `/task <title> [@ YYYY-MM-DD HH:MM]`, `/done <id>`, `/deltask <id>` | Tasks (delete needs approval) |
| `/remind <in 30m \| 18:00 \| YYYY-MM-DD HH:MM> <text>`, `/reminders` | Reminders |
| `/plan` | Today's plan (local model) |
| `/note <text>`, `/recall <query>` | Notes; full-text search over notes and past chats |
| `/search <query>` | Web research summarised locally ("From the web:" + sources) |
| `/github <owner/repo>`, `/check <url>`, `/monitors`, `/digest` | Developer monitoring |
| `/approvals`, `/approve <id>`, `/reject <id>` | Approval workflow |
| `/status`, `/help` | |

## Configuration

Edit `configs/config.json` and restart. Unknown keys are rejected. Validate with:

```sh
./bin/assistant -config configs/config.json -check-config
```

Key settings (see `configs/config.example.json` for all of them):

| Key | Default | Notes |
|---|---|---|
| `server.addr` | `127.0.0.1:8787` | Loopback only; anything else is refused |
| `timezone` | `""` → `$TZ` (set from Android by the scripts) | IANA name, e.g. `Europe/Berlin` |
| `llm.base_url` | `http://127.0.0.1:8080` | Must be loopback |
| `llm.server.managed` | `true` | The assistant runs and supervises llama-server |
| `llm.server.model_path` | chosen by setup | Relative to `models/` |
| `llm.server.threads` / `threads_batch` | min(4, cores) / min(8, cores) | Set by setup from the core count |
| `llm.server.sleep_idle_seconds` | 900 | Unload the model when idle |
| `llm.min_free_mem_mb` / `server.min_free_mem_mb_to_start` | 250 / 1200 | Refuse rather than overload |
| `llm.context_size`, `max_tokens` | 4096, 384 | |
| `agents.max_steps`, `history_messages`, `approval_ttl` | 3, 6, 24h | |
| `telegram.enabled`, `allowed_user_ids` | false, [] | Token comes from `$PA_TELEGRAM_TOKEN` |
| `scheduler.daily_plan_time` | `08:00` | Empty disables it |
| `developer.github_repos`, `digest_time` | [], "" | e.g. `["golang/go"]`, `"18:00"` |
| `developer.monitors` | [] | `{"name":"site","url":"https://…","interval":"5m","timeout":"10s","expect_status":200,"expect_json_key":"status"}` |
| `research.providers` | duckduckgo, wikipedia | Only queries and URLs leave the phone |
| `backup.interval`, `keep` | 24h, 7 | `backups/assistant-*.db` |

Secrets (`PA_TELEGRAM_TOKEN`, optional `PA_GITHUB_TOKEN`) live only in
`configs/secrets.env`, which must be mode 600 and is git-ignored.

**Choosing a model by hand:** `PA_MODEL=small bash scripts/termux-setup.sh --with-model`
(or `default`), or set `PA_MODEL_FILE`, `PA_MODEL_URL` and `PA_MODEL_SHA256` for
any GGUF that Termux's llama.cpp supports, then point `llm.server.model_path` at it.

## Operations

| Task | Command |
|---|---|
| Logs | `tail -f logs/assistant.log` (JSON, rotated at 10 MB × 5), `logs/llama-server.log`, `logs/watchdog.log`, `logs/boot.log`, or the dashboard **Logs** page |
| Execution history | Dashboard **Runs** (each run with its tool calls, tokens and timings) |
| Backup now | `scripts/backup.sh`; list with `scripts/backup.sh --list` |
| Restore | `scripts/backup.sh --restore backups/assistant-YYYYmmdd-HHMMSS.db` (integrity-checked; the current DB is kept) |
| Environment report | `scripts/benchmark.sh [--cpu-test]` |
| Update | `git pull` (or `scripts/adb-termux.sh push` from a computer), `make test && make build`, then `scripts/stop.sh && scripts/start.sh` |

Exit codes: 0 ok, 1 runtime error, 2 config error.

## Developing from a computer (USB)

```sh
make check                                   # fmt + vet + tests (macOS/Linux)
make test-race
scripts/adb-termux.sh check                  # device + Termux readiness
scripts/adb-termux.sh push                   # copy the tree to ~/personal-assistant
scripts/adb-termux.sh run 'cd personal-assistant && GOFLAGS=-p=2 make test build'
adb forward tcp:8787 tcp:8787                # dashboard in your computer's browser
```

`scripts/adb-termux.sh` uses `adb run-as com.termux`, so it needs the
debuggable **GitHub build** of Termux. With an F-Droid build, use SSH instead
(`pkg install openssh`, key auth, `adb forward tcp:8022 tcp:8022`, then
`make deploy TERMUX_SSH="ssh -p 8022 127.0.0.1"`).

> **Build production binaries in Termux, not with `make build-android`.**
> Go binaries cross-compiled with upstream Go fail DNS lookups on Android
> (`lookup … on [::1]:53`) because Android has no `/etc/resolv.conf`.
> Termux's patched Go toolchain resolves correctly. The Makefile sets
> `GOTOOLCHAIN=local` inside Termux so Go never swaps in an upstream toolchain.

## Layout

```
cmd/assistant/      entry point
internal/
  agent/            orchestrator.go planner.go executor.go registry.go commands.go
  llm/              provider.go llama.go types.go guard.go supervisor.go
  tools/            registry.go tasks.go reminders.go knowledge.go web.go github.go uptime.go
  memory/           store.go conversation.go knowledge.go tasks.go runs.go approvals.go jobs.go …
  api/ dashboard/ telegram/ scheduler/ config/ research/ sysinfo/ notify/ health/ logging/ app/
migrations/         embedded SQL migrations
configs/            config.example.json, secrets.env.example
scripts/            termux-setup, start/stop/status, watchdog, backup, benchmark, boot/,
                    adb-termux.sh (host-side USB helper)
docs/               ARCHITECTURE, LLM, SECURITY, ANDROID
.claude/commands/   setup-phone.md (guided setup for Claude Code)
data/ logs/ models/ backups/ run/   runtime state (git-ignored)
```

## Contributing

Issues and pull requests are welcome. Run `make check` before sending a change,
and keep the design constraints listed in [CLAUDE.md](CLAUDE.md): loopback-only
networking, no cloud LLM, permission levels fixed in Go, CGO-free builds.
Reports from phones other than the reference device are especially useful.
Please include the output of `scripts/benchmark.sh`.

## License

[MIT](LICENSE). Model weights are downloaded separately and are covered by
their own licenses: Gemma 4 is distributed under Apache-2.0 by Google.
Built on [llama.cpp](https://github.com/ggml-org/llama.cpp),
[Termux](https://termux.dev) and [modernc.org/sqlite](https://modernc.org/sqlite).
