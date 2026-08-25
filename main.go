package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get(updateRepoAPI)
		if err != nil {
			return updateCheckResultMsg{err: err.Error()}
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return updateCheckResultMsg{err: err.Error()}
		}
		if resp.StatusCode != 200 {
			return updateCheckResultMsg{err: fmt.Sprintf("status %d", resp.StatusCode)}
		}
		var rel githubRelease
		if err := json.Unmarshal(data, &rel); err != nil {
			return updateCheckResultMsg{err: err.Error()}
		}
		want := updateAssetName(runtime.GOOS, runtime.GOARCH)
		assetURL := ""
		for _, a := range rel.Assets {
			if a.Name == want {
				assetURL = a.BrowserDownloadURL
				break
			}
		}
		return updateCheckResultMsg{latestVersion: rel.TagName, assetURL: assetURL}
	}
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
			Description: "Run a shell command on this machine (via cmd.exe) and return its combined stdout/stderr output.",
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
			Description: "Forcibly terminate a running process by name or PID.",
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
			Title   string `xml:"title"`
			Link    struct {
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
func executeAgentToolWithImages(workDir, name string, args map[string]interface{}) (string, []string) {
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
		var b strings.Builder
		b.WriteString("Web UI links (each includes the required access token):\n\n")
		b.WriteString(webServerURLFor("127.0.0.1", cfg.Token) + " (this machine only)\n\n")
		for _, ip := range localLANIPv4s() {
			b.WriteString(webServerURLFor(ip, cfg.Token) + " (same WiFi/LAN)\n\n")
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
		reply, err := ollamaChatStream(modelName, msgs, tools, nil)
		if err != nil && toolsSupported && shouldRetryWithoutTools(err) {
			toolsSupported = false
			reply, err = ollamaChatStream(modelName, msgs, nil, nil)
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

const webServerPort = 8787

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
func webServerURL(token string) string {
	return webServerURLFor("127.0.0.1", token)
}

func webServerURLFor(host, token string) string {
	return fmt.Sprintf("http://%s:%d/?token=%s", host, webServerPort, token)
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
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", webServerPort))
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
	srv := &http.Server{Handler: mux}
	webServerHTTP = srv
	go func() {
		_ = srv.Serve(ln)
	}()
	appendLog("web server started on %s (model %s)", webServerURL(cfg.Token), cfg.Model)
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
  #toolSearch { width: 100%; margin-top: 10px; background: var(--bg-inset); color: var(--text);
                border: 1px solid var(--border); border-radius: 8px; padding: 8px 12px; font: inherit;
                font-size: 13px; outline: none; }
  #toolSearch:focus { border-color: var(--accent); }
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
  <a class="menuItem gh-link" href="https://github.com/affigabmag/llama-shell" target="_blank" rel="noopener">GitHub</a>
  <span class="menuItem" id="toolsBtnMobile">🛠 Tools</span>
  <span class="menuItem" id="helpBtnMobile">❓ Help</span>
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
  chat.innerHTML = items.map(it => {
    if (it.type === 'user') {
      return '<div class="msg user"><div class="role">You</div><div class="content">' + renderMarkdown(it.content) + '</div></div>';
    }
    if (it.type === 'steps') {
      return '<div class="msg assistant"><div class="role">Assistant</div><div class="content">' +
        it.steps.map(renderStep).join('') + '</div></div>';
    }
    const isErr = it.content.startsWith('(error)');
    return '<div class="msg assistant' + (isErr ? ' error' : '') + '"><div class="role">Assistant</div><div class="content">' +
      renderMarkdown(it.content) + '</div></div>';
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
      '<p>Click a row to fill the input with an example prompt, or the copy icon to copy it '+
      'without filling the box.</p>');
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

const menuBtn = document.getElementById('menuBtn');
const menuDropdown = document.getElementById('menuDropdown');
menuBtn.onclick = (e) => { e.stopPropagation(); menuDropdown.classList.toggle('open'); };
document.getElementById('toolsBtnMobile').onclick = () => { menuDropdown.classList.remove('open'); openTools(); };
document.getElementById('helpBtnMobile').onclick = () => { menuDropdown.classList.remove('open'); openHelp(); };
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
  if (s < 4) return 'Thinking';
  if (s < 8) return 'Reasoning';
  if (s < 14) return 'Working through it';
  if (s < 20) return 'Almost done';
  const phrases = ['Still going', 'Taking a while', 'Almost done'];
  return phrases[Math.floor((s - 20) / 6) % phrases.length];
}

function autoGrow() {
  input.style.height = 'auto';
  input.style.height = Math.min(input.scrollHeight, 200) + 'px';
}
input.addEventListener('input', autoGrow);

let busy = false;
let currentController = null;
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
  clearInterval(warmupTimer);
  clearInterval(warmupPollTimer);
  setBusy(false);
  currentController = null;
  render();
  input.focus();
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
	updated, err := runAgentTurnSync(modelName, history, workDir, true)
	if err != nil {
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
	})
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

// runTelegramPollLoop is the bot's whole lifetime: poll, answer, repeat.
// It auto-binds to the first chat that ever messages it (cfg.ChatID == 0)
// and persists that binding, then rejects any other chat ID from then on
// — otherwise anyone who discovers the bot's @username would get the same
// full local tool access (files, commands, network) you have here.
func runTelegramPollLoop(ctx context.Context, cfg telegramConfig, workDir string) {
	var offset int64
	history := []ollamaChatMsg{{Role: "system", Content: agentSystemPrompt(workDir)}}
	boundChatID := cfg.ChatID
	consecutiveErrs := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		updates, err := telegramGetUpdates(cfg.Token, offset)
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
			if boundChatID == 0 {
				boundChatID = chatID
				cfg.ChatID = chatID
				_ = saveTelegramConfig(cfg)
				appendLog("telegram: bound to chat %d", chatID)
			} else if chatID != boundChatID {
				_ = telegramSendMessage(cfg.Token, chatID, "This bot is bound to a different chat.")
				continue
			}
			history = append(history, ollamaChatMsg{Role: "user", Content: u.Message.Text})

			// Instant ack so the chat doesn't look like a dead end while a
			// small model + tool calls can take anywhere from seconds to a
			// couple minutes — plus Telegram's native "typing..." indicator,
			// refreshed on a ticker since it only lasts ~5s per call.
			_ = telegramSendMessage(cfg.Token, chatID, "⏳ Got it — working on it...")
			typingDone := make(chan struct{})
			go func() {
				_ = telegramSendChatAction(cfg.Token, chatID)
				ticker := time.NewTicker(4 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-typingDone:
						return
					case <-ticker.C:
						_ = telegramSendChatAction(cfg.Token, chatID)
					}
				}
			}()

			updated, err := runAgentTurnSync(cfg.Model, history, workDir, true)
			close(typingDone)
			if err != nil {
				_ = telegramSendMessage(cfg.Token, chatID, "error: "+err.Error())
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
			if err := telegramSendMessage(cfg.Token, chatID, cleanMarkdownForDisplay(reply)); err != nil {
				appendLog("telegram: sendMessage error: %s", err.Error())
			}
		}
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

// readLogTail returns the last n lines of the activity log, or a
// placeholder if there's nothing yet.
func readLogTail(n int) string {
	data, err := os.ReadFile(logFilePath())
	if err != nil || len(data) == 0 {
		return "no log entries yet."
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
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
	{"u", "update", viewUpdateText},
	{"w", "setup wizard (install ollama + download starter models)", viewWizard},
	{"t", "tavily API key (enables tavily_search/tavily_extract tools)", viewTavilySettings},
	{"b", "web server (browser access to agentic chat)", viewWebServerSettings},
	{"m", "telegram bot (chat with the agent from your phone)", viewTelegramSettings},
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

	webServerBusy        bool
	webServerMsg         string
	webServerAwaitingDL  bool
	webServerModelList   []string
	webServerModelCursor int

	telegramTokenInput string
	telegramMsg        string

	wizardPendingTelegram bool // chain from wizard: tavily screen's exit routes to telegram next

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
	agentScroll         int // lines scrolled back from the bottom; 0 = live/latest
	agentStreamBuf      string
	agentViewport       viewport.Model
	agentVPReady        bool
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
	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(checkForUpdate(), cubeTickCmd())
}

func (m model) enterMenu(sel string) (tea.Model, tea.Cmd) {
	switch sel {
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

	case ollamaInstallResultMsg:
		m.ollamaInstallRunning = false
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
						m.agentViewport = viewport.New(agentViewportWidth(m.width), agentViewportHeight(m.height))
						m.agentVPReady = true
						m.syncAgentViewport()
						m.view = viewAgentChat
						appendLog("started agentic chat with %s", name)
						return m, tea.Batch(warmupPollOllama(name), agentTickCmd())
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
				m.view = viewShowTable
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
				if len(key) == 1 || key == "space" {
					if key == "space" {
						key = " "
					}
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
						}
						appendLog("opened %s", it.label)
					}
				}
			}
			return m, nil

		case viewTavilySettings:
			switch key {
			case "ctrl+c":
				appendLog("quit")
				return m, tea.Quit
			case "esc":
				if m.wizardPendingTelegram {
					m.wizardPendingTelegram = false
					m.view = viewTelegramSettings
				} else {
					m.view = viewHelpMenu
				}
				m.tavilyKeyInput = ""
				m.tavilyKeyMsg = ""
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
				if m.wizardPendingTelegram {
					m.wizardPendingTelegram = false
					m.view = viewTelegramSettings
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
			switch key {
			case "ctrl+c":
				appendLog("quit")
				return m, tea.Quit
			case "esc":
				m.view = viewHelpMenu
				m.telegramTokenInput = ""
				m.telegramMsg = ""
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
				appendLog("telegram bot token saved")
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
				if m.wizardAnswers["tavily"] {
					m.wizardAnswers["tavily"] = false // consume so the next keypress here goes to the menu, not a loop
					if m.wizardAnswers["telegram"] {
						m.wizardPendingTelegram = true // chain: tavily screen's exit routes to telegram next
					}
					m.view = viewTavilySettings
					appendLog("wizard: opening tavily key setup")
					return m, nil
				}
				if m.wizardAnswers["telegram"] {
					m.wizardAnswers["telegram"] = false
					m.view = viewTelegramSettings
					appendLog("wizard: opening telegram bot setup")
					return m, nil
				}
				m.view = viewMenu
				m.wizardPhase = ""
				return m, nil
			}
			return m, nil

		case viewHelpText, viewDisclaimerText, viewLogText:
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

		case viewUpdateText:
			if m.updateDownloading {
				return m, nil
			}
			if key == "ctrl+c" {
				appendLog("quit")
				return m, tea.Quit
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
		content = renderLogText()
	case viewToolCategories:
		content, _ = m.renderToolCategories()
	case viewTavilySettings:
		content = m.renderTavilySettings()
	case viewTelegramSettings:
		content = m.renderTelegramSettings()
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
	status := lipgloss.NewStyle().Bold(true).Blink(true).Foreground(lipgloss.Color("#FF5F5F")).Background(lipgloss.Color(footerBG)).Render("ollama: not installed")
	if m.ollama.installed {
		status = lipgloss.NewStyle().Foreground(lipgloss.Color("#5FFF5F")).Background(lipgloss.Color(footerBG)).Render("ollama✓")
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
		webURL := webServerURL(cfg.Token)
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
	left := plain.Render(fmt.Sprintf("build %s", buildTime)) + plain.Render("  ") + status

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
const cityCols = 58
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
	"Abidjan", "Abu Dhabi", "Abuja", "Accra", "Addis Ababa", "Adelaide", "Algiers", "Almaty",
	"Amman", "Amsterdam", "Ankara", "Antananarivo", "Antwerp", "Ashgabat", "Asmara", "Astana",
	"Asunción", "Athens", "Atlanta", "Auckland", "Baghdad", "Baku", "Bamako", "Bandar Seri Begawan",
	"Bangalore", "Bangkok", "Bangui", "Banjul", "Barcelona", "Basel", "Basra", "Beijing",
	"Beirut", "Belgrade", "Belfast", "Belmopan", "Berlin", "Bern", "Bhopal", "Bilbao",
	"Birmingham", "Bishkek", "Bissau", "Bogotá", "Boise", "Bologna", "Bonn", "Bordeaux",
	"Boston", "Brasília", "Bratislava", "Brazzaville", "Bridgetown", "Brisbane", "Bristol", "Brussels",
	"Bucharest", "Budapest", "Buenos Aires", "Bujumbura", "Busan", "Cairo", "Calgary", "Cali",
	"Canberra", "Cape Town", "Caracas", "Cardiff", "Casablanca", "Castries", "Cebu City", "Chandigarh",
	"Chengdu", "Chennai", "Chicago", "Chihuahua", "Chisinau", "Chittagong", "Christchurch", "Cluj-Napoca",
	"Cologne", "Colombo", "Conakry", "Copenhagen", "Cordoba", "Curitiba", "Dakar", "Dallas",
	"Damascus", "Da Nang", "Dar es Salaam", "Denver", "Detroit", "Dhaka", "Dijon", "Dili",
	"Djibouti", "Doha", "Dodoma", "Doncaster", "Dortmund", "Dresden", "Dubai", "Dublin",
	"Dushanbe", "Düsseldorf", "Edinburgh", "Edmonton", "Erbil", "Faisalabad", "Florence", "Fortaleza",
	"Frankfurt", "Freetown", "Fresno", "Fukuoka", "Funafuti", "Gaborone", "Gaziantep", "Geneva",
	"Genoa", "Georgetown", "Gothenburg", "Guadalajara", "Guangzhou", "Guatemala City", "Guayaquil", "Hague, The",
	"Hamburg", "Hangzhou", "Hanoi", "Harare", "Harbin", "Havana", "Helsinki", "Ho Chi Minh City",
	"Hobart", "Honiara", "Honolulu", "Houston", "Hyderabad", "Ibadan", "Incheon", "Indianapolis",
	"Islamabad", "Istanbul", "Jaipur", "Jakarta", "Jeddah", "Jerusalem", "Johannesburg", "Juba",
	"Kabul", "Kampala", "Kano", "Kansas City", "Karachi", "Kathmandu", "Kaunas", "Kigali",
	"Kingston", "Kingstown", "Kinshasa", "Kobe", "Kolkata", "Krakow", "Kuala Lumpur", "Kuching",
	"Kuwait City", "Kyiv", "Kyoto", "La Paz", "Lagos", "Lahore", "Las Vegas", "Leeds",
	"Leipzig", "Libreville", "Lilongwe", "Lima", "Lisbon", "Ljubljana", "Lomé", "London",
	"Los Angeles", "Luanda", "Lubumbashi", "Lucknow", "Lusaka", "Luxembourg City", "Lyon", "Macau",
	"Madrid", "Majuro", "Malabo", "Malé", "Managua", "Manama", "Manaus", "Manchester",
	"Manila", "Maputo", "Marrakesh", "Marseille", "Maseru", "Mbabane", "Medellín", "Melbourne",
	"Memphis", "Mexico City", "Miami", "Milan", "Minsk", "Mogadishu", "Monaco", "Monrovia",
	"Monterrey", "Montevideo", "Montreal", "Moroni", "Moscow", "Mumbai", "Munich", "Muscat",
	"Nagoya", "Nairobi", "Nanjing", "Naples", "Nassau", "N'Djamena", "New Delhi", "New Orleans",
	"New York City", "Niamey", "Nicosia", "Nouakchott", "Nur-Sultan", "Nuremberg", "Oklahoma City", "Omaha",
	"Osaka", "Oslo", "Ottawa", "Ouagadougou", "Palikir", "Panama City", "Paramaribo", "Paris",
	"Perth", "Phnom Penh", "Phoenix", "Podgorica", "Port-au-Prince", "Port Louis", "Port Moresby", "Port of Spain",
	"Port Vila", "Porto", "Porto Alegre", "Portland", "Poznań", "Prague", "Praia", "Pretoria",
	"Pristina", "Pusan", "Pyongyang", "Quebec City", "Quito", "Rabat", "Raleigh", "Ramallah",
	"Recife", "Reykjavik", "Riga", "Rio de Janeiro", "Riyadh", "Rome", "Rosario", "Rotterdam",
	"Sacramento", "Saint-Denis", "Salt Lake City", "Salvador", "Samara", "San Antonio", "San Diego", "San José",
	"San Juan", "San Marino", "San Salvador", "Sana'a", "Santiago", "Santo Domingo", "São Paulo", "Sapporo",
	"Sarajevo", "Seattle", "Seoul", "Sevilla", "Shanghai", "Shenzhen", "Singapore", "Skopje",
	"Sofia", "Split", "St. Louis", "St. Petersburg", "Stockholm", "Strasbourg", "Stuttgart", "Suva",
	"Suzhou", "Sydney", "Taipei", "Tallinn", "Tashkent", "Tbilisi", "Tegucigalpa", "Tehran",
	"Tel Aviv", "Thimphu", "Tianjin", "Tijuana", "Tirana", "Tokyo", "Toronto", "Toulouse",
	"Tripoli", "Tunis", "Turin", "Ulaanbaatar", "Utrecht", "Vaduz", "Valencia", "Valletta",
	"Vancouver", "Vatican City", "Venice", "Victoria", "Vienna", "Vientiane", "Vilnius", "Warsaw",
	"Washington DC", "Wellington", "Winnipeg", "Wuhan", "Xi'an", "Yamoussoukro", "Yangon", "Yaoundé",
	"Yekaterinburg", "Yerevan", "Yokohama", "Zagreb", "Zürich", "Aarhus", "Aberdeen", "Adana",
	"Agra", "Ahmedabad", "Akron", "Albany", "Albuquerque", "Alexandria", "Amritsar", "Anaheim",
	"Ankara", "Ann Arbor", "Antalya", "Antioch", "Arequipa", "Arlington", "Aruba", "Astana",
	"Athens (Georgia)", "Augsburg", "Aurora", "Austin", "Bahia Blanca", "Baku", "Balikpapan", "Baltimore",
	"Bamberg", "Bandung", "Bataan", "Bath", "Baton Rouge", "Bedford", "Belém", "Belgorod",
	"Bendigo", "Bergen", "Bexley", "Bhubaneswar", "Białystok", "Blackpool", "Bloemfontein", "Bolton",
	"Bordertown", "Bradford", "Braga", "Brampton", "Brighton", "Brno", "Buffalo", "Burgas",
	"Bydgoszcz", "Cagliari", "Cairns", "Calabar", "Calgary East", "Cambridge", "Campinas", "Cancún",
	"Canterbury", "Cape Coral", "Cartagena", "Catania", "Cebu", "Charleston", "Charlotte", "Chattanooga",
	"Cherbourg", "Chiba", "Chico", "Cincinnati", "Ciudad Juárez", "Cleveland", "Coimbra", "Colombo (Sri Lanka)",
	"Columbus", "Constanța", "Coventry", "Coyoacán", "Cuiabá", "Culiacán", "Cuzco", "Dammam",
	"Davao City", "Dayton", "Debrecen", "Delft", "Derby", "Des Moines", "Dnipro", "Donetsk",
	"Durban", "Durham", "East London", "Eindhoven", "El Paso", "Erie", "Essen", "Exeter",
	"Fez", "Florianópolis", "Fort Worth", "Fukushima", "Gdańsk", "Gdynia", "Gent", "Gijón",
	"Glasgow", "Goiânia", "Gold Coast", "Graz", "Grenoble", "Guadalupe", "Gwangju", "Halifax",
	"Hamilton", "Hangzhou West", "Hartford", "Heraklion", "Hermosillo", "Hiroshima", "Hobart Town", "Hokkaido",
	"Holguín", "Iasi", "Ibiza Town", "Indore", "Inverness", "Iquitos", "Ipoh", "Irkutsk",
	"Izmir", "Jacksonville", "Jerez de la Frontera", "Jinan", "João Pessoa", "Johor Bahru", "Jönköping", "Kaliningrad",
	"Kanazawa", "Kanpur", "Katowice", "Kazan", "Kelowna", "Kemerovo", "Khabarovsk", "Kharkiv",
	"Kingston upon Hull", "Kirov", "Kitakyushu", "Klagenfurt", "Kobenhavn", "Kochi", "Košice", "Kraków",
	"Kumamoto", "Kunming", "La Coruña", "Lausanne", "Le Havre", "León", "Liège", "Lille",
	"Limoges", "Linz", "Little Rock", "Liverpool", "Łódź", "Louisville", "Lviv", "Maastricht",
	"Makassar", "Malaga", "Mandalay", "Mannheim", "Maracaibo", "Mar del Plata", "Matsuyama", "Medan",
	"Mérida", "Messina", "Milwaukee", "Minneapolis", "Mombasa", "Montpellier", "Montreux", "Mysore",
	"Nagano", "Nagasaki", "Nairobi West", "Nanaimo", "Nancy", "Nanning", "Nantes", "Nashville",
	"Newcastle", "Niigata", "Nizhny Novgorod", "Northampton", "Norwich", "Novosibirsk", "Nur City", "Oaxaca",
	"Odense", "Odesa", "Okayama", "Omdurman", "Ontario", "Orenburg", "Orlando", "Oshawa",
	"Oulu", "Padua", "Palembang", "Palermo", "Pamplona", "Panama", "Pärnu", "Peoria",
	"Perm", "Perpignan", "Philadelphia", "Pittsburgh", "Plymouth", "Poitiers", "Ponce", "Portsmouth",
	"Poznan", "Puebla", "Pune", "Querétaro", "Quezon City", "Regina", "Reims", "Rennes",
	"Richmond", "Rochester", "Rostock", "Rostov-on-Don", "Rotterdam West", "Sacramento North", "Saitama", "Salamanca",
	"Salzburg", "San Bernardino", "San Luis Potosí", "Sankt Pölten", "Santa Cruz", "Santa Fe", "Santander", "Saratov",
	"Saskatoon", "Semarang", "Sendai", "Sheffield", "Shizuoka", "Sibiu", "Sochi", "Southampton",
	"Split (Croatia)", "St. John's", "Stavanger", "Stavropol", "Stoke-on-Trent", "Sucre", "Surabaya", "Surat",
	"Sverdlovsk", "Szczecin", "Tacoma", "Tainan", "Taichung", "Tallahassee", "Tampa", "Tampere",
	"Tangier", "Tarragona", "Thessaloniki", "Tijuana North", "Timişoara", "Toledo", "Tomsk", "Torreón",
	"Toulon", "Townsville", "Trieste", "Trois-Rivières", "Trondheim", "Tucson", "Tulsa", "Turku",
	"Ufa", "Umeå", "Valdivia", "Valparaíso", "Varna", "Vaasa", "Verona", "Veracruz",
	"Vigo", "Villahermosa", "Vitoria-Gasteiz", "Vladivostok", "Volgograd", "Wakayama", "Waterloo", "Wichita",
	"Wiesbaden", "Windhoek", "Windsor", "Winston-Salem", "Wolverhampton", "Worcester", "Wrocław", "Wuppertal",
	"Wuxi", "Xiamen", "Yangzhou", "Yaroslavl", "Yokosuka", "York", "Yueyang", "Zadar",
	"Zaragoza", "Zhengzhou", "Zibo", "Zonguldak",
}

var cityCurrentName string
var cityIndex int
var cityOrder []int
var cityIndexLoaded bool

var cityBuildings []cityBuilding
var cityLastGen time.Time
var cityMu sync.Mutex

const cityRegenInterval = 10 * time.Second

// buildCityScene lays out a skyline, regenerating it every cityRegenInterval
// — buildings keep a fixed silhouette and a golden-angle hue each (like the
// "ASCII City" reference) between regens, only their lit-window shimmer
// animates per tick.
func buildCityScene() {
	cityMu.Lock()
	defer cityMu.Unlock()
	if cityBuildings != nil && time.Since(cityLastGen) < cityRegenInterval {
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
	saveCityProgress(cityProgress{Order: cityOrder, Position: cityIndex})
	cityLastGen = time.Now()
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
	buildCityScene()
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
	imgSearchURL := "https://www.google.com/search?tbm=isch&q=" + url.QueryEscape(cityCurrentName)
	labelText := lipgloss.NewStyle().Bold(true).Underline(true).Foreground(lipgloss.Color("#EEF3FF")).Render(cityCurrentName)
	label := "\x1b]8;;" + imgSearchURL + "\x1b\\" + labelText + "\x1b]8;;\x1b\\"
	return label + "\n" + out.String() + "\n"
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
	case viewWebServerSettings:
		return box.Render(m.renderWebServerSettings())
	case viewWebServerModelSelect:
		return box.Render(m.renderWebServerModelSelect())
	case viewHelpText:
		return box.Render(m.scrollHelpBody(renderHelpText()))
	case viewDisclaimerText:
		return box.Render(m.scrollHelpBody(renderDisclaimerText()))
	case viewLogText:
		return box.Render(m.scrollHelpBody(renderLogText()))
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

	current := os.Getenv("TAVILY_API_KEY")
	if current != "" {
		masked := current
		if len(masked) > 8 {
			masked = masked[:4] + strings.Repeat("*", len(masked)-8) + masked[len(masked)-4:]
		}
		b.WriteString(helpKeyStyle.Render("current key: "+masked) + "\n\n")
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
	running := isWebServerRunning()
	switch {
	case running:
		b.WriteString(helpKeyStyle.Render("● running") + fmt.Sprintf("  —  model: %s\n\n", cfg.Model))
		b.WriteString("  " + hyperlink(webServerURL(cfg.Token), greenLinkStyle) + "\n\n")
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

	if m.webServerMsg != "" {
		b.WriteString(m.webServerMsg + "\n\n")
	}
	b.WriteString(helpKeyStyle.Render("[e] enable") + " (choose/confirm model)    " + helpKeyStyle.Render("[d] disable") + "\n")
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

func renderLogText() string {
	return "activity log (most recent entries)\n\n" + readLogTail(200) + "\n\nEsc: back\n"
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
			run[idx].text = reverseRunes(run[idx].text)
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

func renderPrefixedChatLines(prefix, content string, width int, prefixStyle lipgloss.Style) []string {
	wrapped := wrapLines(prefix+content, width)
	out := make([]string, len(wrapped))
	for i, l := range wrapped {
		if i == 0 && strings.HasPrefix(l, prefix) {
			out[i] = prefixStyle.Render(prefix) + linkifyLine(l[len(prefix):], agentToolStyle)
		} else {
			out[i] = linkifyLine(l, agentToolStyle)
		}
	}
	return out
}

func buildAgentChatLines(width int, messages []ollamaChatMsg, modelName string, capabilities string, warmup string, spinnerFrame int, warmupElapsed time.Duration) []string {
	var lines []string
	toolNames := flatToolNames()
	lines = append(lines, agentHeadStyle.Render(fmt.Sprintf("%d tools available — Alt+T to browse by category", len(toolNames))))
	lines = append(lines, renderCapabilityBadges(capabilities))
	if s := renderWarmupStatus(warmup, spinnerFrame, warmupElapsed); s != "" {
		lines = append(lines, s)
	}
	lines = append(lines, "")
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
	lines := buildAgentChatLines(agentViewportWidth(m.width), m.agentMessages, m.agentModelName, m.agentCapabilities, m.agentWarmup, m.agentSpinner, time.Since(m.agentWarmupStarted))
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
		// Not reordered here on purpose: this is the live, still-being-typed
		// input line, and flipping word/character order on every keystroke
		// would make the cursor position and text jump around as you type.
		// Once the message is sent it renders through linkifyLine (via
		// renderPrefixedChatLines), which does apply the RTL fix.
		bottom.WriteString(agentUserStyle.Render("you> ") + agentToolStyle.Render(m.agentInput+"█") + "\n")
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
		"get_public_ip", "get_web_ui_url", "ssh_run", "list_network_interfaces",
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

func main() {
	cleanupOldExe()
	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}
