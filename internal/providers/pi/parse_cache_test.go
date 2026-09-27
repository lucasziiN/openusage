package pi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain keeps every test in this package away from the user's real parse
// cache: scans read and rewrite the cache as a side effect.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "openusage-pi-cache-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	parseCachePath = func() string { return filepath.Join(dir, "pi-sessions.gob") }
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// useParseCache gives one test a cache file of its own.
func useParseCache(t *testing.T, path string) {
	t.Helper()
	previous := parseCachePath
	parseCachePath = func() string { return path }
	t.Cleanup(func() { parseCachePath = previous })
}

func totalCost(t *testing.T, dirs ...string) float64 {
	t.Helper()
	entries, err := readAllSessions(context.Background(), dirs)
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, entry := range entries {
		total += entry.CostUSD
	}
	return total
}

func TestScanTranscripts_ParsesOnlyNewOrChangedFiles(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "cache.gob")
	useParseCache(t, cachePath)
	root := t.TempDir()
	turn := func(id, cost string) string {
		return `{"type":"message","id":"` + id + `","timestamp":"2026-09-27T09:00:00Z","message":{"role":"assistant","model":"m","usage":{"input":1,"cost":{"total":` + cost + `}}}}` + "\n"
	}
	write := func(name, body string) string {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	sessionA := `{"type":"session","id":"a"}` + "\n" + turn("a1", "1.25")
	pathA := write("a.jsonl", sessionA)
	pathB := write("b.jsonl", `{"type":"session","id":"b"}`+"\n"+turn("b1", "2"))
	if got := totalCost(t, root); got != 3.25 {
		t.Fatalf("first scan = %v, want 3.25", got)
	}

	// Same size and modification time: the cached parse is used, so the
	// rewritten price is not seen.
	info, err := os.Stat(pathA)
	if err != nil {
		t.Fatal(err)
	}
	write("a.jsonl", strings.Replace(sessionA, "1.25", "9.75", 1))
	if err := os.Chtimes(pathA, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got := totalCost(t, root); got != 3.25 {
		t.Fatalf("unchanged file was parsed again: total = %v, want the cached 3.25", got)
	}

	// A grown file is parsed again, in full.
	write("a.jsonl", strings.Replace(sessionA, "1.25", "9.75", 1)+turn("a2", "4"))
	if got := totalCost(t, root); got != 15.75 {
		t.Fatalf("grown file: total = %v, want 9.75 + 4 + 2", got)
	}

	// Another sessions directory does not evict this one's records...
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "c.jsonl"), []byte(`{"type":"session","id":"c"}`+"\n"+turn("c1", "8")), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := totalCost(t, other); got != 8 {
		t.Fatalf("other directory = %v, want 8", got)
	}
	// ...but a deleted file leaves both the result and the cache.
	if err := os.Remove(pathB); err != nil {
		t.Fatal(err)
	}
	if got := totalCost(t, root); got != 13.75 {
		t.Fatalf("after deleting b: total = %v, want 13.75", got)
	}
	cached := loadParseCache(cachePath)
	var names []string
	for key := range cached {
		names = append(names, filepath.Base(key))
	}
	if len(cached) != 2 || cached[filepath.Join(canonicalPath(root), "a.jsonl")] == nil {
		t.Fatalf("cached transcripts = %v, want a.jsonl and c.jsonl", names)
	}
}

func TestScanTranscripts_SurvivesUnusableCache(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string) string // returns the cache path
	}{
		{
			name: "corrupt cache file",
			setup: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "cache.gob")
				if err := os.WriteFile(path, []byte("not a gob"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "cache directory is a file",
			setup: func(t *testing.T, dir string) string {
				blocker := filepath.Join(dir, "blocker")
				if err := os.WriteFile(blocker, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(blocker, "cache.gob")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useParseCache(t, tt.setup(t, t.TempDir()))
			root := t.TempDir()
			body := `{"type":"session","id":"s"}` + "\n" +
				`{"type":"message","id":"t1","timestamp":"2026-09-27T09:00:00Z","message":{"role":"assistant","model":"m","usage":{"input":1,"cost":{"total":2.5}}}}` + "\n"
			if err := os.WriteFile(filepath.Join(root, "s.jsonl"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			for scan := 1; scan <= 2; scan++ {
				if got := totalCost(t, root); got != 2.5 {
					t.Fatalf("scan %d = %v, want 2.5", scan, got)
				}
			}
		})
	}
}
