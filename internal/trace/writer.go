package trace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Broadcaster receives trace records after they are persisted.
type Broadcaster interface {
	Broadcast(record map[string]interface{})
}

// Writer writes trace records to a JSONL file and accumulates statistics.
type Writer struct {
	path                   string
	mu                     sync.Mutex
	file                   *os.File
	Count                  int
	TotalInputTokens       int64
	TotalOutputTokens      int64
	TotalCacheReadTokens   int64
	TotalCacheCreateTokens int64
	ModelsUsed             map[string]int
	metadata               map[string]string
	broadcaster            Broadcaster
}

// NewWriter creates a new trace writer that appends to the given JSONL file.
func NewWriter(path string, metadata map[string]string) (*Writer, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}

	return &Writer{
		path:       path,
		file:       file,
		ModelsUsed: make(map[string]int),
		metadata:   metadata,
	}, nil
}

// SetBroadcaster connects the writer to a live viewer.
func (w *Writer) SetBroadcaster(b Broadcaster) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.broadcaster = b
}

// Write writes a single trace record to the JSONL file and updates statistics.
func (w *Writer) Write(record map[string]interface{}) error {
	w.mu.Lock()
	if w.metadata != nil {
		capture := make(map[string]interface{})
		if c, ok := record["capture"].(map[string]interface{}); ok {
			for k, v := range c {
				capture[k] = v
			}
		}
		for k, v := range w.metadata {
			capture[k] = v
		}
		record["capture"] = capture
	}

	data, err := json.Marshal(record)
	if err != nil {
		w.mu.Unlock()
		return err
	}
	data = append(data, '\n')

	if _, err := w.file.Write(data); err != nil {
		w.mu.Unlock()
		return err
	}
	if err := w.file.Sync(); err != nil {
		w.mu.Unlock()
		return err
	}

	w.Count++
	w.updateStats(record)
	broadcaster := w.broadcaster
	w.mu.Unlock()

	if broadcaster != nil {
		broadcaster.Broadcast(record)
	}
	return nil
}

// Close flushes and closes the JSONL file.
func (w *Writer) Close() error {
	if w.file != nil {
		if err := w.file.Sync(); err != nil {
			return err
		}
		return w.file.Close()
	}
	return nil
}

// Path returns the trace file path.
func (w *Writer) Path() string {
	return w.path
}

func (w *Writer) updateStats(record map[string]interface{}) {
	model := "unknown"
	if req, ok := record["request"].(map[string]interface{}); ok {
		if body, ok := req["body"].(map[string]interface{}); ok {
			if m, ok := body["model"].(string); ok {
				model = m
			}
		}
	}
	w.ModelsUsed[model]++

	resp, _ := record["response"].(map[string]interface{})
	body, _ := resp["body"].(map[string]interface{})

	usage := NormalizeUsage(body)
	if len(usage) == 0 && body != nil {
		usage = body
		usage = NormalizeUsage(usage)
	}

	if v, ok := toInt64(usage["input_tokens"]); ok {
		w.TotalInputTokens += v
	}
	if v, ok := toInt64(usage["output_tokens"]); ok {
		w.TotalOutputTokens += v
	}
	if v, ok := toInt64(usage["cache_read_input_tokens"]); ok {
		w.TotalCacheReadTokens += v
	}
	if v, ok := toInt64(usage["cache_creation_input_tokens"]); ok {
		w.TotalCacheCreateTokens += v
	}
}

// Summary returns current trace statistics.
func (w *Writer) Summary() map[string]interface{} {
	return map[string]interface{}{
		"api_calls":           w.Count,
		"input_tokens":        w.TotalInputTokens,
		"output_tokens":       w.TotalOutputTokens,
		"cache_read_tokens":   w.TotalCacheReadTokens,
		"cache_create_tokens": w.TotalCacheCreateTokens,
		"models_used":         w.ModelsUsed,
	}
}

func toInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}
