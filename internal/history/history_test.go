package history

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMaybeMigrateExistingFindsNestedDateTraces(t *testing.T) {
	outputDir := t.TempDir()
	tracePath := filepath.Join(outputDir, "2026-05-22", "trace_120000.jsonl")
	if err := os.MkdirAll(filepath.Dir(tracePath), 0755); err != nil {
		t.Fatalf("create trace dir: %v", err)
	}
	if err := os.WriteFile(tracePath, []byte("{}\n"), 0644); err != nil {
		t.Fatalf("write trace: %v", err)
	}

	manifest := map[string]interface{}{
		"_cloudtap": true,
		"version":   "0.1.0",
		"traces":    []interface{}{},
	}

	MaybeMigrateExisting(outputDir, manifest)

	traces, _ := manifest["traces"].([]interface{})
	if len(traces) != 1 {
		t.Fatalf("traces len = %d, want 1", len(traces))
	}
	entry, _ := traces[0].(map[string]interface{})
	files, _ := entry["files"].([]string)
	if len(files) != 1 || files[0] != "2026-05-22/trace_120000.jsonl" {
		t.Fatalf("files = %#v", entry["files"])
	}
}
