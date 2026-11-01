# Auto-update

How llama-shell's self-update feature works, why it's built the way it is,
and the full record of how it was implemented, tested, and released
(2026-08-23).

## Deployed instances

- **CT 102 (`homeguard`)**: `/root/llama-shell`, updated to `v0.3.0` on
  2026-08-23. SSH access, host details, and the deployment record for
  this instance live in the `gm-datacenter` repo — see
  `doc/credentials.md` ("llama-shell — CT 102") and `doc/log.md` (row
  188). Not duplicated here since that repo is the source of truth for
  infrastructure/access, same as for every other app on that host.

## What it does

- On every startup, the app checks
  `api.github.com/repos/affigabmag/llama-shell/releases/latest` (one
  unauthenticated GET) and compares the release's tag against the running
  binary's baked-in version.
- If a newer release exists for the current OS/arch, the footer shows a
  blinking yellow `update` flag (left of the ollama status), and a new
  `[u] update` item appears in the help menu (`h` then `u`).
- The update screen shows current vs. latest version and offers `[u]`
  download-and-install or `[r]` re-check.
- Installing downloads the matching release zip, extracts the binary, and
  swaps it in for the file on disk. **A restart is required afterward** —
  the swap replaces the file, it doesn't reload the already-running
  process.

It is pull-based only: an already-running copy won't notice a new release
until it's relaunched, or `[r]` is pressed on the update screen.

## Design decisions

- **Version baked in at build time.** A new `appVersion` build var
  (parallel to the existing `buildTime` one) is set via
  `-X main.appVersion=vX.Y.Z`, tied to the git tag a release is cut from.
  Left at its default `"dev"` for a plain `go build`, in which case the
  checker never reports an update — there's no tagged baseline to compare
  against.
- **GitHub Releases API, not a custom update server.** No auth needed for
  public repos; `/releases/latest` conveniently ignores drafts and
  prereleases, so those can be used for staging without confusing real
  installs (see Testing below).
- **Per-platform zip release assets**, matching the naming convention
  already in use on the real repo (discovered by inspecting it, not
  assumed): `llama-shell-<goos>-<goarch>.zip`, containing `llama-shell.exe`
  (Windows) or `llama-shell` (macOS/Linux). `updateAssetName()` /
  `updateBinaryNameInZip()` encode this; `applyUpdateAt()` downloads to a
  temp `.zip`, opens it with `archive/zip`, and extracts by base file name
  so it doesn't care if the archive nests the binary in a subfolder.
- **Self-swap mechanism differs by OS** — confirmed empirically, not
  assumed (see Testing):
  - **Linux/macOS**: one atomic `os.Rename(new, exePath)`. Renaming over a
    running executable's file is allowed; the kernel keeps the old inode
    open (and running) until the process actually exits.
  - **Windows**: a running exe is locked against being renamed *onto*, but
    renaming the running exe file *itself* is allowed. So the swap is two
    steps: rename the running exe to `<exe>.old`, then rename the
    newly-extracted binary into the original path. The leftover `.old` is
    removed on next launch (`cleanupOldExe()`, called from `main()`), and
    pre-emptively before each swap in case a prior cleanup never ran.
  - `applyUpdate()` is a thin `os.Executable()` wrapper around
    `applyUpdateAt(exePath, assetURL)`, split out specifically so the real
    download/extract/rename logic could be exercised directly in a test
    against a throwaway binary, instead of only trusting it by inspection.
- **Real semantic version comparison**, not a string diff. See the bug
  below — the first version just checked whether the strings differed,
  which is wrong in either direction.
- **Blink via existing ANSI SGR trick**: the footer's `update` flag reuses
  the same `lipgloss.NewStyle().Blink(true)` approach already used for
  "ollama: not installed", instead of a manual `tea.Tick`-driven blink
  loop — the terminal blinks it natively.

## Files changed

- `main.go`:
  - `appVersion` var, `updateRepoAPI` const, `updateAssetName()`,
    `updateBinaryNameInZip()`, `githubRelease` struct, `checkForUpdate()`,
    `parseVersionParts()`, `isNewerVersion()`, `updateApplyResultMsg`,
    `applyUpdate()` / `applyUpdateAt()`, `cleanupOldExe()`.
  - New `viewUpdateText` view; `[u]` entry added to `helpMenuItems`; main
    menu `h` label changed to `help / disclaimer / log / update`.
  - Model fields: `updateChecked`, `updateAvailable`, `updateLatest`,
    `updateAssetURL`, `updateCheckErr`, `updateDownloading`,
    `updateResult`, `updateResultErr`.
  - `Init()` now kicks off `checkForUpdate()`; `main()` calls
    `cleanupOldExe()` before starting the program.
  - Key handling for `viewUpdateText` (`u`/`enter` to apply, `r` to
    retry, `esc` back); footer render updated to show the blinking
    `update` flag.
- `README.md`: build command now shows the `appVersion` ldflag, main menu
  table and footer/license mentions updated, new "Update" subsection.
- `CHANGELOG.md`: new dated entry for the feature, plus a follow-up entry
  for the version-comparison bug found during testing.

## Bug found and fixed during testing

The first `isNewerVersion()` only checked whether the current and latest
version strings were **different**:

```go
return norm(current) != norm(latest)
```

That misfires in either direction — a locally built `v0.2.1-test` compared
against the real published `v0.2.0` was flagged as "update available"
even though the installed copy was actually the newer one. Caught by
running the real check against the live repo mid-test (see below), not by
inspection. Fixed with `parseVersionParts()` (parses the leading
dotted-numeric run, ignoring any `-suffix`) plus a real numeric
major/minor/patch comparison in `isNewerVersion()`.

## Testing performed

All against the real `github.com/affigabmag/llama-shell` repo — no mocks.

1. **Unit-level proof of the swap mechanics**, before wiring anything into
   the app: copied the compiled test binary to a scratch dir, launched it
   as a real running process, and used `Rename-Item` to rename it aside
   and rename a replacement into place while it was still running.
   Confirmed Windows allows this (`RENAME-AWAY: OK`, `RENAME-IN: OK`).
2. **First real end-to-end test**, via a throwaway `_test.go` (deleted
   after use, not kept as a permanent test): called the actual
   `checkForUpdate()` and `applyUpdateAt()` production functions against
   the real `v0.2.0` release, downloading the genuine
   `llama-shell-windows-amd64.zip`, extracting `llama-shell.exe`, and
   swapping it onto a real running fake exe — which then launched
   successfully afterward.
3. **Manual staged release test** to exercise the full user-facing flow:
   - Tagged and published a temporary `v0.2.1-test` release as a
     **prerelease** (installed baseline — doesn't need to be "latest").
     "Installed" it by copying the binary to `C:\temp\llama-test\`.
   - Confirmed the running install showed no update flag and the update
     screen said "up to date" — this run exposed the version-comparison
     bug above (`v0.2.1-test` vs. real latest `v0.2.0` was wrongly
     flagged), which was fixed before continuing.
   - Re-verified the fix with a small throwaway test table covering the
     exact failing case plus reversed/equal/dev-baseline cases, then
     deleted the test file.
   - Tagged and published `v0.2.2-test` as a **real (non-prerelease)**
     release — required, since GitHub's `/releases/latest` endpoint
     ignores prereleases and drafts, so a prerelease would never be
     detected as an update.
   - Relaunched the installed copy: footer now showed the blinking
     `update` flag, help screen showed `v0.2.2-test available`.
   - Triggered `[u]` download-and-install from the running app; it
     reported success. Relaunching the same exe path showed the new
     build timestamp baked into the binary, confirming the swap had
     actually replaced the file (screenshots taken by the user during
     this step).
4. **Cleanup**: both test releases and both test tags (`v0.2.1-test`,
   `v0.2.2-test`) were deleted from the repo afterward — they were purely
   for exercising the flow, not real versions.

## Release process (going forward)

1. Bump the version and tag: `git tag vX.Y.Z && git push origin vX.Y.Z`.
2. Cross-compile for all platforms (no `cgo` involved, so this works
   from any OS):
   ```
   $env:CGO_ENABLED = "0"
   foreach ($t in @(
     @{goos="windows"; arch="amd64"; ext=".exe"},
     @{goos="linux";   arch="amd64"; ext=""},
     @{goos="linux";   arch="arm64"; ext=""},
     @{goos="darwin";  arch="amd64"; ext=""},
     @{goos="darwin";  arch="arm64"; ext=""}
   )) {
     $env:GOOS = $t.goos; $env:GOARCH = $t.arch
     $bin = "llama-shell$($t.ext)"
     go build -ldflags "-X main.appVersion=vX.Y.Z -X main.buildTime=<timestamp>" -o "release-build\$bin" .
     Compress-Archive -Path "release-build\$bin" -DestinationPath "release-build\llama-shell-$($t.goos)-$($t.arch).zip" -Force
     Remove-Item "release-build\$bin"
   }
   ```
3. Publish: `gh release create vX.Y.Z release-build\*.zip --title vX.Y.Z --notes "..."`.
   **Do not** pass `--prerelease` for a real release — `/releases/latest`
   (what the app queries) ignores prereleases and drafts entirely.
4. Every already-installed copy built with a matching `-X main.appVersion`
   ldflag will show the update flag next time it's launched (or on `[r]`
   retry) and can self-update from the help menu.

## Known limitations

- Pull-based, checked once at startup — no background polling or push
  notification while the app is already running.
- No checksum/signature verification of the downloaded asset beyond
  HTTPS — relies on GitHub's release integrity. Worth adding a
  `checksums.txt` asset + verification if this needs hardening later.
- If no release asset matches the current OS/arch naming convention, the
  update screen reports that clearly rather than silently doing nothing,
  but there's no fallback (e.g. building from source).
