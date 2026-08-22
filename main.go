package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
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
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// buildTime is overridden at build time via:
//   go build -ldflags "-X main.buildTime=<value>"
// Falls back to "dev" for a plain `go build` with no ldflags.
var buildTime = "dev"

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
	viewHelpMenu
	viewHelpText
	viewDisclaimerText
	viewLogText
	viewFirstRunDisclaimer
	viewAgentChat
	viewToolCategories
	viewAgentHelp
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
// otherwise falls back to plain text — so Ctrl+V in agentic chat works for
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
		out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
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
	htmlTagRe = regexp.MustCompile(`(?is)<script.*?</script>|<style.*?</style>|<[^>]+>`)
	htmlWSRe  = regexp.MustCompile(`\s+`)

	ddgResultLinkRe = regexp.MustCompile(`(?s)<a rel="nofollow" class="result__a" href="([^"]+)">(.*?)</a>`)
	ddgSnippetRe    = regexp.MustCompile(`(?s)<a class="result__snippet"[^>]*>(.*?)</a>`)
)

// stripHTMLTags turns raw HTML into plain readable text: script/style
// blocks and tags are dropped, then whitespace is collapsed.
func stripHTMLTags(s string) string {
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = htmlWSRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
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

	const maxResults = 8
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
		client := &http.Client{Timeout: 20 * time.Second}
		resp, err := client.Get(toolArgString(args, "url"))
		if err != nil {
			return "error: " + err.Error()
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
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
		client := &http.Client{Timeout: 20 * time.Second}
		resp, err := client.Get(toolArgString(args, "url"))
		if err != nil {
			return "error: " + err.Error()
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return "error: " + err.Error()
		}
		return truncateToolOutput(stripHTMLTags(string(data)))

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
		psCmd := fmt.Sprintf("Compress-Archive -Path '%s' -DestinationPath '%s' -Force", src, dst)
		if out, err := exec.Command("powershell", "-NoProfile", "-Command", psCmd).CombinedOutput(); err != nil {
			return "error: " + err.Error() + "\n" + string(out)
		}
		return "compressed to: " + dst

	case "extract_zip":
		src := resolveAgentPath(workDir, toolArgString(args, "zip_path"))
		dst := resolveAgentPath(workDir, toolArgString(args, "dest"))
		psCmd := fmt.Sprintf("Expand-Archive -Path '%s' -DestinationPath '%s' -Force", src, dst)
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

	default:
		return "error: unknown tool " + name
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
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return ollamaChatMsg{}, err
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

type agentTickMsg struct{}

func agentTickCmd() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg {
		return agentTickMsg{}
	})
}

// runAgentTurn drives the tool-call loop for one user message: stream the
// model's reply token-by-token, and while it keeps asking for tools, run
// them locally and feed the results back, until it replies with plain
// content or maxSteps is hit. Runs in a goroutine and reports back over a
// channel so the UI can update live as tokens arrive.
func runAgentTurn(modelName string, history []ollamaChatMsg, workDir string, toolsSupported bool) tea.Cmd {
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
			if toolsSupported {
				tools = agentTools()
			}
			reply, err := attempt(tools)
			// Some models report no "tools" capability at all — Ollama
			// rejects the request outright rather than just ignoring the
			// tool list. Downgrade to a plain chat (no file/tool access)
			// instead of hard-failing, and remember the downgrade so later
			// turns in this same chat skip straight to the plain request.
			if err != nil && toolsSupported && strings.Contains(err.Error(), "does not support tools") {
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
				toolResult := executeAgentTool(workDir, tc.Function.Name, tc.Function.Arguments)
				msgs = append(msgs, ollamaChatMsg{Role: "tool", Content: toolResult})
			}
			ch <- agentStepMsg{ch: ch, messages: append([]ollamaChatMsg(nil), msgs...), toolsSupported: toolsSupported}
		}
		ch <- agentTurnDoneMsg{messages: msgs, toolsSupported: toolsSupported}
	}()
	return waitForAgentStream(ch)
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
	{"h", "help / disclaimer / log"},
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

	helpMenuCursor int
	helpScroll     int

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

	// Agentic chat (own agent, read/write files), started from the
	// "show model info" action menu.
	agentModelName      string
	agentWorkDir        string
	agentCapabilities   string
	agentToolsSupported bool
	agentMessages  []ollamaChatMsg
	agentInput     string
	agentBusy      bool
	agentErr       string
	agentSpinner   int
	agentStarted   time.Time
	agentScroll    int // lines scrolled back from the bottom; 0 = live/latest
	agentStreamBuf string
	agentViewport  viewport.Model
	agentVPReady   bool
}

func initialModel() model {
	m := model{
		ollama: checkOllama(),
	}
	if !isDisclaimerAccepted() {
		m.view = viewFirstRunDisclaimer
	}
	return m
}

func (m model) Init() tea.Cmd {
	return nil
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

	case clipboardPasteMsg:
		switch {
		case msg.err != "":
			m.agentErr = "clipboard paste failed: " + msg.err
		case msg.imagePath != "":
			if m.agentInput != "" && !strings.HasSuffix(m.agentInput, " ") {
				m.agentInput += " "
			}
			m.agentInput += msg.imagePath
			appendLog("pasted image from clipboard: %s", msg.imagePath)
		case msg.text != "":
			m.agentInput += msg.text
		}
		return m, nil

	case agentStreamDeltaMsg:
		m.agentStreamBuf += msg.delta
		return m, waitForAgentStream(msg.ch)

	case agentStepMsg:
		m.agentMessages = msg.messages
		m.agentStreamBuf = ""
		m.agentToolsSupported = msg.toolsSupported
		m.syncAgentViewport()
		return m, waitForAgentStream(msg.ch)

	case agentTurnDoneMsg:
		m.agentBusy = false
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
		if !m.agentBusy {
			return m, nil
		}
		m.agentSpinner++
		return m, agentTickCmd()

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
						m.agentMessages = []ollamaChatMsg{
							{Role: "system", Content: fmt.Sprintf(
								"You are a coding assistant running locally with REAL, WORKING file system access "+
									"through the tools read_file, write_file, append_file, list_dir, make_dir, "+
									"delete_file, and search_files. These tools run on the actual local machine and "+
									"can reach ANY path on this computer's disks — not just one folder. A relative "+
									"path (no drive letter) resolves against the working directory %s; an absolute "+
									"path (e.g. C:\\, C:\\Users\\name\\file.txt, D:\\projects) is used exactly as given "+
									"and is NOT restricted to the working directory. There is no sandbox or "+
									"permission wall here beyond normal OS file permissions. "+
									"You are NOT a hosted AI with no file access — that is false for this session. "+
									"Never say you cannot access files, cannot read a given path, or are restricted "+
									"to one directory: you are not. "+
									"Note the tool distinction: list_dir(path) lists a directory's contents; "+
									"read_file(path) reads one file's contents — pick whichever matches what the "+
									"user asked for. "+
									"Whenever the user asks you to read, inspect, list, create, or write to a file "+
									"or the file system, you MUST call the matching tool immediately instead of "+
									"asking for clarification or describing what the user should do manually — try "+
									"it, then report the actual tool result (including any error it returns) in "+
									"plain text. "+
									"The tools are an ADDITION to your normal abilities, not a replacement: you can "+
									"still chat normally, answer questions, brainstorm, write essays, summaries, "+
									"lists, code, or any other text content directly from your own knowledge, with "+
									"no tool call needed, exactly like any other assistant. Only reach for a tool "+
									"when the request actually needs file access or one of the other listed "+
									"system actions (running a command, opening something, networking, etc). "+
									"Never refuse a normal request by claiming you're 'only' a file/tool assistant "+
									"— that is false.", wd,
							)},
						}
						m.agentInput = ""
						m.agentErr = ""
						m.agentBusy = false
						m.agentViewport = viewport.New(agentViewportWidth(m.width), agentViewportHeight(m.height))
						m.agentVPReady = true
						m.syncAgentViewport()
						m.view = viewAgentChat
						appendLog("started agentic chat with %s", name)
						return m, nil
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
			case "ctrl+h":
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
			case "ctrl+t":
				appendLog("opened tool categories")
				m.view = viewToolCategories
				return m, nil
			case "ctrl+h":
				appendLog("opened agent help")
				m.view = viewAgentHelp
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
				if len(missingPaths) > 0 {
					m.agentErr = fmt.Sprintf("image not attached, file not found: %s (drag-and-drop managers like Ditto sometimes show a path that isn't a real file — use Ctrl+V to paste the image itself instead)", strings.Join(missingPaths, ", "))
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
				return m, tea.Batch(runAgentTurn(m.agentModelName, m.agentMessages, m.agentWorkDir, m.agentToolsSupported), agentTickCmd())
			case "backspace":
				if len(m.agentInput) > 0 {
					r := []rune(m.agentInput)
					m.agentInput = string(r[:len(r)-1])
				}
				return m, nil
			case "ctrl+v":
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
			case "esc", "ctrl+t":
				m.view = viewAgentChat
				return m, nil
			}
			return m, nil

		case viewAgentHelp:
			switch key {
			case "ctrl+c":
				appendLog("quit")
				return m, tea.Quit
			case "esc", "ctrl+h":
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
				m.view = viewMenu
			case "q", "esc", "ctrl+c":
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
				m.view = helpMenuItems[m.helpMenuCursor].dest
				m.helpScroll = 0
				appendLog("opened %s", helpMenuItems[m.helpMenuCursor].label)
			default:
				for _, it := range helpMenuItems {
					if key == it.key {
						m.view = it.dest
						m.helpScroll = 0
						appendLog("opened %s", it.label)
					}
				}
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
			case "ctrl+r":
				return m.rescanCatalog()
			case "ctrl+h":
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
		Background(lipgloss.Color("#5F5FAF")).
		Width(m.width).
		Padding(0, 1)

	if m.view == viewAgentChat {
		seg := func(fg, text string) string {
			return lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("#5F5FAF")).Foreground(lipgloss.Color(fg)).Render(text)
		}
		title := seg("#FFFFFF", "llama-shell — ") +
			seg("#FFD700", fmt.Sprintf("agentic chat: %s", m.agentModelName)) +
			seg("#00FFFF", fmt.Sprintf("   cwd: %s", m.agentWorkDir))
		return lipgloss.NewStyle().Background(lipgloss.Color("#5F5FAF")).Width(m.width).Padding(0, 1).Render(title)
	}
	return style.Render("llama-shell — Ollama TUI")
}

func (m model) renderFooter() string {
	const footerBG = "#5F5FAF"
	status := lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5F5F")).Background(lipgloss.Color(footerBG)).Render("ollama: not installed")
	if m.ollama.installed {
		status = lipgloss.NewStyle().Foreground(lipgloss.Color("#5FFF5F")).Background(lipgloss.Color(footerBG)).Render(
			fmt.Sprintf("ollama: installed (%s)", m.ollama.version),
		)
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

	left := plain.Render(fmt.Sprintf("build %s", buildTime))
	right := status

	if m.view == viewAgentChat || m.view == viewToolCategories || m.view == viewAgentHelp ||
		m.view == viewShowTable || m.view == viewListTable {
		hintText := "Ctrl+H: help"
		if m.view == viewToolCategories || m.view == viewAgentHelp {
			hintText = "Esc: back to chat  Ctrl+C: quit"
		}
		hint := lipgloss.NewStyle().Foreground(lipgloss.Color("#FFA500")).Background(lipgloss.Color(footerBG)).Render(hintText)
		totalGap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - lipgloss.Width(hint) - 4
		if totalGap < 2 {
			totalGap = 2
		}
		leftGap := totalGap / 2
		rightGap := totalGap - leftGap
		line := left + fill(leftGap+1) + hint + fill(rightGap+1) + right
		style := plain.Width(m.width).Padding(0, 1)
		return style.Render(line)
	}

	const githubURL = "https://github.com/affigabmag/llama-shell"
	linkText := lipgloss.NewStyle().Foreground(lipgloss.Color("#5FD7FF")).Background(lipgloss.Color(footerBG)).Underline(true).Render(githubURL)
	// OSC 8 terminal hyperlink escape: wraps linkText so terminals that
	// support it (Windows Terminal, iTerm2, most modern ones) make it
	// clickable. lipgloss.Width() doesn't understand OSC 8, so the gap math
	// below uses len(githubURL) — the link's actual visible width — instead.
	link := "\x1b]8;;" + githubURL + "\x1b\\" + linkText + "\x1b]8;;\x1b\\"

	totalGap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - len(githubURL) - 4
	if totalGap < 2 {
		totalGap = 2
	}
	leftGap := totalGap / 2
	rightGap := totalGap - leftGap
	line := left + fill(leftGap+1) + link + fill(rightGap+1) + right

	// No .Width() here: lipgloss doesn't understand the raw OSC 8 escape in
	// `link`, so width-based pad/truncate could miscount and truncate mid
	// escape sequence, corrupting the terminal's hyperlink state. The line
	// is already sized to m.width by the gap math above; every fragment
	// (including the filler spaces) already carries its own background, so
	// no outer wrapping style is needed to fill in the rest.
	return plain.Padding(0, 1).Render(line)
}

var (
	selectedStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#000000")).Background(lipgloss.Color("#5FD7FF"))
	unselectedStyle = lipgloss.NewStyle()
	headerRowStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#5FD7FF"))

	agentUserStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD700"))
	agentReplyStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#5FFF5F"))
	agentToolStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#808080"))
	agentHeadStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#00FFFF"))

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
func (m model) renderBanner() string {
	gapBlock := strings.Repeat("  \n", 4) + "  " // 5 blank rows, 2 cols wide

	blocks := make([]string, 0, len(bannerWord)*2)
	n := len(bannerWord)
	for i, letter := range bannerWord {
		glyph, ok := bannerGlyphs[letter]
		if !ok {
			continue
		}
		style := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
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
	case viewHelpMenu:
		return box.Render(m.renderHelpMenu())
	case viewHelpText:
		return box.Render(m.scrollHelpBody(renderHelpText()))
	case viewDisclaimerText:
		return box.Render(m.scrollHelpBody(renderDisclaimerText()))
	case viewLogText:
		return box.Render(m.scrollHelpBody(renderLogText()))
	case viewFirstRunDisclaimer:
		return box.Render(renderFirstRunDisclaimer())
	case viewAgentChat:
		// No vertical padding here (unlike the shared `box`): the agentic
		// chat view sizes its own scrollback precisely to the terminal
		// height, so an extra top/bottom pad line would just show up as an
		// unexplained gap above the footer.
		return lipgloss.NewStyle().Padding(0, 2).Render(m.renderAgentChat())
	case viewToolCategories:
		return lipgloss.NewStyle().Padding(1, 2).Render(m.renderToolCategories())
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
	b.WriteString("help / disclaimer / log\n\n")
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

func renderHelpText() string {
	return styleHelpLines(`help

llama-shell is a terminal UI shell for ollama.

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
  ctrl+r     rescan the catalog, ignoring the cache
  ctrl+h     jump straight to this help screen
  c          cancel an in-progress download
  Esc        back to main menu (or clear the search box first)
  CAPABILITIES column: com=completion too=tools ins=insert vis=vision
             emb=embedding thi=thinking aud=audio ("-" = none reported
             yet, or cache still warming up)

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
  ctrl+h     jump straight to this help screen
  Esc / q    back to main menu / quit
  CAPABILITIES column: com=completion too=tools ins=insert vis=vision
             emb=embedding thi=thinking aud=audio ("-" = none reported
             yet, or cache still warming up)

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
    with Ctrl+H from "list models" and "show model info" (Esc from here
    goes to this help menu, not back to that screen).
  - Ctrl+H means something different inside agentic chat: there it opens
    that screen's own keys reference ("Agentic chat - keys"), not this
    screen.

CAPABILITIES column (list models / show model info)
  Each ollama-reported capability is truncated to its first 3 letters:
    com = completion   too = tools       ins = insert
    vis = vision        emb = embedding   thi = thinking
    aud = audio
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
  Ctrl+V       paste - an image on the clipboard is attached directly
               (vision-capable models can then see it); otherwise pastes
               clipboard text
  Ctrl+T       browse all available tools by category
  Ctrl+H       this chat's own keys reference (see below)
  Esc          back to the show-model-info action menu
  Ctrl+C       quit llama-shell
  Typing or pasting a path to an existing image file (.png/.jpg/.jpeg/
  .gif/.bmp/.webp) in your message also attaches it, same as Ctrl+V.

Tool categories (Ctrl+T from agentic chat)
  Lists every tool the agent can call, grouped by what it does (files,
  shell/processes, networking, system/environment, git/ollama,
  open/launch).
  Esc / Ctrl+T   back to chat
  Ctrl+C         quit llama-shell

Agent help (Ctrl+H from agentic chat)
  Same keys reference as above, shown as its own screen.
  Esc            back to chat
  Ctrl+C         quit llama-shell

Esc: back
`)
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

func renderDisclaimerText() string {
	return "disclaimer\n\n" + disclaimerBody + "\n\nEsc: back\n"
}

func renderFirstRunDisclaimer() string {
	return "disclaimer — please read before continuing\n\n" + disclaimerBody +
		"\n\nYou must agree to continue.\n\n[a] I agree, continue    [q] quit\n"
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

func buildAgentChatLines(width int, messages []ollamaChatMsg, modelName string, capabilities string) []string {
	var lines []string
	toolNames := []string{
		"read_file", "write_file", "append_file", "list_dir", "make_dir", "delete_file",
		"search_files", "copy_file", "move_file", "run_command", "run_powershell", "run_python",
		"open_url", "open_path", "list_processes", "kill_process", "ssh_run", "web_search",
		"read_webpage", "http_get", "ping_host", "get_public_ip", "system_info", "list_env_vars",
		"get_env", "get_clipboard", "set_clipboard", "get_datetime", "list_network_interfaces",
		"disk_usage", "list_installed_programs", "compress_zip", "extract_zip", "git_status",
		"git_diff", "git_log", "list_window_titles", "count_lines", "file_hash", "file_info",
		"list_ollama_models", "list_running_ollama_models",
	}
	lines = append(lines, agentHeadStyle.Render(fmt.Sprintf("%d tools available — Ctrl+T to browse by category", len(toolNames))))
	lines = append(lines, renderCapabilityBadges(capabilities))
	lines = append(lines, "")
	for _, msg := range messages {
		before := len(lines)
		switch msg.Role {
		case "system":
			continue
		case "user":
			for _, l := range wrapLines("you> "+msg.Content, width) {
				lines = append(lines, agentUserStyle.Render(l))
			}
		case "tool":
			for _, l := range wrapLines("  [tool result] "+msg.Content, width) {
				lines = append(lines, agentToolStyle.Render(l))
			}
		case "assistant":
			for _, tc := range msg.ToolCalls {
				argsJSON, _ := json.Marshal(tc.Function.Arguments)
				for _, l := range wrapLines(fmt.Sprintf("  [calling %s%s]", tc.Function.Name, string(argsJSON)), width) {
					lines = append(lines, agentToolStyle.Render(l))
				}
			}
			if strings.TrimSpace(msg.Content) != "" {
				for _, l := range wrapLines(modelName+"> "+msg.Content, width) {
					lines = append(lines, agentReplyStyle.Render(l))
				}
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
	lines := buildAgentChatLines(agentViewportWidth(m.width), m.agentMessages, m.agentModelName, m.agentCapabilities)
	m.agentViewport.SetContent(strings.Join(lines, "\n"))
	m.agentViewport.GotoBottom()
}

func (m model) renderAgentChat() string {
	var b strings.Builder

	var bottom strings.Builder
	if m.agentErr != "" {
		bottom.WriteString(redStyle.Render("error: "+m.agentErr) + "\n")
	}
	if m.agentBusy {
		frame := agentSpinnerFrames[m.agentSpinner%len(agentSpinnerFrames)]
		elapsed := time.Since(m.agentStarted).Round(time.Second)
		if strings.TrimSpace(m.agentStreamBuf) == "" {
			bottom.WriteString(fmt.Sprintf("%s %s is thinking... (%s)\n", frame, m.agentModelName, elapsed))
		} else {
			// Cap the live preview: an unbounded growing reply (the model
			// can stream thousands of characters before it's done) must
			// never be allowed to make this block taller than its
			// reserved budget (agentBottomReserve), or the layout
			// overflows and the header scrolls off the top.
			const maxPreviewLines = 6
			streamLines := wrapLines(m.agentModelName+"> "+m.agentStreamBuf+"▌", agentViewportWidth(m.width))
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
				bottom.WriteString(agentReplyStyle.Render(l) + "\n")
			}
			bottom.WriteString(fmt.Sprintf("%s (%s)\n", frame, elapsed))
		}
	} else {
		bottom.WriteString(agentUserStyle.Render("you> "+m.agentInput+"█") + "\n")
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
// reminder, this is the "let me see everything" view (Ctrl+T from chat).
func (m model) renderToolCategories() string {
	var b strings.Builder
	b.WriteString(agentHeadStyle.Render(fmt.Sprintf("Tools by category (%d total)", func() int {
		n := 0
		for _, c := range agentToolCategories {
			n += len(c.tools)
		}
		return n
	}())) + "\n\n")
	for _, cat := range agentToolCategories {
		b.WriteString(headerRowStyle.Render(cat.name) + "\n")
		for _, l := range toolGridLines(cat.tools, 2, m.width-4) {
			b.WriteString("  " + l + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("Esc or Ctrl+T: back to chat\n")
	return b.String()
}

// renderAgentHelp is the Ctrl+H dialog for the agentic chat — the keybind
// list used to live in the footer hint, but wrapped to two lines on
// narrower terminals, so it moved here behind a short "Ctrl+H: help" hint.
func renderAgentHelp() string {
	var b strings.Builder
	b.WriteString(agentHeadStyle.Render("Agentic chat — keys") + "\n\n")
	b.WriteString("  Enter        send your message\n")
	b.WriteString("  Up / Down    scroll history one line\n")
	b.WriteString("  PgUp / PgDn  scroll history one page\n")
	b.WriteString("  Home / End   jump to top / bottom of history\n")
	b.WriteString("  Ctrl+V       paste - an image on the clipboard attaches directly for\n")
	b.WriteString("               vision-capable models, otherwise pastes clipboard text\n")
	b.WriteString("  Ctrl+T       browse all tools by category\n")
	b.WriteString("  Ctrl+H       this help screen\n")
	b.WriteString("  Esc          back to model actions menu\n")
	b.WriteString("  Ctrl+C       quit llama-shell\n\n")
	b.WriteString("A path to an existing image file typed/pasted into your message also\n")
	b.WriteString("attaches it, same as Ctrl+V.\n\n")
	b.WriteString("Looking for the CAPABILITIES column codes (com/too/ins/vis/emb/thi/aud)?\n")
	b.WriteString("Those are on \"list models\" / \"show model info\" screens, not here — back\n")
	b.WriteString("out to one of those and press Ctrl+H there instead.\n\n")
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
		"web_search", "read_webpage", "http_get", "ping_host", "get_public_ip", "ssh_run",
		"list_network_interfaces",
	}},
	{"System & Environment", []string{
		"system_info", "list_env_vars", "get_env", "get_clipboard", "set_clipboard",
		"get_datetime", "disk_usage", "list_installed_programs",
	}},
	{"Git & Ollama", []string{
		"git_status", "git_diff", "git_log", "list_ollama_models", "list_running_ollama_models",
	}},
	{"Open / Launch", []string{"open_url", "open_path"}},
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
	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}
