# Changelog

All entries are from the initial build-out session (2026-08-22). No semantic
versioning — the app footer shows a build timestamp instead (see README).

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
