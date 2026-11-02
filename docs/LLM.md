# Local LLM: evaluation, benchmarks and limitations

All numbers were measured on a **reference phone**: a mid-range device with
a MediaTek Dimensity 6300 (8 cores), 6 GB RAM and Android 16, running Termux
`llama-cpp` 0.8.0 (CPU backend). Nothing here is extrapolated. Your phone will
differ, so see [Tuning for your phone](#tuning-for-your-phone).

## Model selection

| Candidate | Source | Size | Licence | Result |
|---|---|---|---|---|
| **`gemma-4-E2B_q4_0-it.gguf`** | `google/gemma-4-E2B-it-qat-q4_0-gguf` (official) | 3.35 GB | Apache-2.0, not gated | **Selected** |
| `gemma-4-E2B-it-Q4_0.gguf` | `ggml-org/gemma-4-E2B-it-GGUF` | 2.84 GB | Apache-2.0 | Not tested |
| `gemma-4-E2B-it-qat-UD-Q2_K_XL.gguf` | `unsloth/gemma-4-E2B-it-qat-mobile-GGUF` | 2.19 GB | Apache-2.0 | Benchmarked; **used automatically on phones with < 5 GB RAM** (`PA_MODEL=small`) |

- Compatibility was verified, not assumed: `libllama.so` in Termux's
  `llama-cpp` package contains the `gemma4` architecture, the model loads, and
  `/health` returns 200.
- Integrity: SHA-256 checked against the Hugging Face LFS hash
  (`fa401b55…6634` for the default file, `0a5bbc20…da28` for the small one).
  `scripts/termux-setup.sh --with-model` repeats the check.
- Why the official QAT build: quantization-aware training keeps Q4 quality
  close to bf16, and it comes from the model publisher.

## Memory behaviour (the key constraint)

llama.cpp memory-maps the GGUF. Measured from `/proc/<pid>/status` with the model loaded:

| Metric | Value |
|---|---|
| VmRSS | 2.48 GB |
| RssFile (mmap'd weights, reclaimable by the kernel) | 2.43 GB |
| **RssAnon (non-reclaimable)** | **~50 MB** |
| VmSwap | ~90 MB |
| MemAvailable before / after load | 2.80 GB / 3.20 GB |

Because almost all of the model is file-backed, memory pressure makes the
model **slower** (pages are re-read from flash) rather than getting it
**killed**. That is why a 3.35 GB model is usable with roughly 2.8 GB free. The
assistant still refuses to *start* llama-server below
`llm.server.min_free_mem_mb_to_start` (1200 MB) and refuses *inference* below
`llm.min_free_mem_mb` (250 MB), with explicit errors.

`--sleep-idle-seconds 900` makes llama-server unload the model after 15
minutes idle and reload it on the next request (load time is about 11 s).

## Speed

`llama-bench`, pp64 = prompt processing of 64 tokens, tg32 = generating 32 tokens (tok/s):

| Model | Threads | pp64 | tg32 |
|---|---|---|---|
| Q4_0 QAT (selected) | 2 | 6.78 | 4.52 |
| Q4_0 QAT | 4 | 7.31 | 4.56 |
| Q4_0 QAT | 6 | 7.78 | 4.97 |
| Q4_0 QAT | 8 | 9.49 | 4.60 |
| Q2_K_XL | 4 | 8.76 | 5.62 |
| Q2_K_XL | 8 | 5.02 | 4.48 |

- CPU was ~77% idle system-wide during tests, so the bottleneck is the
  hardware's memory and compute, not competing apps.
- Defaults: `-t 4` (generation does not improve beyond 4 threads, and more
  threads mean more heat) and `-tb 8` (prompt processing gains a little).
- The Q2 build is only marginally faster and loses quality, so it is the
  default only where RAM requires it.
- `scripts/termux-setup.sh` caps the defaults at the device's core count when
  it creates `config.json` (`threads = min(4, cores)`, `threads_batch = min(8, cores)`).

Measured end-to-end through llama-server's OpenAI API:

| Request | Prompt tokens | Output tokens | Time |
|---|---|---|---|
| Routing decision (JSON schema), cold | 74 | 24 | ~15.7 s |
| Routing decision, warm | 66 | 24 | 14.7 s |
| 2-sentence answer | 35 | 43 | 13.1 s |
| Same answer, 30/35 tokens from prompt cache | 5 new | 41 | 9.5 s |

**Implication for design:** prompt processing at ~8 tok/s dominates. A
600-token prompt costs about 75 s before the first output token. The agent
system is therefore built to keep prompts small:

- Deterministic `/commands` never call the model.
- Keyword routing skips the router LLM call whenever unambiguous.
- Static instructions come first so llama.cpp's prompt cache reuses them across steps.
- Tool results are clipped (web pages to 1,500 characters).
- Chat history is limited to the last 6 messages, 300 characters each.

Typical natural-language requests take **20–60 s**; research with a page
fetch can take 1–3 minutes. Telegram shows "typing…" meanwhile, and the
dashboard chat polls.

## Structured output ("tool calling")

- llama-server's `response_format: json_schema` was verified on-device to
  constrain Gemma 4 output (it returned `{"agent": "productivity"}` for a
  reminder request).
- The agents use one constrained call per step. The schema is a `oneOf` over
  `{"action": "<tool>", "args": <that tool's schema>}` plus
  `{"action": "respond", "message": …}`, so the output is always parseable JSON.
- Arguments are still strictly decoded in Go (unknown fields rejected, then
  validated). The permission level always comes from the Go registry.

## Thinking mode

Gemma 4 emits its chain of thought into the answer unless thinking is
disabled. `--reasoning-budget 0` alone did **not** prevent this. Passing
`chat_template_kwargs: {"enable_thinking": false}` (set per request and as a
server default) did. Verified on-device.

## Tuning for your phone

1. Get an environment report: `scripts/benchmark.sh --cpu-test`.
2. Benchmark thread counts with the model you use (stop the assistant first so
   the numbers are not skewed):

   ```sh
   llama-bench -m models/<model>.gguf -p 64 -n 32 -t 2,4,6,8
   ```

3. Set `llm.server.threads` to the smallest value with near-best `tg32`
   (generation), and `threads_batch` to the best `pp64` (prompt processing).
   Big.LITTLE CPUs often do best with threads equal to the number of big cores.
4. On phones with 8 GB+ RAM you can raise `llm.context_size` (e.g. 8192); on
   4 GB phones keep 4096 or lower it and use the small model.

Please share results from other phones in an issue (`scripts/benchmark.sh`
output plus `llama-bench`).

## Thermal and battery

Recent Android versions use SELinux to block Termux from reading `/sys/class/thermal` and battery
sysfs. Temperature was therefore read over USB with `adb shell dumpsys battery`:
36.0 °C on the reference phone during the benchmarks, while charging. For long-running measurements,
install Termux:API (`termux-battery-status`, used automatically by
`scripts/benchmark.sh`) or keep the phone on USB and poll `dumpsys battery`.
A sustained-load thermal throttling study (more than 10 minutes of continuous
generation) has **not** been done yet.

## Limitations

- **Latency:** see above. This is a capable but slow assistant; commands are the fast path.
- **Small-model reasoning:** E2B makes mistakes with relative dates, multi-step
  plans and ambiguous requests. Mitigations: the current time is injected into
  every prompt, tool arguments are validated, every write is shown back to the
  user, and destructive actions need approval.
- **Context:** 4096 tokens (`llm.context_size`). Long documents are clipped,
  not summarised in chunks.
- **One request at a time:** a second request waits (queue of 4), then gets a
  clear "busy" error.
- **No vision or audio:** the multimodal projector (`mmproj`) is not loaded,
  which saves about 1 GB.
- **LiteRT-LM:** not evaluated yet. The `llm.Provider` interface allows adding
  it without touching the agents.
