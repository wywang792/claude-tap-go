package proxy

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/md5"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wywang792/claude-tap-go/internal/trace"
)

// Hop-by-hop headers that should not be forwarded.
var hopByHop = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailers":            true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// Sensitive header keys for redaction.
var sensitiveHeaders = map[string]bool{
	"authorization":      true,
	"cookie":             true,
	"set-cookie":         true,
	"set-cookie2":        true,
	"x-api-key":          true,
	"cosy-key":           true,
	"cosy-machinetoken":  true,
	"cosy-machine-token": true,
	"cosy-machineid":     true,
	"cosy-machine-id":    true,
	"cosy-machinetype":   true,
	"cosy-machine-type":  true,
	"cosy-user":          true,
}

var anthropicMetadataUserIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

var reverseHTTPClient = &http.Client{Timeout: 600 * time.Second}

// Allowed API path prefixes.
var allowedPathPrefixes = []string{
	"/v1/messages",
	"/v1/complete",
	"/v1/responses",
	"/v1/chat/completions",
	"/v1/completions",
	"/v1/models",
	"/v1/embeddings",
	"/v1/files",
	"/responses",
	"/chat/completions",
	"/completions",
	"/models",
	"/embeddings",
	"/files",
	"/v1beta/models",
	"/v1alpha/models",
	"/v1internal",
	"/search",
	"/fetch",
	"/usages",
	"/feedback",
}

// FilterHeaders removes hop-by-hop headers and optionally redacts sensitive values.
func FilterHeaders(headers http.Header, redactKeys bool) map[string]string {
	out := make(map[string]string)
	for k, v := range headers {
		key := strings.ToLower(k)
		if hopByHop[key] {
			continue
		}
		val := strings.Join(v, ", ")
		if redactKeys && sensitiveHeaders[key] {
			if (key == "authorization" || key == "x-api-key") && len(val) > 12 {
				out[k] = val[:12] + "..."
			} else {
				out[k] = "***"
			}
		} else {
			out[k] = val
		}
	}
	return out
}

func isAllowedPath(path string, extraPrefixes []string) bool {
	clean := strings.SplitN(path, "?", 2)[0]
	clean = strings.TrimRight(clean, "/")
	allPrefixes := append(allowedPathPrefixes, extraPrefixes...)
	for _, prefix := range allPrefixes {
		if clean == prefix || strings.HasPrefix(clean, prefix+"/") || strings.HasPrefix(clean, prefix+":") {
			return true
		}
	}
	return false
}

func normalizeRequestBodyForUpstream(reqBody map[string]interface{}, target string) (map[string]interface{}, bool) {
	if !isDeepSeekAnthropicTarget(target) {
		return reqBody, false
	}

	metadata, ok := reqBody["metadata"].(map[string]interface{})
	if !ok {
		return reqBody, false
	}

	userID, ok := metadata["user_id"].(string)
	if !ok || anthropicMetadataUserIDPattern.MatchString(userID) {
		return reqBody, false
	}

	normalizedBody := deepCopyMap(reqBody)
	normalizedMetadata, _ := normalizedBody["metadata"].(map[string]interface{})
	if normalizedMetadata == nil {
		normalizedMetadata = make(map[string]interface{})
		normalizedBody["metadata"] = normalizedMetadata
	}
	digest := sha256.Sum256([]byte(userID))
	normalizedMetadata["user_id"] = fmt.Sprintf("claude_tap_%x", digest)[:35]
	return normalizedBody, true
}

func isDeepSeekAnthropicTarget(target string) bool {
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	return u.Host == "api.deepseek.com" && strings.TrimRight(u.Path, "/") == "/anthropic"
}

func deleteHeaderCaseInsensitive(headers map[string]string, name string) {
	for key := range headers {
		if strings.EqualFold(key, name) {
			delete(headers, key)
		}
	}
}

// ReverseProxyContext holds the configuration for the reverse proxy.
type ReverseProxyContext struct {
	TargetURL         string
	Writer            *trace.Writer
	ExtraPathPrefixes []string
	StripPathPrefix   string
	ForceHTTP         bool
	TurnCounter       int
	mu                sync.Mutex
}

func (ctx *ReverseProxyContext) nextTurn() int {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	ctx.TurnCounter++
	return ctx.TurnCounter
}

// ReverseProxyHandler returns an http.Handler that acts as a reverse proxy.
func ReverseProxyHandler(ctx *ReverseProxyContext) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path allowlist check
		if !isAllowedPath(r.URL.Path, ctx.ExtraPathPrefixes) {
			log.Printf("Blocked non-API path: %s %s", r.Method, r.URL.Path)
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}

		turn := ctx.nextTurn()

		// WebSocket upgrade detection
		if strings.ToLower(r.Header.Get("Upgrade")) == "websocket" {
			if ctx.ForceHTTP {
				log.Printf("Rejecting WebSocket upgrade on %s (force_http)", r.URL.Path)
				w.WriteHeader(http.StatusUpgradeRequired)
				return
			}
			handleWebSocket(w, r, ctx, turn)
			return
		}

		handleReverseHTTP(w, r, ctx, turn)
	})
}

func handleReverseHTTP(w http.ResponseWriter, r *http.Request, ctx *ReverseProxyContext, turn int) {
	start := time.Now()
	reqID := fmt.Sprintf("req_%x", md5.Sum([]byte(time.Now().String())))[:15]

	// Strip path prefix
	fwdPath := r.URL.RequestURI()
	if ctx.StripPathPrefix != "" && strings.HasPrefix(fwdPath, ctx.StripPathPrefix) {
		fwdPath = fwdPath[len(ctx.StripPathPrefix):]
		if fwdPath == "" {
			fwdPath = "/"
		}
	}

	target := strings.TrimRight(ctx.TargetURL, "/")
	upstreamURL := target + "/" + strings.TrimLeft(fwdPath, "/")

	// Read request body
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	var reqBody interface{}
	if len(bodyBytes) > 0 {
		if json.Valid(bodyBytes) {
			json.Unmarshal(bodyBytes, &reqBody)
		} else {
			reqBody = string(bodyBytes)
		}
	}

	reqBodyMap, _ := reqBody.(map[string]interface{})
	upstreamBodyBytes := bodyBytes
	if reqBodyMap != nil {
		if normalizedBody, changed := normalizeRequestBodyForUpstream(reqBodyMap, ctx.TargetURL); changed {
			reqBody = normalizedBody
			reqBodyMap = normalizedBody
			upstreamBodyBytes, _ = json.Marshal(normalizedBody)
		}
	}

	isStreaming := false
	if reqBodyMap != nil {
		if stream, ok := reqBodyMap["stream"].(bool); ok {
			isStreaming = stream
		}
	}

	model := ""
	if reqBodyMap != nil {
		if m, ok := reqBodyMap["model"].(string); ok {
			model = m
		}
	}

	// Build upstream request
	reqHeaders := FilterHeaders(r.Header, false)
	delete(reqHeaders, "Host")
	reqHeaders["Accept-Encoding"] = "identity"
	if !bytes.Equal(upstreamBodyBytes, bodyBytes) {
		deleteHeaderCaseInsensitive(reqHeaders, "Content-Length")
	}

	// Debug: log auth-related headers
	hasAuth := reqHeaders["Authorization"] != "" || reqHeaders["X-Api-Key"] != "" || reqHeaders["x-api-key"] != ""
	log.Printf("[Turn %d] -> %s %s (model=%s, stream=%v, upstream=%s, hasAuth=%v)", turn, r.Method, r.URL.Path, model, isStreaming, upstreamURL, hasAuth)

	upstreamReq, err := http.NewRequest(r.Method, upstreamURL, bytes.NewReader(upstreamBodyBytes))
	if err != nil {
		http.Error(w, "Upstream error", http.StatusBadGateway)
		return
	}
	for k, v := range reqHeaders {
		upstreamReq.Header.Set(k, v)
	}
	// Explicitly set Host to upstream host so Go transport doesn't send localhost
	if u, _ := strings.CutPrefix(upstreamURL, "https://"); u != upstreamURL {
		upstreamReq.Host = strings.SplitN(u, "/", 2)[0]
	} else if u, _ := strings.CutPrefix(upstreamURL, "http://"); u != upstreamURL {
		upstreamReq.Host = strings.SplitN(u, "/", 2)[0]
	}

	resp, err := reverseHTTPClient.Do(upstreamReq)
	if err != nil {
		duration := int(time.Since(start).Milliseconds())
		log.Printf("[Turn %d] upstream error: %v", turn, err)
		record := buildRecord(reqID, turn, duration, r.Method, r.URL.RequestURI(), r.Header, reqBody, 502, nil, map[string]interface{}{"error": err.Error()}, nil, "")
		ctx.Writer.Write(record)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if isStreaming && resp.StatusCode == http.StatusOK {
		handleStreamingResponse(w, resp, reqID, turn, start, r.Method, r.URL.RequestURI(), r.Header, reqBody, ctx.Writer, target)
	} else {
		handleNonStreamingResponse(w, resp, reqID, turn, start, r.Method, r.URL.RequestURI(), r.Header, reqBody, ctx.Writer, target)
	}
}

func handleStreamingResponse(w http.ResponseWriter, resp *http.Response, reqID string, turn int, start time.Time, method, path string, reqHeaders http.Header, reqBody interface{}, writer *trace.Writer, upstreamBaseURL string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		handleNonStreamingResponse(w, resp, reqID, turn, start, method, path, reqHeaders, reqBody, writer, upstreamBaseURL)
		return
	}

	// Copy headers
	for k, v := range resp.Header {
		if !hopByHop[strings.ToLower(k)] {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	sse := NewSSEReassembler()
	var allChunks bytes.Buffer

	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			allChunks.Write(chunk)
			w.Write(chunk)
			flusher.Flush()
			sse.FeedBytes(chunk)
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("[Turn %d] stream read error: %v", turn, err)
			}
			break
		}
	}

	duration := int(time.Since(start).Milliseconds())
	reconstructed := sse.Reconstruct()

	var usage map[string]interface{}
	if reconstructed != nil {
		if u, ok := reconstructed["usage"].(map[string]interface{}); ok {
			usage = u
		}
	}
	usage = trace.NormalizeUsage(usage)
	log.Printf("[Turn %d] <- 200 stream done (%dms, in=%v out=%v)", turn, duration, usage["input_tokens"], usage["output_tokens"])

	record := buildRecord(reqID, turn, duration, method, path, reqHeaders, reqBody, resp.StatusCode, resp.Header, reconstructed, sse.Events, upstreamBaseURL)
	writer.Write(record)
}

func handleNonStreamingResponse(w http.ResponseWriter, resp *http.Response, reqID string, turn int, start time.Time, method, path string, reqHeaders http.Header, reqBody interface{}, writer *trace.Writer, upstreamBaseURL string) {
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[Turn %d] response read error: %v", turn, err)
	}
	duration := int(time.Since(start).Milliseconds())

	// Decompress for JSON parsing
	decodeBytes := respBytes
	contentEnc := strings.ToLower(resp.Header.Get("Content-Encoding"))
	if contentEnc == "gzip" && len(respBytes) > 0 {
		if r, err := gzip.NewReader(bytes.NewReader(respBytes)); err == nil {
			if d, err := io.ReadAll(r); err == nil {
				decodeBytes = d
			}
			r.Close()
		}
	} else if contentEnc == "deflate" && len(respBytes) > 0 {
		if r, err := zlib.NewReader(bytes.NewReader(respBytes)); err == nil {
			if d, err := io.ReadAll(r); err == nil {
				decodeBytes = d
			}
			r.Close()
		}
	}

	var respBody interface{}
	if len(decodeBytes) > 0 && json.Valid(decodeBytes) {
		json.Unmarshal(decodeBytes, &respBody)
	} else if len(decodeBytes) > 0 {
		respBody = string(decodeBytes)
	}

	log.Printf("[Turn %d] <- %d (%dms, %d bytes)", turn, resp.StatusCode, duration, len(respBytes))

	record := buildRecord(reqID, turn, duration, method, path, reqHeaders, reqBody, resp.StatusCode, resp.Header, respBody, nil, upstreamBaseURL)
	writer.Write(record)

	// Write response to client
	for k, v := range resp.Header {
		if !hopByHop[strings.ToLower(k)] {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBytes)
}

func buildRecord(reqID string, turn, duration int, method, path string, reqHeaders http.Header, reqBody interface{}, status int, respHeaders http.Header, respBody interface{}, sseEvents []map[string]interface{}, upstreamBaseURL string) map[string]interface{} {
	record := map[string]interface{}{
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
		"request_id":  reqID,
		"turn":        turn,
		"duration_ms": duration,
		"request": map[string]interface{}{
			"method":  method,
			"path":    path,
			"headers": FilterHeaders(reqHeaders, true),
			"body":    reqBody,
		},
		"response": map[string]interface{}{
			"status":  status,
			"headers": FilterHeaders(respHeaders, true),
			"body":    respBody,
		},
	}
	if sseEvents != nil {
		record["response"].(map[string]interface{})["sse_events"] = sseEvents
	}
	if upstreamBaseURL != "" {
		record["upstream_base_url"] = upstreamBaseURL
	}
	return record
}

// --- WebSocket proxy (reverse mode) ---

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func handleWebSocket(w http.ResponseWriter, r *http.Request, ctx *ReverseProxyContext, turn int) {
	start := time.Now()
	reqID := fmt.Sprintf("req_%x", md5.Sum([]byte(time.Now().String())))[:15]

	// Build upstream WS URL
	fwdPath := r.URL.RequestURI()
	if ctx.StripPathPrefix != "" && strings.HasPrefix(fwdPath, ctx.StripPathPrefix) {
		fwdPath = fwdPath[len(ctx.StripPathPrefix):]
		if fwdPath == "" {
			fwdPath = "/"
		}
	}
	target := strings.TrimRight(ctx.TargetURL, "/")
	upstreamURL := target + "/" + strings.TrimLeft(fwdPath, "/")

	// Convert http -> ws
	wsUpstreamURL := ""
	if strings.HasPrefix(upstreamURL, "https://") {
		wsUpstreamURL = "wss://" + upstreamURL[8:]
	} else if strings.HasPrefix(upstreamURL, "http://") {
		wsUpstreamURL = "ws://" + upstreamURL[7:]
	}

	// Connect to upstream WS
	fwdHeaders := http.Header{}
	for k, v := range r.Header {
		key := strings.ToLower(k)
		if hopByHop[key] || strings.HasPrefix(key, "sec-websocket-") {
			continue
		}
		fwdHeaders[k] = v
	}
	delete(fwdHeaders, "Host")

	upstreamWS, _, err := websocket.DefaultDialer.Dial(wsUpstreamURL, fwdHeaders)
	if err != nil {
		duration := int(time.Since(start).Milliseconds())
		log.Printf("[Turn %d] upstream WS connect failed: %v", turn, err)
		record := buildWSRecord(reqID, turn, duration, r.URL.RequestURI(), r.Header, nil, nil, target, err.Error())
		ctx.Writer.Write(record)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer upstreamWS.Close()

	// Accept client upgrade
	clientWS, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer clientWS.Close()

	// Bidirectional relay
	var clientMessages []string
	var serverMessages []string
	done := make(chan struct{}, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msgType, msg, err := clientWS.ReadMessage()
			if err != nil {
				return
			}
			if msgType == websocket.TextMessage {
				clientMessages = append(clientMessages, string(msg))
			}
			if err := upstreamWS.WriteMessage(msgType, msg); err != nil {
				return
			}
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msgType, msg, err := upstreamWS.ReadMessage()
			if err != nil {
				return
			}
			if msgType == websocket.TextMessage {
				serverMessages = append(serverMessages, string(msg))
			}
			if err := clientWS.WriteMessage(msgType, msg); err != nil {
				return
			}
		}
	}()

	// Wait for either side to close
	<-done
	<-done

	duration := int(time.Since(start).Milliseconds())
	record := buildWSRecord(reqID, turn, duration, r.URL.RequestURI(), r.Header, clientMessages, serverMessages, target, "")
	ctx.Writer.Write(record)
	log.Printf("[Turn %d] <- WS closed (%dms, %d->upstream, %d->client)", turn, duration, len(clientMessages), len(serverMessages))
}

func buildWSRecord(reqID string, turn, duration int, path string, reqHeaders http.Header, clientMsgs, serverMsgs []string, upstreamBaseURL, errorMsg string) map[string]interface{} {
	reqBody := reconstructWSRequestBody(clientMsgs)
	wsEvents := parseWSMessages(serverMsgs)
	respBody := reconstructWSResponseBody(wsEvents)

	record := map[string]interface{}{
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
		"request_id":  reqID,
		"turn":        turn,
		"duration_ms": duration,
		"transport":   "websocket",
		"request": map[string]interface{}{
			"method":  "WEBSOCKET",
			"path":    path,
			"headers": FilterHeaders(reqHeaders, true),
			"body":    reqBody,
		},
		"response": map[string]interface{}{
			"status":  101,
			"headers": map[string]string{},
			"body":    respBody,
		},
	}
	if len(wsEvents) > 0 {
		record["response"].(map[string]interface{})["ws_events"] = wsEvents
	}
	if errorMsg != "" {
		record["response"].(map[string]interface{})["status"] = 502
		record["response"].(map[string]interface{})["error"] = errorMsg
	}
	if upstreamBaseURL != "" {
		record["upstream_base_url"] = upstreamBaseURL
	}
	return record
}

func parseWSMessages(messages []string) []map[string]interface{} {
	var events []map[string]interface{}
	for _, msg := range messages {
		var parsed map[string]interface{}
		if json.Unmarshal([]byte(msg), &parsed) == nil {
			events = append(events, parsed)
		} else {
			events = append(events, map[string]interface{}{"raw": msg})
		}
	}
	return events
}

func reconstructWSRequestBody(messages []string) map[string]interface{} {
	var merged map[string]interface{}
	for _, msg := range messages {
		var parsed map[string]interface{}
		if json.Unmarshal([]byte(msg), &parsed) != nil {
			continue
		}
		if merged == nil {
			merged = parsed
			continue
		}
		for k, v := range parsed {
			if k == "input" || k == "tools" {
				if existing, ok := merged[k].([]interface{}); ok {
					if incoming, ok := v.([]interface{}); ok {
						merged[k] = mergeJSONLists(existing, incoming)
					} else {
						merged[k] = v
					}
				} else {
					merged[k] = v
				}
			} else if v != nil && v != "" {
				merged[k] = v
			}
		}
	}
	return merged
}

func reconstructWSResponseBody(events []map[string]interface{}) map[string]interface{} {
	var merged map[string]interface{}
	outputItems := make(map[int]map[string]interface{})

	for _, event := range events {
		eventType, _ := event["type"].(string)
		var payload map[string]interface{}
		if p, ok := event["response"].(map[string]interface{}); ok {
			payload = p
		} else {
			payload = event
		}

		if eventType == "response.created" || eventType == "response.in_progress" ||
			eventType == "response.completed" || eventType == "response.done" {
			if merged == nil {
				merged = deepCopyMap(payload)
			} else {
				for k, v := range payload {
					if k == "output" || k == "usage" {
						if v != nil {
							merged[k] = v
						}
					} else if v != nil && v != "" && v != 0 {
						merged[k] = v
					}
				}
			}
		}

		if eventType == "response.output_item.done" {
			if item, ok := event["item"].(map[string]interface{}); ok {
				if idx := getInt(event, "output_index"); idx >= 0 {
					outputItems[idx] = item
				}
			}
		}
	}

	if len(outputItems) > 0 {
		ordered := make([]interface{}, 0)
		for i := 0; i < len(outputItems)+10; i++ {
			if item, ok := outputItems[i]; ok {
				ordered = append(ordered, item)
			}
		}
		if merged == nil {
			merged = map[string]interface{}{"output": ordered}
		} else if merged["output"] == nil {
			merged["output"] = ordered
		}
	}

	return merged
}

func mergeJSONLists(existing, incoming []interface{}) []interface{} {
	seen := make(map[string]bool)
	merged := make([]interface{}, 0, len(existing)+len(incoming))

	for _, item := range existing {
		key := jsonListItemKey(item)
		if !seen[key] {
			merged = append(merged, item)
			seen[key] = true
		}
	}
	for _, item := range incoming {
		key := jsonListItemKey(item)
		if !seen[key] {
			merged = append(merged, item)
			seen[key] = true
		}
	}
	return merged
}

func jsonListItemKey(item interface{}) string {
	data, _ := json.Marshal(item)
	return string(data)
}
