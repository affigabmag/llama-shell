# llama-shell

[![Latest release](https://img.shields.io/github/v/release/affigabmag/llama-shell)](https://github.com/affigabmag/llama-shell/releases/latest)

A terminal UI (TUI) shell for [Ollama](https://ollama.com), written in Go with 
[Bubble Tea](https://github.com/charmbracelet/bubbletea) + [Lipgloss](https://github.com/charmbracelet/lipgloss).
Single static executable, no runtime dependencies.

Talk to your local model from wherever you actually are: the terminal, a
browser tab, or Telegram on your phone — all three are the same agentic
chat, same tools, same conversation loop underneath.

<img width="1113" height="626" alt="image" src="https://github.com/user-attachments/assets/14a70184-6333-44b4-8992-7f8b92d9bd38" />
<img width="1113" height="626" alt="image" src="https://github.com/user-attachments/assets/71dcccef-e5c3-4dcf-a1d0-29b2fac7bedf" />



## Building

```
go build -ldflags "-X main.buildTime=$(date +%Y%m%d-%H%M%S) -X main.appVersion=v0.4.0" -o llama-shell.exe .
```

The footer shows the build timestamp (there's no user-facing semantic version display — it's a live build tag, not manually bumped). `appVersion` is separate and only used internally to check for updates — set it to match the git tag you're cutting the release from. Omitting it leaves the update checker permanently unable to tell if a newer release exists (it falls back to `"dev"`, which the checker always treats as "nothing to compare").

## Running

```
./llama-shell.exe
```

## Layout

- **Header** (fixed top): app title.
- **Footer** (fixed bottom): purple bar matching the header. Build timestamp (left), a clickable "GitHub" repo link (OSC-8 terminal hyperlink, middle) or a context-specific key hint in the agentic chat/tools/help screens, and on the right: Tavily/Telegram/web-server status flags (grey=off, yellow=configured-but-not-running, cyan=running/bound), then ollama installed/not-installed status with version (green if installed, red if not) — preceded by a blinking yellow `update` flag when a newer release is available.
- **Body**: the current screen. All menus support Up/Down + Enter navigation as well as the letter shortcut shown in brackets.
- **Banner**: "LLAMA-SHELL" rendered as solid-color block letters (blue, white, red, green, cyan, magenta, yellow, repeating by letter position — static, not animated) at the top of the main menu.

## Main menu

| Key | Screen |
|---|---|
| `l` | List models |
| `p` | Running models |
| `s` | Show model info |
| `d` | Device info |
| `h` | Help |
| `q` | Quit |

On first launch, the app shows the disclaimer and requires `[a] I agree` before continuing — declining (any other key) quits the app. If Ollama isn't installed, it then offers to install it: Linux runs the official one-line installer unattended, macOS/Windows open the correct download page (those are signed GUI installers, not scriptable).

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

- **Streaming replies**: parses Ollama's newline-delimited JSON stream chunk-by-chunk, so replies type out token-by-token instead of appearing all at once. A stream that gets cut off mid-generation without a final `done:true` (e.g. the backend crashed) surfaces as a real error instead of silently looking like an empty, successful reply.
- **Model warm-up status** shown in the banner: `○ waiting for model to finish loading...` while a background `ollama ps` poll hasn't seen it yet, flipping to `● model loaded and ready` the instant a real token, tool call, or completed turn arrives — typing is never blocked either way, this is purely informational for a cold-loading model that can otherwise look like the app is frozen.
- **61 tools** across files & archives, shell & processes, networking & web (`web_search` via a DuckDuckGo HTML scrape — no API key; `read_webpage`; `rss_feed`/`find_rss_feed` for structured headlines from a feed, discovered via a page's `<link rel="alternate">` tag rather than a guessed URL; `tavily_search`/`tavily_extract` when a Tavily key is set — see below; `get_web_ui_url` for the browser URL, LAN IPs included; `http_post`/`download_file`), system & environment (including `send_notification`/`read_registry`), git & ollama (including `git_commit`/`git_branch`/`pull_ollama_model`), vision & media (`take_screenshot`/`view_image`/`read_pdf`/`read_document`), data (`run_sql`/`read_csv`/`read_json`), and open/launch. Press **Alt+T** to browse the full list — numbered, grouped by category, `Tab`/`Shift+Tab` to select and auto-scroll, `Enter` for a detail view (real description + two example prompts), `Esc` back.
- **Vision**: type/paste a path to an existing image file, or press **Alt+V** to attach an image straight from the clipboard (falls back to pasting clipboard text if there's no image) — both base64-attach via Ollama's `images` field.
- **Tool mode (`Alt+M`)**, cycles `auto` (default) → `on` → `off`, shown live in the footer. `auto` skips tool-calling for any single message that attaches an image — at least `gemma4:e2b` garbles the image entirely when `tools` is also in the request, so a message can reliably have working vision or working tools, not both — then resumes tools on the next image-free message. `on` always tries tools anyway; `off` never sends them this chat.
- **Keys**: `Enter` send, `Up`/`Down`/`PgUp`/`PgDn`/`Home`/`End` scroll history (via a real `bubbles/viewport`, works even while the model is thinking), `Alt+V` paste, `Alt+T` tool categories, `Alt+M` cycle tool mode, `Alt+H` this keybind help dialog, `Esc` back to the model actions menu, `Ctrl+C` quit. `Alt+` instead of `Ctrl+` deliberately — browser-based terminals (e.g. viewing an LXC console over the web) often reserve `Ctrl+T`/`Ctrl+H`/`Ctrl+R`/`Ctrl+V` for tab/history/reload/paste at the browser level and never forward them to the app.
- **Fixed layout**: the transcript viewport fills the space between the header and the input line; `you>` always sits exactly 3 rows above the very bottom (footer, one blank line, `you>`), regardless of conversation length or scroll position — the viewport's own output is manually padded to its declared height before being placed, since `bubbles/viewport` doesn't pad short content itself, and a global `clampToLastLines` safety net in `View()` guarantees no single frame can ever exceed the terminal's actual height (an earlier bug let a huge streamed reply overflow the frame, which permanently desynced the terminal's scroll position from bubbletea's cursor tracking until restart — the header would just vanish).
- **`you>`/`modelName>` prefixes are colored, message bodies are grey** — the prefix is what tells you who's speaking, the text itself doesn't need to compete with it.
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

### Update (`u` from the help menu)

Checks `github.com/affigabmag/llama-shell`'s latest release on every startup and shows a blinking `update` flag in the footer (left of the ollama status) if a newer one is available for your OS/arch. Open `[u] update` from the help menu to see current vs. latest version and press `[u]` to download and install, or `[r]` to re-check.

Self-update mechanism (differs by OS since a running executable can't be freely overwritten):
- **Linux/macOS**: downloads the release zip, extracts the binary, and does one atomic rename over the running executable's file — allowed even while it's running.
- **Windows**: a running exe can't be renamed *onto*, but *itself* can be renamed — so it renames the running exe aside to `<exe>.old`, then renames the newly-downloaded binary into place. The leftover `.old` file is cleaned up automatically on the next launch.

### Setup wizard (`w` from the help menu)

A guided first-time setup: accept the disclaimer, install Ollama if it's not already on `PATH`, then choose which starter models to pull — `qwen2.5:1.5b` (~1 GB), `gemma2:2b` (~1.6 GB), `gemma4:e2b` (~7 GB). Every question is asked up front (skipping the Ollama-install question entirely if it's already installed); only after all of them are answered does anything actually start. `[esc]`/`[q]` cancels at any point during the questions, `[esc]`/`[a]` aborts the currently-running install/download without touching what's left unselected.

Before starting any download, it checks real free disk space on the drive Ollama actually stores models on (`~/.ollama/models`, or `$OLLAMA_MODELS` if set) against the combined size of everything selected plus a 20% margin — if there isn't enough room, it stops before downloading anything and says how much space is missing, rather than failing partway through a multi-gigabyte pull.

Either way, you need to restart llama-shell afterward to run the new version — the update swaps the file on disk, it doesn't reload the already-running process. Requires the binary to have been built with `-X main.appVersion=vX.Y.Z` matching a real release tag (see Building); otherwise there's no baseline version to compare and the checker never reports an update.

### Web UI (`b` from the help menu)

Serves the same agentic chat — same tools, same system prompt, same conversation loop — as a page in any browser instead of only in the terminal.

- Bound to `127.0.0.1` only, never your local network, and every request needs a random access token baked into the URL (shown once enabled) — without it every request gets a 403. Binding to localhost isn't a real security boundary against other software on the same machine, and the API underneath grants full local tool access (files, commands, network), so the token is the actual gate.
- Enabling picks a model: your last choice if still installed, else `gemma4:e2b`, else whatever else is installed; offers to download `gemma4:e2b` if nothing's installed at all.
- Modern chat layout (full-width messages, real markdown rendering, collapsible tool-call trace per turn) rather than a terminal-styled page — built after research into how Claude.ai/ChatGPT/Open WebUI actually lay out an agentic chat, not a straight skin of the TUI.
- Tool browser (search, grouped by category, collapsed by default, copy-to-clipboard on each example prompt) and a help panel, both reachable from the top bar.
- Status badges (ollama/Tavily/Telegram) and a rotating "thinking"/"loading model" indicator so a slow local model never just looks stuck.
- Mobile-responsive; the composer has a stop button that aborts an in-flight reply client-side.

### Telegram bot (`m` from the help menu)

Same agentic chat again, reachable from Telegram on your phone.

- Uses long-polling (`getUpdates`), not an incoming webhook — no public URL, no port forwarding, no tunnel, unlike the web server option. This is also why Telegram was picked over WhatsApp: WhatsApp's Cloud API needs a public HTTPS endpoint, and the unofficial alternative (Baileys) risks the account getting banned.
- Get a token from [@BotFather](https://t.me/BotFather) (`/newbot`), paste it into the settings screen. It auto-binds to whichever chat messages it first and ignores every other chat after that, so a stranger who finds the bot's username can't drive your local agent.
- Sends an instant "Got it — working on it..." acknowledgment plus Telegram's native "typing..." indicator (kept alive on a ticker) while a reply is in progress, so a slow model doesn't read as a dead bot.
- Token, bound chat, and model persist across restarts (`%LocalAppData%\llama-shell\telegram_config.json`).

### Tavily API key (`t` from the help menu)

Tavily ([tavily.com](https://www.tavily.com/)) is a search + scraping API built for agents. Setting a key here (get one at [app.tavily.com](https://app.tavily.com/)) enables `tavily_search` (real result content, not just snippets) and `tavily_extract` (clean article text — handles pages `read_webpage` can't: cookie walls, JS-only shells). Nothing else needs it; skip the screen if you don't want it.

## Known limitations

- GPU detection for the benchmark's CPU/GPU split comes from `ollama ps`, which only reports an aggregate percentage — it doesn't distinguish which physical GPU on a multi-GPU machine. On Windows, ollama only has a CUDA backend, so in practice "GPU" always means the NVIDIA card if present; integrated GPUs (Intel, etc.) are listed in Device Info but never used by ollama.
- Only ollama + Hugging Face are treated as real model sources. LM Studio, llama.cpp, koboldcpp, text-generation-webui etc. don't maintain their own catalogs (they all consume HF), so they'd just be aliases for the same HF listing. GPT4All does have its own catalog but runs through a different engine entirely — it's not wired up here since none of this app's actions (`run`/`kill`/`remove`) would work against it.

## License

Source-available, view-only — see [LICENSE](LICENSE). You can read the code and run it for personal use, but you may not modify it or redistribute a modified version. Provided with no warranty of any kind. Not affiliated with or endorsed by Ollama Inc. or Hugging Face. On first launch, the app requires you to accept this disclaimer before continuing (`[h] help` in the main menu to review it again later).
