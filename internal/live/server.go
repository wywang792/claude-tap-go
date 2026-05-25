package live

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wywang792/claude-tap-go/internal/history"
)

const viewerScriptAnchor = "<script>\nconst $ = s =>"

//go:embed viewer.html
var embeddedViewerHTML string

//go:embed viewer_i18n.json
var embeddedViewerI18N string

// Server provides SSE-based real-time trace viewing.
type Server struct {
	TracePath string
	Port      int
	Host      string
	OutputDir string

	actualPort  int
	listener    net.Listener
	sseClients  map[chan []byte]struct{}
	records     []map[string]interface{}
	currentDate string
	mu          sync.Mutex
	shutdown    chan struct{}
	srv         *http.Server
}

// NewServer creates a new live viewer server.
func NewServer(tracePath string, port int, host, outputDir string) *Server {
	return &Server{
		TracePath:   tracePath,
		Port:        port,
		Host:        host,
		OutputDir:   outputDir,
		sseClients:  make(map[chan []byte]struct{}),
		currentDate: time.Now().Format("2006-01-02"),
		shutdown:    make(chan struct{}),
	}
}

// Start starts the live viewer server. Returns the actual port.
func (s *Server) Start() (int, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.Host, s.Port))
	if err != nil {
		return 0, err
	}
	s.listener = listener
	s.actualPort = listener.Addr().(*net.TCPAddr).Port

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/viewer", s.handleIndex)
	mux.HandleFunc("/events", s.handleSSE)
	mux.HandleFunc("/records", s.handleRecords)
	mux.HandleFunc("/api/dates", s.handleDates)
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/agents", s.handleAgents)

	// Sub-path handlers
	mux.HandleFunc("/api/traces/", s.handleTracesByDate)
	mux.HandleFunc("/api/sessions/", s.handleSessionDetail)

	// Dashboard
	mux.HandleFunc("/dashboard", s.handleDashboard)

	s.srv = &http.Server{Handler: mux}
	go s.srv.Serve(listener)

	return s.actualPort, nil
}

// Stop stops the live viewer server.
func (s *Server) Stop() {
	close(s.shutdown)

	s.mu.Lock()
	for ch := range s.sseClients {
		close(ch)
	}
	s.sseClients = make(map[chan []byte]struct{})
	s.mu.Unlock()

	if s.srv != nil {
		s.srv.Close()
	}
}

// Broadcast sends a new record to all connected SSE clients.
func (s *Server) Broadcast(record map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	if today != s.currentDate {
		s.records = nil
		s.currentDate = today
	}
	s.records = append(s.records, record)

	data, _ := json.Marshal(record)
	message := fmt.Sprintf("data: %s\n\n", string(data))

	for ch := range s.sseClients {
		select {
		case ch <- []byte(message):
		default:
			// Client too slow, skip
		}
	}
}

// URL returns the viewer URL.
func (s *Server) URL() string {
	return fmt.Sprintf("http://%s:%d", s.Host, s.actualPort)
}

// Port returns the actual port the server is listening on.
func (s *Server) ActualPort() int {
	return s.actualPort
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Serve the embedded viewer HTML with live mode enabled
	html := getViewerHTML(s.TracePath, true)

	w.Write([]byte(html))
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(getDashboardHTML()))
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	ch := make(chan []byte, 64)

	s.mu.Lock()
	// Send existing records
	for _, record := range s.records {
		data, _ := json.Marshal(record)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		flusher.Flush()
	}
	s.sseClients[ch] = struct{}{}
	s.mu.Unlock()

	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	defer func() {
		s.mu.Lock()
		delete(s.sseClients, ch)
		s.mu.Unlock()
	}()

	for {
		select {
		case <-s.shutdown:
			return
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			w.Write(msg)
			flusher.Flush()
		case <-time.After(30 * time.Second):
			// Keepalive
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) handleRecords(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	s.mu.Lock()
	defer s.mu.Unlock()

	json.NewEncoder(w).Encode(s.records)
}

func (s *Server) handleDates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if s.OutputDir == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"dates": []string{}, "has_legacy": false})
		return
	}

	datesSet := make(map[string]bool)
	hasLegacy := false

	entries, _ := os.ReadDir(s.OutputDir)
	for _, entry := range entries {
		if entry.IsDir() && regexpDate(entry.Name()) {
			pattern := filepath.Join(s.OutputDir, entry.Name(), "trace_*.jsonl")
			if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
				datesSet[entry.Name()] = true
			}
		}
		if entry.Name() == "" {
			// legacy
		}
	}

	// Check for legacy traces
	legacyPattern := filepath.Join(s.OutputDir, "trace_*.jsonl")
	if matches, _ := filepath.Glob(legacyPattern); len(matches) > 0 {
		hasLegacy = true
	}

	// Always include today
	datesSet[time.Now().Format("2006-01-02")] = true

	var dates []string
	for d := range datesSet {
		dates = append(dates, d)
	}
	// Sort descending
	sortDatesDesc(dates)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"dates":      dates,
		"has_legacy": hasLegacy,
	})
}

func (s *Server) handleTracesByDate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	dateKey := strings.TrimPrefix(r.URL.Path, "/api/traces/")
	if r.Method == http.MethodDelete {
		result := history.DeleteTraceHistory(s.OutputDir, dateKey, []string{s.TracePath})
		json.NewEncoder(w).Encode(result)
		return
	}

	if s.OutputDir == "" {
		json.NewEncoder(w).Encode([]interface{}{})
		return
	}

	var traceDir string
	if dateKey == "legacy" {
		traceDir = s.OutputDir
	} else if regexpDate(dateKey) {
		traceDir = filepath.Join(s.OutputDir, dateKey)
	} else {
		http.Error(w, "Invalid date format", http.StatusBadRequest)
		return
	}

	pattern := filepath.Join(traceDir, "trace_*.jsonl")
	matches, _ := filepath.Glob(pattern)

	var records []interface{}
	for _, jsonl := range matches {
		data, err := os.ReadFile(jsonl)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var record map[string]interface{}
			if json.Unmarshal([]byte(line), &record) == nil {
				records = append(records, record)
			}
		}
	}

	json.NewEncoder(w).Encode(records)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if s.OutputDir == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"sessions": []interface{}{}})
		return
	}

	sessions := listTraceSessions(s.OutputDir, s.TracePath)
	json.NewEncoder(w).Encode(map[string]interface{}{"sessions": sessions})
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if s.OutputDir == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"agents": []interface{}{}})
		return
	}

	sessions := listTraceSessions(s.OutputDir, s.TracePath)
	agents := groupByAgents(sessions)
	json.NewEncoder(w).Encode(map[string]interface{}{"agents": agents})
}

func (s *Server) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if s.OutputDir == "" {
		http.Error(w, "No output directory configured", http.StatusNotFound)
		return
	}

	// Path: /api/sessions/{session_id}/records or /api/sessions/{session_id}/html
	path := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 {
		http.Error(w, "Invalid session path", http.StatusBadRequest)
		return
	}
	sessionID := parts[0]
	_ = parts[1] // "records" or "html"

	session, err := loadTraceSession(s.OutputDir, sessionID, s.TracePath)
	if err != nil || session == nil {
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(session)
}

// --- dashboard helpers (simplified) ---

func listTraceSessions(outputDir, currentPath string) []map[string]interface{} {
	matches := findTraceFilesRecursive(outputDir)

	var sessions []map[string]interface{}
	for _, jsonl := range matches {
		rel := history.RelPosix(jsonl, outputDir)
		records := readJSONLRecords(jsonl)

		summary := summarizeSession(jsonl, rel, records, currentPath)
		sessions = append(sessions, summary)
	}

	// Sort by updated_at descending
	sortSessionsDesc(sessions)
	return sessions
}

func findTraceFilesRecursive(root string) []string {
	var matches []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if ok, _ := filepath.Match("trace_*.jsonl", d.Name()); ok {
			matches = append(matches, path)
		}
		return nil
	})
	sort.Strings(matches)
	return matches
}

func groupByAgents(sessions []map[string]interface{}) []map[string]interface{} {
	buckets := make(map[string]map[string]interface{})
	for _, s := range sessions {
		key, _ := s["agent_key"].(string)
		bucket, ok := buckets[key]
		if !ok {
			bucket = map[string]interface{}{
				"key":      key,
				"label":    s["agent"],
				"sessions": 0,
				"records":  0,
			}
			buckets[key] = bucket
		}
		bucket["sessions"] = bucket["sessions"].(int) + 1
		if rc, ok := toInt(s["record_count"]); ok {
			bucket["records"] = bucket["records"].(int) + rc
		}
	}

	var result []map[string]interface{}
	for _, b := range buckets {
		result = append(result, b)
	}
	return result
}

func readJSONLRecords(path string) []map[string]interface{} {
	var records []map[string]interface{}
	data, err := os.ReadFile(path)
	if err != nil {
		return records
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record map[string]interface{}
		if json.Unmarshal([]byte(line), &record) == nil {
			records = append(records, record)
		}
	}
	return records
}

func summarizeSession(jsonlPath, relPath string, records []map[string]interface{}, currentPath string) map[string]interface{} {
	stat, _ := os.Stat(jsonlPath)
	size := int64(0)
	updatedAt := ""
	if stat != nil {
		size = stat.Size()
		updatedAt = stat.ModTime().Format(time.RFC3339)
	}

	htmlPath := jsonlPath[:len(jsonlPath)-len(".jsonl")] + ".html"
	logPath := jsonlPath[:len(jsonlPath)-len(".jsonl")] + ".log"

	var startedAt string
	if len(records) > 0 {
		if ts, ok := records[0]["timestamp"].(string); ok {
			startedAt = ts
		}
	}
	if len(records) > 0 {
		if ts, ok := records[len(records)-1]["timestamp"].(string); ok {
			updatedAt = ts
		}
	}
	if startedAt == "" {
		startedAt = updatedAt
	}

	agent := inferAgent(records)
	agentKey := strings.ToLower(strings.ReplaceAll(agent, " ", "-"))

	var inputTokens, outputTokens, cacheReadTokens, cacheCreateTokens int64
	models := make(map[string]int)
	var durationMs int64
	turns := make(map[int]bool)

	for _, record := range records {
		u := extractUsage(record)
		inputTokens += int64(toIntDefault(u["input_tokens"], 0))
		outputTokens += int64(toIntDefault(u["output_tokens"], 0))
		cacheReadTokens += int64(toIntDefault(u["cache_read_input_tokens"], 0))
		cacheCreateTokens += int64(toIntDefault(u["cache_creation_input_tokens"], 0))

		if m := extractModel(record); m != "" {
			models[m]++
		}
		if d, ok := toInt(record["duration_ms"]); ok {
			durationMs += int64(d)
		}
		if t, ok := toInt(record["turn"]); ok {
			turns[t] = true
		}
	}

	totalTokens := inputTokens + outputTokens + cacheReadTokens + cacheCreateTokens
	topModel := ""
	maxC := 0
	for m, c := range models {
		if c > maxC {
			maxC = c
			topModel = m
		}
	}

	isCurrent := false
	currentResolved, _ := filepath.Abs(currentPath)
	jsonlResolved, _ := filepath.Abs(jsonlPath)
	if currentResolved == jsonlResolved {
		isCurrent = true
	}

	status := "complete"
	if isCurrent {
		status = "active"
	}
	if len(records) == 0 {
		status = "empty"
	}

	return map[string]interface{}{
		"id":                  sessionIDForRelPath(relPath),
		"date":                filepath.Base(filepath.Dir(jsonlPath)),
		"agent":               agent,
		"agent_key":           agentKey,
		"status":              status,
		"live":                isCurrent,
		"trace_path":          jsonlPath,
		"rel_trace_path":      relPath,
		"html_path":           htmlPath,
		"log_path":            logPath,
		"started_at":          startedAt,
		"updated_at":          updatedAt,
		"record_count":        len(records),
		"turn_count":          len(turns),
		"duration_ms":         durationMs,
		"input_tokens":        inputTokens,
		"output_tokens":       outputTokens,
		"cache_read_tokens":   cacheReadTokens,
		"cache_create_tokens": cacheCreateTokens,
		"total_tokens":        totalTokens,
		"model":               topModel,
		"size_bytes":          size,
	}
}

func loadTraceSession(outputDir, sessionID, currentPath string) (map[string]interface{}, error) {
	relPath := relPathForSessionID(sessionID)
	if relPath == "" {
		return nil, fmt.Errorf("invalid session id")
	}

	jsonlPath, err := safeTracePath(outputDir, relPath)
	if err != nil {
		return nil, err
	}
	records := readJSONLRecords(jsonlPath)
	summary := summarizeSession(jsonlPath, relPath, records, currentPath)

	return map[string]interface{}{
		"session": summary,
		"records": records,
	}, nil
}

func safeTracePath(outputDir, relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("empty session path")
	}
	nativeRel := filepath.FromSlash(relPath)
	cleanRel := filepath.Clean(nativeRel)
	if filepath.IsAbs(cleanRel) || cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid session path")
	}

	baseAbs, err := filepath.Abs(outputDir)
	if err != nil {
		return "", err
	}
	fullAbs, err := filepath.Abs(filepath.Join(outputDir, cleanRel))
	if err != nil {
		return "", err
	}
	if !samePath(fullAbs, baseAbs) && !strings.HasPrefix(strings.ToLower(fullAbs), strings.ToLower(baseAbs+string(os.PathSeparator))) {
		return "", fmt.Errorf("session path escapes output directory")
	}
	return fullAbs, nil
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func inferAgent(records []map[string]interface{}) string {
	if len(records) == 0 {
		return "Unknown"
	}
	for _, record := range records {
		if capture, ok := record["capture"].(map[string]interface{}); ok {
			if client, ok := capture["client"].(string); ok {
				return clientLabel(client)
			}
		}
	}

	// Infer from upstream URL
	for _, record := range records {
		if upstream, ok := record["upstream_base_url"].(string); ok {
			switch {
			case strings.Contains(upstream, "api.anthropic.com"):
				return "Claude Code"
			case strings.Contains(upstream, "api.openai.com"):
				return "Codex"
			case strings.Contains(upstream, "api.moonshot.cn"):
				return "Kimi"
			case strings.Contains(upstream, "generativelanguage.googleapis.com"):
				return "Gemini"
			}
		}
	}
	return "Unknown"
}

func clientLabel(client string) string {
	labels := map[string]string{
		"claude":   "Claude Code",
		"codex":    "Codex",
		"kimi":     "Kimi",
		"gemini":   "Gemini",
		"opencode": "OpenCode",
		"hermes":   "Hermes",
		"cursor":   "Cursor",
		"pi":       "Pi",
		"qoder":    "Qoder",
		"agy":      "Antigravity",
	}
	if l, ok := labels[strings.ToLower(client)]; ok {
		return l
	}
	return client
}

func extractUsage(record map[string]interface{}) map[string]interface{} {
	resp, _ := record["response"].(map[string]interface{})
	body, _ := resp["body"].(map[string]interface{})
	if usage, ok := body["usage"].(map[string]interface{}); ok {
		return usage
	}
	if body != nil {
		return body
	}
	return map[string]interface{}{}
}

func extractModel(record map[string]interface{}) string {
	req, _ := record["request"].(map[string]interface{})
	body, _ := req["body"].(map[string]interface{})
	if model, ok := body["model"].(string); ok {
		return model
	}
	return ""
}

func sessionIDForRelPath(relPath string) string {
	data := []byte(relPath)
	result := make([]byte, 0, len(data)*2)
	for _, b := range data {
		result = append(result, hexChar(b>>4), hexChar(b&0x0f))
	}
	return string(result)
}

func hexChar(b byte) byte {
	b &= 0x0f
	if b < 10 {
		return '0' + b
	}
	return 'a' + (b - 10)
}

func relPathForSessionID(sessionID string) string {
	if len(sessionID)%2 != 0 {
		return ""
	}
	decoded := make([]byte, len(sessionID)/2)
	for i := 0; i < len(sessionID); i += 2 {
		hi := unhexChar(sessionID[i])
		lo := unhexChar(sessionID[i+1])
		if hi < 0 || lo < 0 {
			return ""
		}
		decoded[i/2] = byte(hi<<4) | byte(lo)
	}
	return string(decoded)
}

func unhexChar(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c - 'a' + 10)
	case c >= 'A' && c <= 'F':
		return int(c - 'A' + 10)
	}
	return -1
}

func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

func toIntDefault(v interface{}, defaultVal int) int {
	if n, ok := toInt(v); ok {
		return n
	}
	return defaultVal
}

func regexpDate(s string) bool {
	if len(s) != 10 {
		return false
	}
	// Simple YYYY-MM-DD check
	for i, c := range s {
		if i == 4 || i == 7 {
			if c != '-' {
				return false
			}
		} else if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func sortDatesDesc(dates []string) {
	for i := 0; i < len(dates); i++ {
		for j := i + 1; j < len(dates); j++ {
			if dates[i] < dates[j] {
				dates[i], dates[j] = dates[j], dates[i]
			}
		}
	}
}

func sortSessionsDesc(sessions []map[string]interface{}) {
	for i := 0; i < len(sessions); i++ {
		for j := i + 1; j < len(sessions); j++ {
			a, _ := sessions[i]["updated_at"].(string)
			b, _ := sessions[j]["updated_at"].(string)
			if a < b {
				sessions[i], sessions[j] = sessions[j], sessions[i]
			}
		}
	}
}

// getViewerHTML returns the embedded viewer HTML with live mode injection.
func getViewerHTML(tracePath string, liveMode bool) string {
	if liveMode {
		jsonlPath, _ := filepath.Abs(tracePath)
		htmlPath := tracePath[:len(tracePath)-len(".jsonl")] + ".html"
		htmlPathAbs, _ := filepath.Abs(htmlPath)

		jsonlPathJS, _ := json.Marshal(jsonlPath)
		htmlPathJS, _ := json.Marshal(htmlPathAbs)
		liveJS := fmt.Sprintf(`const LIVE_MODE = true;
const EMBEDDED_TRACE_DATA = [];
const __TRACE_JSONL_PATH__ = %s;
const __TRACE_HTML_PATH__ = %s;
const __CLAUDE_TAP_VERSION__ = "go";`, jsonlPathJS, htmlPathJS)

		return injectViewerScript(liveJS)
	}
	return injectViewerScript(`const EMBEDDED_TRACE_DATA = [];
const __CLAUDE_TAP_VERSION__ = "go";`)
}

// GenerateViewerHTMLContent returns a static self-contained viewer using the full template.
func GenerateViewerHTMLContent(records []map[string]interface{}, tracePath, htmlPath string) string {
	recordsJSON, _ := json.Marshal(records)
	recordsJS := strings.ReplaceAll(string(recordsJSON), "</", "<\\/")
	tracePathAbs, _ := filepath.Abs(tracePath)
	htmlPathAbs, _ := filepath.Abs(htmlPath)
	tracePathJS, _ := json.Marshal(tracePathAbs)
	htmlPathJS, _ := json.Marshal(htmlPathAbs)

	dataJS := fmt.Sprintf(`const LIVE_MODE = false;
const EMBEDDED_TRACE_DATA = %s;
const __TRACE_JSONL_PATH__ = %s;
const __TRACE_HTML_PATH__ = %s;
const __CLAUDE_TAP_VERSION__ = "go";`, recordsJS, tracePathJS, htmlPathJS)

	return injectViewerScript(dataJS)
}

func injectViewerScript(dataJS string) string {
	template := embeddedViewerHTML
	i18nJS := fmt.Sprintf("<script>\nconst __CLAUDE_TAP_I18N__ = %s;\n</script>\n", embeddedViewerI18N)
	dataScript := fmt.Sprintf("<script>\n%s\n</script>\n", dataJS)
	injected := i18nJS + dataScript + viewerScriptAnchor
	if strings.Contains(template, viewerScriptAnchor) {
		return strings.Replace(template, viewerScriptAnchor, injected, 1)
	}
	return strings.Replace(template, "</head>", i18nJS+dataScript+"</head>", 1)
}

func getDashboardHTML() string {
	return dashboardHTML
}

// Embedded HTML templates 鈥?basic but functional
var viewerHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>claude-tap Trace Viewer</title>
<style>
* { box-sizing: border-box; margin: 0; padding: 0; }
body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; background: #1e1e2e; color: #cdd6f4; display: flex; height: 100vh; }
.sidebar { width: 320px; background: #181825; overflow-y: auto; padding: 16px; border-right: 1px solid #313244; }
.sidebar h2 { font-size: 14px; margin-bottom: 12px; color: #89b4fa; }
.turn-item { padding: 8px 12px; margin-bottom: 4px; cursor: pointer; border-radius: 6px; font-size: 13px; }
.turn-item:hover { background: #313244; }
.turn-item.selected { background: #45475a; }
.turn-id { font-weight: 600; }
.turn-model { color: #a6adc8; font-size: 11px; }
.turn-tokens { color: #f9e2af; font-size: 11px; }
.turn-error { color: #f38ba8; font-size: 11px; }
.main { flex: 1; overflow-y: auto; padding: 24px; }
.request, .response { margin-bottom: 24px; }
.request h3, .response h3 { font-size: 14px; margin-bottom: 8px; color: #89b4fa; }
pre { background: #11111b; border-radius: 8px; padding: 16px; overflow-x: auto; font-size: 13px; line-height: 1.5; }
.empty-state { display: flex; align-items: center; justify-content: center; height: 100%; color: #585b70; font-size: 18px; }
.summary-bar { position: fixed; bottom: 0; left: 0; right: 0; background: #181825; padding: 8px 24px; font-size: 12px; display: flex; gap: 24px; border-top: 1px solid #313244; }
.summary-bar span { color: #a6adc8; }
.summary-bar strong { color: #cdd6f4; }
.response-content { white-space: pre-wrap; word-break: break-word; }
.response-content .thinking { color: #6c7086; font-style: italic; }
.response-content .tool-use { background: #1e1e2e; border: 1px solid #313244; border-radius: 6px; padding: 8px; margin: 8px 0; }
.response-content .tool-use .name { color: #cba6f7; font-weight: 600; }
</style>
</head>
<body>
<div class="sidebar" id="sidebar"><h2>API Calls</h2><div id="turn-list"></div></div>
<div class="main" id="main-view"><div class="empty-state">Live trace: waiting for API calls...</div></div>
<div class="summary-bar" id="summary-bar"></div>
<script>
const $ = s => document.querySelector(s);
const $$ = s => document.querySelectorAll(s);
let records = EMBEDDED_TRACE_DATA || [];
let selectedIdx = -1;
const turnList = $('#turn-list');
const mainView = $('#main-view');
const summaryBar = $('#summary-bar');
function init() {
  renderTurnList();
  updateSummary();
  if (typeof LIVE_MODE !== 'undefined' && LIVE_MODE) {
    const evtSource = new EventSource('/events');
    evtSource.onmessage = e => {
      const r = JSON.parse(e.data);
      records.push(r);
      selectTurn(records.length - 1);
      updateSummary();
    };
  }
}
function renderTurnList() {
  turnList.innerHTML = records.map((r, i) => {
    const req = r.request || {}, resp = r.response || {}, body = resp.body || {};
    const usage = body.usage || {};
    const model = (req.body && req.body.model) || '';
    const status = resp.status || 0;
    const cls = i === selectedIdx ? 'selected' : '';
    const err = status >= 400 ? ' <span class="turn-error">ERR</span>' : '';
    return '<div class="turn-item '+cls+'" onclick="selectTurn('+i+')"><span class="turn-id">#'+ (r.turn||0)+'</span> <span class="turn-model">'+escHtml(String(model))+'</span> <span class="turn-tokens">in:'+(usage.input_tokens||0)+' out:'+(usage.output_tokens||0)+'</span>'+err+'</div>';
  }).join('');
}
function selectTurn(i) { selectedIdx = i; renderTurnList(); renderDetail(records[i]); }
function renderDetail(r) {
  if (!r) { mainView.innerHTML = '<div class="empty-state">Select an API call</div>'; return; }
  const req = r.request || {}, resp = r.response || {};
  const reqBody = formatJSON(req.body);
  const respBody = formatJSON(resp.body);
  mainView.innerHTML = '<div class="request"><h3>Request ('+escHtml(String(req.method || ''))+' '+escHtml(String(req.path || ''))+')</h3><pre>'+escHtml(reqBody)+'</pre></div><div class="response"><h3>Response ('+resp.status+' - '+(r.duration_ms||0)+'ms)</h3><pre>'+escHtml(respBody)+'</pre></div>';
}
function formatJSON(obj) { try { return JSON.stringify(obj, null, 2); } catch(e) { return String(obj); } }
function escHtml(s) { return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }
function updateSummary() {
  let calls = records.length, inTok = 0, outTok = 0, cacheRead = 0;
  records.forEach(r => { const u = ((r.response||{}).body||{}).usage||{}; inTok += u.input_tokens||0; outTok += u.output_tokens||0; cacheRead += u.cache_read_input_tokens||0; });
  summaryBar.innerHTML = '<span>API Calls: <strong>'+calls+'</strong></span><span>Input Tokens: <strong>'+inTok+'</strong></span><span>Output Tokens: <strong>'+outTok+'</strong></span><span>Cache Read: <strong>'+cacheRead+'</strong></span>';
}
init();
</script>
</body>
</html>`

var dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>claude-tap Dashboard</title>
<style>
* { box-sizing: border-box; margin: 0; padding: 0; }
body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; background: #1e1e2e; color: #cdd6f4; padding: 24px; }
h1 { font-size: 24px; margin-bottom: 16px; color: #89b4fa; }
.session-list { display: grid; gap: 8px; }
.session-item { background: #181825; padding: 16px; border-radius: 8px; cursor: pointer; border: 1px solid #313244; }
.session-item:hover { background: #313244; }
.session-item .agent { font-weight: 600; color: #89b4fa; }
.session-item .meta { font-size: 12px; color: #a6adc8; margin-top: 4px; }
.session-item .tokens { font-size: 11px; color: #f9e2af; margin-top: 4px; }
.empty-state { color: #585b70; font-size: 18px; text-align: center; padding: 48px; }
</style>
</head>
<body>
<h1>claude-tap Sessions</h1>
<div class="session-list" id="session-list"><div class="empty-state">Loading sessions...</div></div>
<script>
async function load() {
  try {
    const resp = await fetch('/api/sessions');
    const data = await resp.json();
    const sessions = data.sessions || [];
    const list = document.getElementById('session-list');
    if (sessions.length === 0) {
      list.innerHTML = '<div class="empty-state">No sessions found</div>';
      return;
    }
    list.innerHTML = sessions.map(s => {
      return '<div class="session-item" onclick="location.href=\'/viewer\'"><div class="agent">'+escHtml(s.agent)+'</div><div class="meta">'+s.date+' - '+s.record_count+' records - '+s.model+'</div><div class="tokens">in:'+(s.input_tokens||0)+' out:'+(s.output_tokens||0)+' cache:'+(s.cache_read_tokens||0)+'</div></div>';
    }).join('');
  } catch(e) { document.getElementById('session-list').innerHTML = '<div class="empty-state">Connection error</div>'; }
}
function escHtml(s) { return (s||'').replace(/&/g,'&amp;').replace(/</g,'&lt;'); }
load();
</script>
</body>
</html>`
