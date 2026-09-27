package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/janekbaraniewski/openusage/internal/providers/pi"
	"github.com/janekbaraniewski/openusage/internal/report"
)

func TestParseOmpSpendWindows(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		windows, err := parseOmpSpendWindows([]string{"claude/7d Opus=1790502803386", "chatgpt/5h=0"})
		if err != nil {
			t.Fatal(err)
		}
		if len(windows) != 2 ||
			windows[0].Key != "claude/7d Opus" || !windows[0].Since.Equal(time.UnixMilli(1790502803386)) ||
			windows[1].Key != "chatgpt/5h" || !windows[1].Since.Equal(time.UnixMilli(0)) {
			t.Fatalf("windows = %+v", windows)
		}
		for _, check := range []struct {
			window   int
			provider string
			want     bool
		}{
			{0, "anthropic", true},
			{0, "openai-codex", false},
			{1, "openai-codex", true},
			{1, "codex", true},
			{1, "openrouter", false},
		} {
			if got := windows[check.window].Match(check.provider); got != check.want {
				t.Errorf("%s bills against %s = %v, want %v", check.provider, windows[check.window].Key, got, check.want)
			}
		}
	})

	for _, value := range []string{
		"claude/5h",                 // no start
		"claude=1790502803386",      // no label
		"/5h=1790502803386",         // no subscription
		"claude/ =1790502803386",    // blank label
		"claude/5h=soon",            // not a number
		"claude/5h=-1",              // before the epoch
		"claude/5h=1790502803386.5", // not whole milliseconds
	} {
		t.Run("malformed "+value, func(t *testing.T) {
			if windows, err := parseOmpSpendWindows([]string{value}); err == nil {
				t.Fatalf("accepted %q as %+v", value, windows)
			}
		})
	}

	t.Run("too many", func(t *testing.T) {
		values := make([]string, ompMaxSpendWindows+1)
		for i := range values {
			values[i] = "claude/5h=1"
		}
		if _, err := parseOmpSpendWindows(values[:ompMaxSpendWindows]); err != nil {
			t.Fatalf("the maximum was rejected: %v", err)
		}
		if _, err := parseOmpSpendWindows(values); err == nil {
			t.Fatal("more than the maximum was accepted")
		}
	})
}

func TestOmpCostsCommandEchoesEveryWindow(t *testing.T) {
	// An empty home: no sessions to read and no parse cache to touch.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LocalAppData", filepath.Join(home, "cache"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))

	command := newOmpStatuslineCostsCommand()
	var out bytes.Buffer
	command.SetOut(&out)
	// A comma inside a label must not split the value into two windows.
	command.SetArgs([]string{"--window", "claude/7d Opus, Sonnet=1790502803386", "--window", "chatgpt/5h=1790502803386"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var costs struct {
		Windows []ompWindowSpend `json:"windows"`
	}
	if err := json.Unmarshal(out.Bytes(), &costs); err != nil {
		t.Fatalf("%v in %s", err, out.String())
	}
	want := []ompWindowSpend{{Key: "claude/7d Opus, Sonnet"}, {Key: "chatgpt/5h"}}
	if len(costs.Windows) != len(want) || costs.Windows[0] != want[0] || costs.Windows[1] != want[1] {
		t.Fatalf("windows = %+v, want %+v even without spend", costs.Windows, want)
	}
}

func TestOmpCostsFromLocal(t *testing.T) {
	start := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	local := pi.LocalCosts{
		Block: report.Row{
			Cost: 4, Start: start, End: start.Add(5 * time.Hour), FirstActivity: start, LastActivity: start,
			ProjectedCost: 4, // the report's projection for a block without a rate
		},
		HasBlock: true,
		LastHour: 1.5,
		Windows:  []pi.WindowSpend{{Key: "claude/5h"}},
		Days:     []pi.DayCost{{Date: "2026-09-27", CostUSD: 4, Turns: 2}},
		Hours:    make([]float64, 24),
		Models:   []pi.CostShare{{Provider: "anthropic", Name: "claude-opus-5-5", TodayUSD: 4, WeekUSD: 9}},
		Sessions: []pi.SessionCost{
			{
				ID: "01a0dc0c", Title: "Fix\x1b[31m the\nstatusline \u202e" + strings.Repeat("long ", 20),
				Project: "openusage-omp", CostUSD: 9, TodayUSD: 4, SubagentUSD: 3, Turns: 7, LastActivity: start,
			},
			{ID: "01a0dd1b", CostUSD: 1, Turns: 1, LastActivity: start},
		},
		TodayTokens: pi.TokenTotals{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4},
	}
	historyKeys := []string{"days", "hours", "models", "projects", "sessions", "tokens", "monthUSD"}
	decode := func(t *testing.T, costs ompCosts) map[string]json.RawMessage {
		t.Helper()
		data, err := json.Marshal(costs)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		return fields
	}

	t.Run("without history", func(t *testing.T) {
		fields := decode(t, ompCostsFromLocal(local, false))
		for _, key := range historyKeys {
			if _, ok := fields[key]; ok {
				t.Errorf("%s present without --days", key)
			}
		}
		if _, ok := fields["todayUSD"]; ok {
			t.Error("todayUSD present although today is not known")
		}
		if string(fields["lastHourUSD"]) != "1.5" || string(fields["windows"]) != `[{"key":"claude/5h","costUSD":0,"turns":0}]` {
			t.Errorf("lastHourUSD %s windows %s", fields["lastHourUSD"], fields["windows"])
		}
		var block ompBlockDetails
		if err := json.Unmarshal(fields["block"], &block); err != nil || block.CostUSD != 4 || block.ProjectedUSD != 0 {
			t.Errorf("block %s: a block without a rate has no projection", fields["block"])
		}
	})

	t.Run("with history", func(t *testing.T) {
		costs := ompCostsFromLocal(local, true)
		fields := decode(t, costs)
		for _, key := range historyKeys {
			if raw, ok := fields[key]; !ok || string(raw) == "null" {
				t.Errorf("%s = %s with --days", key, raw)
			}
		}
		if _, ok := fields["monthProjectedUSD"]; ok {
			t.Error("monthProjectedUSD present although 0")
		}
		if string(fields["projects"]) != "[]" {
			t.Errorf("projects = %s, want an empty list", fields["projects"])
		}
		if got := costs.Models[0]; got.Subscription != "claude" || got.Name != "claude-opus-5-5" {
			t.Errorf("model = %+v", got)
		}
		first, second := costs.Sessions[0], costs.Sessions[1]
		if !strings.HasPrefix(first.Title, "Fix [31m the statusline long long") ||
			utf8.RuneCountInString(first.Title) > ompCostTextRunes || !strings.HasSuffix(first.Title, "…") {
			t.Errorf("title %q: want printable, collapsed, cut to at most %d runes", first.Title, ompCostTextRunes)
		}
		if first.SubagentUSD != 3 || first.LastActivityAt != start.UnixMilli() {
			t.Errorf("session = %+v", first)
		}
		if second.Title != "" || second.Project != "(no project)" {
			t.Errorf("untitled session = %+v, want an empty title and a project placeholder", second)
		}
	})
}
