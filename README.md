# llama-shell

A terminal UI (TUI) shell for [Ollama](https://ollama.com), written in Go with 
[Bubble Tea](https://github.com/charmbracelet/bubbletea) + [Lipgloss](https://github.com/charmbracelet/lipgloss).
Single static executable, no runtime dependencies.

<img width="1113" height="626" alt="image" src="https://github.com/user-attachments/assets/14a70184-6333-44b4-8992-7f8b92d9bd38" />
<img width="1113" height="626" alt="image" src="https://github.com/user-attachments/assets/71dcccef-e5c3-4dcf-a1d0-29b2fac7bedf" />



## Building

```
go build -ldflags "-X main.buildTime=$(date +%Y%m%d-%H%M%S)" -o llama-shell.exe .
```

The footer shows this build timestamp (there's no semantic version — it's a live build tag, not manually bumped).

## Running

```
./llama-shell.exe
```

## Layout

- **Header** (fixed top): app title.
- **Footer** (fixed bottom): purple bar matching the header. Build timestamp (left), a clickable GitHub repo link (OSC-8 terminal hyperlink, middle) or a context-specific key hint in the agentic chat/tools/help screens, and ollama installed/not-installed status with version (right, green if installed, red if not).
- **Body**: the current screen. All menus support Up/Down + Enter navigation as well as the letter shortcut shown in brackets.
- **Banner**: "LLAMA-SHELL" rendered as solid-color block letters (blue, white, red, green, cyan, magenta, yellow, repeating by letter position — static, not animated) at the top of the main menu.

## Main menu

| Key | Screen |
|---|---|
| `l` | List models |
| `p` | Running models |
| `s` | Show model info |
| `d` | Device info |
| `h` | Help / disclaimer / log |
| `q` | Quit |

On first launch, the app shows the disclaimer and requires `[a] I agree` before continuing — declining (any other key) quits the app.

### List models (`l`)

Queries three sources every time it's opened (no cache — always fresh):

- **ollama** — locally installed models, via `ollama list`.
- **ollama-library** — the full public catalog scraped from `ollama.com/library` (~235 model families). Size shown is the available parameter sizes (e.g. `2b,7b,72b`), since the index page doesn't expose download size.
- **huggingface** — top 50 GGUF repos by downloads, via the public HF API. (HF hosts 100k+ GGUF repos — this is a slice, not the whole catalog.)

Columns: `NAME`, `SOURCE`, `LOCAL` (installed y/n, cross-referenced against the ollama source), `SIZE`, `CAPABILITIES` (via `ollama show`, e.g. `completion,vision,tools` — only populated for installed/`ollama`-source rows; library/HF entries show `-` since there's nothing to introspect until they're pulled).

Controls:
- Type to filter/search the list live.
- Up/Down to select, Enter to download (or shows "already installed" if local).
- Selecting an `ollama-library` entry with multiple parameter sizes opens a size picker first.
- Downloads show a real streaming progress bar (parsed from `ollama pull`'s own output — its raw ANSI/bar characters are stripped and the line is rebuilt from just label/percent/size/speed/eta) with `[c]` to cancel mid-download.
- `ctrl+r` to force a rescan.

### Running models (`p`)

Live `ollama ps` output, scrollable/selectable. Enter on a row asks to confirm, then stops that model (`ollama stop`). `r` to refresh.

### Show model info (`s`)

Runs `ollama show` against every locally installed model (progress bar while scanning) and caches the result (`%LocalAppData%\llama-shell\models_cache.json`) so it loads instantly next time. `r` forces a rescan ignoring the cache.

Table columns: `NAME`, `PARAMS`, `QUANT`, `CONTEXT`, `ARCH`, `SIZE`, `CAPABILITIES` (via `ollama show`'s "Capabilities" section, e.g. `completion,vision,audio,tools,thinking` — tells you at a glance whether a model is chat-only, multimodal, or tool-calling capable), `CPU/GPU`, `MATCH%` (the last two come from the benchmark — see below; `-` until you've run it).

Enter on a row opens an action menu:
- `[a]` run own agentic chat — see below, listed first.
- `[x]` run interactively — hands the terminal to `ollama run <model>` for a real chat session.
- `[r]` remove — `ollama rm`.
- `[k]` kill/stop — `ollama stop`.

The list is also automatically invalidated (forced rescan) the next time you open it after downloading a new model from the List models screen, so newly pulled models show up right away instead of a stale cached list. Cache entries from before the `CAPABILITIES` column existed won't have it populated until you press `r` to force a rescan.

### Agentic chat (`a` from Show model info)

llama-shell's own built-in chat + tool-calling agent — no Aider/OpenCode/Claude Code needed, talks to Ollama's `/api/chat` directly.

- **Streaming replies**: parses Ollama's newline-delimited JSON stream chunk-by-chunk, so replies type out token-by-token instead of appearing all at once.
- **42 tools** across files & archives, shell & processes, networking & web (including `web_search` via a DuckDuckGo HTML scrape, no API key), system & environment, git & ollama, and open/launch. Press **Ctrl+T** to browse the full list grouped by category; the chat header just shows a one-line count.
- **Keys**: `Enter` send, `Up`/`Down`/`PgUp`/`PgDn`/`Home`/`End` scroll history (via a real `bubbles/viewport`, works even while the model is thinking), `Ctrl+T` tool categories, `Ctrl+H` this keybind help dialog, `Esc` back to the model actions menu, `Ctrl+C` quit.
- **Fixed layout**: the transcript viewport fills the space between the header and the input line; `you>` always sits exactly 3 rows above the very bottom (footer, one blank line, `you>`), regardless of conversation length or scroll position — the viewport's own output is manually padded to its declared height before being placed, since `bubbles/viewport` doesn't pad short content itself, and a global `clampToLastLines` safety net in `View()` guarantees no single frame can ever exceed the terminal's actual height (an earlier bug let a huge streamed reply overflow the frame, which permanently desynced the terminal's scroll position from bubbletea's cursor tracking until restart — the header would just vanish).
- Tool-calling reliability depends entirely on the model — small/local models can refuse or hallucinate. The system prompt explicitly tells the model it has real file/system access and that tools are additive to normal conversation, not a replacement for it, after early tests showed models either refusing everything ("I'm just an AI") or refusing *everything else* once tools were mentioned ("I can only do file ops").

### Device info (`d`)

Static machine info: OS/arch, CPU model, logical core count, RAM, per-drive used/total space, GPU(s). Cached to disk (`device_cache.json`) so it's instant on repeat visits; `r` forces a rescan and refreshes the cache.

`b` — **benchmark all installed models**. This is the CPU/GPU-suitability scan:

1. Shows a red warning + `[y]/[n]` confirm (it loads and unloads every model, one at a time — can take a long time).
2. Models are benchmarked **smallest-first** (by size), so cancelling partway through still yields the most data points.
3. For each model: `ollama run <model> hi` (load + time it), read its live CPU/GPU split from `ollama ps`, `ollama stop` it, move to the next.
4. Progress bar + a live-growing results table (NAME / CPU-GPU / MATCH%) shown as each model finishes.
5. `[c]` cancels mid-run (kills the current model's process); whatever was measured so far is still saved to cache and shown on the cancellation screen.
6. Results are written into the same cache `show model info` reads, so its `CPU/GPU` and `MATCH%` columns populate from this pass.

**CPU/GPU column format**: since the column header already says "CPU/GPU", raw ollama output like `100% GPU` is shown as `100g`; `100% CPU` as `100c`; a genuine split like `44%/56% CPU/GPU` as `44/56` (order matches the header, no unit labels needed).

**Match score** (0-100%): `0.7 × GPU-share + 0.3 × load-speed-score` (load speed scored against a 30-second-to-zero heuristic). Higher = better fit for this machine.

## Known limitations

- GPU detection for the benchmark's CPU/GPU split comes from `ollama ps`, which only reports an aggregate percentage — it doesn't distinguish which physical GPU on a multi-GPU machine. On Windows, ollama only has a CUDA backend, so in practice "GPU" always means the NVIDIA card if present; integrated GPUs (Intel, etc.) are listed in Device Info but never used by ollama.
- Only ollama + Hugging Face are treated as real model sources. LM Studio, llama.cpp, koboldcpp, text-generation-webui etc. don't maintain their own catalogs (they all consume HF), so they'd just be aliases for the same HF listing. GPT4All does have its own catalog but runs through a different engine entirely — it's not wired up here since none of this app's actions (`run`/`kill`/`remove`) would work against it.

## License

Source-available, view-only — see [LICENSE](LICENSE). You can read the code and run it for personal use, but you may not modify it or redistribute a modified version. Provided with no warranty of any kind. Not affiliated with or endorsed by Ollama Inc. or Hugging Face. On first launch, the app requires you to accept this disclaimer before continuing (`help / disclaimer / log` in the main menu to review it again later).
