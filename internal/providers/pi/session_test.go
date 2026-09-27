package pi

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestReadPiSessionFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	body := `{"type":"session","id":"pi_ses_001","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/home/jane/work/project-x"}
{"type":"message","id":"msg_001","parentId":null,"timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"assistant","model":"claude-3-5-sonnet","provider":"anthropic","usage":{"input":100,"output":50,"cacheRead":10,"cacheWrite":5,"totalTokens":165}}}
{"type":"message","id":"msg_002","timestamp":"2026-01-01T00:00:02.000Z","message":{"role":"user","model":"claude-3-5-sonnet","provider":"anthropic"}}
{"type":"message","id":"msg_003","timestamp":"2026-01-01T00:00:03.000Z","message":{"role":"assistant","model":"claude-3-5-sonnet","provider":"anthropic"}}
this is not json at all, skip me
{"type":"message","id":"msg_004","timestamp":"2026-01-01T00:00:04.000Z","message":{"role":"assistant","model":"gpt-4o","provider":"openai","usage":{"input":200,"output":80}}}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, meta, err := readPiSessionFile(path)
	if err != nil {
		t.Fatalf("readPiSessionFile: %v", err)
	}
	if meta.SessionID != "pi_ses_001" {
		t.Errorf("session id = %q, want pi_ses_001", meta.SessionID)
	}
	if meta.WorkspaceLabel != "project-x" {
		t.Errorf("workspace label = %q, want project-x", meta.WorkspaceLabel)
	}
	if meta.HeaderTime.IsZero() {
		t.Error("header time not parsed")
	}

	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (one assistant w/ usage, one gpt assistant w/ usage)", len(entries))
	}

	first := entries[0]
	if first.Model != "claude-3-5-sonnet" || first.Provider != "anthropic" {
		t.Errorf("first entry model/provider = %q/%q", first.Model, first.Provider)
	}
	if first.Input != 100 || first.Output != 50 || first.CacheRead != 10 || first.CacheWrite != 5 {
		t.Errorf("first entry tokens unexpected: %+v", first)
	}
	if first.SessionID != "pi_ses_001" {
		t.Errorf("first entry session id = %q", first.SessionID)
	}
	if first.WorkspaceLabel != "project-x" {
		t.Errorf("first entry workspace = %q", first.WorkspaceLabel)
	}

	second := entries[1]
	if second.Model != "gpt-4o" || second.Provider != "openai" {
		t.Errorf("second model/provider = %q/%q", second.Model, second.Provider)
	}
	if second.Input != 200 || second.Output != 80 || second.CacheRead != 0 || second.CacheWrite != 0 {
		t.Errorf("second tokens unexpected: %+v", second)
	}
}

func TestReadPiSessionFile_OmpTitleBeforeSession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	body := `{"type":"title","v":1,"title":"project-x"}
{"type":"session","id":"omp_ses_001","timestamp":"2026-02-01T00:00:00.000Z","cwd":"/home/jane/work/project-x"}
{"type":"message","id":"msg_001","timestamp":"2026-02-01T00:00:01.000Z","message":{"role":"assistant","model":"claude-sonnet","provider":"anthropic","usage":{"input":100,"output":40,"cacheRead":7,"cacheWrite":3}}}
{"type":"message","id":"msg_002","timestamp":"2026-02-01T00:00:02.000Z","message":{"role":"user","model":"claude-sonnet","provider":"anthropic","usage":{"input":500,"output":600}}}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, meta, err := readPiSessionFile(path)
	if err != nil {
		t.Fatalf("readPiSessionFile: %v", err)
	}
	if meta.SessionID != "omp_ses_001" {
		t.Fatalf("session id = %q, want omp_ses_001", meta.SessionID)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 assistant entry", len(entries))
	}
	entry := entries[0]
	if entry.SessionID != "omp_ses_001" || entry.Model != "claude-sonnet" || entry.Provider != "anthropic" {
		t.Errorf("entry session/model/provider = %q/%q/%q", entry.SessionID, entry.Model, entry.Provider)
	}
	if entry.Input != 100 || entry.Output != 40 || entry.CacheRead != 7 || entry.CacheWrite != 3 {
		t.Errorf("entry tokens = %+v", entry)
	}
}

func TestReadPiSessionFile_InvalidHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	body := `{"type":"message","id":"msg_001","message":{"role":"assistant","model":"x","provider":"y","usage":{"input":1}}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, meta, err := readPiSessionFile(path)
	if err != nil {
		t.Fatalf("readPiSessionFile: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected zero entries from header-less file, got %d", len(entries))
	}
	if meta.SessionID != "" {
		t.Errorf("expected empty meta, got %+v", meta)
	}
}

func TestReadPiSessionFile_GarbageHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	body := "not json\n" + `{"type":"message","message":{"role":"assistant","usage":{"input":1}}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, _, err := readPiSessionFile(path)
	if err != nil {
		t.Fatalf("readPiSessionFile: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected zero entries from garbage-header file, got %d", len(entries))
	}
}

func TestReadPiSessionFile_RejectsInvalidOmpPrefixes(t *testing.T) {
	title := `{"type":"title","v":1,"title":"project-x"}`
	session := `{"type":"session","id":"omp_ses_001"}`
	assistant := `{"type":"message","message":{"role":"assistant","model":"m","provider":"p","usage":{"input":1}}}`
	tests := []struct {
		name string
		body string
	}{
		{
			name: "unversioned title",
			body: `{"type":"title","title":"project-x"}` + "\n" + session + "\n" + assistant,
		},
		{
			name: "multiple title records",
			body: title + "\n" + title + "\n" + session + "\n" + assistant,
		},
		{
			name: "title without session header",
			body: title + "\n" + assistant,
		},
		{
			name: "arbitrary record before session",
			body: `{"type":"other"}` + "\n" + session + "\n" + assistant,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			entries, meta, err := readPiSessionFile(path)
			if err != nil {
				t.Fatalf("readPiSessionFile: %v", err)
			}
			if len(entries) != 0 || meta.SessionID != "" {
				t.Errorf("invalid prefix was accepted: entries = %d, meta = %+v", len(entries), meta)
			}
		})
	}
}

func TestReadPiSessionFile_FallbackToMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	body := `{"type":"session","id":"pi_ses_002","cwd":"/x"}
{"type":"message","id":"msg_001","message":{"role":"assistant","model":"m","provider":"p","usage":{"input":10,"output":20}}}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, _, err := readPiSessionFile(path)
	if err != nil {
		t.Fatalf("readPiSessionFile: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Timestamp.IsZero() {
		t.Error("expected mtime fallback timestamp, got zero")
	}
}

func TestReadPiSessionFile_AllZeroTokensFiltered(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	body := `{"type":"session","id":"pi_ses_003","cwd":"/x"}
{"type":"message","message":{"role":"assistant","model":"m","provider":"p","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0}}}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, _, err := readPiSessionFile(path)
	if err != nil {
		t.Fatalf("readPiSessionFile: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected zero entries for all-zero usage, got %d", len(entries))
	}
}

func TestReadPiSessionFile_RecordedCostWithoutTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	body := `{"type":"session","id":"omp_ses_cost"}
{"type":"message","message":{"role":"assistant","model":"m","usage":{"input":0,"output":0,"cost":{"total":0.25}}}}
{"type":"message","message":{"role":"assistant","model":"m","usage":{"input":0,"cost":{"total":-1}}}}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, _, err := readPiSessionFile(path)
	if err != nil {
		t.Fatalf("readPiSessionFile: %v", err)
	}
	if len(entries) != 1 || !entries[0].HasCost || entries[0].CostUSD != 0.25 {
		t.Fatalf("recorded cost-only turn = %+v, want one $0.25 turn", entries)
	}
}

func TestWorkspaceLabel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"/", ""},
		{"/home/jane/work/project-x", "project-x"},
		{"/home/jane/work/project-x/", "project-x"},
		{"project-x", "project-x"},
	}
	// workspaceLabel runs filepath.ToSlash, which only rewrites backslashes on
	// Windows. So a Windows-style cwd yields the last segment on Windows but is
	// opaque (no separator → whole string) on Unix.
	if runtime.GOOS == "windows" {
		cases = append(cases, struct{ in, want string }{`C:\Users\jane\proj`, "proj"})
	} else {
		cases = append(cases, struct{ in, want string }{`C:\Users\jane\proj`, `C:\Users\jane\proj`})
	}
	for _, tc := range cases {
		got := workspaceLabel(tc.in)
		if got != tc.want {
			t.Errorf("workspaceLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestReadPiSessionFile_LongLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	big := strings.Repeat("x", 200_000)
	body := `{"type":"session","id":"pi_ses_004","cwd":"/x"}` + "\n" +
		`{"type":"message","message":{"role":"assistant","model":"` + big + `","provider":"p","usage":{"input":1}}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, _, err := readPiSessionFile(path)
	if err != nil {
		t.Fatalf("readPiSessionFile: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
}

func TestReadPiSessionFile_KeepsTurnsAfterLinesOverOneMiB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	toolResult := `{"type":"message","message":{"role":"toolResult","content":"` + strings.Repeat("x", 3<<20) + `"}}`
	body := `{"type":"session","id":"omp_ses_big","cwd":"/x"}` + "\n" + toolResult + "\n" +
		`{"type":"message","id":"a1","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m","usage":{"input":1,"cost":{"total":2.5}}}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, _, err := readPiSessionFile(path)
	if err != nil {
		t.Fatalf("readPiSessionFile: %v", err)
	}
	if len(entries) != 1 || entries[0].CostUSD != 2.5 {
		t.Fatalf("turn after a 3 MiB tool result was lost: %+v", entries)
	}
}

func TestReadPiSessionFile_CountsModelUsageSideCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	// OMP records auto-thinking and judge calls as model_usage entries; their
	// role is "judge", so no "assistant" appears on the line.
	body := `{"type":"session","id":"omp_ses_side","timestamp":"2026-09-23T09:00:00Z","cwd":"/work/kete"}
{"type":"model_usage","id":"54b93071","parentId":"92dbf2a3","timestamp":"2026-09-23T09:31:58.312Z","purpose":"auto-thinking","role":"judge","provider":"openai-codex","model":"gpt-6-luna","usage":{"input":255,"output":25,"cacheRead":7,"cacheWrite":3,"cost":{"total":0.25}}}
{"type":"model_usage","id":"69558f4d","timestamp":"2026-09-23T09:32:50.131Z","provider":"openai-codex","model":"gpt-6-luna"}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, _, err := readPiSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := piModelEntry{
		SessionID: "omp_ses_side", WorkspaceLabel: "kete", Provider: "openai-codex", Model: "gpt-6-luna",
		Input: 255, Output: 25, CacheRead: 7, CacheWrite: 3, CostUSD: 0.25, HasCost: true,
		Timestamp: time.Date(2026, 9, 23, 9, 31, 58, 312_000_000, time.UTC),
		TurnKey:   "54b93071@2026-09-23T09:31:58.312Z",
	}
	if len(entries) != 1 || entries[0] != want {
		t.Fatalf("side calls = %+v, want only the one with usage: %+v", entries, want)
	}
}

func TestReadPiSessionFile_SessionTitle(t *testing.T) {
	header := func(title string) string {
		return `{"type":"session","id":"s","timestamp":"2026-09-23T09:00:00Z"` + title + "}\n"
	}
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			// OMP rewrites the fixed-width first line in place on every retitle;
			// the header keeps the title the session was created with.
			name: "title record is newer than the header",
			body: `{"type":"title","v":1,"title":"Audit statusline","updatedAt":"2026-09-24T15:20:02.126Z","pad":"   "}` + "\n" +
				header(`,"title":"Deploy fixes"`),
			want: "Audit statusline",
		},
		{
			name: "empty title record falls back to the header",
			body: `{"type":"title","v":1,"title":""}` + "\n" + header(`,"title":"Deploy fixes"`),
			want: "Deploy fixes",
		},
		{
			name: "untitled",
			body: header(""),
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, meta, err := readPiSessionFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if meta.SessionID != "s" || meta.Title != tt.want {
				t.Fatalf("session %q title %q, want s titled %q", meta.SessionID, meta.Title, tt.want)
			}
		})
	}
}

func TestReadAllSessions_ResolvesTurnsCopiedFromParents(t *testing.T) {
	turn := func(id, at string, cost string) string {
		return `{"type":"message","id":"` + id + `","timestamp":"` + at + `","message":{"role":"assistant","model":"m","usage":{"input":1,"cost":{"total":` + cost + `}}}}` + "\n"
	}
	parent := `{"type":"session","id":"parent","timestamp":"2026-09-26T04:49:59Z","cwd":"/work/room-for"}` + "\n" +
		turn("a1", "2026-09-26T04:50:00.000Z", "10") + turn("a2", "2026-09-26T05:00:00.000Z", "20")
	// A fork starts with its parent's entries verbatim, then continues.
	fork := `{"type":"session","id":"fork","timestamp":"2026-09-26T08:55:54Z","cwd":"/work/room-for","parentSession":"parent"}` + "\n" +
		turn("a1", "2026-09-26T04:50:00.000Z", "10") + turn("a2", "2026-09-26T05:00:00.000Z", "20") +
		// A copy without an id cannot be matched to its original.
		`{"type":"message","timestamp":"2026-09-26T05:10:00.000Z","message":{"role":"assistant","model":"m","usage":{"input":1,"cost":{"total":7}}}}` + "\n" +
		turn("b1", "2026-09-26T09:00:00.000Z", "5") +
		// Same short id as a parent turn but a different time: a distinct turn.
		turn("a1", "2026-09-26T09:05:00.000Z", "1")
	// /tan clones the session into its artifacts directory, zeroing the
	// copies' cost, then works on in the clone.
	tan := `{"type":"session","id":"tan","timestamp":"2026-09-26T06:00:00Z","cwd":"/work/room-for","parentSession":"parent"}` + "\n" +
		turn("a1", "2026-09-26T04:50:00.000Z", "0") + turn("a2", "2026-09-26T05:00:00.000Z", "0") +
		turn("c1", "2026-09-26T06:10:00.000Z", "3")
	const (
		parentFile = "2026-09-26T04-49-59Z_parent.jsonl"
		tanFile    = "2026-09-26T04-49-59Z_parent/Tan-01a0dd1b.jsonl" // walked before its parent
		forkFile   = "0-fork.jsonl"                                   // walked before the parent
	)

	tests := []struct {
		name  string
		files map[string]string
		want  map[string]float64 // spend by session id
		turns int
	}{
		{
			name:  "fork is counted once, in the parent",
			files: map[string]string{parentFile: parent, forkFile: fork},
			want:  map[string]float64{"parent": 30, "fork": 6},
			turns: 4,
		},
		{
			name:  "tan clone keeps the parent's priced turns",
			files: map[string]string{parentFile: parent, tanFile: tan},
			want:  map[string]float64{"parent": 30, "tan": 3},
			turns: 3,
		},
		{
			name:  "deleted parent leaves the fork's copies",
			files: map[string]string{forkFile: fork},
			want:  map[string]float64{"fork": 36},
			turns: 4,
		},
		{
			name:  "priced copy beats a zeroed one when the parent is gone",
			files: map[string]string{tanFile: tan, forkFile: fork},
			want:  map[string]float64{"fork": 36, "tan": 3},
			turns: 5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for name, body := range tt.files {
				path := filepath.Join(root, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			entries, err := readAllSessions(context.Background(), []string{root})
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]float64{}
			for _, entry := range entries {
				got[entry.SessionID] += entry.CostUSD
			}
			if len(entries) != tt.turns || !maps.Equal(got, tt.want) {
				t.Fatalf("%d turns, spend by session %v; want %d turns, %v", len(entries), got, tt.turns, tt.want)
			}
		})
	}
}
