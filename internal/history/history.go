package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const manifestFile = ".cloudtap-manifest.json"

// LoadManifest loads or creates the trace manifest file.
func LoadManifest(outputDir string) (map[string]interface{}, error) {
	manifestPath := filepath.Join(outputDir, manifestFile)
	data, err := os.ReadFile(manifestPath)
	if err == nil {
		var manifest map[string]interface{}
		if json.Unmarshal(data, &manifest) == nil {
			if _, ok := manifest["_cloudtap"]; ok {
				return manifest, nil
			}
		}
	}

	manifest := map[string]interface{}{
		"_cloudtap": true,
		"version":   "0.1.0",
		"traces":    []interface{}{},
	}
	MaybeMigrateExisting(outputDir, manifest)
	SaveManifest(outputDir, manifest)
	return manifest, nil
}

// SaveManifest saves the trace manifest to disk.
func SaveManifest(outputDir string, manifest map[string]interface{}) error {
	manifestPath := filepath.Join(outputDir, manifestFile)
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(manifestPath, data, 0644)
}

// RegisterTrace registers a new trace session in the manifest.
func RegisterTrace(outputDir, ts string, traceFiles []string, metadata map[string]string) (map[string]interface{}, error) {
	manifest, err := LoadManifest(outputDir)
	if err != nil {
		return nil, err
	}

	entry := map[string]interface{}{
		"timestamp":  ts,
		"files":      traceFiles,
		"created_at": time.Now().UTC().Format(time.RFC3339),
	}
	for k, v := range metadata {
		entry[k] = v
	}

	traces, _ := manifest["traces"].([]interface{})
	traces = append(traces, entry)
	manifest["traces"] = traces

	err = SaveManifest(outputDir, manifest)
	return manifest, err
}

// CleanupTraces removes oldest traces exceeding maxTraces. Returns count of deleted sessions.
func CleanupTraces(outputDir string, maxTraces int) int {
	if maxTraces <= 0 {
		return 0
	}

	manifest, err := LoadManifest(outputDir)
	if err != nil {
		return 0
	}

	traces, _ := manifest["traces"].([]interface{})
	if len(traces) <= maxTraces {
		return 0
	}

	// Sort by timestamp
	sort.Slice(traces, func(i, j int) bool {
		ti, _ := traces[i].(map[string]interface{})
		tj, _ := traces[j].(map[string]interface{})
		return getString(ti, "timestamp") < getString(tj, "timestamp")
	})

	toRemove := traces[:len(traces)-maxTraces]
	removed := 0
	var parentsToCheck []string

	for _, entry := range toRemove {
		entryMap, _ := entry.(map[string]interface{})
		files, _ := entryMap["files"].([]interface{})
		for _, f := range files {
			fname, _ := f.(string)
			fpath := filepath.Join(outputDir, fname)
			parentsToCheck = append(parentsToCheck, filepath.Dir(fpath))
			os.Remove(fpath)
		}
		removed++
	}

	// Remove empty dirs
	for _, parent := range parentsToCheck {
		entries, _ := os.ReadDir(parent)
		if len(entries) == 0 {
			os.Remove(parent)
		}
	}

	manifest["traces"] = traces[len(toRemove):]
	SaveManifest(outputDir, manifest)
	return removed
}

// MaybeMigrateExisting auto-registers existing trace_*.jsonl files not in the manifest.
func MaybeMigrateExisting(outputDir string, manifest map[string]interface{}) {
	knownFiles := make(map[string]bool)
	if traces, ok := manifest["traces"].([]interface{}); ok {
		for _, entry := range traces {
			if entryMap, ok := entry.(map[string]interface{}); ok {
				if files, ok := entryMap["files"].([]interface{}); ok {
					for _, f := range files {
						if fname, ok := f.(string); ok {
							knownFiles[filepath.ToSlash(fname)] = true
						}
					}
				}
			}
		}
	}

	matches := findTraceFilesRecursive(outputDir)
	for _, jsonl := range matches {
		rel := RelPosix(jsonl, outputDir)
		if knownFiles[rel] || knownFiles[filepath.Base(jsonl)] {
			continue
		}

		stem := filepath.Base(jsonl)
		stem = strings.TrimSuffix(stem, ".jsonl")
		ts := strings.TrimPrefix(stem, "trace_")

		files := []string{rel}
		for _, suffix := range []string{".log", ".html"} {
			companion := jsonl[:len(jsonl)-len(".jsonl")] + suffix
			if _, err := os.Stat(companion); err == nil {
				files = append(files, RelPosix(companion, outputDir))
			}
		}

		traces, _ := manifest["traces"].([]interface{})
		traces = append(traces, map[string]interface{}{
			"timestamp":  ts,
			"files":      files,
			"created_at": time.Now().UTC().Format(time.RFC3339),
		})
		manifest["traces"] = traces
	}
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

// DeleteTraceHistory deletes stored trace files for a date key while keeping protected active files.
func DeleteTraceHistory(outputDir, dateKey string, protectedPaths []string) map[string]interface{} {
	result := map[string]interface{}{
		"date":           dateKey,
		"deleted_files":  0,
		"deleted_traces": 0,
		"skipped_files":  0,
	}

	manifest, _ := LoadManifest(outputDir)
	protected := make(map[string]bool)
	for _, p := range protectedPaths {
		protected[filepath.Clean(p)] = true
	}

	var traceDir string
	if dateKey == "legacy" {
		traceDir = outputDir
	} else {
		traceDir = filepath.Join(outputDir, dateKey)
	}

	matches, _ := filepath.Glob(filepath.Join(traceDir, "trace_*.jsonl"))
	for _, jsonl := range matches {
		if protected[jsonl] {
			result["skipped_files"] = result["skipped_files"].(int) + 1
			continue
		}
		for _, suffix := range []string{"", ".log", ".html"} {
			fpath := jsonl
			if suffix != "" {
				fpath = jsonl[:len(jsonl)-len(".jsonl")] + suffix
			}
			if _, err := os.Stat(fpath); err == nil {
				if err := os.Remove(fpath); err == nil {
					result["deleted_files"] = result["deleted_files"].(int) + 1
				}
			}
		}
	}

	// Remove from manifest
	if traces, ok := manifest["traces"].([]interface{}); ok {
		var remaining []interface{}
		for _, entry := range traces {
			entryMap, _ := entry.(map[string]interface{})
			files, _ := entryMap["files"].([]interface{})
			shouldRemove := false
			for _, f := range files {
				fname, _ := f.(string)
				if strings.HasPrefix(fname, dateKey+"/") || (dateKey == "legacy" && !strings.Contains(fname, "/")) {
					shouldRemove = true
					break
				}
			}
			if !shouldRemove {
				remaining = append(remaining, entry)
			} else {
				result["deleted_traces"] = result["deleted_traces"].(int) + 1
			}
		}
		manifest["traces"] = remaining
		SaveManifest(outputDir, manifest)
	}

	return result
}

// CleanupEmptyDirs removes empty date directories.
func CleanupEmptyDirs(outputDir string) {
	entries, _ := os.ReadDir(outputDir)
	for _, entry := range entries {
		if entry.IsDir() {
			dir := filepath.Join(outputDir, entry.Name())
			subEntries, _ := os.ReadDir(dir)
			if len(subEntries) == 0 {
				os.Remove(dir)
			}
		}
	}
}

// RelPosix returns a relative path with forward slashes for cross-platform manifest portability.
func RelPosix(path, base string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return filepath.Base(path)
	}
	return filepath.ToSlash(rel)
}

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
