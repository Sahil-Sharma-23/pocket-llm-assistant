# Architecture

One Go process plus one llama-server child, both inside Termux. Everything
is local except the explicit network tools (search, GitHub, monitors) and
Telegram's long polling.

```
             Telegram (long poll, allow-listed users)      Browser → http://127.0.0.1:8787
                          │                                          │
                 internal/telegram                          internal/dashboard (+ internal/api)
                          └──────────────┬───────────────────────────┘
                                         ▼
                              internal/agent.Orchestrator
             ┌──────────── /commands (deterministic, no LLM) ───────────┐
             │                                                          │
             │  free text → Planner.Route (keywords → LLM enum)        │
             │            → agent loop: Planner.Decide (JSON-schema)   │
             │            → Executor (allowlist, validation, perms,    │
             │                        approvals) → tools.Registry      │
             └──────────────────────────┬───────────────────────────────┘
                                        ▼
     internal/tools ── tasks · reminders · knowledge · preferences · web · github · uptime
           │                 │                         │
   internal/memory     internal/research        internal/scheduler ── reminders, monitors,
   (SQLite + FTS5,     (public-only dialer)      daily plan, digest, backup, maintenance
    migrations)                                          │
                                                  internal/notify ── Telegram + dashboard feed
     internal/llm: Guard (1 slot, queue, memory floor, timeout) → LlamaClient (loopback only)
                   Supervisor → llama-server child (restart, backoff, sleep-idle unload)
```

## Packages

| Package | Responsibility |
|---|---|
| `cmd/assistant` | Flags, signals (SIGINT/SIGTERM; SIGHUP deliberately not handled), timezone fix, exit codes |
| `internal/config` | Defaults → JSON → env; validation of every limit; loopback-only enforcement |
| `internal/app` | Wiring, lifecycle, background workers, job handlers, resource monitor, status |
| `internal/api` | Loopback HTTP server, `/livez`, `/healthz` (+ details), Host guard, panic recovery |
| `internal/dashboard` | Server-rendered UI, CSRF, async chat |
| `internal/telegram` | Bot API client (stdlib), allowlist, approval buttons, notifier |
| `internal/agent` | Agents, planner (routing + constrained decisions), executor, orchestrator, commands |
| `internal/tools` | Typed tool registry and all tools |
| `internal/llm` | `Provider` interface, llama.cpp client, guard, llama-server supervisor |
| `internal/memory` | SQLite store: conversations, messages, preferences, tasks, knowledge, runs, tool calls, approvals, jobs, monitor results |
| `migrations` | Embedded SQL migrations (applied in order, once each) |
| `internal/scheduler` | Persisted job runner (sleep-until-due, wake on change) |
| `internal/research` | Wikipedia/DuckDuckGo search, page fetching/extraction, SSRF-safe HTTP client |
| `internal/sysinfo` | `/proc/meminfo` and `/proc/<pid>/status` (what Android 16 still permits) |
| `internal/notify` | Notification fan-out |
| `internal/health` | Named checks with timeouts |
| `internal/logging` | slog + size-rotating file |


## Request flow (free text)

1. The interface calls `Orchestrator.Handle`. Runs are serialized per
   conversation and limited globally (`agents.max_concurrent_runs`).
2. The user message is persisted (and indexed for full-text search).
3. Routing: regex keywords pick an agent when exactly one matches. Otherwise
   one LLM call with an `enum` schema decides, falling back to `assistant`.
4. Agent loop (≤ `agents.max_steps`): `Planner.Decide` makes one
   grammar-constrained call. If the result is a tool call, the executor
   validates it, applies permissions, runs it or files an approval, records it
   in `tool_calls`, and the result is fed back. If it is `respond`, that is the
   final answer. If the steps run out, a respond-only call forces an answer.
5. The run (agent, status, LLM calls, tokens, timings) and the reply are persisted.

## Permission model

| Level | Behaviour | Tools |
|---|---|---|
| READ | Executes immediately | `task_list`, `plan_data`, `reminder_list`, `knowledge_search`, `conversation_search`, `preference_list`, `web_search`, `web_fetch`, `github_repo_status`, `http_check`, `monitor_status` |
| WRITE | Executes immediately unless the call is *consequential* | `task_create`, `task_complete`, `reminder_create`, `knowledge_save`, `preference_set`, `research_save`; `reminder_cancel` (consequential → approval) |
| SENSITIVE | Always needs approval | `task_delete`, `knowledge_delete` |

Approvals are delivered as Telegram buttons, on the dashboard Approvals page,
and through `/approve <id>` and `/reject <id>`. They expire after 24 h and
execute at most once.

## Concurrency and resource limits

- One inference at a time (`llm.Guard` slot), `llm.max_queue` waiters, then `ErrBusy`.
- `llm.min_free_mem_mb` before each inference and `min_free_mem_mb_to_start` before
  starting llama-server; violations are explicit errors shown to the user.
- `GOMEMLIMIT` 256 MB and `GOMAXPROCS` 2 for the Go process. It used about 17 MB RSS in operation.
- Bounded worker pools: scheduler (`max_workers`) and Telegram in-flight updates.
- SQLite: WAL mode, `synchronous=NORMAL`, `busy_timeout`, `_txlock=immediate`,
  and at most 4 connections.

## Failure handling

- **Android kills the assistant:** the watchdog restarts it (capped per hour).
  At startup, runs left `running` are marked failed and approvals left
  `approved` are marked failed (not re-executed). `start.sh` clears an
  orphaned llama-server.
- **Android kills llama-server:** the supervisor restarts it with exponential
  backoff. Requests in the meantime wait (`Ensure`) or fail with "model unavailable".
- **The model returns garbage:** grammar constraints prevent most of it.
  `DecodeJSON` handles truncation, code fences and surrounding prose. Failures
  become an honest user message, and the run is recorded as `error`.
- **Missed schedules** (phone asleep or app dead): one-shot reminders fire late
  with an explicit "delivered N min late" note; recurring jobs do not replay
  missed runs.
