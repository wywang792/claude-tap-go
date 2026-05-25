package live

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListTraceSessionsFindsNestedTraces(t *testing.T) {
	outputDir := t.TempDir()
	tracePath := filepath.Join(outputDir, "2026-05-22", "trace_120000.jsonl")
	if err := os.MkdirAll(filepath.Dir(tracePath), 0755); err != nil {
		t.Fatalf("create trace dir: %v", err)
	}
	if err := os.WriteFile(tracePath, []byte(`{"timestamp":"2026-05-22T12:00:00Z","capture":{"client":"claude"},"turn":1}`+"\n"), 0644); err != nil {
		t.Fatalf("write trace: %v", err)
	}

	sessions := listTraceSessions(outputDir, tracePath)
	if len(sessions) != 1 {
		t.Fatalf("sessions len = %d, want 1", len(sessions))
	}
	if sessions[0]["rel_trace_path"] != "2026-05-22/trace_120000.jsonl" {
		t.Fatalf("rel_trace_path = %v", sessions[0]["rel_trace_path"])
	}
}

func TestSafeTracePathRejectsTraversal(t *testing.T) {
	outputDir := t.TempDir()
	if _, err := safeTracePath(outputDir, "../outside.jsonl"); err == nil {
		t.Fatal("safeTracePath accepted traversal path")
	}
}
