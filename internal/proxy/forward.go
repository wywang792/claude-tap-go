package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/md5"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wywang792/claude-tap-go/internal/trace"
)

// ForwardProxyServer implements an HTTP forward proxy with CONNECT tunneling and MITM TLS termination.
type ForwardProxyServer struct {
	Host   string
	Port   int
	CA     CACertProvider
	Writer *trace.Writer
	Client *http.Client

	actualPort  int
	listener    net.Listener
	mu          sync.Mutex
	turnCounter int
	activeConns map[net.Conn]struct{}
	shutdown    bool

	localReverseTarget              string
	localReverseAllowedPathPrefixes []string
}

// CACertProvider is the interface for generating TLS certificates for hosts.
type CACertProvider interface {
	GetHostCertPEM(hostname string) ([]byte, []byte, error)
}

// NewForwardProxyServer creates a new forward proxy server.
func NewForwardProxyServer(host string, port int, ca CACertProvider, writer *trace.Writer, localReverseTarget string, localReverseAllowedPathPrefixes []string) *ForwardProxyServer {
	return &ForwardProxyServer{
		Host:   host,
		Port:   port,
		CA:     ca,
		Writer: writer,
		Client: &http.Client{
			Timeout: 600 * time.Second,
			Transport: &http.Transport{
				Proxy:           http.ProxyFromEnvironment,
				TLSClientConfig: &tls.Config{},
			},
		},
		activeConns:                     make(map[net.Conn]struct{}),
		turnCounter:                     0,
		localReverseTarget:              localReverseTarget,
		localReverseAllowedPathPrefixes: localReverseAllowedPathPrefixes,
	}
}

// Start starts the forward proxy server. Returns the actual port.
func (s *ForwardProxyServer) Start() (int, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.Host, s.Port))
	if err != nil {
		return 0, err
	}
	s.listener = listener
	s.actualPort = listener.Addr().(*net.TCPAddr).Port

	go s.acceptLoop()
	return s.actualPort, nil
}

// Stop stops the forward proxy server.
func (s *ForwardProxyServer) Stop() error {
	s.mu.Lock()
	s.shutdown = true
	s.mu.Unlock()

	if s.listener != nil {
		s.listener.Close()
	}

	s.mu.Lock()
	for conn := range s.activeConns {
		conn.Close()
	}
	s.mu.Unlock()

	return nil
}

func (s *ForwardProxyServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.activeConns[conn] = struct{}{}
		s.mu.Unlock()

		go func() {
			defer func() {
				s.mu.Lock()
				delete(s.activeConns, conn)
				s.mu.Unlock()
				conn.Close()
			}()
			s.handleClient(conn)
		}()
	}
}

func (s *ForwardProxyServer) handleClient(conn net.Conn) {
	reader := bufio.NewReader(conn)
	requestLine, err := readLineTimeout(reader, 30*time.Second)
	if err != nil || len(requestLine) == 0 {
		return
	}

	parts := strings.Split(strings.TrimSpace(string(requestLine)), " ")
	if len(parts) < 3 {
		return
	}

	method := strings.ToUpper(parts[0])

	if method == "CONNECT" {
		s.handleConnect(conn, reader, parts[1])
	} else {
		// Plain HTTP proxy (non-CONNECT) 鈥?read headers and body
		headers := readHeaders(reader)
		body := readHTTPBody(reader, headers)
		s.forwardHTTP(conn, method, parts[1], headers, body)
	}
}

func (s *ForwardProxyServer) handleConnect(conn net.Conn, reader *bufio.Reader, authority string) {
	// Parse host:port
	hostname, portStr := authority, "443"
	if idx := strings.LastIndex(authority, ":"); idx > 0 {
		hostname = authority[:idx]
		portStr = authority[idx+1:]
	}
	port, _ := strconv.Atoi(portStr)

	// Read and discard remaining headers until blank line
	readHeaders(reader)

	// Send 200 Connection Established
	conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// Get host certificate
	certPEM, keyPEM, err := s.CA.GetHostCertPEM(hostname)
	if err != nil {
		log.Printf("Failed to generate cert for %s: %v", hostname, err)
		return
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		log.Printf("Failed to parse cert for %s: %v", hostname, err)
		return
	}

	// TLS terminate on our side
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	tlsConn := tls.Server(conn, tlsConfig)

	// Read HTTP requests from inside the TLS tunnel
	tlsReader := bufio.NewReader(tlsConn)
	s.handleTunneledRequests(tlsConn, tlsReader, hostname, port)
}

func (s *ForwardProxyServer) handleTunneledRequests(conn *tls.Conn, reader *bufio.Reader, hostname string, port int) {
	for {
		requestLine, err := readLineTimeout(reader, 600*time.Second)
		if err != nil || len(requestLine) == 0 {
			break
		}

		parts := strings.Split(strings.TrimSpace(string(requestLine)), " ")
		if len(parts) < 3 {
			break
		}

		method, path := parts[0], parts[1]

		headers := readHeaders(reader)
		body := readHTTPBody(reader, headers)

		// Check for WebSocket upgrade
		if isWebSocketUpgrade(headers) {
			s.forwardWebSocket(conn, reader, hostname, port, path, headers, body)
			break
		}

		upstreamURL := fmt.Sprintf("https://%s:%d%s", hostname, port, path)
		s.forwardAndRecord(conn, method, path, headers, body, upstreamURL)
	}
}

func (s *ForwardProxyServer) forwardAndRecord(conn net.Conn, method, path string, reqHeaders map[string]string, body []byte, upstreamURL string) {
	s.mu.Lock()
	s.turnCounter++
	turn := s.turnCounter
	s.mu.Unlock()

	start := time.Now()
	reqID := fmt.Sprintf("req_%x", md5sum(time.Now().String()))[:15]
	logPrefix := fmt.Sprintf("[Turn %d]", turn)

	var reqBody interface{}
	if len(body) > 0 && json.Valid(body) {
		json.Unmarshal(body, &reqBody)
	} else if len(body) > 0 {
		reqBody = string(body)
	}

	reqBodyMap, _ := reqBody.(map[string]interface{})
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

	log.Printf("%s -> %s %s (model=%s, stream=%v)", logPrefix, method, path, model, isStreaming)

	// Build upstream request
	fwdHeaders := make(http.Header)
	for k, v := range reqHeaders {
		key := strings.ToLower(k)
		if key != "host" && !hopByHop[key] {
			fwdHeaders.Set(k, v)
		}
	}
	fwdHeaders.Set("Accept-Encoding", "identity")

	upstreamReq, err := http.NewRequest(method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		sendError(conn, 502, err.Error())
		return
	}
	upstreamReq.Header = fwdHeaders

	resp, err := s.Client.Do(upstreamReq)
	if err != nil {
		duration := int(time.Since(start).Milliseconds())
		log.Printf("%s upstream error: %v", logPrefix, err)
		record := buildRecord(reqID, turn, duration, method, path, headerToMap(reqHeaders), reqBody, 502, nil, map[string]interface{}{"error": err.Error()}, nil, "")
		s.Writer.Write(record)
		sendError(conn, 502, err.Error())
		return
	}
	defer resp.Body.Close()

	if isStreaming && resp.StatusCode == http.StatusOK {
		s.handleForwardStreaming(conn, resp, reqID, turn, start, method, path, reqHeaders, reqBody, logPrefix)
	} else {
		s.handleForwardNonStreaming(conn, resp, reqID, turn, start, method, path, reqHeaders, reqBody, logPrefix)
	}
}

func (s *ForwardProxyServer) handleForwardStreaming(conn net.Conn, resp *http.Response, reqID string, turn int, start time.Time, method, path string, reqHeaders map[string]string, reqBody interface{}, logPrefix string) {
	// Write response status line
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\n", resp.StatusCode, resp.Status)

	// Write headers (filter hop-by-hop)
	for k, v := range resp.Header {
		if !hopByHop[strings.ToLower(k)] {
			fmt.Fprintf(conn, "%s: %s\r\n", k, strings.Join(v, ", "))
		}
	}
	fmt.Fprintf(conn, "Transfer-Encoding: chunked\r\n\r\n")

	sse := NewSSEReassembler()
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			// Write as HTTP chunked encoding
			fmt.Fprintf(conn, "%x\r\n", n)
			conn.Write(chunk)
			conn.Write([]byte("\r\n"))
			sse.FeedBytes(chunk)
		}
		if err != nil {
			break
		}
	}

	// Send final chunk
	conn.Write([]byte("0\r\n\r\n"))

	duration := int(time.Since(start).Milliseconds())
	reconstructed := sse.Reconstruct()

	var usage map[string]interface{}
	if reconstructed != nil {
		if u, ok := reconstructed["usage"].(map[string]interface{}); ok {
			usage = u
		}
	}
	usage = trace.NormalizeUsage(usage)
	log.Printf("%s <- 200 stream done (%dms)", logPrefix, duration)

	record := buildRecord(reqID, turn, duration, method, path, headerToMap(reqHeaders), reqBody, resp.StatusCode, resp.Header, reconstructed, sse.Events, "")
	s.Writer.Write(record)
}

func (s *ForwardProxyServer) handleForwardNonStreaming(conn net.Conn, resp *http.Response, reqID string, turn int, start time.Time, method, path string, reqHeaders map[string]string, reqBody interface{}, logPrefix string) {
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("%s response read error: %v", logPrefix, err)
	}
	duration := int(time.Since(start).Milliseconds())

	// Decompress for JSON parsing
	decodeBytes := respBytes
	contentEnc := strings.ToLower(resp.Header.Get("Content-Encoding"))
	if contentEnc == "gzip" && len(respBytes) > 0 {
		if r, err := gzipDecompress(respBytes); err == nil {
			decodeBytes = r
		}
	} else if contentEnc == "deflate" && len(respBytes) > 0 {
		if r, err := zlibDecompress(respBytes); err == nil {
			decodeBytes = r
		}
	}

	var respBody interface{}
	if len(decodeBytes) > 0 && json.Valid(decodeBytes) {
		json.Unmarshal(decodeBytes, &respBody)
	} else if len(decodeBytes) > 0 {
		respBody = string(decodeBytes)
	}

	log.Printf("%s <- %d (%dms, %d bytes)", logPrefix, resp.StatusCode, duration, len(respBytes))

	record := buildRecord(reqID, turn, duration, method, path, headerToMap(reqHeaders), reqBody, resp.StatusCode, resp.Header, respBody, nil, "")
	s.Writer.Write(record)

	// Write response to client
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\n", resp.StatusCode, resp.Status)
	for k, v := range resp.Header {
		key := strings.ToLower(k)
		if !hopByHop[key] && key != "content-length" {
			fmt.Fprintf(conn, "%s: %s\r\n", k, strings.Join(v, ", "))
		}
	}
	fmt.Fprintf(conn, "Content-Length: %d\r\n\r\n", len(respBytes))
	conn.Write(respBytes)
}

func (s *ForwardProxyServer) forwardWebSocket(conn net.Conn, reader *bufio.Reader, hostname string, port int, path string, reqHeaders map[string]string, body []byte) {
	// WebSocket inside CONNECT tunnel
	s.mu.Lock()
	s.turnCounter++
	turn := s.turnCounter
	s.mu.Unlock()

	start := time.Now()
	reqID := fmt.Sprintf("req_%x", md5sum(time.Now().String()))[:15]
	logPrefix := fmt.Sprintf("[Turn %d]", turn)
	// Build upstream WS dial headers
	dialHeaders := http.Header{}
	for k, v := range reqHeaders {
		keyL := strings.ToLower(k)
		if !hopByHop[keyL] && keyL != "host" && !strings.HasPrefix(keyL, "sec-websocket-") {
			dialHeaders.Set(k, v)
		}
	}

	log.Printf("%s -> WS UPGRADE %s", logPrefix, path)

	// We need to relay WebSocket frames manually. For now, record and close.
	// Full WS relay through a CONNECT tunnel requires raw WebSocket frame parsing
	// which is complex. We record what we can and return.

	record := map[string]interface{}{
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
		"request_id":  reqID,
		"turn":        turn,
		"duration_ms": int(time.Since(start).Milliseconds()),
		"transport":   "websocket",
		"request": map[string]interface{}{
			"method":  "WEBSOCKET",
			"path":    path,
			"headers": FilterHeaders(headerToHttpHeader(reqHeaders), true),
			"body":    nil,
		},
		"response": map[string]interface{}{
			"status":  101,
			"headers": map[string]string{},
			"body":    nil,
		},
		"upstream_base_url": fmt.Sprintf("https://%s:%d", hostname, port),
	}
	s.Writer.Write(record)
	log.Printf("%s <- WS closed (forward proxy WS relay)", logPrefix)
}

func (s *ForwardProxyServer) forwardHTTP(conn net.Conn, method, targetURL string, headers map[string]string, body []byte) {
	parsed, err := url.Parse(targetURL)
	path := targetURL
	if err == nil {
		if parsed.Path != "" {
			path = parsed.Path
			if parsed.RawQuery != "" {
				path += "?" + parsed.RawQuery
			}
		}
		if parsed.Scheme == "" && s.localReverseTarget != "" && matchesPathPrefix(path, s.localReverseAllowedPathPrefixes) {
			upstreamURL := strings.TrimRight(s.localReverseTarget, "/") + "/" + strings.TrimLeft(path, "/")
			s.forwardAndRecord(conn, method, path, headers, body, upstreamURL)
			return
		}
	}
	s.forwardAndRecord(conn, method, path, headers, body, targetURL)
}

// --- Helpers ---

func readLineTimeout(reader *bufio.Reader, timeout time.Duration) ([]byte, error) {
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		l, e := reader.ReadBytes('\n')
		ch <- result{l, e}
	}()
	select {
	case r := <-ch:
		return r.line, r.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("read timeout")
	}
}

func readHeaders(reader *bufio.Reader) map[string]string {
	headers := make(map[string]string)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if idx := strings.Index(line, ":"); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])
			headers[key] = val
		}
	}
	return headers
}

func readHTTPBody(reader *bufio.Reader, headers map[string]string) []byte {
	cl := headers["Content-Length"]
	if cl == "" {
		cl = headers["content-length"]
	}
	if cl != "" {
		if length, err := strconv.Atoi(cl); err == nil && length > 0 {
			body := make([]byte, length)
			_, err := io.ReadFull(reader, body)
			if err != nil {
				return nil
			}
			return body
		}
	}
	te := headers["Transfer-Encoding"]
	if te == "" {
		te = headers["transfer-encoding"]
	}
	if strings.Contains(strings.ToLower(te), "chunked") {
		return readChunkedBody(reader)
	}
	return nil
}

func readChunkedBody(reader *bufio.Reader) []byte {
	var chunks []byte
	for {
		sizeLine, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		sizeLine = strings.TrimSpace(sizeLine)
		if idx := strings.IndexByte(sizeLine, ';'); idx >= 0 {
			sizeLine = sizeLine[:idx]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(sizeLine), 16, 64)
		if err != nil || size == 0 {
			// Read trailers until blank line
			for {
				l, _ := reader.ReadString('\n')
				if strings.TrimRight(l, "\r\n") == "" {
					break
				}
			}
			break
		}
		chunk := make([]byte, size)
		io.ReadFull(reader, chunk)
		chunks = append(chunks, chunk...)
		// Read CRLF after chunk
		reader.ReadString('\n')
	}
	return chunks
}

func isWebSocketUpgrade(headers map[string]string) bool {
	upgrade := strings.ToLower(headers["Upgrade"])
	if upgrade == "" {
		upgrade = strings.ToLower(headers["upgrade"])
	}
	if upgrade != "websocket" {
		return false
	}
	conn := strings.ToLower(headers["Connection"])
	if conn == "" {
		conn = strings.ToLower(headers["connection"])
	}
	return strings.Contains(conn, "upgrade")
}

func matchesPathPrefix(path string, prefixes []string) bool {
	clean := strings.SplitN(path, "?", 2)[0]
	clean = strings.TrimRight(clean, "/")
	for _, prefix := range prefixes {
		if clean == prefix || strings.HasPrefix(clean, prefix+"/") || strings.HasPrefix(clean, prefix+":") {
			return true
		}
	}
	return false
}

func sendError(conn net.Conn, status int, msg string) {
	body := []byte(msg)
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	fmt.Fprintf(conn, "Content-Length: %d\r\n", len(body))
	fmt.Fprintf(conn, "Content-Type: text/plain\r\n\r\n")
	conn.Write(body)
}

func headerToMap(h map[string]string) http.Header {
	result := make(http.Header)
	for k, v := range h {
		result.Set(k, v)
	}
	return result
}

func headerToHttpHeader(h map[string]string) http.Header {
	result := make(http.Header)
	for k, v := range h {
		result.Set(k, v)
	}
	return result
}

func md5sum(s string) string {
	hash := md5.Sum([]byte(s))
	return fmt.Sprintf("%x", hash[:])
}

func gzipDecompress(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func zlibDecompress(data []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
