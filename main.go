package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	mrand "math/rand"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// buildTime is overridden at build time via:
//
//	go build -ldflags "-X main.buildTime=<value>"
//
// Falls back to "dev" for a plain `go build` with no ldflags.
var buildTime = "dev"

// appVersion is overridden at build time via:
//
//	go build -ldflags "-X main.appVersion=v0.1.0"
//
// tied to the git tag a release is cut from. Falls back to "dev" for a
// plain `go build`, in which case the update checker never reports an
// update — there is no baseline version to compare against.
var appVersion = "dev"

const updateRepoAPI = "https://api.github.com/repos/affigabmag/llama-shell/releases/latest"

// updateAssetName is the release-asset naming convention actually used by
// github.com/affigabmag/llama-shell releases: a per-platform zip (e.g.
// "llama-shell-windows-amd64.zip") containing the binary — "llama-shell.exe"
// on Windows, "llama-shell" on macOS/Linux.
func updateAssetName(goos, goarch string) string {
	return fmt.Sprintf("llama-shell-%s-%s.zip", goos, goarch)
}

// updateBinaryNameInZip is the file name of the binary inside that zip.
func updateBinaryNameInZip(goos string) string {
	if goos == "windows" {
		return "llama-shell.exe"
	}
	return "llama-shell"
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

type updateCheckResultMsg struct {
	latestVersion string
	assetURL      string
	err           string
}

// checkForUpdate hits the GitHub releases API once and reports whether a
// newer version than the running binary is available for this OS/arch.
// Silently reports nothing actionable when appVersion is "dev" (no
// ldflag-injected baseline to compare against).
func checkForUpdate() tea.Cmd {
	return func() tea.Msg {
		latest, assetURL, err := checkForUpdateSync()
		if err != nil {
			return updateCheckResultMsg{err: err.Error()}
		}
		return updateCheckResultMsg{latestVersion: latest, assetURL: assetURL}
	}
}

// checkForUpdateSync is the plain (non-tea.Cmd) version of the GitHub
// releases check, reused by both the manual "check for update" UI action
// and the background daily auto-update timer, which has no tea.Program
// message loop to post a result back into.
func checkForUpdateSync() (latestVersion, assetURL string, err error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(updateRepoAPI)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("status %d", resp.StatusCode)
	}
	var rel githubRelease
	if err := json.Unmarshal(data, &rel); err != nil {
		return "", "", err
	}
	want := updateAssetName(runtime.GOOS, runtime.GOARCH)
	for _, a := range rel.Assets {
		if a.Name == want {
			assetURL = a.BrowserDownloadURL
			break
		}
	}
	return rel.TagName, assetURL, nil
}

// parseVersionParts pulls the leading dotted numeric run out of a version
// string, e.g. "v0.2.1-test" -> [0, 2, 1]. A trailing "-test"/"-beta" etc.
// suffix is ignored; anything non-numeric truncates parsing at that point.
func parseVersionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexByte(v, '-'); i != -1 {
		v = v[:i]
	}
	var parts []int
	for _, s := range strings.Split(v, ".") {
		n, err := strconv.Atoi(s)
		if err != nil {
			break
		}
		parts = append(parts, n)
	}
	return parts
}

// isNewerVersion does a real semantic comparison (not a string diff, which
// would misfire on any mismatch regardless of direction — e.g. a locally
// built "v0.2.1-test" vs. published "v0.2.0" is NOT an available update).
func isNewerVersion(current, latest string) bool {
	if current == "" || current == "dev" || latest == "" {
		return false
	}
	cur := parseVersionParts(current)
	lat := parseVersionParts(latest)
	for i := 0; i < len(cur) || i < len(lat); i++ {
		var c, l int
		if i < len(cur) {
			c = cur[i]
		}
		if i < len(lat) {
			l = lat[i]
		}
		if l != c {
			return l > c
		}
	}
	return false
}

type updateApplyResultMsg struct {
	err     string
	message string
}

// applyUpdate downloads the matching release asset and swaps it in for the
// running executable. Linux/macOS allow an atomic rename over a running
// binary's file (the kernel keeps the old inode open until the process
// exits), so one os.Rename does it. Windows locks the running exe against
// being overwritten or renamed onto, so it takes two steps: rename the
// running exe out of the way first, then rename the new one into place;
// the leftover ".old" file is cleaned up on next startup.
func applyUpdate(assetURL string) tea.Cmd {
	return func() tea.Msg {
		exePath, err := os.Executable()
		if err != nil {
			return updateApplyResultMsg{err: err.Error()}
		}
		exePath, err = filepath.EvalSymlinks(exePath)
		if err != nil {
			return updateApplyResultMsg{err: err.Error()}
		}
		return applyUpdateAt(exePath, assetURL)
	}
}

// applyUpdateAt does the actual download/extract/swap for exePath, split
// out from applyUpdate so the swap mechanics can be exercised directly
// against a throwaway binary in tests instead of the real running exe.
func applyUpdateAt(exePath, assetURL string) updateApplyResultMsg {
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(assetURL)
	if err != nil {
		return updateApplyResultMsg{err: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return updateApplyResultMsg{err: fmt.Sprintf("download failed: status %d", resp.StatusCode)}
	}

	// Release assets are zips (see updateAssetName) containing the
	// binary alongside e.g. README/LICENSE — download to a temp zip
	// file first since zip.OpenReader needs a seekable file, not a
	// streaming response body.
	zipPath := exePath + ".update.zip"
	zipOut, err := os.Create(zipPath)
	if err != nil {
		return updateApplyResultMsg{err: err.Error()}
	}
	if _, err := io.Copy(zipOut, resp.Body); err != nil {
		zipOut.Close()
		os.Remove(zipPath)
		return updateApplyResultMsg{err: err.Error()}
	}
	zipOut.Close()
	defer os.Remove(zipPath)

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return updateApplyResultMsg{err: "couldn't open downloaded zip: " + err.Error()}
	}
	defer zr.Close()

	wantName := updateBinaryNameInZip(runtime.GOOS)
	var binFile *zip.File
	for _, f := range zr.File {
		if filepath.Base(f.Name) == wantName {
			binFile = f
			break
		}
	}
	if binFile == nil {
		return updateApplyResultMsg{err: fmt.Sprintf("no %q found inside the downloaded zip", wantName)}
	}

	rc, err := binFile.Open()
	if err != nil {
		return updateApplyResultMsg{err: err.Error()}
	}
	defer rc.Close()

	newPath := exePath + ".new"
	out, err := os.Create(newPath)
	if err != nil {
		return updateApplyResultMsg{err: err.Error()}
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		os.Remove(newPath)
		return updateApplyResultMsg{err: err.Error()}
	}
	out.Close()
	if runtime.GOOS != "windows" {
		_ = os.Chmod(newPath, 0o755)
	}

	if runtime.GOOS == "windows" {
		oldPath := exePath + ".old"
		os.Remove(oldPath) // leftover from a previous update, if any
		if err := os.Rename(exePath, oldPath); err != nil {
			os.Remove(newPath)
			return updateApplyResultMsg{err: "couldn't move running exe aside: " + err.Error()}
		}
		if err := os.Rename(newPath, exePath); err != nil {
			return updateApplyResultMsg{err: "couldn't move new exe into place: " + err.Error()}
		}
	} else {
		if err := os.Rename(newPath, exePath); err != nil {
			os.Remove(newPath)
			return updateApplyResultMsg{err: err.Error()}
		}
	}

	return updateApplyResultMsg{message: "Updated. Restart llama-shell to use the new version."}
}

// cleanupOldExe removes a ".old" file left behind by a prior Windows
// update swap (see applyUpdate) — harmless no-op on other OSes or when
// there's nothing to clean up.
func cleanupOldExe() {
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	_ = os.Remove(exePath + ".old")
}

// boxVertices are the 4 corners of a FLAT rectangle (zero depth, all
// z=0), rotating in 3D — it'll periodically look edge-on/thin as it
// spins past 90°, which is correct for a genuinely flat shape (like a
// card flipping), not a rendering bug. boxEdges connects them as a
// simple quad outline. triangleVertices/Edges are a flat triangle the
// same way. The main-menu banner renders both side by side, each
// spinning independently, instead of static block-letter text.
var boxVertices = [4][3]float64{
	{-1.6, -0.8, 0}, {1.6, -0.8, 0}, {1.6, 0.8, 0}, {-1.6, 0.8, 0},
}

var boxEdges = [4][2]int{
	{0, 1}, {1, 2}, {2, 3}, {3, 0},
}

var triangleVertices = [3][3]float64{
	{0, -1, 0}, {0.87, 0.6, 0}, {-0.87, 0.6, 0},
}

var triangleEdges = [3][2]int{
	{0, 1}, {1, 2}, {2, 0},
}

// circleVertices approximates a flat circle (zero depth, all z=0) as a
// 16-point polygon; circleEdges connects them consecutively around the
// loop. Same "flat shape spinning in 3D" treatment as the rectangle and
// triangle.
var circleVertices = func() [16][3]float64 {
	var v [16][3]float64
	for i := range v {
		a := float64(i) / 16 * 2 * math.Pi
		v[i] = [3]float64{math.Cos(a), math.Sin(a), 0}
	}
	return v
}()

var circleEdges = func() [16][2]int {
	var e [16][2]int
	for i := range e {
		e[i] = [2]int{i, (i + 1) % 16}
	}
	return e
}()

// cubeColorStops is the banner's color loop, evenly spaced around the
// cycle: yellow -> orange -> red -> green -> blue -> white -> (back to
// yellow).
var cubeColorStops = [6][3]int{
	{255, 215, 0},
	{255, 140, 0},
	{255, 60, 60},
	{80, 220, 100},
	{80, 140, 255},
	{255, 255, 255},
}

// cubeColorAt interpolates cubeColorStops at a phase in [0,1) (wrapping),
// so the color blends smoothly through the loop rather than jumping
// between flat colors.
func cubeColorAt(phase float64) string {
	n := len(cubeColorStops)
	phase = math.Mod(phase, 1.0)
	if phase < 0 {
		phase += 1.0
	}
	seg := phase * float64(n)
	idx := int(seg) % n
	next := (idx + 1) % n
	frac := seg - math.Floor(seg)
	c0, c1 := cubeColorStops[idx], cubeColorStops[next]
	lerp := func(a, b int) int { return int(float64(a) + (float64(b)-float64(a))*frac) }
	return fmt.Sprintf("#%02X%02X%02X", lerp(c0[0], c1[0]), lerp(c0[1], c1[1]), lerp(c0[2], c1[2]))
}

// depthRamp is a density ramp from farthest to nearest — the wireframe
// renders each pixel with a character picked from this by its depth
// (rotated z), so the shape reads with real pseudo-3D shading instead of
// a single flat character for every line.
var depthRamp = []byte{'.', '*', '%', '#', '@'}

func depthChar(z float64) byte {
	const zMin, zMax = -1.8, 1.8 // typical range after rotating these shapes
	t := (z - zMin) / (zMax - zMin)
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return depthRamp[int(t*float64(len(depthRamp)-1))]
}

// renderWireframe projects verts (rotated by angle around all three
// axes — X, Y, and Z — each at a different rate for a genuine tumble
// rather than a flat single-axis spin) onto a w*h character grid via
// simple perspective, then rasterizes each edge onto it with a basic
// parametric line walk, shading each pixel by depth via depthChar. The
// horizontal axis is stretched (2.2x) to compensate for terminal
// characters being taller than they are wide — without it any shape
// would render squashed. Shared by every shape so they all spin with
// visually consistent depth/perspective.
func renderWireframe(verts [][3]float64, edges [][2]int, w, h int, angle float64) string {
	grid := make([][]byte, h)
	for i := range grid {
		grid[i] = make([]byte, w)
		for j := range grid[i] {
			grid[i][j] = ' '
		}
	}

	ax, ay, az := angle*0.6, angle, angle*0.9
	cosX, sinX := math.Cos(ax), math.Sin(ax)
	cosY, sinY := math.Cos(ay), math.Sin(ay)
	cosZ, sinZ := math.Cos(az), math.Sin(az)

	// [3]float64{screenX, screenY, depthZ} — depth carried alongside the
	// 2D projection so drawLine can interpolate it too, not just position.
	proj := make([][3]float64, len(verts))
	for i, v := range verts {
		x, y, z := v[0], v[1], v[2]
		x1 := x*cosY + z*sinY
		z1 := -x*sinY + z*cosY
		y2 := y*cosX - z1*sinX
		z2 := y*sinX + z1*cosX
		x2 := x1*cosZ - y2*sinZ
		y3 := x1*sinZ + y2*cosZ
		scale := 6.0 / (4.0 - z2)
		proj[i] = [3]float64{x2 * scale * 2.2, y3 * scale, z2}
	}

	cx, cy := w/2, h/2
	setPixel := func(px, py, z float64) {
		gx, gy := cx+int(math.Round(px)), cy+int(math.Round(py))
		if gx >= 0 && gx < w && gy >= 0 && gy < h {
			grid[gy][gx] = depthChar(z)
		}
	}
	drawLine := func(p0, p1 [3]float64) {
		steps := int(math.Max(math.Abs(p1[0]-p0[0]), math.Abs(p1[1]-p0[1]))*2) + 1
		for s := 0; s <= steps; s++ {
			t := float64(s) / float64(steps)
			setPixel(p0[0]+(p1[0]-p0[0])*t, p0[1]+(p1[1]-p0[1])*t, p0[2]+(p1[2]-p0[2])*t)
		}
	}
	for _, e := range edges {
		drawLine(proj[e[0]], proj[e[1]])
	}

	lines := make([]string, h)
	for i, row := range grid {
		lines[i] = string(row)
	}
	return strings.Join(lines, "\n")
}

type view int

const (
	viewMenu view = iota
	viewListScan
	viewListTable
	viewPs
	viewShowScan
	viewShowTable
	viewDeviceInfo
	viewHelpMenu
	viewHelpText
	viewDisclaimerText
	viewLogText
	viewUpdateText
	viewWizard
	viewFirstRunDisclaimer
	viewAgentChat
	viewToolCategories
	viewAgentHelp
	viewOllamaInstallPrompt
	viewTavilySettings
	viewWebServerSettings
	viewWebServerModelSelect
	viewTelegramSettings
	viewAutopilot
	viewEmailSettings
	viewBackupSettings
	viewBackupBrowser
)

type cmdResultMsg struct {
	output string
}

func runOllama(args ...string) tea.Cmd {
	return func() tea.Msg {
		out, err := exec.Command("ollama", args...).CombinedOutput()
		text := string(out)
		if err != nil && text == "" {
			text = err.Error()
		}
		return cmdResultMsg{output: text}
	}
}

// --- Agentic chat: minimal Ollama /api/chat client with file tools ---

type ollamaToolCallFn struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

type ollamaToolCall struct {
	Function ollamaToolCallFn `json:"function"`
}

type ollamaChatMsg struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Images    []string         `json:"images,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

// imageExtensions are the file types Ollama's vision models accept via a
// message's "images" field (base64-encoded, not passed as tool results).
var imageExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".bmp": true, ".webp": true,
}

// maxImageBytes caps a single attached image's file size before base64
// encoding, so a mistakenly-referenced huge file doesn't blow up the
// request body or the terminal-visible chat history.
const maxImageBytes = 20 * 1024 * 1024

// extractImagePaths scans a chat message for whitespace-separated tokens
// that look like a path to an existing image file, so pasting or typing a
// path (e.g. "C:\...\screenshot.png what do you see") attaches it as image
// data the model can actually see, instead of just a text string it can't
// open itself. missing carries back any token that has an image extension
// but doesn't resolve to a real file (e.g. a drag-and-drop manager's
// ephemeral stub path) — a silent no-op there previously left the user with
// no clue why "vision" wasn't working.
func extractImagePaths(text string) (found []string, missing []string) {
	seen := map[string]bool{}
	for _, tok := range strings.Fields(text) {
		tok = strings.Trim(tok, `"'`)
		ext := strings.ToLower(filepath.Ext(tok))
		if !imageExtensions[ext] || seen[tok] {
			continue
		}
		seen[tok] = true
		info, err := os.Stat(tok)
		if err != nil || info.IsDir() || info.Size() > maxImageBytes {
			missing = append(missing, tok)
			continue
		}
		found = append(found, tok)
	}
	return found, missing
}

type clipboardPasteMsg struct {
	text      string
	imagePath string
	err       string
}

// pasteFromClipboard grabs whatever is on the Windows clipboard — an image
// (screenshot, copied picture) takes priority and gets saved to a temp PNG,
// otherwise falls back to plain text — so Alt+V in agentic chat works for
// both without the user needing to know which one is on the clipboard.
func pasteFromClipboard() tea.Cmd {
	return func() tea.Msg {
		const script = `
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$img = [System.Windows.Forms.Clipboard]::GetImage()
if ($img) {
  $path = [System.IO.Path]::Combine([System.IO.Path]::GetTempPath(), 'llama-shell-clip-' + [guid]::NewGuid().ToString('N') + '.png')
  $img.Save($path, [System.Drawing.Imaging.ImageFormat]::Png)
  Write-Output ('IMG:' + $path)
} else {
  Write-Output ([System.Windows.Forms.Clipboard]::GetText())
}`
		// -sta is required: System.Windows.Forms.Clipboard throws
		// "current thread must be set to single thread apartment" without
		// it, since PowerShell's default -Command execution is MTA.
		out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-sta", "-Command", script).CombinedOutput()
		if err != nil {
			return clipboardPasteMsg{err: strings.TrimSpace(string(out))}
		}
		result := strings.TrimSpace(string(out))
		if path, ok := strings.CutPrefix(result, "IMG:"); ok {
			return clipboardPasteMsg{imagePath: path}
		}
		return clipboardPasteMsg{text: result}
	}
}

// loadImagesBase64 reads and base64-encodes each path for the Ollama chat
// API's "images" field. Paths that fail to read are silently skipped —
// extractImagePaths already confirmed they exist, but a name collision or a
// removal between stat and read shouldn't abort sending the message.
func loadImagesBase64(paths []string) []string {
	var out []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		out = append(out, base64.StdEncoding.EncodeToString(data))
	}
	return out
}

type ollamaToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type ollamaTool struct {
	Type     string             `json:"type"`
	Function ollamaToolFunction `json:"function"`
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaChatMsg `json:"messages"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
}

type ollamaChatResponse struct {
	Message ollamaChatMsg `json:"message"`
	Done    bool          `json:"done"`
}

// agentTools describes the file-system tools the agent may call. Kept
// intentionally small (read/write/list) to match what the user asked for —
// reading and creating files — without adding shell-exec risk.
func agentTools() []ollamaTool {
	strProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	return []ollamaTool{
		{Type: "function", Function: ollamaToolFunction{
			Name:        "read_file",
			Description: "Read the full text contents of a file.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": strProp("File path, relative to the working directory or absolute.")},
				"required":   []string{"path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "write_file",
			Description: "Create or overwrite a file with the given text content. Creates parent directories as needed.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":    strProp("File path, relative to the working directory or absolute."),
					"content": strProp("Full text content to write to the file."),
				},
				"required": []string{"path", "content"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "list_dir",
			Description: "List files and subdirectories in a directory.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": strProp("Directory path, relative to the working directory or absolute. Use \".\" for the working directory.")},
				"required":   []string{"path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "append_file",
			Description: "Append text content to the end of a file. Creates the file if it doesn't exist.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":    strProp("File path, relative to the working directory or absolute."),
					"content": strProp("Text to append to the file."),
				},
				"required": []string{"path", "content"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "make_dir",
			Description: "Create a directory, including any missing parent directories.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": strProp("Directory path, relative to the working directory or absolute.")},
				"required":   []string{"path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "delete_file",
			Description: "Delete a single file. Refuses to delete directories.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": strProp("File path, relative to the working directory or absolute.")},
				"required":   []string{"path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "search_files",
			Description: "Recursively search under a directory for files whose name contains the given substring (case-insensitive).",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":  strProp("Directory to search under, relative to the working directory or absolute. Use \".\" for the working directory."),
					"query": strProp("Substring to match against file names, case-insensitive."),
				},
				"required": []string{"path", "query"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "copy_file",
			Description: "Copy a file from one path to another.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"src": strProp("Source file path."),
					"dst": strProp("Destination file path."),
				},
				"required": []string{"src", "dst"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "move_file",
			Description: "Move or rename a file.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"src": strProp("Source file path."),
					"dst": strProp("Destination file path."),
				},
				"required": []string{"src", "dst"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "run_command",
			Description: "Run a shell command on this machine (via cmd.exe) and return its combined stdout/stderr output. Use this for Ollama model lifecycle actions there's no dedicated tool for — e.g. `ollama stop <model>` to unload/restart a running model (it reloads automatically the next time it's used), `ollama ps` to see what's running.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"command": strProp("The full command line to run, e.g. \"dir\" or \"tasklist\".")},
				"required":   []string{"command"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "open_url",
			Description: "Open a URL in the default web browser.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"url": strProp("The URL to open, e.g. https://example.com")},
				"required":   []string{"url"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "open_path",
			Description: "Open a local file or folder with its default associated application.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": strProp("File or folder path to open.")},
				"required":   []string{"path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "list_processes",
			Description: "List currently running processes on this machine.",
			Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "kill_process",
			Description: "Forcibly terminate a running process by name or PID. Name can be given with or without \".exe\" — it's added automatically if missing.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"target": strProp("Process name (e.g. \"notepad.exe\") or numeric PID.")},
				"required":   []string{"target"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "ssh_run",
			Description: "Run a command on a remote machine over SSH, using the system ssh client and whatever key-based/agent authentication is already configured locally (e.g. in ~/.ssh/config). Cannot supply a password.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"host":    strProp("Target as user@host or a Host alias from ~/.ssh/config."),
					"command": strProp("Command to run on the remote host."),
				},
				"required": []string{"host", "command"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "http_get",
			Description: "Fetch a URL over HTTP(S) and return its response body as text (truncated if large).",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"url": strProp("URL to fetch.")},
				"required":   []string{"url"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "ping_host",
			Description: "Check network reachability of a host by pinging it a few times.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"host": strProp("Hostname or IP address to ping.")},
				"required":   []string{"host"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "get_datetime",
			Description: "Get the current local date and time on this machine.",
			Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "system_info",
			Description: "Get basic system info: OS, architecture, CPU count, hostname.",
			Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "get_env",
			Description: "Read the value of an environment variable on this machine.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"name": strProp("Environment variable name, e.g. \"PATH\".")},
				"required":   []string{"name"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "get_clipboard",
			Description: "Read the current text contents of the system clipboard.",
			Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "set_clipboard",
			Description: "Set the system clipboard to the given text.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"text": strProp("Text to place on the clipboard.")},
				"required":   []string{"text"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "web_search",
			Description: "Search the public internet (via DuckDuckGo) and return a list of result titles, URLs, and snippets.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"query": strProp("Search query.")},
				"required":   []string{"query"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "read_webpage",
			Description: "Fetch a web page and return its visible text content with HTML tags stripped, for reading articles or docs.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"url": strProp("URL of the page to read.")},
				"required":   []string{"url"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "rss_feed",
			Description: "Fetch an RSS/Atom feed URL and return its items as a clean list of title / link / published date / summary, parsed from the XML. Use this instead of read_webpage for feed URLs (e.g. site.com/rss, feeds.*, /rssindex) — many news sites gate their HTML pages behind a cookie-consent wall but serve their RSS feed unrestricted.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"url": strProp("URL of the RSS or Atom feed.")},
				"required":   []string{"url"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "find_rss_feed",
			Description: "Discover a site's real RSS/Atom feed URL(s) by fetching a page and reading its <link rel=\"alternate\" type=\"application/rss+xml\"> / atom+xml tags — the standard way sites advertise their feed. Call this on a site's homepage or section page BEFORE guessing an RSS URL for rss_feed: a guessed path (e.g. /rss, /feed) is often wrong and just wastes a round trip, since not every site follows that convention.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"url": strProp("URL of a page on the site to scan for a feed link (e.g. the homepage).")},
				"required":   []string{"url"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "tavily_search",
			Description: "Search the web via the Tavily API (requires the TAVILY_API_KEY environment variable to be set) and return ranked results with title, URL, and a real content snippet — a paid, higher-quality alternative to web_search.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query":       strProp("Search query."),
					"max_results": strProp("Max results to return (default 5)."),
				},
				"required": []string{"query"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "tavily_extract",
			Description: "Scrape a URL via the Tavily API (requires the TAVILY_API_KEY environment variable to be set) and return its clean extracted article text — no HTML tags, nav/ads clutter, or cookie-consent walls. Prefer this over read_webpage for a page that read_webpage returned as a consent wall, a JS-only shell, or an unusable jumble, IF a Tavily key is configured.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"url": strProp("URL of the page to scrape.")},
				"required":   []string{"url"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "get_web_ui_url",
			Description: "Get the URL to open this same agentic chat in a web browser (llama-shell's own web UI), including its required access token. Call this whenever the user asks for the web UI's URL/link/address — never guess or say there isn't one without checking first.",
			Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "get_stock_quote",
			Description: "Get the actual current/live price for a stock ticker or ANY country's market index, straight from a JSON API — no scraping, no cookie-consent walls, always has the real number. Use this INSTEAD OF web_search/read_webpage for any stock price or index-value question, worldwide, not just well-known indices. Symbols: NASDAQ-100 -> ^NDX, NASDAQ Composite -> ^IXIC, S&P 500 -> ^GSPC, Dow Jones -> ^DJI, Russell 2000 -> ^RUT, VIX -> ^VIX, TA-35 -> TA35.TA, TA-125 -> TA125.TA, FTSE 100 -> ^FTSE, DAX -> ^GDAXI, CAC 40 -> ^FCHI, IBEX 35 -> ^IBEX, FTSE MIB -> FTSEMIB.MI, Euro Stoxx 50 -> ^STOXX50E, AEX -> ^AEX, SMI -> ^SSMI, OMX Stockholm -> ^OMX, Nikkei 225 -> ^N225, Hang Seng -> ^HSI, Shanghai Composite -> 000001.SS, Shenzhen Component -> 399001.SZ, KOSPI -> ^KS11, KOSDAQ -> ^KQ11, TAIEX -> ^TWII, Nifty 50 -> ^NSEI, Sensex -> ^BSESN, Straits Times -> ^STI, ASX 200 -> ^AXJO, NZX 50 -> ^NZ50, TSX -> ^GSPTSE, Bovespa -> ^BVSP, IPC (Mexico) -> ^MXX, MERVAL -> ^MERV, MOEX -> IMOEX.ME, JSE Top 40 -> ^J200, EGX 30 -> ^CASE30, a company -> its ticker (e.g. AAPL, MSFT, TSLA). The user's wording may be garbled by autocorrect — resolve it to the real symbol yourself, never pass their literal typo through. If a country/index isn't listed here or this returns no data, fall back to web_search to find the right Yahoo Finance symbol.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"symbol": strProp("Ticker or index symbol, e.g. ^NDX, ^GSPC, ^DJI, AAPL, TSLA.")},
				"required":   []string{"symbol"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "send_email",
			Description: "Send an email through the Gmail account configured in llama-shell's help/settings ([e] email). Only 'to' and 'subject' are required — never ask the user for a body if they didn't give one, just send it without one (or write a short reasonable body yourself from context if that makes sense). Fails with a clear error if no account has been set up yet — tell the user to configure it there rather than trying anything else.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"to":      strProp("Recipient email address."),
					"subject": strProp("Email subject line."),
					"body":    strProp("Plain-text email body. Optional — omit or leave empty if the user didn't ask for specific content."),
				},
				"required": []string{"to", "subject"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "get_public_ip",
			Description: "Get this machine's public (internet-facing) IP address.",
			Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "list_env_vars",
			Description: "List the names of all environment variables set on this machine.",
			Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "list_network_interfaces",
			Description: "List network interfaces and their IP configuration.",
			Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "disk_usage",
			Description: "List local disk drives with their free and used space.",
			Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "list_installed_programs",
			Description: "List software installed on this machine (from the Windows uninstall registry).",
			Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "compress_zip",
			Description: "Compress a file or directory into a .zip archive.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":     strProp("File or directory to compress."),
					"zip_path": strProp("Destination .zip file path."),
				},
				"required": []string{"path", "zip_path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "extract_zip",
			Description: "Extract a .zip archive into a destination directory.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"zip_path": strProp("Source .zip file path."),
					"dest":     strProp("Destination directory to extract into."),
				},
				"required": []string{"zip_path", "dest"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "git_status",
			Description: "Run `git status` in a repository directory.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": strProp("Path to the git repository. Use \".\" for the working directory.")},
				"required":   []string{"path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "git_diff",
			Description: "Run `git diff` in a repository directory to show unstaged changes.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": strProp("Path to the git repository. Use \".\" for the working directory.")},
				"required":   []string{"path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "git_log",
			Description: "Show recent commit history (one line per commit) for a git repository.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":  strProp("Path to the git repository. Use \".\" for the working directory."),
					"count": strProp("Number of commits to show, as a string, e.g. \"10\"."),
				},
				"required": []string{"path", "count"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "run_python",
			Description: "Run a short Python snippet (via `python -c`) and return its output.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"code": strProp("Python source code to execute.")},
				"required":   []string{"code"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "run_powershell",
			Description: "Run a PowerShell script/command and return its output.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"script": strProp("PowerShell script or command to run.")},
				"required":   []string{"script"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "list_window_titles",
			Description: "List the titles of currently open application windows.",
			Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "count_lines",
			Description: "Count the number of lines in a text file.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": strProp("File path, relative to the working directory or absolute.")},
				"required":   []string{"path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "file_hash",
			Description: "Compute the SHA-256 checksum of a file.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": strProp("File path, relative to the working directory or absolute.")},
				"required":   []string{"path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "file_info",
			Description: "Get metadata about a file or directory: size, last-modified time, and whether it's a directory.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": strProp("File or directory path, relative to the working directory or absolute.")},
				"required":   []string{"path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "list_ollama_models",
			Description: "List Ollama models installed on this machine (`ollama list`).",
			Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "list_running_ollama_models",
			Description: "List Ollama models currently loaded in memory (`ollama ps`).",
			Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "take_screenshot",
			Description: "Capture the current screen and attach it as an image for you to see directly (requires a vision-capable model).",
			Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "view_image",
			Description: "Read an existing local image file and attach it for you to see directly (requires a vision-capable model) — use this to look at a screenshot, photo, or diagram already on disk.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": strProp("Image file path, relative to the working directory or absolute.")},
				"required":   []string{"path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "read_pdf",
			Description: "Extract text from a PDF file. Requires poppler's `pdftotext` to be installed and on PATH.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": strProp("PDF file path, relative to the working directory or absolute.")},
				"required":   []string{"path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "read_document",
			Description: "Extract plain text from a document so you can read and answer questions about it — .pdf (requires poppler's `pdftotext` on PATH), .docx (Word, parsed directly, no external tool needed), or any plain-text file (.txt/.md/etc, read as-is). Picks the right extraction method from the file extension.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": strProp("Document file path (.pdf, .docx, or plain text), relative to the working directory or absolute.")},
				"required":   []string{"path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "http_post",
			Description: "POST a request body to a URL over HTTP(S) and return the response as text (truncated if large).",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"url":          strProp("URL to POST to."),
					"body":         strProp("Request body to send."),
					"content_type": strProp("Content-Type header, e.g. \"application/json\". Defaults to application/json if omitted."),
				},
				"required": []string{"url", "body"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "download_file",
			Description: "Download a URL's contents directly to a local file.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"url":  strProp("URL to download."),
					"path": strProp("Destination file path, relative to the working directory or absolute."),
				},
				"required": []string{"url", "path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "send_notification",
			Description: "Show a system notification/message dialog to the user on this machine.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"title":   strProp("Notification title."),
					"message": strProp("Notification body text."),
				},
				"required": []string{"title", "message"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "read_registry",
			Description: "Read a Windows registry key's values (read-only).",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"key": strProp("Registry key path, e.g. \"HKLM:\\Software\\Microsoft\\Windows NT\\CurrentVersion\".")},
				"required":   []string{"key"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "run_sql",
			Description: "Run a read/write SQL query against a local SQLite database file. Requires the `sqlite3` CLI to be installed and on PATH.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"db_path": strProp("SQLite database file path, relative to the working directory or absolute."),
					"query":   strProp("SQL query to run."),
				},
				"required": []string{"db_path", "query"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "read_csv",
			Description: "Read a CSV file and return it formatted as an aligned table.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": strProp("CSV file path, relative to the working directory or absolute.")},
				"required":   []string{"path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "read_json",
			Description: "Read a JSON file and return it pretty-printed and validated.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": strProp("JSON file path, relative to the working directory or absolute.")},
				"required":   []string{"path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "git_commit",
			Description: "Stage all changes and create a git commit with the given message.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":    strProp("Path to the git repository. Use \".\" for the working directory."),
					"message": strProp("Commit message."),
				},
				"required": []string{"path", "message"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "git_branch",
			Description: "List git branches, or create and switch to a new one if a name is given.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": strProp("Path to the git repository. Use \".\" for the working directory."),
					"name": strProp("Name of a new branch to create and switch to. Omit to just list existing branches."),
				},
				"required": []string{"path"},
			},
		}},
		{Type: "function", Function: ollamaToolFunction{
			Name:        "pull_ollama_model",
			Description: "Download an Ollama model (`ollama pull`).",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"name": strProp("Model name to pull, e.g. \"llama3.2:1b\".")},
				"required":   []string{"name"},
			},
		}},
	}
}

// truncateToolOutput caps a tool result's length so one runaway command
// (e.g. `tasklist`, `dir /s`) can't blow out the model's context.
func truncateToolOutput(s string) string {
	const maxLen = 8000
	if len(s) > maxLen {
		return s[:maxLen] + "\n...(truncated)"
	}
	return s
}

// readDocxText extracts plain text from a .docx file. Word's format is a
// zip archive of XML parts — the visible document text lives in
// word/document.xml as a sequence of <w:t> runs grouped into <w:p>
// paragraphs (table cells are just paragraphs nested one level deeper, so
// no special-casing is needed to pick up table text too).
func readDocxText(path string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer zr.Close()

	var docFile *zip.File
	for _, f := range zr.File {
		if strings.ReplaceAll(f.Name, "\\", "/") == "word/document.xml" {
			docFile = f
			break
		}
	}
	if docFile == nil {
		return "", fmt.Errorf("word/document.xml not found — not a valid .docx")
	}

	rc, err := docFile.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()

	var b strings.Builder
	dec := xml.NewDecoder(rc)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch el := tok.(type) {
		case xml.StartElement:
			if el.Name.Local == "t" {
				var text string
				if err := dec.DecodeElement(&text, &el); err != nil {
					return "", err
				}
				b.WriteString(text)
			} else if el.Name.Local == "tab" {
				b.WriteString("\t")
			} else if el.Name.Local == "br" || el.Name.Local == "cr" {
				b.WriteString("\n")
			}
		case xml.EndElement:
			if el.Name.Local == "p" {
				b.WriteString("\n")
			}
		}
	}
	return b.String(), nil
}

func resolveAgentPath(workDir, p string) string {
	if p == "" {
		p = "."
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(workDir, p)
}

func toolArgString(args map[string]interface{}, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// executeAgentTool runs one tool call locally and returns its text result,
// which is fed back to the model as a "tool" role message.
var (
	htmlWSRe = regexp.MustCompile(`\s+`)

	ddgResultLinkRe = regexp.MustCompile(`(?s)<a rel="nofollow" class="result__a" href="([^"]+)">(.*?)</a>`)
	ddgSnippetRe    = regexp.MustCompile(`(?s)<a class="result__snippet"[^>]*>(.*?)</a>`)
)

// stripHTMLTags turns raw HTML into plain readable text. A regex-based
// tag stripper breaks the moment an attribute value (e.g. a large inline
// style="...") contains a literal '>', so this scans char-by-char instead,
// tracking quote state inside tags and skipping script/style element
// bodies entirely by their closing tag name.
func stripHTMLTags(s string) string {
	var out strings.Builder
	lower := strings.ToLower(s)
	i := 0
	for i < len(s) {
		if s[i] != '<' {
			out.WriteByte(s[i])
			i++
			continue
		}

		// Comments (<!-- ... -->) can carry raw CSS/JS (fallback styles,
		// conditional-comment hacks). Drop them verbatim by literal
		// "-->" search — quote-tracking doesn't apply inside a comment.
		if strings.HasPrefix(s[i:], "<!--") {
			end := strings.Index(s[i+4:], "-->")
			if end < 0 {
				break // unterminated comment: drop the remainder of the document
			}
			i = i + 4 + end + 3
			continue
		}

		// Identify the tag name to see if it opens a script/style block.
		j := i + 1
		for j < len(s) && s[j] != '>' && s[j] != ' ' && s[j] != '\t' && s[j] != '\n' && s[j] != '/' {
			j++
		}
		tagName := lower[i+1 : j]

		if tagName == "script" || tagName == "style" {
			closer := "</" + tagName
			end := strings.Index(lower[j:], closer)
			if end < 0 {
				break // unterminated block: drop the remainder of the document
			}
			i = j + end
			// Skip past the closing tag itself (up to its '>').
			for i < len(s) && s[i] != '>' {
				i++
			}
			i++
			continue
		}

		// Ordinary tag: skip to its unquoted '>', respecting quoted
		// attribute values so an embedded '>' doesn't end the tag early.
		var quote byte
		for i < len(s) {
			c := s[i]
			if quote != 0 {
				if c == quote {
					quote = 0
				}
			} else if c == '"' || c == '\'' {
				quote = c
			} else if c == '>' {
				i++
				break
			}
			i++
		}
	}
	return strings.TrimSpace(htmlWSRe.ReplaceAllString(out.String(), " "))
}

var htmlLinkTagRe = regexp.MustCompile(`(?is)<link\s[^>]*>`)

type discoveredFeed struct {
	title string
	url   string
}

// htmlTagAttr pulls one attribute's value out of a raw <tag ...> string,
// accepting either single or double quotes.
func htmlTagAttr(tag, attr string) string {
	re := regexp.MustCompile(`(?i)` + attr + `\s*=\s*"([^"]*)"|` + attr + `\s*=\s*'([^']*)'`)
	m := re.FindStringSubmatch(tag)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

// findRSSFeedLinks scans raw HTML for <link rel="alternate" type=".../rss+xml
// or .../atom+xml" href="..."> tags — the standard mechanism a site uses to
// advertise its feed(s) to browsers/readers — and resolves each href
// against pageURL (feeds are commonly linked with a relative path).
func findRSSFeedLinks(html, pageURL string) []discoveredFeed {
	base, err := url.Parse(pageURL)
	var feeds []discoveredFeed
	for _, tag := range htmlLinkTagRe.FindAllString(html, -1) {
		rel := strings.ToLower(htmlTagAttr(tag, "rel"))
		typ := strings.ToLower(htmlTagAttr(tag, "type"))
		if !strings.Contains(rel, "alternate") {
			continue
		}
		if !strings.Contains(typ, "rss") && !strings.Contains(typ, "atom") && !strings.Contains(typ, "xml") {
			continue
		}
		href := htmlTagAttr(tag, "href")
		if href == "" {
			continue
		}
		resolved := href
		if err == nil {
			if u, uerr := url.Parse(href); uerr == nil {
				resolved = base.ResolveReference(u).String()
			}
		}
		title := htmlTagAttr(tag, "title")
		if title == "" {
			title = "feed"
		}
		feeds = append(feeds, discoveredFeed{title: title, url: resolved})
	}
	return feeds
}

// rssItem is one parsed feed entry, normalized across RSS 2.0's <item> and
// Atom's <entry> shapes.
type rssItem struct {
	Title     string
	Link      string
	Published string
	Summary   string
}

// parseRSSFeed parses either RSS 2.0 (<rss><channel><item>) or Atom
// (<feed><entry>) XML into a normalized item list.
func parseRSSFeed(data []byte) ([]rssItem, error) {
	var rss struct {
		Channel struct {
			Items []struct {
				Title       string `xml:"title"`
				Link        string `xml:"link"`
				PubDate     string `xml:"pubDate"`
				Description string `xml:"description"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(data, &rss); err == nil && len(rss.Channel.Items) > 0 {
		items := make([]rssItem, len(rss.Channel.Items))
		for i, it := range rss.Channel.Items {
			items[i] = rssItem{Title: it.Title, Link: it.Link, Published: it.PubDate, Summary: it.Description}
		}
		return items, nil
	}

	var atom struct {
		Entries []struct {
			Title string `xml:"title"`
			Link  struct {
				Href string `xml:"href,attr"`
			} `xml:"link"`
			Updated string `xml:"updated"`
			Summary string `xml:"summary"`
		} `xml:"entry"`
	}
	if err := xml.Unmarshal(data, &atom); err != nil {
		return nil, err
	}
	items := make([]rssItem, len(atom.Entries))
	for i, it := range atom.Entries {
		items[i] = rssItem{Title: it.Title, Link: it.Link.Href, Published: it.Updated, Summary: it.Summary}
	}
	return items, nil
}

// tavilyPost POSTs a JSON body to a Tavily API endpoint and returns the raw
// response body, erroring on a non-2xx status.
func tavilyPost(endpoint string, body []byte) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("POST", endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Tavily API returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// fetchURL issues a GET with browser-like headers and retries once after a
// short backoff on 429, since many sites (e.g. Yahoo Finance) block Go's
// default User-Agent outright.
func fetchURL(target string) (*http.Response, []byte, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("GET", target, nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		resp, err := client.Do(req)
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode == 429 && attempt == 0 {
			resp.Body.Close()
			time.Sleep(2 * time.Second)
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return resp, nil, err
		}
		return resp, data, nil
	}
}

// webSearch scrapes DuckDuckGo's no-JS HTML results page — no API key
// needed, unlike DuckDuckGo's official API (Instant Answer only, not real
// search) or any other search provider.
func webSearch(query string) (string, error) {
	endpoint := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; llama-shell/1.0)")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	page := string(body)

	links := ddgResultLinkRe.FindAllStringSubmatch(page, -1)
	snippets := ddgSnippetRe.FindAllStringSubmatch(page, -1)

	const maxResults = 15
	var b strings.Builder
	for i := 0; i < len(links) && i < maxResults; i++ {
		title := strings.TrimSpace(stripHTMLTags(links[i][2]))
		realURL := links[i][1]
		if u, err := url.Parse(realURL); err == nil {
			if uddg := u.Query().Get("uddg"); uddg != "" {
				if decoded, err := url.QueryUnescape(uddg); err == nil {
					realURL = decoded
				}
			}
		}
		snippet := ""
		if i < len(snippets) {
			snippet = strings.TrimSpace(stripHTMLTags(snippets[i][1]))
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n\n", i+1, title, realURL, snippet)
	}
	if b.Len() == 0 {
		return "(no results)", nil
	}
	return b.String(), nil
}

// executeAgentToolWithImages wraps executeAgentTool for the two tools that
// need to hand image bytes back to the model (take_screenshot, view_image),
// which executeAgentTool's plain string return can't carry. Everything else
// delegates straight through with no images.
// logAgentToolCall records every tool call the agent makes (name + args,
// then success/error + a truncated preview of the result) across all three
// surfaces (TUI, web UI, Telegram) that funnel through this one function —
// giving the log a real audit trail of what the model actually did, not
// just what the user/UI did.
func logAgentToolCall(name string, args map[string]interface{}, result string) {
	argsJSON, _ := json.Marshal(args)
	outcome := "ok"
	if strings.HasPrefix(strings.TrimSpace(result), "error") {
		outcome = "ERROR"
	}
	appendLog("tool call: %s(%s) -> %s: %s", name, truncateName(string(argsJSON), 200), outcome, truncateName(result, 200))
}

func executeAgentToolWithImages(workDir, name string, args map[string]interface{}) (string, []string) {
	result, images := executeAgentToolWithImagesInner(workDir, name, args)
	logAgentToolCall(name, args, result)
	return result, images
}

func executeAgentToolWithImagesInner(workDir, name string, args map[string]interface{}) (string, []string) {
	switch name {
	case "take_screenshot":
		const script = `
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$bounds = [System.Windows.Forms.SystemInformation]::VirtualScreen
$bmp = New-Object System.Drawing.Bitmap $bounds.Width, $bounds.Height
$g = [System.Drawing.Graphics]::FromImage($bmp)
$g.CopyFromScreen($bounds.Location, [System.Drawing.Point]::Empty, $bounds.Size)
$path = [System.IO.Path]::Combine([System.IO.Path]::GetTempPath(), 'llama-shell-shot-' + [guid]::NewGuid().ToString('N') + '.png')
$bmp.Save($path, [System.Drawing.Imaging.ImageFormat]::Png)
Write-Output $path`
		out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-sta", "-Command", script).CombinedOutput()
		if err != nil {
			return "error: " + err.Error() + "\n" + string(out), nil
		}
		path := strings.TrimSpace(string(out))
		data, err := os.ReadFile(path)
		if err != nil {
			return "error reading captured screenshot: " + err.Error(), nil
		}
		_ = os.Remove(path)
		return "captured screenshot (attached as an image)", []string{base64.StdEncoding.EncodeToString(data)}

	case "view_image":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		data, err := os.ReadFile(full)
		if err != nil {
			return "error: " + err.Error(), nil
		}
		return "attached " + full + " as an image", []string{base64.StdEncoding.EncodeToString(data)}
	}
	return executeAgentTool(workDir, name, args), nil
}

func executeAgentTool(workDir, name string, args map[string]interface{}) string {
	switch name {
	case "read_file":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		data, err := os.ReadFile(full)
		if err != nil {
			return "error: " + err.Error()
		}
		const maxLen = 20000
		if len(data) > maxLen {
			return string(data[:maxLen]) + "\n...(truncated)"
		}
		return string(data)

	case "write_file":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "error: " + err.Error()
		}
		if err := os.WriteFile(full, []byte(toolArgString(args, "content")), 0o644); err != nil {
			return "error: " + err.Error()
		}
		return "wrote file: " + full

	case "list_dir":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		entries, err := os.ReadDir(full)
		if err != nil {
			return "error: " + err.Error()
		}
		if len(entries) == 0 {
			return "(empty directory)"
		}
		var names []string
		for _, e := range entries {
			if e.IsDir() {
				names = append(names, e.Name()+"/")
			} else {
				names = append(names, e.Name())
			}
		}
		return strings.Join(names, "\n")

	case "append_file":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "error: " + err.Error()
		}
		f, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return "error: " + err.Error()
		}
		defer f.Close()
		if _, err := f.WriteString(toolArgString(args, "content")); err != nil {
			return "error: " + err.Error()
		}
		return "appended to file: " + full

	case "make_dir":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		if err := os.MkdirAll(full, 0o755); err != nil {
			return "error: " + err.Error()
		}
		return "created directory: " + full

	case "delete_file":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		info, err := os.Stat(full)
		if err != nil {
			return "error: " + err.Error()
		}
		if info.IsDir() {
			return "error: refusing to delete a directory: " + full
		}
		if err := os.Remove(full); err != nil {
			return "error: " + err.Error()
		}
		return "deleted file: " + full

	case "search_files":
		root := resolveAgentPath(workDir, toolArgString(args, "path"))
		query := strings.ToLower(toolArgString(args, "query"))
		var matches []string
		const maxMatches = 200
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !d.IsDir() && strings.Contains(strings.ToLower(d.Name()), query) {
				matches = append(matches, p)
				if len(matches) >= maxMatches {
					return filepath.SkipAll
				}
			}
			return nil
		})
		if err != nil {
			return "error: " + err.Error()
		}
		if len(matches) == 0 {
			return "(no matches)"
		}
		result := strings.Join(matches, "\n")
		if len(matches) >= maxMatches {
			result += "\n...(truncated at " + strconv.Itoa(maxMatches) + " matches)"
		}
		return result

	case "copy_file":
		src := resolveAgentPath(workDir, toolArgString(args, "src"))
		dst := resolveAgentPath(workDir, toolArgString(args, "dst"))
		data, err := os.ReadFile(src)
		if err != nil {
			return "error: " + err.Error()
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "error: " + err.Error()
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return "error: " + err.Error()
		}
		return "copied to: " + dst

	case "move_file":
		src := resolveAgentPath(workDir, toolArgString(args, "src"))
		dst := resolveAgentPath(workDir, toolArgString(args, "dst"))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "error: " + err.Error()
		}
		if err := os.Rename(src, dst); err != nil {
			return "error: " + err.Error()
		}
		return "moved to: " + dst

	case "run_command":
		out, err := exec.Command("cmd", "/c", toolArgString(args, "command")).CombinedOutput()
		result := string(out)
		if err != nil {
			result += "\n(exit error: " + err.Error() + ")"
		}
		return truncateToolOutput(result)

	case "open_url":
		url := toolArgString(args, "url")
		if !strings.Contains(url, "://") {
			url = "https://" + url
		}
		if err := exec.Command("cmd", "/c", "start", "", url).Start(); err != nil {
			return "error: " + err.Error()
		}
		return "opened URL: " + url

	case "open_path":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		if err := exec.Command("cmd", "/c", "start", "", full).Start(); err != nil {
			return "error: " + err.Error()
		}
		return "opened: " + full

	case "list_processes":
		out, err := exec.Command("tasklist").CombinedOutput()
		if err != nil {
			return "error: " + err.Error()
		}
		return truncateToolOutput(string(out))

	case "kill_process":
		target := toolArgString(args, "target")
		var out []byte
		var err error
		if _, convErr := strconv.Atoi(target); convErr == nil {
			out, err = exec.Command("taskkill", "/F", "/PID", target).CombinedOutput()
		} else {
			// Windows' taskkill /IM matches the image name exactly, which
			// always includes ".exe" — a model asked to "kill/restart X"
			// reliably passes the bare name (e.g. "ollama"), which then
			// fails as "process not found" even though the process is
			// running, since the real image name is "ollama.exe".
			if filepath.Ext(target) == "" {
				target += ".exe"
			}
			out, err = exec.Command("taskkill", "/F", "/IM", target).CombinedOutput()
		}
		result := string(out)
		if err != nil {
			result += "\n(exit error: " + err.Error() + ")"
		}
		return result

	case "ssh_run":
		out, err := exec.Command("ssh", toolArgString(args, "host"), toolArgString(args, "command")).CombinedOutput()
		result := string(out)
		if err != nil {
			result += "\n(exit error: " + err.Error() + ")"
		}
		return truncateToolOutput(result)

	case "http_get":
		resp, data, err := fetchURL(toolArgString(args, "url"))
		if err != nil {
			return "error: " + err.Error()
		}
		return truncateToolOutput(fmt.Sprintf("status %d\n%s", resp.StatusCode, string(data)))

	case "ping_host":
		out, err := exec.Command("ping", "-n", "3", toolArgString(args, "host")).CombinedOutput()
		result := string(out)
		if err != nil {
			result += "\n(exit error: " + err.Error() + ")"
		}
		return result

	case "get_datetime":
		return time.Now().Format("2006-01-02 15:04:05 (Monday) MST")

	case "system_info":
		hostname, _ := os.Hostname()
		return fmt.Sprintf("OS: %s\nArch: %s\nCPUs: %d\nHostname: %s", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), hostname)

	case "get_env":
		val, ok := os.LookupEnv(toolArgString(args, "name"))
		if !ok {
			return "(not set)"
		}
		return val

	case "get_clipboard":
		out, err := exec.Command("powershell", "-NoProfile", "-Command", "Get-Clipboard").CombinedOutput()
		if err != nil {
			return "error: " + err.Error()
		}
		return string(out)

	case "set_clipboard":
		cmd := exec.Command("powershell", "-NoProfile", "-Command", "Set-Clipboard -Value $input")
		cmd.Stdin = strings.NewReader(toolArgString(args, "text"))
		if out, err := cmd.CombinedOutput(); err != nil {
			return "error: " + err.Error() + "\n" + string(out)
		}
		return "clipboard set"

	case "web_search":
		result, err := webSearch(toolArgString(args, "query"))
		if err != nil {
			return "error: " + err.Error()
		}
		return result

	case "read_webpage":
		_, data, err := fetchURL(toolArgString(args, "url"))
		if err != nil {
			return "error: " + err.Error()
		}
		return truncateToolOutput(stripHTMLTags(string(data)))

	case "rss_feed":
		_, data, err := fetchURL(toolArgString(args, "url"))
		if err != nil {
			return "error: " + err.Error()
		}
		items, err := parseRSSFeed(data)
		if err != nil {
			return "error: " + err.Error()
		}
		if len(items) == 0 {
			return "feed fetched but no items found — it may not be a valid RSS/Atom feed"
		}
		var b strings.Builder
		for i, it := range items {
			fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, strings.TrimSpace(it.Title), strings.TrimSpace(it.Link))
			if pub := strings.TrimSpace(it.Published); pub != "" {
				fmt.Fprintf(&b, "   %s\n", pub)
			}
			if sum := strings.TrimSpace(stripHTMLTags(it.Summary)); sum != "" {
				fmt.Fprintf(&b, "   %s\n", sum)
			}
		}
		return truncateToolOutput(b.String())

	case "find_rss_feed":
		pageURL := toolArgString(args, "url")
		_, data, err := fetchURL(pageURL)
		if err != nil {
			return "error: " + err.Error()
		}
		feeds := findRSSFeedLinks(string(data), pageURL)
		if len(feeds) == 0 {
			return "no RSS/Atom <link> tag found on that page — this site may not expose a feed, or advertises it somewhere else (try the homepage if this was a section page, or fall back to web_search/tavily_extract/read_webpage)"
		}
		var b strings.Builder
		for _, f := range feeds {
			fmt.Fprintf(&b, "%s — %s\n", f.title, f.url)
		}
		return truncateToolOutput(b.String())

	case "tavily_search":
		apiKey := os.Getenv("TAVILY_API_KEY")
		if apiKey == "" {
			return "error: TAVILY_API_KEY environment variable is not set — get a key at https://www.tavily.com/ and set it to use this tool"
		}
		maxResults := toolArgString(args, "max_results")
		if maxResults == "" {
			maxResults = "5"
		}
		n, err := strconv.Atoi(maxResults)
		if err != nil || n <= 0 {
			n = 5
		}
		body, _ := json.Marshal(map[string]interface{}{
			"api_key":     apiKey,
			"query":       toolArgString(args, "query"),
			"max_results": n,
		})
		data, err := tavilyPost("https://api.tavily.com/search", body)
		if err != nil {
			return "error: " + err.Error()
		}
		var resp struct {
			Results []struct {
				Title   string `json:"title"`
				URL     string `json:"url"`
				Content string `json:"content"`
			} `json:"results"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return "error: couldn't parse Tavily response: " + err.Error()
		}
		if len(resp.Results) == 0 {
			return "no results"
		}
		var b strings.Builder
		for i, r := range resp.Results {
			fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1, strings.TrimSpace(r.Title), strings.TrimSpace(r.URL), strings.TrimSpace(r.Content))
		}
		return truncateToolOutput(b.String())

	case "tavily_extract":
		apiKey := os.Getenv("TAVILY_API_KEY")
		if apiKey == "" {
			return "error: TAVILY_API_KEY environment variable is not set — get a key at https://www.tavily.com/ and set it to use this tool"
		}
		body, _ := json.Marshal(map[string]interface{}{
			"api_key": apiKey,
			"urls":    []string{toolArgString(args, "url")},
		})
		data, err := tavilyPost("https://api.tavily.com/extract", body)
		if err != nil {
			return "error: " + err.Error()
		}
		var resp struct {
			Results []struct {
				URL        string `json:"url"`
				RawContent string `json:"raw_content"`
			} `json:"results"`
			FailedResults []struct {
				URL   string `json:"url"`
				Error string `json:"error"`
			} `json:"failed_results"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return "error: couldn't parse Tavily response: " + err.Error()
		}
		if len(resp.Results) == 0 {
			if len(resp.FailedResults) > 0 {
				return "error: Tavily couldn't extract that URL: " + resp.FailedResults[0].Error
			}
			return "error: Tavily returned no content for that URL"
		}
		return truncateToolOutput(strings.TrimSpace(resp.Results[0].RawContent))

	case "get_web_ui_url":
		if !isWebServerRunning() {
			return "The web UI is not currently running. Enable it in llama-shell via [h] help/settings -> [b] web server."
		}
		cfg := loadWebServerConfig()
		port := webServerEffectivePort(cfg)
		var b strings.Builder
		b.WriteString("Web UI links (each includes the required access token):\n\n")
		b.WriteString(webServerURLFor("127.0.0.1", cfg.Token, port) + " (this machine only)\n\n")
		for _, ip := range localLANIPv4s() {
			b.WriteString(webServerURLFor(ip, cfg.Token, port) + " (same WiFi/LAN)\n\n")
		}
		b.WriteString("No tunnel (e.g. Cloudflare Tunnel) is set up, so none of these work from outside this network.")
		return b.String()

	case "get_public_ip":
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get("https://api.ipify.org")
		if err != nil {
			return "error: " + err.Error()
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return "error: " + err.Error()
		}
		return strings.TrimSpace(string(data))

	case "get_stock_quote":
		symbol, _ := args["symbol"].(string)
		symbol = strings.TrimSpace(symbol)
		if symbol == "" {
			return "error: symbol is required"
		}
		client := &http.Client{Timeout: 10 * time.Second}
		req, _ := http.NewRequest("GET", "https://query1.finance.yahoo.com/v8/finance/chart/"+url.QueryEscape(symbol), nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
		resp, err := client.Do(req)
		if err != nil {
			return "error: " + err.Error()
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return "error: " + err.Error()
		}
		var parsed struct {
			Chart struct {
				Result []struct {
					Meta struct {
						Symbol             string  `json:"symbol"`
						RegularMarketPrice float64 `json:"regularMarketPrice"`
						PreviousClose      float64 `json:"previousClose"`
						Currency           string  `json:"currency"`
						RegularMarketTime  int64   `json:"regularMarketTime"`
						LongName           string  `json:"longName"`
						ShortName          string  `json:"shortName"`
						InstrumentType     string  `json:"instrumentType"`
					} `json:"meta"`
				} `json:"result"`
				Error json.RawMessage `json:"error"`
			} `json:"chart"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			return "error: could not parse quote response: " + err.Error()
		}
		if len(parsed.Chart.Result) == 0 {
			return fmt.Sprintf("error: no quote data found for symbol %q (raw response: %s)", symbol, truncateToolOutput(string(data)))
		}
		meta := parsed.Chart.Result[0].Meta
		name := meta.LongName
		if name == "" {
			name = meta.ShortName
		}
		change := meta.RegularMarketPrice - meta.PreviousClose
		// An index (^GSPC, ^NDX, ^FTSE, etc.) is a unitless point value, not
		// an amount of money — Yahoo's API still labels it with a currency
		// code (the currency the constituent stocks trade in), which reads
		// as wrong/confusing tacked onto an index level, so drop it there.
		isIndex := meta.InstrumentType == "INDEX" || strings.HasPrefix(meta.Symbol, "^")
		unit := " " + meta.Currency
		if isIndex {
			unit = " points"
		}
		return fmt.Sprintf("%s (%s): %.2f%s (previous close %.2f, change %+.2f), as of %s",
			name, meta.Symbol, meta.RegularMarketPrice, unit, meta.PreviousClose, change,
			time.Unix(meta.RegularMarketTime, 0).Format(time.RFC1123))

	case "send_email":
		to := toolArgString(args, "to")
		subject := toolArgString(args, "subject")
		body := toolArgString(args, "body") // optional — fine to send empty
		if to == "" || subject == "" {
			return "error: to and subject are required"
		}
		cfg := loadEmailConfig()
		if err := sendEmailViaGmail(cfg, to, subject, body); err != nil {
			return "error: " + err.Error()
		}
		return "email sent to " + to

	case "list_env_vars":
		vars := os.Environ()
		names := make([]string, len(vars))
		for i, v := range vars {
			if idx := strings.IndexByte(v, '='); idx >= 0 {
				names[i] = v[:idx]
			} else {
				names[i] = v
			}
		}
		sort.Strings(names)
		return strings.Join(names, "\n")

	case "list_network_interfaces":
		out, err := exec.Command("ipconfig", "/all").CombinedOutput()
		if err != nil {
			return "error: " + err.Error()
		}
		return truncateToolOutput(string(out))

	case "disk_usage":
		out, err := exec.Command("powershell", "-NoProfile", "-Command",
			"Get-PSDrive -PSProvider FileSystem | Format-Table -AutoSize | Out-String -Width 200").CombinedOutput()
		if err != nil {
			return "error: " + err.Error()
		}
		return string(out)

	case "list_installed_programs":
		out, err := exec.Command("powershell", "-NoProfile", "-Command",
			"Get-ItemProperty 'HKLM:\\Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\*' | "+
				"Where-Object DisplayName | Sort-Object DisplayName | Select-Object -ExpandProperty DisplayName").CombinedOutput()
		if err != nil {
			return "error: " + err.Error()
		}
		return truncateToolOutput(string(out))

	case "compress_zip":
		src := resolveAgentPath(workDir, toolArgString(args, "path"))
		dst := resolveAgentPath(workDir, toolArgString(args, "zip_path"))
		psCmd := fmt.Sprintf("Compress-Archive -Path '%s' -DestinationPath '%s' -Force", psQuote(src), psQuote(dst))
		if out, err := exec.Command("powershell", "-NoProfile", "-Command", psCmd).CombinedOutput(); err != nil {
			return "error: " + err.Error() + "\n" + string(out)
		}
		return "compressed to: " + dst

	case "extract_zip":
		src := resolveAgentPath(workDir, toolArgString(args, "zip_path"))
		dst := resolveAgentPath(workDir, toolArgString(args, "dest"))
		psCmd := fmt.Sprintf("Expand-Archive -Path '%s' -DestinationPath '%s' -Force", psQuote(src), psQuote(dst))
		if out, err := exec.Command("powershell", "-NoProfile", "-Command", psCmd).CombinedOutput(); err != nil {
			return "error: " + err.Error() + "\n" + string(out)
		}
		return "extracted to: " + dst

	case "git_status":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		out, err := exec.Command("git", "-C", full, "status").CombinedOutput()
		result := string(out)
		if err != nil {
			result += "\n(exit error: " + err.Error() + ")"
		}
		return result

	case "git_diff":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		out, err := exec.Command("git", "-C", full, "diff").CombinedOutput()
		result := string(out)
		if err != nil {
			result += "\n(exit error: " + err.Error() + ")"
		}
		return truncateToolOutput(result)

	case "git_log":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		count := toolArgString(args, "count")
		if count == "" {
			count = "10"
		}
		out, err := exec.Command("git", "-C", full, "log", "-n", count, "--oneline").CombinedOutput()
		result := string(out)
		if err != nil {
			result += "\n(exit error: " + err.Error() + ")"
		}
		return result

	case "run_python":
		out, err := exec.Command("python", "-c", toolArgString(args, "code")).CombinedOutput()
		result := string(out)
		if err != nil {
			result += "\n(exit error: " + err.Error() + ")"
		}
		return truncateToolOutput(result)

	case "run_powershell":
		out, err := exec.Command("powershell", "-NoProfile", "-Command", toolArgString(args, "script")).CombinedOutput()
		result := string(out)
		if err != nil {
			result += "\n(exit error: " + err.Error() + ")"
		}
		return truncateToolOutput(result)

	case "list_window_titles":
		out, err := exec.Command("powershell", "-NoProfile", "-Command",
			"Get-Process | Where-Object { $_.MainWindowTitle -ne '' } | Select-Object -ExpandProperty MainWindowTitle").CombinedOutput()
		if err != nil {
			return "error: " + err.Error()
		}
		return string(out)

	case "count_lines":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		data, err := os.ReadFile(full)
		if err != nil {
			return "error: " + err.Error()
		}
		return strconv.Itoa(strings.Count(string(data), "\n") + 1)

	case "file_hash":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		data, err := os.ReadFile(full)
		if err != nil {
			return "error: " + err.Error()
		}
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:])

	case "file_info":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		info, err := os.Stat(full)
		if err != nil {
			return "error: " + err.Error()
		}
		return fmt.Sprintf("path: %s\nsize: %d bytes\nmodified: %s\nis_dir: %v",
			full, info.Size(), info.ModTime().Format(time.RFC3339), info.IsDir())

	case "list_ollama_models":
		out, err := exec.Command("ollama", "list").CombinedOutput()
		if err != nil {
			return "error: " + err.Error()
		}
		return string(out)

	case "list_running_ollama_models":
		out, err := exec.Command("ollama", "ps").CombinedOutput()
		if err != nil {
			return "error: " + err.Error()
		}
		return string(out)

	case "read_pdf":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		out, err := exec.Command("pdftotext", full, "-").CombinedOutput()
		if err != nil {
			return "error: " + err.Error() + " (requires poppler's pdftotext on PATH)\n" + string(out)
		}
		return truncateToolOutput(string(out))

	case "read_document":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		const maxLen = 40000
		switch strings.ToLower(filepath.Ext(full)) {
		case ".pdf":
			out, err := exec.Command("pdftotext", full, "-").CombinedOutput()
			if err != nil {
				return "error: " + err.Error() + " (requires poppler's pdftotext on PATH)\n" + string(out)
			}
			if len(out) > maxLen {
				return string(out[:maxLen]) + "\n...(truncated)"
			}
			return string(out)
		case ".doc":
			return "error: legacy .doc isn't supported — convert it to .docx or .pdf first"
		case ".docx":
			text, err := readDocxText(full)
			if err != nil {
				return "error: " + err.Error()
			}
			if len(text) > maxLen {
				return text[:maxLen] + "\n...(truncated)"
			}
			return text
		default:
			data, err := os.ReadFile(full)
			if err != nil {
				return "error: " + err.Error()
			}
			if len(data) > maxLen {
				return string(data[:maxLen]) + "\n...(truncated)"
			}
			return string(data)
		}

	case "http_post":
		contentType := toolArgString(args, "content_type")
		if contentType == "" {
			contentType = "application/json"
		}
		client := &http.Client{Timeout: 20 * time.Second}
		resp, err := client.Post(toolArgString(args, "url"), contentType, strings.NewReader(toolArgString(args, "body")))
		if err != nil {
			return "error: " + err.Error()
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return "error: " + err.Error()
		}
		return truncateToolOutput(fmt.Sprintf("status %d\n%s", resp.StatusCode, string(data)))

	case "download_file":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		client := &http.Client{Timeout: 60 * time.Second}
		resp, err := client.Get(toolArgString(args, "url"))
		if err != nil {
			return "error: " + err.Error()
		}
		defer resp.Body.Close()
		out, err := os.Create(full)
		if err != nil {
			return "error: " + err.Error()
		}
		defer out.Close()
		n, err := io.Copy(out, resp.Body)
		if err != nil {
			return "error: " + err.Error()
		}
		return fmt.Sprintf("downloaded %d bytes to: %s", n, full)

	case "send_notification":
		psCmd := fmt.Sprintf("Add-Type -AssemblyName System.Windows.Forms; "+
			"[System.Windows.Forms.MessageBox]::Show(%s, %s) | Out-Null",
			toolArgQuoted(args, "message"), toolArgQuoted(args, "title"))
		if out, err := exec.Command("powershell", "-NoProfile", "-Command", psCmd).CombinedOutput(); err != nil {
			return "error: " + err.Error() + "\n" + string(out)
		}
		return "notification shown"

	case "read_registry":
		psCmd := fmt.Sprintf("Get-ItemProperty -Path '%s' | Format-List", psQuote(toolArgString(args, "key")))
		out, err := exec.Command("powershell", "-NoProfile", "-Command", psCmd).CombinedOutput()
		if err != nil {
			return "error: " + err.Error() + "\n" + string(out)
		}
		return truncateToolOutput(string(out))

	case "run_sql":
		full := resolveAgentPath(workDir, toolArgString(args, "db_path"))
		out, err := exec.Command("sqlite3", full, toolArgString(args, "query")).CombinedOutput()
		if err != nil {
			return "error: " + err.Error() + " (requires the sqlite3 CLI on PATH)\n" + string(out)
		}
		return truncateToolOutput(string(out))

	case "read_csv":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		f, err := os.Open(full)
		if err != nil {
			return "error: " + err.Error()
		}
		defer f.Close()
		rows, err := csv.NewReader(f).ReadAll()
		if err != nil {
			return "error: " + err.Error()
		}
		return truncateToolOutput(formatCSVTable(rows))

	case "read_json":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		data, err := os.ReadFile(full)
		if err != nil {
			return "error: " + err.Error()
		}
		var v interface{}
		if err := json.Unmarshal(data, &v); err != nil {
			return "error: invalid JSON: " + err.Error()
		}
		pretty, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return "error: " + err.Error()
		}
		return truncateToolOutput(string(pretty))

	case "git_commit":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		if out, err := exec.Command("git", "-C", full, "add", "-A").CombinedOutput(); err != nil {
			return "error staging changes: " + err.Error() + "\n" + string(out)
		}
		out, err := exec.Command("git", "-C", full, "commit", "-m", toolArgString(args, "message")).CombinedOutput()
		result := string(out)
		if err != nil {
			result += "\n(exit error: " + err.Error() + ")"
		}
		return result

	case "git_branch":
		full := resolveAgentPath(workDir, toolArgString(args, "path"))
		name := toolArgString(args, "name")
		if name == "" {
			out, err := exec.Command("git", "-C", full, "branch").CombinedOutput()
			if err != nil {
				return "error: " + err.Error() + "\n" + string(out)
			}
			return string(out)
		}
		out, err := exec.Command("git", "-C", full, "checkout", "-b", name).CombinedOutput()
		result := string(out)
		if err != nil {
			result += "\n(exit error: " + err.Error() + ")"
		}
		return result

	case "pull_ollama_model":
		out, err := exec.Command("ollama", "pull", toolArgString(args, "name")).CombinedOutput()
		result := string(out)
		if err != nil {
			result += "\n(exit error: " + err.Error() + ")"
		}
		return truncateToolOutput(result)

	default:
		return "error: unknown tool " + name
	}
}

// toolArgQuoted returns a tool argument as a single-quoted PowerShell string
// literal, with embedded single quotes escaped by doubling — for building
// -Command strings safely from model-supplied text.
func toolArgQuoted(args map[string]interface{}, key string) string {
	return "'" + psQuote(toolArgString(args, key)) + "'"
}

// psQuote escapes single quotes in s by doubling them, for safe embedding
// inside a single-quoted PowerShell string literal.
func psQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// formatCSVTable renders parsed CSV rows as a plain aligned text table.
func formatCSVTable(rows [][]string) string {
	if len(rows) == 0 {
		return "(empty)"
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	var b strings.Builder
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) {
				b.WriteString(cell)
				if i < len(row)-1 {
					b.WriteString(strings.Repeat(" ", widths[i]-len(cell)+2))
				}
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

type agentWarmupMsg struct {
	ready bool
	err   string
}

// warmupPollOllama checks `ollama ps` for whether modelName is already
// loaded into memory, so the chat can show a "connecting..." vs "ready"
// status without competing with the user's own first message for the
// model slot. An earlier version fired its own throwaway generate request
// to force-detect readiness, but that raced with a real message sent while
// the model was still cold-loading/being swapped in and corrupted that
// turn (confirmed: removing the extra request fixed a vision regression) —
// polling `ollama ps` only ever reads state, it never triggers a request.
func warmupPollOllama(modelName string) tea.Cmd {
	return func() tea.Msg {
		out, err := exec.Command("ollama", "ps").CombinedOutput()
		if err != nil {
			return agentWarmupMsg{err: err.Error()}
		}
		return agentWarmupMsg{ready: strings.Contains(string(out), modelName)}
	}
}

// warmupPollTick schedules the next warmupPollOllama check — used while
// still "pending" so the status flips to "ready" shortly after the model
// actually finishes loading, without ever issuing a competing request.
func warmupPollTick(modelName string) tea.Cmd {
	return tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg {
		return warmupPollOllama(modelName)()
	})
}

// agentKeepWarmInterval: how often, while sitting in the agentic chat, to
// verify via `ollama ps` that the model is still actually loaded in
// memory — Ollama unloads an idle model on its own after a while, and a
// long-lived chat session (especially one driving the web server/Telegram
// bot) should catch that and reload it rather than let the next real
// message eat a cold-load delay with no warning.
const agentKeepWarmInterval = 10 * time.Minute

type agentKeepWarmTickMsg struct{}

func agentKeepWarmTickCmd() tea.Cmd {
	return tea.Tick(agentKeepWarmInterval, func(time.Time) tea.Msg {
		return agentKeepWarmTickMsg{}
	})
}

type agentRewarmCheckedMsg struct {
	modelName string
	wasLoaded bool
}

// checkAndRewarmModel runs `ollama ps` and, if the model isn't in the
// list (Ollama unloaded it), fires off `ollama run <model> hi` in the
// background to reload it — fire-and-forget, not awaited, so this never
// blocks the UI waiting for a cold load to finish.
func checkAndRewarmModel(modelName string) tea.Cmd {
	return func() tea.Msg {
		out, err := exec.Command("ollama", "ps").CombinedOutput()
		loaded := err == nil && strings.Contains(string(out), modelName)
		if !loaded {
			_ = exec.Command("ollama", "run", modelName, "hi").Start()
		}
		return agentRewarmCheckedMsg{modelName: modelName, wasLoaded: loaded}
	}
}

// ollamaChatStream sends one /api/chat request with stream:true. Ollama's
// streaming protocol is newline-delimited JSON: each line is a partial
// {"message":{"content":"<token(s)>"},"done":false} chunk, ending with a
// line that has "done":true. Every content delta is sent to deltaCh (if
// non-nil) as it arrives, and the fully assembled message is returned once
// the stream ends.
func ollamaChatStream(modelName string, messages []ollamaChatMsg, tools []ollamaTool, deltaCh chan<- string) (ollamaChatMsg, error) {
	reqBody := ollamaChatRequest{Model: modelName, Messages: messages, Tools: tools, Stream: true}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return ollamaChatMsg{}, err
	}
	client := &http.Client{Timeout: 180 * time.Second}
	resp, err := client.Post("http://localhost:11434/api/chat", "application/json", bytes.NewReader(buf))
	if err != nil {
		return ollamaChatMsg{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return ollamaChatMsg{}, fmt.Errorf("ollama returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var final ollamaChatMsg
	final.Role = "assistant"
	var content strings.Builder
	sawDone := false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var chunk ollamaChatResponse
		if err := json.Unmarshal(line, &chunk); err != nil {
			continue
		}
		if chunk.Message.Content != "" {
			content.WriteString(chunk.Message.Content)
			if deltaCh != nil {
				deltaCh <- chunk.Message.Content
			}
		}
		if len(chunk.Message.ToolCalls) > 0 {
			final.ToolCalls = chunk.Message.ToolCalls
		}
		if chunk.Done {
			sawDone = true
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return ollamaChatMsg{}, err
	}
	// The stream ended (EOF, no read error) but never sent its final
	// done:true chunk — the connection was cut mid-response, almost always
	// because the ollama/llama-server backend crashed or was killed while
	// generating (e.g. the 0xC0000409 stack-overrun crash seen with some
	// models). Without this check that silently looked like a successful
	// empty reply: no error, no content, nothing shown to the user at all.
	if !sawDone {
		return ollamaChatMsg{}, fmt.Errorf("ollama's response stream ended unexpectedly without finishing (the model/backend likely crashed mid-generation — check if `ollama ps` still shows it running)")
	}
	final.Content = content.String()
	return final, nil
}

type agentStreamDeltaMsg struct {
	ch    chan tea.Msg
	delta string
}

type agentStepMsg struct {
	ch             chan tea.Msg
	messages       []ollamaChatMsg
	toolsSupported bool
}

type agentTurnDoneMsg struct {
	messages       []ollamaChatMsg
	err            string
	toolsSupported bool
}

func waitForAgentStream(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		return <-ch
	}
}

// agentSpinnerFrames is a classic braille spinner, ticked while the model
// is thinking/calling tools so the screen doesn't look frozen.
var agentSpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// agentThinkingPhrase rotates the status text alongside the spinner so a
// long wait doesn't just sit on a static "thinking..." — it reads as
// progress toward a finish, not a stall. The first few stages are one-shot
// ("almost done" included on purpose, so waiting reads as nearly over
// rather than open-ended); past that it cycles a small set of longer-wait
// phrases every few seconds so it keeps feeling alive.
func agentThinkingPhrase(elapsed time.Duration) string {
	switch {
	case elapsed < 4*time.Second:
		return "is thinking..."
	case elapsed < 8*time.Second:
		return "is reasoning..."
	case elapsed < 14*time.Second:
		return "is working..."
	case elapsed < 20*time.Second:
		return "is almost done..."
	default:
		phrases := []string{"is still going...", "is taking a while...", "is almost done..."}
		idx := int((elapsed - 20*time.Second) / (6 * time.Second))
		return phrases[idx%len(phrases)]
	}
}

type agentTickMsg struct{}

func agentTickCmd() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg {
		return agentTickMsg{}
	})
}

// cubeTickMsg drives the main-menu banner's rotation — runs for the whole
// program lifetime (cheap: one float increment per tick) rather than only
// while viewMenu is showing, so it doesn't need to be restarted every time
// the user navigates back to the main menu.
type cubeTickMsg struct{}

func cubeTickCmd() tea.Cmd {
	return tea.Tick(90*time.Millisecond, func(time.Time) tea.Msg {
		return cubeTickMsg{}
	})
}

// runAgentTurn drives the tool-call loop for one user message: stream the
// model's reply token-by-token, and while it keeps asking for tools, run
// them locally and feed the results back, until it replies with plain
// content or maxSteps is hit. Runs in a goroutine and reports back over a
// channel so the UI can update live as tokens arrive.
// suppressTools disables tools for just this one turn without touching the
// model's underlying toolsSupported capability flag — used for "auto" tool
// mode, which skips tools on any turn that attaches an image (see the call
// site for why) but must not let that turn's request permanently forget the
// model actually does support tools once no image is involved.
// agentSystemPrompt builds the system message for a fresh agentic chat,
// shared by the TUI's own chat and the local web server so both get the
// exact same tool-usage rules (file access, web/RSS/Tavily fallback
// order, etc.) rather than two prompts drifting apart over time.
func agentSystemPrompt(wd string) string {
	return fmt.Sprintf(
		"You are a local coding assistant with REAL file system access via read_file, "+
			"write_file, append_file, list_dir, make_dir, delete_file, search_files — these "+
			"reach any path on disk, not just one folder. Relative paths resolve against %s; "+
			"absolute paths (C:\\..., D:\\...) work as given, unrestricted. Never claim you lack "+
			"file access or are sandboxed to one directory — you're not. Use list_dir for "+
			"directory contents, read_file for one file's contents. On any file/fs request, "+
			"call the matching tool immediately and report the real result (errors included) "+
			"— don't ask for clarification or describe what the user should do manually. "+
			"Tools ADD to your abilities, they don't replace normal chat — only use one when "+
			"the request actually needs file/system/network access; never refuse a normal "+
			"request by claiming you're 'only' a tool assistant. "+
			"Web tools: web_search gives snippets only, never full article text — for a "+
			"specific story or 'top N' request, follow up with read_webpage or rss_feed on "+
			"the best result and answer from that; don't stop at a link list. "+
			"HARD RULE for any live/current-value question — a stock price, an index level "+
			"(NASDAQ, S&P 500, Dow, etc.), an exchange rate, a score, the weather, anything that "+
			"changes in real time: you MUST call web_search (or tavily_search) and then "+
			"read_webpage/tavily_extract on a real result to actually fetch the current number, "+
			"then answer with that number. NEVER respond with 'I don't have real-time access' "+
			"or hand back a list of links for the user to go check themselves — you have the "+
			"tools to check it yourself, so use them and report the actual value you found "+
			"(plus its source URL). Only say you couldn't find it after actually trying the "+
			"tools and getting nothing usable back. "+
			"HARD RULE for ALL web results, not just 'top N': every item in ANY answer built "+
			"from web_search/tavily_search/read_webpage/rss_feed must include BOTH the actual "+
			"URL AND a one-to-two-sentence summary of what it's actually about, taken from the "+
			"snippet/content the tool gave you — a bare link, or a title with no summary, is "+
			"NEVER an acceptable answer on its own; the user wants to know what the content "+
			"actually says without clicking through. "+
			"For ANY 'top N' request specifically (a site's headlines, or a general topic like "+
			"'top 10 news about X'), your final answer MUST additionally list exactly N distinct "+
			"items (same URL+summary rule applies to each). If web_search's first batch has fewer than N usable "+
			"results, call it again with a broader/different query instead of quietly stopping "+
			"short or padding with vague unsourced claims. "+
			"HARD RULE: a section/category homepage (e.g. 'BBC News - World', 'Reuters World "+
			"News', a site's /world or /news landing page) is NOT a story — its web_search "+
			"snippet is just that page's generic meta description ('the latest news from X'), "+
			"which is not a real news item. When asked for 'top N stories about the world/news' "+
			"with no single site named, skip web_search entirely and go straight to "+
			"rss_feed on https://news.google.com/rss?hl=en-US&gl=US&ceid=US:en — this is a known-"+
			"working general headline feed covering exactly this case, do not try find_rss_feed on "+
			"individual outlets (reuters.com/cnn.com/bbc.com/etc.) for this general request, that "+
			"is slow and unreliable; save find_rss_feed for when the user names one specific site. "+
			"Report N real individual articles from that feed's actual items — never list "+
			"homepages as if they were the stories, and never say you 'can't pull a live list "+
			"without a functional RSS feed' without having actually called rss_feed on that URL "+
			"first. "+
			"HARD RULE, no exceptions: NEVER call read_webpage on finance.yahoo.com or "+
			"ynet.co.il, no matter how the user phrases the request (news, topics, headlines, "+
			"stories, top N, or just mentioning the site) and even if you already have an older "+
			"read_webpage result for that site earlier in this conversation — that old result is "+
			"a vague unusable blob, not real headlines, so don't reuse or re-summarize it. "+
			"Always call rss_feed fresh instead: finance.yahoo.com -> "+
			"https://finance.yahoo.com/news/rssindex, ynet.co.il -> "+
			"https://www.ynet.co.il/Integration/StoryRss2.xml (both sites show a cookie-consent "+
			"wall via plain GET, which is exactly why read_webpage on them only ever produces "+
			"vague theme summaries instead of actual news items). For any other site, call "+
			"find_rss_feed first to discover its real feed — do not guess a path, and do not "+
			"fall back to read_webpage on that site's raw HTML for a 'topics'/'themes' summary "+
			"either, since that produces the same vague, no-real-items failure. If find_rss_feed "+
			"finds nothing, fall back to tavily_extract/web_search — read_webpage's stripped "+
			"text can't be split into an exact count of items, so treat it as a last resort, "+
			"not a first choice, for any 'top N' or 'topics/themes' request. Prefer tavily_extract "+
			"over read_webpage when TAVILY_API_KEY is set (cleaner text, no consent walls); "+
			"if it errors for a missing key, just fall back silently. Every web-sourced item "+
			"in your answer MUST cite its actual URL, formatted as a markdown link wrapped "+
			"around the item's own headline/title text — e.g. "+
			"'[Stanley Druckenmiller and Cathie Wood agree on 2 tech giant stocks]"+
			"(https://finance.yahoo.com/markets/stocks/articles/...)' — never print the raw URL "+
			"as separate visible text like 'URL: https://...' or on its own line, and never "+
			"write '(Source: MSN)'/'(Bloomberg)' alone with no link at all: the headline text "+
			"itself IS the clickable citation. "+
			"CRITICAL: if the user's message is about reaching/browsing/connecting to "+
			"THIS ASSISTANT itself over a network — any phrasing at all, including vague or "+
			"garbled ones like 'how to browse to u', 'to your address so I can browse', "+
			"'give me your address', 'what's your link', 'how do I open you in a browser', "+
			"'your url' — you MUST treat that as a request for the web UI URL and call "+
			"get_web_ui_url immediately. Do NOT reply that you are an AI with no physical "+
			"address, do NOT ask the user to clarify or provide a URL themselves — YOU are "+
			"the one who has the URL, they are asking YOU for it. Call get_web_ui_url and "+
			"return EVERY line it gives you verbatim — the loopback URL AND every LAN URL, "+
			"each on its own line, exactly as returned. This is very often asked from a phone "+
			"over Telegram, where the loopback (127.0.0.1) URL is USELESS — the LAN URLs are "+
			"the ones that actually work from another device, so never trim the reply down to "+
			"just the first link or summarize the rest as 'other links'.", wd,
	)
}

// shouldRetryWithoutTools reports whether a chat error means "give up on
// tools for this turn and retry plain" rather than a real failure worth
// surfacing as-is. Covers two distinct cases: (1) Ollama flatly rejects
// models with no tool-calling capability, and (2) some models (seen with
// gemma4:e2b) crash their native llama-server backend outright — exit
// 0xc0000409, GGML_ASSERT(n_inputs < GGML_SCHED_MAX_SPLIT_INPUTS) — when
// handed this app's full tool list (60+ schemas) in one request. That's a
// fixed scheduler limit in the backend, not something a bigger/smaller
// prompt fixes, so the only working mitigation is dropping tools entirely
// for this turn and answering without them instead of hard-failing.
func shouldRetryWithoutTools(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "does not support tools") ||
		strings.Contains(msg, "GGML_SCHED_MAX_SPLIT_INPUTS") ||
		strings.Contains(msg, "llama-server process has terminated")
}

// liveDataRe matches requests for a value that changes in real time — a
// small model's trained-in "I don't have real-time access" refusal is
// strong enough to override the system prompt's tool-usage rules, so a
// per-turn reminder right next to the actual question (not just once at
// the top of the conversation) is what actually gets it to call the tool.
var liveDataRe = regexp.MustCompile(`(?i)\b(stock price|share price|stock index|stock indexes|` +
	`stock indices|market index|market indices|index value|index values|indices|nasdaq|s&p ?500|` +
	`dow jones|kospi|kosdaq|nikkei|hang seng|sensex|nifty|exchange rate|currency rate|` +
	`current price|current value|closing value|closing price|live score|` +
	`weather (in|for|today)|forecast)\b`)

// liveDataNudge returns a one-off system reminder to inject right before
// the model call when the latest user message asks for a live value — it
// is never stored back into the conversation history, only sent for this
// one request, so it doesn't show up as a stray system bubble in the chat.
// formatElapsedDuration renders "56s", or "1m 04s" past a minute — mirrors
// the web UI's formatElapsed so Telegram replies carry the same at-a-glance
// timing info.
func formatElapsedDuration(d time.Duration) string {
	total := int(d.Round(time.Second).Seconds())
	if total < 60 {
		return fmt.Sprintf("%ds", total)
	}
	return fmt.Sprintf("%dm %02ds", total/60, total%60)
}

func liveDataNudge(msgs []ollamaChatMsg) *ollamaChatMsg {
	if len(msgs) == 0 {
		return nil
	}
	last := msgs[len(msgs)-1]
	if last.Role != "user" || !liveDataRe.MatchString(last.Content) {
		return nil
	}
	return &ollamaChatMsg{Role: "system", Content: "The message right above asks for a live/" +
		"current value. If it's a stock price or a market index, call get_stock_quote with the " +
		"right Yahoo Finance symbol — a LARGE reference table, use it for ANY country's index, " +
		"not just the well-known ones: NASDAQ-100 -> ^NDX, NASDAQ Composite -> ^IXIC, S&P 500 -> " +
		"^GSPC, Dow Jones -> ^DJI, Russell 2000 -> ^RUT, VIX -> ^VIX, TA-35 -> TA35.TA, TA-125 -> " +
		"TA125.TA, FTSE 100 -> ^FTSE, DAX (Germany) -> ^GDAXI, CAC 40 (France) -> ^FCHI, IBEX 35 " +
		"(Spain) -> ^IBEX, FTSE MIB (Italy) -> FTSEMIB.MI, Euro Stoxx 50 -> ^STOXX50E, AEX " +
		"(Netherlands) -> ^AEX, SMI (Switzerland) -> ^SSMI, OMX Stockholm -> ^OMX, Nikkei 225 -> " +
		"^N225, Hang Seng -> ^HSI, Shanghai Composite -> 000001.SS, Shenzhen Component -> " +
		"399001.SZ, KOSPI (South Korea) -> ^KS11, KOSDAQ -> ^KQ11, TAIEX (Taiwan) -> ^TWII, " +
		"Nifty 50 (India) -> ^NSEI, Sensex (India) -> ^BSESN, Straits Times (Singapore) -> ^STI, " +
		"ASX 200 (Australia) -> ^AXJO, NZX 50 (New Zealand) -> ^NZ50, TSX (Canada) -> ^GSPTSE, " +
		"Bovespa (Brazil) -> ^BVSP, IPC (Mexico) -> ^MXX, MERVAL (Argentina) -> ^MERV, MOEX " +
		"(Russia) -> IMOEX.ME, JSE Top 40 (South Africa) -> ^J200, EGX 30 (Egypt) -> ^CASE30, a " +
		"company -> its ticker. The user's own wording may be garbled/typo'd (phone autocorrect, " +
		"etc.) — figure out what they actually meant and use the REAL symbol from this list, " +
		"never pass their garbled text straight through as the symbol. If the country/index " +
		"isn't in this table, or get_stock_quote comes back with no data, call web_search first " +
		"to find the right Yahoo Finance symbol/number instead of giving up or claiming you have " +
		"no way to check it — you do, you just need the right symbol first. get_stock_quote hits " +
		"a real JSON API and always returns the actual number, no scraping or consent walls " +
		"involved, so prefer it over web_search/read_webpage once you have the right symbol. For " +
		"anything else live (weather, a score, etc.), call web_search or tavily_search " +
		"first — snippets often contain the number directly. Then answer with the actual number " +
		"you got back. Do NOT say you lack real-time access and do NOT just hand back a list of " +
		"links; fetch it yourself and report the real value."}
}

func runAgentTurn(modelName string, history []ollamaChatMsg, workDir string, toolsSupported bool, suppressTools bool) tea.Cmd {
	ch := make(chan tea.Msg)
	go func() {
		msgs := append([]ollamaChatMsg(nil), history...)
		// attempt streams one request with the given tool list, forwarding
		// live tokens to ch as they arrive.
		attempt := func(tools []ollamaTool) (ollamaChatMsg, error) {
			deltaCh := make(chan string)
			type result struct {
				reply ollamaChatMsg
				err   error
			}
			resultCh := make(chan result, 1)
			go func() {
				reply, err := ollamaChatStream(modelName, msgs, tools, deltaCh)
				close(deltaCh)
				resultCh <- result{reply, err}
			}()
			for d := range deltaCh {
				ch <- agentStreamDeltaMsg{ch: ch, delta: d}
			}
			res := <-resultCh
			return res.reply, res.err
		}
		const maxSteps = 8
		for i := 0; i < maxSteps; i++ {
			var tools []ollamaTool
			if toolsSupported && !suppressTools {
				tools = agentTools()
			}
			nudged := false
			if i == 0 {
				if n := liveDataNudge(msgs); n != nil {
					msgs = append(msgs, *n)
					nudged = true
				}
			}
			reply, err := attempt(tools)
			// Some models report no "tools" capability at all — Ollama
			// rejects the request outright rather than just ignoring the
			// tool list. Downgrade to a plain chat (no file/tool access)
			// instead of hard-failing, and remember the downgrade so later
			// turns in this same chat skip straight to the plain request.
			if err != nil && toolsSupported && shouldRetryWithoutTools(err) {
				toolsSupported = false
				reply, err = attempt(nil)
			}
			if nudged {
				msgs = msgs[:len(msgs)-1]
			}
			if err != nil {
				ch <- agentTurnDoneMsg{messages: msgs, err: err.Error(), toolsSupported: toolsSupported}
				return
			}
			msgs = append(msgs, reply)
			if len(reply.ToolCalls) == 0 {
				break
			}
			for _, tc := range reply.ToolCalls {
				toolResult, toolImages := executeAgentToolWithImages(workDir, tc.Function.Name, tc.Function.Arguments)
				msgs = append(msgs, ollamaChatMsg{Role: "tool", Content: toolResult, Images: toolImages})
			}
			ch <- agentStepMsg{ch: ch, messages: append([]ollamaChatMsg(nil), msgs...), toolsSupported: toolsSupported}
		}
		ch <- agentTurnDoneMsg{messages: msgs, toolsSupported: toolsSupported}
	}()
	return waitForAgentStream(ch)
}

// runAgentTurnSync is the same tool-calling loop as runAgentTurn, blocking
// instead of streaming through bubbletea messages — for callers with no
// tea.Program to drive, like the local web server's HTTP handler.
func runAgentTurnSync(modelName string, history []ollamaChatMsg, workDir string, toolsSupported bool) ([]ollamaChatMsg, error) {
	msgs := append([]ollamaChatMsg(nil), history...)
	const maxSteps = 8
	for i := 0; i < maxSteps; i++ {
		var tools []ollamaTool
		if toolsSupported {
			tools = agentTools()
		}
		nudged := false
		if i == 0 {
			if n := liveDataNudge(msgs); n != nil {
				msgs = append(msgs, *n)
				nudged = true
			}
		}
		reply, err := ollamaChatStream(modelName, msgs, tools, nil)
		if err != nil && toolsSupported && shouldRetryWithoutTools(err) {
			toolsSupported = false
			reply, err = ollamaChatStream(modelName, msgs, nil, nil)
		}
		if nudged {
			msgs = msgs[:len(msgs)-1]
		}
		if err != nil {
			return msgs, err
		}
		msgs = append(msgs, reply)
		if len(reply.ToolCalls) == 0 {
			break
		}
		for _, tc := range reply.ToolCalls {
			toolResult, toolImages := executeAgentToolWithImages(workDir, tc.Function.Name, tc.Function.Arguments)
			msgs = append(msgs, ollamaChatMsg{Role: "tool", Content: toolResult, Images: toolImages})
		}
	}
	return msgs, nil
}

type modelRow struct {
	Name     string `json:"name"`
	Size     string `json:"size"`
	Modified string `json:"modified"`
	Arch     string `json:"arch"`
	Params   string `json:"params"`
	Quant    string `json:"quant"`
	Context  string `json:"context"`
	// Capabilities from `ollama show`'s "Capabilities" section, e.g.
	// "completion,vision,tools" — tells you at a glance whether a model is
	// chat-only, multimodal (vision/audio), or usable for tool-calling.
	Capabilities string `json:"capabilities"`

	// Populated by the "benchmark all" pass triggered from Device Info.
	Benchmarked bool    `json:"benchmarked"`
	CPUGPU      string  `json:"cpu_gpu"`
	LoadSecs    float64 `json:"load_secs"`
	MatchScore  int     `json:"match_score"`
}

type scanStartMsg struct {
	rows []modelRow
	err  string
}

type scanStepMsg struct {
	index int
	row   modelRow
}

func disclaimerAcceptedFilePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "llama-shell", "disclaimer_accepted")
}

func isDisclaimerAccepted() bool {
	_, err := os.Stat(disclaimerAcceptedFilePath())
	return err == nil
}

func markDisclaimerAccepted() {
	path := disclaimerAcceptedFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte(time.Now().Format(time.RFC3339)), 0o644)
}

// clearDisclaimerAccepted removes any existing acceptance marker on
// decline — not just "don't create one". Without this, declining after a
// PRIOR run had already accepted (marker file already on disk from
// before) would silently do nothing: the old marker stays, and the next
// launch skips straight past the gate as if nothing had changed.
func clearDisclaimerAccepted() {
	_ = os.Remove(disclaimerAcceptedFilePath())
}

// tavilyKeyFilePath is where the Tavily API key is persisted locally, so
// the user only has to enter it once via the settings screen instead of
// setting an environment variable by hand every session.
func tavilyKeyFilePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "llama-shell", "tavily_api_key")
}

// saveTavilyKey persists the key to disk and makes it immediately usable
// in this process (the tool handlers read it via os.Getenv), so a restart
// isn't needed after saving.
func saveTavilyKey(key string) error {
	// Defense in depth: strip any non-printable rune (a stray control
	// character slipping in during a paste is exactly what made
	// os.Setenv fail with "invalid argument" before the input filter
	// existed) so a bad paste can't silently corrupt the saved key either.
	key = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, key)
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("key is empty after removing invalid characters")
	}
	path := tavilyKeyFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		return err
	}
	return os.Setenv("TAVILY_API_KEY", key)
}

// webServerConfig persists across restarts: whether the local web server
// should be running and which model it serves.
type webServerConfig struct {
	Enabled bool   `json:"enabled"`
	Model   string `json:"model"`
	Token   string `json:"token"`
	// Port is 0 in an existing config saved before this field existed,
	// or if the user never changed it — webServerEffectivePort treats
	// 0 as "use the default", so old configs keep working unchanged.
	Port int `json:"port,omitempty"`
}

// webServerEffectivePort is the port actually used — cfg.Port if the
// user set one, otherwise the original hardcoded default.
func webServerEffectivePort(cfg webServerConfig) int {
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return webServerDefaultPort
	}
	return cfg.Port
}

// genWebServerToken makes a random per-install access token, embedded as
// a query param in the URL shown to the user. Binding to 127.0.0.1 keeps
// this off the network, but it's not a security boundary against other
// software/browser tabs on the same machine — this token is the actual
// gate: without it every request gets a 403, since the API underneath
// grants full local tool access (files, commands, network).
func genWebServerToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// autoUpdateConfig persists the auto-update checkbox and its daily check
// time. Enabled defaults to true and Hour/Minute default to 3:00 AM when no
// config file exists yet (first run) — the feature is opt-out, not opt-in,
// per how it was asked for.
type autoUpdateConfig struct {
	Enabled bool `json:"enabled"`
	Hour    int  `json:"hour"`
	Minute  int  `json:"minute"`
}

func defaultAutoUpdateConfig() autoUpdateConfig {
	return autoUpdateConfig{Enabled: true, Hour: 3, Minute: 0}
}

func autoUpdateConfigPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "llama-shell", "autoupdate_config.json")
}

func loadAutoUpdateConfig() autoUpdateConfig {
	data, err := os.ReadFile(autoUpdateConfigPath())
	if err != nil {
		return defaultAutoUpdateConfig()
	}
	var cfg autoUpdateConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return defaultAutoUpdateConfig()
	}
	return cfg
}

func saveAutoUpdateConfig(cfg autoUpdateConfig) error {
	path := autoUpdateConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func webServerConfigPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "llama-shell", "webserver_config.json")
}

func loadWebServerConfig() webServerConfig {
	data, err := os.ReadFile(webServerConfigPath())
	if err != nil {
		return webServerConfig{}
	}
	var cfg webServerConfig
	_ = json.Unmarshal(data, &cfg)
	return cfg
}

func saveWebServerConfig(cfg webServerConfig) error {
	path := webServerConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

const webServerDefaultPort = 8787

var (
	webServerMu      sync.Mutex
	webServerHTTP    *http.Server
	webServerModel   string
	webServerWorkDir string
	// webServerLastErr holds the reason the most recent start attempt
	// failed (e.g. auto-start at launch racing a port already in use), so
	// the settings screen can show WHY it's "enabled but not running"
	// instead of leaving that a silent mystery.
	webServerLastErr string
)

// isWebServerRunning reports whether the local web server is currently
// listening in this process.
func isWebServerRunning() bool {
	webServerMu.Lock()
	defer webServerMu.Unlock()
	return webServerHTTP != nil
}

// webServerURL is what gets shown to the user for a given host/IP — the
// token is a required query param, not decoration, so a bare link with
// no token 403s.
func webServerURL(token string, port int) string {
	return webServerURLFor("127.0.0.1", token, port)
}

func webServerURLFor(host, token string, port int) string {
	return fmt.Sprintf("http://%s:%d/?token=%s", host, port, token)
}

// localLANIPv4s returns this machine's non-loopback IPv4 addresses —
// the ones a phone on the same WiFi could actually use to reach the web
// UI, now that it binds all interfaces instead of loopback-only.
func localLANIPv4s() []string {
	var ips []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() {
			// Link-local (169.254.x.x) is what an adapter auto-assigns
			// itself when DHCP fails — never a real, reachable LAN
			// address, just clutter alongside the actual WiFi/Ethernet IP.
			continue
		}
		v4 := ipNet.IP.To4()
		if v4 == nil {
			continue
		}
		ips = append(ips, v4.String())
	}
	return ips
}

// startWebServer starts (or is a no-op if already running) the local
// chat server, bound to all interfaces so it's reachable
// from this machine alone, never the local network.
func startWebServer(cfg webServerConfig, workDir string) error {
	webServerMu.Lock()
	defer webServerMu.Unlock()
	if webServerHTTP != nil {
		return nil
	}
	// Binds all interfaces (not just loopback) so it's reachable from
	// other devices on the same LAN — e.g. a phone browsing to it. Still
	// gated by cfg.Token on every request; that's the real security
	// boundary now, since the network boundary is deliberately wider
	// than before.
	port := webServerEffectivePort(cfg)
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		webServerLastErr = err.Error()
		return err
	}
	webServerLastErr = ""
	webServerModel = cfg.Model
	webServerWorkDir = workDir
	mux := http.NewServeMux()
	mux.HandleFunc("/", webRequireToken(cfg.Token, webHandleChatPage))
	mux.HandleFunc("/api/chat", webRequireToken(cfg.Token, webHandleChatAPI))
	mux.HandleFunc("/api/tools", webRequireToken(cfg.Token, webHandleTools))
	mux.HandleFunc("/api/warmup", webRequireToken(cfg.Token, webHandleWarmup))
	mux.HandleFunc("/api/status", webRequireToken(cfg.Token, webHandleStatus))
	mux.HandleFunc("/api/logs", webRequireToken(cfg.Token, webHandleLogs))
	srv := &http.Server{Handler: mux}
	webServerHTTP = srv
	go func() {
		_ = srv.Serve(ln)
	}()
	appendLog("web server started on %s (model %s)", webServerURL(cfg.Token, port), cfg.Model)
	return nil
}

// stopWebServer shuts the server down if running; a no-op otherwise.
func stopWebServer() {
	webServerMu.Lock()
	srv := webServerHTTP
	webServerHTTP = nil
	webServerMu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	appendLog("web server stopped")
}

// webRequireToken gates every route behind the configured access token —
// see genWebServerToken's comment for why this matters even bound to
// localhost.
func webRequireToken(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query().Get("token")
		if got == "" {
			got = r.Header.Get("X-Auth-Token")
		}
		if token == "" || got != token {
			http.Error(w, "forbidden — missing or wrong token", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// webChatPageHTML is a minimal, self-contained chat UI: no build step, no
// external assets — just enough to drive the same tool-calling agent as
// the TUI's own agentic chat, from a browser.
const webChatPageHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>llama-shell</title>
<link rel="icon" href="data:image/svg+xml,<svg xmlns=%22http://www.w3.org/2000/svg%22 viewBox=%220 0 100 100%22><text y=%22.9em%22 font-size=%2290%22>🦙</text></svg>">
<style>
  :root {
    --bg: #212123; --bg-raised: #2a2a2d; --bg-inset: #1a1a1c;
    --text: #e8e8ea; --text-dim: #98989e; --border: #38383c;
    --accent: #8b7cf6; --accent-dim: #635a9e;
    --user-tint: rgba(139,124,246,0.08);
  }
  * { box-sizing: border-box; }
  body {
    background: var(--bg); color: var(--text); margin: 0; padding: 0;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    font-size: 15px; line-height: 1.6;
  }
  code, pre, .mono { font-family: ui-monospace, SFMono-Regular, "Cascadia Code", Consolas, Menlo, monospace; }

  #topbar {
    position: fixed; top:0; left:0; right:0; min-height: 56px; z-index: 5;
    display:flex; flex-wrap: wrap; align-items:center; justify-content:space-between; row-gap: 6px;
    padding: 10px 20px; background: var(--bg); border-bottom: 1px solid var(--border);
  }
  #topbar .brand { display:flex; align-items:center; gap:10px; font-weight:600; flex-shrink:0; }
  #topbar .brand .dot { width:8px; height:8px; border-radius:50%; background: var(--accent); }
  #topbar .meta { color: var(--text-dim); font-size: 13px; margin-left: 8px; }
  #topbar .meta .mono { color: var(--text); }
  /* overflow-x:auto so on a narrow phone screen the badges/Tools/Help
     stay reachable by swiping instead of silently overflowing past the
     viewport edge with no way to get to them. */
  #topbar .icons { display:flex; gap:8px; overflow-x:auto; -webkit-overflow-scrolling:touch;
                    scrollbar-width:none; min-width:0; }
  #topbar .icons::-webkit-scrollbar { display:none; }
  #topbar .icons .iconbtn, #topbar .icons #badges, #topbar .icons .gh-link { flex-shrink:0; }
  #badges { flex-shrink:0; }
  #menuBtn { display:none; }
  #menuDropdown {
    display:none; position:fixed; top:56px; right:12px; z-index:6;
    background: var(--bg-raised); border: 1px solid var(--border); border-radius: 10px;
    padding: 6px; flex-direction: column; min-width: 160px;
    box-shadow: 0 8px 24px rgba(0,0,0,0.35);
  }
  #menuDropdown.open { display:flex; }
  #menuDropdown .menuItem {
    display:block; padding: 10px 12px; border-radius: 7px; cursor:pointer;
    color: var(--text); font-size: 14px; text-decoration:none;
  }
  #menuDropdown .menuItem:hover { background: var(--bg); }
  .iconbtn {
    display:flex; align-items:center; gap:6px; cursor:pointer; padding:7px 12px; border-radius:8px;
    color: var(--text-dim); font-size: 13px; border: 1px solid transparent;
  }
  .iconbtn:hover { background: var(--bg-raised); color: var(--text); border-color: var(--border); }
  .badge { font-size: 12px; padding: 4px 9px; border-radius: 6px; margin-right: 6px; white-space: nowrap; }
  .badge.on { background: rgba(74,222,128,0.12); color: #4ade80; }
  .badge.warn { background: rgba(255,215,0,0.12); color: #FFD700; }
  .badge.off { background: var(--bg-raised); color: var(--text-dim); }

  #chatWrap { padding-top: 56px; padding-bottom: 140px; min-height: 100vh; }
  #chat { max-width: 720px; margin: 0 auto; padding: 8px 24px; }

  .msg { padding: 22px 0; }
  .msg.assistant { }
  .msg .role { font-size: 12px; font-weight: 600; color: var(--text-dim); margin-bottom: 6px;
               text-transform: uppercase; letter-spacing: 0.03em; }
  .msg.user .role { color: var(--accent); }
  .msg .content { white-space: normal; word-wrap: break-word; }
  .msg .content p { margin: 0 0 12px 0; }
  .msg .content p:last-child { margin-bottom: 0; }
  .replyTime { font-size: 11px; color: var(--text-dim); margin-top: 8px; font-variant-numeric: tabular-nums; }
  .msg .content a { color: var(--accent); text-decoration: none; }
  .msg .content a:hover { text-decoration: underline; }
  .msg .content code { background: var(--bg-inset); padding: 2px 5px; border-radius: 4px; font-size: 13px; }
  .msg .content pre { background: var(--bg-inset); padding: 12px 14px; border-radius: 8px; overflow-x: auto;
                       border: 1px solid var(--border); }
  .msg .content pre code { background: none; padding: 0; }
  .msg .content ul, .msg .content ol { margin: 0 0 12px 0; padding-left: 22px; }
  .msg.error .content { color: #ff8080; }

  .step {
    margin: 10px 0; border: 1px solid var(--border); border-radius: 8px; background: var(--bg-raised);
    overflow: hidden;
  }
  .step summary {
    cursor: pointer; padding: 9px 12px; font-size: 13px; color: var(--text-dim);
    display: flex; align-items: center; gap: 8px; list-style: none;
  }
  .step summary::-webkit-details-marker { display: none; }
  .step summary::before { content: '▸'; font-size: 11px; }
  .step[open] summary::before { content: '▾'; }
  .step summary .toolname { color: var(--accent); font-weight: 600; }
  .step .stepbody { padding: 0 12px 12px 12px; }
  .step .stepbody pre { margin: 6px 0 0 0; background: var(--bg-inset); padding: 10px 12px;
                          border-radius: 6px; font-size: 12.5px; max-height: 320px; overflow: auto;
                          white-space: pre-wrap; word-wrap: break-word; }

  .thinking { display:flex; align-items:center; gap:10px; padding: 22px 0; color: var(--text-dim); font-size: 14px; }
  .thinking .dots span { display:inline-block; width:6px; height:6px; border-radius:50%; background: var(--accent);
                          margin-right: 3px; animation: pulse 1.2s infinite ease-in-out; }
  .thinking .dots span:nth-child(2) { animation-delay: 0.15s; }
  .thinking .dots span:nth-child(3) { animation-delay: 0.3s; }
  @keyframes pulse { 0%,80%,100% { opacity: 0.25; } 40% { opacity: 1; } }

  #composerWrap {
    position: fixed; bottom: 0; left: 0; right: 0; padding: 16px 24px 20px 24px;
    background: linear-gradient(to top, var(--bg) 60%, transparent);
  }
  #composer {
    max-width: 720px; margin: 0 auto; display:flex; align-items:flex-end; gap:10px;
    background: var(--bg-raised); border: 1px solid var(--border); border-radius: 16px; padding: 10px 10px 10px 16px;
  }
  #input {
    flex:1; background: transparent; color: var(--text); border: none; outline: none;
    font: inherit; resize: none; max-height: 200px; padding: 6px 0;
  }
  #send {
    background: var(--accent); color: #fff; border: none; border-radius: 10px; width: 36px; height: 36px;
    cursor: pointer; font-size: 16px; flex-shrink: 0;
  }
  #send.stop { background: #c25050; }
  #hint { text-align:center; font-size: 11px; color: var(--text-dim); margin-top: 8px; }

  #overlay { display:none; position:fixed; top:0; left:0; right:0; bottom:0; background:rgba(0,0,0,0.6);
             z-index:10; align-items:center; justify-content:center; }
  #overlay.open { display:flex; }
  #panel { background: var(--bg); border: 1px solid var(--border); border-radius: 12px; max-width:720px;
           max-height:80vh; width:90%; display:flex; flex-direction:column; overflow:hidden; }
  #panelHeader { flex-shrink:0; padding: 20px 24px 14px 24px; border-bottom: 1px solid var(--border); }
  #panelHeader h2 { margin: 0; }
  #panelHeader p { color: var(--text-dim); margin: 8px 0 0 0; font-size: 13px; }
  .toolsHintRow { display:flex; align-items:flex-start; justify-content:space-between; gap:12px; margin-top:8px; }
  .toolsHintRow p { margin:0; flex:1; }
  .toolsCollapseBtns { display:flex; gap:10px; flex-shrink:0; white-space:nowrap; padding-top:1px; }
  .miniBtn { color: var(--accent); font-size: 12px; cursor:pointer; text-decoration: underline; }
  .miniBtn:hover { color: var(--text); }
  #toolSearch { width: 100%; margin-top: 10px; background: var(--bg-inset); color: var(--text);
                border: 1px solid var(--border); border-radius: 8px; padding: 8px 12px; font: inherit;
                font-size: 13px; outline: none; }
  #toolSearch:focus { border-color: var(--accent); }
  #logSearch { width: 100%; margin-top: 10px; background: var(--bg-inset); color: var(--text);
               border: 1px solid var(--border); border-radius: 8px; padding: 8px 12px; font: inherit;
               font-size: 13px; outline: none; }
  #logSearch:focus { border-color: var(--accent); }
  .logLines { font-size: 12px; white-space: pre-wrap; word-break: break-word; color: var(--text-dim); }
  #panelClose { float:right; cursor:pointer; color: var(--text-dim); }
  #panelClose:hover { color: var(--text); }
  #panelBody { overflow-y:auto; padding: 8px 24px 24px 24px; flex:1; }
  .catGroup { margin-top: 14px; }
  .catGroup summary::-webkit-details-marker { display: none; }
  .catHeading { color: #4ade80; font-weight: 700; font-size: 12px; text-transform: uppercase;
                letter-spacing: 0.04em; cursor: pointer; list-style: none; padding: 4px 0;
                display: flex; align-items: center; gap: 6px; }
  .catHeading::before { content: '▸'; font-size: 10px; color: #4ade80; }
  .catGroup[open] > .catHeading::before { content: '▾'; }
  .catBody { padding-left: 2px; }
  .catHeading:first-child { margin-top: 12px; }
  .toolRow { padding:10px 0; border-bottom:1px solid var(--border); cursor:pointer; display:flex; gap:10px; }
  .toolRow:hover { background: var(--bg-raised); }
  .toolRow .num { color: var(--text-dim); font-size: 13px; flex-shrink:0; width: 22px; padding-top: 1px; }
  .toolRow .body { flex:1; min-width:0; }
  .toolRow .name { color: var(--accent); font-weight:600; }
  .toolRow .desc { color: var(--text-dim); font-size: 13px; margin-top: 2px; }
  .toolRow .ex { color: var(--text-dim); font-size: 12px; margin-top: 4px; font-style: italic;
                 display:flex; align-items:center; gap:6px; }
  .toolRow .ex .copyIcon { cursor:pointer; color: var(--text-dim); flex-shrink:0; display:flex; }
  .toolRow .ex .copyIcon:hover { color: var(--accent); }
  .toolRow .ex .copyIcon.copied { color: #5fd7a0; }

  @media (max-width: 640px) {
    #topbar { padding: 0 12px; }
    /* Hidden rather than wrapped: a wrapping topbar has variable height,
       which would desync #chatWrap's fixed padding-top and #log's fixed
       top offset, shifting content under the bar. The model/cwd meta is
       nice-to-have on desktop, not essential on a phone. */
    #topbar .meta { display: none; }
    .iconbtn { padding: 6px 9px; font-size: 12px; gap: 4px; }
    /* Trim the GitHub link's own padding and the badge gaps rather than
       hide anything — Tools/Help stay reachable by swiping the icons row
       if the screen is narrow enough that it still doesn't all fit. */
    .badge { font-size: 11px; padding: 3px 7px; margin-right: 4px; }
    /* Fold GitHub/Tools/Help into the burger menu on mobile instead of
       relying on a horizontal scroll to reach them — a scrollable row
       that doesn't visually hint it scrolls just looks broken. */
    #topbar .icons > .gh-link, #topbar .icons > #toolsBtn, #topbar .icons > #helpBtn { display:none; }
    #menuBtn { display:flex; font-size: 22px; padding: 8px 14px; }
    /* Badges wrap onto their own full-width row below the brand/menu row
       — order puts brand first, the menu button second (same line), and
       the badges last with flex-basis:100% so they fall to line two
       instead of squeezing the burger into an unreadably thin sliver. */
    #topbar .brand { order: 1; }
    #topbar .icons { order: 2; }
    #badges { order: 3; flex-basis: 100%; }
    #chatWrap { padding-top: 92px; }
    #menuDropdown { top: 92px; }
    #chat { padding: 8px 14px; }
    .msg { padding: 16px 0; }
    #composerWrap { padding: 10px 12px 14px 12px; }
    #composer { border-radius: 14px; padding: 8px 8px 8px 14px; }
    /* 16px, not the body's 15px: iOS Safari auto-zooms the page on focusing
       any input under 16px, which is jarring in a chat composer. */
    #input { font-size: 16px; }
    #hint { font-size: 10px; padding: 0 6px; }
    #panel { width: 94vw; max-height: 85vh; }
    #panelHeader { padding: 16px 16px 12px 16px; }
    #panelBody { padding: 8px 16px 20px 16px; }
  }
</style>
</head>
<body>
<div id="topbar">
  <div class="brand">
    <span class="dot"></span> llama-shell
    <span class="meta">model: <span class="mono">__MODEL__</span> · cwd: <span class="mono">__CWD__</span></span>
  </div>
  <span id="badges"></span>
  <div class="icons">
    <a class="iconbtn gh-link" href="https://github.com/affigabmag/llama-shell" target="_blank" rel="noopener" style="text-decoration:none">GitHub</a>
    <span class="iconbtn" id="toolsBtn">🛠 Tools</span>
    <span class="iconbtn" id="helpBtn">❓ Help</span>
    <span class="iconbtn" id="menuBtn" aria-label="Menu">☰</span>
  </div>
</div>
<div id="menuDropdown">
  <a class="menuItem gh-link" href="https://github.com/affigabmag/llama-shell" target="_blank" rel="noopener">🐙 GitHub</a>
  <span class="menuItem" id="toolsBtnMobile">🛠 Tools</span>
  <span class="menuItem" id="helpBtnMobile">❓ Help</span>
  <span class="menuItem" id="logsBtnMobile">📜 Logs</span>
</div>
<div id="chatWrap"><div id="chat"></div></div>
<div id="composerWrap">
  <div id="composer">
    <textarea id="input" rows="1" autofocus placeholder="Message llama-shell..."></textarea>
    <button id="send">➤</button>
  </div>
  <div id="hint">Enter to send · Shift+Enter for a new line · tools run on the host machine, not your browser</div>
</div>
<div id="overlay">
  <div id="panel">
    <div id="panelHeader"><span id="panelClose">✕</span></div>
    <div id="panelBody"></div>
  </div>
</div>
<script>
const token = new URLSearchParams(location.search).get('token') || '';
let messages = [];
let warmupTimer = null;
const chat = document.getElementById('chat');
const overlay = document.getElementById('overlay');
const panelBody = document.getElementById('panelBody');
const input = document.getElementById('input');
const sendBtn = document.getElementById('send');

function esc(s) { return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }

// Minimal markdown: fenced code blocks, inline code, bold, links, bare URLs,
// paragraphs. Escapes first, then injects tags around already-safe text.
const BT = String.fromCharCode(96); // backtick — can't appear literally in a Go raw string
function renderMarkdown(raw) {
  let s = esc(raw);
  s = s.replace(new RegExp(BT+BT+BT+'([a-zA-Z0-9]*)\\n([\\s\\S]*?)'+BT+BT+BT, 'g'), (m, lang, code) => '<pre><code>' + code + '</code></pre>');
  s = s.replace(new RegExp(BT+'([^'+BT+'\\n]+)'+BT, 'g'), '<code>$1</code>');
  // Collapse "**Headline**\n[url](url)" (bold title followed by a
  // redundant link whose visible label IS the url) into one real link —
  // a small model reliably does this instead of "[Headline](url)" no
  // matter how the prompt asks, so handle it here rather than keep
  // fighting it with more prompt tuning.
  s = s.replace(/\*\*([^*\n]+)\*\*\s*\n+\s*\[(https?:\/\/[^\]]+)\]\(\2\)/g,
    '<strong><a href="$2" target="_blank" rel="noopener">$1</a></strong>');
  s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
  s = s.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');
  // Autolink bare URLs, but skip ones already inside an href="..." from the pass above.
  const parts = s.split(/(<a [^>]*>.*?<\/a>)/g);
  s = parts.map((part, i) => {
    if (i % 2 === 1) return part; // already an anchor tag, leave as-is
    return part.replace(/https?:\/\/[^\s<)]+/g, u => '<a href="' + u + '" target="_blank" rel="noopener">' + u + '</a>');
  }).join('');
  const paras = s.split(/\n\n+/).map(p => '<p>' + p.replace(/\n/g, '<br>') + '</p>');
  return paras.join('');
}

// Groups the flat message list into display items: user turns, assistant
// text, and tool-call "steps" (each paired with its result message) shown
// as a collapsed trace — the same "progress steps" pattern real agent UIs
// use, so tool activity is inspectable without cluttering the main thread.
function buildDisplayItems(msgs) {
  const items = [];
  for (let i = 0; i < msgs.length; i++) {
    const m = msgs[i];
    if (m.role === 'system' || m.role === 'tool') continue;
    if (m.role === 'user') { items.push({ type: 'user', content: m.content }); continue; }
    if (m.role === 'assistant') {
      if (m.tool_calls && m.tool_calls.length) {
        let j = i + 1;
        const steps = m.tool_calls.map(tc => {
          const result = (msgs[j] && msgs[j].role === 'tool') ? msgs[j].content : null;
          j++;
          return { name: tc.function.name, args: tc.function.arguments, result };
        });
        items.push({ type: 'steps', steps });
      }
      if (m.content && m.content.trim() !== '') {
        items.push({ type: 'assistant', content: m.content });
      }
    }
  }
  return items;
}

function renderStep(step) {
  const argsStr = JSON.stringify(step.args || {});
  return '<details class="step">' +
    '<summary><span class="toolname">' + esc(step.name) + '</span><span class="mono">(' + esc(argsStr) + ')</span></summary>' +
    '<div class="stepbody"><pre class="mono">' + esc(step.result == null ? '(no result yet)' : step.result) + '</pre></div>' +
    '</details>';
}

function render() {
  const items = buildDisplayItems(messages);
  let replyIdx = 0;
  chat.innerHTML = items.map(it => {
    if (it.type === 'user') {
      return '<div class="msg user"><div class="role">You</div><div class="content">' + renderMarkdown(it.content) + '</div></div>';
    }
    if (it.type === 'steps') {
      return '<div class="msg assistant"><div class="role">Assistant</div><div class="content">' +
        it.steps.map(renderStep).join('') + '</div></div>';
    }
    const isErr = it.content.startsWith('(error)');
    const secs = turnElapsed[replyIdx++];
    const timing = (secs != null) ? '<div class="replyTime">' + formatElapsed(secs) + '</div>' : '';
    return '<div class="msg assistant' + (isErr ? ' error' : '') + '"><div class="role">Assistant</div><div class="content">' +
      renderMarkdown(it.content) + '</div>' + timing + '</div>';
  }).join('');
  window.scrollTo(0, document.body.scrollHeight);
}

const panelHeader = document.getElementById('panelHeader');
function closeOverlay() { overlay.classList.remove('open'); }
document.getElementById('panelClose').onclick = closeOverlay;
overlay.addEventListener('click', e => { if (e.target === overlay) closeOverlay(); });
function setPanelHeader(html) {
  panelHeader.innerHTML = '<span id="panelClose">✕</span>' + html;
  document.getElementById('panelClose').onclick = closeOverlay;
}

const COPY_SVG = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" ' +
  'stroke-width="2" stroke-linecap="round" stroke-linejoin="round">' +
  '<rect x="9" y="9" width="13" height="13" rx="2"></rect>' +
  '<path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>';

async function openTools() {
  setPanelHeader('<h2>Tools</h2><p>Loading…</p>');
  panelBody.innerHTML = '';
  overlay.classList.add('open');
  try {
    const resp = await fetch('/api/tools?token=' + encodeURIComponent(token));
    const tools = await resp.json();
    setPanelHeader('<h2>Tools (' + tools.length + ')</h2>' +
      '<input id="toolSearch" placeholder="Search tools by name or description..." />' +
      '<div class="toolsHintRow"><p>Click a row to fill the input with an example prompt, or the copy icon to copy it '+
      'without filling the box.</p>' +
      '<div class="toolsCollapseBtns"><span class="miniBtn" id="collapseAllBtn">Collapse all</span>' +
      '<span class="miniBtn" id="expandAllBtn">Expand all</span></div></div>');
    document.getElementById('collapseAllBtn').onclick = () =>
      panelBody.querySelectorAll('.catGroup').forEach(d => { d.open = false; });
    document.getElementById('expandAllBtn').onclick = () =>
      panelBody.querySelectorAll('.catGroup').forEach(d => { d.open = true; });
    let html = '';
    let lastCat = null;
    let num = 0;
    let catCount = 0;
    tools.forEach(t => {
      if (t.category !== lastCat) {
        if (lastCat !== null) html += '</div></details>';
        catCount++;
        html += '<details class="catGroup"><summary class="catHeading">' + esc(t.category) + '</summary><div class="catBody">';
        lastCat = t.category;
      }
      num++;
      const exHtml = (t.examples || []).map(e =>
        '<div class="ex"><span class="copyText">"' + esc(e) + '"</span>' +
        '<span class="copyIcon" data-copy="' + esc(e) + '" title="Copy">' + COPY_SVG + '</span></div>'
      ).join('');
      const searchHay = (t.name + ' ' + t.description + ' ' + (t.examples || []).join(' ')).toLowerCase();
      html += '<div class="toolRow" data-example="' + esc((t.examples || [])[0] || '') + '" ' +
        'data-search="' + esc(searchHay) + '">' +
        '<div class="num">' + num + '.</div>' +
        '<div class="body"><div class="name">' + esc(t.name) + '</div>' +
        '<div class="desc">' + esc(t.description) + '</div>' + exHtml + '</div></div>';
    });
    if (catCount > 0) html += '</div></details>';
    panelBody.innerHTML = html;
    panelBody.querySelectorAll('.copyIcon').forEach(icon => {
      icon.onclick = (e) => {
        e.stopPropagation();
        const text = icon.getAttribute('data-copy');
        navigator.clipboard.writeText(text).then(() => {
          icon.classList.add('copied');
          setTimeout(() => icon.classList.remove('copied'), 1200);
        }).catch(() => {});
      };
    });
    panelBody.querySelectorAll('.toolRow').forEach(row => {
      row.onclick = () => {
        const ex = row.getAttribute('data-example');
        if (ex) { input.value = ex; input.dispatchEvent(new Event('input')); }
        closeOverlay();
        input.focus();
      };
    });
    const groups = panelBody.querySelectorAll('.catGroup');
    document.getElementById('toolSearch').addEventListener('input', (e) => {
      const q = e.target.value.trim().toLowerCase();
      groups.forEach(group => {
        let anyVisible = false;
        group.querySelectorAll('.toolRow').forEach(row => {
          const match = !q || row.getAttribute('data-search').includes(q);
          row.style.display = match ? '' : 'none';
          if (match) anyVisible = true;
        });
        group.style.display = anyVisible ? '' : 'none';
        group.open = q !== '' && anyVisible;
      });
    });
  } catch (e) {
    setPanelHeader('<h2>Tools</h2>');
    panelBody.innerHTML = '<p style="color:#ff8080">failed to load: ' + esc(String(e)) + '</p>';
  }
}
function openHelp() {
  setPanelHeader('<h2>Help</h2>');
  panelBody.innerHTML =
    '<p>This is the same agentic chat as llama-shell\'s terminal UI, running in a browser.</p>' +
    '<p><b>Enter</b> sends your message, <b>Shift+Enter</b> adds a new line.<br>' +
    '<b>🛠 Tools</b> browses every tool available to the model, grouped by category — click a row for an ' +
    'example prompt, or its copy icon to copy the prompt text.<br>' +
    'Each assistant turn shows any tool calls as a collapsed step you can expand to inspect.</p>' +
    '<p style="color:var(--text-dim)">All tool calls (file read/write, commands, web, etc.) run on the ' +
    'machine hosting this server, not your browser.</p>';
  overlay.classList.add('open');
}
document.getElementById('toolsBtn').onclick = openTools;
document.getElementById('helpBtn').onclick = openHelp;

// Fetches from the server fresh every open (and on each search keystroke)
// rather than once — the log keeps growing while the panel might sit open,
// and re-fetching is cheap since the server already caps it at 1000 lines.
async function openLogs() {
  setPanelHeader('<h2>Logs</h2><input id="logSearch" placeholder="Search logs..." />');
  panelBody.innerHTML = '<p>Loading…</p>';
  overlay.classList.add('open');
  const searchInput = document.getElementById('logSearch');
  let debounceTimer = null;
  async function loadLogs(q) {
    try {
      const resp = await fetch('/api/logs?token=' + encodeURIComponent(token) +
        (q ? '&q=' + encodeURIComponent(q) : ''));
      const data = await resp.json();
      panelBody.innerHTML = data.lines.length
        ? '<pre class="mono logLines">' + data.lines.map(esc).join('\n') + '</pre>'
        : '<p>No matching log entries.</p>';
    } catch (e) {
      panelBody.innerHTML = '<p style="color:#f87171">Failed to load logs: ' + esc(String(e)) + '</p>';
    }
  }
  searchInput.oninput = () => {
    clearTimeout(debounceTimer);
    debounceTimer = setTimeout(() => loadLogs(searchInput.value), 250);
  };
  loadLogs('');
  searchInput.focus();
}

const menuBtn = document.getElementById('menuBtn');
const menuDropdown = document.getElementById('menuDropdown');
menuBtn.onclick = (e) => { e.stopPropagation(); menuDropdown.classList.toggle('open'); };
document.getElementById('toolsBtnMobile').onclick = () => { menuDropdown.classList.remove('open'); openTools(); };
document.getElementById('helpBtnMobile').onclick = () => { menuDropdown.classList.remove('open'); openHelp(); };
document.getElementById('logsBtnMobile').onclick = () => { menuDropdown.classList.remove('open'); openLogs(); };
document.addEventListener('click', (e) => {
  if (!menuDropdown.contains(e.target) && e.target !== menuBtn) menuDropdown.classList.remove('open');
});

async function loadStatusBadges() {
  const el = document.getElementById('badges');
  try {
    const resp = await fetch('/api/status?token=' + encodeURIComponent(token));
    const s = await resp.json();
    const badge = (cls, text) => '<span class="badge ' + cls + '">' + esc(text) + '</span>';
    let html = '';
    if (s.buildVersion) html += badge('off', 'build: ' + s.buildVersion);
    html += s.ollamaInstalled ? badge('on', 'ollama: ' + (s.ollamaVersion || 'installed')) : badge('warn', 'ollama: not installed');
    html += s.tavilyConfigured ? badge('on', 'tavily: set') : badge('off', 'tavily: off');
    if (s.telegramBound) html += badge('on', 'tg: bound');
    else if (s.telegramRunning) html += badge('warn', 'tg: running, not bound');
    else html += badge('off', 'tg: off');
    el.innerHTML = html;
  } catch (e) { /* leave badges as they were */ }
}
loadStatusBadges();
setInterval(loadStatusBadges, 15000);

let warmupLoaded = true;
async function pollWarmup() {
  try {
    const resp = await fetch('/api/warmup?token=' + encodeURIComponent(token));
    const data = await resp.json();
    warmupLoaded = !!data.loaded;
  } catch (e) { /* keep last known state */ }
}

// Mirrors the TUI's agentThinkingPhrase ladder: elapsed time reads as
// progress toward a finish instead of one static, seemingly-stuck label.
function thinkingPhrase(elapsedMs) {
  if (!warmupLoaded) return 'Loading model into memory';
  const s = elapsedMs / 1000;
  const secs = Math.floor(s) + 's';
  if (s < 4) return 'Thinking (' + secs + ')';
  if (s < 8) return 'Reasoning (' + secs + ')';
  if (s < 14) return 'Working through it (' + secs + ')';
  if (s < 20) return 'Almost done (' + secs + ')';
  const phrases = ['Still going', 'Taking a while', 'Almost done'];
  return phrases[Math.floor((s - 20) / 6) % phrases.length] + ' (' + secs + ')';
}

function autoGrow() {
  input.style.height = 'auto';
  input.style.height = Math.min(input.scrollHeight, 200) + 'px';
}
input.addEventListener('input', autoGrow);

let busy = false;
let currentController = null;
// One entry per completed turn's elapsed seconds, in order — matched up to
// the Nth content-bearing assistant reply at render time. Kept separate
// from the messages array itself so this display-only text never gets
// sent back to the model as part of the conversation history.
let turnElapsed = [];
function setBusy(b) {
  busy = b;
  sendBtn.textContent = b ? '■' : '➤';
  sendBtn.title = b ? 'Stop' : 'Send';
  sendBtn.classList.toggle('stop', b);
}

async function send() {
  const text = input.value.trim();
  if (!text) return;
  input.value = '';
  autoGrow();
  setBusy(true);
  currentController = new AbortController();
  messages.push({ role: 'user', content: text });
  render();
  const thinkingRow = document.createElement('div');
  thinkingRow.className = 'thinking';
  thinkingRow.innerHTML = '<span class="dots"><span></span><span></span><span></span></span><span id="thinkingLabel">Thinking</span>';
  chat.appendChild(thinkingRow);
  window.scrollTo(0, document.body.scrollHeight);
  const label = document.getElementById('thinkingLabel');
  const startedAt = Date.now();
  warmupLoaded = true;
  pollWarmup();
  const warmupPollTimer = setInterval(pollWarmup, 1500);
  warmupTimer = setInterval(() => {
    if (label) label.textContent = thinkingPhrase(Date.now() - startedAt);
  }, 1000);
  try {
    const resp = await fetch('/api/chat?token=' + encodeURIComponent(token), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ messages: messages }),
      signal: currentController.signal,
    });
    const data = await resp.json();
    if (data.error) {
      messages.push({ role: 'assistant', content: '(error) ' + data.error });
    } else {
      messages = data.messages;
    }
  } catch (e) {
    if (e.name === 'AbortError') {
      messages.push({ role: 'assistant', content: '(stopped)' });
    } else {
      messages.push({ role: 'assistant', content: '(error) ' + e });
    }
  }
  turnElapsed.push(Math.round((Date.now() - startedAt) / 1000));
  clearInterval(warmupTimer);
  clearInterval(warmupPollTimer);
  setBusy(false);
  currentController = null;
  render();
  input.focus();
}

// Formats seconds as "56s", or "1m 04s" past a minute.
function formatElapsed(totalSec) {
  if (totalSec < 60) return totalSec + 's';
  const m = Math.floor(totalSec / 60);
  const s = totalSec % 60;
  return m + 'm ' + String(s).padStart(2, '0') + 's';
}
sendBtn.onclick = () => {
  if (busy) {
    if (currentController) currentController.abort();
    return;
  }
  send();
};
input.addEventListener('keydown', e => {
  if (e.key === 'Enter' && !e.shiftKey && !busy) { e.preventDefault(); send(); }
});
</script>
</body>
</html>`

// listInstalledModelNames returns the NAME column of `ollama list` — the
// set of models actually available locally, so the web server settings
// screen can offer a real choice instead of guessing.
func listInstalledModelNames() ([]string, error) {
	out, err := exec.Command("ollama", "list").CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s", msg)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	var names []string
	for i, line := range lines {
		if i == 0 {
			continue // header
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		names = append(names, fields[0])
	}
	return names, nil
}

// pickDefaultModelIndex picks the web server's default model selection:
// whatever was configured last time if it's still installed, else
// gemma4:e2b (this app's recommended default), else the other two
// wizard-known small models, else just the first installed model.
func pickDefaultModelIndex(names []string, preferred string) int {
	priority := []string{preferred, "gemma4:e2b", "gemma2:2b", "qwen2.5:1.5b"}
	for _, p := range priority {
		if p == "" {
			continue
		}
		for i, n := range names {
			if n == p {
				return i
			}
		}
	}
	return 0
}

type webServerPullDoneMsg struct {
	err string
}

// pullModelForWebServer downloads gemma4:e2b (this app's default) when
// the web server is being enabled with no local model installed at all.
// No live progress bar here (unlike the setup wizard's pull) — this is a
// one-off fallback path, not worth duplicating that machinery for.
func pullModelForWebServer(name string) tea.Cmd {
	return func() tea.Msg {
		out, err := exec.Command("ollama", "pull", name).CombinedOutput()
		if err != nil {
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				msg = err.Error()
			}
			return webServerPullDoneMsg{err: msg}
		}
		return webServerPullDoneMsg{}
	}
}

func webHandleChatPage(w http.ResponseWriter, r *http.Request) {
	webServerMu.Lock()
	modelName, workDir := webServerModel, webServerWorkDir
	webServerMu.Unlock()
	page := strings.ReplaceAll(webChatPageHTML, "__MODEL__", modelName)
	page = strings.ReplaceAll(page, "__CWD__", workDir)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}

type webChatRequest struct {
	Messages []ollamaChatMsg `json:"messages"`
}

type webChatResponse struct {
	Messages []ollamaChatMsg `json:"messages,omitempty"`
	Error    string          `json:"error,omitempty"`
}

func webHandleChatAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req webChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(webChatResponse{Error: "bad request: " + err.Error()})
		return
	}
	webServerMu.Lock()
	modelName, workDir := webServerModel, webServerWorkDir
	webServerMu.Unlock()

	history := req.Messages
	if len(history) == 0 || history[0].Role != "system" {
		history = append([]ollamaChatMsg{{Role: "system", Content: agentSystemPrompt(workDir)}}, history...)
	}
	if len(history) > 0 && history[len(history)-1].Role == "user" {
		appendLog("web chat message: %s", truncateName(history[len(history)-1].Content, 80))
	}
	updated, err := runAgentTurnSync(modelName, history, workDir, true)
	if err != nil {
		appendLog("web chat: turn error: %s", err.Error())
		json.NewEncoder(w).Encode(webChatResponse{Error: err.Error(), Messages: updated})
		return
	}
	json.NewEncoder(w).Encode(webChatResponse{Messages: updated})
}

type webToolInfo struct {
	Name        string   `json:"name"`
	Category    string   `json:"category"`
	Description string   `json:"description"`
	Examples    []string `json:"examples"`
}

// webHandleTools lists every tool available to the model, grouped by the
// same categories as the TUI's Alt+T tool browser (agentToolCategories),
// with descriptions and example prompts.
func webHandleTools(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var out []webToolInfo
	for _, cat := range agentToolCategories {
		for _, name := range cat.tools {
			info := webToolInfo{Name: name, Category: cat.name, Description: toolDescription(name)}
			if ex, ok := toolExamples[name]; ok {
				info.Examples = []string{ex[0], ex[1]}
			}
			out = append(out, info)
		}
	}
	json.NewEncoder(w).Encode(out)
}

type webWarmupResponse struct {
	Loaded bool `json:"loaded"`
}

// webHandleWarmup lets the page distinguish "the model is still loading
// into memory" from "the model is actually thinking about my message" —
// the same distinction the TUI shows via `ollama ps` polling — instead of
// a single generic "thinking..." that's misleading during a cold load.
func webHandleWarmup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	webServerMu.Lock()
	modelName := webServerModel
	webServerMu.Unlock()
	out, err := exec.Command("ollama", "ps").CombinedOutput()
	if err != nil {
		json.NewEncoder(w).Encode(webWarmupResponse{Loaded: false})
		return
	}
	json.NewEncoder(w).Encode(webWarmupResponse{Loaded: strings.Contains(string(out), modelName)})
}

type webStatusResponse struct {
	TavilyConfigured bool   `json:"tavilyConfigured"`
	TelegramRunning  bool   `json:"telegramRunning"`
	TelegramBound    bool   `json:"telegramBound"`
	OllamaInstalled  bool   `json:"ollamaInstalled"`
	OllamaVersion    string `json:"ollamaVersion"`
	BuildVersion     string `json:"buildVersion"`
}

// webHandleStatus surfaces the same integration state the TUI's footer
// shows (ollama installed? Tavily key set? Telegram bot running/bound?),
// so the web UI isn't blind to state that only has a screen in the
// terminal app.
func webHandleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	tgCfg := loadTelegramConfig()
	ollama := checkOllama()
	json.NewEncoder(w).Encode(webStatusResponse{
		TavilyConfigured: os.Getenv("TAVILY_API_KEY") != "",
		TelegramRunning:  isTelegramRunning(),
		TelegramBound:    isTelegramRunning() && tgCfg.ChatID != 0,
		OllamaInstalled:  ollama.installed,
		OllamaVersion:    ollama.version,
		BuildVersion:     buildTime,
	})
}

// webHandleLogs serves the same activity log the TUI's own log viewer
// shows (last maxDisplayedLogLines events, newest first), optionally
// filtered via ?q= — the same substring-match search the TUI uses.
func webHandleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	lines := readLogLines(r.URL.Query().Get("q"))
	if lines == nil {
		lines = []string{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"lines": lines})
}

// telegramConfig persists across restarts: the bot token, which model
// answers, and the chat this bot is bound to (0 = not yet bound — it
// auto-binds to whichever chat messages it first, see runTelegramPollLoop).
type telegramConfig struct {
	Token  string `json:"token"`
	Model  string `json:"model"`
	ChatID int64  `json:"chat_id"`
}

func telegramConfigPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "llama-shell", "telegram_config.json")
}

func loadTelegramConfig() telegramConfig {
	data, err := os.ReadFile(telegramConfigPath())
	if err != nil {
		return telegramConfig{}
	}
	var cfg telegramConfig
	_ = json.Unmarshal(data, &cfg)
	return cfg
}

func saveTelegramConfig(cfg telegramConfig) error {
	path := telegramConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// emailConfig persists Gmail SMTP credentials — App Password auth (not
// the account's real password; Gmail rejects plain-password SMTP login
// entirely once 2FA is on, and recommends App Passwords even without it).
// Host/port are fixed to Gmail's real submission endpoint, not user
// input — there's only one correct value, so asking for it would just be
// one more thing to get wrong.
type emailConfig struct {
	FromAddress string `json:"from_address"`
	AppPassword string `json:"app_password"`
	DisplayName string `json:"display_name,omitempty"`
}

const (
	emailSMTPHost = "smtp.gmail.com"
	emailSMTPPort = "587"
)

func emailConfigPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "llama-shell", "email_config.json")
}

func loadEmailConfig() emailConfig {
	data, err := os.ReadFile(emailConfigPath())
	if err != nil {
		return emailConfig{}
	}
	var cfg emailConfig
	_ = json.Unmarshal(data, &cfg)
	return cfg
}

func saveEmailConfig(cfg emailConfig) error {
	path := emailConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// sendEmailViaGmail sends one plain-text email through Gmail's SMTP
// submission endpoint using the saved App Password. net/smtp's SendMail
// upgrades to STARTTLS automatically when the server offers it (Gmail
// always does on port 587), so no manual TLS handshake is needed here.
func sendEmailViaGmail(cfg emailConfig, to, subject, body string) error {
	if cfg.FromAddress == "" || cfg.AppPassword == "" {
		return fmt.Errorf("email isn't configured yet — set it up in help/settings first")
	}
	auth := smtp.PlainAuth("", cfg.FromAddress, cfg.AppPassword, emailSMTPHost)
	from := cfg.FromAddress
	if cfg.DisplayName != "" {
		from = fmt.Sprintf("%s <%s>", cfg.DisplayName, cfg.FromAddress)
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s\r\n", from, to, subject, body)
	return smtp.SendMail(emailSMTPHost+":"+emailSMTPPort, auth, cfg.FromAddress, []string{to}, []byte(msg))
}

type emailTestResultMsg struct {
	err string
}

func sendTestEmail(cfg emailConfig) tea.Cmd {
	return func() tea.Msg {
		err := sendEmailViaGmail(cfg, cfg.FromAddress,
			"llama-shell test email",
			"This confirms llama-shell can send email through your Gmail account. If you're reading this, it worked.")
		if err != nil {
			return emailTestResultMsg{err: err.Error()}
		}
		return emailTestResultMsg{}
	}
}

// backupMagic tags a llama-shell backup file so import can reject
// anything else (a random file, or one from a future incompatible
// format) with a clear error instead of a confusing decrypt failure.
const backupMagic = "LSB1"

const (
	backupSaltLen       = 16
	backupNonceLen      = 12
	backupKDFIterations = 200000
	backupKeyLen        = 32 // AES-256
	// backupFixedPassphrase is the sole key material behind every backup
	// file — there's no password prompt and no key file, by design (the
	// user just wants "not plain text if someone opens the file", not
	// real secrecy from someone who has the app's own source). Anyone
	// with this source can decrypt any backup; this is obfuscation
	// against casual viewing/editing, not a security boundary.
	backupFixedPassphrase = "llama-shell-backup-v1-not-a-secret"
)

// pbkdf2SHA256 derives a key of length keyLen from password+salt using
// PBKDF2 (RFC 8018) with HMAC-SHA256, iterated `iterations` times. Go's
// standard library has no PBKDF2 (only golang.org/x/crypto does), and
// this feature is meant to work with zero extra dependencies — the
// algorithm itself is a few lines of HMAC chaining, so it's inlined here
// rather than vendoring a library for it.
func pbkdf2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen
	var dk []byte
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		var blockIndex [4]byte
		binary.BigEndian.PutUint32(blockIndex[:], uint32(block))
		prf.Write(blockIndex[:])
		u := prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iterations; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

// llamaShellConfigDir is the one directory every persisted setting in
// this app lives under (email/telegram/tavily/webserver/autoupdate
// configs, city banner progress, the disclaimer marker, etc.) — backup
// export/import operates on this whole directory rather than listing
// each file by name, so a future new setting is included automatically.
func llamaShellConfigDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "llama-shell"), nil
}

// zipConfigDir walks llamaShellConfigDir() and returns it as an
// in-memory zip (archive/zip, already used elsewhere for the
// compress_zip tool) — the plaintext that gets encrypted for export.
func zipConfigDir() ([]byte, error) {
	dir, err := llamaShellConfigDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		w, err := zw.Create(e.Name())
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// unzipIntoConfigDir extracts a zip previously produced by
// zipConfigDir back into llamaShellConfigDir(), overwriting whatever is
// there — that's the point of a restore.
func unzipIntoConfigDir(zipData []byte) error {
	dir, err := llamaShellConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(f.Name)), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// encryptBackup derives a one-time key from the fixed passphrase plus a
// fresh random salt (PBKDF2) and seals plaintext with AES-256-GCM. The
// salt still varies per file (so two backups never share a key/nonce
// pair even though the passphrase is constant); the passphrase itself
// never varies, so there's nothing for the user to type or lose.
// Output layout: magic(4) | salt(16) | nonce(12) | ciphertext+tag.
func encryptBackup(plaintext []byte) ([]byte, error) {
	salt := make([]byte, backupSaltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	key := pbkdf2SHA256([]byte(backupFixedPassphrase), salt, backupKDFIterations, backupKeyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, backupNonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, len(backupMagic)+len(salt)+len(nonce)+len(ciphertext))
	out = append(out, []byte(backupMagic)...)
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ciphertext...)
	return out, nil
}

// decryptBackup reverses encryptBackup. gcm.Open's authentication check
// (GCM is AEAD) catches a corrupted/truncated/tampered-with file rather
// than silently returning garbage.
func decryptBackup(data []byte) ([]byte, error) {
	if len(data) < len(backupMagic)+backupSaltLen+backupNonceLen {
		return nil, fmt.Errorf("not a llama-shell backup file (too short)")
	}
	if string(data[:len(backupMagic)]) != backupMagic {
		return nil, fmt.Errorf("not a llama-shell backup file (bad header)")
	}
	rest := data[len(backupMagic):]
	salt := rest[:backupSaltLen]
	nonce := rest[backupSaltLen : backupSaltLen+backupNonceLen]
	ciphertext := rest[backupSaltLen+backupNonceLen:]
	key := pbkdf2SHA256([]byte(backupFixedPassphrase), salt, backupKDFIterations, backupKeyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("corrupted backup file")
	}
	return plaintext, nil
}

// defaultBackupPath is the pre-filled suggestion shown in the backup
// form — the user's home directory keeps it somewhere they'll actually
// find again, rather than the app's own cache dir (which is also what's
// being backed up, so writing there would be a little odd).
func defaultBackupPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, "llama-shell-backup.lsb")
}

// exportBackup zips the whole config directory, encrypts it, and
// writes it to path.
func exportBackup(path string) error {
	plain, err := zipConfigDir()
	if err != nil {
		return err
	}
	encrypted, err := encryptBackup(plain)
	if err != nil {
		return err
	}
	return os.WriteFile(path, encrypted, 0o600)
}

// importBackup reads path, decrypts it, and restores it over the
// current config directory.
func importBackup(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	plain, err := decryptBackup(data)
	if err != nil {
		return err
	}
	return unzipIntoConfigDir(plain)
}

// fileBrowseEntry is one row in the in-terminal directory browser used
// by the backup export/import screen — a plain text-mode "..[dir]/file"
// list navigated with the keyboard, not an OS GUI dialog.
type fileBrowseEntry struct {
	name  string
	isDir bool
}

// listDirEntries lists dir for the backup file browser: ".." first
// (unless dir is already a filesystem root), then subdirectories, then
// files — each group alphabetical. Files are filtered to *.lsb (the
// only thing this browser is ever used to pick) so a folder full of
// unrelated files doesn't bury the one that matters; directories are
// never filtered, since you need to see all of them to navigate.
func listDirEntries(dir string) ([]fileBrowseEntry, error) {
	raw, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var dirs, files []fileBrowseEntry
	for _, e := range raw {
		if e.IsDir() {
			dirs = append(dirs, fileBrowseEntry{name: e.Name(), isDir: true})
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".lsb") {
			files = append(files, fileBrowseEntry{name: e.Name(), isDir: false})
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i].name) < strings.ToLower(dirs[j].name) })
	sort.Slice(files, func(i, j int) bool { return strings.ToLower(files[i].name) < strings.ToLower(files[j].name) })

	entries := make([]fileBrowseEntry, 0, len(dirs)+len(files)+1)
	if parent := filepath.Dir(dir); parent != dir {
		entries = append(entries, fileBrowseEntry{name: "..", isDir: true})
	}
	entries = append(entries, dirs...)
	entries = append(entries, files...)
	return entries, nil
}

// enterBackupBrowser switches into the in-terminal directory browser,
// starting in the same folder defaultBackupPath() suggests (the user's
// home directory). mode is "export" or "import".
func (m model) enterBackupBrowser(mode string) model {
	m.backupMode = mode
	m.backupMsg = ""
	m.backupBrowseDir = filepath.Dir(defaultBackupPath())
	m.backupBrowseCursor = 0
	m.backupBrowseFilename = filepath.Base(defaultBackupPath())
	m.backupBrowseEditingName = mode == "export"
	entries, err := listDirEntries(m.backupBrowseDir)
	if err != nil {
		m.backupMsg = redStyle.Render("can't open " + m.backupBrowseDir + ": " + err.Error())
		m.view = viewBackupSettings
		return m
	}
	m.backupBrowseEntries = entries
	m.view = viewBackupBrowser
	return m
}

// confirmBackupExport runs when Enter is pressed in the filename field
// during export — joins the current browsed directory with the typed
// filename and writes the encrypted backup there.
func (m model) confirmBackupExport() (model, tea.Cmd) {
	name := strings.TrimSpace(m.backupBrowseFilename)
	if name == "" {
		m.backupMsg = "enter a file name"
		return m, nil
	}
	if !strings.EqualFold(filepath.Ext(name), ".lsb") {
		name += ".lsb"
	}
	path := filepath.Join(m.backupBrowseDir, name)
	if err := exportBackup(path); err != nil {
		m.backupMsg = redStyle.Render("export failed: " + err.Error())
		appendLog("backup: export failed: %s", err.Error())
	} else {
		m.backupMsg = helpKeyStyle.Render("exported to " + path)
		appendLog("backup: exported settings to %s", path)
	}
	m.view = viewBackupSettings
	return m, nil
}

// confirmBackupImport runs when a .lsb file is selected in import
// mode — decrypts and restores it over the current config directory.
func (m model) confirmBackupImport(path string) (model, tea.Cmd) {
	if err := importBackup(path); err != nil {
		m.backupMsg = redStyle.Render("import failed: " + err.Error())
		appendLog("backup: import failed: %s", err.Error())
	} else {
		m.backupMsg = helpKeyStyle.Render("imported from " + path + " — restart llama-shell for all of it to take effect")
		appendLog("backup: imported settings from %s", path)
	}
	m.view = viewBackupSettings
	return m, nil
}

var (
	telegramMu      sync.Mutex
	telegramCancel  context.CancelFunc
	telegramRunning bool
	telegramLastErr string
)

func isTelegramRunning() bool {
	telegramMu.Lock()
	defer telegramMu.Unlock()
	return telegramRunning
}

// startTelegramBot launches (or is a no-op if already running) the
// long-polling loop against Telegram's getUpdates — no incoming webhook,
// no public URL, nothing reachable from outside this machine, unlike the
// web server. That's the whole reason Telegram was chosen over WhatsApp.
func startTelegramBot(cfg telegramConfig, workDir string) {
	telegramMu.Lock()
	if telegramRunning {
		telegramMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	telegramCancel = cancel
	telegramRunning = true
	telegramLastErr = ""
	telegramMu.Unlock()
	go runTelegramPollLoop(ctx, cfg, workDir)
	appendLog("telegram bot started (model %s)", cfg.Model)
}

func stopTelegramBot() {
	telegramMu.Lock()
	cancel := telegramCancel
	telegramRunning = false
	telegramMu.Unlock()
	if cancel != nil {
		cancel()
	}
	appendLog("telegram bot stopped")
}

type tgChat struct {
	ID int64 `json:"id"`
}

type tgMessage struct {
	Chat tgChat `json:"chat"`
	Text string `json:"text"`
}

type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

type tgGetUpdatesResp struct {
	OK     bool       `json:"ok"`
	Result []tgUpdate `json:"result"`
}

// telegramGetUpdates long-polls up to 30s for new messages — Telegram
// holds the connection open server-side and returns early the moment a
// message arrives, so this isn't a tight busy-loop despite running
// continuously.
func telegramGetUpdates(token string, offset int64) ([]tgUpdate, error) {
	client := &http.Client{Timeout: 35 * time.Second}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30", token, offset)
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("telegram getUpdates returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var parsed tgGetUpdatesResp
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, err
	}
	if !parsed.OK {
		return nil, fmt.Errorf("telegram getUpdates: not ok")
	}
	return parsed.Result, nil
}

func telegramSendMessage(token string, chatID int64, text string) error {
	body, _ := json.Marshal(map[string]interface{}{"chat_id": chatID, "text": text})
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Post(fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token), "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram sendMessage returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

// telegramSendChatAction shows Telegram's native "typing..." indicator.
// It only lasts ~5s per call, so the caller needs to re-send it on a
// ticker for as long as it's actually working.
func telegramSendChatAction(token string, chatID int64) error {
	body, _ := json.Marshal(map[string]interface{}{"chat_id": chatID, "action": "typing"})
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(fmt.Sprintf("https://api.telegram.org/bot%s/sendChatAction", token), "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram sendChatAction returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

// telegramIncomingMsg is one user text message handed from the fetcher
// goroutine to the processing loop in runTelegramPollLoop.
type telegramIncomingMsg struct {
	chatID int64
	text   string
}

// runTelegramPollLoop is the bot's whole lifetime: poll, answer, repeat.
// It auto-binds to the first chat that ever messages it (cfg.ChatID == 0)
// and persists that binding, then rejects any other chat ID from then on
// — otherwise anyone who discovers the bot's @username would get the same
// full local tool access (files, commands, network) you have here.
func runTelegramPollLoop(ctx context.Context, cfg telegramConfig, workDir string) {
	history := []ollamaChatMsg{{Role: "system", Content: agentSystemPrompt(workDir)}}
	var boundChatID atomic.Int64
	boundChatID.Store(cfg.ChatID)
	var busy atomic.Bool
	var busyChatID atomic.Int64
	incoming := make(chan telegramIncomingMsg, 32)
	// Captured once as its own immutable value — the fetcher goroutine
	// must never touch the shared `cfg` variable itself, since the main
	// loop below mutates cfg.ChatID concurrently; even touching a
	// different field of the same struct from two goroutines is a real
	// data race, not just a logical one.
	token := cfg.Token

	// Fetcher runs independently of how long a turn takes to process —
	// without this, a message sent while the bot is mid-turn would just
	// sit unnoticed until the NEXT getUpdates call (after the current
	// turn finishes), with no acknowledgment that it was even received.
	// This lets it reply immediately with "please wait" for a message
	// that arrives while busy, then still queue it for real processing.
	go func() {
		var offset int64
		consecutiveErrs := 0
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			updates, err := telegramGetUpdates(token, offset)
			if err != nil {
				consecutiveErrs++
				telegramMu.Lock()
				telegramLastErr = err.Error()
				telegramMu.Unlock()
				appendLog("telegram: getUpdates error: %s", err.Error())
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Duration(min(consecutiveErrs, 6)) * 5 * time.Second):
				}
				continue
			}
			consecutiveErrs = 0
			for _, u := range updates {
				offset = u.UpdateID + 1
				if u.Message == nil || strings.TrimSpace(u.Message.Text) == "" {
					continue
				}
				chatID := u.Message.Chat.ID
				if bound := boundChatID.Load(); bound != 0 && chatID != bound {
					appendLog("telegram: rejected message from unbound chat %d", chatID)
					_ = telegramSendMessage(token, chatID, "This bot is bound to a different chat.")
					continue
				}
				if busy.Load() && chatID == busyChatID.Load() {
					_ = telegramSendMessage(token, chatID,
						"⏳ Still working on your previous message — please wait, I'll get to this one right after.")
				}
				select {
				case incoming <- telegramIncomingMsg{chatID: chatID, text: u.Message.Text}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	for {
		var msg telegramIncomingMsg
		select {
		case <-ctx.Done():
			return
		case msg = <-incoming:
		}
		chatID := msg.chatID
		if boundChatID.Load() == 0 {
			boundChatID.Store(chatID)
			cfg.ChatID = chatID
			_ = saveTelegramConfig(cfg)
			appendLog("telegram: bound to chat %d", chatID)
		}
		busy.Store(true)
		busyChatID.Store(chatID)
		history = append(history, ollamaChatMsg{Role: "user", Content: msg.text})
		appendLog("telegram message: %s", truncateName(msg.text, 80))

		// Instant ack so the chat doesn't look like a dead end while a
		// small model + tool calls can take anywhere from seconds to a
		// couple minutes — plus Telegram's native "typing..." indicator,
		// refreshed on a ticker since it only lasts ~5s per call.
		_ = telegramSendMessage(cfg.Token, chatID, "⏳ Got it — working on it...")
		turnStarted := time.Now()
		typingDone := make(chan struct{})
		go func() {
			_ = telegramSendChatAction(cfg.Token, chatID)
			typingTicker := time.NewTicker(4 * time.Second)
			defer typingTicker.Stop()
			// A separate, slower ticker for an actual text message —
			// the typing indicator alone disappears/gets missed easily
			// on the other end, so a visible "still working" note every
			// minute makes it obvious the bot hasn't gone silent on a
			// long tool-calling turn.
			statusTicker := time.NewTicker(1 * time.Minute)
			defer statusTicker.Stop()
			elapsedMin := 0
			for {
				select {
				case <-typingDone:
					return
				case <-typingTicker.C:
					_ = telegramSendChatAction(cfg.Token, chatID)
				case <-statusTicker.C:
					elapsedMin++
					_ = telegramSendMessage(cfg.Token, chatID,
						fmt.Sprintf("⏳ Still working on it... (%dm)", elapsedMin))
				}
			}
		}()

		updated, err := runAgentTurnSync(cfg.Model, history, workDir, true)
		close(typingDone)
		elapsed := formatElapsedDuration(time.Since(turnStarted))
		if err != nil {
			_ = telegramSendMessage(cfg.Token, chatID, "error: "+err.Error()+" ("+elapsed+")")
			busy.Store(false)
			continue
		}
		history = updated
		var reply string
		for _, m := range updated {
			if m.Role == "assistant" && strings.TrimSpace(m.Content) != "" {
				reply = m.Content
			}
		}
		if reply == "" {
			reply = "(no reply)"
		}
		reply = cleanMarkdownForDisplay(reply) + "\n\n⏱ " + elapsed
		if err := telegramSendMessage(cfg.Token, chatID, reply); err != nil {
			appendLog("telegram: sendMessage error: %s", err.Error())
		}
		busy.Store(false)
	}
}

// loadTavilyKey reads a previously saved key (if any) into this process's
// environment on startup, without overriding a key the user already set
// externally (e.g. via setx) for this session.
func loadTavilyKey() {
	if os.Getenv("TAVILY_API_KEY") != "" {
		return
	}
	data, err := os.ReadFile(tavilyKeyFilePath())
	if err != nil {
		return
	}
	if key := strings.TrimSpace(string(data)); key != "" {
		os.Setenv("TAVILY_API_KEY", key)
	}
}

func logFilePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "llama-shell", "activity.log")
}

// appendLog records a timestamped activity line (downloads, removes,
// kills, benchmark runs) for the in-app log viewer. Best-effort: logging
// never blocks or fails the action it's recording.
func appendLog(format string, args ...interface{}) {
	path := logFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	line := fmt.Sprintf(format, args...)
	fmt.Fprintf(f, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), line)
}

// maxDisplayedLogLines caps how much of the (unbounded, ever-growing) log
// file the log viewers show — the file on disk keeps everything, this only
// limits what gets loaded/rendered for a screen or the /api/logs response.
const maxDisplayedLogLines = 5000

// readLogLines returns up to maxDisplayedLogLines most recent log lines,
// newest first, optionally filtered to those containing query
// (case-insensitive substring match, applied after capping to the display
// window — matching "last 1000 events" first, then narrowing).
func readLogLines(query string) []string {
	data, err := os.ReadFile(logFilePath())
	if err != nil || len(data) == 0 {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > maxDisplayedLogLines {
		lines = lines[len(lines)-maxDisplayedLogLines:]
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return lines
	}
	q := strings.ToLower(query)
	filtered := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l), q) {
			filtered = append(filtered, l)
		}
	}
	return filtered
}

// readLogTail returns the last n lines of the activity log, or a
// placeholder if there's nothing yet.
func readLogTail(n int) string {
	lines := readLogLines("")
	if lines == nil {
		return "no log entries yet."
	}
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func cacheFilePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "llama-shell", "models_cache.json")
}

func loadCache() ([]modelRow, bool) {
	data, err := os.ReadFile(cacheFilePath())
	if err != nil {
		return nil, false
	}
	var rows []modelRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, false
	}
	return rows, true
}

func saveCache(rows []modelRow) error {
	path := cacheFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// fetchModelList runs `ollama list` and returns basic rows (name, size, modified).
func fetchModelList() tea.Cmd {
	return func() tea.Msg {
		out, err := exec.Command("ollama", "list").CombinedOutput()
		if err != nil {
			return scanStartMsg{err: strings.TrimSpace(string(out))}
		}
		lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
		var rows []modelRow
		for i, line := range lines {
			if i == 0 {
				continue // header
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			row := modelRow{Name: fields[0]}
			// NAME ID SIZE_NUM SIZE_UNIT MODIFIED...
			if len(fields) >= 4 {
				row.Size = fields[2] + " " + fields[3]
			}
			if len(fields) >= 5 {
				row.Modified = strings.Join(fields[4:], " ")
			}
			rows = append(rows, row)
		}
		return scanStartMsg{rows: rows}
	}
}

// scanOne runs `ollama show <name>` for a single model and parses key fields.
func scanOne(index int, row modelRow) tea.Cmd {
	return func() tea.Msg {
		out, _ := exec.Command("ollama", "show", row.Name).CombinedOutput()
		text := string(out)
		lower := strings.ToLower(text)
		row.Arch = extractField(lower, text, "architecture")
		row.Params = extractField(lower, text, "parameters")
		row.Quant = extractField(lower, text, "quantization")
		row.Context = extractField(lower, text, "context length")
		row.Capabilities = extractCapabilities(text)
		return scanStepMsg{index: index, row: row}
	}
}

// extractCapabilities pulls the "Capabilities" section from `ollama show`
// output — a header line followed by one capability per line (e.g.
// completion, vision, audio, tools, thinking) until the next blank line —
// and joins them into a compact comma list.
func extractCapabilities(text string) string {
	var caps []string
	capturing := false
	for _, l := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(l)
		if !capturing {
			if strings.EqualFold(trimmed, "Capabilities") {
				capturing = true
			}
			continue
		}
		if trimmed == "" {
			break
		}
		if len(trimmed) > 3 {
			trimmed = trimmed[:3]
		}
		caps = append(caps, trimmed)
	}
	if len(caps) == 0 {
		return "-"
	}
	return strings.Join(caps, ",")
}

// extractField finds a line like "  key   value" (case-insensitive key match)
// and returns the trimmed value portion.
func extractField(lowerText, origText, key string) string {
	lowerLines := strings.Split(lowerText, "\n")
	origLines := strings.Split(origText, "\n")
	for i, l := range lowerLines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, key) {
			rest := strings.TrimSpace(trimmed[len(key):])
			if rest != "" {
				// pull matching value from original-case line to preserve casing
				if i < len(origLines) {
					ot := strings.TrimSpace(origLines[i])
					if len(ot) > len(key) {
						v := strings.TrimSpace(ot[len(key):])
						if v != "" {
							return v
						}
					}
				}
				return rest
			}
		}
	}
	return "-"
}

type showRmMsg struct {
	name   string
	output string
	err    string
}

func removeModel(name string) tea.Cmd {
	return func() tea.Msg {
		out, err := exec.Command("ollama", "rm", name).CombinedOutput()
		if err != nil {
			return showRmMsg{name: name, output: string(out), err: err.Error()}
		}
		return showRmMsg{name: name, output: string(out)}
	}
}

type showStopMsg struct {
	name   string
	output string
	err    string
}

func stopModel(name string) tea.Cmd {
	return func() tea.Msg {
		out, err := exec.Command("ollama", "stop", name).CombinedOutput()
		if err != nil {
			return showStopMsg{name: name, output: string(out), err: err.Error()}
		}
		return showStopMsg{name: name, output: string(out)}
	}
}

type showRunDoneMsg struct {
	name string
	err  error
}

// runModelInteractive suspends the TUI, hands the terminal to `ollama run`
// so the user can chat with the model directly, then resumes the TUI.
func runModelInteractive(name string) tea.Cmd {
	appendLog("running %s interactively (ollama run)", name)
	c := exec.Command("ollama", "run", name)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return showRunDoneMsg{name: name, err: err}
	})
}

// catalogRow is one entry in the multi-source model catalog (the "list models" view).
type catalogRow struct {
	Name      string `json:"name"`
	Source    string `json:"source"`
	Size      string `json:"size"`
	Modified  string `json:"modified"`
	Installed bool   `json:"installed"`
	// Capabilities is only populated for locally installed ("ollama"
	// source) rows, via `ollama show` — library/huggingface entries
	// aren't installed, so there's nothing to introspect yet.
	Capabilities string `json:"capabilities"`
}

var catalogStages = []string{"ollama (local)", "ollama library (remote catalog)", "hugging face (top 50 GGUF by downloads)"}

type catalogStageMsg struct {
	stage int
	rows  []catalogRow
	err   string
}

// fetchCatalogStage runs the source lookup for a given stage index.
func fetchCatalogStage(stage int) tea.Cmd {
	switch stage {
	case 0:
		return fetchOllamaCatalog
	case 1:
		return fetchOllamaLibraryCatalog
	case 2:
		return fetchHuggingFaceCatalog
	}
	return func() tea.Msg { return catalogStageMsg{stage: stage} }
}

func fetchOllamaCatalog() tea.Msg {
	out, err := exec.Command("ollama", "list").CombinedOutput()
	if err != nil {
		return catalogStageMsg{stage: 0, err: "ollama list failed: " + strings.TrimSpace(string(out))}
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	var rows []catalogRow
	for i, line := range lines {
		if i == 0 {
			continue // header
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		row := catalogRow{Name: fields[0], Source: "ollama", Installed: true}
		if len(fields) >= 4 {
			row.Size = fields[2] + " " + fields[3]
		}
		if len(fields) >= 5 {
			row.Modified = strings.Join(fields[4:], " ")
		}
		if showOut, err := exec.Command("ollama", "show", row.Name).CombinedOutput(); err == nil {
			row.Capabilities = extractCapabilities(string(showOut))
		} else {
			row.Capabilities = "-"
		}
		rows = append(rows, row)
	}
	return catalogStageMsg{stage: 0, rows: rows}
}

var (
	libraryCardRe = regexp.MustCompile(`(?s)<li[^>]*>(.*?)</li>`)
	libraryLinkRe = regexp.MustCompile(`href="/library/([a-zA-Z0-9._-]+)"`)
	// parameter-size badges render with this distinct background color;
	// capability badges (tools, vision, embedding, ...) use a different one.
	librarySizeRe = regexp.MustCompile(`bg-\[#ddf4ff\][^>]*>\s*([a-zA-Z0-9.]+)\s*</span>`)
)

// fetchOllamaLibraryCatalog scrapes ollama.com/library for the full set of
// model families ollama can pull, not just what's installed locally. The
// index page doesn't expose download size, so the available parameter
// sizes (e.g. "2b, 8b") are shown instead as the closest size signal.
func fetchOllamaLibraryCatalog() tea.Msg {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://ollama.com/library")
	if err != nil {
		return catalogStageMsg{stage: 1, err: "ollama library request failed: " + err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return catalogStageMsg{stage: 1, err: err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return catalogStageMsg{stage: 1, err: fmt.Sprintf("ollama library returned status %d", resp.StatusCode)}
	}

	seen := map[string]bool{}
	var rows []catalogRow
	for _, card := range libraryCardRe.FindAllStringSubmatch(string(body), -1) {
		linkMatch := libraryLinkRe.FindStringSubmatch(card[1])
		if linkMatch == nil {
			continue
		}
		name := linkMatch[1]
		if seen[name] {
			continue
		}
		seen[name] = true

		sizes := librarySizeRe.FindAllStringSubmatch(card[1], -1)
		sizeStr := "-"
		if len(sizes) > 0 {
			parts := make([]string, len(sizes))
			for i, s := range sizes {
				parts[i] = s[1]
			}
			sizeStr = strings.Join(parts, ",")
		}

		rows = append(rows, catalogRow{Name: name, Source: "ollama-library", Size: sizeStr, Modified: "-"})
	}
	return catalogStageMsg{stage: 1, rows: rows}
}

func fetchHuggingFaceCatalog() tea.Msg {
	client := &http.Client{Timeout: 8 * time.Second}
	req, err := http.NewRequest("GET",
		"https://huggingface.co/api/models?filter=gguf&sort=downloads&direction=-1&limit=50", nil)
	if err != nil {
		return catalogStageMsg{stage: 2, err: err.Error()}
	}
	resp, err := client.Do(req)
	if err != nil {
		return catalogStageMsg{stage: 2, err: "hugging face request failed: " + err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return catalogStageMsg{stage: 2, err: err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return catalogStageMsg{stage: 2, err: fmt.Sprintf("hugging face returned status %d", resp.StatusCode)}
	}

	var items []struct {
		ID           string `json:"id"`
		LastModified string `json:"lastModified"`
	}
	if err := json.Unmarshal(body, &items); err != nil {
		return catalogStageMsg{stage: 2, err: "could not parse hugging face response: " + err.Error()}
	}

	rows := make([]catalogRow, 0, len(items))
	for _, it := range items {
		modified := it.LastModified
		if idx := strings.Index(modified, "T"); idx > 0 {
			modified = modified[:idx]
		}
		rows = append(rows, catalogRow{Name: it.ID, Source: "huggingface", Size: "-", Modified: modified})
	}
	return catalogStageMsg{stage: 2, rows: rows}
}

type psRow struct {
	Name    string
	Details string
}

type psFetchMsg struct {
	rows []psRow
	err  string
}

func fetchPs() tea.Msg {
	out, err := exec.Command("ollama", "ps").CombinedOutput()
	if err != nil {
		return psFetchMsg{err: strings.TrimSpace(string(out))}
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	var rows []psRow
	for i, line := range lines {
		if i == 0 {
			continue // header
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		idx := strings.IndexAny(trimmed, " \t")
		row := psRow{Name: trimmed}
		if idx > 0 {
			row.Name = trimmed[:idx]
			row.Details = strings.TrimSpace(trimmed[idx:])
		}
		rows = append(rows, row)
	}
	return psFetchMsg{rows: rows}
}

type killResultMsg struct {
	name   string
	output string
	err    string
}

func killModel(name string) tea.Cmd {
	return func() tea.Msg {
		out, err := exec.Command("ollama", "stop", name).CombinedOutput()
		if err != nil {
			return killResultMsg{name: name, output: string(out), err: err.Error()}
		}
		return killResultMsg{name: name, output: string(out)}
	}
}

// downloadChanMsg is emitted once a pull has started; it carries the
// channel that further progress/done messages will arrive on, and the
// running command so an abort key can kill it.
type downloadChanMsg struct {
	ch  chan tea.Msg
	cmd *exec.Cmd
}

// downloadLineMsg is one progress update parsed from `ollama pull` output.
// pct is -1 when the line didn't contain a percentage.
type downloadLineMsg struct {
	line string
	pct  int
}

type downloadDoneMsg struct {
	err error
}

var pullPctRe = regexp.MustCompile(`(\d+)%`)

// ollama redraws several concurrent layer-pull lines in place by moving the
// cursor between them (A/B = up/down, K = erase line). Losing those moves
// without a substitute runs separate lines together, so they become "\n"
// instead of vanishing; every other CSI sequence (color, hide/show cursor,
// absolute positioning) is just noise for our purposes and is dropped.
var ansiMoveRe = regexp.MustCompile(`\x1b\[[0-9]*[ABK]`)
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

func stripANSI(s string) string {
	s = ansiMoveRe.ReplaceAllString(s, "\n")
	s = ansiRe.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if r == '\n' {
			return '\n'
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

var (
	pullSizeRe  = regexp.MustCompile(`[\d.]+\s?[KMGT]?i?B\s*/\s*[\d.]+\s?[KMGT]?i?B`)
	pullSpeedRe = regexp.MustCompile(`[\d.]+\s?[KMGT]?i?B/s`)
	pullEtaRe   = regexp.MustCompile(`\b(?:\d+h)?(?:\d+m)?\d+s\b`)
)

// cleanPullLine rebuilds a pull-progress line from only its known fields
// (label, percent, size, speed, eta), discarding whatever glyphs ollama
// used to draw its own bar — we already draw our own, and those glyphs
// vary and are easy to miss trying to match/strip directly.
func cleanPullLine(raw string) string {
	var out []string
	for _, p := range strings.Split(raw, "\n") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		pctLoc := pullPctRe.FindStringIndex(p)
		if pctLoc == nil {
			out = append(out, p)
			continue
		}
		label := strings.TrimSpace(p[:pctLoc[0]])
		pct := p[pctLoc[0]:pctLoc[1]]
		rest := p[pctLoc[1]:]

		var parts []string
		if label != "" {
			parts = append(parts, label)
		}
		parts = append(parts, pct)
		if s := pullSizeRe.FindString(rest); s != "" {
			parts = append(parts, s)
		}
		if s := pullSpeedRe.FindString(rest); s != "" {
			parts = append(parts, s)
		}
		if s := pullEtaRe.FindString(rest); s != "" {
			parts = append(parts, s)
		}
		out = append(out, strings.Join(parts, "   "))
	}
	return strings.Join(out, "\n")
}

// splitCROrLF is a bufio.SplitFunc that treats both \r and \n as line
// terminators, since ollama's pull progress redraws a line with \r.
func splitCROrLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// startDownload runs `ollama pull` for a catalog row, streaming progress
// lines back over a channel so the UI can show a live bar and let the user
// abort mid-download. Hugging Face repos go through ollama's hf.co/ passthrough.
func startDownload(row catalogRow) tea.Cmd {
	target := row.Name
	if row.Source == "huggingface" {
		target = "hf.co/" + row.Name
	}
	return func() tea.Msg {
		cmd := exec.Command("ollama", "pull", target)
		pr, pw := io.Pipe()
		cmd.Stdout = pw
		cmd.Stderr = pw
		ch := make(chan tea.Msg, 8)

		if err := cmd.Start(); err != nil {
			pw.Close()
			go func() { ch <- downloadDoneMsg{err: err}; close(ch) }()
			return downloadChanMsg{ch: ch, cmd: cmd}
		}

		go func() {
			err := cmd.Wait()
			pw.CloseWithError(err)
		}()

		go func() {
			scanner := bufio.NewScanner(pr)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			scanner.Split(splitCROrLF)
			for scanner.Scan() {
				line := cleanPullLine(stripANSI(scanner.Text()))
				if line == "" {
					continue
				}
				pct := -1
				if mm := pullPctRe.FindStringSubmatch(line); mm != nil {
					if v, convErr := strconv.Atoi(mm[1]); convErr == nil {
						pct = v
					}
				}
				ch <- downloadLineMsg{line: line, pct: pct}
			}
			ch <- downloadDoneMsg{err: scanner.Err()}
			close(ch)
		}()

		return downloadChanMsg{ch: ch, cmd: cmd}
	}
}

// waitForDownloadMsg pulls the next message off a running download's channel.
func waitForDownloadMsg(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return downloadDoneMsg{}
		}
		return msg
	}
}

// Setup wizard: a guided disclaimer-accept + install-ollama +
// download-starter-models flow, reached from the help menu. Every
// question is asked up front, then the selected actions run in sequence
// — this is deliberately separate from startDownload()'s own state (used
// by the "list models" screen) even though it reuses the same
// stripANSI/cleanPullLine parsing, so a wizard run can't collide with an
// unrelated download the user might have already had in progress.

type wizardQuestion struct {
	id     string
	prompt string
}

type wizardAction struct {
	kind  string // "install_ollama" | "pull"
	model string // set for "pull"
}

type wizardPullChanMsg struct {
	ch  chan tea.Msg
	cmd *exec.Cmd
}

type wizardPullLineMsg struct {
	line string
	pct  int
}

type wizardPullDoneMsg struct {
	model string
	err   error
}

func wizardPullModel(model string) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command("ollama", "pull", model)
		pr, pw := io.Pipe()
		cmd.Stdout = pw
		cmd.Stderr = pw
		ch := make(chan tea.Msg, 8)

		if err := cmd.Start(); err != nil {
			pw.Close()
			go func() { ch <- wizardPullDoneMsg{model: model, err: err}; close(ch) }()
			return wizardPullChanMsg{ch: ch, cmd: cmd}
		}

		go func() {
			err := cmd.Wait()
			pw.CloseWithError(err)
		}()

		go func() {
			scanner := bufio.NewScanner(pr)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			scanner.Split(splitCROrLF)
			for scanner.Scan() {
				line := cleanPullLine(stripANSI(scanner.Text()))
				if line == "" {
					continue
				}
				pct := -1
				if mm := pullPctRe.FindStringSubmatch(line); mm != nil {
					if v, convErr := strconv.Atoi(mm[1]); convErr == nil {
						pct = v
					}
				}
				ch <- wizardPullLineMsg{line: line, pct: pct}
			}
			ch <- wizardPullDoneMsg{model: model, err: scanner.Err()}
			close(ch)
		}()

		return wizardPullChanMsg{ch: ch, cmd: cmd}
	}
}

func waitForWizardPullMsg(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return wizardPullDoneMsg{}
		}
		return msg
	}
}

func startWizardAction(a wizardAction) tea.Cmd {
	switch a.kind {
	case "install_ollama":
		return installOllama()
	case "pull":
		return wizardPullModel(a.model)
	}
	return nil
}

func buildWizardActions(answers map[string]bool) []wizardAction {
	var actions []wizardAction
	if answers["ollama"] {
		actions = append(actions, wizardAction{kind: "install_ollama"})
	}
	if answers["qwen"] {
		actions = append(actions, wizardAction{kind: "pull", model: "qwen2.5:1.5b"})
	}
	if answers["gemma2"] {
		actions = append(actions, wizardAction{kind: "pull", model: "gemma2:2b"})
	}
	if answers["gemma4"] {
		actions = append(actions, wizardAction{kind: "pull", model: "gemma4:e2b"})
	}
	return actions
}

// advanceWizard moves to the next queued action, or to the "done" phase
// if that was the last one.
func advanceWizard(m model) (tea.Model, tea.Cmd) {
	m.wizardActionIdx++
	if m.wizardActionIdx >= len(m.wizardActions) {
		m.wizardPhase = "done"
		return m, nil
	}
	return m, startWizardAction(m.wizardActions[m.wizardActionIdx])
}

// finishWizardQuestions is called once every question has an answer —
// builds the action list and, if any of it downloads a model, checks
// real free disk space before starting anything. A model half-downloaded
// because the disk filled up mid-pull is a worse experience than telling
// the user up front and not starting at all.
func finishWizardQuestions(m model) (tea.Model, tea.Cmd) {
	// The disclaimer question only blocks anything if it was actually
	// asked THIS run (skipDisclaimer runs never add it, and a Go map
	// defaults a missing key to false — so check presence, not just the
	// answer, or a skipped disclaimer would wrongly read as "declined").
	for _, q := range m.wizardQuestions {
		if q.id == "disclaimer" && !m.wizardAnswers["disclaimer"] {
			m.wizardPhase = "done"
			m.wizardLog = append(m.wizardLog, "disclaimer declined — no actions were taken")
			appendLog("wizard: disclaimer declined, no actions taken")
			return m, nil
		}
	}
	m.wizardActions = buildWizardActions(m.wizardAnswers)
	m.wizardActionIdx = 0
	if len(m.wizardActions) == 0 {
		m.wizardPhase = "done"
		return m, nil
	}
	ok, free, needed, msg := checkWizardDiskSpace(m.wizardActions)
	m.wizardDiskFreeBytes = free
	m.wizardDiskNeededBytes = needed
	if !ok {
		m.wizardPhase = "blocked"
		m.wizardDiskMsg = msg
		return m, nil
	}
	m.wizardPhase = "run"
	return m, startWizardAction(m.wizardActions[0])
}

// approxModelSizeBytes are the real download sizes from ollama.com's own
// library pages (qwen2.5:1.5b, gemma2:2b) and the user's own local
// install (gemma4:e2b, confirmed 7.2 GB via `ollama list`) — not guesses.
var approxModelSizeBytes = map[string]uint64{
	"qwen2.5:1.5b": 986 * 1000 * 1000,
	"gemma2:2b":    1600 * 1000 * 1000,
	"gemma4:e2b":   7200 * 1000 * 1000,
}

// ollamaModelsDir is where `ollama pull` actually writes blobs — needed
// so the disk-space check below looks at the right drive/filesystem, not
// just wherever llama-shell happens to be running from.
func ollamaModelsDir() string {
	if v := os.Getenv("OLLAMA_MODELS"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".ollama", "models")
}

// diskFreeBytes reports free space on the drive/filesystem holding dir.
// Shells out rather than using syscall.Statfs, since that type doesn't
// exist when this same file is cross-compiled for windows — matches the
// existing pattern elsewhere in this file (installOllama, openInBrowser)
// of branching per-OS command instead of per-OS build-tagged files.
func diskFreeBytes(dir string) (uint64, error) {
	if runtime.GOOS == "windows" {
		vol := filepath.VolumeName(dir)
		driveLetter := strings.TrimSuffix(vol, ":")
		if driveLetter == "" {
			driveLetter = "C"
		}
		out, err := exec.Command("powershell", "-NoProfile", "-Command",
			fmt.Sprintf("(Get-PSDrive -Name %q).Free", driveLetter)).Output()
		if err != nil {
			return 0, err
		}
		return strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	}
	out, err := exec.Command("df", "-Pk", dir).Output()
	if err != nil {
		return 0, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected df output: %q", string(out))
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, fmt.Errorf("unexpected df output line: %q", lines[len(lines)-1])
	}
	availKB, err := strconv.ParseUint(fields[3], 10, 64)
	if err != nil {
		return 0, err
	}
	return availKB * 1024, nil
}

// checkWizardDiskSpace sums the estimated size of every selected pull
// action and compares it against real free space on the drive Ollama
// actually stores models on. A 20% safety margin is added on top of the
// raw download total since ollama's blob store briefly holds compressed
// and decompressed data at once mid-pull.
func checkWizardDiskSpace(actions []wizardAction) (ok bool, freeBytes, neededBytes uint64, msg string) {
	var needed uint64
	for _, a := range actions {
		if a.kind == "pull" {
			needed += approxModelSizeBytes[a.model]
		}
	}
	if needed == 0 {
		return true, 0, 0, ""
	}
	needed = needed + needed/5 // +20% margin

	dir := ollamaModelsDir()
	free, err := diskFreeBytes(dir)
	if err != nil {
		// Can't verify — don't block the wizard over a check that itself
		// failed, just proceed without the guarantee.
		return true, 0, needed, ""
	}
	if free >= needed {
		return true, free, needed, ""
	}
	toGB := func(b uint64) float64 { return float64(b) / 1e9 }
	return false, free, needed, fmt.Sprintf(
		"Not enough free disk space on %s: need ~%.1f GB (incl. margin), only %.1f GB free.",
		dir, toGB(needed), toGB(free))
}

// advanceWizardChain moves to the next queued wizard setup screen, or —
// if the queue is empty — back to viewMenu when this navigation came from
// the wizard (wizardInChain), else viewHelpMenu for a screen opened
// directly (not part of any chain). Called from each chainable settings
// screen's own exit points (Esc, or a successful save) instead of each
// hardcoding its "next" destination.
func (m model) advanceWizardChain() model {
	if len(m.wizardPendingSetups) > 0 {
		next := m.wizardPendingSetups[0]
		m.wizardPendingSetups = m.wizardPendingSetups[1:]
		m.view = next
		if next == viewEmailSettings {
			m.emailEditing = loadEmailConfig().FromAddress == ""
		}
		appendLog("wizard: opening %s setup", helpMenuLabelForView(next))
		return m
	}
	if m.wizardInChain {
		m.wizardInChain = false
		m.wizardPhase = ""
		m.view = viewMenu
		return m
	}
	m.view = viewHelpMenu
	return m
}

// helpMenuLabelForView gives a short human name for a wizard-chained
// settings screen, for log messages only.
func helpMenuLabelForView(v view) string {
	switch v {
	case viewTavilySettings:
		return "tavily key"
	case viewTelegramSettings:
		return "telegram bot"
	case viewEmailSettings:
		return "email"
	default:
		return "settings"
	}
}

// enterWizard builds the setup-wizard question list and switches to it.
// skipDisclaimer omits the disclaimer question — used when the wizard is
// entered right after the first-run disclaimer gate already accepted it
// a moment earlier, so it isn't asked twice in the same flow.
func (m model) enterWizard(skipDisclaimer bool) model {
	m.wizardQuestions = nil
	if !skipDisclaimer {
		m.wizardQuestions = append(m.wizardQuestions, wizardQuestion{id: "disclaimer", prompt: "Accept the disclaimer to continue with setup?"})
	}
	if !m.ollama.installed {
		m.wizardQuestions = append(m.wizardQuestions, wizardQuestion{id: "ollama", prompt: "Install Ollama now?"})
		m.wizardOllamaSkipNote = ""
	} else {
		m.wizardOllamaSkipNote = fmt.Sprintf("install ollama: already installed (%s) — skipped", m.ollama.version)
	}
	m.wizardQuestions = append(m.wizardQuestions,
		wizardQuestion{id: "qwen", prompt: "Download qwen2.5:1.5b (small, fast, ~1 GB)?"},
		wizardQuestion{id: "gemma2", prompt: "Download gemma2:2b (~1.6 GB)?"},
		wizardQuestion{id: "gemma4", prompt: "Download gemma4:e2b (~7 GB)?"},
		wizardQuestion{id: "tavily", prompt: "Set up a Tavily API key now, for web-search/scraping tools in agentic chat?"},
		wizardQuestion{id: "telegram", prompt: "Set up a Telegram bot now, to chat with the agent from your phone?"},
		wizardQuestion{id: "email", prompt: "Set up a Gmail account now, so the agent can send email?"},
	)
	m.wizardQIndex = 0
	m.wizardAnswers = map[string]bool{}
	m.wizardActions = nil
	m.wizardActionIdx = 0
	m.wizardLog = nil
	m.wizardPhase = "ask"
	m.wizardCancelled = false
	m.wizardPullCh = nil
	m.wizardPullCmd = nil
	m.wizardPullPct = 0
	m.wizardPullLine = ""
	m.view = viewWizard
	return m
}

// markInstalled cross-references huggingface catalog entries against the
// locally installed ollama models and sets Installed accordingly. ollama
// rows are already local by definition.
func markInstalled(rows []catalogRow) {
	local := map[string]bool{}
	for _, r := range rows {
		if r.Source == "ollama" {
			base := r.Name
			if idx := strings.Index(base, ":"); idx != -1 {
				base = base[:idx]
			}
			local[strings.ToLower(base)] = true
		}
	}
	for i, r := range rows {
		if r.Source == "ollama" {
			continue
		}
		repo := r.Name
		if idx := strings.LastIndex(repo, "/"); idx != -1 {
			repo = repo[idx+1:]
		}
		rows[i].Installed = local[strings.ToLower(repo)]
	}
}

// filteredCatalog returns catalogRows narrowed by catalogSearch (case
// insensitive substring match on name or source).
func (m model) filteredCatalog() []catalogRow {
	if m.catalogSearch == "" {
		return m.catalogRows
	}
	q := strings.ToLower(m.catalogSearch)
	var out []catalogRow
	for _, r := range m.catalogRows {
		if strings.Contains(strings.ToLower(r.Name), q) || strings.Contains(strings.ToLower(r.Source), q) {
			out = append(out, r)
		}
	}
	return out
}

// parseSizeList splits a catalog row's Size field ("0.5b,1.5b,7b") into its
// individual parameter-size options, or nil if there's nothing to pick from.
func parseSizeList(size string) []string {
	if size == "" || size == "-" {
		return nil
	}
	return strings.Split(size, ",")
}

var sizeUnitRe = regexp.MustCompile(`([\d.]+)\s*([KMGT]?B)`)

// parseSizeBytes converts a modelRow.Size string ("4.7 GB", "397 MB") to
// bytes for sorting. Unknown/unparsable sizes (e.g. "-" for cloud models)
// sort last, since we want known-small models benchmarked first.
func parseSizeBytes(size string) float64 {
	mm := sizeUnitRe.FindStringSubmatch(size)
	if mm == nil {
		return math.MaxFloat64
	}
	v, err := strconv.ParseFloat(mm[1], 64)
	if err != nil {
		return math.MaxFloat64
	}
	switch mm[2] {
	case "KB":
		return v * 1024
	case "MB":
		return v * 1024 * 1024
	case "GB":
		return v * 1024 * 1024 * 1024
	case "TB":
		return v * 1024 * 1024 * 1024 * 1024
	default:
		return v
	}
}

type deviceInfo struct {
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	CPUModel string   `json:"cpu_model"`
	Cores    int      `json:"cores"`
	RAMTotal string   `json:"ram_total"`
	Disks    []string `json:"disks"`
	GPUs     []string `json:"gpus"`
}

func deviceCacheFilePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "llama-shell", "device_cache.json")
}

func loadDeviceCache() (*deviceInfo, bool) {
	data, err := os.ReadFile(deviceCacheFilePath())
	if err != nil {
		return nil, false
	}
	var info deviceInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, false
	}
	return &info, true
}

func saveDeviceCache(info deviceInfo) error {
	path := deviceCacheFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

type deviceInfoMsg struct {
	info deviceInfo
}

// runQuick runs a command with a short timeout and returns its trimmed
// output, or "" if it fails — device info is best-effort, never fatal.
func runQuick(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gatherDeviceInfo() tea.Msg {
	info := deviceInfo{
		OS:    runtime.GOOS,
		Arch:  runtime.GOARCH,
		Cores: runtime.NumCPU(),
	}

	switch runtime.GOOS {
	case "windows":
		info.CPUModel = runQuick("powershell", "-NoProfile", "-Command",
			"(Get-CimInstance Win32_Processor | Select-Object -First 1).Name")

		if ramBytes := runQuick("powershell", "-NoProfile", "-Command",
			"(Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory"); ramBytes != "" {
			if v, err := strconv.ParseFloat(ramBytes, 64); err == nil {
				info.RAMTotal = fmt.Sprintf("%.1f GB", v/1024/1024/1024)
			}
		}

		if diskOut := runQuick("powershell", "-NoProfile", "-Command",
			`Get-CimInstance Win32_LogicalDisk -Filter "DriveType=3" | ForEach-Object { "$($_.DeviceID) $([math]::Round(($_.Size-$_.FreeSpace)/1GB,1))GB used / $([math]::Round($_.Size/1GB,1))GB total" }`); diskOut != "" {
			info.Disks = strings.Split(diskOut, "\n")
		}

		if gpuOut := runQuick("powershell", "-NoProfile", "-Command",
			"(Get-CimInstance Win32_VideoController).Name"); gpuOut != "" {
			info.GPUs = strings.Split(gpuOut, "\n")
		}

	case "linux":
		if cpuinfo, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, line := range strings.Split(string(cpuinfo), "\n") {
				if strings.HasPrefix(line, "model name") {
					if idx := strings.Index(line, ":"); idx != -1 {
						info.CPUModel = strings.TrimSpace(line[idx+1:])
					}
					break
				}
			}
		}
		if meminfo, err := os.ReadFile("/proc/meminfo"); err == nil {
			for _, line := range strings.Split(string(meminfo), "\n") {
				if strings.HasPrefix(line, "MemTotal:") {
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						if kb, err := strconv.ParseFloat(fields[1], 64); err == nil {
							info.RAMTotal = fmt.Sprintf("%.1f GB", kb/1024/1024)
						}
					}
					break
				}
			}
		}
		if diskOut := runQuick("df", "-h", "/"); diskOut != "" {
			lines := strings.Split(diskOut, "\n")
			if len(lines) >= 2 {
				info.Disks = []string{lines[1]}
			}
		}
		if gpuOut := runQuick("sh", "-c", "lspci 2>/dev/null | grep -i vga"); gpuOut != "" {
			info.GPUs = strings.Split(gpuOut, "\n")
		}

	case "darwin":
		info.CPUModel = runQuick("sysctl", "-n", "machdep.cpu.brand_string")
		if ramBytes := runQuick("sysctl", "-n", "hw.memsize"); ramBytes != "" {
			if v, err := strconv.ParseFloat(ramBytes, 64); err == nil {
				info.RAMTotal = fmt.Sprintf("%.1f GB", v/1024/1024/1024)
			}
		}
		if diskOut := runQuick("df", "-h", "/"); diskOut != "" {
			lines := strings.Split(diskOut, "\n")
			if len(lines) >= 2 {
				info.Disks = []string{lines[1]}
			}
		}
		if gpuOut := runQuick("sh", "-c", "system_profiler SPDisplaysDataType 2>/dev/null | grep Chipset"); gpuOut != "" {
			info.GPUs = strings.Split(gpuOut, "\n")
		}
	}

	if info.CPUModel == "" {
		info.CPUModel = "unknown"
	}
	if info.RAMTotal == "" {
		info.RAMTotal = "unknown"
	}
	if len(info.Disks) == 0 {
		info.Disks = []string{"unknown"}
	}
	if len(info.GPUs) == 0 {
		info.GPUs = []string{"unknown"}
	}

	return deviceInfoMsg{info: info}
}

// --- Per-model CPU/GPU benchmark, triggered from Device Info ---
//
// For each installed model: load it (`ollama run <model> hi`), time how
// long that takes, read its live CPU/GPU split from `ollama ps`, then
// `ollama stop` it before moving to the next so only one model occupies
// memory at a time.

var benchProcRe = regexp.MustCompile(`\d+%(?:/\d+%)?\s*(?:CPU/GPU|GPU|CPU)`)
var benchGPUPctRe = regexp.MustCompile(`(?:(\d+)%/)?(\d+)%\s*(CPU/GPU|GPU|CPU)`)

// formatCPUGPU turns raw ollama ps output ("100% GPU", "44%/56% CPU/GPU")
// into a compact value for the CPU/GPU column: a 100%-on-one-side result
// becomes just the number plus a c/g letter (the column header already
// says "CPU/GPU", no need to spell it out); a genuine split keeps both
// numbers in that same CPU/GPU order without the unit labels.
func formatCPUGPU(raw string) string {
	if raw == "" || raw == "-" {
		return "-"
	}
	mm := benchGPUPctRe.FindStringSubmatch(raw)
	if mm == nil {
		return raw
	}
	switch mm[3] {
	case "GPU":
		return mm[2] + "g"
	case "CPU":
		return mm[2] + "c"
	case "CPU/GPU":
		return mm[1] + "/" + mm[2]
	}
	return raw
}

// benchStartMsg is emitted the moment a model's `ollama run` process starts,
// carrying the command (so a cancel key can kill it) and the channel its
// completion will arrive on.
type benchStartMsg struct {
	index int
	cmd   *exec.Cmd
	ch    chan tea.Msg
}

type benchLoadDoneMsg struct {
	index   int
	elapsed float64
	err     error
}

type benchMeasuredMsg struct {
	index  int
	cpuGpu string
}

func startBenchLoad(index int, name string) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command("ollama", "run", name, "hi")
		ch := make(chan tea.Msg, 1)
		start := time.Now()

		if err := cmd.Start(); err != nil {
			go func() {
				ch <- benchLoadDoneMsg{index: index, err: err}
				close(ch)
			}()
			return benchStartMsg{index: index, cmd: cmd, ch: ch}
		}

		go func() {
			err := cmd.Wait()
			elapsed := time.Since(start).Seconds()
			ch <- benchLoadDoneMsg{index: index, elapsed: elapsed, err: err}
			close(ch)
		}()

		return benchStartMsg{index: index, cmd: cmd, ch: ch}
	}
}

func waitForBenchMsg(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return benchLoadDoneMsg{}
		}
		return msg
	}
}

// measureAndUnload reads the model's live CPU/GPU split from `ollama ps`
// and then stops it, freeing memory before the next model in the loop.
func measureAndUnload(index int, name string) tea.Cmd {
	return func() tea.Msg {
		out, _ := exec.Command("ollama", "ps").CombinedOutput()
		cpuGpu := "-"
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || fields[0] != name {
				continue
			}
			if m := benchProcRe.FindString(line); m != "" {
				cpuGpu = m
			}
			break
		}
		_ = exec.Command("ollama", "stop", name).Run()
		return benchMeasuredMsg{index: index, cpuGpu: cpuGpu}
	}
}

// computeMatchScore grades 0-100 how well a model suits this machine,
// weighting the GPU share of the split heavily (70%) and how fast it
// loaded (30%, against a 30s-to-zero heuristic — tune as real data comes in).
func computeMatchScore(cpuGpu string, elapsedSecs float64) int {
	gpuPct := 0.0
	if mm := benchGPUPctRe.FindStringSubmatch(cpuGpu); mm != nil {
		switch mm[3] {
		case "GPU":
			gpuPct = 100
		case "CPU":
			gpuPct = 0
		case "CPU/GPU":
			if v, err := strconv.ParseFloat(mm[2], 64); err == nil {
				gpuPct = v
			}
		}
	}
	timeScore := 100 - elapsedSecs*100/30
	if timeScore < 0 {
		timeScore = 0
	}
	if timeScore > 100 {
		timeScore = 100
	}
	score := 0.7*gpuPct + 0.3*timeScore
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return int(score + 0.5)
}

type ollamaStatus struct {
	installed bool
	version   string
}

func checkOllama() ollamaStatus {
	_, err := exec.LookPath("ollama")
	if err != nil {
		return ollamaStatus{installed: false}
	}
	out, err := exec.Command("ollama", "--version").Output()
	v := strings.TrimSpace(string(out))
	if err != nil || v == "" {
		return ollamaStatus{installed: true, version: "unknown"}
	}
	// output looks like "ollama version is 0.32.5" — keep just the number.
	if idx := strings.LastIndex(v, " "); idx != -1 {
		last := v[idx+1:]
		if len(last) > 0 && (last[0] >= '0' && last[0] <= '9') {
			v = "v" + last
		}
	}
	return ollamaStatus{installed: true, version: v}
}

type ollamaInstallResultMsg struct {
	output string
	err    string
}

// installOllama runs the best available unattended install path per OS.
// Linux uses Ollama's official one-line installer script. macOS and
// Windows don't offer a silent/scriptable install (the official
// distribution is a signed .app / .exe installer meant to be run
// interactively), so those just open the download page instead of trying
// to fake an unattended install.
func installOllama() tea.Cmd {
	return func() tea.Msg {
		switch runtime.GOOS {
		case "linux":
			out, err := exec.Command("sh", "-c", "curl -fsSL https://ollama.com/install.sh | sh").CombinedOutput()
			if err != nil {
				return ollamaInstallResultMsg{output: string(out), err: err.Error()}
			}
			return ollamaInstallResultMsg{output: string(out)}
		case "darwin":
			openErr := openInBrowser("https://ollama.com/download/mac")
			msg := "Opened https://ollama.com/download/mac — download and run the installer, then relaunch llama-shell."
			if openErr != nil {
				msg = "Couldn't open a browser automatically. Download and install Ollama from https://ollama.com/download/mac, then relaunch llama-shell."
			}
			return ollamaInstallResultMsg{output: msg}
		default: // windows and anything else
			openErr := openInBrowser("https://ollama.com/download/windows")
			msg := "Opened https://ollama.com/download/windows — download and run the installer, then relaunch llama-shell."
			if openErr != nil {
				msg = "Couldn't open a browser automatically. Download and install Ollama from https://ollama.com/download/windows, then relaunch llama-shell."
			}
			return ollamaInstallResultMsg{output: msg}
		}
	}
}

// openInBrowser opens a URL with the OS's default handler — same mechanism
// as the open_url agent tool, duplicated here to avoid a dependency from
// this early-startup path into the agent-tool code.
func openInBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

type menuItem struct {
	key   string
	label string
}

var menuItems = []menuItem{
	{"a", "autopilot        (auto: install ollama + gemma4:e2b + enable web server)"},
	{"l", "list models      (ollama + library + huggingface)"},
	{"p", "running models    (ollama ps)"},
	{"s", "Select Model      (scan all + cache)"},
	{"d", "device info       (cpu/ram/disk/gpu)"},
	{"h", "help / settings"},
	{"q", "quit"},
}

type helpMenuItem struct {
	key   string
	label string
	dest  view
}

var helpMenuItems = []helpMenuItem{
	{"h", "read help", viewHelpText},
	{"d", "disclaimer (no warranty)", viewDisclaimerText},
	{"g", "view log", viewLogText},
	{"u", "update (includes auto-update settings)", viewUpdateText},
	{"w", "setup wizard (install ollama + download starter models)", viewWizard},
	{"t", "tavily API key (enables tavily_search/tavily_extract tools)", viewTavilySettings},
	{"b", "web server (browser access to agentic chat)", viewWebServerSettings},
	{"m", "telegram bot (chat with the agent from your phone)", viewTelegramSettings},
	{"e", "email (Gmail app password, sends a test email)", viewEmailSettings},
	{"x", "backup / restore (encrypted export & import of all settings)", viewBackupSettings},
}

type model struct {
	width, height int
	ollama        ollamaStatus
	cubeAngle     float64 // main-menu banner's rotation phase, advanced by cubeTickCmd

	view    view
	output  string
	loading bool

	menuCursor int

	// installedDirty is set after a successful download so the next visit
	// to "show model info" forces a fresh scan instead of trusting its cache.
	installedDirty bool

	deviceInfo    *deviceInfo
	deviceLoading bool

	benchConfirm    bool
	benchRunning    bool
	benchIndex      int
	benchTotal      int
	benchCmd        *exec.Cmd
	benchCh         chan tea.Msg
	benchElapsed    float64
	benchCancelling bool
	benchDoneMsg    string

	helpMenuCursor int
	helpScroll     int

	ollamaInstallRunning bool
	ollamaInstallResult  string
	ollamaInstallErr     string

	wizardPhase           string // "", "ask", "blocked", "run", "done"
	wizardQuestions       []wizardQuestion
	wizardQIndex          int
	wizardAnswers         map[string]bool
	wizardActions         []wizardAction
	wizardActionIdx       int
	wizardLog             []string
	wizardCancelled       bool
	wizardDiskMsg         string
	wizardDiskFreeBytes   uint64
	wizardDiskNeededBytes uint64
	wizardOllamaSkipNote  string
	wizardPullCh          chan tea.Msg
	wizardPullCmd         *exec.Cmd
	wizardPullPct         int
	wizardPullLine        string

	updateChecked     bool
	updateAvailable   bool
	updateLatest      string
	updateAssetURL    string
	updateCheckErr    string
	updateDownloading bool
	updateResult      string
	updateResultErr   string

	agentPasting     bool
	agentPasteNotice string

	toolCatCursor  int
	toolDetailOpen bool

	scanRows   []modelRow
	scanTotal  int
	scanDone   int
	scanErr    string
	fromCache  bool
	scanCursor int

	scanAction    bool
	scanActionSel int
	scanBusy      bool
	scanBusyLabel string
	scanActionMsg string

	catalogRows   []catalogRow
	catalogStage  int
	catalogErrs   []string
	catalogCursor int
	catalogSearch string

	tavilyKeyInput string
	tavilyKeyMsg   string
	tavilyEditing  bool

	telegramEditing bool

	// emailFieldFocus: 0 = address, 1 = display name (optional), 2 = app password.
	emailAddrInput        string
	emailDisplayNameInput string
	emailPassInput        string
	emailFieldFocus       int
	emailMsg              string
	emailSending          bool
	// emailEditing: false shows the greyed-out configured summary (with a
	// [r] reconfigure option) whenever an account is already saved; true
	// shows the live input fields. Always true when nothing is saved yet.
	emailEditing bool

	// backupMode: "" = the export/import chooser menu, "export" or
	// "import" once one is picked — that switches to viewBackupBrowser,
	// an in-terminal directory browser (no OS GUI dialog, no password
	// prompt — see backupFixedPassphrase).
	backupMode          string
	backupMsg           string
	backupBrowseDir     string
	backupBrowseEntries []fileBrowseEntry
	backupBrowseCursor  int
	// backupBrowseFilename/backupBrowseEditingName: export mode only —
	// the typed destination filename and whether the filename field (vs.
	// the directory list) currently has keyboard focus.
	backupBrowseFilename    string
	backupBrowseEditingName bool

	autoUpdateEditingTime bool
	autoUpdateTimeInput   string
	autoUpdateMsg         string

	// agentReturnView is where Esc from the agentic chat goes back to —
	// viewShowTable normally (entered via "show model info"), but viewMenu
	// when autopilot started the chat directly with nothing scanned.
	agentReturnView view

	// autopilotPhase drives the one-click "install ollama + pull gemma4:e2b
	// + enable web server" flow: "installing_ollama", "pulling_model",
	// "enabling_server", "done", or "" before it starts.
	autopilotPhase string
	autopilotMsg   string
	// startupCmd carries the tea.Cmd from an --autopilot-triggered
	// enterMenu("a") call in initialModel() through to Init(), which is
	// the only place a Model can actually hand bubbletea an initial Cmd.
	startupCmd tea.Cmd
	// logQuery/logSearchMode drive the searchable log viewer: typing '/'
	// enters search mode, subsequent characters filter, Enter locks the
	// filter in while returning to normal scroll navigation.
	logQuery      string
	logSearchMode bool

	webServerBusy        bool
	webServerMsg         string
	webServerAwaitingDL  bool
	webServerModelList   []string
	webServerModelCursor int
	webServerEditingPort bool
	webServerPortInput   string

	telegramTokenInput string
	telegramMsg        string

	// wizardPendingSetups queues the optional setup screens (tavily,
	// telegram, email) the wizard's "done" phase chains through in order —
	// each screen's exit pops the next one instead of going to help menu.
	// wizardInChain marks that this navigation came from the wizard, so
	// the LAST screen in the chain goes back to viewMenu (matching a
	// completed wizard run) instead of viewHelpMenu (a direct, non-wizard
	// visit to that same settings screen).
	wizardPendingSetups []view
	wizardInChain       bool

	downloadConfirm  *catalogRow
	downloading      bool
	downloadTarget   string
	downloadMsg      string
	downloadCh       chan tea.Msg
	downloadCmd      *exec.Cmd
	downloadPct      int
	downloadLine     string
	downloadAborting bool

	sizeSelectRow     *catalogRow
	sizeSelectOptions []string
	sizeSelectCursor  int

	psRows        []psRow
	psCursor      int
	psErr         string
	psKillTarget  string
	psKilling     bool
	psKillMsg     string
	psConfirmKill bool

	// Agentic chat (own agent, read/write files), started from the
	// "show model info" action menu.
	agentModelName      string
	agentWorkDir        string
	agentCapabilities   string
	agentToolsSupported bool
	agentToolMode       string // "auto" (default), "on", "off"
	agentWarmup         string // "pending", "ready", or "error: ..."
	agentMessages       []ollamaChatMsg
	agentInput          string
	agentBusy           bool
	agentErr            string
	agentSpinner        int
	agentStarted        time.Time
	agentWarmupStarted  time.Time
	// agentTurnTimes has one entry per completed turn, in order — matched
	// up to the Nth content-bearing assistant reply when rendering, same
	// approach as the web UI's turnElapsed.
	agentTurnTimes []time.Duration
	agentScroll    int // lines scrolled back from the bottom; 0 = live/latest
	agentStreamBuf string
	agentViewport  viewport.Model
	agentVPReady   bool
}

func initialModel() model {
	loadTavilyKey()
	m := model{
		ollama: checkOllama(),
	}
	if !isDisclaimerAccepted() {
		m.view = viewFirstRunDisclaimer
	}
	if cfg := loadWebServerConfig(); cfg.Enabled && cfg.Model != "" {
		wd, err := os.Getwd()
		if err != nil {
			wd = "."
		}
		if err := startWebServer(cfg, wd); err != nil {
			appendLog("web server: failed to auto-start: %s", err.Error())
		}
	}
	if tgCfg := loadTelegramConfig(); tgCfg.Token != "" && tgCfg.Model != "" {
		wd, err := os.Getwd()
		if err != nil {
			wd = "."
		}
		startTelegramBot(tgCfg, wd)
	}
	// --autopilot reuses the exact same flow as pressing [a] in the main
	// menu — but only once the disclaimer's actually been accepted; a
	// first run still needs that gate regardless of the flag.
	if startupAutopilot && m.view != viewFirstRunDisclaimer {
		newM, cmd := m.enterMenu("a")
		m = newM.(model)
		m.startupCmd = cmd
	}
	return m
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{checkForUpdate(), cubeTickCmd()}
	if m.startupCmd != nil {
		cmds = append(cmds, m.startupCmd)
	}
	return tea.Batch(cmds...)
}

const autopilotModel = "gemma4:e2b"

func (m model) enterMenu(sel string) (tea.Model, tea.Cmd) {
	switch sel {
	case "a":
		m.view = viewAutopilot
		m.autopilotMsg = ""
		if !checkOllama().installed {
			m.autopilotPhase = "installing_ollama"
			appendLog("autopilot: ollama not installed, opening installer")
			return m, installOllama()
		}
		m.autopilotPhase = "pulling_model"
		appendLog("autopilot: pulling %s", autopilotModel)
		return m, pullModelForWebServer(autopilotModel)
	case "l":
		appendLog("opened list models")
		m.catalogCursor = 0
		m.catalogSearch = ""
		m.downloadConfirm = nil
		m.downloading = false
		m.downloadMsg = ""
		m.view = viewListScan
		m.catalogRows = nil
		m.catalogStage = 0
		m.catalogErrs = nil
		return m, fetchCatalogStage(0)
	case "p":
		appendLog("opened running models")
		m.view = viewPs
		m.loading = true
		m.psRows = nil
		m.psErr = ""
		m.psCursor = 0
		m.psConfirmKill = false
		m.psKilling = false
		m.psKillMsg = ""
		return m, fetchPs
	case "s":
		appendLog("opened show model info")
		m.scanCursor = 0
		m.scanAction = false
		m.scanBusy = false
		m.scanActionMsg = ""
		if !m.installedDirty {
			if rows, ok := loadCache(); ok && len(rows) > 0 {
				m.view = viewShowTable
				m.scanRows = rows
				m.fromCache = true
				return m, nil
			}
		}
		m.installedDirty = false
		m.view = viewShowScan
		m.loading = true
		m.scanRows = nil
		m.scanTotal = 0
		m.scanDone = 0
		m.scanErr = ""
		m.fromCache = false
		return m, fetchModelList()
	case "d":
		appendLog("opened device info")
		m.view = viewDeviceInfo
		if info, ok := loadDeviceCache(); ok {
			m.deviceInfo = info
			m.deviceLoading = false
			return m, nil
		}
		m.deviceLoading = true
		m.deviceInfo = nil
		return m, gatherDeviceInfo
	case "h":
		appendLog("opened help menu")
		m.view = viewHelpMenu
		m.helpMenuCursor = 0
		return m, nil
	case "q":
		appendLog("quit")
		return m, tea.Quit
	}
	return m, nil
}

func (m model) rescanCatalog() (tea.Model, tea.Cmd) {
	appendLog("rescanned catalog (ignoring cache)")
	m.view = viewListScan
	m.catalogRows = nil
	m.catalogStage = 0
	m.catalogErrs = nil
	m.catalogCursor = 0
	return m, fetchCatalogStage(0)
}

func (m model) rescan() (tea.Model, tea.Cmd) {
	appendLog("rescanned installed models (ignoring cache)")
	_ = os.Remove(cacheFilePath())
	m.view = viewShowScan
	m.loading = true
	m.scanRows = nil
	m.scanTotal = 0
	m.scanDone = 0
	m.scanErr = ""
	m.fromCache = false
	m.scanCursor = 0
	m.scanAction = false
	m.scanBusy = false
	m.scanActionMsg = ""
	return m, fetchModelList()
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.agentVPReady {
			m.agentViewport.Width = agentViewportWidth(m.width)
			m.agentViewport.Height = agentViewportHeight(m.height)
			m.syncAgentViewport()
		}
		return m, nil

	case cmdResultMsg:
		m.loading = false
		m.output = msg.output
		return m, nil

	case catalogStageMsg:
		if m.view != viewListScan {
			return m, nil
		}
		if msg.err != "" {
			m.catalogErrs = append(m.catalogErrs, msg.err)
		}
		m.catalogRows = append(m.catalogRows, msg.rows...)
		m.catalogStage = msg.stage + 1
		if m.catalogStage >= len(catalogStages) {
			markInstalled(m.catalogRows)
			m.view = viewListTable
			return m, nil
		}
		return m, fetchCatalogStage(m.catalogStage)

	case webServerPullDoneMsg:
		m.webServerBusy = false
		if m.view == viewAutopilot {
			if msg.err != "" {
				m.autopilotPhase = "error"
				m.autopilotMsg = "model download failed: " + msg.err
				appendLog("autopilot: model download failed: %s", msg.err)
				return m, nil
			}
			appendLog("autopilot: %s ready", autopilotModel)
			cfg := loadWebServerConfig()
			if cfg.Token == "" {
				cfg.Token = genWebServerToken()
			}
			cfg.Enabled = true
			cfg.Model = autopilotModel
			if err := saveWebServerConfig(cfg); err != nil {
				m.autopilotPhase = "error"
				m.autopilotMsg = "error saving web server config: " + err.Error()
				return m, nil
			}
			wd, err := os.Getwd()
			if err != nil {
				wd = "."
			}
			if err := startWebServer(cfg, wd); err != nil {
				m.autopilotPhase = "error"
				m.autopilotMsg = "failed to start web server: " + err.Error()
				appendLog("autopilot: web server start failed: %s", err.Error())
				return m, nil
			}
			m.autopilotPhase = "done"
			m.autopilotMsg = webServerURL(cfg.Token, webServerEffectivePort(cfg))
			appendLog("autopilot: web server enabled with model %s", autopilotModel)
			// Drop straight into the TUI's own agentic chat too, instead of
			// leaving the user on a static "done" screen — autopilot's
			// whole point is getting to a working chat with zero manual
			// steps, and the web server alone doesn't give them that here.
			m.agentModelName = autopilotModel
			m.agentWorkDir = wd
			m.agentReturnView = viewMenu
			m.agentCapabilities = "" // unknown for a freshly pulled model; runAgentTurn falls back if tools aren't supported
			m.agentToolsSupported = true
			m.agentToolMode = "auto"
			m.agentWarmup = "pending"
			m.agentMessages = []ollamaChatMsg{{Role: "system", Content: agentSystemPrompt(wd)}}
			m.agentInput = ""
			m.agentErr = ""
			m.agentBusy = false
			m.agentStarted = time.Now()
			m.agentWarmupStarted = time.Now()
			m.agentTurnTimes = nil
			m.agentViewport = viewport.New(agentViewportWidth(m.width), agentViewportHeight(m.height))
			m.agentVPReady = true
			m.syncAgentViewport()
			m.view = viewAgentChat
			appendLog("autopilot: started agentic chat with %s", autopilotModel)
			return m, tea.Batch(warmupPollTick(m.agentModelName), agentKeepWarmTickCmd())
		}
		if msg.err != "" {
			m.webServerMsg = "download failed: " + msg.err
			appendLog("web server: gemma4:e2b download failed: %s", msg.err)
			return m, nil
		}
		m.webServerMsg = "gemma4:e2b downloaded — press 'e' to enable now"
		appendLog("web server: gemma4:e2b downloaded")
		return m, nil

	case psFetchMsg:
		m.loading = false
		m.psRows = msg.rows
		m.psErr = msg.err
		if m.psCursor >= len(m.psRows) {
			m.psCursor = 0
		}
		return m, nil

	case killResultMsg:
		m.psKilling = false
		if msg.err != "" {
			m.psKillMsg = fmt.Sprintf("failed to stop %s:\n%s\n%s", msg.name, msg.err, msg.output)
			appendLog("stop failed: %s: %s", msg.name, msg.err)
		} else {
			m.psKillMsg = fmt.Sprintf("%s stopped.", msg.name)
			appendLog("stopped %s (from running models)", msg.name)
		}
		return m, nil

	case deviceInfoMsg:
		m.deviceLoading = false
		info := msg.info
		m.deviceInfo = &info
		_ = saveDeviceCache(info)
		return m, nil

	case benchStartMsg:
		m.benchCmd = msg.cmd
		m.benchCh = msg.ch
		return m, waitForBenchMsg(msg.ch)

	case benchLoadDoneMsg:
		m.benchCmd = nil
		if m.benchCancelling {
			_ = saveCache(m.scanRows)
			appendLog("benchmark cancelled after %d/%d model(s)", m.benchIndex, m.benchTotal)
			m.benchRunning = false
			m.benchCancelling = false
			m.benchDoneMsg = fmt.Sprintf(
				"benchmark cancelled after %d/%d model(s). partial results saved.",
				m.benchIndex, m.benchTotal,
			)
			return m, nil
		}
		m.benchElapsed = msg.elapsed
		return m, measureAndUnload(msg.index, m.scanRows[msg.index].Name)

	case benchMeasuredMsg:
		row := &m.scanRows[msg.index]
		row.CPUGPU = msg.cpuGpu
		row.LoadSecs = m.benchElapsed
		row.Benchmarked = true
		row.MatchScore = computeMatchScore(msg.cpuGpu, m.benchElapsed)
		_ = saveCache(m.scanRows)
		appendLog("benchmarked %s: %s, match %d%%, %.1fs", row.Name, formatCPUGPU(row.CPUGPU), row.MatchScore, row.LoadSecs)

		m.benchIndex++
		if m.benchIndex >= m.benchTotal {
			m.benchRunning = false
			m.benchDoneMsg = fmt.Sprintf("benchmark complete: %d model(s) measured.", m.benchTotal)
			appendLog("benchmark complete: %d model(s) measured", m.benchTotal)
			return m, nil
		}
		next := m.benchIndex
		return m, startBenchLoad(next, m.scanRows[next].Name)

	case downloadChanMsg:
		m.downloadCh = msg.ch
		m.downloadCmd = msg.cmd
		return m, waitForDownloadMsg(msg.ch)

	case downloadLineMsg:
		m.downloadLine = msg.line
		if msg.pct >= 0 {
			m.downloadPct = msg.pct
		}
		return m, waitForDownloadMsg(m.downloadCh)

	case downloadDoneMsg:
		m.downloading = false
		name := m.downloadTarget
		switch {
		case m.downloadAborting:
			m.downloadMsg = fmt.Sprintf("download of %s aborted.", name)
			appendLog("download aborted: %s", name)
		case msg.err != nil:
			m.downloadMsg = fmt.Sprintf("download failed for %s:\n%s", name, msg.err.Error())
			appendLog("download failed: %s: %s", name, msg.err.Error())
		default:
			m.downloadMsg = fmt.Sprintf("%s downloaded successfully.", name)
			m.installedDirty = true
			appendLog("downloaded %s", name)
		}
		m.downloadCh = nil
		m.downloadCmd = nil
		return m, nil

	case showRmMsg:
		m.scanBusy = false
		if msg.err != "" {
			m.scanActionMsg = fmt.Sprintf("failed to remove %s:\n%s\n%s", msg.name, msg.err, msg.output)
			appendLog("remove failed: %s: %s", msg.name, msg.err)
			return m, nil
		}
		for i, r := range m.scanRows {
			if r.Name == msg.name {
				m.scanRows = append(m.scanRows[:i], m.scanRows[i+1:]...)
				break
			}
		}
		if m.scanCursor >= len(m.scanRows) {
			m.scanCursor = len(m.scanRows) - 1
			if m.scanCursor < 0 {
				m.scanCursor = 0
			}
		}
		_ = saveCache(m.scanRows)
		m.scanActionMsg = fmt.Sprintf("%s removed.", msg.name)
		appendLog("removed %s (from show model info)", msg.name)
		return m, nil

	case showStopMsg:
		m.scanBusy = false
		if msg.err != "" {
			m.scanActionMsg = fmt.Sprintf("failed to stop %s:\n%s\n%s", msg.name, msg.err, msg.output)
			appendLog("stop failed: %s: %s", msg.name, msg.err)
		} else {
			m.scanActionMsg = fmt.Sprintf("%s stopped.", msg.name)
			appendLog("stopped %s (from show model info)", msg.name)
		}
		return m, nil

	case showRunDoneMsg:
		m.scanBusy = false
		if msg.err != nil {
			m.scanActionMsg = fmt.Sprintf("chat session with %s ended: %v", msg.name, msg.err)
		} else {
			m.scanActionMsg = fmt.Sprintf("chat session with %s ended.", msg.name)
		}
		appendLog("ran %s interactively", msg.name)
		return m, nil

	case scanStartMsg:
		if msg.err != "" {
			m.loading = false
			m.scanErr = msg.err
			return m, nil
		}
		m.scanRows = msg.rows
		m.scanTotal = len(msg.rows)
		m.scanDone = 0
		if m.scanTotal == 0 {
			m.loading = false
			return m, nil
		}
		return m, scanOne(0, m.scanRows[0])

	case scanStepMsg:
		m.scanRows[msg.index] = msg.row
		m.scanDone++
		if m.scanDone >= m.scanTotal {
			m.loading = false
			m.view = viewShowTable
			_ = saveCache(m.scanRows)
			return m, nil
		}
		next := msg.index + 1
		return m, scanOne(next, m.scanRows[next])

	case updateCheckResultMsg:
		m.updateChecked = true
		m.updateCheckErr = msg.err
		m.updateLatest = msg.latestVersion
		m.updateAssetURL = msg.assetURL
		if msg.err == "" && isNewerVersion(appVersion, msg.latestVersion) {
			m.updateAvailable = true
			appendLog("update available: %s (current %s)", msg.latestVersion, appVersion)
		}
		return m, nil

	case updateApplyResultMsg:
		m.updateDownloading = false
		if msg.err != "" {
			m.updateResultErr = msg.err
			appendLog("update failed: %s", msg.err)
		} else {
			m.updateResult = msg.message
			m.updateAvailable = false
			appendLog("update applied: %s", m.updateLatest)
		}
		return m, nil

	case emailTestResultMsg:
		m.emailSending = false
		if msg.err != "" {
			m.emailMsg = redStyle.Render("saved, but the test email failed: " + msg.err)
			appendLog("email: test send failed: %s", msg.err)
		} else {
			m.emailMsg = helpKeyStyle.Render("saved — test email sent, check your inbox")
			appendLog("email: test send succeeded")
		}
		m.emailAddrInput = ""
		m.emailPassInput = ""
		if msg.err == "" {
			// A successful save+test drops back to the greyed-out
			// configured summary — no reason to keep showing raw editable
			// fields once it's confirmed working.
			m.emailEditing = false
		}
		return m, nil

	case ollamaInstallResultMsg:
		m.ollamaInstallRunning = false
		if m.view == viewAutopilot {
			// Windows/macOS have no scriptable silent installer (see
			// installOllama) — this only opened the download page. Can't
			// proceed to the model pull automatically until the user
			// finishes that install and relaunches.
			m.autopilotPhase = "waiting_for_manual_install"
			m.autopilotMsg = strings.TrimSpace(msg.output)
			if msg.err != "" {
				m.autopilotMsg = strings.TrimSpace(msg.output + " " + msg.err)
			}
			appendLog("autopilot: ollama install step: %s", m.autopilotMsg)
			return m, nil
		}
		if m.view == viewWizard {
			m.ollama = checkOllama()
			if msg.err != "" {
				m.wizardLog = append(m.wizardLog, "install ollama: FAILED — "+strings.TrimSpace(msg.output+" "+msg.err))
				appendLog("wizard: ollama install failed: %s", msg.err)
			} else {
				m.wizardLog = append(m.wizardLog, "install ollama: done")
				appendLog("wizard: ollama installed")
			}
			return advanceWizard(m)
		}
		if msg.err != "" {
			m.ollamaInstallErr = strings.TrimSpace(msg.output + "\n" + msg.err)
			appendLog("ollama install failed: %s", msg.err)
		} else {
			m.ollamaInstallResult = strings.TrimSpace(msg.output)
			appendLog("ollama install finished")
		}
		return m, nil

	case wizardPullChanMsg:
		m.wizardPullCh = msg.ch
		m.wizardPullCmd = msg.cmd
		m.wizardPullPct = 0
		m.wizardPullLine = ""
		return m, waitForWizardPullMsg(m.wizardPullCh)

	case wizardPullLineMsg:
		m.wizardPullLine = msg.line
		if msg.pct >= 0 {
			m.wizardPullPct = msg.pct
		}
		return m, waitForWizardPullMsg(m.wizardPullCh)

	case wizardPullDoneMsg:
		status := "done"
		if m.wizardCancelled {
			status = "aborted"
		} else if msg.err != nil {
			status = "FAILED — " + msg.err.Error()
		}
		m.wizardLog = append(m.wizardLog, fmt.Sprintf("download %s: %s", msg.model, status))
		appendLog("wizard: %s pull %s", msg.model, status)
		m.wizardPullCh = nil
		m.wizardPullCmd = nil
		if m.wizardCancelled {
			m.wizardPhase = "done"
			return m, nil
		}
		return advanceWizard(m)

	case agentWarmupMsg:
		if msg.err != "" {
			m.agentWarmup = "error: " + msg.err
			appendLog("model warmup check failed: %s", msg.err)
			m.syncAgentViewport()
			return m, nil
		}
		if msg.ready {
			m.agentWarmup = "ready"
			appendLog("model warmup complete: %s", m.agentModelName)
			m.syncAgentViewport()
			return m, nil
		}
		// Not loaded yet — keep polling as long as this same chat/model is
		// still the active one, so leaving the chat or switching models
		// doesn't leave a stray poll loop running forever.
		if m.view == viewAgentChat && m.agentWarmup == "pending" {
			return m, warmupPollTick(m.agentModelName)
		}
		return m, nil

	case clipboardPasteMsg:
		m.agentPasting = false
		switch {
		case msg.err != "":
			m.agentErr = "clipboard paste failed: " + msg.err
		case msg.imagePath != "":
			if m.agentInput != "" && !strings.HasSuffix(m.agentInput, " ") {
				m.agentInput += " "
			}
			m.agentInput += msg.imagePath
			m.agentPasteNotice = "pasted image: " + filepath.Base(msg.imagePath)
			appendLog("pasted image from clipboard: %s", msg.imagePath)
		case msg.text != "":
			m.agentInput += msg.text
			m.agentPasteNotice = "pasted clipboard text (" + strconv.Itoa(len(msg.text)) + " chars)"
			appendLog("pasted clipboard text (%d chars)", len(msg.text))
		default:
			m.agentPasteNotice = "clipboard is empty"
		}
		return m, nil

	case agentStreamDeltaMsg:
		// A real token streaming in is the strongest possible proof the
		// model is loaded and answering — overrides a stale "pending" from
		// the ollama-ps poll (e.g. a name-format mismatch that never
		// matches) so the status line can't say "loading" forever while
		// answers are visibly arriving. Only resync the viewport on the
		// transition into "ready", not on every single token.
		if m.agentWarmup != "ready" {
			m.agentWarmup = "ready"
			m.syncAgentViewport()
		}
		m.agentStreamBuf += msg.delta
		return m, waitForAgentStream(msg.ch)

	case agentStepMsg:
		m.agentWarmup = "ready" // a completed tool round proves the model answered
		m.agentMessages = msg.messages
		m.agentStreamBuf = ""
		m.agentToolsSupported = msg.toolsSupported
		m.syncAgentViewport()
		return m, waitForAgentStream(msg.ch)

	case agentTurnDoneMsg:
		m.agentBusy = false
		// Only record a time if this turn actually produced a rendered
		// reply — buildAgentChatLines only advances its own counter for a
		// content-bearing assistant message, so a timed-but-unrendered
		// turn (e.g. an error with no reply) would misalign every
		// subsequent turn's time against the wrong displayed bubble.
		if lastMsg := lastAssistantContent(msg.messages); lastMsg != "" {
			m.agentTurnTimes = append(m.agentTurnTimes, time.Since(m.agentStarted))
		}
		if msg.err == "" {
			m.agentWarmup = "ready"
		}
		m.agentMessages = msg.messages
		// A turn error always wins. A clean turn leaves any warning already
		// set this turn (e.g. "image not attached, file not found") alone
		// instead of wiping it the instant the reply finishes streaming.
		if msg.err != "" {
			m.agentErr = msg.err
		}
		m.agentStreamBuf = ""
		if m.agentToolsSupported && !msg.toolsSupported {
			appendLog("%s does not support tool calling — continuing as plain chat (no file access)", m.agentModelName)
		}
		m.agentToolsSupported = msg.toolsSupported
		m.syncAgentViewport()
		return m, nil

	case agentTickMsg:
		if !m.agentBusy && m.agentWarmup != "pending" {
			return m, nil
		}
		m.agentSpinner++
		return m, agentTickCmd()

	case agentKeepWarmTickMsg:
		if m.view != viewAgentChat {
			// Left the chat — don't reschedule, let this loop end
			// naturally instead of ticking forever in the background.
			return m, nil
		}
		next := agentKeepWarmTickCmd()
		if m.agentBusy {
			// Don't run a competing `ollama ps`/`ollama run` against a
			// turn that's already in flight; just check again next cycle.
			return m, next
		}
		return m, tea.Batch(next, checkAndRewarmModel(m.agentModelName))

	case agentRewarmCheckedMsg:
		if !msg.wasLoaded {
			appendLog("keep-warm: %s was unloaded, reloading in the background", msg.modelName)
		}
		return m, nil

	case cubeTickMsg:
		m.cubeAngle += 0.12
		if m.cubeAngle > 1000 {
			m.cubeAngle -= 1000 // keep the float from growing unbounded over a long-running session
		}
		return m, cubeTickCmd()

	case tea.KeyMsg:
		key := msg.String()

		switch m.view {
		case viewMenu:
			switch key {
			case "ctrl+c", "esc":
				appendLog("quit")
				return m, tea.Quit
			case "up", "k":
				m.menuCursor--
				if m.menuCursor < 0 {
					m.menuCursor = len(menuItems) - 1
				}
				return m, nil
			case "down", "j":
				m.menuCursor++
				if m.menuCursor >= len(menuItems) {
					m.menuCursor = 0
				}
				return m, nil
			case "enter":
				return m.enterMenu(menuItems[m.menuCursor].key)
			default:
				for _, it := range menuItems {
					if key == it.key {
						return m.enterMenu(it.key)
					}
				}
			}
			return m, nil

		case viewShowTable:
			if key == "ctrl+c" {
				appendLog("quit")
				return m, tea.Quit
			}

			if m.scanActionMsg != "" {
				m.scanActionMsg = ""
				return m, nil
			}

			if m.scanBusy {
				return m, nil
			}

			if m.scanAction {
				name := m.scanRows[m.scanCursor].Name
				choose := func(idx int) (tea.Model, tea.Cmd) {
					m.scanAction = false
					switch idx {
					case 0:
						wd, err := os.Getwd()
						if err != nil {
							wd = "."
						}
						m.agentModelName = name
						m.agentWorkDir = wd
						m.agentReturnView = viewShowTable
						m.agentCapabilities = m.scanRows[m.scanCursor].Capabilities
						// Known-unsupported skips the doomed first request
						// entirely; unknown ("-", stale/no cache) stays
						// optimistic and relies on the runtime fallback in
						// runAgentTurn if the model rejects the tools list.
						m.agentToolsSupported = m.agentCapabilities == "" || m.agentCapabilities == "-" ||
							strings.Contains(m.agentCapabilities, "too")
						m.agentToolMode = "auto"
						m.agentWarmup = "pending"
						m.agentMessages = []ollamaChatMsg{
							{Role: "system", Content: agentSystemPrompt(wd)},
						}
						m.agentInput = ""
						m.agentErr = ""
						m.agentBusy = false
						m.agentStarted = time.Now()
						m.agentWarmupStarted = time.Now()
						m.agentTurnTimes = nil
						m.agentViewport = viewport.New(agentViewportWidth(m.width), agentViewportHeight(m.height))
						m.agentVPReady = true
						m.syncAgentViewport()
						m.view = viewAgentChat
						appendLog("started agentic chat with %s", name)
						return m, tea.Batch(warmupPollOllama(name), agentTickCmd(), agentKeepWarmTickCmd())
					case 1:
						m.scanBusy = true
						m.scanBusyLabel = fmt.Sprintf("running %s...", name)
						appendLog("running %s (from show model info)", name)
						return m, runModelInteractive(name)
					case 2:
						m.scanBusy = true
						m.scanBusyLabel = fmt.Sprintf("removing %s...", name)
						appendLog("removing %s (from show model info)", name)
						return m, removeModel(name)
					case 3:
						m.scanBusy = true
						m.scanBusyLabel = fmt.Sprintf("stopping %s...", name)
						appendLog("stopping %s (from show model info)", name)
						return m, stopModel(name)
					}
					return m, nil
				}
				switch key {
				case "up":
					m.scanActionSel--
					if m.scanActionSel < 0 {
						m.scanActionSel = 3
					}
				case "down":
					m.scanActionSel++
					if m.scanActionSel > 3 {
						m.scanActionSel = 0
					}
				case "esc", "n":
					m.scanAction = false
				case "enter":
					return choose(m.scanActionSel)
				case "a":
					return choose(0)
				case "x":
					return choose(1)
				case "r":
					return choose(2)
				case "k":
					return choose(3)
				}
				return m, nil
			}

			switch key {
			case "q":
				appendLog("quit")
				return m, tea.Quit
			case "esc":
				appendLog("back to main menu")
				m.view = viewMenu
				return m, nil
			case "alt+h":
				appendLog("opened help")
				m.view = viewHelpText
				m.helpScroll = 0
				return m, nil
			case "up":
				if len(m.scanRows) > 0 {
					m.scanCursor--
					if m.scanCursor < 0 {
						m.scanCursor = len(m.scanRows) - 1
					}
				}
				return m, nil
			case "down":
				if len(m.scanRows) > 0 {
					m.scanCursor++
					if m.scanCursor >= len(m.scanRows) {
						m.scanCursor = 0
					}
				}
				return m, nil
			case "r":
				return m.rescan()
			case "enter":
				if len(m.scanRows) == 0 {
					return m, nil
				}
				m.scanAction = true
				m.scanActionSel = 0
				return m, nil
			}
			return m, nil

		case viewAgentChat:
			if key == "ctrl+c" {
				appendLog("quit")
				return m, tea.Quit
			}
			// Navigation works even while the model is thinking, so history
			// can be reviewed without waiting. Gated to these exact key
			// names before ever touching viewport.Update, so its own
			// letter-key aliases (e.g. vim-style j/k) can never swallow a
			// character the user is trying to type into agentInput.
			switch key {
			case "up", "down", "pgup", "pgdown", "home", "end":
				var cmd tea.Cmd
				m.agentViewport, cmd = m.agentViewport.Update(msg)
				return m, cmd
			case "alt+t":
				appendLog("opened tool categories")
				m.view = viewToolCategories
				m.toolCatCursor = 0
				m.toolDetailOpen = false
				m.helpScroll = 0
				return m, nil
			case "alt+h":
				appendLog("opened agent help")
				m.view = viewAgentHelp
				return m, nil
			case "alt+m":
				switch m.agentToolMode {
				case "on":
					m.agentToolMode = "off"
				case "off":
					m.agentToolMode = "auto"
				default:
					m.agentToolMode = "on"
				}
				m.agentPasteNotice = "tool mode: " + m.agentToolMode
				appendLog("tool mode set to %s", m.agentToolMode)
				return m, nil
			}
			if m.agentBusy {
				return m, nil
			}
			switch key {
			case "esc":
				appendLog("exited agentic chat with %s", m.agentModelName)
				m.view = m.agentReturnView
				return m, nil
			case "enter":
				text := strings.TrimSpace(m.agentInput)
				if text == "" {
					return m, nil
				}
				imagePaths, missingPaths := extractImagePaths(text)
				m.agentMessages = append(m.agentMessages, ollamaChatMsg{
					Role:    "user",
					Content: text,
					Images:  loadImagesBase64(imagePaths),
				})
				m.agentInput = ""
				m.agentErr = ""
				m.agentPasteNotice = ""
				if len(missingPaths) > 0 {
					m.agentErr = fmt.Sprintf("image not attached, file not found: %s (drag-and-drop managers like Ditto sometimes show a path that isn't a real file — use Alt+V to paste the image itself instead)", strings.Join(missingPaths, ", "))
				}
				m.agentBusy = true
				m.agentStreamBuf = ""
				m.agentStarted = time.Now()
				m.syncAgentViewport()
				if len(imagePaths) > 0 {
					appendLog("agent chat message: %s (+%d image(s))", truncateName(text, 80), len(imagePaths))
				} else if len(missingPaths) > 0 {
					appendLog("agent chat message: %s (image path not found: %s)", truncateName(text, 80), strings.Join(missingPaths, ", "))
				} else {
					appendLog("agent chat message: %s", truncateName(text, 80))
				}
				// "auto" (the default) skips tools on any turn that attaches an
				// image: Ollama's tool-calling chat template garbles image
				// tokens for at least gemma4:e2b (confirmed directly against
				// /api/chat — the same image decodes fine without `tools` in
				// the request, and comes back "unreadable/corrupted" with it),
				// so a turn can have working vision or working tools, not
				// reliably both. "on" always tries tools anyway (the user's
				// explicit override); "off" never does. This only suppresses
				// tools for THIS turn — it never touches agentToolsSupported,
				// so the model's real tool-calling capability is remembered
				// correctly for the next image-free message.
				suppressTools := m.agentToolMode == "off" ||
					(m.agentToolMode == "auto" && len(imagePaths) > 0)
				return m, tea.Batch(runAgentTurn(m.agentModelName, m.agentMessages, m.agentWorkDir, m.agentToolsSupported, suppressTools), agentTickCmd())
			case "backspace":
				if len(m.agentInput) > 0 {
					r := []rune(m.agentInput)
					m.agentInput = string(r[:len(r)-1])
				}
				return m, nil
			case "alt+v":
				m.agentPasting = true
				m.agentPasteNotice = ""
				return m, pasteFromClipboard()
			default:
				if key == "space" {
					m.agentInput += " "
				} else if r, size := utf8.DecodeRuneInString(key); size == len(key) && size > 0 && unicode.IsPrint(r) {
					// size == len(key) rules out multi-key names like
					// "ctrl+c"/"alt+v" (which aren't a single valid rune)
					// while still accepting any printable Unicode
					// character, not just single-byte ASCII — otherwise
					// Hebrew/Arabic/CJK/etc. input gets silently dropped.
					m.agentInput += key
				}
				return m, nil
			}

		case viewToolCategories:
			switch key {
			case "ctrl+c":
				appendLog("quit")
				return m, tea.Quit
			case "esc":
				if m.toolDetailOpen {
					m.toolDetailOpen = false
					return m, nil
				}
				m.view = viewAgentChat
				return m, nil
			case "alt+t":
				m.view = viewAgentChat
				return m, nil
			case "enter":
				if !m.toolDetailOpen {
					m.toolDetailOpen = true
					appendLog("viewed tool detail: %s", m.selectedToolName())
				}
				return m, nil
			case "tab":
				if m.toolDetailOpen {
					return m, nil
				}
				names := flatToolNames()
				m.toolCatCursor = (m.toolCatCursor + 1) % len(names)
				m.ensureToolCursorVisible()
				return m, nil
			case "shift+tab":
				if m.toolDetailOpen {
					return m, nil
				}
				names := flatToolNames()
				m.toolCatCursor = (m.toolCatCursor - 1 + len(names)) % len(names)
				m.ensureToolCursorVisible()
				return m, nil
			case "up", "k":
				if m.toolDetailOpen {
					return m, nil
				}
				m.helpScroll--
				m.clampHelpScroll()
				return m, nil
			case "down", "j":
				if m.toolDetailOpen {
					return m, nil
				}
				m.helpScroll++
				m.clampHelpScroll()
				return m, nil
			case "pgup":
				if m.toolDetailOpen {
					return m, nil
				}
				m.helpScroll -= helpPageLines(m.height)
				m.clampHelpScroll()
				return m, nil
			case "pgdown":
				if m.toolDetailOpen {
					return m, nil
				}
				m.helpScroll += helpPageLines(m.height)
				m.clampHelpScroll()
				return m, nil
			case "home":
				if m.toolDetailOpen {
					return m, nil
				}
				m.helpScroll = 0
				return m, nil
			case "end":
				if m.toolDetailOpen {
					return m, nil
				}
				m.helpScroll = 1 << 30
				m.clampHelpScroll()
				return m, nil
			}
			return m, nil

		case viewAgentHelp:
			switch key {
			case "ctrl+c":
				appendLog("quit")
				return m, tea.Quit
			case "esc", "alt+h":
				m.view = viewAgentChat
				return m, nil
			}
			return m, nil

		case viewShowScan:
			switch key {
			case "ctrl+c":
				appendLog("quit")
				return m, tea.Quit
			case "esc":
				m.view = viewMenu
				return m, nil
			}
			return m, nil

		case viewDeviceInfo:
			if key == "ctrl+c" {
				appendLog("quit")
				return m, tea.Quit
			}

			if m.benchDoneMsg != "" {
				m.benchDoneMsg = ""
				return m, nil
			}

			if m.benchRunning {
				if key == "c" && m.benchCmd != nil && m.benchCmd.Process != nil && !m.benchCancelling {
					m.benchCancelling = true
					appendLog("benchmark cancel requested")
					_ = m.benchCmd.Process.Kill()
				}
				return m, nil
			}

			if m.benchConfirm {
				switch key {
				case "y", "enter":
					m.benchConfirm = false
					m.benchRunning = true
					m.benchIndex = 0
					m.benchTotal = len(m.scanRows)
					m.benchCancelling = false
					appendLog("benchmark started: %d model(s)", m.benchTotal)
					return m, startBenchLoad(0, m.scanRows[0].Name)
				case "n", "esc":
					m.benchConfirm = false
				}
				return m, nil
			}

			switch key {
			case "q":
				appendLog("quit")
				return m, tea.Quit
			case "esc":
				appendLog("back to main menu")
				m.view = viewMenu
				return m, nil
			case "r":
				if !m.deviceLoading {
					m.deviceLoading = true
					m.deviceInfo = nil
					appendLog("rescanned device info (ignoring cache)")
					return m, gatherDeviceInfo
				}
			case "b":
				if len(m.scanRows) == 0 {
					if rows, ok := loadCache(); ok && len(rows) > 0 {
						m.scanRows = rows
					} else if msg, ok := fetchModelList()().(scanStartMsg); ok && msg.err == "" {
						m.scanRows = msg.rows
					}
				}
				if len(m.scanRows) == 0 {
					m.benchDoneMsg = "no installed models found to benchmark."
					return m, nil
				}
				sort.Slice(m.scanRows, func(i, j int) bool {
					return parseSizeBytes(m.scanRows[i].Size) < parseSizeBytes(m.scanRows[j].Size)
				})
				m.benchConfirm = true
			}
			return m, nil

		case viewFirstRunDisclaimer:
			switch key {
			case "a", "enter":
				markDisclaimerAccepted()
				m = m.enterWizard(true)
				appendLog("first run: disclaimer accepted, opening setup wizard")
			case "q", "esc", "ctrl+c":
				clearDisclaimerAccepted()
				appendLog("quit")
				return m, tea.Quit
			}
			return m, nil

		case viewOllamaInstallPrompt:
			if m.ollamaInstallRunning {
				return m, nil
			}
			if m.ollamaInstallResult != "" || m.ollamaInstallErr != "" {
				// Any key dismisses the result and moves on.
				m.ollamaInstallResult = ""
				m.ollamaInstallErr = ""
				m.ollama = checkOllama()
				m.view = viewMenu
				return m, nil
			}
			switch key {
			case "y", "enter":
				m.ollamaInstallRunning = true
				appendLog("installing ollama")
				return m, installOllama()
			case "n", "esc", "q":
				appendLog("skipped ollama install")
				m.view = viewMenu
			case "ctrl+c":
				appendLog("quit")
				return m, tea.Quit
			}
			return m, nil

		case viewHelpMenu:
			switch key {
			case "ctrl+c":
				appendLog("quit")
				return m, tea.Quit
			case "esc":
				m.view = viewMenu
				return m, nil
			case "up", "k":
				m.helpMenuCursor--
				if m.helpMenuCursor < 0 {
					m.helpMenuCursor = len(helpMenuItems) - 1
				}
			case "down", "j":
				m.helpMenuCursor++
				if m.helpMenuCursor >= len(helpMenuItems) {
					m.helpMenuCursor = 0
				}
			case "enter":
				it := helpMenuItems[m.helpMenuCursor]
				if it.dest == viewWizard {
					m = m.enterWizard(false)
				} else {
					m.view = it.dest
					m.helpScroll = 0
					if it.dest == viewEmailSettings {
						m.emailEditing = loadEmailConfig().FromAddress == ""
					}
				}
				appendLog("opened %s", it.label)
			default:
				for _, it := range helpMenuItems {
					if key == it.key {
						if it.dest == viewWizard {
							m = m.enterWizard(false)
						} else {
							m.view = it.dest
							m.helpScroll = 0
							if it.dest == viewEmailSettings {
								m.emailEditing = loadEmailConfig().FromAddress == ""
							}
						}
						appendLog("opened %s", it.label)
					}
				}
			}
			return m, nil

		case viewTavilySettings:
			if os.Getenv("TAVILY_API_KEY") != "" && !m.tavilyEditing {
				// Configured summary — only reconfigure/back apply, same
				// pattern as the email settings screen.
				switch key {
				case "ctrl+c":
					appendLog("quit")
					return m, tea.Quit
				case "esc":
					m = m.advanceWizardChain()
					m.tavilyKeyMsg = ""
					return m, nil
				case "r":
					m.tavilyEditing = true
					m.tavilyKeyInput = ""
					m.tavilyKeyMsg = ""
					appendLog("tavily: reconfiguring")
				}
				return m, nil
			}
			switch key {
			case "ctrl+c":
				appendLog("quit")
				return m, tea.Quit
			case "esc":
				m = m.advanceWizardChain()
				m.tavilyKeyInput = ""
				m.tavilyKeyMsg = ""
				m.tavilyEditing = false
				return m, nil
			case "enter":
				trimmed := strings.TrimSpace(m.tavilyKeyInput)
				if trimmed == "" {
					m.tavilyKeyMsg = "type a key first, or Esc to cancel"
					return m, nil
				}
				if err := saveTavilyKey(trimmed); err != nil {
					m.tavilyKeyMsg = "error saving key: " + err.Error()
					m.tavilyKeyInput = ""
					return m, nil
				}
				m.tavilyKeyMsg = "saved — tavily_search/tavily_extract are ready to use"
				appendLog("tavily API key saved")
				m.tavilyKeyInput = ""
				m.tavilyEditing = false
				if len(m.wizardPendingSetups) > 0 {
					m = m.advanceWizardChain()
				}
				return m, nil
			case "backspace":
				if len(m.tavilyKeyInput) > 0 {
					m.tavilyKeyInput = m.tavilyKeyInput[:len(m.tavilyKeyInput)-1]
				}
				return m, nil
			case "up":
				m.helpScroll--
				m.clampHelpScroll()
				return m, nil
			case "down":
				m.helpScroll++
				m.clampHelpScroll()
				return m, nil
			case "pgup":
				m.helpScroll -= helpPageLines(m.height)
				m.clampHelpScroll()
				return m, nil
			case "pgdown":
				m.helpScroll += helpPageLines(m.height)
				m.clampHelpScroll()
				return m, nil
			case "home":
				m.helpScroll = 0
				return m, nil
			case "end":
				m.helpScroll = 1 << 30
				m.clampHelpScroll()
				return m, nil
			default:
				// Reject anything that isn't one printable rune — a fast
				// paste burst on some Windows terminals can occasionally
				// misfire an extra control keystroke (e.g. a stray NUL),
				// and os.Setenv hard-errors ("invalid argument") if that
				// ends up embedded in the value.
				if r, size := utf8.DecodeRuneInString(key); size == len(key) && size > 0 && unicode.IsPrint(r) {
					m.tavilyKeyInput += key
				}
				return m, nil
			}

		case viewTelegramSettings:
			if loadTelegramConfig().Token != "" && !m.telegramEditing {
				switch key {
				case "ctrl+c":
					appendLog("quit")
					return m, tea.Quit
				case "esc":
					m = m.advanceWizardChain()
					m.telegramMsg = ""
					return m, nil
				case "r":
					m.telegramEditing = true
					m.telegramTokenInput = ""
					m.telegramMsg = ""
					appendLog("telegram: reconfiguring")
				}
				return m, nil
			}
			switch key {
			case "ctrl+c":
				appendLog("quit")
				return m, tea.Quit
			case "esc":
				m = m.advanceWizardChain()
				m.telegramTokenInput = ""
				m.telegramMsg = ""
				m.telegramEditing = false
				return m, nil
			case "enter":
				trimmed := strings.TrimSpace(m.telegramTokenInput)
				if trimmed == "" {
					stopTelegramBot()
					cfg := loadTelegramConfig()
					cfg.Token = ""
					_ = saveTelegramConfig(cfg)
					m.telegramMsg = "disabled"
					m.telegramTokenInput = ""
					m.telegramEditing = false
					return m, nil
				}
				names, err := listInstalledModelNames()
				if err != nil || len(names) == 0 {
					m.telegramMsg = "no local model installed — run the setup wizard ([h] help/settings -> [w]) to download one first"
					return m, nil
				}
				existing := loadTelegramConfig()
				model := names[pickDefaultModelIndex(names, existing.Model)]
				cfg := telegramConfig{Token: trimmed, Model: model, ChatID: existing.ChatID}
				if existing.Token != trimmed {
					cfg.ChatID = 0 // a new token is a new bot identity — don't carry over the old binding
				}
				if err := saveTelegramConfig(cfg); err != nil {
					m.telegramMsg = "error saving token: " + err.Error()
					return m, nil
				}
				stopTelegramBot()
				wd, err := os.Getwd()
				if err != nil {
					wd = "."
				}
				startTelegramBot(cfg, wd)
				m.telegramMsg = "saved — bot is running, model " + model + ". Message it on Telegram to bind this chat."
				m.telegramTokenInput = ""
				m.telegramEditing = false
				appendLog("telegram bot token saved")
				if len(m.wizardPendingSetups) > 0 {
					m = m.advanceWizardChain()
				}
				return m, nil
			case "backspace":
				if len(m.telegramTokenInput) > 0 {
					m.telegramTokenInput = m.telegramTokenInput[:len(m.telegramTokenInput)-1]
				}
				return m, nil
			case "up":
				m.helpScroll--
				m.clampHelpScroll()
				return m, nil
			case "down":
				m.helpScroll++
				m.clampHelpScroll()
				return m, nil
			case "pgup":
				m.helpScroll -= helpPageLines(m.height)
				m.clampHelpScroll()
				return m, nil
			case "pgdown":
				m.helpScroll += helpPageLines(m.height)
				m.clampHelpScroll()
				return m, nil
			case "home":
				m.helpScroll = 0
				return m, nil
			case "end":
				m.helpScroll = 1 << 30
				m.clampHelpScroll()
				return m, nil
			default:
				if r, size := utf8.DecodeRuneInString(key); size == len(key) && size > 0 && unicode.IsPrint(r) {
					m.telegramTokenInput += key
				}
				return m, nil
			}

		case viewEmailSettings:
			if m.emailSending {
				return m, nil
			}
			if loadEmailConfig().FromAddress != "" && !m.emailEditing {
				// Configured summary screen — only reconfigure/back apply,
				// there are no live fields to Tab between here.
				switch key {
				case "ctrl+c":
					appendLog("quit")
					return m, tea.Quit
				case "esc":
					m = m.advanceWizardChain()
					m.emailMsg = ""
					return m, nil
				case "r":
					cfg := loadEmailConfig()
					m.emailEditing = true
					// Address and display name usually don't change on a
					// reconfigure (only the app password typically does,
					// e.g. after revoking/regenerating it) — pre-fill those
					// two so the user isn't forced to retype them.
					m.emailAddrInput = cfg.FromAddress
					m.emailDisplayNameInput = cfg.DisplayName
					m.emailPassInput = ""
					m.emailFieldFocus = 2
					m.emailMsg = ""
					appendLog("email: reconfiguring")
				}
				return m, nil
			}
			switch key {
			case "ctrl+c":
				appendLog("quit")
				return m, tea.Quit
			case "esc":
				m = m.advanceWizardChain()
				m.emailAddrInput = ""
				m.emailDisplayNameInput = ""
				m.emailPassInput = ""
				m.emailMsg = ""
				m.emailFieldFocus = 0
				m.emailEditing = false
				return m, nil
			case "tab", "down":
				m.emailFieldFocus = (m.emailFieldFocus + 1) % 3
				return m, nil
			case "up":
				m.emailFieldFocus = (m.emailFieldFocus + 2) % 3
				return m, nil
			case "enter":
				// Enter just advances through address -> display name ->
				// password like Tab, rather than trying to submit from an
				// earlier field — only the last field (password) submits.
				if m.emailFieldFocus < 2 {
					m.emailFieldFocus++
					return m, nil
				}
				addr := strings.TrimSpace(m.emailAddrInput)
				displayName := strings.TrimSpace(m.emailDisplayNameInput)
				pass := strings.ReplaceAll(strings.TrimSpace(m.emailPassInput), " ", "")
				if addr == "" || pass == "" {
					m.emailMsg = "both the Gmail address and the app password are required"
					return m, nil
				}
				cfg := emailConfig{FromAddress: addr, AppPassword: pass, DisplayName: displayName}
				if err := saveEmailConfig(cfg); err != nil {
					m.emailMsg = "error saving config: " + err.Error()
					return m, nil
				}
				appendLog("email: config saved for %s, sending test email", addr)
				m.emailSending = true
				m.emailMsg = ""
				return m, sendTestEmail(cfg)
			case "backspace":
				switch m.emailFieldFocus {
				case 0:
					if len(m.emailAddrInput) > 0 {
						m.emailAddrInput = m.emailAddrInput[:len(m.emailAddrInput)-1]
					}
				case 1:
					if len(m.emailDisplayNameInput) > 0 {
						m.emailDisplayNameInput = m.emailDisplayNameInput[:len(m.emailDisplayNameInput)-1]
					}
				default:
					if len(m.emailPassInput) > 0 {
						m.emailPassInput = m.emailPassInput[:len(m.emailPassInput)-1]
					}
				}
				return m, nil
			default:
				if r, size := utf8.DecodeRuneInString(key); size == len(key) && size > 0 && unicode.IsPrint(r) {
					switch m.emailFieldFocus {
					case 0:
						m.emailAddrInput += key
					case 1:
						m.emailDisplayNameInput += key
					default:
						m.emailPassInput += key
					}
				}
				return m, nil
			}

		case viewBackupSettings:
			switch key {
			case "ctrl+c":
				appendLog("quit")
				return m, tea.Quit
			case "esc":
				m.view = viewHelpMenu
				m.backupMode = ""
				m.backupMsg = ""
				return m, nil
			case "e":
				m = m.enterBackupBrowser("export")
			case "i":
				m = m.enterBackupBrowser("import")
			}
			return m, nil

		case viewBackupBrowser:
			switch key {
			case "ctrl+c":
				appendLog("quit")
				return m, tea.Quit
			case "esc":
				m.view = viewBackupSettings
				return m, nil
			case "up":
				if m.backupBrowseEditingName {
					m.backupBrowseEditingName = false
					return m, nil
				}
				if m.backupBrowseCursor > 0 {
					m.backupBrowseCursor--
				}
				return m, nil
			case "down":
				if m.backupBrowseEditingName {
					return m, nil
				}
				if m.backupBrowseCursor < len(m.backupBrowseEntries)-1 {
					m.backupBrowseCursor++
				}
				return m, nil
			case "tab":
				// Export mode only — toggle focus between the directory
				// list and the destination filename field.
				if m.backupMode == "export" {
					m.backupBrowseEditingName = !m.backupBrowseEditingName
				}
				return m, nil
			case "enter":
				if m.backupBrowseEditingName {
					return m.confirmBackupExport()
				}
				if len(m.backupBrowseEntries) == 0 {
					return m, nil
				}
				sel := m.backupBrowseEntries[m.backupBrowseCursor]
				if sel.isDir {
					var next string
					if sel.name == ".." {
						next = filepath.Dir(m.backupBrowseDir)
					} else {
						next = filepath.Join(m.backupBrowseDir, sel.name)
					}
					m.backupBrowseDir = next
					m.backupBrowseCursor = 0
					entries, err := listDirEntries(next)
					if err != nil {
						m.backupMsg = redStyle.Render("can't open " + next + ": " + err.Error())
						m.view = viewBackupSettings
						return m, nil
					}
					m.backupBrowseEntries = entries
					return m, nil
				}
				// Selected an existing .lsb file.
				if m.backupMode == "export" {
					m.backupBrowseFilename = sel.name
					m.backupBrowseEditingName = true
					return m, nil
				}
				return m.confirmBackupImport(filepath.Join(m.backupBrowseDir, sel.name))
			case "backspace":
				if m.backupBrowseEditingName && len(m.backupBrowseFilename) > 0 {
					m.backupBrowseFilename = m.backupBrowseFilename[:len(m.backupBrowseFilename)-1]
				}
				return m, nil
			default:
				if m.backupBrowseEditingName {
					if r, size := utf8.DecodeRuneInString(key); size == len(key) && size > 0 && unicode.IsPrint(r) {
						m.backupBrowseFilename += key
					}
				}
				return m, nil
			}

		case viewAutopilot:
			if key == "ctrl+c" {
				appendLog("quit")
				return m, tea.Quit
			}
			if key == "esc" {
				m.view = viewMenu
				return m, nil
			}
			return m, nil

		case viewWebServerSettings:
			if key == "ctrl+c" {
				appendLog("quit")
				return m, tea.Quit
			}
			if m.webServerAwaitingDL {
				switch key {
				case "y", "enter":
					m.webServerAwaitingDL = false
					m.webServerBusy = true
					m.webServerMsg = "downloading gemma4:e2b... this can take a while depending on your connection"
					appendLog("web server: no local model, downloading gemma4:e2b")
					return m, pullModelForWebServer("gemma4:e2b")
				case "n", "esc":
					m.webServerAwaitingDL = false
					m.webServerMsg = "cancelled — enable again once you have a model installed"
				}
				return m, nil
			}
			if m.webServerBusy {
				return m, nil // busy downloading — ignore keys except ctrl+c above
			}
			if m.webServerEditingPort {
				switch key {
				case "esc":
					m.webServerEditingPort = false
					m.webServerPortInput = ""
				case "enter":
					port, err := strconv.Atoi(strings.TrimSpace(m.webServerPortInput))
					if err != nil || port < 1 || port > 65535 {
						m.webServerMsg = "invalid port — enter a number 1-65535"
					} else {
						cfg := loadWebServerConfig()
						cfg.Port = port
						if err := saveWebServerConfig(cfg); err != nil {
							m.webServerMsg = "error saving config: " + err.Error()
						} else {
							appendLog("web server: port set to %d", port)
							m.webServerMsg = fmt.Sprintf("saved — port %d takes effect next time the server (re)starts", port)
							// Apply immediately if it's already running,
							// rather than leaving the change silently
							// pending until the next manual restart.
							if isWebServerRunning() {
								stopWebServer()
								wd, werr := os.Getwd()
								if werr != nil {
									wd = "."
								}
								if serr := startWebServer(cfg, wd); serr != nil {
									m.webServerMsg = "saved, but restart on the new port failed: " + serr.Error()
								} else {
									m.webServerMsg = fmt.Sprintf("saved — now running on port %d", port)
								}
							}
						}
					}
					m.webServerEditingPort = false
					m.webServerPortInput = ""
				case "backspace":
					if len(m.webServerPortInput) > 0 {
						m.webServerPortInput = m.webServerPortInput[:len(m.webServerPortInput)-1]
					}
				default:
					if r, size := utf8.DecodeRuneInString(key); size == len(key) && size > 0 && unicode.IsDigit(r) {
						m.webServerPortInput += key
					}
				}
				return m, nil
			}
			switch key {
			case "esc":
				m.view = viewHelpMenu
				m.webServerMsg = ""
				return m, nil
			case "e":
				names, err := listInstalledModelNames()
				if err != nil {
					m.webServerMsg = "error listing models: " + err.Error()
					return m, nil
				}
				if len(names) == 0 {
					m.webServerAwaitingDL = true
					m.webServerMsg = ""
					return m, nil
				}
				cfg := loadWebServerConfig()
				m.webServerModelList = names
				m.webServerModelCursor = pickDefaultModelIndex(names, cfg.Model)
				m.view = viewWebServerModelSelect
				return m, nil
			case "d":
				stopWebServer()
				cfg := loadWebServerConfig()
				cfg.Enabled = false
				if err := saveWebServerConfig(cfg); err != nil {
					m.webServerMsg = "error saving config: " + err.Error()
				} else {
					m.webServerMsg = "disabled"
				}
				appendLog("web server disabled")
				return m, nil
			case "p":
				m.webServerEditingPort = true
				m.webServerPortInput = strconv.Itoa(webServerEffectivePort(loadWebServerConfig()))
				m.webServerMsg = ""
				return m, nil
			}
			return m, nil

		case viewWebServerModelSelect:
			if key == "ctrl+c" {
				appendLog("quit")
				return m, tea.Quit
			}
			switch key {
			case "esc":
				m.view = viewWebServerSettings
				return m, nil
			case "up", "k":
				m.webServerModelCursor--
				if m.webServerModelCursor < 0 {
					m.webServerModelCursor = len(m.webServerModelList) - 1
				}
			case "down", "j":
				m.webServerModelCursor++
				if m.webServerModelCursor >= len(m.webServerModelList) {
					m.webServerModelCursor = 0
				}
			case "enter":
				selected := m.webServerModelList[m.webServerModelCursor]
				cfg := loadWebServerConfig()
				if cfg.Token == "" {
					cfg.Token = genWebServerToken()
				}
				cfg.Enabled = true
				cfg.Model = selected
				if err := saveWebServerConfig(cfg); err != nil {
					m.webServerMsg = "error saving config: " + err.Error()
					m.view = viewWebServerSettings
					return m, nil
				}
				wd, err := os.Getwd()
				if err != nil {
					wd = "."
				}
				if err := startWebServer(cfg, wd); err != nil {
					m.webServerMsg = "failed to start server: " + err.Error()
				} else {
					m.webServerMsg = ""
				}
				appendLog("web server enabled with model %s", selected)
				m.view = viewWebServerSettings
				return m, nil
			}
			return m, nil

		case viewWizard:
			switch m.wizardPhase {
			case "ask":
				if key == "ctrl+c" {
					appendLog("quit")
					return m, tea.Quit
				}
				if key == "esc" || key == "q" {
					appendLog("wizard cancelled")
					m.view = viewMenu
					m.wizardPhase = ""
					return m, nil
				}
				q := m.wizardQuestions[m.wizardQIndex]
				switch key {
				case "y", "enter":
					m.wizardAnswers[q.id] = true
				case "n":
					m.wizardAnswers[q.id] = false
					if q.id == "disclaimer" {
						// Declining the disclaimer means the user doesn't
						// accept using this app at all — continuing to any
						// other screen would look like the decline didn't
						// matter, so quit immediately instead. Also clear
						// any existing acceptance marker from an earlier
						// session — otherwise a decline here after a past
						// accept would silently do nothing.
						clearDisclaimerAccepted()
						appendLog("disclaimer declined — quitting")
						return m, tea.Quit
					}
				default:
					return m, nil
				}
				m.wizardQIndex++
				if m.wizardQIndex >= len(m.wizardQuestions) {
					return finishWizardQuestions(m)
				}
				return m, nil

			case "blocked":
				// any key just leaves — nothing to run until there's
				// room, and re-entering the wizard re-asks everything.
				m.view = viewMenu
				m.wizardPhase = ""
				return m, nil

			case "run":
				if key == "ctrl+c" {
					appendLog("quit")
					return m, tea.Quit
				}
				if key == "esc" || key == "a" {
					if m.wizardPullCmd != nil && m.wizardPullCmd.Process != nil {
						_ = m.wizardPullCmd.Process.Kill()
					}
					appendLog("wizard aborted")
					m.wizardCancelled = true
					m.wizardPhase = "done"
					return m, nil
				}
				return m, nil

			case "done":
				var queue []view
				if m.wizardAnswers["tavily"] {
					queue = append(queue, viewTavilySettings)
				}
				if m.wizardAnswers["telegram"] {
					queue = append(queue, viewTelegramSettings)
				}
				if m.wizardAnswers["email"] {
					queue = append(queue, viewEmailSettings)
				}
				// Consume all three now — otherwise revisiting this "done"
				// screen later (e.g. after the chain finishes and the user
				// somehow lands back here) would rebuild the same queue.
				m.wizardAnswers["tavily"] = false
				m.wizardAnswers["telegram"] = false
				m.wizardAnswers["email"] = false
				if len(queue) > 0 {
					m.view = queue[0]
					m.wizardPendingSetups = queue[1:]
					m.wizardInChain = true
					if queue[0] == viewEmailSettings {
						m.emailEditing = loadEmailConfig().FromAddress == ""
					}
					appendLog("wizard: opening %s setup", helpMenuLabelForView(queue[0]))
					return m, nil
				}
				m.view = viewMenu
				m.wizardPhase = ""
				return m, nil
			}
			return m, nil

		case viewHelpText, viewDisclaimerText:
			switch key {
			case "ctrl+c":
				appendLog("quit")
				return m, tea.Quit
			case "esc":
				m.view = viewHelpMenu
				m.helpScroll = 0
				return m, nil
			case "up", "k":
				m.helpScroll--
			case "down", "j":
				m.helpScroll++
			case "pgup":
				m.helpScroll -= helpPageLines(m.height)
			case "pgdown":
				m.helpScroll += helpPageLines(m.height)
			case "home":
				m.helpScroll = 0
			case "end":
				m.helpScroll = 1 << 30 // clamped below
			}
			m.clampHelpScroll()
			return m, nil

		case viewLogText:
			if m.logSearchMode {
				switch key {
				case "ctrl+c":
					appendLog("quit")
					return m, tea.Quit
				case "esc":
					m.logSearchMode = false
					m.logQuery = ""
				case "enter":
					m.logSearchMode = false
				case "backspace":
					if len(m.logQuery) > 0 {
						m.logQuery = m.logQuery[:len(m.logQuery)-1]
					}
				default:
					if r, size := utf8.DecodeRuneInString(key); size == len(key) && size > 0 && unicode.IsPrint(r) {
						m.logQuery += key
					}
				}
				m.helpScroll = 0
				m.clampHelpScroll()
				return m, nil
			}
			switch key {
			case "ctrl+c":
				appendLog("quit")
				return m, tea.Quit
			case "esc":
				if m.logQuery != "" {
					m.logQuery = ""
					m.helpScroll = 0
				} else {
					m.view = viewHelpMenu
					m.helpScroll = 0
				}
				return m, nil
			case "/":
				m.logSearchMode = true
			case "up", "k":
				m.helpScroll--
			case "down", "j":
				m.helpScroll++
			case "pgup":
				m.helpScroll -= helpPageLines(m.height)
			case "pgdown":
				m.helpScroll += helpPageLines(m.height)
			case "home":
				m.helpScroll = 0
			case "end":
				m.helpScroll = 1 << 30 // clamped below
			}
			m.clampHelpScroll()
			return m, nil

		case viewUpdateText:
			if m.updateDownloading {
				return m, nil
			}
			if key == "ctrl+c" {
				appendLog("quit")
				return m, tea.Quit
			}
			if m.autoUpdateEditingTime {
				switch key {
				case "esc":
					m.autoUpdateEditingTime = false
					m.autoUpdateTimeInput = ""
				case "enter":
					hour, minute, ok := parseHHMM(m.autoUpdateTimeInput)
					if !ok {
						m.autoUpdateMsg = "invalid time — use 24h HH:MM, e.g. 03:00 or 22:30"
					} else {
						cfg := loadAutoUpdateConfig()
						cfg.Hour, cfg.Minute = hour, minute
						_ = saveAutoUpdateConfig(cfg)
						appendLog("auto-update: check time set to %02d:%02d", hour, minute)
						m.autoUpdateMsg = fmt.Sprintf("saved — will check daily at %02d:%02d", hour, minute)
					}
					m.autoUpdateEditingTime = false
					m.autoUpdateTimeInput = ""
				case "backspace":
					if len(m.autoUpdateTimeInput) > 0 {
						m.autoUpdateTimeInput = m.autoUpdateTimeInput[:len(m.autoUpdateTimeInput)-1]
					}
				default:
					if r, size := utf8.DecodeRuneInString(key); size == len(key) && size > 0 && unicode.IsPrint(r) {
						m.autoUpdateTimeInput += key
					}
				}
				return m, nil
			}
			if m.updateResult != "" || m.updateResultErr != "" {
				m.updateResult = ""
				m.updateResultErr = ""
				m.view = viewHelpMenu
				return m, nil
			}
			switch key {
			case "esc":
				m.view = viewHelpMenu
				m.autoUpdateMsg = ""
				return m, nil
			case "u", "enter":
				if m.updateAvailable && m.updateAssetURL != "" {
					m.updateDownloading = true
					appendLog("downloading update %s", m.updateLatest)
					return m, applyUpdate(m.updateAssetURL)
				}
				if m.updateAvailable && m.updateAssetURL == "" {
					m.updateResultErr = fmt.Sprintf("no %s release asset found for this platform", updateAssetName(runtime.GOOS, runtime.GOARCH))
				}
			case "r":
				m.updateChecked = false
				m.updateCheckErr = ""
				appendLog("re-checking for update")
				return m, checkForUpdate()
			case "e":
				cfg := loadAutoUpdateConfig()
				cfg.Enabled = true
				_ = saveAutoUpdateConfig(cfg)
				appendLog("auto-update: enabled")
				m.autoUpdateMsg = "auto-update enabled"
			case "d":
				cfg := loadAutoUpdateConfig()
				cfg.Enabled = false
				_ = saveAutoUpdateConfig(cfg)
				appendLog("auto-update: disabled")
				m.autoUpdateMsg = "auto-update disabled"
			case "t":
				m.autoUpdateEditingTime = true
				m.autoUpdateTimeInput = ""
				m.autoUpdateMsg = ""
			}
			return m, nil

		case viewListTable:
			if key == "ctrl+c" {
				appendLog("quit")
				return m, tea.Quit
			}

			// dismiss a completed download result message with any key
			if m.downloadMsg != "" {
				m.downloadMsg = ""
				return m, nil
			}

			// download in progress: block navigation, allow aborting
			if m.downloading {
				if key == "c" && m.downloadCmd != nil && m.downloadCmd.Process != nil && !m.downloadAborting {
					m.downloadAborting = true
					appendLog("cancelling download: %s", m.downloadTarget)
					_ = m.downloadCmd.Process.Kill()
				}
				return m, nil
			}

			// choosing which parameter size to pull (e.g. qwen2 -> 0.5b/1.5b/7b/72b)
			if m.sizeSelectRow != nil {
				switch key {
				case "up":
					m.sizeSelectCursor--
					if m.sizeSelectCursor < 0 {
						m.sizeSelectCursor = len(m.sizeSelectOptions) - 1
					}
				case "down":
					m.sizeSelectCursor++
					if m.sizeSelectCursor >= len(m.sizeSelectOptions) {
						m.sizeSelectCursor = 0
					}
				case "esc":
					m.sizeSelectRow = nil
				case "enter":
					row := *m.sizeSelectRow
					row.Name = row.Name + ":" + m.sizeSelectOptions[m.sizeSelectCursor]
					m.sizeSelectRow = nil
					m.downloadConfirm = &row
				}
				return m, nil
			}

			// confirmation prompt for a pending download
			if m.downloadConfirm != nil {
				switch key {
				case "y", "enter":
					row := *m.downloadConfirm
					m.downloadConfirm = nil
					m.downloading = true
					m.downloadTarget = row.Name
					m.downloadPct = 0
					m.downloadLine = ""
					m.downloadAborting = false
					appendLog("starting download: %s", row.Name)
					return m, startDownload(row)
				case "n", "esc":
					m.downloadConfirm = nil
					return m, nil
				}
				return m, nil
			}

			filtered := m.filteredCatalog()

			switch key {
			case "alt+r":
				return m.rescanCatalog()
			case "alt+h":
				appendLog("opened help")
				m.view = viewHelpText
				m.helpScroll = 0
				return m, nil
			case "esc":
				if m.catalogSearch != "" {
					m.catalogSearch = ""
					m.catalogCursor = 0
					return m, nil
				}
				appendLog("back to main menu")
				m.view = viewMenu
				return m, nil
			case "up":
				if len(filtered) > 0 {
					m.catalogCursor--
					if m.catalogCursor < 0 {
						m.catalogCursor = len(filtered) - 1
					}
				}
				return m, nil
			case "down":
				if len(filtered) > 0 {
					m.catalogCursor++
					if m.catalogCursor >= len(filtered) {
						m.catalogCursor = 0
					}
				}
				return m, nil
			case "enter":
				if len(filtered) == 0 || m.catalogCursor >= len(filtered) {
					return m, nil
				}
				row := filtered[m.catalogCursor]
				if row.Installed {
					m.downloadMsg = fmt.Sprintf("%s is already installed locally.", row.Name)
					return m, nil
				}
				sizes := parseSizeList(row.Size)
				if row.Source == "ollama-library" && len(sizes) > 1 {
					m.sizeSelectRow = &row
					m.sizeSelectOptions = sizes
					m.sizeSelectCursor = 0
					return m, nil
				}
				if row.Source == "ollama-library" && len(sizes) == 1 {
					row.Name = row.Name + ":" + sizes[0]
				}
				m.downloadConfirm = &row
				return m, nil
			case "backspace":
				if len(m.catalogSearch) > 0 {
					m.catalogSearch = m.catalogSearch[:len(m.catalogSearch)-1]
					m.catalogCursor = 0
				}
				return m, nil
			default:
				if len(key) == 1 {
					m.catalogSearch += key
					m.catalogCursor = 0
				}
				return m, nil
			}

		case viewListScan:
			switch key {
			case "ctrl+c":
				appendLog("quit")
				return m, tea.Quit
			case "esc":
				m.view = viewMenu
				return m, nil
			}
			return m, nil

		case viewPs:
			if key == "ctrl+c" {
				appendLog("quit")
				return m, tea.Quit
			}

			if m.psKillMsg != "" {
				m.psKillMsg = ""
				m.loading = true
				return m, fetchPs
			}

			if m.psKilling {
				return m, nil
			}

			if m.psConfirmKill {
				switch key {
				case "y", "enter":
					m.psConfirmKill = false
					m.psKilling = true
					name := m.psRows[m.psCursor].Name
					m.psKillTarget = name
					appendLog("stopping %s (from running models)", name)
					return m, killModel(name)
				case "n", "esc":
					m.psConfirmKill = false
					return m, nil
				}
				return m, nil
			}

			switch key {
			case "q":
				appendLog("quit")
				return m, tea.Quit
			case "esc":
				appendLog("back to main menu")
				m.view = viewMenu
				return m, nil
			case "r":
				m.loading = true
				m.psRows = nil
				m.psErr = ""
				appendLog("rescanned running models")
				return m, fetchPs
			case "up", "k":
				if len(m.psRows) > 0 {
					m.psCursor--
					if m.psCursor < 0 {
						m.psCursor = len(m.psRows) - 1
					}
				}
				return m, nil
			case "down", "j":
				if len(m.psRows) > 0 {
					m.psCursor++
					if m.psCursor >= len(m.psRows) {
						m.psCursor = 0
					}
				}
				return m, nil
			case "enter":
				if len(m.psRows) == 0 {
					return m, nil
				}
				m.psConfirmKill = true
				return m, nil
			}
			return m, nil

		default:
			switch key {
			case "ctrl+c", "q":
				appendLog("quit")
				return m, tea.Quit
			case "esc":
				m.view = viewMenu
				m.output = ""
				return m, nil
			}
			return m, nil
		}
	}
	return m, nil
}

func (m model) View() string {
	header := m.renderHeader()
	footer := m.renderFooter()
	body := m.renderBody()

	headerHeight := lipgloss.Height(header)
	footerHeight := lipgloss.Height(footer)
	bodyHeight := m.height - headerHeight - footerHeight
	if bodyHeight < 0 {
		bodyHeight = 0
	}

	// Hard safety net: if any view's own height math is off and body ends
	// up taller than bodyHeight, printing it anyway makes the frame taller
	// than the terminal. The terminal then scrolls to fit it, which
	// desyncs bubbletea's cursor tracking from the real screen — the
	// header (printed first) scrolls out of view and never comes back
	// until the app restarts. Clamping here, for every view, means a
	// per-view bug becomes "history got cut a line early", never "the
	// header vanished forever".
	body = clampToLastLines(body, bodyHeight)

	bodyBox := lipgloss.NewStyle().
		Width(m.width).
		Height(bodyHeight).
		Render(body)

	return lipgloss.JoinVertical(lipgloss.Left, header, bodyBox, footer)
}

// clampToLastLines keeps at most maxLines lines of s, dropping from the
// top (oldest content) so the newest output — including the input
// line — stays visible when something runs long.
func clampToLastLines(s string, maxLines int) string {
	if maxLines <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[len(lines)-maxLines:], "\n")
}

// helpPageLines is how many content lines fit in the static help/disclaimer/
// log screens: total terminal height minus header, footer, and the one
// scroll-status line these screens always print at the bottom.
func helpPageLines(height int) int {
	h := height - 2 - 1
	if h < 3 {
		h = 3
	}
	return h
}

// clampHelpScroll bounds m.helpScroll to the actual content of whichever of
// the three static text screens is current, so PgDn/End can't run scroll
// off past the real end (which would otherwise take many keypresses to
// recover from — see the 1<<30 "End" sentinel below).
func (m *model) clampHelpScroll() {
	var content string
	switch m.view {
	case viewHelpText:
		content = renderHelpText()
	case viewDisclaimerText:
		content = renderDisclaimerText()
	case viewLogText:
		content = m.renderLogText()
	case viewToolCategories:
		content, _ = m.renderToolCategories()
	case viewTavilySettings:
		content = m.renderTavilySettings()
	case viewTelegramSettings:
		content = m.renderTelegramSettings()
	case viewEmailSettings:
		content = m.renderEmailSettings()
	case viewBackupSettings:
		content = m.renderBackupSettings()
	case viewBackupBrowser:
		content = m.renderBackupBrowser()
	default:
		return
	}
	total := strings.Count(content, "\n") + 1
	maxScroll := total - helpPageLines(m.height)
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.helpScroll > maxScroll {
		m.helpScroll = maxScroll
	}
	if m.helpScroll < 0 {
		m.helpScroll = 0
	}
}

// selectedToolName returns the tool at toolCatCursor, clamped into range —
// used both to render the detail view and to drive scroll-into-view.
func (m model) selectedToolName() string {
	names := flatToolNames()
	if len(names) == 0 {
		return ""
	}
	c := m.toolCatCursor
	if c < 0 {
		c = 0
	}
	if c >= len(names) {
		c = len(names) - 1
	}
	return names[c]
}

// ensureToolCursorVisible scrolls the tool-category list just enough to
// keep the currently selected tool on screen after Tab/Shift+Tab moves it —
// unlike the other help screens, this one auto-scrolls to the selection
// instead of taking free Up/Down scroll input.
func (m *model) ensureToolCursorVisible() {
	_, cursorLine := m.renderToolCategories()
	page := helpPageLines(m.height)
	if cursorLine < m.helpScroll {
		m.helpScroll = cursorLine
	} else if cursorLine >= m.helpScroll+page {
		m.helpScroll = cursorLine - page + 1
	}
	if m.helpScroll < 0 {
		m.helpScroll = 0
	}
}

// scrollHelpBody windows a static help/disclaimer/log text to the current
// scroll offset and appends a status line, so overflowing content (like the
// full keybinding reference) is reachable instead of being silently cut off
// the top by the outer clampToLastLines() in View().
func (m model) scrollHelpBody(content string) string {
	lines := strings.Split(content, "\n")
	total := len(lines)
	page := helpPageLines(m.height)
	maxScroll := total - page
	if maxScroll < 0 {
		maxScroll = 0
	}
	scroll := m.helpScroll
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}
	end := scroll + page
	if end > total {
		end = total
	}
	window := strings.Join(lines[scroll:end], "\n")
	status := fmt.Sprintf("-- line %d-%d of %d — Up/Down/PgUp/PgDn/Home/End to scroll --", scroll+1, end, total)
	return window + "\n" + helpDescStyle.Render(status)
}

func (m model) renderHeader() string {
	style := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#FFFFFF")).
		Background(lipgloss.Color("#3A3A66")).
		Width(m.width).
		Padding(0, 1)

	if m.view == viewAgentChat {
		seg := func(fg, text string) string {
			return lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("#3A3A66")).Foreground(lipgloss.Color(fg)).Render(text)
		}
		title := seg("#FFFFFF", "llama-shell — ") +
			seg("#FFD700", fmt.Sprintf("agentic chat: %s", m.agentModelName)) +
			seg("#00FFFF", fmt.Sprintf("   cwd: %s", m.agentWorkDir))
		return lipgloss.NewStyle().Background(lipgloss.Color("#3A3A66")).Width(m.width).Padding(0, 1).Render(title)
	}
	return style.Render("llama-shell — Ollama TUI")
}

// footerOSC8Re matches an OSC 8 hyperlink escape sequence, so its bytes
// can be stripped before measuring visible width — lipgloss.Width()
// doesn't recognize OSC 8 (only standard SGR color codes), so a string
// containing one would otherwise be over-counted as wider than it really
// renders, breaking the footer's centering/gap math.
var footerOSC8Re = regexp.MustCompile(`\x1b\]8;;[^\x07]*(?:\x1b\\|\x07)`)

func footerVisibleWidth(s string) int {
	return lipgloss.Width(footerOSC8Re.ReplaceAllString(s, ""))
}

func (m model) renderFooter() string {
	const footerBG = "#3A3A66"
	if m.view == viewLogText {
		// The footer is the one row that's reliably visible no matter what
		// (it's the last thing drawn each frame) — put the search
		// indicator here too, not just at the top of the scrollable body,
		// so it can't get scrolled out of view on a terminal whose real
		// row count doesn't match what bubbletea thinks it is.
		greenBold := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#5FFF5F")).Background(lipgloss.Color(footerBG))
		plainFooter := lipgloss.NewStyle().Foreground(lipgloss.Color("#AAAAAA")).Background(lipgloss.Color(footerBG))
		var queryRendered string
		if m.logSearchMode {
			queryRendered = plainFooter.Render(m.logQuery + "█")
		} else if m.logQuery == "" {
			// Blink the placeholder — a static hint blends into the rest
			// of the dim footer text and is easy to miss entirely.
			queryRendered = plainFooter.Bold(true).Blink(true).Render("(press / to type)")
		} else {
			queryRendered = plainFooter.Render(m.logQuery)
		}
		line := plainFooter.Render(" ") + greenBold.Render("Search") + plainFooter.Render(": ") + queryRendered
		pad := m.width - footerVisibleWidth(line)
		if pad < 0 {
			pad = 0
		}
		return line + plainFooter.Render(strings.Repeat(" ", pad))
	}
	status := lipgloss.NewStyle().Bold(true).Blink(true).Foreground(lipgloss.Color("#FF5F5F")).Background(lipgloss.Color(footerBG)).Render("ollama: not installed")
	if m.ollama.installed {
		status = lipgloss.NewStyle().Foreground(lipgloss.Color("#5FFF5F")).Background(lipgloss.Color(footerBG)).Render(
			fmt.Sprintf("ollama✓ %s", m.ollama.version),
		)
	}
	if m.updateAvailable {
		updateFlag := lipgloss.NewStyle().Bold(true).Blink(true).Foreground(lipgloss.Color("#FFD700")).Background(lipgloss.Color(footerBG)).Render("update")
		status = updateFlag + lipgloss.NewStyle().Background(lipgloss.Color(footerBG)).Render("  ") + status
	}
	// Always show explicit web-server state, not just when it's running —
	// no indicator at all reads as ambiguous (off? unknown? crashed?)
	// rather than a clear "off".
	var webFlag string
	switch {
	case isWebServerRunning():
		cfg := loadWebServerConfig()
		webURL := webServerURL(cfg.Token, webServerEffectivePort(cfg))
		linkStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#5FD7FF")).Background(lipgloss.Color(footerBG)).Underline(true)
		// OSC 8 hyperlink so clicking "web: running" opens the server URL —
		// lipgloss.Width() doesn't understand this escape, so any width math
		// over a string containing webFlag must use footerVisibleWidth
		// (below), not lipgloss.Width, or the gap/padding math miscounts.
		webFlag = "\x1b]8;;" + webURL + "\x1b\\" + linkStyle.Render("web✓") + "\x1b]8;;\x1b\\"
	case loadWebServerConfig().Enabled:
		webFlag = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF5F5F")).Background(lipgloss.Color(footerBG)).Render("web:down")
	default:
		webFlag = lipgloss.NewStyle().Foreground(lipgloss.Color("#888888")).Background(lipgloss.Color(footerBG)).Render("web off")
	}
	status = webFlag + lipgloss.NewStyle().Background(lipgloss.Color(footerBG)).Render("  ") + status

	// Same "always show explicit state" reasoning for Telegram: distinguish
	// bound (actually reachable) from merely running (started but nobody's
	// messaged it yet) from configured-but-dead from never set up.
	var tgFlag string
	switch {
	case isTelegramRunning():
		tgCfg := loadTelegramConfig()
		if tgCfg.ChatID != 0 {
			tgFlag = lipgloss.NewStyle().Foreground(lipgloss.Color("#5FD7FF")).Background(lipgloss.Color(footerBG)).Render("tg✓")
		} else {
			tgFlag = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD700")).Background(lipgloss.Color(footerBG)).Render("tg:unbound")
		}
	case loadTelegramConfig().Token != "":
		tgFlag = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF5F5F")).Background(lipgloss.Color(footerBG)).Render("tg:down")
	default:
		tgFlag = lipgloss.NewStyle().Foreground(lipgloss.Color("#888888")).Background(lipgloss.Color(footerBG)).Render("tg off")
	}
	status = tgFlag + lipgloss.NewStyle().Background(lipgloss.Color(footerBG)).Render("  ") + status

	var tavilyFlag string
	if os.Getenv("TAVILY_API_KEY") != "" {
		tavilyFlag = lipgloss.NewStyle().Foreground(lipgloss.Color("#5FD7FF")).Background(lipgloss.Color(footerBG)).Render("tavily✓")
	} else {
		tavilyFlag = lipgloss.NewStyle().Foreground(lipgloss.Color("#888888")).Background(lipgloss.Color(footerBG)).Render("tavily off")
	}
	status = tavilyFlag + lipgloss.NewStyle().Background(lipgloss.Color(footerBG)).Render("  ") + status

	if loadEmailConfig().FromAddress != "" {
		mailFlag := lipgloss.NewStyle().Foreground(lipgloss.Color("#5FD7FF")).Background(lipgloss.Color(footerBG)).Render("mail✓")
		status = mailFlag + lipgloss.NewStyle().Background(lipgloss.Color(footerBG)).Render("  ") + status
	}

	// Every fragment placed on the footer line — including plain filler
	// spaces — must carry its own Background explicitly. Each styled
	// segment's Render() ends with a full ANSI reset, which also kills
	// whatever background an outer wrapping style set before it; a raw,
	// unstyled space between two segments has no SGR of its own, so it
	// falls back to the terminal's default (black) the moment the
	// preceding segment resets.
	plain := lipgloss.NewStyle().Foreground(lipgloss.Color("#AAAAAA")).Background(lipgloss.Color(footerBG))
	fill := func(n int) string {
		if n < 0 {
			n = 0
		}
		return plain.Render(strings.Repeat(" ", n))
	}

	// Flags live in `left`, not `right`: when the terminal's real width is
	// narrower than m.width (the recurring stale-WindowSizeMsg issue), the
	// gap math still comes out wrong, but content that overflows the right
	// edge is what silently disappears — putting the flags immediately
	// after "build" keeps them visible even when that happens, instead of
	// them being the first thing clipped off-screen.
	buildLabel := fmt.Sprintf("build %s", buildTime)
	if appVersion != "" && appVersion != "dev" {
		buildLabel += " " + appVersion
	}
	left := plain.Render(buildLabel) + plain.Render("  ") + status

	if m.view == viewAgentChat || m.view == viewToolCategories || m.view == viewAgentHelp ||
		m.view == viewShowTable || m.view == viewListTable {
		hintText := "Alt+H: help"
		if m.view == viewAgentChat {
			mode := m.agentToolMode
			if mode == "" {
				mode = "auto"
			}
			hintText = "tool mode: " + mode + "  Alt+H: help"
		} else if m.view == viewToolCategories {
			hintText = "Tab: next  Enter: details  Esc: back"
		} else if m.view == viewAgentHelp {
			hintText = "Esc: back to chat  Ctrl+C: quit"
		}
		hint := lipgloss.NewStyle().Foreground(lipgloss.Color("#FFA500")).Background(lipgloss.Color(footerBG)).Render(hintText)
		totalGap := m.width - footerVisibleWidth(left) - lipgloss.Width(hint) - 6
		if totalGap < 2 {
			totalGap = 2
		}
		line := left + fill(totalGap) + hint
		style := plain.Width(m.width).Padding(0, 1)
		return style.Render(line)
	}

	const githubURL = "https://github.com/affigabmag/llama-shell"
	const githubLabel = "GitHub"
	linkText := lipgloss.NewStyle().Foreground(lipgloss.Color("#5FD7FF")).Background(lipgloss.Color(footerBG)).Underline(true).Render(githubLabel)
	// OSC 8 terminal hyperlink escape: wraps linkText so terminals that
	// support it (Windows Terminal, iTerm2, most modern ones) make it
	// clickable.
	link := "\x1b]8;;" + githubURL + "\x1b\\" + linkText + "\x1b]8;;\x1b\\"

	// Everything left-aligned in one fixed reading order, no gap/centering
	// math against m.width — that math is what kept clipping content off
	// the right edge whenever the terminal's real width didn't match what
	// bubbletea reported.
	line := left + plain.Render("  ") + link
	pad := m.width - footerVisibleWidth(line) - 1
	return plain.Render(" ") + line + fill(pad)
}

var (
	selectedStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#000000")).Background(lipgloss.Color("#5FD7FF"))
	unselectedStyle = lipgloss.NewStyle()
	headerRowStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#5FD7FF"))

	agentUserStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD700"))
	agentReplyStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#5FFF5F"))
	agentToolStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#A6A6A6"))
	agentHeadStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#00FFFF"))
	// agentLinkStyle is the "you>" prompt color (#FFD700) 10% darker, so URLs
	// read as links without competing with the prompt itself.
	agentLinkStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#E6C200")).Underline(true)
	greenLinkStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#5FFF5F")).Underline(true)

	helpKeyStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#5FFF5F"))
	helpDescStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#808080"))
)

// styleHelpLines colorizes "key   description" lines in a help screen: the
// key/hotkey token green, its description grey. A line only qualifies if it
// starts with at least 2 spaces of indent AND has a further run of 2+
// spaces separating the key from its description — that excludes section
// headers (no indent) and wrapped continuation lines (indent but no second
// gap), which are left as plain text.
func styleHelpLines(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		indentLen := len(line) - len(trimmed)
		if indentLen < 2 || trimmed == "" {
			continue
		}
		idx := strings.Index(trimmed, "  ")
		if idx <= 0 {
			continue
		}
		key := trimmed[:idx]
		rest := trimmed[idx:]
		gapLen := len(rest) - len(strings.TrimLeft(rest, " "))
		gap := rest[:gapLen]
		desc := rest[gapLen:]
		lines[i] = line[:indentLen] + helpKeyStyle.Render(key) + gap + helpDescStyle.Render(desc)
	}
	return strings.Join(lines, "\n")
}

// renderBanner draws "LLAMA-SHELL" as block letters. Each letter is one
// solid color, pinned by its position in the word (1st=blue, 2nd=white,
// 3rd=red, 4th=green, 5th=cyan, ... repeating) — static, not animated.
// cityCols/cityRows size the skyline banner's character grid; kept in the
// same rough footprint as the old rotating-shapes banner (9 lines tall) so
// swapping it in doesn't change the main menu's vertical layout.
// cityCols is mutable (not const) — it tracks the terminal's actual width
// so the skyline spans the full banner instead of a fixed narrow strip.
var cityCols = 58

const cityRows = 8

var cityTexChars = []byte{'#', '@', '%', '&', '+', '$'}

type cityBuilding struct {
	x0, w, h int
	hue      float64
	tex      byte
}

// cityNames is a pool of real-world capitals and major cities — one is
// picked at random every time the skyline regenerates, purely cosmetic
// labeling, not tied to the generated silhouette in any way.
var cityNames = []string{
	"Abidjan (Ivory Coast)", "Abu Dhabi (UAE)", "Abuja (Nigeria)", "Accra (Ghana)", "Addis Ababa (Ethiopia)", "Adelaide (Australia)", "Algiers (Algeria)", "Almaty (Kazakhstan)",
	"Amman (Jordan)", "Amsterdam (Netherlands)", "Ankara (Turkey)", "Antananarivo (Madagascar)", "Antwerp (Belgium)", "Ashgabat (Turkmenistan)", "Asmara (Eritrea)", "Astana (Kazakhstan)",
	"Asunción (Paraguay)", "Athens (Greece)", "Atlanta (USA)", "Auckland (New Zealand)", "Baghdad (Iraq)", "Baku (Azerbaijan)", "Bamako (Mali)", "Bandar Seri Begawan (Brunei)",
	"Bangalore (India)", "Bangkok (Thailand)", "Bangui (Central African Republic)", "Banjul (Gambia)", "Barcelona (Spain)", "Basel (Switzerland)", "Basra (Iraq)", "Beijing (China)",
	"Beirut (Lebanon)", "Belgrade (Serbia)", "Belfast (Northern Ireland)", "Belmopan (Belize)", "Berlin (Germany)", "Bern (Switzerland)", "Bhopal (India)", "Bilbao (Spain)",
	"Birmingham (England)", "Bishkek (Kyrgyzstan)", "Bissau (Guinea-Bissau)", "Bogotá (Colombia)", "Boise (USA)", "Bologna (Italy)", "Bonn (Germany)", "Bordeaux (France)",
	"Boston (USA)", "Brasília (Brazil)", "Bratislava (Slovakia)", "Brazzaville (Congo)", "Bridgetown (Barbados)", "Brisbane (Australia)", "Bristol (England)", "Brussels (Belgium)",
	"Bucharest (Romania)", "Budapest (Hungary)", "Buenos Aires (Argentina)", "Bujumbura (Burundi)", "Busan (South Korea)", "Cairo (Egypt)", "Calgary (Canada)", "Cali (Colombia)",
	"Canberra (Australia)", "Cape Town (South Africa)", "Caracas (Venezuela)", "Cardiff (Wales)", "Casablanca (Morocco)", "Castries (Saint Lucia)", "Cebu City (Philippines)", "Chandigarh (India)",
	"Chengdu (China)", "Chennai (India)", "Chicago (USA)", "Chihuahua (Mexico)", "Chisinau (Moldova)", "Chittagong (Bangladesh)", "Christchurch (New Zealand)", "Cluj-Napoca (Romania)",
	"Cologne (Germany)", "Colombo (Sri Lanka)", "Conakry (Guinea)", "Copenhagen (Denmark)", "Cordoba (Argentina)", "Curitiba (Brazil)", "Dakar (Senegal)", "Dallas (USA)",
	"Damascus (Syria)", "Da Nang (Vietnam)", "Dar es Salaam (Tanzania)", "Denver (USA)", "Detroit (USA)", "Dhaka (Bangladesh)", "Dijon (France)", "Dili (Timor-Leste)",
	"Djibouti (Djibouti)", "Doha (Qatar)", "Dodoma (Tanzania)", "Doncaster (England)", "Dortmund (Germany)", "Dresden (Germany)", "Dubai (UAE)", "Dublin (Ireland)",
	"Dushanbe (Tajikistan)", "Düsseldorf (Germany)", "Edinburgh (Scotland)", "Edmonton (Canada)", "Erbil (Iraq)", "Faisalabad (Pakistan)", "Florence (Italy)", "Fortaleza (Brazil)",
	"Frankfurt (Germany)", "Freetown (Sierra Leone)", "Fresno (USA)", "Fukuoka (Japan)", "Funafuti (Tuvalu)", "Gaborone (Botswana)", "Gaziantep (Turkey)", "Geneva (Switzerland)",
	"Genoa (Italy)", "Georgetown (Guyana)", "Gothenburg (Sweden)", "Guadalajara (Mexico)", "Guangzhou (China)", "Guatemala City (Guatemala)", "Guayaquil (Ecuador)", "The Hague (Netherlands)",
	"Hamburg (Germany)", "Hangzhou (China)", "Hanoi (Vietnam)", "Harare (Zimbabwe)", "Harbin (China)", "Havana (Cuba)", "Helsinki (Finland)", "Ho Chi Minh City (Vietnam)",
	"Hobart (Australia)", "Honiara (Solomon Islands)", "Honolulu (USA)", "Houston (USA)", "Hyderabad (India)", "Ibadan (Nigeria)", "Incheon (South Korea)", "Indianapolis (USA)",
	"Islamabad (Pakistan)", "Istanbul (Turkey)", "Jaipur (India)", "Jakarta (Indonesia)", "Jeddah (Saudi Arabia)", "Jerusalem (Israel)", "Johannesburg (South Africa)", "Juba (South Sudan)",
	"Kabul (Afghanistan)", "Kampala (Uganda)", "Kano (Nigeria)", "Kansas City (USA)", "Karachi (Pakistan)", "Kathmandu (Nepal)", "Kaunas (Lithuania)", "Kigali (Rwanda)",
	"Kingston (Jamaica)", "Kingstown (Saint Vincent and the Grenadines)", "Kinshasa (DR Congo)", "Kobe (Japan)", "Kolkata (India)", "Krakow (Poland)", "Kuala Lumpur (Malaysia)", "Kuching (Malaysia)",
	"Kuwait City (Kuwait)", "Kyiv (Ukraine)", "Kyoto (Japan)", "La Paz (Bolivia)", "Lagos (Nigeria)", "Lahore (Pakistan)", "Las Vegas (USA)", "Leeds (England)",
	"Leipzig (Germany)", "Libreville (Gabon)", "Lilongwe (Malawi)", "Lima (Peru)", "Lisbon (Portugal)", "Ljubljana (Slovenia)", "Lomé (Togo)", "London (England)",
	"Los Angeles (USA)", "Luanda (Angola)", "Lubumbashi (DR Congo)", "Lucknow (India)", "Lusaka (Zambia)", "Luxembourg City (Luxembourg)", "Lyon (France)", "Macau (China)",
	"Madrid (Spain)", "Majuro (Marshall Islands)", "Malabo (Equatorial Guinea)", "Malé (Maldives)", "Managua (Nicaragua)", "Manama (Bahrain)", "Manaus (Brazil)", "Manchester (England)",
	"Manila (Philippines)", "Maputo (Mozambique)", "Marrakesh (Morocco)", "Marseille (France)", "Maseru (Lesotho)", "Mbabane (Eswatini)", "Medellín (Colombia)", "Melbourne (Australia)",
	"Memphis (USA)", "Mexico City (Mexico)", "Miami (USA)", "Milan (Italy)", "Minsk (Belarus)", "Mogadishu (Somalia)", "Monaco (Monaco)", "Monrovia (Liberia)",
	"Monterrey (Mexico)", "Montevideo (Uruguay)", "Montreal (Canada)", "Moroni (Comoros)", "Moscow (Russia)", "Mumbai (India)", "Munich (Germany)", "Muscat (Oman)",
	"Nagoya (Japan)", "Nairobi (Kenya)", "Nanjing (China)", "Naples (Italy)", "Nassau (Bahamas)", "N'Djamena (Chad)", "New Delhi (India)", "New Orleans (USA)",
	"New York City (USA)", "Niamey (Niger)", "Nicosia (Cyprus)", "Nouakchott (Mauritania)", "Nur-Sultan (Kazakhstan)", "Nuremberg (Germany)", "Oklahoma City (USA)", "Omaha (USA)",
	"Osaka (Japan)", "Oslo (Norway)", "Ottawa (Canada)", "Ouagadougou (Burkina Faso)", "Palikir (Micronesia)", "Panama City (Panama)", "Paramaribo (Suriname)", "Paris (France)",
	"Perth (Australia)", "Phnom Penh (Cambodia)", "Phoenix (USA)", "Podgorica (Montenegro)", "Port-au-Prince (Haiti)", "Port Louis (Mauritius)", "Port Moresby (Papua New Guinea)", "Port of Spain (Trinidad and Tobago)",
	"Port Vila (Vanuatu)", "Porto (Portugal)", "Porto Alegre (Brazil)", "Portland (USA)", "Poznań (Poland)", "Prague (Czech Republic)", "Praia (Cape Verde)", "Pretoria (South Africa)",
	"Pristina (Kosovo)", "Pusan (South Korea)", "Pyongyang (North Korea)", "Quebec City (Canada)", "Quito (Ecuador)", "Rabat (Morocco)", "Raleigh (USA)", "Ramallah (Palestine)",
	"Recife (Brazil)", "Reykjavik (Iceland)", "Riga (Latvia)", "Rio de Janeiro (Brazil)", "Riyadh (Saudi Arabia)", "Rome (Italy)", "Rosario (Argentina)", "Rotterdam (Netherlands)",
	"Sacramento (USA)", "Saint-Denis (Réunion, France)", "Salt Lake City (USA)", "Salvador (Brazil)", "Samara (Russia)", "San Antonio (USA)", "San Diego (USA)", "San José (Costa Rica)",
	"San Juan (Puerto Rico)", "San Marino (San Marino)", "San Salvador (El Salvador)", "Sana'a (Yemen)", "Santiago (Chile)", "Santo Domingo (Dominican Republic)", "São Paulo (Brazil)", "Sapporo (Japan)",
	"Sarajevo (Bosnia and Herzegovina)", "Seattle (USA)", "Seoul (South Korea)", "Sevilla (Spain)", "Shanghai (China)", "Shenzhen (China)", "Singapore (Singapore)", "Skopje (North Macedonia)",
	"Sofia (Bulgaria)", "Split (Croatia)", "St. Louis (USA)", "St. Petersburg (Russia)", "Stockholm (Sweden)", "Strasbourg (France)", "Stuttgart (Germany)", "Suva (Fiji)",
	"Suzhou (China)", "Sydney (Australia)", "Taipei (Taiwan)", "Tallinn (Estonia)", "Tashkent (Uzbekistan)", "Tbilisi (Georgia)", "Tegucigalpa (Honduras)", "Tehran (Iran)",
	"Tel Aviv (Israel)", "Thimphu (Bhutan)", "Tianjin (China)", "Tijuana (Mexico)", "Tirana (Albania)", "Tokyo (Japan)", "Toronto (Canada)", "Toulouse (France)",
	"Tripoli (Libya)", "Tunis (Tunisia)", "Turin (Italy)", "Ulaanbaatar (Mongolia)", "Utrecht (Netherlands)", "Vaduz (Liechtenstein)", "Valencia (Spain)", "Valletta (Malta)",
	"Vancouver (Canada)", "Vatican City (Vatican City)", "Venice (Italy)", "Victoria (Seychelles)", "Vienna (Austria)", "Vientiane (Laos)", "Vilnius (Lithuania)", "Warsaw (Poland)",
	"Washington DC (USA)", "Wellington (New Zealand)", "Winnipeg (Canada)", "Wuhan (China)", "Xi'an (China)", "Yamoussoukro (Ivory Coast)", "Yangon (Myanmar)", "Yaoundé (Cameroon)",
	"Yekaterinburg (Russia)", "Yerevan (Armenia)", "Yokohama (Japan)", "Zagreb (Croatia)", "Zürich (Switzerland)", "Aarhus (Denmark)", "Aberdeen (Scotland)", "Adana (Turkey)",
	"Agra (India)", "Ahmedabad (India)", "Akron (USA)", "Albany (USA)", "Albuquerque (USA)", "Alexandria (Egypt)", "Amritsar (India)", "Anaheim (USA)",
	"Ankara (Turkey)", "Ann Arbor (USA)", "Antalya (Turkey)", "Antioch (USA)", "Arequipa (Peru)", "Arlington (USA)", "Aruba (Aruba)", "Astana (Kazakhstan)",
	"Athens, Georgia (USA)", "Augsburg (Germany)", "Aurora (USA)", "Austin (USA)", "Bahia Blanca (Argentina)", "Baku (Azerbaijan)", "Balikpapan (Indonesia)", "Baltimore (USA)",
	"Bamberg (Germany)", "Bandung (Indonesia)", "Bataan (Philippines)", "Bath (England)", "Baton Rouge (USA)", "Bedford (England)", "Belém (Brazil)", "Belgorod (Russia)",
	"Bendigo (Australia)", "Bergen (Norway)", "Bexley (England)", "Bhubaneswar (India)", "Białystok (Poland)", "Blackpool (England)", "Bloemfontein (South Africa)", "Bolton (England)",
	"Bordertown (Australia)", "Bradford (England)", "Braga (Portugal)", "Brampton (Canada)", "Brighton (England)", "Brno (Czech Republic)", "Buffalo (USA)", "Burgas (Bulgaria)",
	"Bydgoszcz (Poland)", "Cagliari (Italy)", "Cairns (Australia)", "Calabar (Nigeria)", "Calgary East (Canada)", "Cambridge (England)", "Campinas (Brazil)", "Cancún (Mexico)",
	"Canterbury (England)", "Cape Coral (USA)", "Cartagena (Colombia)", "Catania (Italy)", "Cebu (Philippines)", "Charleston (USA)", "Charlotte (USA)", "Chattanooga (USA)",
	"Cherbourg (France)", "Chiba (Japan)", "Chico (USA)", "Cincinnati (USA)", "Ciudad Juárez (Mexico)", "Cleveland (USA)", "Coimbra (Portugal)", "Colombo, Sri Lanka (Sri Lanka)",
	"Columbus (USA)", "Constanța (Romania)", "Coventry (England)", "Coyoacán (Mexico)", "Cuiabá (Brazil)", "Culiacán (Mexico)", "Cuzco (Peru)", "Dammam (Saudi Arabia)",
	"Davao City (Philippines)", "Dayton (USA)", "Debrecen (Hungary)", "Delft (Netherlands)", "Derby (England)", "Des Moines (USA)", "Dnipro (Ukraine)", "Donetsk (Ukraine)",
	"Durban (South Africa)", "Durham (England)", "East London (South Africa)", "Eindhoven (Netherlands)", "El Paso (USA)", "Erie (USA)", "Essen (Germany)", "Exeter (England)",
	"Fez (Morocco)", "Florianópolis (Brazil)", "Fort Worth (USA)", "Fukushima (Japan)", "Gdańsk (Poland)", "Gdynia (Poland)", "Gent (Belgium)", "Gijón (Spain)",
	"Glasgow (Scotland)", "Goiânia (Brazil)", "Gold Coast (Australia)", "Graz (Austria)", "Grenoble (France)", "Guadalupe (Mexico)", "Gwangju (South Korea)", "Halifax (Canada)",
	"Hamilton (Canada)", "Hangzhou West (China)", "Hartford (USA)", "Heraklion (Greece)", "Hermosillo (Mexico)", "Hiroshima (Japan)", "Hobart Town (Australia)", "Hokkaido (Japan)",
	"Holguín (Cuba)", "Iasi (Romania)", "Ibiza Town (Spain)", "Indore (India)", "Inverness (Scotland)", "Iquitos (Peru)", "Ipoh (Malaysia)", "Irkutsk (Russia)",
	"Izmir (Turkey)", "Jacksonville (USA)", "Jerez de la Frontera (Spain)", "Jinan (China)", "João Pessoa (Brazil)", "Johor Bahru (Malaysia)", "Jönköping (Sweden)", "Kaliningrad (Russia)",
	"Kanazawa (Japan)", "Kanpur (India)", "Katowice (Poland)", "Kazan (Russia)", "Kelowna (Canada)", "Kemerovo (Russia)", "Khabarovsk (Russia)", "Kharkiv (Ukraine)",
	"Kingston upon Hull (England)", "Kirov (Russia)", "Kitakyushu (Japan)", "Klagenfurt (Austria)", "Kobenhavn (Denmark)", "Kochi (India)", "Košice (Slovakia)", "Kraków (Poland)",
	"Kumamoto (Japan)", "Kunming (China)", "La Coruña (Spain)", "Lausanne (Switzerland)", "Le Havre (France)", "León (Mexico)", "Liège (Belgium)", "Lille (France)",
	"Limoges (France)", "Linz (Austria)", "Little Rock (USA)", "Liverpool (England)", "Łódź (Poland)", "Louisville (USA)", "Lviv (Ukraine)", "Maastricht (Netherlands)",
	"Makassar (Indonesia)", "Malaga (Spain)", "Mandalay (Myanmar)", "Mannheim (Germany)", "Maracaibo (Venezuela)", "Mar del Plata (Argentina)", "Matsuyama (Japan)", "Medan (Indonesia)",
	"Mérida (Mexico)", "Messina (Italy)", "Milwaukee (USA)", "Minneapolis (USA)", "Mombasa (Kenya)", "Montpellier (France)", "Montreux (Switzerland)", "Mysore (India)",
	"Nagano (Japan)", "Nagasaki (Japan)", "Nairobi West (Kenya)", "Nanaimo (Canada)", "Nancy (France)", "Nanning (China)", "Nantes (France)", "Nashville (USA)",
	"Newcastle (England)", "Niigata (Japan)", "Nizhny Novgorod (Russia)", "Northampton (England)", "Norwich (England)", "Novosibirsk (Russia)", "Nur City (Kazakhstan)", "Oaxaca (Mexico)",
	"Odense (Denmark)", "Odesa (Ukraine)", "Okayama (Japan)", "Omdurman (Sudan)", "Ontario (USA)", "Orenburg (Russia)", "Orlando (USA)", "Oshawa (Canada)",
	"Oulu (Finland)", "Padua (Italy)", "Palembang (Indonesia)", "Palermo (Italy)", "Pamplona (Spain)", "Panama (Panama)", "Pärnu (Estonia)", "Peoria (USA)",
	"Perm (Russia)", "Perpignan (France)", "Philadelphia (USA)", "Pittsburgh (USA)", "Plymouth (England)", "Poitiers (France)", "Ponce (Puerto Rico)", "Portsmouth (England)",
	"Poznan (Poland)", "Puebla (Mexico)", "Pune (India)", "Querétaro (Mexico)", "Quezon City (Philippines)", "Regina (Canada)", "Reims (France)", "Rennes (France)",
	"Richmond (USA)", "Rochester (USA)", "Rostock (Germany)", "Rostov-on-Don (Russia)", "Rotterdam West (Netherlands)", "Sacramento North (USA)", "Saitama (Japan)", "Salamanca (Spain)",
	"Salzburg (Austria)", "San Bernardino (USA)", "San Luis Potosí (Mexico)", "Sankt Pölten (Austria)", "Santa Cruz (Bolivia)", "Santa Fe (USA)", "Santander (Spain)", "Saratov (Russia)",
	"Saskatoon (Canada)", "Semarang (Indonesia)", "Sendai (Japan)", "Sheffield (England)", "Shizuoka (Japan)", "Sibiu (Romania)", "Sochi (Russia)", "Southampton (England)",
	"Split, Croatia (Croatia)", "St. John's (Canada)", "Stavanger (Norway)", "Stavropol (Russia)", "Stoke-on-Trent (England)", "Sucre (Bolivia)", "Surabaya (Indonesia)", "Surat (India)",
	"Sverdlovsk (Russia)", "Szczecin (Poland)", "Tacoma (USA)", "Tainan (Taiwan)", "Taichung (Taiwan)", "Tallahassee (USA)", "Tampa (USA)", "Tampere (Finland)",
	"Tangier (Morocco)", "Tarragona (Spain)", "Thessaloniki (Greece)", "Tijuana North (Mexico)", "Timişoara (Romania)", "Toledo (USA)", "Tomsk (Russia)", "Torreón (Mexico)",
	"Toulon (France)", "Townsville (Australia)", "Trieste (Italy)", "Trois-Rivières (Canada)", "Trondheim (Norway)", "Tucson (USA)", "Tulsa (USA)", "Turku (Finland)",
	"Ufa (Russia)", "Umeå (Sweden)", "Valdivia (Chile)", "Valparaíso (Chile)", "Varna (Bulgaria)", "Vaasa (Finland)", "Verona (Italy)", "Veracruz (Mexico)",
	"Vigo (Spain)", "Villahermosa (Mexico)", "Vitoria-Gasteiz (Spain)", "Vladivostok (Russia)", "Volgograd (Russia)", "Wakayama (Japan)", "Waterloo (Canada)", "Wichita (USA)",
	"Wiesbaden (Germany)", "Windhoek (Namibia)", "Windsor (Canada)", "Winston-Salem (USA)", "Wolverhampton (England)", "Worcester (England)", "Wrocław (Poland)", "Wuppertal (Germany)",
	"Wuxi (China)", "Xiamen (China)", "Yangzhou (China)", "Yaroslavl (Russia)", "Yokosuka (Japan)", "York (England)", "Yueyang (China)", "Zadar (Croatia)",
	"Zaragoza (Spain)", "Zhengzhou (China)", "Zibo (China)", "Zonguldak (Turkey)",
	"Aachen (Germany)", "Aalborg (Denmark)", "Abakan (Russia)", "Abbotsford (Canada)", "Aberystwyth (Wales)", "Abilene (USA)", "Acapulco (Mexico)", "Adelaide Hills (Australia)",
	"Ain Sokhna (Egypt)", "Ajaccio (France)", "Ajman (UAE)", "Akita (Japan)", "Al Ain (UAE)", "Albacete (Spain)", "Alesund (Norway)", "Alicante (Spain)",
	"Almere (Netherlands)", "Alofi (Niue)", "Amarillo (USA)", "Amravati (India)", "Anchorage (USA)", "Ancona (Italy)", "Andorra la Vella (Andorra)", "Angeles City (Philippines)",
	"Ankeny (USA)", "Annecy (France)", "Antipolo (Philippines)", "Antofagasta (Chile)", "Aomori (Japan)", "Apeldoorn (Netherlands)", "Appleton (USA)", "Arad (Romania)",
	"Aracaju (Brazil)", "Arad, Israel (Israel)", "Arkhangelsk (Russia)", "Arles (France)", "Arnhem (Netherlands)", "Ashdod (Israel)", "Asheville (USA)", "Ashkelon (Israel)",
	"Assisi (Italy)", "Astrakhan (Russia)", "Asuncion Bay (Paraguay)", "Athlone (Ireland)", "Atlantic City (USA)", "Auburn (USA)", "Augusta (USA)", "Aviemore (Scotland)",
	"Avignon (France)", "Ayr (Scotland)", "Bacolod (Philippines)", "Badajoz (Spain)", "Baden-Baden (Germany)", "Baguio (Philippines)", "Bahawalpur (Pakistan)", "Baie-Comeau (Canada)",
	"Bakersfield (USA)", "Balashikha (Russia)", "Ballarat (Australia)", "Banff (Canada)", "Bangor (Wales)", "Baoding (China)", "Baotou (China)", "Baracoa (Cuba)",
	"Barcelona, Venezuela (Venezuela)", "Bari (Italy)", "Barquisimeto (Venezuela)", "Barrie (Canada)", "Basseterre (Saint Kitts and Nevis)", "Bathurst (Australia)", "Batumi (Georgia)", "Bayreuth (Germany)",
	"Beaufort West (South Africa)", "Beira (Mozambique)", "Bellingham (USA)", "Benalmadena (Spain)", "Bend (USA)", "Bendery (Moldova)", "Bengaluru Rural (India)", "Benidorm (Spain)",
	"Bergamo (Italy)", "Berkeley (USA)", "Bethlehem (Palestine)", "Beziers (France)", "Bhavnagar (India)", "Biarritz (France)", "Bielefeld (Germany)", "Bikaner (India)",
	"Billings (USA)", "Biloxi (USA)", "Bishop's Stortford (England)", "Bitola (North Macedonia)", "Blantyre (Malawi)", "Bloomington (USA)", "Bodo (Norway)", "Bogor (Indonesia)",
	"Boise City (USA)", "Bolzano (Italy)", "Bonaire (Bonaire)", "Bordeaux Lac (France)", "Boryeong (South Korea)", "Bratsk (Russia)", "Bregenz (Austria)", "Bremerton (USA)",
	"Brest, France (France)", "Bridgeport (USA)", "Bridgetown Village (Barbados)", "Brindisi (Italy)", "Broken Hill (Australia)", "Brownsville (USA)", "Bruges (Belgium)", "Bucaramanga (Colombia)",
	"Bukhara (Uzbekistan)", "Bundaberg (Australia)", "Burlington (USA)", "Bydgoszcz East (Poland)", "Caen (France)", "Cagayan de Oro (Philippines)", "Cairo, USA (USA)", "Cajamarca (Peru)",
	"Calais (France)", "Caldas Novas (Brazil)", "Cali Valle (Colombia)", "Callao (Peru)", "Camaguey (Cuba)", "Campeche (Mexico)", "Canakkale (Turkey)", "Cannes (France)",
	"Canoas (Brazil)", "Canyonville (USA)", "Cape Girardeau (USA)", "Carcassonne (France)", "Cardenas (Cuba)", "Carletonville (South Africa)", "Carrara (Italy)", "Cartago (Costa Rica)",
	"Casper (USA)", "Catanzaro (Italy)", "Caxias do Sul (Brazil)", "Cayenne (French Guiana)", "Cebu Lapu-Lapu (Philippines)", "Cedar Rapids (USA)", "Celle (Germany)", "Chachapoyas (Peru)",
	"Chalkida (Greece)", "Champaign (USA)", "Changwon (South Korea)", "Charleroi (Belgium)", "Chelmsford (England)", "Cheltenham (England)", "Chemnitz (Germany)", "Cherkasy (Ukraine)",
	"Chester (England)", "Chetumal (Mexico)", "Chiang Mai (Thailand)", "Chibougamau (Canada)", "Chichester (England)", "Chillan (Chile)", "Chimoio (Mozambique)", "Chinandega (Nicaragua)",
	"Christiansted (US Virgin Islands)", "Cienfuegos (Cuba)", "Cirebon (Indonesia)", "Ciudad Bolivar (Venezuela)", "Ciudad del Este (Paraguay)", "Ciudad Guayana (Venezuela)", "Clermont-Ferrand (France)", "Cluj (Romania)",
	"Cochabamba (Bolivia)", "Coeur d'Alene (USA)", "Colchester (England)", "Cologne West (Germany)", "Colonia del Sacramento (Uruguay)", "Colorado Springs (USA)", "Comayagua (Honduras)", "Constantine (Algeria)",
	"Copacabana, Bolivia (Bolivia)", "Cordoba, Spain (Spain)", "Corfu Town (Greece)", "Cork (Ireland)", "Corner Brook (Canada)", "Cortina d'Ampezzo (Italy)", "Corumba (Brazil)", "Cotonou (Benin)",
	"Coventry East (England)", "Cozumel (Mexico)", "Cremona (Italy)", "Crestview (USA)", "Cuenca (Ecuador)", "Cuernavaca (Mexico)", "Culebra (Puerto Rico)", "Cusco Region (Peru)",
	"Dagupan (Philippines)", "Dalian (China)", "Danang Bay (Vietnam)", "Danville (USA)", "Darmstadt (Germany)", "Darwin (Australia)", "Datong (China)", "Daugavpils (Latvia)",
	"Davenport (USA)", "David (Panama)", "Deauville (France)", "Denpasar (Indonesia)", "Derry (Northern Ireland)", "Dessau (Germany)", "Dijon Nord (France)", "Dinard (France)",
	"Diyarbakir (Turkey)", "Dodge City (USA)", "Dolores, Argentina (Argentina)", "Dordrecht (Netherlands)", "Dortmund Ost (Germany)", "Douala (Cameroon)", "Douglas (Isle of Man)", "Dover (England)",
	"Dubrovnik (Croatia)", "Duluth (USA)", "Dunedin (New Zealand)", "Durres (Albania)", "Eau Claire (USA)", "Edirne (Turkey)", "Eilat (Israel)", "Ekaterinburg Oblast (Russia)",
	"El Jadida (Morocco)", "Ely (England)", "Enschede (Netherlands)", "Erie County (USA)", "Erlangen (Germany)", "Esbjerg (Denmark)", "Eskisehir (Turkey)", "Essaouira (Morocco)",
	"Ferrara (Italy)", "Figueira da Foz (Portugal)", "Flagstaff (USA)", "Flensburg (Germany)", "Florence, USA (USA)", "Foggia (Italy)", "Fontainebleau (France)", "Fort Collins (USA)",
	"Fort Lauderdale (USA)", "Fort Myers (USA)", "Frankfurt Oder (Germany)", "Fredericton (Canada)", "Fribourg (Switzerland)", "Fuerteventura (Spain)", "Fujairah (UAE)", "Fukui (Japan)",
	"Gaborone North (Botswana)", "Galway (Ireland)", "Gandia (Spain)", "Garda (Italy)", "Gaza City (Palestine)", "Gdansk Oliwa (Poland)", "Geelong (Australia)", "Genk (Belgium)",
	"George Town (Malaysia)", "Georgetown, Guyana (Guyana)", "Gera (Germany)", "Gerona (Spain)", "Gijon Nuevo (Spain)", "Girona (Spain)", "Gisborne (New Zealand)", "Gjirokaster (Albania)",
	"Gliwice (Poland)", "Gold Coast West (Australia)", "Goma (DR Congo)", "Gore (New Zealand)", "Gorakhpur (India)", "Gothenburg Archipelago (Sweden)", "Granada (Spain)", "Grand Rapids (USA)",
	"Graz Ost (Austria)", "Great Falls (USA)", "Greeley (USA)", "Grenada Town (Grenada)", "Groningen (Netherlands)", "Guarulhos (Brazil)", "Guaruja (Brazil)", "Gulfport (USA)",
	"Gwalior (India)", "Halden (Norway)", "Halle (Germany)", "Hamamatsu (Japan)", "Hamilton, New Zealand (New Zealand)", "Hangzhou Bay (China)", "Harlingen (USA)", "Harrisburg (USA)",
	"Hartlepool (England)", "Hastings (England)", "Hat Yai (Thailand)", "Havelock North (New Zealand)", "Helsingborg (Sweden)", "Hermiston (USA)", "Hilo (USA)", "Hobart North (Australia)",
	"Hoi An (Vietnam)", "Homs (Syria)", "Hong Kong Island (Hong Kong)", "Honolulu East (USA)", "Hoorn (Netherlands)", "Horsham (England)", "Huelva (Spain)", "Huntsville (USA)",
	"Hyderabad, Pakistan (Pakistan)", "Iasi Vale (Romania)", "Ibadan North (Nigeria)", "Iloilo (Philippines)", "Innsbruck (Austria)", "Inuvik (Canada)", "Ioannina (Greece)", "Ipswich (England)",
	"Isafjordur (Iceland)", "Ischia (Italy)", "Isfahan (Iran)", "Ithaca (USA)", "Jaen (Spain)", "Jalandhar (India)", "Jamestown, Saint Helena (Saint Helena)", "Jasper (Canada)",
	"Jerez (Spain)", "Jinja (Uganda)", "Jodhpur (India)", "Joensuu (Finland)", "Joetsu (Japan)", "Johor Bahru East (Malaysia)", "Joinville (Brazil)", "Jonkoping (Sweden)",
	"Jos (Nigeria)", "Jyvaskyla (Finland)", "Kaduna (Nigeria)", "Kaifeng (China)", "Kalamata (Greece)", "Kalgoorlie (Australia)", "Kamloops (Canada)", "Kandy (Sri Lanka)",
	"Kano North (Nigeria)", "Karlsruhe (Germany)", "Karlstad (Sweden)", "Kars (Turkey)", "Kassel (Germany)", "Katmandu Valley (Nepal)", "Kelowna West (Canada)", "Kemi (Finland)",
	"Kermanshah (Iran)", "Khartoum (Sudan)", "Kielce (Poland)", "Kimberley (South Africa)", "Kingston, New York (USA)", "Kirkcaldy (Scotland)", "Kisumu (Kenya)", "Kitchener (Canada)",
	"Klaipeda (Lithuania)", "Knoxville (USA)", "Kobe Port (Japan)", "Koh Samui (Thailand)", "Koln Deutz (Germany)", "Kolobrzeg (Poland)", "Konya (Turkey)", "Kosice East (Slovakia)",
	"Kota Kinabalu (Malaysia)", "Krasnodar (Russia)", "Krasnoyarsk (Russia)", "Kristiansand (Norway)", "Kuantan (Malaysia)", "Kumasi (Ghana)", "Kunshan (China)", "Kuopio (Finland)",
	"Kutaisi (Georgia)", "La Ceiba (Honduras)", "La Rochelle (France)", "La Serena (Chile)", "Labuan (Malaysia)", "Lafayette (USA)", "Lampang (Thailand)", "Langkawi (Malaysia)",
	"Lansing (USA)", "Larnaca (Cyprus)", "Las Palmas (Spain)", "Latacunga (Ecuador)", "Lecce (Italy)", "Leon, Nicaragua (Nicaragua)", "Leon, Spain (Spain)", "Lerwick (Scotland)",
	"Levuka (Fiji)", "Lexington (USA)", "Liberec (Czech Republic)", "Libourne (France)", "Ljubljana Sever (Slovenia)", "Llandudno (Wales)", "Loja (Ecuador)", "Lokoja (Nigeria)",
	"Lombok (Indonesia)", "Longyearbyen (Norway)", "Lorient (France)", "Los Cabos (Mexico)", "Lubango (Angola)", "Lubbock (USA)", "Lubeck (Germany)", "Lucerne (Switzerland)",
	"Lugano (Switzerland)", "Lusaka South (Zambia)", "Luton (England)", "Luxor (Egypt)", "Lyon Confluence (France)", "Maastricht Oost (Netherlands)", "Machu Picchu (Peru)", "Madison (USA)",
	"Magadan (Russia)", "Magdeburg (Germany)", "Malacca (Malaysia)", "Malaga Puerto (Spain)", "Malmo (Sweden)", "Manado (Indonesia)", "Manavgat (Turkey)", "Manzanillo (Mexico)",
	"Maputo Sul (Mozambique)", "Maracay (Venezuela)", "Marbella (Spain)", "Marrakech Medina (Morocco)", "Marsa Alam (Egypt)", "Maseru East (Lesotho)", "Matamoros (Mexico)", "Mataro (Spain)",
	"Mazatlan (Mexico)", "Mbale (Uganda)", "Medan Kota (Indonesia)", "Mendoza (Argentina)", "Merida, Venezuela (Venezuela)", "Meridian (USA)", "Merida Yucatan (Mexico)", "Metz (France)",
	"Mikkeli (Finland)", "Milford Haven (Wales)", "Missoula (USA)", "Mobile (USA)", "Modena (Italy)", "Mombasa North (Kenya)", "Mons (Belgium)", "Montego Bay (Jamaica)",
	"Monterey (USA)", "Montpellier Sud (France)", "Morelia (Mexico)", "Moron, Argentina (Argentina)", "Mostar (Bosnia and Herzegovina)", "Mtwara (Tanzania)", "Mudanya (Turkey)", "Mumbai Suburban (India)",
	"Mumbles (Wales)", "Murcia (Spain)", "Muscat Old Town (Oman)", "Mysore East (India)", "Nakhon Ratchasima (Thailand)", "Nanaimo North (Canada)", "Nanchang (China)", "Nancy Est (France)",
	"Nanjing East (China)", "Napier (New Zealand)", "Naples, USA (USA)", "Narvik (Norway)", "Nassau North (Bahamas)", "Nazareth (Israel)", "Nelson (New Zealand)", "Newport (Wales)",
	"Newquay (England)", "Niagara Falls (Canada)", "Nice (France)", "Niksic (Montenegro)", "Nogales (Mexico)", "Nong Khai (Thailand)", "Norfolk (USA)", "Norrkoping (Sweden)",
	"North Bay (Canada)", "Northampton West (England)", "Novi Sad (Serbia)", "Nuku'alofa (Tonga)", "Nyeri (Kenya)", "Oaxaca de Juarez (Mexico)", "Odawara (Japan)", "Ohrid (North Macedonia)",
	"Okinawa (Japan)", "Olbia (Italy)", "Olomouc (Czech Republic)", "Olsztyn (Poland)", "Omaha South (USA)", "Ontario, Canada (Canada)", "Opatija (Croatia)", "Oradea (Romania)",
	"Oran (Algeria)", "Orense (Spain)", "Orizaba (Mexico)", "Orsk (Russia)", "Osijek (Croatia)", "Ostrava (Czech Republic)", "Otago (New Zealand)", "Oxford (England)",
	"Padang (Indonesia)", "Paducah (USA)", "Palanga (Lithuania)", "Palma de Mallorca (Spain)", "Pamukkale (Turkey)", "Panama La Vieja (Panama)", "Papeete (French Polynesia)", "Paphos (Cyprus)",
	"Parana (Argentina)", "Parintins (Brazil)", "Parma (Italy)", "Pasadena (USA)", "Pattaya (Thailand)", "Pau (France)", "Penang (Malaysia)", "Perpignan Centre (France)",
	"Perugia (Italy)", "Pescara (Italy)", "Petaling Jaya (Malaysia)", "Phan Thiet (Vietnam)", "Phuket (Thailand)", "Piraeus (Greece)", "Pisa (Italy)", "Plzen (Czech Republic)",
	"Pointe-a-Pitre (Guadeloupe)", "Pokhara (Nepal)", "Ponta Delgada (Portugal)", "Poole (England)", "Port Elizabeth (South Africa)", "Port Harcourt (Nigeria)", "Port Said (Egypt)", "Portimao (Portugal)",
	"Porto-Novo (Benin)", "Potsdam (Germany)", "Poznan Old Town (Poland)", "Pucon (Chile)", "Puebla de Zaragoza (Mexico)", "Puerto Iguazu (Argentina)", "Puerto Montt (Chile)", "Puerto Plata (Dominican Republic)",
	"Puerto Princesa (Philippines)", "Pula (Croatia)", "Punta Arenas (Chile)", "Punta Cana (Dominican Republic)", "Pushkar (India)", "Quang Ngai (Vietnam)", "Queenstown (New Zealand)", "Quimper (France)",
	"Racine (USA)", "Rangpur (Bangladesh)", "Ras al-Khaimah (UAE)", "Rawalpindi (Pakistan)", "Regensburg (Germany)", "Reggio Calabria (Italy)", "Rethymno (Greece)", "Reus (Spain)",
	"Rimini (Italy)", "Roanoke (USA)", "Rockford (USA)", "Rockhampton (Australia)", "Rostock Ost (Germany)", "Rotorua (New Zealand)", "Rovaniemi (Finland)", "Ruse (Bulgaria)",
	"Saarbrucken (Germany)", "Sabadell (Spain)", "Saguenay (Canada)", "Saint-Malo (France)", "Salem, USA (USA)", "Salta (Argentina)", "Saltillo (Mexico)", "San Cristobal, Venezuela (Venezuela)",
	"San Ignacio, Belize (Belize)", "San Pedro Sula (Honduras)", "Sandakan (Malaysia)", "Santa Barbara (USA)", "Santa Marta (Colombia)", "Santarem (Brazil)", "Santiago de Compostela (Spain)", "Sapporo Chuo (Japan)",
	"Sarajevo Novo (Bosnia and Herzegovina)", "Saskatoon East (Canada)", "Savannah (USA)", "Semporna (Malaysia)", "Setubal (Portugal)", "Sevilla Este (Spain)", "Shizuoka Chuo (Japan)", "Siauliai (Lithuania)",
	"Siem Reap (Cambodia)", "Sihanoukville (Cambodia)", "Sioux Falls (USA)", "Skagway (USA)", "Skiathos (Greece)", "Sligo (Ireland)", "Sochi Adler (Russia)", "Sofia Ovcha Kupel (Bulgaria)",
	"Songkhla (Thailand)", "Sopot (Poland)", "Sorrento (Italy)", "South Bend (USA)", "Split Sustipan (Croatia)", "Spokane (USA)", "St. Augustine (USA)", "St. George's (Grenada)",
	"St. Moritz (Switzerland)", "Stara Zagora (Bulgaria)", "Strasbourg Neudorf (France)", "Sucre Centro (Bolivia)", "Sukhumi (Georgia)", "Surakarta (Indonesia)", "Sylhet (Bangladesh)", "Szeged (Hungary)",
	"Tabuk (Saudi Arabia)", "Taganrog (Russia)", "Tainan City (Taiwan)", "Taipei East (Taiwan)", "Takamatsu (Japan)", "Tallahassee North (USA)", "Tampico (Mexico)", "Tangalle (Sri Lanka)",
	"Tanger Med (Morocco)", "Taormina (Italy)", "Tarnow (Poland)", "Tartu (Estonia)", "Tashkent Sity (Uzbekistan)", "Taupo (New Zealand)", "Tbilisi Vake (Georgia)", "Tegucigalpa Sur (Honduras)",
	"Terrassa (Spain)", "Thessaloniki Kalamaria (Greece)", "Thimphu Valley (Bhutan)", "Thunder Bay (Canada)", "Tiflis (Georgia)", "Tijuana Playas (Mexico)", "Timmins (Canada)", "Tirana Ere (Albania)",
	"Toamasina (Madagascar)", "Toledo, Spain (Spain)", "Toluca (Mexico)", "Tomsk Oblast (Russia)", "Topeka (USA)", "Torun (Poland)", "Toulon Est (France)", "Trabzon (Turkey)",
	"Trelew (Argentina)", "Trichy (India)", "Trollhattan (Sweden)", "Tromso (Norway)", "Trujillo, Peru (Peru)", "Tulcan (Ecuador)", "Turku Saaristo (Finland)", "Tuscaloosa (USA)",
	"Uberlandia (Brazil)", "Udaipur (India)", "Uddevalla (Sweden)", "Uithoorn (Netherlands)", "Ulan-Ude (Russia)", "Umtata (South Africa)", "Uppsala (Sweden)", "Urumqi (China)",
	"Usti nad Labem (Czech Republic)", "Utica (USA)", "Vaduz Old Town (Liechtenstein)", "Valdez (USA)", "Valdosta (USA)", "Valencia West (Spain)", "Vallejo (USA)", "Valletta Waterfront (Malta)",
	"Vancouver Island (Canada)", "Varanasi (India)", "Varberg (Sweden)", "Varkaus (Finland)", "Vasteras (Sweden)", "Vejle (Denmark)", "Ventimiglia (Italy)", "Veracruz Puerto (Mexico)",
	"Vernon (Canada)", "Vicenza (Italy)", "Victoria Falls (Zimbabwe)", "Vientiane Sud (Laos)", "Vigo Ria (Spain)", "Vila Nova de Gaia (Portugal)", "Villa Carlos Paz (Argentina)", "Vinnytsia (Ukraine)",
	"Visby (Sweden)", "Vlissingen (Netherlands)", "Vologda (Russia)", "Waco (USA)", "Wagga Wagga (Australia)", "Wanaka (New Zealand)", "Warri (Nigeria)", "Waterford (Ireland)",
	"Whistler (Canada)", "Whitehorse (Canada)", "Wichita Falls (USA)", "Windermere (England)", "Winkler (Canada)", "Wollongong (Australia)", "Worthing (England)", "Wroclaw Stare Miasto (Poland)",
	"Xalapa (Mexico)", "Xiamen Gulangyu (China)", "Yakutsk (Russia)", "Yalta (Ukraine)", "Yangzhou Old Town (China)", "Yazd (Iran)", "Yellowknife (Canada)", "Yerevan Kentron (Armenia)",
	"York Region (Canada)", "Yuma (USA)", "Zacatecas (Mexico)", "Zadar Stari Grad (Croatia)", "Zakynthos (Greece)", "Zamboanga (Philippines)", "Zamora, Spain (Spain)", "Zanzibar City (Tanzania)",
	"Zaporizhzhia (Ukraine)", "Zermatt (Switzerland)", "Zhuhai (China)", "Zilina (Slovakia)", "Zug (Switzerland)",
}

var cityCurrentName string
var cityIndex int
var cityOrder []int
var cityIndexLoaded bool

// cityMajorCount marks the boundary in cityNames between world capitals /
// major global cities (index < cityMajorCount, skyline-worthy) and the
// smaller secondary cities/towns appended afterward, which read as more of
// a countryside/small-town place — those get the grass-and-river scene
// instead of a skyline.
const cityMajorCount = 357

var cityIsCountryside bool

var cityBuildings []cityBuilding
var cityLastGen time.Time
var cityMu sync.Mutex

const cityRegenInterval = 15 * time.Second

// buildCityScene lays out a skyline, regenerating it every cityRegenInterval
// — buildings keep a fixed silhouette and a golden-angle hue each (like the
// "ASCII City" reference) between regens, only their lit-window shimmer
// animates per tick.
func buildCityScene(width int) {
	if width < 20 {
		width = 20
	}
	cityMu.Lock()
	defer cityMu.Unlock()
	widthChanged := width != cityCols
	if widthChanged {
		cityCols = width
	}
	if cityBuildings != nil && !widthChanged && time.Since(cityLastGen) < cityRegenInterval {
		return
	}
	r := mrand.New(mrand.NewSource(time.Now().UnixNano()))
	hueStart := r.Float64() * 360
	buildings := make([]cityBuilding, 0, cityCols/4)
	x, i := 0, 0
	for x < cityCols {
		w := 3 + r.Intn(5)
		if x+w > cityCols {
			w = cityCols - x
		}
		if w <= 0 {
			break
		}
		h := 2 + r.Intn(cityRows-1)
		if r.Float64() < 0.15 {
			h = cityRows
		}
		hue := hueStart + float64(i)*137.508
		tex := cityTexChars[i%len(cityTexChars)]
		buildings = append(buildings, cityBuilding{x0: x, w: w, h: h, hue: hue, tex: tex})
		x += w + 1 + r.Intn(2)
		i++
	}
	cityBuildings = buildings
	if !cityIndexLoaded {
		p := loadCityProgress()
		if len(p.Order) != len(cityNames) {
			// No valid saved shuffle (first run, or the city list's length
			// changed) — build a fresh pseudo-random permutation so the
			// walk order isn't the list's alphabetical order.
			p.Order = mrand.New(mrand.NewSource(time.Now().UnixNano())).Perm(len(cityNames))
			p.Position = 0
		}
		cityOrder = p.Order
		cityIndex = p.Position
		cityIndexLoaded = true
	} else {
		cityIndex++
	}
	cityIndex = cityIndex % len(cityOrder)
	cityCurrentName = cityNames[cityOrder[cityIndex]]
	cityIsCountryside = cityOrder[cityIndex] >= cityMajorCount
	saveCityProgress(cityProgress{Order: cityOrder, Position: cityIndex})
	cityLastGen = time.Now()
}

// countryUTCOffset is a coarse, DST-ignoring approximation — good enough
// for a decorative day/night sky, not for anything that needs to be exact.
// Multi-timezone countries get one representative offset (their capital's).
var countryUTCOffset = map[string]float64{
	"USA": -5, "Canada": -5, "Mexico": -6, "Brazil": -3, "Argentina": -3,
	"Chile": -4, "Peru": -5, "Colombia": -5, "Venezuela": -4, "Ecuador": -5,
	"Bolivia": -4, "Paraguay": -4, "Uruguay": -3, "Guyana": -4, "Suriname": -3,
	"Cuba": -5, "Jamaica": -5, "Haiti": -5, "Dominican Republic": -4, "Puerto Rico": -4,
	"Belize": -6, "Guatemala": -6, "Honduras": -6, "El Salvador": -6, "Nicaragua": -6,
	"Costa Rica": -6, "Panama": -5, "Bahamas": -5, "Barbados": -4, "Trinidad and Tobago": -4,
	"Saint Lucia": -4, "Saint Vincent and the Grenadines": -4, "Aruba": -4,
	"England": 0, "Scotland": 0, "Wales": 0, "Northern Ireland": 0, "Ireland": 0,
	"Portugal": 0, "Spain": 1, "France": 1, "Germany": 1, "Netherlands": 1,
	"Belgium": 1, "Switzerland": 1, "Austria": 1, "Italy": 1, "Poland": 1,
	"Czech Republic": 1, "Slovakia": 1, "Hungary": 1, "Slovenia": 1, "Croatia": 1,
	"Bosnia and Herzegovina": 1, "Serbia": 1, "Montenegro": 1, "North Macedonia": 1,
	"Albania": 1, "Kosovo": 1, "Denmark": 1, "Norway": 1, "Sweden": 1,
	"Luxembourg": 1, "Malta": 1, "Monaco": 1, "Liechtenstein": 1, "San Marino": 1,
	"Vatican City": 1, "Greece": 2, "Romania": 2, "Bulgaria": 2, "Finland": 2,
	"Estonia": 2, "Latvia": 2, "Lithuania": 2, "Cyprus": 2, "Ukraine": 2,
	"Moldova": 2, "Israel": 2, "Palestine": 2, "Lebanon": 2, "Egypt": 2,
	"Russia": 3, "Turkey": 3, "Iraq": 3, "Jordan": 3, "Syria": 3, "Belarus": 3,
	"Kenya": 3, "Tanzania": 3, "Ethiopia": 3, "Sudan": 2, "Somalia": 3, "Djibouti": 3,
	"Yemen": 3, "Saudi Arabia": 3, "Qatar": 3, "Bahrain": 3, "Kuwait": 3,
	"Iran": 3.5, "UAE": 4, "Oman": 4, "Georgia": 4, "Armenia": 4, "Azerbaijan": 4,
	"Afghanistan": 4.5, "Pakistan": 5, "India": 5.5, "Sri Lanka": 5.5,
	"Nepal": 5.75, "Bhutan": 6, "Bangladesh": 6, "Kazakhstan": 6, "Uzbekistan": 5,
	"Kyrgyzstan": 6, "Tajikistan": 5, "Turkmenistan": 5, "Myanmar": 6.5,
	"Thailand": 7, "Vietnam": 7, "Cambodia": 7, "Laos": 7, "Indonesia": 7,
	"Malaysia": 8, "Singapore": 8, "Philippines": 8, "China": 8, "Taiwan": 8,
	"Brunei": 8, "Mongolia": 8, "Hong Kong": 8, "North Korea": 9, "South Korea": 9,
	"Japan": 9, "Timor-Leste": 9, "Papua New Guinea": 10, "Australia": 10,
	"New Zealand": 12, "Fiji": 12, "Vanuatu": 11, "Solomon Islands": 11,
	"Tuvalu": 12, "Micronesia": 11, "Marshall Islands": 12,
	"Nigeria": 1, "Ghana": 0, "Ivory Coast": 0, "Senegal": 0, "Mali": 0,
	"Gambia": 0, "Guinea": 0, "Guinea-Bissau": 0, "Sierra Leone": 0, "Liberia": 0,
	"Togo": 0, "Benin": 1, "Burkina Faso": 0, "Niger": 1, "Chad": 1, "Cameroon": 1,
	"Central African Republic": 1, "Gabon": 1, "Congo": 1, "DR Congo": 1,
	"Equatorial Guinea": 1, "Angola": 1, "Zambia": 2, "Zimbabwe": 2, "Malawi": 2,
	"Mozambique": 2, "Namibia": 2, "Botswana": 2, "South Africa": 2, "Lesotho": 2,
	"Eswatini": 2, "Madagascar": 3, "Mauritius": 4, "Seychelles": 4, "Comoros": 3,
	"Rwanda": 2, "Uganda": 3, "Burundi": 2, "South Sudan": 2, "Eritrea": 3,
	"Algeria": 1, "Morocco": 1, "Tunisia": 1, "Libya": 2, "Mauritania": 0,
	"Cape Verde": -1, "Réunion, France": 4,
}

// cityIsDaylight approximates whether it's roughly daytime (6:00-18:00
// local) in the country named in cityCurrentName — purely decorative, so
// an unrecognized/multi-zone country just falls back to false (night look).
func cityIsDaylight() bool {
	start := strings.LastIndexByte(cityCurrentName, '(')
	end := strings.LastIndexByte(cityCurrentName, ')')
	if start < 0 || end < 0 || end <= start {
		return false
	}
	country := cityCurrentName[start+1 : end]
	offset, ok := countryUTCOffset[country]
	if !ok {
		return false
	}
	localHour := math.Mod(float64(time.Now().UTC().Hour())+offset, 24)
	if localHour < 0 {
		localHour += 24
	}
	return localHour >= 6 && localHour < 18
}

type cityProgress struct {
	Order    []int `json:"order"`
	Position int   `json:"position"`
}

func cityProgressPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "llama-shell", "city_progress.json")
}

func loadCityProgress() cityProgress {
	data, err := os.ReadFile(cityProgressPath())
	if err != nil {
		return cityProgress{}
	}
	var p cityProgress
	_ = json.Unmarshal(data, &p)
	return p
}

func saveCityProgress(p cityProgress) {
	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	path := cityProgressPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, data, 0o644)
}

// hslHex mirrors the artifact's hslToRgb — golden-angle hue spacing is
// what actually guarantees neighboring buildings read as distinct colors.
func hslHex(h, s, l float64) string {
	h = math.Mod(h, 360)
	if h < 0 {
		h += 360
	}
	c := (1 - math.Abs(2*l-1)) * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	mm := l - c/2
	var rr, gg, bb float64
	switch {
	case h < 60:
		rr, gg, bb = c, x, 0
	case h < 120:
		rr, gg, bb = x, c, 0
	case h < 180:
		rr, gg, bb = 0, c, x
	case h < 240:
		rr, gg, bb = 0, x, c
	case h < 300:
		rr, gg, bb = x, 0, c
	default:
		rr, gg, bb = c, 0, x
	}
	return fmt.Sprintf("#%02X%02X%02X", int((rr+mm)*255), int((gg+mm)*255), int((bb+mm)*255))
}

// pseudoHash gives each cell a stable, well-mixed integer so twinkle/window
// placement looks random but doesn't need to be stored anywhere.
func pseudoHash(a, b int) uint32 {
	h := uint32(a)*374761393 + uint32(b)*668265263
	h = (h ^ (h >> 13)) * 1274126177
	return h ^ (h >> 16)
}

func (m model) renderCityBanner() string {
	width := m.width - 4 // matches renderBody's box Padding(1, 2): 2 cols each side
	if width <= 0 {
		width = 58
	}
	buildCityScene(width)

	var scene string
	if cityIsCountryside {
		scene = m.renderCountrysideScene()
	} else {
		scene = m.renderSkylineScene()
	}

	imgSearchURL := "https://www.google.com/search?tbm=isch&q=" + url.QueryEscape(cityCurrentName)
	labelText := lipgloss.NewStyle().Bold(true).Underline(true).Foreground(lipgloss.Color("#EEF3FF")).Render(cityCurrentName)
	label := "\x1b]8;;" + imgSearchURL + "\x1b\\" + labelText + "\x1b]8;;\x1b\\"
	return label + "\n" + scene + "\n"
}

func (m model) renderSkylineScene() string {
	buildingAt := make([]*cityBuilding, cityCols)
	for i := range cityBuildings {
		b := &cityBuildings[i]
		for c := b.x0; c < b.x0+b.w && c < cityCols; c++ {
			buildingAt[c] = b
		}
	}

	skyStyle := map[string]lipgloss.Style{}
	styleFor := func(hex string) lipgloss.Style {
		if s, ok := skyStyle[hex]; ok {
			return s
		}
		s := lipgloss.NewStyle().Background(lipgloss.Color(hex))
		skyStyle[hex] = s
		return s
	}

	daylight := cityIsDaylight()
	daySkyStyle := lipgloss.NewStyle().Background(lipgloss.Color("#5AA9D6"))

	var out strings.Builder
	for row := 0; row < cityRows; row++ {
		distFromBottom := cityRows - row
		for col := 0; col < cityCols; col++ {
			b := buildingAt[col]
			if b != nil && distFromBottom <= b.h {
				// Solid background fill with a regular window grid punched
				// out — every odd row/column relative to the building's own
				// origin is a black window, so the pattern is symmetric and
				// static rather than random noise. The outline (left/right
				// edge columns and the roofline) is never punched, so the
				// building always reads as a solid rectangle.
				isEdge := col == b.x0 || col == b.x0+b.w-1 || distFromBottom == b.h || distFromBottom == 1
				if !isEdge && (col-b.x0)%2 == 1 && row%2 == 1 {
					out.WriteByte(' ')
					continue
				}
				hex := hslHex(b.hue, 0.72, 0.58)
				out.WriteString(styleFor(hex).Render(" "))
			} else if daylight {
				// It's currently daytime in this city's country — a plain
				// blue sky reads more truthfully than the default starry
				// night look. No stars, no other change.
				out.WriteString(daySkyStyle.Render(" "))
			} else {
				// Sky: almost every cell stays blank. The rare star holds a
				// fixed position and only pulses slowly, so motion reads as
				// "occasionally a star blinks" rather than constant static.
				seed := pseudoHash(col*31, row*17)
				if seed%14 != 0 {
					out.WriteByte(' ')
					continue
				}
				phase := math.Sin(m.cubeAngle*0.1 + float64(seed%991)*0.05)
				if phase < 0.4 {
					out.WriteByte(' ')
					continue
				}
				ch := "."
				if phase > 0.8 {
					ch = "*"
				}
				out.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#EEF3FF")).Render(ch))
			}
		}
		out.WriteByte('\n')
	}
	ground := lipgloss.NewStyle().Foreground(lipgloss.Color("#2BE3A6")).Render(strings.Repeat("‾", cityCols))
	out.WriteString(ground)
	return out.String()
}

// renderCountrysideScene draws a daytime grass/river/sky scene — used for
// the smaller/secondary place names in cityNames (index >= cityMajorCount)
// that read more as a small town or countryside than a skyline. Same
// cityRows x cityCols grid as the skyline, laid out top (sky) to bottom
// (grass), with a full-width river band a couple rows above the ground.
func (m model) renderCountrysideScene() string {
	skyRows := 2
	riverRow := skyRows + 1 // one hill row, then the river
	bgStyle := map[string]lipgloss.Style{}
	styleFor := func(hex string) lipgloss.Style {
		if s, ok := bgStyle[hex]; ok {
			return s
		}
		s := lipgloss.NewStyle().Background(lipgloss.Color(hex))
		bgStyle[hex] = s
		return s
	}
	fgStyle := map[string]lipgloss.Style{}
	glyphFor := func(bgHex, fgHex string) lipgloss.Style {
		key := bgHex + fgHex
		if s, ok := fgStyle[key]; ok {
			return s
		}
		s := lipgloss.NewStyle().Background(lipgloss.Color(bgHex)).Foreground(lipgloss.Color(fgHex))
		fgStyle[key] = s
		return s
	}

	// Sun/moon position is stable for the scene's lifetime (seeded off the
	// city index, not time), so it doesn't jump around every regen tick.
	sunSeed := pseudoHash(cityIndex, 99)
	sunCol := int(sunSeed % uint32(cityCols))
	daylight := cityIsDaylight()

	var out strings.Builder
	for row := 0; row < cityRows; row++ {
		switch {
		case row < skyRows:
			if daylight {
				skyHex := "#5AA9D6"
				for col := 0; col < cityCols; col++ {
					if col == sunCol && row == 0 {
						out.WriteString(glyphFor(skyHex, "#FFE9A8").Render("@"))
						continue
					}
					seed := pseudoHash(col*13, row*29)
					if seed%23 == 0 {
						out.WriteString(glyphFor(skyHex, "#FFFFFF").Render("~"))
						continue
					}
					out.WriteString(styleFor(skyHex).Render(" "))
				}
			} else {
				// Night: dark sky, a static moon, and slow-pulsing stars —
				// mirrors the skyline's night treatment for consistency.
				skyHex := "#0B1E33"
				for col := 0; col < cityCols; col++ {
					if col == sunCol && row == 0 {
						out.WriteString(glyphFor(skyHex, "#E8ECF5").Render("☾"))
						continue
					}
					seed := pseudoHash(col*31, row*17)
					if seed%14 != 0 {
						out.WriteString(styleFor(skyHex).Render(" "))
						continue
					}
					phase := math.Sin(m.cubeAngle*0.1 + float64(seed%991)*0.05)
					if phase < 0.4 {
						out.WriteString(styleFor(skyHex).Render(" "))
						continue
					}
					ch := "."
					if phase > 0.8 {
						ch = "*"
					}
					out.WriteString(glyphFor(skyHex, "#EEF3FF").Render(ch))
				}
			}
		case row == skyRows:
			// Hill line with tree silhouettes poking above the grass.
			hillHex := "#4C8C4A"
			for col := 0; col < cityCols; col++ {
				seed := pseudoHash(col*7, 3)
				if seed%9 == 0 {
					out.WriteString(glyphFor(hillHex, "#1F5C2E").Render("▲"))
					continue
				}
				out.WriteString(styleFor(hillHex).Render(" "))
			}
		case row == riverRow:
			// River flows: the shimmer pattern scrolls with m.cubeAngle
			// instead of just blinking in place, so it actually reads as
			// moving water rather than static texture.
			riverHex := "#2E86DE"
			offset := int(m.cubeAngle * 6)
			for col := 0; col < cityCols; col++ {
				if ((col+offset)%7 == 0) || ((col+offset*2)%11 == 0) {
					out.WriteString(glyphFor(riverHex, "#BEE7F5").Render("~"))
					continue
				}
				out.WriteString(styleFor(riverHex).Render(" "))
			}
		default:
			// Grass, darker toward the bottom, with static blade ticks.
			depth := row - riverRow
			grassHex := "#3D8B40"
			bladeHex := "#79C36B"
			if depth >= 2 {
				grassHex, bladeHex = "#2F6B3B", "#5CAE52"
			}
			for col := 0; col < cityCols; col++ {
				seed := pseudoHash(col*17, row*31)
				if seed%3 == 0 {
					ch := "'"
					if seed%2 == 0 {
						ch = "/"
					}
					out.WriteString(glyphFor(grassHex, bladeHex).Render(ch))
					continue
				}
				out.WriteString(styleFor(grassHex).Render(" "))
			}
		}
		out.WriteByte('\n')
	}
	ground := lipgloss.NewStyle().Foreground(lipgloss.Color("#2F6B3B")).Render(strings.Repeat("‾", cityCols))
	out.WriteString(ground)
	return out.String()
}

func (m model) renderBanner() string {
	return m.renderCityBanner()
}

func (m model) renderBody() string {
	box := lipgloss.NewStyle().Padding(1, 2)

	switch m.view {
	case viewListScan:
		return box.Render(m.renderCatalogProgress())
	case viewListTable:
		return box.Render(m.renderCatalogTable())
	case viewPs:
		return box.Render(m.renderPsTable())
	case viewShowScan:
		return box.Render(m.renderScanProgress())
	case viewShowTable:
		return box.Render(m.renderTable())
	case viewDeviceInfo:
		return box.Render(m.renderDeviceInfo())
	case viewHelpMenu:
		return box.Render(m.renderHelpMenu())
	case viewTavilySettings:
		return box.Render(m.scrollHelpBody(m.renderTavilySettings()))
	case viewTelegramSettings:
		return box.Render(m.scrollHelpBody(m.renderTelegramSettings()))
	case viewAutopilot:
		return box.Render(m.renderAutopilot())
	case viewEmailSettings:
		return box.Render(m.scrollHelpBody(m.renderEmailSettings()))
	case viewBackupSettings:
		return box.Render(m.scrollHelpBody(m.renderBackupSettings()))
	case viewBackupBrowser:
		return box.Render(m.scrollHelpBody(m.renderBackupBrowser()))
	case viewWebServerSettings:
		return box.Render(m.renderWebServerSettings())
	case viewWebServerModelSelect:
		return box.Render(m.renderWebServerModelSelect())
	case viewHelpText:
		return box.Render(m.scrollHelpBody(renderHelpText()))
	case viewDisclaimerText:
		return box.Render(m.scrollHelpBody(renderDisclaimerText()))
	case viewLogText:
		// No top padding here (unlike the shared `box`) — Search must be
		// the literal first row right under the title bar, not pushed
		// down by a blank padding line first.
		return lipgloss.NewStyle().Padding(0, 2).Render(m.scrollHelpBody(m.renderLogText()))
	case viewUpdateText:
		return box.Render(m.renderUpdateText())
	case viewWizard:
		return box.Render(m.renderWizard())
	case viewFirstRunDisclaimer:
		return box.Render(renderFirstRunDisclaimer())
	case viewOllamaInstallPrompt:
		return box.Render(m.renderOllamaInstallPrompt())
	case viewAgentChat:
		// No vertical padding here (unlike the shared `box`): the agentic
		// chat view sizes its own scrollback precisely to the terminal
		// height, so an extra top/bottom pad line would just show up as an
		// unexplained gap above the footer.
		return lipgloss.NewStyle().Padding(0, 2).Render(m.renderAgentChat())
	case viewToolCategories:
		if m.toolDetailOpen {
			return lipgloss.NewStyle().Padding(1, 2).Render(renderToolDetail(m.selectedToolName()))
		}
		content, _ := m.renderToolCategories()
		return lipgloss.NewStyle().Padding(1, 2).Render(m.scrollHelpBody(content))
	case viewAgentHelp:
		return lipgloss.NewStyle().Padding(1, 2).Render(renderAgentHelp())
	}

	var b strings.Builder
	b.WriteString(m.renderBanner())
	b.WriteString("\nWelcome to llama-shell\n\n")
	for i, it := range menuItems {
		line := fmt.Sprintf("[%s] %s", it.key, it.label)
		if i == m.menuCursor {
			b.WriteString(selectedStyle.Render("> " + line))
		} else {
			b.WriteString(unselectedStyle.Render("  " + line))
		}
		b.WriteString("\n")
	}
	b.WriteString("\nUp/Down + Enter, or press the letter shown.\n")
	return box.Render(b.String())
}

var redStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF0000"))

// renderBenchTable builds a NAME/CPU-GPU/MATCH% table for the first count
// entries of m.scanRows that have been benchmarked. Used both while the
// benchmark is running (grows live) and on the done/cancelled screen.
func (m model) renderBenchTable(count int) string {
	if count <= 0 {
		return ""
	}
	cols := []string{"NAME", "CPU/GPU", "MATCH%"}
	widths := []int{len(cols[0]), len(cols[1]), len(cols[2])}
	rowsData := make([][]string, 0, count)
	for i := 0; i < count && i < len(m.scanRows); i++ {
		r := m.scanRows[i]
		if !r.Benchmarked {
			continue
		}
		row := []string{r.Name, formatCPUGPU(r.CPUGPU), fmt.Sprintf("%d%%", r.MatchScore)}
		for j, v := range row {
			if len(v) > widths[j] {
				widths[j] = len(v)
			}
		}
		rowsData = append(rowsData, row)
	}
	if len(rowsData) == 0 {
		return ""
	}

	pad := func(s string, w int) string {
		if len(s) >= w {
			return s
		}
		return s + strings.Repeat(" ", w-len(s))
	}

	var b strings.Builder
	var headerLine strings.Builder
	for i, c := range cols {
		headerLine.WriteString(pad(c, widths[i]))
		headerLine.WriteString("  ")
	}
	b.WriteString(headerRowStyle.Render(headerLine.String()))
	b.WriteString("\n")
	for _, row := range rowsData {
		var line strings.Builder
		for i, v := range row {
			line.WriteString(pad(v, widths[i]))
			line.WriteString("  ")
		}
		b.WriteString(line.String())
		b.WriteString("\n")
	}
	return b.String()
}

func (m model) renderHelpMenu() string {
	var b strings.Builder
	b.WriteString("help\n\n")
	for i, it := range helpMenuItems {
		line := fmt.Sprintf("[%s] %s", it.key, it.label)
		if i == m.helpMenuCursor {
			b.WriteString(selectedStyle.Render("> " + line))
		} else {
			b.WriteString(unselectedStyle.Render("  " + line))
		}
		b.WriteString("\n")
	}
	b.WriteString("\nUp/Down + Enter, or press the letter shown. Esc: back to main menu.\n")
	return b.String()
}

// renderTavilySettings shows the current key status (masked, if set) and
// lets the user type a new one to save. Tavily (tavily.com) is a search +
// web-scraping API built for LLM agents — tavily_search returns ranked
// results with real content snippets, tavily_extract turns a URL into
// clean article text (bypassing cookie walls/JS shells read_webpage
// can't). Both tools need this key to do anything.
// hyperlink wraps text in an OSC 8 terminal hyperlink escape (so terminals
// that support it, like Windows Terminal, make it Ctrl+click-able) styled
// with the given color.
func hyperlink(url string, style lipgloss.Style) string {
	return "\x1b]8;;" + url + "\x1b\\" + style.Render(url) + "\x1b]8;;\x1b\\"
}

func (m model) renderTavilySettings() string {
	var b strings.Builder
	b.WriteString("tavily API key\n\n")

	current := os.Getenv("TAVILY_API_KEY")
	if current != "" && !m.tavilyEditing {
		greyed := lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))
		masked := current
		if len(masked) > 8 {
			masked = masked[:4] + strings.Repeat("*", len(masked)-8) + masked[len(masked)-4:]
		}
		b.WriteString(helpKeyStyle.Render("● configured") + "\n\n")
		b.WriteString(greyed.Render("Key: "+masked) + "\n")
		if m.tavilyKeyMsg != "" {
			b.WriteString("\n" + m.tavilyKeyMsg + "\n")
		}
		b.WriteString("\n[r] reconfigure    Esc: back\n")
		return b.String()
	}

	b.WriteString("HOW TO GET A KEY (takes 2 minutes, free):\n")
	b.WriteString("  1. Open " + hyperlink("https://app.tavily.com/", greenLinkStyle) + " and sign up (email or Google).\n")
	b.WriteString("  2. It opens straight to \"API Playground\". In the \"API key\" box, top\n")
	b.WriteString("     right, click the eye icon to reveal it, then the copy icon next to it\n")
	b.WriteString("     (it looks like tvly-dev-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx).\n")
	b.WriteString("  3. Come back here, right-click to paste it into the box below, then\n")
	b.WriteString("     press Enter to save.\n\n")
	b.WriteString("WHAT THIS IS FOR:\n")
	b.WriteString("Tavily (" + hyperlink("https://www.tavily.com/", greenLinkStyle) + ") is a search + web-scraping service built for\n")
	b.WriteString("AI agents. Setting a key here enables two extra tools in agentic chat:\n")
	b.WriteString("  tavily_search   — web search with real result content, not just snippets\n")
	b.WriteString("  tavily_extract  — scrape a URL's clean article text (handles pages\n")
	b.WriteString("                    read_webpage can't: cookie walls, JS-only shells)\n")
	b.WriteString("Nothing else in this app needs a key — skip this screen if you don't want it.\n\n")

	if current != "" {
		masked := current
		if len(masked) > 8 {
			masked = masked[:4] + strings.Repeat("*", len(masked)-8) + masked[len(masked)-4:]
		}
		b.WriteString(helpKeyStyle.Render("current key (reconfiguring): "+masked) + "\n\n")
	} else {
		b.WriteString(agentToolStyle.Render("no key set — tavily_search/tavily_extract will return an error until one is") + "\n\n")
	}

	b.WriteString(fmt.Sprintf("new key: %s_\n", m.tavilyKeyInput))
	if m.tavilyKeyMsg != "" {
		b.WriteString("\n" + m.tavilyKeyMsg + "\n")
	}
	b.WriteString("\nEnter: save   Esc: back (discards what you typed)\n")
	return b.String()
}

func (m model) renderTelegramSettings() string {
	var b strings.Builder
	b.WriteString("telegram bot\n\n")

	cfg := loadTelegramConfig()
	running := isTelegramRunning()

	if cfg.Token != "" && !m.telegramEditing {
		greyed := lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))
		masked := maskAppPassword(cfg.Token)
		b.WriteString(helpKeyStyle.Render("● configured") + "\n\n")
		b.WriteString(greyed.Render("Token: "+masked) + "\n")
		if running {
			b.WriteString(greyed.Render(fmt.Sprintf("Running — model: %s", cfg.Model)))
			if cfg.ChatID != 0 {
				b.WriteString(greyed.Render(fmt.Sprintf("  —  bound to chat %d", cfg.ChatID)) + "\n")
			} else {
				b.WriteString("\n" + agentToolStyle.Render("Not bound yet — message the bot on Telegram to bind it.") + "\n")
			}
		} else {
			b.WriteString(redStyle.Render("Saved but not running.") + "\n")
		}
		if m.telegramMsg != "" {
			b.WriteString("\n" + m.telegramMsg + "\n")
		}
		b.WriteString("\n[r] reconfigure    Esc: back\n")
		return b.String()
	}

	if running {
		// Already set up — the step-by-step guide is just noise once it's
		// working, so show a short status instead of repeating it.
		b.WriteString(helpKeyStyle.Render("● running") + fmt.Sprintf("  —  model: %s", cfg.Model))
		if cfg.ChatID != 0 {
			b.WriteString(fmt.Sprintf("  —  bound to chat %d\n\n", cfg.ChatID))
		} else {
			b.WriteString("\n\nNot bound yet: open a chat with your bot on Telegram and send it any\nmessage — that's what binds it.\n\n")
		}
		b.WriteString("Paste a new token below to switch bots, or clear the box and press\nEnter to disable.\n\n")
	} else {
		b.WriteString("3 steps:\n")
		b.WriteString("  1. In Telegram, message " + hyperlink("https://t.me/BotFather", greenLinkStyle) + " with  /newbot\n")
		b.WriteString("     then answer its two questions (a name, then a username ending\n")
		b.WriteString("     in \"bot\").\n")
		b.WriteString("  2. It replies with a token. Copy it, paste it below, press Enter.\n")
		b.WriteString("  3. Open a chat with your new bot on Telegram and send it any\n")
		b.WriteString("     message — that binds it to you (and locks out everyone else).\n\n")
		b.WriteString(agentToolStyle.Render("(needs a local model already installed — [w] setup wizard if you don't have one)") + "\n\n")
		if cfg.Token != "" {
			b.WriteString(redStyle.Render("● token saved but not running — re-paste it below to restart") + "\n\n")
		} else {
			b.WriteString(agentToolStyle.Render("○ disabled — no token set") + "\n\n")
		}
	}

	b.WriteString(fmt.Sprintf("new token: %s_\n", m.telegramTokenInput))
	if m.telegramMsg != "" {
		b.WriteString("\n" + m.telegramMsg + "\n")
	}
	b.WriteString("\nEnter: save (empty box + Enter disables)   Esc: back (discards what you typed)\n")
	return b.String()
}

// maskAppPassword shows only the last 4 characters — enough to recognize
// which password is saved without ever fully displaying it on screen.
func maskAppPassword(s string) string {
	if s == "" {
		return ""
	}
	n := len(s)
	if n <= 4 {
		return strings.Repeat("•", n)
	}
	return strings.Repeat("•", n-4) + s[n-4:]
}

func (m model) renderEmailSettings() string {
	var b strings.Builder
	b.WriteString("email (Gmail)\n\n")
	cfg := loadEmailConfig()

	configured := cfg.FromAddress != "" && !m.emailEditing && !m.emailSending
	if configured {
		greyed := lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))
		b.WriteString(helpKeyStyle.Render("● configured") + "\n\n")
		b.WriteString(greyed.Render(fmt.Sprintf("Gmail address: %s", cfg.FromAddress)) + "\n")
		displayName := cfg.DisplayName
		if displayName == "" {
			displayName = "(none)"
		}
		b.WriteString(greyed.Render(fmt.Sprintf("Display name:  %s", displayName)) + "\n")
		b.WriteString(greyed.Render("App password:  XXXXXXX") + "\n")
		if m.emailMsg != "" {
			b.WriteString("\n" + m.emailMsg + "\n")
		}
		b.WriteString("\n[r] reconfigure    Esc: back\n")
		return b.String()
	}

	b.WriteString("HOW TO GET A GMAIL APP PASSWORD (takes ~2 minutes):\n")
	b.WriteString("  1. Your Google account needs 2-Step Verification turned on first:\n")
	b.WriteString("     " + hyperlink("https://myaccount.google.com/security", greenLinkStyle) + "\n")
	b.WriteString("  2. Then generate an App Password here:\n")
	b.WriteString("     " + hyperlink("https://myaccount.google.com/apppasswords", greenLinkStyle) + "\n")
	b.WriteString("     Name it anything (e.g. \"llama-shell\") and click Create.\n")
	b.WriteString("  3. Google shows a 16-character code (spaces don't matter) — copy it,\n")
	b.WriteString("     that's the App Password below, NOT your normal Gmail password.\n\n")
	b.WriteString(agentToolStyle.Render(fmt.Sprintf("SMTP server: %s:%s (STARTTLS) — fixed, this is Gmail's real one", emailSMTPHost, emailSMTPPort)) + "\n\n")

	cursorFor := func(field int) string {
		if m.emailFieldFocus == field {
			return "█"
		}
		return ""
	}
	styleFor := func(field int) lipgloss.Style {
		if m.emailFieldFocus == field {
			return helpKeyStyle
		}
		return unselectedStyle
	}
	b.WriteString(styleFor(0).Render(fmt.Sprintf("Gmail address: %s%s", m.emailAddrInput, cursorFor(0))) + "\n")
	b.WriteString(styleFor(1).Render(fmt.Sprintf("Display name:  %s%s", m.emailDisplayNameInput, cursorFor(1))) + agentToolStyle.Render(" (optional)") + "\n")
	b.WriteString(styleFor(2).Render(fmt.Sprintf("App password:  %s%s", maskAppPassword(m.emailPassInput), cursorFor(2))) + "\n")

	if m.emailSending {
		b.WriteString("\nSending a test email to confirm it works...\n")
	} else if m.emailMsg != "" {
		b.WriteString("\n" + m.emailMsg + "\n")
	}
	b.WriteString("\nTab: switch field   Enter: save + send test email   Esc: back\n")
	return b.String()
}

// renderBackupSettings shows either the export/import chooser or the
// path+password form for whichever one was picked. There's no
// "configured" summary here (unlike email/tavily/telegram) — backup
// isn't a persistent credential, it's a one-shot action you take
// whenever you want a fresh export or a restore.
func (m model) renderBackupSettings() string {
	var b strings.Builder
	b.WriteString("backup / restore\n\n")
	b.WriteString("Exports every setting in this app (email, telegram, tavily, web\n")
	b.WriteString("server, auto-update, disclaimer, etc.) into one encrypted file — or\n")
	b.WriteString("restores them from one made earlier. AES-256-GCM, so the file isn't\n")
	b.WriteString("plain text if opened or edited directly. No password to set or\n")
	b.WriteString("remember — " + agentToolStyle.Render("this protects against casual viewing/editing, not a\ndetermined attacker with this app's source code.") + "\n\n")
	b.WriteString(helpKeyStyle.Render("[e]") + " export settings to a new encrypted file\n")
	b.WriteString(helpKeyStyle.Render("[i]") + " import settings from an encrypted file (overwrites current settings)\n")

	if m.backupMsg != "" {
		b.WriteString("\n" + m.backupMsg + "\n")
	}
	b.WriteString("\nEsc: back\n")
	return b.String()
}

// renderBackupBrowser draws the in-terminal directory browser used by
// both export (with an editable destination filename below the list)
// and import (Enter on a .lsb file restores it immediately).
func (m model) renderBackupBrowser() string {
	var b strings.Builder
	verb := "Export to"
	if m.backupMode == "import" {
		verb = "Import from"
	}
	b.WriteString(helpKeyStyle.Render(verb) + "\n")
	b.WriteString(agentToolStyle.Render(m.backupBrowseDir) + "\n\n")

	for i, e := range m.backupBrowseEntries {
		line := e.name
		if e.isDir {
			line += "/"
		}
		style := unselectedStyle
		if i == m.backupBrowseCursor && !m.backupBrowseEditingName {
			style = helpKeyStyle
			line = "> " + line
		} else {
			line = "  " + line
		}
		b.WriteString(style.Render(line) + "\n")
	}
	if len(m.backupBrowseEntries) == 0 {
		b.WriteString(agentToolStyle.Render("  (empty directory)") + "\n")
	}

	if m.backupMode == "export" {
		cursor := ""
		nameStyle := unselectedStyle
		if m.backupBrowseEditingName {
			cursor = "█"
			nameStyle = helpKeyStyle
		}
		b.WriteString("\n" + nameStyle.Render(fmt.Sprintf("File name: %s%s", m.backupBrowseFilename, cursor)) + "\n")
	}

	if m.backupMsg != "" {
		b.WriteString("\n" + m.backupMsg + "\n")
	}

	if m.backupMode == "export" {
		b.WriteString("\nUp/Down: move   Enter: open folder / edit name   Tab: switch to file name   Enter on name: save   Esc: cancel\n")
	} else {
		b.WriteString("\nUp/Down: move   Enter: open folder / import file   Esc: cancel\n")
	}
	return b.String()
}

// parseHHMM parses a 24-hour "HH:MM" string, tolerating single-digit hours
// (e.g. "3:00"). Used for the auto-update check-time setting.
func parseHHMM(s string) (hour, minute int, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	mi, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || h < 0 || h > 23 || mi < 0 || mi > 59 {
		return 0, 0, false
	}
	return h, mi, true
}

func (m model) renderAutopilot() string {
	var b strings.Builder
	b.WriteString(helpKeyStyle.Render("autopilot") + "\n\n")
	switch m.autopilotPhase {
	case "installing_ollama":
		b.WriteString("Ollama isn't installed — opening the installer download page...\n")
	case "waiting_for_manual_install":
		b.WriteString(m.autopilotMsg + "\n\n")
		b.WriteString(redStyle.Render("Windows/macOS installers aren't scriptable — finish the install, "+
			"then relaunch llama-shell and run autopilot again.") + "\n")
	case "pulling_model":
		b.WriteString(fmt.Sprintf("Downloading %s (this app's default model)... this can take a while.\n", autopilotModel))
	case "error":
		b.WriteString(redStyle.Render(m.autopilotMsg) + "\n")
	case "done":
		b.WriteString(helpKeyStyle.Render("● ready") + "\n\n")
		b.WriteString(fmt.Sprintf("%s installed, web server enabled with model %s:\n\n", autopilotModel, autopilotModel))
		b.WriteString("  " + hyperlink(m.autopilotMsg, greenLinkStyle) + "\n")
	}
	b.WriteString("\nEsc: back to main menu\n")
	return b.String()
}

func (m model) renderWebServerSettings() string {
	var b strings.Builder
	b.WriteString("web server\n\n")
	b.WriteString("WHAT THIS DOES:\n")
	b.WriteString("Runs the same agentic chat (all tools: files, commands, web, etc.) as a\n")
	b.WriteString("page in any browser, instead of only in this terminal — including a\n")
	b.WriteString("phone on the same WiFi, since it binds all network interfaces, not just\n")
	b.WriteString("this machine. NOT reachable from outside your local network (no tunnel\n")
	b.WriteString("is set up). Gated behind a random access token baked into the URL —\n")
	b.WriteString("without it every request gets a 403 — since anyone who can open it gets\n")
	b.WriteString("the same full tool access you have here (reading/writing files, running\n")
	b.WriteString("commands, etc). Your Windows Firewall may prompt to allow this the\n")
	b.WriteString("first time; allow it if you want LAN/phone access to work.\n\n")

	cfg := loadWebServerConfig()
	port := webServerEffectivePort(cfg)
	running := isWebServerRunning()
	switch {
	case running:
		b.WriteString(helpKeyStyle.Render("● running") + fmt.Sprintf("  —  model: %s\n\n", cfg.Model))
		b.WriteString("  " + hyperlink(webServerURL(cfg.Token, port), greenLinkStyle) + "\n\n")
	case cfg.Enabled:
		b.WriteString(redStyle.Render("● enabled in settings, but not running right now") + "\n")
		webServerMu.Lock()
		lastErr := webServerLastErr
		webServerMu.Unlock()
		if lastErr != "" {
			b.WriteString(redStyle.Render("  reason: "+lastErr) + "\n")
		}
		b.WriteString("  press " + helpKeyStyle.Render("[e]") + " to retry starting it\n\n")
	default:
		b.WriteString(agentToolStyle.Render("○ disabled") + "\n\n")
	}

	if m.webServerAwaitingDL {
		b.WriteString(redStyle.Render("no local model installed.") + "\n")
		b.WriteString("Download gemma4:e2b (this app's default, ~7 GB) now?\n\n")
		b.WriteString(helpKeyStyle.Render("[y] yes") + "    " + helpKeyStyle.Render("[n] no") + "\n")
		return b.String()
	}
	if m.webServerBusy {
		b.WriteString(m.webServerMsg + "\n")
		return b.String()
	}

	if m.webServerEditingPort {
		cursor := ""
		if len(m.webServerPortInput) < 5 {
			cursor = "█"
		}
		b.WriteString(helpKeyStyle.Render(fmt.Sprintf("Port: %s%s", m.webServerPortInput, cursor)) + "\n")
		b.WriteString(agentToolStyle.Render(fmt.Sprintf("(currently %d)", port)) + "\n\n")
		b.WriteString("Enter: save   Esc: cancel\n")
		return b.String()
	}

	b.WriteString(fmt.Sprintf("Port: %d\n\n", port))
	if m.webServerMsg != "" {
		b.WriteString(m.webServerMsg + "\n\n")
	}
	b.WriteString(helpKeyStyle.Render("[e] enable") + " (choose/confirm model)    " + helpKeyStyle.Render("[d] disable") + "    " + helpKeyStyle.Render("[p] port") + "\n")
	b.WriteString("Esc: back\n")
	return b.String()
}

func (m model) renderWebServerModelSelect() string {
	var b strings.Builder
	b.WriteString("web server — choose a model\n\n")
	for i, name := range m.webServerModelList {
		line := name
		if i == m.webServerModelCursor {
			b.WriteString(selectedStyle.Render("> " + line))
		} else {
			b.WriteString(unselectedStyle.Render("  " + line))
		}
		b.WriteString("\n")
	}
	b.WriteString("\nUp/Down + Enter to confirm and start the server. Esc: cancel.\n")
	return b.String()
}

func renderHelpText() string {
	body := styleHelpLines(`help

llama-shell is a terminal UI shell for ollama.
Project: __GITHUB_LINK__

Every screen supports Up/Down + Enter for navigation, in addition to the
letter shortcut shown in brackets. Esc goes back one level; ctrl+c always
quits immediately.

Main menu
  l  list models     - browse ollama (local + library) and huggingface,
                        search by typing, Enter to download a model
  p  running models   - live ollama ps, Enter to stop a selected model
  s  show model info  - full details for every installed model, Enter
                        to run/remove/stop a selected one
  d  device info      - this machine's specs; 'b' benchmarks every
                        installed model's CPU/GPU split
  h  this menu        - help, disclaimer, activity log
  q  quit

List models
  type       filter the catalog by name, Esc clears the search box
  Enter      download the selected model (size picker first if it has
             more than one size)
  alt+r      rescan the catalog, ignoring the cache
  alt+h      jump straight to this help screen
  c          cancel an in-progress download
  Esc        back to main menu (or clear the search box first)
  CAPABILITIES column: see the "CAPABILITIES column" table further down
             this screen for what each 3-letter code means.

Running models
  Enter      stop the selected model (confirm y/n)
  r          refresh the list
  Esc / q    back to main menu / quit

Show model info
  Enter      open the action menu for the selected model:
               a = start agentic chat, x = run interactively,
               r = remove, k = stop
  r          rescan installed models, ignoring the cache (outside the
             action menu)
  alt+h      jump straight to this help screen
  Esc / q    back to main menu / quit
  CAPABILITIES column: see the "CAPABILITIES column" table further down
             this screen for what each 3-letter code means.

Device info
  b          benchmark every installed model's CPU/GPU split
  r          refresh device info, ignoring the cache
  c          cancel a running benchmark (partial results are kept)
  Esc / q    back to main menu / quit

Tips
  - "list models" search box: just start typing to filter, Esc clears it.
  - Downloads show a live progress bar; press 'c' to cancel mid-download.
  - The device-info benchmark loads and unloads every model in turn to
    measure its CPU/GPU split - it's slow by nature; cancel any time with
    'c' and whatever was measured so far is kept.

This screen
  - Reached by pressing 'h' from the main menu. It also opens directly
    with Alt+H from "list models" and "show model info" (Esc from here
    goes to this help menu, not back to that screen).
  - Alt+H means something different inside agentic chat: there it opens
    that screen's own keys reference ("Agentic chat - keys"), not this
    screen.

CAPABILITIES column (list models / show model info)
  Each ollama-reported capability is truncated to its first 3 letters.

  CODE  WORD        MEANING
  com   completion  plain text generation - can hold a normal conversation
  too   tools       can call functions (file/shell access in agentic chat)
  ins   insert      can fill in the middle of existing text (code infill)
  vis   vision      can accept and understand image input
  emb   embedding   turns text into vectors (search/RAG, not chat)
  thi   thinking    supports an explicit reasoning/"thinking" step
  aud   audio       can accept and understand audio input

  A bare "-" means no capability info yet (cache still warming up) or
  ollama reports none for that model.

Agentic chat (started from show model info -> 'a')
  A local coding assistant with real read/write file system access,
  scoped to the working directory it was started from (absolute paths
  can still reach anywhere on disk).
  Enter        send your typed message
  Up / Down    scroll history one line (works while the model is busy)
  PgUp / PgDn  scroll history one page
  Home / End   jump to top / bottom of history
  Alt+V        paste - an image on the clipboard is attached directly
               (vision-capable models can then see it); otherwise pastes
               clipboard text
  Alt+T        browse all available tools by category
  Alt+H        this chat's own keys reference (see below)
  Alt+M        cycle tool mode: auto -> on -> off -> auto (see below)
  Esc          back to the show-model-info action menu
  Ctrl+C       quit llama-shell
  Typing or pasting a path to an existing image file (.png/.jpg/.jpeg/
  .gif/.bmp/.webp) in your message also attaches it, same as Alt+V.

  Tool mode (Alt+M cycles it; shown briefly above the input line):
    auto (default) - tools stay on normally, but are automatically
             turned off for any single message that attaches an image.
             Some models (confirmed for at least gemma4:e2b) garble the
             image entirely when the request also lists tools, so a
             message can reliably have working vision or working tools,
             not both - auto picks vision whenever an image is present,
             then goes back to tools on the next message.
    on       always try tools, even on a message with an image attached
             (accept the vision-breaking tradeoff above, e.g. because
             you need the agent to call a tool this turn regardless).
    off      never send tools this chat - plain conversation only, no
             file/system access, regardless of what the model supports.

Tool categories (Alt+T from agentic chat)
  Lists every tool the agent can call, numbered and grouped by what it
  does (files, shell/processes, networking, system/environment,
  git/ollama, vision/media, data, open/launch).
  Tab / Shift+Tab  select next / previous tool
  Enter            show that tool's description and two example prompts
  Esc              close the detail view, or back to chat from the list
  Alt+T            back to chat (from the list)
  Ctrl+C           quit llama-shell

Agent help (Alt+H from agentic chat)
  Same keys reference as above, shown as its own screen.
  Esc            back to chat
  Ctrl+C         quit llama-shell

Esc: back
`)
	linkStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#5FD7FF")).Underline(true).Render("GitHub")
	link := "\x1b]8;;https://github.com/affigabmag/llama-shell\x1b\\" + linkStyle + "\x1b]8;;\x1b\\"
	return strings.Replace(body, "__GITHUB_LINK__", link, 1)
}

const disclaimerBody = `llama-shell is an independent, unofficial tool. It is not affiliated
with, endorsed by, or sponsored by Ollama Inc., Hugging Face, or any
model author listed in its catalogs. "Ollama" and any other product
names referenced are the property of their respective owners.

License. This software is source-available for viewing and personal
use only. You may read the code and run it, but you may not modify it
or redistribute a modified version - see the LICENSE file in the
repository for the exact terms.

No warranty. This software is provided "as is", without warranty of
any kind, express or implied, including but not limited to the
warranties of merchantability, fitness for a particular purpose, and
noninfringement. In no event shall the authors be liable for any
claim, damages, or other liability arising from, out of, or in
connection with the software or its use.

Use at your own risk - in particular, the "remove" and "stop" actions
call ollama directly and are not reversible from within this app.`

// disclaimerLeadPhrases are the attention-grabbing lead-ins of each
// disclaimer paragraph, rendered in red so they stand out from the
// surrounding legal boilerplate.
var disclaimerLeadPhrases = []string{
	"llama-shell is an independent, unofficial tool.",
	"License.",
	"No warranty.",
	"Use at your own risk",
}

func styledDisclaimerBody() string {
	body := disclaimerBody
	for _, p := range disclaimerLeadPhrases {
		body = strings.Replace(body, p, redStyle.Render(p), 1)
	}
	return body
}

func renderDisclaimerText() string {
	return redStyle.Render("disclaimer") + "\n\n" + styledDisclaimerBody() + "\n\nEsc: back\n"
}

func renderFirstRunDisclaimer() string {
	return redStyle.Render("disclaimer — please read before continuing") + "\n\n" + styledDisclaimerBody() +
		"\n\nYou must agree to continue.\n\n" +
		helpKeyStyle.Render("[a] I agree, continue") + "    " + redStyle.Render("[q] quit") + "\n"
}

func (m model) renderOllamaInstallPrompt() string {
	if m.ollamaInstallRunning {
		return redStyle.Render("ollama not installed") + "\n\n" +
			"Installing... this may take a minute.\n"
	}
	if m.ollamaInstallErr != "" {
		return redStyle.Render("ollama install failed") + "\n\n" + m.ollamaInstallErr +
			"\n\n(press any key to continue)\n"
	}
	if m.ollamaInstallResult != "" {
		return helpKeyStyle.Render("ollama") + "\n\n" + m.ollamaInstallResult +
			"\n\n(press any key to continue)\n"
	}
	return redStyle.Render("ollama not installed") + "\n\n" +
		"llama-shell talks to Ollama to list, run, and manage local models —\n" +
		"without it there's nothing to manage.\n\n" +
		"Install it now?\n\n" +
		helpKeyStyle.Render("[y] yes, install") + "    " + redStyle.Render("[n] no, I'll do it myself") + "\n"
}

func (m model) renderUpdateText() string {
	current := appVersion
	if current == "" {
		current = "dev"
	}

	if m.updateDownloading {
		return helpKeyStyle.Render("update") + "\n\n" +
			fmt.Sprintf("Downloading %s for %s/%s...\n", m.updateLatest, runtime.GOOS, runtime.GOARCH)
	}
	if m.updateResultErr != "" {
		return redStyle.Render("update failed") + "\n\n" + m.updateResultErr +
			"\n\n(press any key to continue)\n"
	}
	if m.updateResult != "" {
		return helpKeyStyle.Render("update") + "\n\n" + m.updateResult +
			"\n\n(press any key to continue)\n"
	}
	if m.autoUpdateEditingTime {
		return helpKeyStyle.Render("update") + "\n\n" +
			"new auto-update check time (24h HH:MM), then Enter to save, Esc to cancel:\n" +
			"> " + m.autoUpdateTimeInput + "█\n"
	}

	var b strings.Builder
	b.WriteString(helpKeyStyle.Render("update") + "\n\n")
	b.WriteString(fmt.Sprintf("current version : %s\n", current))
	b.WriteString(fmt.Sprintf("platform        : %s/%s\n\n", runtime.GOOS, runtime.GOARCH))

	switch {
	case !m.updateChecked:
		b.WriteString("Checking for updates...\n")
	case m.updateCheckErr != "":
		b.WriteString(redStyle.Render("couldn't check for updates:") + "\n" + m.updateCheckErr + "\n\n")
		b.WriteString("[r] retry\n")
	case m.updateAvailable:
		b.WriteString(fmt.Sprintf("latest version  : %s  ", m.updateLatest) + redStyle.Render("(update available)") + "\n\n")
		if m.updateAssetURL != "" {
			b.WriteString("[u] download and install\n")
		} else {
			b.WriteString(redStyle.Render(fmt.Sprintf("no release asset named %q for this platform", updateAssetName(runtime.GOOS, runtime.GOARCH))) + "\n")
		}
	case current == "dev":
		b.WriteString("running a dev build (no version tag) — can't compare against latest release.\n")
	default:
		b.WriteString(fmt.Sprintf("latest version  : %s  (up to date)\n", m.updateLatest))
	}

	b.WriteString("\n")
	autoCfg := loadAutoUpdateConfig()
	b.WriteString(helpKeyStyle.Render("auto-update") + "\n")
	if autoCfg.Enabled {
		b.WriteString(helpKeyStyle.Render("● enabled") + fmt.Sprintf("  —  daily check at %02d:%02d\n", autoCfg.Hour, autoCfg.Minute))
	} else {
		b.WriteString(agentToolStyle.Render("○ disabled") + fmt.Sprintf("  —  would check daily at %02d:%02d\n", autoCfg.Hour, autoCfg.Minute))
	}
	if m.autoUpdateMsg != "" {
		b.WriteString(m.autoUpdateMsg + "\n")
	}
	b.WriteString("[e] enable    [d] disable    [t] set check time\n")

	b.WriteString("\nEsc: back\n")
	return b.String()
}

func (m model) renderWizard() string {
	var b strings.Builder
	b.WriteString(helpKeyStyle.Render("setup wizard") + "\n\n")

	switch m.wizardPhase {
	case "ask":
		if m.wizardOllamaSkipNote != "" {
			b.WriteString("  " + m.wizardOllamaSkipNote + "\n")
		}
		for i := 0; i < m.wizardQIndex; i++ {
			q := m.wizardQuestions[i]
			ans := "no"
			if m.wizardAnswers[q.id] {
				ans = "yes"
			}
			b.WriteString(fmt.Sprintf("  %s  [%s]\n", q.prompt, ans))
		}
		if len(m.wizardQuestions) > 0 {
			b.WriteString("\n" + m.wizardQuestions[m.wizardQIndex].prompt + "\n\n")
		}
		b.WriteString(helpKeyStyle.Render("[y] yes") + "    " + helpKeyStyle.Render("[n] no") + "    " + redStyle.Render("[esc] cancel wizard") + "\n")

	case "blocked":
		b.WriteString(redStyle.Render("not enough disk space") + "\n\n")
		b.WriteString(m.wizardDiskMsg + "\n\n")
		b.WriteString("Free up space and open the wizard again — nothing was downloaded.\n\n")
		b.WriteString("(press any key to continue)\n")

	case "run":
		if m.wizardOllamaSkipNote != "" {
			b.WriteString("  " + m.wizardOllamaSkipNote + "\n")
		}
		if m.wizardDiskNeededBytes > 0 {
			b.WriteString(fmt.Sprintf("  disk: %.1f GB free, ~%.1f GB needed (incl. margin) — %s\n",
				float64(m.wizardDiskFreeBytes)/1e9, float64(m.wizardDiskNeededBytes)/1e9,
				helpKeyStyle.Render("OK")))
		}
		for _, l := range m.wizardLog {
			b.WriteString("  " + l + "\n")
		}
		if m.wizardActionIdx < len(m.wizardActions) {
			a := m.wizardActions[m.wizardActionIdx]
			b.WriteString("\n")
			switch a.kind {
			case "install_ollama":
				b.WriteString("Installing Ollama...\n")
			case "pull":
				const barWidth = 30
				pct := m.wizardPullPct
				if pct < 0 {
					pct = 0
				}
				filled := barWidth * pct / 100
				bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
				b.WriteString(fmt.Sprintf("Downloading %s\n[%s] %d%%\n%s\n", a.model, bar, pct, m.wizardPullLine))
			}
		}
		b.WriteString("\n" + redStyle.Render("[esc] abort") + "\n")

	case "done":
		if m.wizardCancelled {
			b.WriteString(redStyle.Render("wizard aborted") + "\n\n")
		} else {
			b.WriteString("wizard finished\n\n")
		}

		questionW := 0
		for _, q := range m.wizardQuestions {
			if len(q.prompt) > questionW {
				questionW = len(q.prompt)
			}
		}
		b.WriteString(headerRowStyle.Render(fmt.Sprintf("  %-*s  %s", questionW, "question", "answer")) + "\n")
		for _, q := range m.wizardQuestions {
			ans := "no"
			if m.wizardAnswers[q.id] {
				ans = "yes"
			}
			b.WriteString(fmt.Sprintf("  %-*s  %s\n", questionW, q.prompt, ans))
		}
		b.WriteString("\n")

		for _, l := range m.wizardLog {
			b.WriteString("  " + l + "\n")
		}
		switch {
		case m.wizardAnswers["tavily"] && m.wizardAnswers["telegram"]:
			b.WriteString("\n(press any key to continue to Tavily key setup, then Telegram bot setup)\n")
		case m.wizardAnswers["tavily"]:
			b.WriteString("\n(press any key to continue to Tavily key setup)\n")
		case m.wizardAnswers["telegram"]:
			b.WriteString("\n(press any key to continue to Telegram bot setup)\n")
		default:
			b.WriteString("\n(press any key to continue)\n")
		}
	}

	return b.String()
}

// logSearchLabelStyle renders the word "Search" in green so the log
// screen's search box reads as an actual UI control on row 1, not just
// text buried in a description line.
var logSearchLabelStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#5FFF5F"))

func (m model) renderLogText() string {
	label := logSearchLabelStyle.Render("Search")
	var searchRow, subRow string
	switch {
	case m.logSearchMode:
		searchRow = label + ": " + m.logQuery + "█"
		subRow = "(Enter to lock filter, Esc to clear/exit search)"
	case m.logQuery != "":
		searchRow = label + fmt.Sprintf(": %q", m.logQuery)
		subRow = "(/ to change, Esc on this screen to clear)"
	default:
		searchRow = label + ": " + lipgloss.NewStyle().Bold(true).Blink(true).Render("(press / to type)")
		subRow = fmt.Sprintf("activity log — most recent %d events", maxDisplayedLogLines)
	}
	header := searchRow + "\n" + subRow + "\n\n"

	lines := readLogLines(m.logQuery)
	body := "no matching log entries."
	if len(lines) > 0 {
		body = strings.Join(lines, "\n")
	}
	return header + body + "\n\nEsc: back\n"
}

func (m model) renderDeviceInfo() string {
	if m.benchDoneMsg != "" {
		var b strings.Builder
		b.WriteString("device info\n\n" + m.benchDoneMsg + "\n")
		if table := m.renderBenchTable(m.benchIndex); table != "" {
			b.WriteString("\n")
			b.WriteString(table)
		}
		b.WriteString("\n(press any key to continue)")
		return b.String()
	}

	if m.benchRunning {
		const barWidth = 40
		pct := 0
		if m.benchTotal > 0 {
			pct = m.benchIndex * 100 / m.benchTotal
		}
		filled := barWidth * pct / 100
		bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
		name := ""
		if m.benchIndex < len(m.scanRows) {
			name = m.scanRows[m.benchIndex].Name
		}
		status := "loading & measuring..."
		if m.benchCancelling {
			status = "cancelling..."
		}

		var b strings.Builder
		b.WriteString(fmt.Sprintf(
			"device info\n\nbenchmarking models — this takes a while (each model loads, is measured, then unloads)\n\n[%s] %d/%d\n\ncurrent: %s (%s)\n\n[c] cancel\n",
			bar, m.benchIndex, m.benchTotal, name, status,
		))

		if table := m.renderBenchTable(m.benchIndex); table != "" {
			b.WriteString("\nmeasured so far:\n\n")
			b.WriteString(table)
		}

		return b.String()
	}

	if m.benchConfirm {
		return redStyle.Render(
			"device info\n\nThis will load and unload EVERY installed model, one at a time,\n" +
				"to measure its CPU/GPU split. It can take a long time (each model\n" +
				"has to fully load, generate a token, then unload).\n\n" +
				"Continue?\n\n[y] yes   [n] no",
		)
	}

	if m.deviceLoading || m.deviceInfo == nil {
		return "device info\n\ngathering device info..."
	}
	d := m.deviceInfo

	var b strings.Builder
	b.WriteString("device info\n\n")
	b.WriteString(fmt.Sprintf("OS            %s/%s\n", d.OS, d.Arch))
	b.WriteString(fmt.Sprintf("CPU           %s\n", d.CPUModel))
	b.WriteString(fmt.Sprintf("Cores         %d (logical)\n", d.Cores))
	b.WriteString(fmt.Sprintf("RAM           %s\n", d.RAMTotal))
	b.WriteString("Disks\n")
	for _, disk := range d.Disks {
		b.WriteString("  " + disk + "\n")
	}
	b.WriteString("GPU\n")
	for _, gpu := range d.GPUs {
		b.WriteString("  " + gpu + "\n")
	}
	yellowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFF5F"))
	b.WriteString("\nEsc: back  r: refresh  ")
	b.WriteString(yellowStyle.Render("b: benchmark all models (cpu/gpu + match%)"))
	b.WriteString("\n")
	return b.String()
}

func (m model) renderScanProgress() string {
	if !m.ollama.installed {
		return "show model info\n\nollama is not installed or not on PATH.\n\nEsc: back"
	}
	if m.scanErr != "" {
		return fmt.Sprintf("show model info\n\nerror running ollama list:\n%s\nEsc: back", m.scanErr)
	}
	if m.scanTotal == 0 {
		return "show model info\n\nfetching model list..."
	}

	const barWidth = 40
	pct := float64(m.scanDone) / float64(m.scanTotal)
	filled := int(pct * barWidth)
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)

	var cur string
	if m.scanDone < len(m.scanRows) {
		cur = m.scanRows[m.scanDone].Name
	}

	return fmt.Sprintf(
		"show model info — scanning all models\n\n[%s] %d/%d\n\ncurrent: %s\n",
		bar, m.scanDone, m.scanTotal, cur,
	)
}

func (m model) renderCatalogProgress() string {
	const barWidth = 40
	total := len(catalogStages)
	pct := float64(m.catalogStage) / float64(total)
	filled := int(pct * barWidth)
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)

	var cur string
	if m.catalogStage < total {
		cur = catalogStages[m.catalogStage]
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf(
		"list models — querying all sources\n\n[%s] %d/%d\n\ncurrent source: %s\n",
		bar, m.catalogStage, total, cur,
	))
	if len(m.catalogErrs) > 0 {
		b.WriteString("\nwarnings:\n")
		for _, e := range m.catalogErrs {
			b.WriteString("  - " + e + "\n")
		}
	}
	return b.String()
}

func (m model) renderPsTable() string {
	if !m.ollama.installed {
		return "running models\n\nollama is not installed or not on PATH.\n\nEsc: back"
	}
	if m.loading {
		return "running models\n\nfetching..."
	}
	if m.psErr != "" {
		return fmt.Sprintf("running models\n\nerror running ollama ps:\n%s\n\nEsc: back  r: retry", m.psErr)
	}
	if m.psKillMsg != "" {
		return "running models\n\n" + m.psKillMsg + "\n\n(press any key to refresh)"
	}
	if m.psKilling {
		return fmt.Sprintf("running models\n\nstopping %s...", m.psKillTarget)
	}
	if m.psConfirmKill && len(m.psRows) > 0 {
		return fmt.Sprintf(
			"running models\n\nStop \"%s\"?\n\n[y] yes   [n] no",
			m.psRows[m.psCursor].Name,
		)
	}
	if len(m.psRows) == 0 {
		return "running models\n\nno models currently running.\n\nEsc: back  r: refresh"
	}

	cols := []string{"NAME", "DETAILS"}
	widths := []int{len(cols[0]), len(cols[1])}
	rowsData := make([][]string, len(m.psRows))
	for i, r := range m.psRows {
		rowsData[i] = []string{r.Name, r.Details}
		for j, v := range rowsData[i] {
			if len(v) > widths[j] {
				widths[j] = len(v)
			}
		}
	}
	m.fitNameColumn(widths, rowsData)

	pad := func(s string, w int) string {
		if len(s) >= w {
			return s
		}
		return s + strings.Repeat(" ", w-len(s))
	}

	const overhead = 10
	visibleRows := m.height - overhead
	if visibleRows < 3 {
		visibleRows = 3
	}
	total := len(rowsData)
	start := 0
	if total > visibleRows {
		start = m.psCursor - visibleRows/2
		if start < 0 {
			start = 0
		}
		if start > total-visibleRows {
			start = total - visibleRows
		}
	}
	end := start + visibleRows
	if end > total {
		end = total
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("running models — %d running\n\n", total))

	var headerLine strings.Builder
	for i, c := range cols {
		headerLine.WriteString(pad(c, widths[i]))
		headerLine.WriteString("  ")
	}
	b.WriteString(headerRowStyle.Render(headerLine.String()))
	b.WriteString("\n")

	for i := start; i < end; i++ {
		var line strings.Builder
		for j, v := range rowsData[i] {
			line.WriteString(pad(v, widths[j]))
			line.WriteString("  ")
		}
		if i == m.psCursor {
			b.WriteString(selectedStyle.Render(line.String()))
		} else {
			b.WriteString(unselectedStyle.Render(line.String()))
		}
		b.WriteString("\n")
	}

	b.WriteString("\nUp/Down: select  Enter: stop model  Esc: back  r: refresh\n")
	return b.String()
}

func (m model) renderCatalogTable() string {
	if m.downloadMsg != "" {
		return "list models\n\n" + m.downloadMsg + "\n\n(press any key to continue)"
	}
	if m.downloading {
		const barWidth = 40
		pct := m.downloadPct
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		filled := barWidth * pct / 100
		bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
		status := m.downloadLine
		if m.downloadAborting {
			status = "aborting..."
		}
		return fmt.Sprintf(
			"list models\n\ndownloading %s\n\n[%s] %d%%\n\n%s\n\n[c] cancel download",
			m.downloadTarget, bar, pct, status,
		)
	}
	if m.sizeSelectRow != nil {
		var b strings.Builder
		b.WriteString(fmt.Sprintf("list models\n\n%s — choose a size\n\n", m.sizeSelectRow.Name))
		for i, s := range m.sizeSelectOptions {
			line := fmt.Sprintf("%s:%s", m.sizeSelectRow.Name, s)
			if i == m.sizeSelectCursor {
				b.WriteString(selectedStyle.Render("> " + line))
			} else {
				b.WriteString(unselectedStyle.Render("  " + line))
			}
			b.WriteString("\n")
		}
		b.WriteString("\nUp/Down + Enter to pick. Esc to cancel.\n")
		return b.String()
	}
	if m.downloadConfirm != nil {
		return fmt.Sprintf(
			"list models\n\nDownload \"%s\" from %s to local ollama?\n\n[y] yes   [n] no",
			m.downloadConfirm.Name, m.downloadConfirm.Source,
		)
	}

	searchLine := fmt.Sprintf("Search: %s_", m.catalogSearch)

	if len(m.catalogRows) == 0 {
		msg := "list models\n\n" + searchLine + "\n\nno models found."
		if len(m.catalogErrs) > 0 {
			msg += "\n\nerrors:\n"
			for _, e := range m.catalogErrs {
				msg += "  - " + e + "\n"
			}
		}
		return msg + "\n\nEsc: back  alt+r: rescan"
	}

	catalogRows := m.filteredCatalog()
	if len(catalogRows) == 0 {
		return fmt.Sprintf(
			"list models\n\n%s\n\nno matches for %q.\n\nEsc: clear search  alt+r: rescan\n",
			searchLine, m.catalogSearch,
		)
	}
	if m.catalogCursor >= len(catalogRows) {
		m.catalogCursor = len(catalogRows) - 1
	}

	cols := []string{"NAME", "SOURCE", "LOCAL", "SIZE", "CAPABILITIES"}
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = len(c)
	}
	rowsData := make([][]string, len(catalogRows))
	for i, r := range catalogRows {
		local := "no"
		if r.Installed {
			local = "yes"
		}
		caps := r.Capabilities
		if caps == "" {
			caps = "-"
		}
		rowsData[i] = []string{r.Name, r.Source, local, r.Size, caps}
		for j, v := range rowsData[i] {
			if len(v) > widths[j] {
				widths[j] = len(v)
			}
		}
	}
	m.fitNameColumn(widths, rowsData)

	pad := func(s string, w int) string {
		if len(s) >= w {
			return s
		}
		return s + strings.Repeat(" ", w-len(s))
	}

	// figure out how many rows fit given terminal height: outer header(1) +
	// footer(1) + box padding top/bottom(2) + this screen's own title(1) +
	// search(1) + blank(1) + column header(1) = 8 fixed lines.
	const overhead = 8
	visibleRows := m.height - overhead
	if visibleRows < 3 {
		visibleRows = 3
	}
	total := len(rowsData)
	start := 0
	if total > visibleRows {
		start = m.catalogCursor - visibleRows/2
		if start < 0 {
			start = 0
		}
		if start > total-visibleRows {
			start = total - visibleRows
		}
	}
	end := start + visibleRows
	if end > total {
		end = total
	}

	var b strings.Builder
	if m.catalogSearch != "" {
		b.WriteString(fmt.Sprintf("list models — %d of %d entr(y/ies)\n", total, len(m.catalogRows)))
	} else {
		b.WriteString(fmt.Sprintf("list models — %d entr(y/ies)\n", total))
	}
	b.WriteString(searchLine + "\n\n")

	var headerLine strings.Builder
	for i, c := range cols {
		headerLine.WriteString(pad(c, widths[i]))
		headerLine.WriteString("  ")
	}
	b.WriteString(headerRowStyle.Render(headerLine.String()))
	b.WriteString("\n")

	for i := start; i < end; i++ {
		var line strings.Builder
		for j, v := range rowsData[i] {
			line.WriteString(pad(v, widths[j]))
			line.WriteString("  ")
		}
		if i == m.catalogCursor {
			b.WriteString(selectedStyle.Render(line.String()))
		} else {
			b.WriteString(unselectedStyle.Render(line.String()))
		}
		b.WriteString("\n")
	}

	if len(m.catalogErrs) > 0 {
		b.WriteString("\nwarnings:\n")
		for _, e := range m.catalogErrs {
			b.WriteString("  - " + e + "\n")
		}
	}

	return b.String()
}

func (m model) renderTable() string {
	if m.scanActionMsg != "" {
		return "show model info\n\n" + m.scanActionMsg + "\n\n(press any key to continue)"
	}
	if m.scanBusy {
		extra := ""
		if strings.HasPrefix(m.scanBusyLabel, "running ") {
			extra = "\n\n(handing terminal to ollama — this is interactive)"
		}
		return "show model info\n\n" + m.scanBusyLabel + extra
	}

	if len(m.scanRows) == 0 {
		return "show model info\n\nno models found.\n\nEsc: back  r: rescan"
	}

	if m.scanAction {
		name := m.scanRows[m.scanCursor].Name
		opts := []string{"run own agentic chat (chat + file read/write tools)", "run interactively (ollama run)", "remove (ollama rm)", "kill / stop (ollama stop)"}
		var b strings.Builder
		b.WriteString(fmt.Sprintf("show model info\n\n%s — choose an action\n\n", name))
		letters := []string{"a", "x", "r", "k"}
		for i, o := range opts {
			line := fmt.Sprintf("[%s] %s", letters[i], o)
			if i == m.scanActionSel {
				b.WriteString(selectedStyle.Render("> " + line))
			} else {
				b.WriteString(unselectedStyle.Render("  " + line))
			}
			b.WriteString("\n")
		}
		b.WriteString("\nUp/Down + Enter, or press the letter shown. Esc to cancel.\n")
		return b.String()
	}

	cols := []string{"NAME", "PARAMS", "QUANT", "CONTEXT", "ARCH", "SIZE", "CAPABILITIES", "CPU/GPU", "MATCH%"}
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = len(c)
	}
	rowsData := make([][]string, len(m.scanRows))
	for i, r := range m.scanRows {
		cpuGpu := "-"
		match := "-"
		if r.Benchmarked {
			cpuGpu = formatCPUGPU(r.CPUGPU)
			match = fmt.Sprintf("%d%%", r.MatchScore)
		}
		caps := r.Capabilities
		if caps == "" {
			caps = "-"
		}
		rowsData[i] = []string{r.Name, r.Params, r.Quant, r.Context, r.Arch, r.Size, caps, cpuGpu, match}
		for j, v := range rowsData[i] {
			if len(v) > widths[j] {
				widths[j] = len(v)
			}
		}
	}
	m.fitNameColumn(widths, rowsData)

	pad := func(s string, w int) string {
		if len(s) >= w {
			return s
		}
		return s + strings.Repeat(" ", w-len(s))
	}

	const overhead = 10
	visibleRows := m.height - overhead
	if visibleRows < 3 {
		visibleRows = 3
	}
	total := len(rowsData)
	start := 0
	if total > visibleRows {
		start = m.scanCursor - visibleRows/2
		if start < 0 {
			start = 0
		}
		if start > total-visibleRows {
			start = total - visibleRows
		}
	}
	end := start + visibleRows
	if end > total {
		end = total
	}

	var b strings.Builder
	source := "freshly scanned"
	if m.fromCache {
		source = "loaded from cache"
	}
	b.WriteString(fmt.Sprintf("show model info — %d model(s), %s\n\n", total, source))

	var headerLine strings.Builder
	for i, c := range cols {
		headerLine.WriteString(pad(c, widths[i]))
		headerLine.WriteString("  ")
	}
	b.WriteString(headerRowStyle.Render(headerLine.String()))
	b.WriteString("\n")

	for i := start; i < end; i++ {
		var line strings.Builder
		for j, v := range rowsData[i] {
			line.WriteString(pad(v, widths[j]))
			line.WriteString("  ")
		}
		if i == m.scanCursor {
			b.WriteString(selectedStyle.Render(line.String()))
		} else {
			b.WriteString(unselectedStyle.Render(line.String()))
		}
		b.WriteString("\n")
	}

	b.WriteString("\nCPU/GPU + MATCH% come from the benchmark in Device Info ([d] from the main menu).\n")
	return b.String()
}

// renderAgentChat draws the built-in agentic chat: scrolling transcript
// (chat first, tool calls shown inline) over a text input line. Header and
// footer are drawn by View() around this, same as every other screen.
// agentBottomReserve is the fixed vertical budget set aside below the
// scrollable transcript for the input/thinking block. It's deliberately
// generous (covers the capped streaming preview + spinner/timer + an error
// line) so the viewport's own Height rarely needs to change mid-conversation
// — the global clampToLastLines() safety net in View() covers the rest.
const agentBottomReserve = 10

func agentViewportWidth(totalWidth int) int {
	w := totalWidth - 4 // matches renderBody's Padding(0,2) horizontal inset
	if w < 20 {
		w = 20
	}
	return w
}

func agentViewportHeight(totalHeight int) int {
	h := totalHeight - 2 /* header+footer */ - agentBottomReserve
	if h < 4 {
		h = 4
	}
	return h
}

// buildAgentChatLines renders the tool-grid banner plus the full message
// history as pre-wrapped, pre-styled lines, ready to hand to the viewport.
// capabilityBadgeNames maps the 3-char codes ollama-show reports (see
// extractCapabilities) to full words for the chat-start capability table.
var capabilityBadgeNames = []struct{ code, name string }{
	{"com", "completion"}, {"too", "tools"}, {"vis", "vision"},
	{"ins", "insert"}, {"emb", "embedding"}, {"thi", "thinking"}, {"aud", "audio"},
}

// renderCapabilityBadges shows a green check / red X per known capability so
// it's obvious at a glance why tool calls or image attachments will or
// won't work for this model, instead of finding out from a runtime error.
func renderCapabilityBadges(caps string) string {
	if caps == "" || caps == "-" {
		return agentToolStyle.Render("capabilities: unknown (model info not scanned yet — press 'r' on show model info to check)")
	}
	have := map[string]bool{}
	for _, c := range strings.Split(caps, ",") {
		have[strings.TrimSpace(c)] = true
	}
	var parts []string
	for _, k := range capabilityBadgeNames {
		if have[k.code] {
			parts = append(parts, helpKeyStyle.Render("✓ "+k.name))
		} else {
			parts = append(parts, redStyle.Render("✗ "+k.name))
		}
	}
	return "capabilities: " + strings.Join(parts, "  ")
}

// renderWarmupStatus shows whether the model has actually answered a
// request yet — cold-loading a large model can take a minute or more with
// zero other feedback, which reads as the app being frozen even though
// typing still works the whole time.
func renderWarmupStatus(warmup string, spinnerFrame int, elapsed time.Duration) string {
	switch {
	case warmup == "ready":
		return helpKeyStyle.Render("● model loaded and ready")
	case strings.HasPrefix(warmup, "error: "):
		return redStyle.Render("● couldn't reach the model: " + strings.TrimPrefix(warmup, "error: "))
	case warmup == "pending":
		// Deliberately NOT agentThinkingPhrase's escalating "almost done"
		// ladder: that's honest only when tied to an actual in-flight
		// request. Here we have zero real progress signal — just
		// pending-vs-ready from polling `ollama ps` — so claiming "almost
		// done" would be a guess dressed up as a status.
		frame := agentSpinnerFrames[spinnerFrame%len(agentSpinnerFrames)]
		return agentToolStyle.Render(fmt.Sprintf("%s loading model into memory... (%s)", frame, elapsed.Round(time.Second)))
	default:
		return ""
	}
}

// renderPrefixedChatLines wraps "prefix+content" to width, keeping the
// "you> " / "modelName> " prefix in its own bright color on the first line
// while the message body itself (first line's remainder, plus every
// continuation line) renders in plain grey — the prefix is what tells you
// who's speaking, the actual text doesn't need to compete with it.
var chatURLRe = regexp.MustCompile(`https?://[^\s)\]}>"']+`)

// linkifyLine renders a plain (unstyled) line with base, except any URL
// substring, which gets agentLinkStyle plus an OSC 8 hyperlink escape so
// terminals that support it (Windows Terminal, iTerm2, etc.) make it
// clickable. Must run on already-wrapped plain text — lipgloss.Width()
// doesn't understand OSC 8, so linkifying before wrapping would throw off
// the wrap math.
// isRTLRune reports whether r belongs to a right-to-left script (Hebrew or
// Arabic and its extensions).
func isRTLRune(r rune) bool {
	return (r >= 0x0590 && r <= 0x05FF) || // Hebrew
		(r >= 0x0600 && r <= 0x06FF) || // Arabic
		(r >= 0x0750 && r <= 0x077F) || // Arabic Supplement
		(r >= 0xFB1D && r <= 0xFB4F) || // Hebrew presentation forms
		(r >= 0xFB50 && r <= 0xFDFF) || // Arabic presentation forms A
		(r >= 0xFE70 && r <= 0xFEFF) // Arabic presentation forms B
}

func containsRTL(s string) bool {
	for _, r := range s {
		if isRTLRune(r) {
			return true
		}
	}
	return false
}

// reorderMixedToken fixes a word that mixes RTL letters with an embedded
// LTR run (digits, a number like "150", a Latin abbreviation) — e.g. a
// Hebrew word glued to a number by a hyphen. Reversing the whole token's
// characters (the old behavior) also reversed the digits themselves
// ("150" -> "051"), which is wrong: digits are logically LTR even inside
// an RTL sentence. This splits the token into maximal RTL/non-RTL rune
// runs, reverses the RUN ORDER (the word still flows right-to-left) and
// reverses characters only within each RTL run, leaving any embedded
// digit/Latin run's internal character order untouched.
func reorderMixedToken(text string) string {
	type run struct {
		text string
		rtl  bool
	}
	runes := []rune(text)
	var runs []run
	i := 0
	for i < len(runes) {
		start := i
		rtl := isRTLRune(runes[i])
		for i < len(runes) && isRTLRune(runes[i]) == rtl {
			i++
		}
		runs = append(runs, run{text: string(runes[start:i]), rtl: rtl})
	}
	for a, b := 0, len(runs)-1; a < b; a, b = a+1, b-1 {
		runs[a], runs[b] = runs[b], runs[a]
	}
	var out strings.Builder
	for _, r := range runs {
		if r.rtl {
			out.WriteString(reverseRunes(r.text))
		} else {
			out.WriteString(r.text)
		}
	}
	return out.String()
}

func reverseRunes(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

// fixRTLDisplay makes Hebrew/Arabic readable on a terminal with no bidi
// support. Such text is stored in logical (reading) order — first letter
// typed is the rightmost glyph — but a plain terminal just prints runes
// left to right in array order, which comes out backwards for RTL scripts
// (each word's letters reversed, and word order reversed within the
// sentence). This finds the span from the first to the last RTL-containing
// word on the line, reverses that span's word order, and reverses the
// characters within each RTL word — leaving any interior non-RTL token
// (a number, a URL) in its own correct character order but relocated to
// its new position, exactly like a real bidi renderer would lay it out.
// Runs per rendered (already width-wrapped) line, so a paragraph that
// wraps across multiple terminal rows still gets each row's RTL span laid
// out correctly.
func fixRTLDisplay(line string) string {
	if !containsRTL(line) {
		return line
	}

	type token struct {
		text    string
		isSpace bool
	}
	var tokens []token
	runes := []rune(line)
	i := 0
	for i < len(runes) {
		start := i
		isSpace := runes[i] == ' '
		for i < len(runes) && (runes[i] == ' ') == isSpace {
			i++
		}
		tokens = append(tokens, token{text: string(runes[start:i]), isSpace: isSpace})
	}

	firstRTL, lastRTL := -1, -1
	for idx, t := range tokens {
		if !t.isSpace && containsRTL(t.text) {
			if firstRTL == -1 {
				firstRTL = idx
			}
			lastRTL = idx
		}
	}
	if firstRTL == -1 {
		return line
	}

	run := make([]token, lastRTL-firstRTL+1)
	copy(run, tokens[firstRTL:lastRTL+1])
	for a, b := 0, len(run)-1; a < b; a, b = a+1, b-1 {
		run[a], run[b] = run[b], run[a]
	}
	for idx := range run {
		if !run[idx].isSpace && containsRTL(run[idx].text) {
			run[idx].text = reorderMixedToken(run[idx].text)
		}
	}

	var b strings.Builder
	for _, t := range tokens[:firstRTL] {
		b.WriteString(t.text)
	}
	for _, t := range run {
		b.WriteString(t.text)
	}
	for _, t := range tokens[lastRTL+1:] {
		b.WriteString(t.text)
	}
	return b.String()
}

func linkifyLine(l string, base lipgloss.Style) string {
	l = fixRTLDisplay(l)
	matches := chatURLRe.FindAllStringIndex(l, -1)
	if len(matches) == 0 {
		return base.Render(l)
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		if m[0] > last {
			b.WriteString(base.Render(l[last:m[0]]))
		}
		u := l[m[0]:m[1]]
		b.WriteString("\x1b]8;;" + u + "\x1b\\" + agentLinkStyle.Render(u) + "\x1b]8;;\x1b\\")
		last = m[1]
	}
	if last < len(l) {
		b.WriteString(base.Render(l[last:]))
	}
	return b.String()
}

// stripMarkdownBold drops literal "**" markdown bold markers. This is a
// plain-text terminal, not a markdown renderer, so "**text**" just showed
// up as literal asterisks (worse, unpaired ones after RTL reordering) —
// removing them outright reads cleaner than trying to preserve pairing.
func stripMarkdownBold(s string) string {
	return strings.ReplaceAll(s, "**", "")
}

// mdLinkRe matches markdown link syntax [label](url). This plain-text
// terminal doesn't render it into a hyperlink, so it just showed up as
// literal brackets/parens — and when the model sets label==url (common),
// the URL appeared twice, once from each half.
var mdLinkRe = regexp.MustCompile(`\[([^\]]*)\]\((https?://[^)\s]+)\)`)

// stripMarkdownLinks collapses "[label](url)" down to one clean form: just
// the URL if the label IS the url (or empty), otherwise "label: url".
func stripMarkdownLinks(s string) string {
	return mdLinkRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := mdLinkRe.FindStringSubmatch(m)
		label, url := strings.TrimSpace(sub[1]), sub[2]
		if label == "" || label == url {
			return url
		}
		return label + ": " + url
	})
}

// cleanMarkdownForDisplay strips the markdown constructs this plain-text
// terminal can't render (bold markers, link syntax) before a message is
// shown.
// stripMarkdownCodeSpans removes literal backtick characters (both
// `single` and ```triple``` code-span markers) — Telegram messages sent
// with no parse_mode render them as plain text, not formatting, and a
// backtick sitting right next to a URL has been observed to throw off
// Telegram's own URL auto-detection, truncating the tappable link before
// the query string.
func stripMarkdownCodeSpans(s string) string {
	return strings.ReplaceAll(s, "`", "")
}

func cleanMarkdownForDisplay(s string) string {
	return stripMarkdownCodeSpans(stripMarkdownBold(stripMarkdownLinks(s)))
}

// rightAlignIfRTL pads s with leading spaces so it hugs the right edge of
// width columns — an RTL (Hebrew/Arabic) line reads naturally anchored to
// the right, not flush against the left margin like LTR text. A no-op for
// lines with no RTL content, or that already fill the width.
func rightAlignIfRTL(s string, width int) string {
	if !containsRTL(s) {
		return s
	}
	pad := width - lipgloss.Width(s)
	if pad <= 0 {
		return s
	}
	return strings.Repeat(" ", pad) + s
}

func renderPrefixedChatLines(prefix, content string, width int, prefixStyle lipgloss.Style) []string {
	wrapped := wrapLines(prefix+content, width)
	out := make([]string, len(wrapped))
	for i, l := range wrapped {
		if i == 0 && strings.HasPrefix(l, prefix) {
			rest := rightAlignIfRTL(l[len(prefix):], width-lipgloss.Width(prefix))
			out[i] = prefixStyle.Render(prefix) + linkifyLine(rest, agentToolStyle)
		} else {
			out[i] = linkifyLine(rightAlignIfRTL(l, width), agentToolStyle)
		}
	}
	return out
}

// lastAssistantContent returns the last non-empty assistant message's
// content in msgs, or "" if none — used to decide whether a completed turn
// actually produced a displayed reply (vs. an error with nothing to show).
func lastAssistantContent(msgs []ollamaChatMsg) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && strings.TrimSpace(msgs[i].Content) != "" {
			return msgs[i].Content
		}
	}
	return ""
}

func buildAgentChatLines(width int, messages []ollamaChatMsg, modelName string, capabilities string, warmup string, spinnerFrame int, warmupElapsed time.Duration, turnTimes []time.Duration) []string {
	var lines []string
	toolNames := flatToolNames()
	lines = append(lines, agentHeadStyle.Render(fmt.Sprintf("%d tools available — Alt+T to browse by category", len(toolNames))))
	lines = append(lines, renderCapabilityBadges(capabilities))
	if s := renderWarmupStatus(warmup, spinnerFrame, warmupElapsed); s != "" {
		lines = append(lines, s)
	}
	lines = append(lines, "")
	replyIdx := 0
	for _, msg := range messages {
		before := len(lines)
		switch msg.Role {
		case "system":
			continue
		case "user":
			lines = append(lines, renderPrefixedChatLines("you> ", msg.Content, width, agentUserStyle)...)
		case "tool":
			// Tool calls/results are process, not the answer — the model
			// still gets the full msg.Content for its own reasoning
			// (unaffected, this is render-only), but the transcript only
			// shows the user's messages and the model's actual replies.
			continue
		case "assistant":
			if strings.TrimSpace(msg.Content) != "" {
				lines = append(lines, renderPrefixedChatLines(modelName+"> ", cleanMarkdownForDisplay(msg.Content), width, agentReplyStyle)...)
				// Elapsed time for this turn, display-only (never fed back
				// into msg.Content, so it never pollutes the model's own
				// context on the next turn) — mirrors the web UI/Telegram
				// timing footer so all three surfaces show it consistently.
				if replyIdx < len(turnTimes) {
					lines = append(lines, agentToolStyle.Render("⏱ "+formatElapsedDuration(turnTimes[replyIdx])))
				}
				replyIdx++
			}
		}
		// Only add a separating blank line for messages that actually
		// rendered something — an empty assistant turn (tool-call-only,
		// or a trailing empty-content reply) shouldn't create an extra gap.
		if len(lines) > before {
			lines = append(lines, "")
		}
	}
	return lines
}

// syncAgentViewport rebuilds the transcript content and jumps to the
// bottom — call after any change to agentMessages (a new message sent, a
// tool round finishing, or a turn completing).
func (m *model) syncAgentViewport() {
	if !m.agentVPReady {
		return
	}
	lines := buildAgentChatLines(agentViewportWidth(m.width), m.agentMessages, m.agentModelName, m.agentCapabilities, m.agentWarmup, m.agentSpinner, time.Since(m.agentWarmupStarted), m.agentTurnTimes)
	m.agentViewport.SetContent(strings.Join(lines, "\n"))
	m.agentViewport.GotoBottom()
}

func (m model) renderAgentChat() string {
	var b strings.Builder

	var bottom strings.Builder
	if m.agentErr != "" {
		bottom.WriteString(redStyle.Render("error: "+m.agentErr) + "\n")
	}
	if m.agentPasting {
		bottom.WriteString(agentToolStyle.Render("pasting from clipboard...") + "\n")
	} else if m.agentPasteNotice != "" {
		bottom.WriteString(helpKeyStyle.Render(m.agentPasteNotice) + "\n")
	}
	if m.agentBusy {
		frame := agentSpinnerFrames[m.agentSpinner%len(agentSpinnerFrames)]
		elapsed := time.Since(m.agentStarted).Round(time.Second)
		if strings.TrimSpace(m.agentStreamBuf) == "" {
			bottom.WriteString(fmt.Sprintf("%s %s %s (%s)\n", frame, m.agentModelName, agentThinkingPhrase(elapsed), elapsed))
		} else {
			// Cap the live preview: an unbounded growing reply (the model
			// can stream thousands of characters before it's done) must
			// never be allowed to make this block taller than its
			// reserved budget (agentBottomReserve), or the layout
			// overflows and the header scrolls off the top.
			const maxPreviewLines = 6
			streamLines := wrapLines(m.agentModelName+"> "+cleanMarkdownForDisplay(m.agentStreamBuf)+"▌", agentViewportWidth(m.width))
			shown := streamLines
			truncatedAbove := 0
			if len(shown) > maxPreviewLines {
				truncatedAbove = len(shown) - maxPreviewLines
				shown = shown[truncatedAbove:]
			}
			if truncatedAbove > 0 {
				bottom.WriteString(agentToolStyle.Render(fmt.Sprintf("  ... (%d more line(s) so far)", truncatedAbove)) + "\n")
			}
			for _, l := range shown {
				bottom.WriteString(agentReplyStyle.Render(fixRTLDisplay(l)) + "\n")
			}
			bottom.WriteString(fmt.Sprintf("%s (%s)\n", frame, elapsed))
		}
	} else {
		// Reordered live, same as a sent message — a Hebrew/Arabic typist
		// needs to read what they're typing as they type it, not just once
		// it's sent. The cursor moves to the left edge of the RTL span
		// instead of trailing at the end, matching where the next
		// (logically-appended, visually-leftward) character will land.
		liveInput := m.agentInput
		var inputRendered string
		if containsRTL(liveInput) {
			inputRendered = "█" + fixRTLDisplay(liveInput)
		} else {
			inputRendered = liveInput + "█"
		}
		bottom.WriteString(agentUserStyle.Render("you> ") + agentToolStyle.Render(inputRendered) + "\n")
	}

	if m.agentVPReady {
		// The viewport must fill exactly the space left over after the
		// bottom block (error/busy/you> lines, 1 line idle, up to ~8 while
		// streaming) and the scroll-position notice row — computed fresh
		// every frame from the bottom block's actual size, not a fixed
		// reserve. A fixed reserve either leaves a dead gap of blank rows
		// above "you>" when idle (the block is much smaller than the
		// reserve) or, if too small, forces the outer lipgloss .Height()
		// call in View() to pad the shortfall onto the very bottom of the
		// WHOLE frame instead (after the input line) — which is what made
		// "you>" float near the top with a gap below it.
		bodyHeight := m.height - 2 // header + footer, matches View()'s math
		bottomLines := strings.Count(bottom.String(), "\n")
		contentHeight := bodyHeight - bottomLines - 1 // -1 for the scroll-notice/blank row
		if contentHeight < 1 {
			contentHeight = 1
		}
		vpCopy := m.agentViewport
		vpCopy.Height = contentHeight
		vp := vpCopy.View()
		if got := strings.Count(vp, "\n") + 1; got < contentHeight {
			vp += strings.Repeat("\n", contentHeight-got)
		}
		b.WriteString(vp)
		b.WriteString("\n")
		if !m.agentViewport.AtBottom() {
			b.WriteString(agentHeadStyle.Render("-- scrolled up; End or PgDn repeatedly to catch up --") + "\n")
		} else {
			b.WriteString("\n")
		}
	}
	b.WriteString(bottom.String())
	// Input/status block sits fixed right above the footer: footer (row 1
	// from bottom), "you>" (row 2) — always, regardless of conversation
	// length or scroll state.
	return b.String()
}

// renderToolCategories shows the full tool list grouped by what each tool
// actually does — the flat 4-row grid in the chat header is a quick
// reminder, this is the "let me see everything" view (Alt+T from chat).
// renderToolCategories draws the numbered, Tab-navigable tool list.
// Returns the rendered text plus the 0-based line index the currently
// selected tool sits on, so the caller can scroll that line into view.
func (m model) renderToolCategories() (string, int) {
	names := flatToolNames()
	if len(names) == 0 {
		return "(no tools)", 0
	}
	cursor := m.toolCatCursor
	if cursor < 0 {
		cursor = 0
	}
	if cursor >= len(names) {
		cursor = len(names) - 1
	}
	selectedLabel := fmt.Sprintf("%d. %s", cursor+1, names[cursor])

	var b strings.Builder
	b.WriteString(agentHeadStyle.Render(fmt.Sprintf("Tools by category (%d total) — Tab/Shift+Tab select, Enter for details", len(names))) + "\n\n")
	lineNo := 2
	cursorLine := 0
	globalIdx := 0
	for _, cat := range agentToolCategories {
		b.WriteString(headerRowStyle.Render(cat.name) + "\n")
		lineNo++
		labels := make([]string, len(cat.tools))
		for i, name := range cat.tools {
			labels[i] = fmt.Sprintf("%d. %s", globalIdx+i+1, name)
		}
		for _, l := range toolGridLines(labels, 2, m.width-4) {
			if strings.Contains(l, selectedLabel) {
				l = strings.Replace(l, selectedLabel, selectedStyle.Render(selectedLabel), 1)
				cursorLine = lineNo
			}
			b.WriteString("  " + l + "\n")
			lineNo++
		}
		b.WriteString("\n")
		lineNo++
		globalIdx += len(cat.tools)
	}
	b.WriteString("Esc or Alt+T: back to chat\n")
	return b.String(), cursorLine
}

// renderToolDetail shows one tool's name, description, and two example
// prompts — opened with Enter on the tool list, closed with Esc.
func renderToolDetail(name string) string {
	var b strings.Builder
	b.WriteString(agentHeadStyle.Render(name) + "\n\n")
	desc := toolDescription(name)
	if desc == "" {
		desc = "(no description)"
	}
	for _, l := range wrapLines(desc, 76) {
		b.WriteString(l + "\n")
	}
	b.WriteString("\n" + headerRowStyle.Render("Examples") + "\n")
	if ex, ok := toolExamples[name]; ok {
		b.WriteString("  1. " + ex[0] + "\n")
		b.WriteString("  2. " + ex[1] + "\n")
	} else {
		b.WriteString("  (no examples yet)\n")
	}
	b.WriteString("\nEsc: back to tool list\n")
	return b.String()
}

// renderAgentHelp is the Alt+H dialog for the agentic chat — the keybind
// list used to live in the footer hint, but wrapped to two lines on
// narrower terminals, so it moved here behind a short "Alt+H: help" hint.
func renderAgentHelp() string {
	var b strings.Builder
	b.WriteString(agentHeadStyle.Render("Agentic chat — keys") + "\n\n")
	b.WriteString("  Enter        send your message\n")
	b.WriteString("  Up / Down    scroll history one line\n")
	b.WriteString("  PgUp / PgDn  scroll history one page\n")
	b.WriteString("  Home / End   jump to top / bottom of history\n")
	b.WriteString("  Alt+V        paste - an image on the clipboard attaches directly for\n")
	b.WriteString("               vision-capable models, otherwise pastes clipboard text\n")
	b.WriteString("  Alt+T        browse all tools by category\n")
	b.WriteString("  Alt+H        this help screen\n")
	b.WriteString("  Alt+M        cycle tool mode: auto -> on -> off -> auto\n")
	b.WriteString("  Esc          back to model actions menu\n")
	b.WriteString("  Ctrl+C       quit llama-shell\n\n")
	b.WriteString("A path to an existing image file typed/pasted into your message also\n")
	b.WriteString("attaches it, same as Alt+V.\n\n")
	b.WriteString("Tool mode (Alt+M): auto (default) turns tools off just for a message\n")
	b.WriteString("that attaches an image, since at least gemma4:e2b garbles the image\n")
	b.WriteString("entirely when tools are also in the request - then goes back to tools\n")
	b.WriteString("on for the next image-free message. on = always try tools even with an\n")
	b.WriteString("image attached. off = never send tools this chat.\n\n")
	b.WriteString("Looking for the CAPABILITIES column codes (com/too/ins/vis/emb/thi/aud)?\n")
	b.WriteString("Those are on \"list models\" / \"show model info\" screens, not here — back\n")
	b.WriteString("out to one of those and press Alt+H there instead.\n\n")
	b.WriteString("Esc: back to chat\n")
	return styleHelpLines(b.String())
}

type toolCategory struct {
	name  string
	tools []string
}

var agentToolCategories = []toolCategory{
	{"Files & Archives", []string{
		"read_file", "write_file", "append_file", "list_dir", "make_dir", "delete_file",
		"search_files", "copy_file", "move_file", "count_lines", "file_hash", "file_info",
		"compress_zip", "extract_zip",
	}},
	{"Shell & Processes", []string{
		"run_command", "run_powershell", "run_python", "list_processes", "kill_process",
		"list_window_titles",
	}},
	{"Networking & Web", []string{
		"web_search", "read_webpage", "rss_feed", "find_rss_feed", "tavily_search", "tavily_extract", "http_get", "http_post", "download_file", "ping_host",
		"get_public_ip", "get_web_ui_url", "ssh_run", "list_network_interfaces", "send_email",
	}},
	{"System & Environment", []string{
		"system_info", "list_env_vars", "get_env", "get_clipboard", "set_clipboard",
		"get_datetime", "disk_usage", "list_installed_programs", "send_notification",
		"read_registry",
	}},
	{"Git & Ollama", []string{
		"git_status", "git_diff", "git_log", "git_commit", "git_branch",
		"list_ollama_models", "list_running_ollama_models", "pull_ollama_model",
	}},
	{"Vision & Media", []string{"take_screenshot", "view_image", "read_pdf", "read_document"}},
	{"Data", []string{"run_sql", "read_csv", "read_json"}},
	{"Open / Launch", []string{"open_url", "open_path"}},
}

// flatToolNames lists every tool in the same order they're displayed in,
// derived from agentToolCategories so the two can never drift apart.
func flatToolNames() []string {
	var names []string
	for _, cat := range agentToolCategories {
		names = append(names, cat.tools...)
	}
	return names
}

// toolDescription looks up a tool's description from its single source of
// truth (agentTools()) rather than duplicating description text here.
func toolDescription(name string) string {
	for _, t := range agentTools() {
		if t.Function.Name == name {
			return t.Function.Description
		}
	}
	return ""
}

// toolExamples gives two short natural-language prompts per tool, shown in
// the tool detail view (Enter on a tool in the Alt+T screen).
var toolExamples = map[string][2]string{
	"read_file":                  {"Read the contents of config.yaml", "What does main.go say on line 42?"},
	"write_file":                 {"Create a file notes.txt with today's TODO list", "Write a .gitignore for a Go project"},
	"append_file":                {"Add a new entry to CHANGELOG.md", "Append today's date to log.txt"},
	"list_dir":                   {"What's in the src folder?", "List everything in the current directory"},
	"make_dir":                   {"Create a folder called backups", "Make a nested dir build/output"},
	"delete_file":                {"Delete the old.log file", "Remove tmp/scratch.txt"},
	"search_files":               {"Find every file with \"config\" in the name", "Search this project for files named test"},
	"copy_file":                  {"Copy README.md to README.bak", "Duplicate template.txt as new.txt"},
	"move_file":                  {"Rename draft.txt to final.txt", "Move report.pdf into the archive folder"},
	"count_lines":                {"How many lines are in main.go?", "Count lines in the log file"},
	"file_hash":                  {"What's the SHA-256 of installer.exe?", "Checksum this download to verify it"},
	"file_info":                  {"When was this file last modified?", "How big is the video file?"},
	"compress_zip":               {"Zip up the dist folder", "Compress these logs into an archive"},
	"extract_zip":                {"Extract release.zip into this folder", "Unzip the downloaded archive"},
	"run_command":                {"Run \"dir\" and show me the output", "Check disk space with a shell command"},
	"run_powershell":             {"List all running services via PowerShell", "Get the top 5 processes by memory"},
	"run_python":                 {"Run a quick Python script to sum 1 to 100", "Use Python to check if a number is prime"},
	"list_processes":             {"What processes are running right now?", "Is chrome.exe running?"},
	"kill_process":               {"Kill the process called notepad.exe", "Stop PID 4821"},
	"list_window_titles":         {"What windows do I have open?", "Is there a Notepad window open?"},
	"web_search":                 {"Search the web for the latest Go release", "Look up how to install poppler on Windows"},
	"read_webpage":               {"Summarize the article at this URL", "Read the docs page and tell me the API key format"},
	"rss_feed":                   {"What are the top stories on finance.yahoo.com/news/rssindex?", "Get the latest posts from this blog's RSS feed"},
	"find_rss_feed":              {"Does globes.co.il have an RSS feed?", "Find the real RSS feed URL for this blog"},
	"tavily_search":              {"Search for the latest news on the Fed rate decision", "Find recent articles about the new Go release"},
	"tavily_extract":             {"Scrape the clean article text from this URL", "Extract the real content from this page — read_webpage just gave me a cookie wall"},
	"http_get":                   {"Fetch https://api.github.com/status", "Check what this API endpoint returns"},
	"http_post":                  {"POST this JSON payload to my webhook URL", "Send a test request to the local API"},
	"download_file":              {"Download this ZIP to the downloads folder", "Save this image to disk"},
	"ping_host":                  {"Is 8.8.8.8 reachable?", "Ping github.com to check connectivity"},
	"get_stock_quote":            {"What is the NASDAQ-100 index value right now?", "What's Apple's stock price?"},
	"send_email":                 {"Email test@example.com with subject 'Hi' and body 'Just testing'", "Send an email summarizing this conversation to my boss"},
	"get_public_ip":              {"What's my public IP address?", "Check what IP this machine shows on the internet"},
	"get_web_ui_url":             {"What's the URL to browse to the web UI?", "Give me the link to open llama-shell in a browser"},
	"ssh_run":                    {"Run \"uptime\" on my home server", "SSH into the pi and check disk usage"},
	"list_network_interfaces":    {"What's my local IP address?", "Show all network adapters and their config"},
	"system_info":                {"What OS and CPU does this machine have?", "How many cores does this computer have?"},
	"list_env_vars":              {"What environment variables are set?", "List all env var names"},
	"get_env":                    {"What's the value of PATH?", "Check the JAVA_HOME environment variable"},
	"get_clipboard":              {"What's currently on my clipboard?", "Read what I just copied"},
	"set_clipboard":              {"Copy \"hello world\" to my clipboard", "Put this generated password on the clipboard"},
	"get_datetime":               {"What time is it right now?", "What's today's date?"},
	"disk_usage":                 {"How much free disk space do I have?", "Check space on all drives"},
	"list_installed_programs":    {"What software is installed on this machine?", "Is Python installed?"},
	"send_notification":          {"Show me a popup saying the build finished", "Notify me when this task is done"},
	"read_registry":              {"What's in HKLM Software Microsoft Windows NT CurrentVersion?", "Check this registry key's values"},
	"git_status":                 {"What's the git status of this repo?", "Are there any uncommitted changes?"},
	"git_diff":                   {"Show me the unstaged changes", "What did I just edit?"},
	"git_log":                    {"Show the last 5 commits", "What's the recent commit history?"},
	"git_commit":                 {"Commit all changes with message \"fix bug\"", "Stage and commit my edits"},
	"git_branch":                 {"What branches exist in this repo?", "Create a new branch called feature-x"},
	"list_ollama_models":         {"What Ollama models are installed?", "List my local models"},
	"list_running_ollama_models": {"What models are currently loaded?", "Is anything running in Ollama right now?"},
	"pull_ollama_model":          {"Pull the llama3.2:1b model", "Download qwen2.5-coder:7b"},
	"take_screenshot":            {"Take a screenshot and tell me what's on screen", "Capture my screen and describe it"},
	"view_image":                 {"Look at screenshot.png and describe it", "What's in this diagram file?"},
	"read_pdf":                   {"Extract the text from report.pdf", "What does this PDF say on page 1?"},
	"read_document":              {"What does this Word doc say: notes.docx", "Summarize contract.pdf and readme.txt"},
	"run_sql":                    {"Query the users table for all rows", "Run SELECT COUNT(*) on my database"},
	"read_csv":                   {"Show me the contents of data.csv as a table", "Read sales.csv"},
	"read_json":                  {"Pretty-print config.json", "Validate and show this JSON file"},
	"open_url":                   {"Open github.com in my browser", "Open the Ollama download page"},
	"open_path":                  {"Open this folder in Explorer", "Open the PDF with its default app"},
}

// wrapLines does a plain rune-count wrap so a long chat/tool message doesn't
// overflow the terminal width.
// toolGridLines lays names out column-major (down each column, then the
// next) into a grid, starting from targetRows and growing the row count if
// that many columns wouldn't fit maxWidth — never overflows horizontally,
// even if that means more rows than requested.
func toolGridLines(names []string, targetRows, maxWidth int) []string {
	if maxWidth < 20 {
		maxWidth = 20
	}
	rows := targetRows
	if rows < 1 {
		rows = 1
	}
	for {
		cols := (len(names) + rows - 1) / rows
		colWidths := make([]int, cols)
		for i, n := range names {
			c := i / rows
			if len(n) > colWidths[c] {
				colWidths[c] = len(n)
			}
		}
		total := 0
		for _, w := range colWidths {
			total += w + 2
		}
		if total <= maxWidth || rows >= len(names) {
			lines := make([]string, rows)
			for r := 0; r < rows; r++ {
				var sb strings.Builder
				for c := 0; c < cols; c++ {
					idx := c*rows + r
					if idx >= len(names) {
						continue
					}
					sb.WriteString(names[idx])
					pad := colWidths[c] - len(names[idx]) + 2
					if pad > 0 {
						sb.WriteString(strings.Repeat(" ", pad))
					}
				}
				lines[r] = strings.TrimRight(sb.String(), " ")
			}
			return lines
		}
		rows++
	}
}

func wrapLines(s string, width int) []string {
	if width < 10 {
		width = 10
	}
	var out []string
	for _, raw := range strings.Split(s, "\n") {
		r := []rune(raw)
		for len(r) > width {
			out = append(out, string(r[:width]))
			r = r[width:]
		}
		out = append(out, string(r))
	}
	return out
}

// truncateName shortens a name to fit width w, adding an ellipsis if cut.
func truncateName(s string, w int) string {
	if len(s) <= w {
		return s
	}
	if w <= 1 {
		return s[:w]
	}
	return s[:w-1] + "…"
}

// fitNameColumn shrinks the NAME column (assumed index 0) so the whole row
// fits within the terminal width, truncating long names as needed.
func (m model) fitNameColumn(widths []int, rowsData [][]string) {
	avail := m.width - 4 // box padding (1,2) takes 2 cols left + 2 right
	if avail < 20 {
		avail = 20
	}
	totalOther := 0
	for i := 1; i < len(widths); i++ {
		totalOther += widths[i] + 2
	}
	nameWidth := avail - totalOther - 2
	if nameWidth < 6 {
		nameWidth = 6
	}
	if widths[0] > nameWidth {
		widths[0] = nameWidth
		for i := range rowsData {
			rowsData[i][0] = truncateName(rowsData[i][0], nameWidth)
		}
	}
}

func (m model) renderCmdView(label string) string {
	if !m.ollama.installed {
		return fmt.Sprintf("%s\n\nollama is not installed or not on PATH.\n\nEsc: back", label)
	}
	if m.loading {
		return fmt.Sprintf("%s\n\nrunning...", label)
	}
	return fmt.Sprintf("%s\n\n%s\nEsc: back", label, m.output)
}

// pendingRelaunchExePath is set by the auto-update daemon right before it
// calls p.Quit() — main() checks it after p.Run() returns (so the TUI has
// already restored the terminal cleanly) and, if set, execs the freshly
// swapped-in binary and exits this process.
var pendingRelaunchExePath string

// durationUntilNextClockTime returns how long to sleep until the next local
// occurrence of hour:minute, today's if it hasn't passed yet, otherwise
// tomorrow's. Read fresh from config each loop iteration (not cached), so
// changing the time in settings takes effect on the very next wake cycle.
func durationUntilNextClockTime(hour, minute int) time.Duration {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next.Sub(now)
}

// runAutoUpdateDaemon wakes once a day at the configured time and, if the
// auto-update checkbox is on and a newer release exists, downloads it,
// swaps it into place, and asks the running TUI to quit so main() can
// relaunch the new binary. Runs for the whole process lifetime; never
// returns on its own.
func runAutoUpdateDaemon(p *tea.Program) {
	for {
		cfg := loadAutoUpdateConfig()
		time.Sleep(durationUntilNextClockTime(cfg.Hour, cfg.Minute))
		if !loadAutoUpdateConfig().Enabled {
			continue
		}
		latest, assetURL, err := checkForUpdateSync()
		if err != nil {
			appendLog("auto-update: check failed: %s", err.Error())
			continue
		}
		if assetURL == "" || !isNewerVersion(appVersion, latest) {
			continue
		}
		exePath, err := os.Executable()
		if err != nil {
			appendLog("auto-update: could not resolve exe path: %s", err.Error())
			continue
		}
		exePath, err = filepath.EvalSymlinks(exePath)
		if err != nil {
			appendLog("auto-update: could not resolve exe path: %s", err.Error())
			continue
		}
		result := applyUpdateAt(exePath, assetURL)
		if result.err != "" {
			appendLog("auto-update: failed to apply %s: %s", latest, result.err)
			continue
		}
		appendLog("auto-update: downloaded %s, relaunching", latest)
		pendingRelaunchExePath = exePath
		p.Quit()
		return
	}
}

// startupAutopilot is set by handleCLIArgs when llama-shell was launched
// with --autopilot — initialModel() checks it to jump straight into the
// same autopilot flow the main menu's [a] triggers, instead of showing
// the menu first.
var startupAutopilot bool

// startupMinimized is set by handleCLIArgs when launched with
// --minimized — main() minimizes the console window (Windows only, see
// minimizeConsoleWindow) right after starting the TUI, so llama-shell can
// be launched e.g. from a startup script without popping a window in the
// user's face.
var startupMinimized bool

// handleCLIArgs checks os.Args for flags that print-and-exit (help,
// --tools[-extended], --log) before the TUI (tea.NewProgram) ever starts
// — these are meant to work from a plain terminal pipe (e.g.
// `llama-shell --tools | grep ...`), which an alt-screen TUI can't do.
// --autopilot and --minimized instead just set a startup flag and fall
// through to the normal TUI startup, since both affect how the TUI itself
// comes up rather than replacing it.
func handleCLIArgs() {
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--help", "-h", "/help", "/?", "-?", "help":
			printCLIHelp()
			os.Exit(0)
		case "--tools":
			printCLITools(false)
			os.Exit(0)
		case "--tools-extended":
			printCLITools(true)
			os.Exit(0)
		case "--autopilot":
			startupAutopilot = true
		case "--minimized":
			startupMinimized = true
		case "--log":
			n := 100
			if i+1 < len(args) {
				if parsed, err := strconv.Atoi(args[i+1]); err == nil {
					n = parsed
					i++ // consume the number too
				}
			}
			printCLILog(n)
			os.Exit(0)
		case "--export-backup":
			path := defaultBackupPath()
			if i+1 < len(args) {
				path = args[i+1]
				i++
			}
			if err := exportBackup(path); err != nil {
				fmt.Fprintln(os.Stderr, "export failed: "+err.Error())
				os.Exit(1)
			}
			fmt.Println("exported settings to " + path)
			os.Exit(0)
		case "--import-backup":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "usage: llama-shell --import-backup <path>")
				os.Exit(1)
			}
			path := args[i+1]
			i++
			if err := importBackup(path); err != nil {
				fmt.Fprintln(os.Stderr, "import failed: "+err.Error())
				os.Exit(1)
			}
			fmt.Println("imported settings from " + path)
			os.Exit(0)
		}
	}
}

func printCLIHelp() {
	fmt.Println("llama-shell — Ollama TUI")
	fmt.Println()
	fmt.Println("Usage: llama-shell [flag]")
	fmt.Println()
	fmt.Println("  --help, -h, /help, /?, -?   Show this help and exit.")
	fmt.Println("  --autopilot                 Skip the menu: check Ollama is installed (opens")
	fmt.Println("                              the installer page if not), pull " + autopilotModel)
	fmt.Println("                              if missing, enable + start the web server, then")
	fmt.Println("                              load straight into the agentic chat with it —")
	fmt.Println("                              the same flow as pressing [a] in the main menu.")
	fmt.Println("  --minimized                 Start normally, but minimize the console window")
	fmt.Println("                              right away (Windows only) — for launching from a")
	fmt.Println("                              startup script without popping up in your face.")
	fmt.Println("  --tools                     List every agent tool, grouped by category, and")
	fmt.Println("                              exit — the same tool list shown in the TUI")
	fmt.Println("                              (Alt+T), the web UI's Tools panel, and used by")
	fmt.Println("                              the agent itself; this is just a plain-text dump")
	fmt.Println("                              of it for scripting/piping from a shell.")
	fmt.Println("  --tools-extended            Same, but numbered and with the two example")
	fmt.Println("                              prompts shown for each tool (matches the TUI's")
	fmt.Println("                              Alt+T detail view) instead of just the name and")
	fmt.Println("                              one-line description.")
	fmt.Println("  --log [N]                   Print the last N activity log lines (default 100")
	fmt.Println("                              if N is omitted) and exit. Same log the TUI's")
	fmt.Println("                              [h] -> [g] view-log screen and its search read.")
	fmt.Println("  --export-backup [path]      Export every setting (email, telegram, tavily,")
	fmt.Println("                              web server, auto-update, disclaimer, etc.) to an")
	fmt.Println("                              encrypted file and exit. Path defaults to")
	fmt.Println("                              " + defaultBackupPath() + " if omitted.")
	fmt.Println("                              Same feature as [h] -> [x] in the menu.")
	fmt.Println("  --import-backup <path>      Import settings from an encrypted file made by")
	fmt.Println("                              --export-backup (or [h] -> [x]) and exit —")
	fmt.Println("                              overwrites current settings.")
	fmt.Println()
	fmt.Println("Run with no flags to open the normal interactive menu.")
}

// printCLITools dumps agentToolCategories — the exact same data structure
// the TUI's Alt+T browser and the web UI's /api/tools endpoint read from
// — as plain text, so all three surfaces (TUI, web UI, and this CLI
// listing) can never drift apart into three different tool lists.
func printCLITools(extended bool) {
	total := len(flatToolNames())
	fmt.Printf("llama-shell tools (%d)\n\n", total)
	num := 0
	for _, cat := range agentToolCategories {
		fmt.Println(cat.name + ":")
		for _, name := range cat.tools {
			num++
			if !extended {
				fmt.Printf("  %-26s %s\n", name, toolDescription(name))
				continue
			}
			fmt.Printf("  %d. %s\n", num, name)
			fmt.Printf("     %s\n", toolDescription(name))
			if ex, ok := toolExamples[name]; ok {
				fmt.Printf("     e.g. \"%s\"\n", ex[0])
				fmt.Printf("     e.g. \"%s\"\n", ex[1])
			}
			fmt.Println()
		}
		fmt.Println()
	}
}

// printCLILog prints the last n activity log lines, oldest first (a
// terminal reads top-to-bottom, so ascending timestamp order reads
// naturally, unlike the TUI/web UI's newest-first list view). Same
// underlying source (readLogLines) as those two.
func printCLILog(n int) {
	if n <= 0 {
		n = 100
	}
	lines := readLogLines("") // newest first
	if len(lines) > n {
		lines = lines[:n]
	}
	if len(lines) == 0 {
		fmt.Println("no log entries yet.")
		return
	}
	for i := len(lines) - 1; i >= 0; i-- {
		fmt.Println(lines[i])
	}
}

func main() {
	handleCLIArgs()
	cleanupOldExe()
	preventSleep()
	if startupMinimized {
		minimizeConsoleWindow()
	}
	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	go runAutoUpdateDaemon(p)
	if _, err := p.Run(); err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	if pendingRelaunchExePath != "" {
		cmd := exec.Command(pendingRelaunchExePath)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Println("auto-update: relaunch failed:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}
