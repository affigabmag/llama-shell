# Changelog

All entries are from the initial build-out session (2026-08-22). No semantic
versioning — the app footer shows a build timestamp instead (see README).

## Web UI, Telegram bot, Tavily, RSS tools — ninth session (2026-08-25)

- **New: web UI** (`[b]` from the help menu) — the same agentic chat served
  as a browser page. Bound to `127.0.0.1` only and gated behind a random
  access-token query param (every request 403s without it) — binding to
  localhost isn't a real security boundary against other local software,
  and the API underneath grants full tool access, so the token is the
  actual gate. Enabling picks a model (last choice → `gemma4:e2b` →
  whatever's installed) and offers to pull `gemma4:e2b` if nothing's
  installed.
  - First cut mimicked the TUI's terminal look; explicitly redirected
    after feedback ("this is not a tui... research how web agentic
    should look like") — researched Open WebUI/ChatGPT/Claude.ai
    conventions (full-width messages over bubbles, collapsed-by-default
    tool-call trace, docked composer) and rebuilt around that instead.
  - Real minimal markdown renderer (bold, inline/fenced code, links,
    autolinked bare URLs) plus a specific normalization pass: a small
    model reliably wrote `**Headline**` followed by a redundant
    `[url](url)` line instead of `[Headline](url)`, so that exact shape
    gets collapsed into one real link regardless of how much the prompt
    asked it not to.
  - Tool browser: search box, grouped by category (collapsible,
    collapsed by default), numbered, copy-icon per example prompt.
  - Rotating "Thinking → Reasoning → Working through it → Almost done →
    (cycling)" status mirroring the TUI's ladder, switching to "loading
    model into memory" via an `/api/warmup` poll (`ollama ps` check) —
    same honesty principle as the TUI: no fake progress claims during
    model load, since there's no real signal for it.
  - Stop button (send button becomes ■ while busy) that aborts the
    in-flight fetch client-side.
  - Status badges (ollama/Tavily/Telegram) via a new `/api/status`
    endpoint; mobile-responsive breakpoint at 640px; emoji favicon.
- **New: Telegram bot** (`[m]` from the help menu) — same agentic chat,
  reachable from Telegram. Chosen over WhatsApp specifically because it
  supports long-polling (`getUpdates`): no incoming webhook, no public
  URL, no tunnel — WhatsApp's Cloud API needs a public HTTPS endpoint,
  and the unofficial Baileys alternative risks the account getting
  banned. Get a token from `@BotFather` (`/newbot`), paste it into the
  settings screen; the bot auto-binds to whichever chat messages it
  first and rejects every other chat after that, so a stranger who
  finds the bot's username can't drive the local agent. Sends an
  instant "Got it — working on it..." ack plus Telegram's native
  "typing..." indicator (refreshed on a ticker, since it only lasts
  ~5s per call) while a reply is in progress. Token/model/bound chat
  persist to `telegram_config.json` and auto-start on next launch.
  Added to the setup wizard as a y/n question alongside Tavily, chained
  so answering yes to both actually visits both settings screens in
  sequence.
- **New: Tavily integration** (`[t]` from the help menu) — `tavily_search`/
  `tavily_extract` tools, gated on a `TAVILY_API_KEY` persisted via its
  own settings screen (guide + masked key display), reused as the model
  for the wizard's Tavily question too.
- **New tools** (56 → 60): `rss_feed` (parses RSS/Atom into a clean
  title/link/date/summary list — real `encoding/xml`, not another
  regex scrape), `find_rss_feed` (discovers a site's real feed via its
  `<link rel="alternate">` tag instead of guessing a path),
  `tavily_search`, `tavily_extract`.
- **Fixed a real HTML-tag-stripping bug** in `read_webpage`/
  `stripHTMLTags`: the original regex tag-matcher broke the moment an
  attribute value (e.g. a large inline `style="..."`) or an HTML
  comment contained a literal `>` — Yahoo Finance/ynet pages leaked raw
  CSS into the model's context this way. Replaced with a quote- and
  comment-aware char-by-char scanner.
- **Fixed a native backend crash** (`GGML_ASSERT(n_inputs <
  GGML_SCHED_MAX_SPLIT_INPUTS)`, exit `0xc0000409`) hit by `gemma4:e2b`
  when handed this app's full 60-tool schema list in one request — a
  fixed scheduler limit in the backend, not a prompt-size issue. Now
  auto-retries the same turn with tools stripped instead of hard-failing
  (`shouldRetryWithoutTools`, extending the existing "model doesn't
  support tools" downgrade path). Also shrank the system prompt itself
  (~55-60%) as a second mitigation.
- **System-prompt hard rules added after repeated real-model testing**,
  each one verified against the actual model rather than assumed fixed:
  finance.yahoo.com/ynet.co.il must go through `rss_feed` on their known
  feed URLs, never `read_webpage` (both show a cookie-consent wall to a
  plain GET); any other site must try `find_rss_feed` before guessing a
  path; "top N" answers must contain exactly N items; every web-sourced
  item must cite its real URL *and* a content summary, not a bare link
  or a source name alone — broadened from "top N only" to "any web
  result" after a follow-up correction.
- **RTL (Hebrew/Arabic) display fix** in the TUI: such text is stored in
  logical order, but this terminal just prints codepoints left-to-right
  with no bidi support, so it rendered backwards. Fixed by reversing
  word order and per-word character order within the RTL span of a
  line (embedded numbers/URLs stay upright and just get repositioned) —
  a real, if simplified, bidi pass, not just a cosmetic tweak, and only
  touches lines that actually contain RTL characters.
- **Wizard**: no longer short-circuits on the first "no" — every
  question is asked through to the end regardless of any decline; only
  the disclaimer's answer is checked afterward to gate whether any
  actions run. Its own map-default-false trap (an unasked question
  reading as "declined") is guarded against explicitly. "Done" screen
  now shows a question/answer summary table before the action log.
- **Footer**: added explicit-always-shown Tavily/Telegram/web-server
  status flags (grey=off, yellow=configured-but-not-running,
  cyan=running/bound) rather than showing nothing when off, which read
  as ambiguous; the GitHub link now shows as "GitHub" instead of the
  full raw URL (gap-math updated to the shorter visible width); the
  same link added to the `[h] read help` screen.
- Real credentials (a live Tavily key, a Telegram bot token) got pasted
  into chat during this session by necessity of screen-sharing the
  setup flow — both are called out as compromised/rotate-if-unsure in
  the relevant commit context. Added a gitignored `CREDENTIALS.md` for
  local setup notes since this is a public repo.

## Setup wizard, disk-space gate, main-menu label simplified — eighth session (2026-08-23)

- **New `[w]` setup wizard**, from the help menu: accept disclaimer →
  install Ollama (question skipped entirely if already installed) →
  pick which of `qwen2.5:1.5b` / `gemma2:2b` / `gemma4:e2b` to download.
  Deliberately asks **every** question up front and only starts running
  anything once all are answered, rather than interleaving prompts with
  actions — a mid-run "install ollama?" prompt would block on user input
  right when a later step (a multi-GB pull) is the one that actually
  benefits from being left running unattended.
  - Declining the disclaimer question cancels the whole wizard
    immediately (same as the first-run disclaimer gate).
  - `[esc]`/`[q]` cancels during the question phase; `[esc]`/`[a]` aborts
    the currently in-progress install/pull during the run phase — kills
    the process, leaves whatever hasn't started yet un-started.
  - Reuses the existing `installOllama()` cmd for the install step, and
    a **dedicated** copy of the "list models" screen's own
    stream-parsing pull mechanism (`wizardPullModel`/
    `wizardPullChanMsg`/etc., sharing the pure `stripANSI`/
    `cleanPullLine` helpers but not the model fields) for downloads —
    kept separate on purpose so a wizard run can never collide with an
    unrelated download already in progress from the List Models screen.
- **Disk-space gate before starting anything**: sums the real download
  size of every selected model (`986 MB` qwen2.5:1.5b and `1.6 GB`
  gemma2:2b confirmed from ollama.com's own library pages; `7.2 GB`
  gemma4:e2b confirmed from the user's own already-installed copy via
  `ollama list` — not guessed), adds a 20% margin (ollama's blob store
  briefly holds compressed+decompressed data at once mid-pull), and
  compares against real free space on the drive Ollama actually stores
  models on (`$OLLAMA_MODELS` or `~/.ollama/models`) — not wherever
  llama-shell happens to be running from. If it doesn't fit, the wizard
  stops before downloading anything and says how much is missing.
  Verified against real disk state (not mocked): a live `diskFreeBytes()`
  call returned genuine free space (509.5 GB on the dev machine), the
  real ~9.8 GB combined total correctly passed, and an artificially
  inflated fake model size correctly triggered the "not enough space"
  path — via a throwaway test, deleted after use.
  - Free-space lookup shells out per OS rather than using
    `syscall.Statfs` — that type doesn't exist when this same file is
    cross-compiled for Windows, so it'd need a build-tag-split file;
    shelling out (`Get-PSDrive` on Windows, `df -Pk` elsewhere) matches
    the existing per-OS-branch pattern already used by `installOllama()`
    and `openInBrowser()` instead of introducing a new file-splitting
    convention just for this.
- **Main-menu `[h]` label simplified to plain `help`** (was
  `help / disclaimer / log / update`, which was about to grow a 5th
  segment for the wizard) — the help screen itself still lists every
  sub-item.

## `read_document` tool (Word/PDF/text, Tier-1 "RAG") — seventh session (2026-08-23)

- **New `read_document` agent tool**: extracts full plain text from a
  file and dispatches by extension — `.pdf` (existing `pdftotext`,
  reused as-is), `.docx` (new: parsed directly with `archive/zip` +
  `encoding/xml`, no external dependency), anything else read as plain
  text. `.doc` (legacy binary format) explicitly rejected with a message
  telling the user to convert to `.docx`/`.pdf` first — parsing it
  properly would need a real OLE/CFB parser, not worth it for how rare
  actual `.doc` files are now.
- **Why not full RAG (chunk + embed + vector search)**: for a handful of
  files that fit in context, dumping the whole extracted text into the
  chat (same as the existing `read_pdf` tool already does) is simpler and
  sufficient — no vector store, no separate embedding model, no chunking
  strategy to get wrong. Real RAG only earns its complexity once
  documents are too large/numerous to fit in context at once; that's a
  separate, larger feature if it's ever needed.
- **`.docx` extraction approach**: Word's format is a zip of XML parts;
  the visible text lives in `word/document.xml` as `<w:t>` runs grouped
  into `<w:p>` paragraphs — walked with a plain `xml.Decoder` token loop
  (no full-document unmarshal needed), emitting a newline per paragraph
  end and per `<w:br>`/`<w:cr>`, and a tab per `<w:tab>`. Table cells are
  just paragraphs nested one level deeper, so table text comes through
  for free with no special-casing.
- **Verified against a genuine `.docx`**, not a mocked one: built a real
  minimal Word-format zip (proper `[Content_Types].xml` +
  `word/document.xml` with split runs and a `<w:br/>`) and ran the actual
  `readDocxText()` against it in a throwaway test (deleted after use).
  Caught and fixed one real bug this way: `Compress-Archive` on Windows
  writes zip entry names with backslash separators (`word\document.xml`)
  instead of the zip-spec-standard forward slash real Word/LibreOffice
  always uses — harmless for real `.docx` files, but the entry-name
  match is now normalized (`strings.ReplaceAll(..., "\\", "/")`) anyway
  since it costs nothing and removes the assumption.
- Added to the `Vision & Media` tool category (alongside `read_pdf`) and
  the tool-browser example-prompts map; tool count in README bumped
  55 → 56.

## Self-update from GitHub releases — sixth session (2026-08-23)

- **Fixed a version-comparison bug found during a real end-to-end test**:
  `isNewerVersion()` originally just compared the current and latest
  version strings for *inequality*, which misfires whenever they differ in
  either direction — a locally built `v0.2.1-test` running against a real
  published `v0.2.0` was flagged as "update available" even though the
  installed copy was actually newer. Verified against the real
  `github.com/affigabmag/llama-shell` releases API (temporary `v0.2.1-test`
  → `v0.2.2-test` prerelease/release pair, deleted afterward) before
  fixing it. Replaced with `parseVersionParts()` + a real numeric
  major/minor/patch comparison that ignores any `-suffix`.
- **New `appVersion` build var**, parallel to the existing `buildTime` one:
  set via `-X main.appVersion=vX.Y.Z` at build time, tied to the git tag a
  release is cut from. Falls back to `"dev"` for a plain `go build`, in
  which case the update checker never reports an update — there's no
  tagged baseline to compare the latest release against.
- **Update check on every startup** (`Init()` → `checkForUpdate()`): one
  unauthenticated GET to `api.github.com/repos/affigabmag/llama-shell/releases/latest`.
  Pull-based only — a copy that's already running won't notice a new
  release until it's relaunched or `[r]` retry is pressed on the update
  screen.
- **New `[u] update` item** in the help menu (`h` → `u`), main-menu label
  updated to `help / disclaimer / log / update`. Shows current vs. latest
  version, `[u]` to download and install, `[r]` to re-check.
- **Blinking `update` flag in the footer**, immediately left of the
  ollama status, shown whenever a newer release is available — reuses the
  same `lipgloss .Blink(true)` SGR trick already used for
  "ollama: not installed" rather than a manual tick-driven blink loop.
- **Release-asset convention discovered from the real repo, not assumed**:
  `gh api repos/affigabmag/llama-shell/releases` showed `v0.2.0` assets are
  zips (`llama-shell-<goos>-<goarch>.zip`, e.g.
  `llama-shell-windows-amd64.zip`) containing the binary
  (`llama-shell.exe` on Windows, `llama-shell` on macOS/Linux) —
  **not** a raw binary per platform, which was the first (wrong,
  never-shipped) implementation. `updateAssetName()`/
  `updateBinaryNameInZip()` match this; `applyUpdateAt()` downloads to a
  temp `.zip`, opens it with `archive/zip`, and extracts by base name so
  it doesn't care whether the zip nests the binary in a subfolder.
- **Self-swap mechanism differs by OS**, confirmed empirically rather than
  assumed:
  - **Linux/macOS**: a single `os.Rename(new, exePath)` — renaming over a
    running executable's file is allowed; the kernel keeps the old inode
    open (and running) until the process actually exits.
  - **Windows**: a running exe is locked against a direct rename onto it,
    but *renaming the running exe itself* is allowed — verified live with
    a real running test binary (`Rename-Item` on a `Start-Process`'d exe
    succeeded). So the swap is two steps: rename the running exe to
    `<exe>.old`, then rename the newly-extracted binary into the original
    path. The leftover `.old` is deleted on next launch (`cleanupOldExe()`,
    called from `main()`), and also pre-emptively before each swap in case
    a prior update's cleanup never ran.
  - `applyUpdate()` is a thin `os.Executable()` wrapper around
    `applyUpdateAt(exePath, assetURL)` — split out specifically so the
    real production download/extract/rename code could be exercised in a
    throwaway test against the actual live `v0.2.0` release and a real
    running fake binary, instead of trusting the logic by inspection. Test
    passed end-to-end (real download, real extraction, real swap over a
    running process, swapped binary launches) and was deleted afterward —
    it was a one-time verification, not a kept test file.
- Update state lives on `model` (`updateChecked`, `updateAvailable`,
  `updateLatest`, `updateAssetURL`, `updateCheckErr`, `updateDownloading`,
  `updateResult`, `updateResultErr`) — same pattern as the existing ollama
  install-prompt flow.

## Tool browser overhaul, tool mode, stream-crash detection — fifth session (2026-08-23)

- **Tool-categories screen (`Alt+T`) was silently dropping ~20 tools**: it had
  no scrolling, so `clampToLastLines` cut whatever didn't fit off the *top* —
  exactly the "Files & Archives" and "Shell & Processes" categories. Fixed:
  - Every tool is now numbered (`1. read_file`, `2. write_file`, ...),
    sequential across all categories, derived from a single
    `flatToolNames()` source shared with the chat banner's tool count so the
    two can't drift apart.
  - `Tab` / `Shift+Tab` cycle a highlighted selection through all 55 tools,
    auto-scrolling to keep it on screen; `Up`/`Down`/`PgUp`/`PgDn`/`Home`/`End`
    still free-scroll independently.
  - `Enter` opens a detail view for the selected tool: its real description
    (pulled from the same `agentTools()` source sent to Ollama, not
    duplicated) plus two example prompts, closed with `Esc`.
- **Tool mode (`Alt+M`)**, cycles `auto` → `on` → `off` → `auto`, shown live
  in the footer (`tool mode: auto  Alt+H: help`):
  - `auto` (default): tools stay on normally but are automatically skipped
    for any single message that attaches an image. Confirmed via direct
    `/api/chat` testing that at least `gemma4:e2b` garbles the image
    entirely when `tools` is also in the request (works fine without it,
    "unreadable/corrupted characters" with it) — a message can reliably have
    working vision or working tools, not both, so `auto` always prefers
    vision when an image is present, then resumes tools on the next
    image-free message. Implemented as a per-turn `suppressTools` flag on
    `runAgentTurn` that never touches the model's actually-remembered
    `agentToolsSupported` capability, so a suppressed turn can't
    accidentally make the app forget the model supports tools.
  - `on`: always tries tools even with an image attached (accepts the
    vision-breaking tradeoff). `off`: never sends tools this chat.
- **Fixed a silent-failure bug in `ollamaChatStream`**: if Ollama's response
  stream got cut off mid-generation (backend crash, e.g. the `0xC0000409`
  stack-overrun crash seen with some models) without ever sending its final
  `done:true` chunk, the old code treated that as a normal, successful,
  *empty* reply — no error, nothing shown, total silence. Now a stream that
  ends without `done:true` surfaces a real error instead.
- **Model warm-up status** in the chat banner: `○ waiting for model to
  finish loading...` / `● model loaded and ready` / a red error. First
  implementation fired its own throwaway generate request on chat open to
  force a readiness check — that raced with a real message sent while the
  model was still cold-loading/being swapped in and corrupted that turn
  (reproduced: a working vision model started failing immediately after
  this was added). Replaced with a passive `ollama ps` poll that only ever
  reads state, plus marking the status "ready" the instant any real token,
  tool round, or completed turn arrives — so a stale/never-matching poll
  can't leave the banner stuck on "loading" while answers are visibly
  streaming in.
- **Chat text now reads prefix-colored, body-grey**: `you>` / `modelName>`
  keep their bright colors so it's still obvious who's talking, but the
  actual message text (typed input included, live while composing) renders
  in plain grey instead of competing for attention.
- **"list models" was leaving a dead gap above the footer**: its visible-row
  count used a hardcoded `overhead = 12` guess; recomputed precisely from
  the actual fixed lines (header/footer/box-padding/title/search/blank/
  column-header = 8) so the table now uses all the space it actually has.
- **First-run disclaimer**: the attention-grabbing lead-in of each paragraph
  (`llama-shell is an independent, unofficial tool.`, `License.`,
  `No warranty.`, `Use at your own risk`) and the "disclaimer" title itself
  render in red; `[a] I agree, continue` is green, `[q] quit` is red — same
  treatment on the `Ctrl+H` → disclaimer screen.
- **Offers to install Ollama** right after agreeing to the disclaimer, if
  it's not found: Linux runs the official one-line installer
  (`curl -fsSL https://ollama.com/install.sh | sh`) unattended; macOS/Windows
  open the correct download page instead, since those installs are signed
  GUI installers, not scriptable. First run only.
- **`ollama: not installed`** in the footer now blinks (and is bold) so it's
  harder to miss; the header/footer bar itself was darkened
  (`#5F5FAF` → `#3A3A66`) for more contrast against that red text.

## 13 new tools, clipboard-paste fix, Alt-key rebind — fourth session (2026-08-23)

- **Fixed clipboard image paste for real**: `System.Windows.Forms.Clipboard`
  requires PowerShell to run in STA mode; the paste script was launched
  without `-sta` and silently failed every time. Added it.
- **Paste now gives visible feedback**: a "pasting from clipboard..." status
  while running, then "pasted image: \<name\>" / "pasted clipboard text (N
  chars)" shown above the input line until the next message is sent.
- **All `Ctrl+<key>` chat/list/show-info shortcuts rebound to `Alt+<key>`**
  (`Alt+H`/`Alt+T`/`Alt+V`/`Alt+R`) — browsers reserve `Ctrl+T`/`H`/`R`/`V`
  for tab/history/reload/paste and never forward them to a web-based
  terminal (e.g. viewing an LXC console through a browser). `Ctrl+C` stays,
  since terminals reliably forward SIGINT even through a browser.
- **CAPABILITIES help section reworked as an actual table** (`CODE | WORD |
  MEANING`, one row per capability) instead of a run-on paragraph; the
  inline mentions on "list models"/"show model info" now point at it
  instead of duplicating a shorter, driftable copy.
- **13 new agent tools** (55 total): `take_screenshot`/`view_image` (attach
  image bytes directly to a tool result so a vision-capable model can see
  them — required a new `executeAgentToolWithImages` wrapper since plain
  tool results were text-only), `read_pdf` (via poppler's `pdftotext`),
  `http_post`, `download_file`, `send_notification`, `read_registry`,
  `run_sql` (via the `sqlite3` CLI), `read_csv`, `read_json`, `git_commit`,
  `git_branch`, `pull_ollama_model`.

## Agentic chat polish, vision support, help overhaul — third session (2026-08-23)

- **Capabilities column**: `list models` / `show model info` now show each
  Ollama-reported capability truncated to its first 3 letters
  (`com,too,vis,...`), with a green-check/red-X legend documented in the
  `Ctrl+H` help screen.
- **`Ctrl+H` everywhere**: works from `list models` and `show model info`
  directly (previously only inside agentic chat), with a footer hint on both
  screens.
- **Help screens now scroll**: `Up`/`Down`/`PgUp`/`PgDn`/`Home`/`End` on the
  help/disclaimer/activity-log screens — the reference text had grown past
  one terminal page and the old fixed clamp silently cut content off the
  top.
- **Key-reference styling**: hotkey names render green, descriptions grey,
  on both the main help screen and the agentic-chat help dialog.
- **Broader activity logging**: menu navigation, rescans, downloads/cancels,
  run/remove/stop actions, benchmark start/cancel, and chat messages sent
  (truncated preview) are now logged, not just async completions.
- **Agentic chat layout fix**: the input row and bottom status block used a
  fixed line-count reserve that left a dead gap of blank rows above `you>`
  when idle. The viewport now sizes itself every frame from the bottom
  block's actual size, so there's no reserved gap — closes right up.
- **Vision support in agentic chat**:
  - Typing/pasting a path to an existing image file
    (`.png/.jpg/.jpeg/.gif/.bmp/.webp`) in a chat message now reads and
    base64-attaches it via Ollama's `images` field, so vision-capable models
    actually receive the pixels instead of an unopenable path string.
  - `Ctrl+V` pastes directly from the Windows clipboard — an image (e.g. a
    screenshot) is saved to a temp PNG and attached the same way; falls back
    to clipboard text otherwise.
  - A missing/unreadable image path now surfaces a visible warning instead
    of silently sending as plain text (drag-and-drop managers like Ditto can
    show a path that isn't a real file).
- **Tool-calling capability detection**: the chat banner now shows a
  green/red badge row for every capability (`completion`, `tools`,
  `vision`, `insert`, `embedding`, `thinking`, `audio`) as soon as a chat
  starts. If the cached capability info already says a model doesn't
  support `tools`, the tool list is skipped on the first request instead of
  sending a request Ollama is guaranteed to reject; if that information was
  stale, a runtime "does not support tools" error auto-downgrades to plain
  chat (no file/tool access) instead of hard-failing the turn.

## Agentic chat (`a` from Show model info) — new feature, second session (2026-08-22)

Added a built-in chat + tool-calling agent, talking to Ollama's `/api/chat`
directly — no external agent harness (Aider/OpenCode/Claude Code) needed.
Motivated by earlier local-model testing (see the parent `gm-vscode` repo's
session log) showing small/local models hallucinate fake tool calls through
those harnesses' OpenAI-style function-calling layer; wiring against Ollama's
native API directly, with a from-scratch tool loop, gave more control over
that failure mode.

- **Action menu**: added `[a]` "run own agentic chat" to the show-model-info
  per-model action menu, alongside existing run/remove/kill — moved to first
  position (was last) per feedback that it's the primary reason to open the
  menu.
- **Streaming**: switched from a single blocking `/api/chat` call
  (`stream:false`) to Ollama's real streaming protocol — newline-delimited
  JSON, one partial `message.content` chunk per line, ending in a `done:true`
  line — parsed with `bufio.Scanner`. Replies now type out token-by-token.
  Runs in a goroutine reporting back over a channel (`agentStreamDeltaMsg` /
  `agentStepMsg` / `agentTurnDoneMsg`), same pattern as the existing
  download-progress and benchmark plumbing.
- **Tool-call loop**: up to 8 rounds per turn — model replies, if it returns
  `tool_calls` each one is executed locally and the result fed back as a
  `tool`-role message, repeat until a plain-content reply or the round cap.
- **Tools — grew in three passes to 42 total**:
  1. First pass (7): `read_file`, `write_file`, `append_file`, `list_dir`,
     `make_dir`, `delete_file`, `search_files`.
  2. Second pass (+15): `copy_file`, `move_file`, `run_command` (shell exec
     via `cmd.exe`), `open_url`, `open_path`, `list_processes`,
     `kill_process`, `ssh_run` (system `ssh` client, existing key/agent auth
     only — no password support), `http_get`, `ping_host`, `system_info`,
     `get_env`, `get_clipboard`, `set_clipboard`, `get_datetime`.
  3. Third pass (+20): `web_search` (DuckDuckGo HTML-results scrape — no API
     key, unlike DuckDuckGo's own Instant-Answer-only API), `read_webpage`
     (HTML-tag-stripped page text), `get_public_ip`, `list_env_vars`,
     `list_network_interfaces`, `disk_usage`, `list_installed_programs`
     (Windows uninstall registry), `compress_zip`/`extract_zip`
     (PowerShell `Compress-Archive`/`Expand-Archive`), `git_status`/
     `git_diff`/`git_log`, `run_python`, `run_powershell`,
     `list_window_titles`, `count_lines`, `file_hash` (SHA-256),
     `file_info`, `list_ollama_models`/`list_running_ollama_models` (this
     app's own domain — `ollama list`/`ollama ps`).
  - `run_command`, `run_powershell`, `run_python`, and `ssh_run` give the
    model real, unconfirmed shell/remote execution — no approval gate.
    Deliberately did *not* add shutdown/reboot/format-type tools without
    explicit sign-off, given how unreliable small local models had already
    proven to be earlier in the session.
- **Tool categories screen** (`Ctrl+T` from chat): the chat header's
  4-row tool grid got hard to scan once the count passed 30, so it now just
  shows a one-line count and the full list moved to its own screen, grouped
  into Files & Archives / Shell & Processes / Networking & Web / System &
  Environment / Git & Ollama / Open & Launch.
- **Help dialog** (`Ctrl+H` from chat): the footer's keybind hint
  (`Enter: send  Ctrl+T: tools  Esc: back...`) wrapped to two lines on
  narrower terminals, breaking the footer's fixed-height assumption. Moved
  the full keybind list to a `Ctrl+H` dialog; footer hint shortened to just
  `Ctrl+H: help`. Both `Ctrl+T` and `Ctrl+H` (not plain `t`/`h`) specifically
  to avoid swallowing those letters while typing a chat message.
- **System prompt tuning** — two rounds of failure, both from over-steering:
  1. Model initially refused everything file-related ("I'm just an AI, I
     don't have file access") despite `ollama show` confirming the model
     declares the `tools` capability — a confidence/training issue, not a
     missing capability. Fixed by explicitly telling it it has REAL working
     access, is not sandboxed, and must call a tool instead of describing
     what the user should do manually.
  2. That fix overcorrected: the model then refused *plain* requests ("tell
     me about X in bullets") by claiming its only job was file/tool
     operations. Fixed by adding an explicit clause that tools are additive
     to normal conversation, not a replacement for it.
- **Layout — the long fight over `you>`'s position**, in the order it
  actually happened:
  1. First version: manual line-slicing/windowing math for scrollback,
     with a fixed guessed constant for "chrome" (header/footer/padding/input
     line budget). Wrong on the first two guesses (double-counted, then
     under-counted, lipgloss box padding).
  2. A genuinely new failure mode: a huge streamed reply (model wrote a full
     HTML document) had no cap on the live "thinking" preview, so one frame
     grew taller than the terminal. The terminal scrolled to fit it, which
     permanently desynced bubbletea's internal cursor-position tracking from
     the terminal's real scroll offset — the header (printed first) scrolled
     out of view and **never came back, even after later frames shrank back
     down, until the app was restarted**. Root-caused via the box-model math,
     not guessed.
  3. Two fixes, one local and one global: capped the live streaming preview
     to its last N lines (local), and added a `clampToLastLines` hard
     safety net in `View()` that trims the WHOLE rendered body to the actual
     available rows before handing it to lipgloss, regardless of which
     view's own height math might be wrong (global, defense-in-depth).
  4. Replaced the hand-rolled scrollback math entirely with
     `github.com/charmbracelet/bubbles/viewport` (bumped `go.mod` to
     Go 1.24.2 and bubbletea/lipgloss to match) — added Up/Down/PgUp/PgDn/
     Home/End scrolling for free, gated behind an exact-key-name switch
     before ever calling `viewport.Update()` so its own vim-style letter
     keybinds (j/k/etc.) can never swallow a character being typed into the
     chat input.
  5. Discovered `viewport.View()` doesn't pad short content up to its own
     declared `Height` — it only returns however many lines are actually
     visible. Without manually padding that shortfall back in, lipgloss's
     outer `.Height()` call pads it onto the very bottom of the *whole*
     frame instead, after the input line — meaning `you>`'s row kept
     floating depending on conversation length. Fixed by manually padding
     `viewport.View()`'s output to its declared height before use, and
     reserving one always-printed row (blank when unused) for the
     scroll-position notice so that toggling it doesn't shift anything by 1.
  6. Position briefly (and incorrectly) flipped to "3rd row from the *top*"
     per a garbled request, then reverted back to the original, correct
     spec: footer (row 1 from bottom), one blank line (row 2), `you>`
     (row 3) — always. Root cause of the final miscount: the blank-line
     separator was being written *before* `bottom.String()` (between the
     transcript and the input line) instead of *after* it (between the
     input line and the footer) — total row count was already correct, just
     the gap was on the wrong side of `you>`.
  7. Mid-investigation, discovered **two stale `llama-shell.exe` processes
     running simultaneously** from prior test sessions — likely the reason
     several "fixed" builds still appeared broken (whichever stale window
     had focus wasn't necessarily running the latest binary). Killed both;
     rebuilding always requires fully closing every running instance first
     since the linker can't overwrite a locked `.exe`.
- **Footer**: unified background color with the header's purple
  (`#5F5FAF`) instead of a separate dark gray — required adding an explicit
  `Background()` to every individually-styled text fragment (status, hint,
  GitHub link), not just the outer wrapping style, since each fragment's own
  ANSI reset otherwise kills the outer background for the plain-space gaps
  between fragments. Also added a clickable GitHub repo link
  (`https://github.com/affigabmag/llama-shell`) via a raw OSC-8 terminal
  hyperlink escape — deliberately not wrapped in `.Width()` since lipgloss
  doesn't understand OSC-8 and could truncate mid-escape-sequence, corrupting
  the terminal's hyperlink state; the line is instead sized to the terminal
  width by manual gap math up front.
- **Model capabilities columns**: added a `CAPABILITIES` column (e.g.
  `completion,vision,audio,tools,thinking`) to both List models and Show
  model info, parsed from `ollama show`'s "Capabilities" section — lets you
  tell chat-only vs. multimodal vs. tool-calling-capable models apart at a
  glance. Only populated for locally installed models (an extra `ollama show`
  call per model during the List models scan); library/Hugging Face catalog
  entries show `-` since there's nothing installed yet to introspect.
- Removed the redundant `ollama-agent` sibling project (a separate
  LangChain/DeepAgents-based CLI agent explored earlier in the parent
  `gm-vscode` workspace) — superseded by this built-in agent; confirmed
  nothing from it was `pip install`ed globally before deleting the directory.

## README screenshots — attempted, blocked, abandoned in favor of manual capture
- Asked for screenshots of the main menu and show-model-info screens for the
  README. Since this is a TUI with no headless render path, that means an
  actual screenshot of a running terminal window, not a mock-up.
- Tried automating it end-to-end from the agent side:
  1. Launched `llama-shell.exe` directly via `Start-Process`, found its
     console window by enumerating top-level windows (`EnumWindows` +
     `GetWindowText`, since `Process.MainWindowHandle` came back `0` for a
     console app hosted in a `conhost`/`OpenConsole` child window).
  2. Moved/resized that window, called `SetForegroundWindow`, and captured
     the region with `Graphics.CopyFromScreen`.
  3. Result was a grayscale image — no banner colors, no yellow highlight.
     Diagnosed by sampling a pixel grid and checking R/G/B spread (a real
     Windows Terminal capture should show plenty of non-gray pixels; this
     had ~0%).
  4. Suspected legacy `conhost` lacking full truecolor VT rendering, so
     relaunched through `wt.exe` (Windows Terminal) instead, matching what
     the user's own screenshots throughout this session actually showed.
     Same grayscale result.
  5. Root-caused it by calling `GetForegroundWindow()` immediately before
     the capture: it returned the VS Code window, not the terminal, even
     after `SetForegroundWindow` / `BringWindowToTop` / `ShowWindow` /
     `Microsoft.VisualBasic.Interaction.AppActivate` all reported success.
     Windows deliberately blocks background/automated processes from
     stealing foreground focus (a documented OS security restriction) — so
     every capture had actually been screenshotting whatever real window
     already had focus (VS Code), not the app, regardless of which API said
     it had switched focus.
- Conclusion: this can't be fixed with more automation from an agent process
  — it's an OS-level restriction, not a bug in the capture script. Manual
  screenshots (user-initiated, e.g. Win+Shift+S) aren't subject to the same
  restriction, which is why every screenshot pasted directly by the user
  during this session rendered fine. Bad captures were deleted rather than
  committed; README screenshots are pending a manually-provided image.

## Help / Disclaimer / Log (`h`) — new menu item
- Added a submenu with three options:
  - `[h]` read help — keybindings and screen-by-screen usage notes.
  - `[d]` disclaimer — not affiliated with Ollama Inc. or Hugging Face, no
    warranty, and a note on the license's no-modification restriction.
  - `[g]` view log — tails the app's own activity log (last 200 lines).
- Added real activity logging (`%LocalAppData%\llama-shell\activity.log`):
  downloads (success/fail/abort), removes, stops (from both the running-models
  and show-model-info screens), interactive runs, and every benchmark
  start/per-model result/cancel/complete.
- Added a first-run gate: on first launch (checked via a marker file), the
  full disclaimer is shown with a single way forward — `[a] I agree` to
  continue, anything else (`q`/`Esc`/`ctrl+c`) quits without entering the app.
  Accepted once, never shown again on that machine.

## Licensing
- Switched from MIT to a custom source-available, view-only license: reading
  the code and running it is permitted, modifying it or redistributing a
  modified version is not. No warranty, all rights otherwise reserved. Not an
  OSI-approved open-source license by design — noted explicitly in the
  LICENSE file and in the in-app disclaimer.

## Repo
- Initialized git, pushed to a new public GitHub repo
  (`github.com/affigabmag/llama-shell`), first release cut as `v0.1.0`.

## Scaffolding
- Initial project: Go + Bubble Tea + Lipgloss chosen over Rust/Python/Node for
  fast iteration, mature TUI ecosystem, single static cross-platform binary.
- Fixed header/footer layout; footer shows ollama installed/not-installed status.
- Animated intro banner ("llama-shell" ASCII art), later reworked (see below).
- Main menu: `l` list models, `p` running models, `s` show model info, `q` quit.
- Wired `l`/`p`/`s` to real `ollama list` / `ollama ps` / `ollama show` calls.

## Navigation
- All menus made selectable via Up/Down + Enter, in addition to letter shortcuts.

## List models (`l`)
- Originally just `ollama list` output; expanded into a multi-source catalog:
  - Added Hugging Face (top GGUF repos by downloads) as a second source, with a
    `SOURCE` column.
  - Added a `LOCAL` column (installed y/n).
  - Discovered `ollama list` only shows *installed* models, not the full ollama
    catalog — added a third source scraping `ollama.com/library` (~235 model
    families).
  - Removed caching for this view (the `LOCAL` flag was going stale) — always
    fetches fresh with a progress bar across all three sources now.
  - Added a live-filtering search box (type to narrow the list).
  - Made the list scrollable/selectable; Enter on a row asks to confirm
    download (`[y]/[n]`), then runs `ollama pull` (Hugging Face repos go
    through ollama's `hf.co/` passthrough).
  - `ollama-library` entries with multiple parameter sizes (e.g. `qwen2` ->
    `0.5b,1.5b,7b,72b`) now open a size-picker sub-menu before the download
    confirm.
  - Download progress was originally a blocking spinner; replaced with a real
    streaming progress bar parsed from `ollama pull`'s own output, plus a
    `[c]` cancel key that kills the in-flight process.
  - Fixed a rendering-corruption bug: `ollama pull`'s own ANSI escape codes
    and block-bar characters were leaking into the captured text and being
    printed as part of the TUI frame, corrupting the terminal. Fixed by
    stripping ANSI/cursor-movement codes and rebuilding the status line from
    only whitelisted fields (label, percent, size, speed, eta) rather than
    trying to detect and cut out the bar characters directly.
  - Bumped the Hugging Face slice from top-20 to top-50 by downloads (still
    not exhaustive — HF hosts 100k+ GGUF repos).

## Running models (`p`)
- Made scrollable/selectable; Enter opens a confirm dialog to stop
  (`ollama stop`) the selected model. `r` to refresh.

## Show model info (`s`)
- Originally a single `ollama show <model>` lookup; changed to scan *all*
  installed models (progress bar) and display a table (name, params, quant,
  context, arch, size — `MODIFIED` later dropped as redundant).
- Results cached to `%LocalAppData%\llama-shell\models_cache.json`; loads
  instantly from cache on repeat visits, `r` forces a rescan.
- Made scrollable/selectable; Enter opens an action menu: `[x]` run
  interactively (hands the terminal to `ollama run` via `tea.ExecProcess`),
  `[r]` remove (`ollama rm`), `[k]` kill/stop (`ollama stop`). Menu order was
  later changed to run-first per feedback.
- The cache is now automatically invalidated the visit after a successful
  download from the List models screen, so newly pulled models show up
  immediately instead of behind a stale cache.
- Added `CPU/GPU` and `MATCH%` columns, populated by the Device Info
  benchmark (see below) via the same cache file.

## Device info (`d`) — new menu item
- Added: OS/arch, CPU model, logical core count, RAM total, per-drive
  used/total, GPU(s) — via WMI/CIM on Windows, `/proc` + `sysctl` fallbacks
  for Linux/macOS (untested on those platforms in this session).
- Cached to `device_cache.json`; `r` forces a rescan and refreshes the cache.
- Added `b`: benchmark every installed model's CPU/GPU split.
  - Confirm dialog rendered in red, warning it can take a long time.
  - Models benchmarked smallest-first (by parsed size) so a mid-run cancel
    still yields the most data points.
  - Per model: `ollama run <model> hi` (timed), read the live split from
    `ollama ps`, `ollama stop` it, advance.
  - Live-growing results table shown during the run and on the done/cancel
    screen (not just a one-line summary).
  - `[c]` cancels mid-model (kills the process); partial results are saved
    to cache either way.
  - Match score (0-100%) = `0.7 × GPU-share + 0.3 × load-speed score`.
  - CPU/GPU display format simplified: since the column header already says
    "CPU/GPU", `100% GPU` -> `100g`, `100% CPU` -> `100c`, a genuine split
    `44%/56% CPU/GPU` -> `44/56` (no repeated unit labels).

## Banner
- Iterated several times:
  1. Scrolling single-color ASCII art banner shown for ~1.6s at startup, then
     replaced by the menu.
  2. Changed to display constantly in the main menu, colored per-character
     from a rotating palette — caused mixed colors within single letters.
  3. Rebuilt entirely as a custom 5-row block-letter font ("LLAMA-SHELL"),
     each letter rendered as a single styled multi-line block so its color
     can never mix row-to-row.
  4. Color-per-letter was animating/rotating over time; changed to a static
     pinned mapping by letter position (1st=blue, 2nd=white, 3rd=red,
     4th=green, 5th=cyan, ... repeating) per explicit request — no more
     animation, removed the now-unused tick machinery entirely.

## Footer / versioning
- Originally a hardcoded `const version = "0.1.0"` that never changed.
- Replaced with a build-timestamp `var buildTime`, injected via
  `-ldflags "-X main.buildTime=<timestamp>"` at build time (no git repo
  available to use a commit hash instead).

## Environment notes (this session)
- Go wasn't installed; MSI installer attempts failed repeatedly (corrupted
  by a race between two concurrent downloads writing the same file, plus a
  locked-file issue from a stuck installer service). Resolved by switching
  to the portable zip distribution instead of the MSI installer.
