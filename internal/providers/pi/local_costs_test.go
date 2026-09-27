package pi

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLocalCostSummaryWithholdsUnpricedBlock(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	root := filepath.Join(home, ".omp", "agent", "sessions")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "session.jsonl")
	priced := `{"type":"session","id":"omp-1"}
{"type":"message","timestamp":"2026-01-02T10:00:00Z","message":{"role":"assistant","model":"gpt","usage":{"input":100,"cost":{"total":1}}}}
{"type":"message","timestamp":"2026-01-02T11:00:00Z","message":{"role":"assistant","model":"claude","usage":{"input":200,"cost":{"total":2}}}}
`
	if err := os.WriteFile(path, []byte(priced), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	costs, err := LocalCostSummary(context.Background(), now, LocalCostOptions{})
	if err != nil || !costs.HasBlock {
		t.Fatalf("active block = %+v, error %v", costs, err)
	}
	if costs.Block.Cost != 3 || costs.Block.TimeRemaining != 3*time.Hour || costs.Block.BurnRateUSDPerHour != 3 {
		t.Fatalf("wrong active block: cost %v remaining %v burn %v",
			costs.Block.Cost, costs.Block.TimeRemaining, costs.Block.BurnRateUSDPerHour)
	}
	if !costs.TodayKnown || costs.Today != 3 {
		t.Fatalf("today = %v known %v, want 3", costs.Today, costs.TodayKnown)
	}

	unpriced := `{"type":"message","timestamp":"2026-01-02T11:30:00Z","message":{"role":"assistant","model":"gpt","usage":{"input":100}}}
`
	if err := os.WriteFile(path, []byte(priced+unpriced), 0o600); err != nil {
		t.Fatal(err)
	}
	costs, err = LocalCostSummary(context.Background(), now, LocalCostOptions{})
	if err != nil || costs.HasBlock || costs.TodayKnown {
		t.Fatalf("a partial sum must not be shown as the block or day total: %+v, error %v", costs, err)
	}
}

func TestLocalCostSummaryBucketsByLocalCalendarDay(t *testing.T) {
	auckland := time.FixedZone("NZDT", 13*60*60)
	now := time.Date(2026, 9, 27, 22, 0, 0, 0, auckland) // 09:00 UTC
	entry := func(utc string, provider, model, project string, cost float64) piModelEntry {
		at, err := time.Parse(time.RFC3339, utc)
		if err != nil {
			t.Fatal(err)
		}
		return piModelEntry{
			Timestamp: at, Provider: provider, Model: model, WorkspaceLabel: project,
			Input: 10, CostUSD: cost, HasCost: true,
		}
	}
	entries := []piModelEntry{
		// 00:30 local on the 27th, still the 26th in UTC.
		entry("2026-09-26T11:30:00Z", "anthropic", "opus", "room-for", 4),
		// 23:30 local on the 26th.
		entry("2026-09-26T10:30:00Z", "openai-codex", "gpt", "kete", 2),
		// Eight local days ago: history-only, outside the seven-day totals.
		entry("2026-09-19T01:00:00Z", "anthropic", "opus", "kete", 50),
		entry("2026-09-27T08:00:00Z", "openai-codex", "gpt", "room-for", 1),
	}

	costs := summarizeLocalCosts(entries, now, LocalCostOptions{HistoryDays: 9})
	if !costs.TodayKnown || costs.Today != 5 {
		t.Fatalf("today = %v known %v, want the two turns after local midnight ($5)", costs.Today, costs.TodayKnown)
	}
	if len(costs.Days) != 9 || costs.Days[0].Date != "2026-09-19" || costs.Days[8].Date != "2026-09-27" {
		t.Fatalf("days = %+v, want 9 local days ending today", costs.Days)
	}
	if got := costs.Days[0]; got.CostUSD != 50 || got.Turns != 1 {
		t.Fatalf("oldest day = %+v", got)
	}
	if got := costs.Days[7]; got.Date != "2026-09-26" || got.CostUSD != 2 {
		t.Fatalf("yesterday = %+v, want only the 23:30 local turn", got)
	}
	if len(costs.Hours) != 24 || costs.Hours[0] != 4 || costs.Hours[21] != 1 {
		t.Fatalf("hours = %v, want $4 at 00:00 and $1 at 21:00 local", costs.Hours)
	}
	if len(costs.Models) != 2 ||
		costs.Models[0] != (CostShare{Provider: "anthropic", Name: "opus", TodayUSD: 4, WeekUSD: 4}) ||
		costs.Models[1] != (CostShare{Provider: "openai-codex", Name: "gpt", TodayUSD: 1, WeekUSD: 3}) {
		t.Fatalf("models = %+v", costs.Models)
	}
	if len(costs.Projects) != 2 ||
		costs.Projects[0] != (CostShare{Name: "room-for", TodayUSD: 5, WeekUSD: 5}) ||
		costs.Projects[1] != (CostShare{Name: "kete", WeekUSD: 2}) {
		t.Fatalf("projects = %+v", costs.Projects)
	}
}

func TestSummarizeLocalCostsLastHourEndsNow(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		at   time.Time
		want float64
	}{
		{name: "exactly an hour ago", at: now.Add(-time.Hour), want: 0},
		{name: "just inside the hour", at: now.Add(-time.Hour + time.Second), want: 2},
		{name: "now", at: now, want: 2},
		{name: "after now", at: now.Add(time.Second), want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := []piModelEntry{{Timestamp: tt.at, Input: 1, CostUSD: 2, HasCost: true}}
			if got := summarizeLocalCosts(entries, now, LocalCostOptions{}).LastHour; got != tt.want {
				t.Fatalf("last hour = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSummarizeLocalCostsWindowSpend(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	windowStart := now.Add(-5 * time.Hour)
	turn := func(at time.Time, provider string, cost float64) piModelEntry {
		return piModelEntry{Timestamp: at, Provider: provider, Model: "m", Input: 1, CostUSD: cost, HasCost: cost > 0}
	}
	entries := []piModelEntry{
		turn(windowStart.Add(-time.Second), "anthropic", 32),
		turn(windowStart, "anthropic", 1),
		turn(now.Add(-2*time.Hour), "openai-codex", 4),
		turn(now.Add(-30*time.Minute), "anthropic", 0), // unpriced, still a turn
		turn(now.Add(-10*time.Minute), "anthropic", 8),
	}
	provider := func(want string) func(string) bool {
		return func(got string) bool { return got == want }
	}
	windows := []SpendWindow{
		{Key: "claude/5h", Since: windowStart, Match: provider("anthropic")},
		{Key: "gemini/5h", Since: windowStart, Match: provider("google")},
		{Key: "chatgpt/7d", Since: now.AddDate(0, 0, -7), Match: provider("openai-codex")},
		{Key: "any/1h", Since: now.Add(-time.Hour)},
	}
	got := summarizeLocalCosts(entries, now, LocalCostOptions{Windows: windows}).Windows
	want := []WindowSpend{
		{Key: "claude/5h", CostUSD: 9, Turns: 3},
		{Key: "gemini/5h"},
		{Key: "chatgpt/7d", CostUSD: 4, Turns: 1},
		{Key: "any/1h", CostUSD: 8, Turns: 2},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("windows = %+v, want %+v", got, want)
	}
}

func TestSummarizeLocalCostsTokensAndMonthFollowTheLocalCalendar(t *testing.T) {
	pacific := time.FixedZone("PDT", -7*60*60)
	local := func(month time.Month, day, hour, minute int) time.Time {
		return time.Date(2026, month, day, hour, minute, 0, 0, pacific)
	}
	turn := func(at time.Time, tokens int64, cost float64) piModelEntry {
		return piModelEntry{
			Timestamp: at.UTC(), Input: tokens, Output: 2 * tokens, CacheRead: 3 * tokens, CacheWrite: 4 * tokens,
			CostUSD: cost, HasCost: true,
		}
	}
	monthEdges := []piModelEntry{
		turn(local(8, 31, 23, 0), 0, 16), // August locally, already September in UTC
		turn(local(9, 1, 0, 0), 0, 32),
	}

	t.Run("tokens", func(t *testing.T) {
		entries := []piModelEntry{
			turn(local(9, 27, 0, 10), 1, 1),
			turn(local(9, 26, 23, 50), 10, 2), // yesterday locally, today in UTC
			turn(local(9, 21, 0, 0), 100, 4),  // first of the seven days
			turn(local(9, 20, 23, 59), 1000, 8),
		}
		costs := summarizeLocalCosts(entries, local(9, 27, 9, 0), LocalCostOptions{HistoryDays: 1})
		if want := (TokenTotals{1, 2, 3, 4}); costs.TodayTokens != want {
			t.Errorf("today tokens = %+v, want %+v", costs.TodayTokens, want)
		}
		if want := (TokenTotals{111, 222, 333, 444}); costs.WeekTokens != want {
			t.Errorf("week tokens = %+v, want %+v", costs.WeekTokens, want)
		}
	})

	tests := []struct {
		name          string
		now           time.Time
		wantMonth     float64
		wantProjected float64
	}{
		{name: "under two days in", now: local(9, 2, 23, 59), wantMonth: 32, wantProjected: 0},
		// 48 of September's 720 hours have passed.
		{name: "two days in", now: local(9, 3, 0, 0), wantMonth: 32, wantProjected: 480},
		{name: "next month", now: local(10, 5, 0, 0), wantMonth: 0, wantProjected: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			costs := summarizeLocalCosts(monthEdges, tt.now, LocalCostOptions{HistoryDays: 1})
			if costs.Month != tt.wantMonth || costs.MonthProjected != tt.wantProjected {
				t.Fatalf("month = %v projected %v, want %v projected %v",
					costs.Month, costs.MonthProjected, tt.wantMonth, tt.wantProjected)
			}
		})
	}
}

func TestLocalCostSessionsRollUpSubagentAndTanTranscripts(t *testing.T) {
	root := t.TempDir()
	write := func(name string, lines ...string) {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	header := func(id, at, extra string) string {
		return `{"type":"session","id":"` + id + `","timestamp":"` + at + `","cwd":"/dev/kete"` + extra + `}`
	}
	turn := func(id, at, cost string) string {
		return `{"type":"message","id":"` + id + `","timestamp":"` + at + `","message":{"role":"assistant","model":"m","usage":{"input":1,"cost":{"total":` + cost + `}}}}`
	}
	write("S1.jsonl",
		`{"type":"title","v":1,"title":"Audit statusline"}`,
		header("s1", "2026-09-19T08:59:00Z", `,"title":"Old title"`),
		turn("t2", "2026-09-19T09:00:00Z", "50"), // eight days ago
		turn("t1", "2026-09-27T09:00:00Z", "4"))
	write("S1/Explore.jsonl",
		header("sub", "2026-09-27T09:10:00Z", `,"parentSession":"s1"`),
		turn("u1", "2026-09-27T09:20:00Z", "2"))
	write("S1/Explore/Explore.Deep.jsonl",
		header("subsub", "2026-09-26T10:00:00Z", `,"parentSession":"sub"`),
		turn("v1", "2026-09-26T10:05:00Z", "1"))
	write("S1/Tan-9.jsonl",
		header("tan", "2026-09-27T09:30:00Z", `,"parentSession":"s1"`),
		turn("t2", "2026-09-19T09:00:00Z", "0"),
		turn("t1", "2026-09-27T09:00:00Z", "0"),
		turn("w1", "2026-09-27T09:40:00Z", "3"))
	write("S2.jsonl",
		`{"type":"session","id":"s2","timestamp":"2026-09-24T00:00:00Z","cwd":"/dev/room-for","title":"Pi title"}`,
		turn("x1", "2026-09-24T01:00:00Z", "8"))
	write("S3.jsonl", header("s3", "2026-09-27T07:00:00Z", ""), turn("y1", "2026-09-27T08:00:00Z", "0"))
	write("S4.jsonl", header("s4", "2026-09-27T07:00:00Z", ""), turn("z1", "2026-09-27T08:00:00Z", "1"))

	entries, err := readAllSessions(context.Background(), []string{root})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	got := summarizeLocalCosts(entries, now, LocalCostOptions{HistoryDays: 1, TopSessions: 2}).Sessions
	want := []SessionCost{
		{
			ID: "s1", Title: "Audit statusline", Project: "kete", CostUSD: 10, TodayUSD: 9, SubagentUSD: 6, Turns: 4,
			LastActivity: time.Date(2026, 9, 27, 9, 40, 0, 0, time.UTC),
		},
		{
			ID: "s2", Title: "Pi title", Project: "room-for", CostUSD: 8, Turns: 1,
			LastActivity: time.Date(2026, 9, 24, 1, 0, 0, 0, time.UTC),
		},
	}
	if !slices.EqualFunc(got, want, func(a, b SessionCost) bool {
		lastA, lastB := a.LastActivity, b.LastActivity
		a.LastActivity, b.LastActivity = time.Time{}, time.Time{}
		return a == b && lastA.Equal(lastB)
	}) {
		t.Fatalf("sessions = %+v\nwant %+v", got, want)
	}
}
