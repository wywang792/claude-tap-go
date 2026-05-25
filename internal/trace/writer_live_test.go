package trace_test

import (
	"bufio"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wywang792/claude-tap-go/internal/live"
	"github.com/wywang792/claude-tap-go/internal/trace"
)

func TestWriterBroadcastsToLiveServer(t *testing.T) {
	outputDir := t.TempDir()
	tracePath := filepath.Join(outputDir, "2026-05-22", "trace_test.jsonl")

	server := live.NewServer(tracePath, 0, "127.0.0.1", outputDir)
	port, err := server.Start()
	if err != nil {
		t.Fatalf("start live server: %v", err)
	}
	defer server.Stop()

	writer, err := trace.NewWriter(tracePath, map[string]string{"client": "claude"})
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	defer writer.Close()
	writer.SetBroadcaster(server)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/events")
	if err != nil {
		t.Fatalf("connect sse: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sse status = %d", resp.StatusCode)
	}

	record := map[string]interface{}{
		"request_id":  "req_live_test",
		"turn":        1,
		"duration_ms": 7,
		"request": map[string]interface{}{
			"method": "POST",
			"path":   "/v1/messages",
			"body": map[string]interface{}{
				"model": "test-model",
			},
		},
		"response": map[string]interface{}{
			"status": 200,
			"body": map[string]interface{}{
				"usage": map[string]interface{}{
					"input_tokens":  1,
					"output_tokens": 2,
				},
			},
		},
	}

	if err := writer.Write(record); err != nil {
		t.Fatalf("write record: %v", err)
	}

	reader := bufio.NewReader(resp.Body)
	var line string
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read sse line: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			break
		}
	}

	var got map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: "))), &got); err != nil {
		t.Fatalf("decode sse record: %v", err)
	}
	if got["request_id"] != "req_live_test" {
		t.Fatalf("request_id = %v", got["request_id"])
	}
	if capture, _ := got["capture"].(map[string]interface{}); capture["client"] != "claude" {
		t.Fatalf("capture client = %v", capture["client"])
	}
}
