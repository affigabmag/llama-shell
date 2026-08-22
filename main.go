package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// buildTime is overridden at build time via:
//   go build -ldflags "-X main.buildTime=<value>"
// Falls back to "dev" for a plain `go build` with no ldflags.
var buildTime = "dev"

var bannerColors = []string{
	"#5F87FF", // blue
	"#FFFFFF", // white
	"#FF5F5F", // red
	"#5FFF5F", // green
	"#5FFFFF", // cyan
	"#FF5FFF", // magenta
	"#FFFF5F", // yellow
}

// bannerGlyph is a 5-row-tall block letter. Every row must be the same
// width so letters line up; a blank glyph (the space between words) is
// just 5 empty rows of a given width.
var bannerGlyphs = map[rune][]string{
	'L': {
		"#    ",
		"#    ",
		"#    ",
		"#    ",
		"#####",
	},
	'A': {
		" ### ",
		"#   #",
		"#####",
		"#   #",
		"#   #",
	},
	'M': {
		"#   #",
		"## ##",
		"# # #",
		"#   #",
		"#   #",
	},
	'S': {
		" ####",
		"#    ",
		" ### ",
		"    #",
		"#### ",
	},
	'H': {
		"#   #",
		"#   #",
		"#####",
		"#   #",
		"#   #",
	},
	'E': {
		"#####",
		"#    ",
		"###  ",
		"#    ",
		"#####",
	},
	'-': {
		"     ",
		"     ",
		"#####",
		"     ",
		"     ",
	},
}

const bannerWord = "LLAMA-SHELL"

type view int

const (
	viewMenu view = iota
	viewListScan
	viewListTable
	viewPs
	viewShowScan
	viewShowTable
	viewDeviceInfo
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

type modelRow struct {
	Name     string `json:"name"`
	Size     string `json:"size"`
	Modified string `json:"modified"`
	Arch     string `json:"arch"`
	Params   string `json:"params"`
	Quant    string `json:"quant"`
	Context  string `json:"context"`

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
		return scanStepMsg{index: index, row: row}
	}
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

type menuItem struct {
	key   string
	label string
}

var menuItems = []menuItem{
	{"l", "list models      (ollama + library + huggingface)"},
	{"p", "running models    (ollama ps)"},
	{"s", "show model info   (scan all + cache)"},
	{"d", "device info       (cpu/ram/disk/gpu)"},
	{"q", "quit"},
}

type model struct {
	width, height int
	ollama        ollamaStatus

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

	scanRows  []modelRow
	scanTotal int
	scanDone  int
	scanErr   string
	fromCache bool
	scanCursor int

	scanAction     bool
	scanActionSel  int
	scanBusy       bool
	scanBusyLabel  string
	scanActionMsg  string

	catalogRows   []catalogRow
	catalogStage  int
	catalogErrs   []string
	catalogCursor int
	catalogSearch string

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

	psRows       []psRow
	psCursor     int
	psErr        string
	psKillTarget string
	psKilling    bool
	psKillMsg    string
	psConfirmKill bool
}

func initialModel() model {
	return model{
		ollama: checkOllama(),
	}
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) enterMenu(sel string) (tea.Model, tea.Cmd) {
	switch sel {
	case "l":
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
		m.view = viewDeviceInfo
		if info, ok := loadDeviceCache(); ok {
			m.deviceInfo = info
			m.deviceLoading = false
			return m, nil
		}
		m.deviceLoading = true
		m.deviceInfo = nil
		return m, gatherDeviceInfo
	case "q":
		return m, tea.Quit
	}
	return m, nil
}

func (m model) rescanCatalog() (tea.Model, tea.Cmd) {
	m.view = viewListScan
	m.catalogRows = nil
	m.catalogStage = 0
	m.catalogErrs = nil
	m.catalogCursor = 0
	return m, fetchCatalogStage(0)
}

func (m model) rescan() (tea.Model, tea.Cmd) {
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
		} else {
			m.psKillMsg = fmt.Sprintf("%s stopped.", msg.name)
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

		m.benchIndex++
		if m.benchIndex >= m.benchTotal {
			m.benchRunning = false
			m.benchDoneMsg = fmt.Sprintf("benchmark complete: %d model(s) measured.", m.benchTotal)
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
		case msg.err != nil:
			m.downloadMsg = fmt.Sprintf("download failed for %s:\n%s", name, msg.err.Error())
		default:
			m.downloadMsg = fmt.Sprintf("%s downloaded successfully.", name)
			m.installedDirty = true
		}
		m.downloadCh = nil
		m.downloadCmd = nil
		return m, nil

	case showRmMsg:
		m.scanBusy = false
		if msg.err != "" {
			m.scanActionMsg = fmt.Sprintf("failed to remove %s:\n%s\n%s", msg.name, msg.err, msg.output)
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
		return m, nil

	case showStopMsg:
		m.scanBusy = false
		if msg.err != "" {
			m.scanActionMsg = fmt.Sprintf("failed to stop %s:\n%s\n%s", msg.name, msg.err, msg.output)
		} else {
			m.scanActionMsg = fmt.Sprintf("%s stopped.", msg.name)
		}
		return m, nil

	case showRunDoneMsg:
		m.scanBusy = false
		if msg.err != nil {
			m.scanActionMsg = fmt.Sprintf("chat session with %s ended: %v", msg.name, msg.err)
		} else {
			m.scanActionMsg = fmt.Sprintf("chat session with %s ended.", msg.name)
		}
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

	case tea.KeyMsg:
		key := msg.String()

		switch m.view {
		case viewMenu:
			switch key {
			case "ctrl+c":
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
						m.scanBusy = true
						m.scanBusyLabel = fmt.Sprintf("running %s...", name)
						return m, runModelInteractive(name)
					case 1:
						m.scanBusy = true
						m.scanBusyLabel = fmt.Sprintf("removing %s...", name)
						return m, removeModel(name)
					case 2:
						m.scanBusy = true
						m.scanBusyLabel = fmt.Sprintf("stopping %s...", name)
						return m, stopModel(name)
					}
					return m, nil
				}
				switch key {
				case "up":
					m.scanActionSel--
					if m.scanActionSel < 0 {
						m.scanActionSel = 2
					}
				case "down":
					m.scanActionSel++
					if m.scanActionSel > 2 {
						m.scanActionSel = 0
					}
				case "esc", "n":
					m.scanAction = false
				case "enter":
					return choose(m.scanActionSel)
				case "x":
					return choose(0)
				case "r":
					return choose(1)
				case "k":
					return choose(2)
				}
				return m, nil
			}

			switch key {
			case "q":
				return m, tea.Quit
			case "esc":
				m.view = viewMenu
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

		case viewShowScan:
			switch key {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				m.view = viewMenu
				return m, nil
			}
			return m, nil

		case viewDeviceInfo:
			if key == "ctrl+c" {
				return m, tea.Quit
			}

			if m.benchDoneMsg != "" {
				m.benchDoneMsg = ""
				return m, nil
			}

			if m.benchRunning {
				if key == "c" && m.benchCmd != nil && m.benchCmd.Process != nil && !m.benchCancelling {
					m.benchCancelling = true
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
					return m, startBenchLoad(0, m.scanRows[0].Name)
				case "n", "esc":
					m.benchConfirm = false
				}
				return m, nil
			}

			switch key {
			case "q":
				return m, tea.Quit
			case "esc":
				m.view = viewMenu
				return m, nil
			case "r":
				if !m.deviceLoading {
					m.deviceLoading = true
					m.deviceInfo = nil
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

		case viewListTable:
			if key == "ctrl+c" {
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
					return m, startDownload(row)
				case "n", "esc":
					m.downloadConfirm = nil
					return m, nil
				}
				return m, nil
			}

			filtered := m.filteredCatalog()

			switch key {
			case "ctrl+r":
				return m.rescanCatalog()
			case "esc":
				if m.catalogSearch != "" {
					m.catalogSearch = ""
					m.catalogCursor = 0
					return m, nil
				}
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
				return m, tea.Quit
			case "esc":
				m.view = viewMenu
				return m, nil
			}
			return m, nil

		case viewPs:
			if key == "ctrl+c" {
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
					return m, killModel(name)
				case "n", "esc":
					m.psConfirmKill = false
					return m, nil
				}
				return m, nil
			}

			switch key {
			case "q":
				return m, tea.Quit
			case "esc":
				m.view = viewMenu
				return m, nil
			case "r":
				m.loading = true
				m.psRows = nil
				m.psErr = ""
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

	bodyBox := lipgloss.NewStyle().
		Width(m.width).
		Height(bodyHeight).
		Render(body)

	return lipgloss.JoinVertical(lipgloss.Left, header, bodyBox, footer)
}

func (m model) renderHeader() string {
	style := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#FFFFFF")).
		Background(lipgloss.Color("#5F5FAF")).
		Width(m.width).
		Padding(0, 1)
	return style.Render("llama-shell — Ollama TUI")
}

func (m model) renderFooter() string {
	status := lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5F5F")).Render("ollama: not installed")
	if m.ollama.installed {
		status = lipgloss.NewStyle().Foreground(lipgloss.Color("#5FFF5F")).Render(
			fmt.Sprintf("ollama: installed (%s)", m.ollama.version),
		)
	}

	left := fmt.Sprintf("build %s", buildTime)
	right := status

	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}

	line := left + strings.Repeat(" ", gap) + right

	style := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#AAAAAA")).
		Background(lipgloss.Color("#1A1A1A")).
		Width(m.width).
		Padding(0, 1)
	return style.Render(line)
}

var (
	selectedStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#000000")).Background(lipgloss.Color("#5FD7FF"))
	unselectedStyle = lipgloss.NewStyle()
	headerRowStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#5FD7FF"))
)

// renderBanner draws "LLAMA-SHELL" as block letters. Each letter is one
// solid color, pinned by its position in the word (1st=blue, 2nd=white,
// 3rd=red, 4th=green, 5th=cyan, ... repeating) — static, not animated.
func (m model) renderBanner() string {
	gapBlock := strings.Repeat("  \n", 4) + "  " // 5 blank rows, 2 cols wide

	blocks := make([]string, 0, len(bannerWord)*2)
	n := len(bannerWord)
	for i, letter := range bannerWord {
		glyph, ok := bannerGlyphs[letter]
		if !ok {
			continue
		}
		idx := i % len(bannerColors)
		style := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(bannerColors[idx]))
		// Render the whole 5-row glyph as ONE styled block so its color
		// can never differ row to row.
		blocks = append(blocks, style.Render(strings.Join(glyph, "\n")))
		if i != n-1 {
			blocks = append(blocks, gapBlock)
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, blocks...) + "\n"
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
		return msg + "\n\nEsc: back  ctrl+r: rescan"
	}

	catalogRows := m.filteredCatalog()
	if len(catalogRows) == 0 {
		return fmt.Sprintf(
			"list models\n\n%s\n\nno matches for %q.\n\nEsc: clear search  ctrl+r: rescan\n",
			searchLine, m.catalogSearch,
		)
	}
	if m.catalogCursor >= len(catalogRows) {
		m.catalogCursor = len(catalogRows) - 1
	}

	cols := []string{"NAME", "SOURCE", "LOCAL", "SIZE", "MODIFIED"}
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
		rowsData[i] = []string{r.Name, r.Source, local, r.Size, r.Modified}
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

	// figure out how many rows fit given terminal height
	const overhead = 12 // title/search/blank/colheader/footer lines + box padding + header/footer bars
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

	b.WriteString("\nType to search  Up/Down: select  Enter: download  Esc: clear search/back  ctrl+r: refresh\n")
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
		opts := []string{"run interactively (ollama run)", "remove (ollama rm)", "kill / stop (ollama stop)"}
		var b strings.Builder
		b.WriteString(fmt.Sprintf("show model info\n\n%s — choose an action\n\n", name))
		letters := []string{"x", "r", "k"}
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

	cols := []string{"NAME", "PARAMS", "QUANT", "CONTEXT", "ARCH", "SIZE", "CPU/GPU", "MATCH%"}
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
		rowsData[i] = []string{r.Name, r.Params, r.Quant, r.Context, r.Arch, r.Size, cpuGpu, match}
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

	b.WriteString("\nUp/Down: select  Enter: remove/kill/run  Esc: back  r: rescan (ignore cache)\n")
	b.WriteString("CPU/GPU + MATCH% come from the benchmark in Device Info ([d] from the main menu).\n")
	return b.String()
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
	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}
