package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSummarizeOmpHistoryCountsLimitHitsAndDailyPeaks(t *testing.T) {
	zone := time.FixedZone("UTC+2", 2*60*60)
	now := time.Date(2026, 9, 27, 15, 0, 0, 0, zone)
	local := func(day, hour, minute int) int64 {
		return time.Date(2026, 9, day, hour, minute, 0, 0, zone).UnixMilli()
	}
	entry := func(provider, account, limitID, windowLabel string, recordedAt int64, used, status string, resetsAt int64) string {
		return fmt.Sprintf(`{"recordedAt":%d,"provider":%q,"accountKey":%q,"email":"private@example.com","accountId":"acct-id",
			"limitId":%q,"label":"Some label","windowLabel":%q,"usedFraction":%s,"status":%q,"resetsAt":%d}`,
			recordedAt, provider, account, limitID, windowLabel, used, status, resetsAt)
	}
	entries := []string{
		// Claude 5h, account A. Exhausted the evening before the first day,
		// and still in that same window just after midnight: one episode.
		entry("anthropic", "acct-a", "anthropic:5h", "5 Hour", local(24, 23, 0), "1", "exhausted", local(25, 1, 0)),
		entry("anthropic", "acct-a", "anthropic:5h", "5 Hour", local(25, 0, 30), "1", "exhausted", local(25, 1, 0)),
		entry("anthropic", "acct-a", "anthropic:5h", "5 Hour", local(25, 10, 0), "0.3", "ok", local(25, 15, 0)),
		entry("anthropic", "acct-a", "anthropic:5h", "5 Hour", local(25, 12, 0), "1", "exhausted", local(25, 15, 0)), // hit 1
		// No fetch in between, but that window reset at 15:00 the day before.
		entry("anthropic", "acct-a", "anthropic:5h", "5 Hour", local(26, 9, 0), "1.02", "exhausted", local(26, 11, 0)), // hit 2
		// 23:30 UTC on the 26th is 01:30 on the 27th here.
		entry("anthropic", "acct-a", "anthropic:5h", "5 Hour", time.Date(2026, 9, 26, 23, 30, 0, 0, time.UTC).UnixMilli(), "0.5", "ok", local(27, 5, 0)),
		// Account B's first snapshot is already exhausted.
		entry("anthropic", "acct-b", "anthropic:5h", "5 Hour", local(26, 8, 0), "0.97", "exhausted", local(26, 10, 0)), // hit 3
		entry("anthropic", "acct-a", "anthropic:7d:fable", "7 Day", local(26, 12, 0), "0.2", "ok", local(30, 0, 0)),
		// Codex meters put the window key last.
		entry("openai-codex", "acct-c", "openai-codex:spark:primary", "5 hours", local(27, 9, 0), "0.4", "ok", local(27, 12, 0)),
		entry("openai-codex", "acct-c", "openai-codex:primary", "5 hours", local(26, 20, 0), "null", "exhausted", local(26, 22, 0)),
		`{"recordedAt":"yesterday","provider":"anthropic"}`,
	}
	output := []byte(`{"generatedAt":1,"sinceMs":0,"entries":[` + strings.Join(entries, ",") + `]}`)

	history := summarizeOmpHistory(output, nil, 3, now)
	fraction := func(value float64) *float64 { return &value }
	days := func(peaks ...*float64) []ompHistoryDay {
		dates := []string{"2026-09-25", "2026-09-26", "2026-09-27"}
		result := make([]ompHistoryDay, len(dates))
		for index, date := range dates {
			result[index] = ompHistoryDay{Date: date, PeakFraction: peaks[index]}
		}
		return result
	}
	want := ompQuotaHistory{Schema: 3, Days: 3, Providers: []ompHistoryProvider{
		{Key: "chatgpt", Name: "ChatGPT", Windows: []ompHistoryWindow{
			// Exhausted without a recorded fraction: a hit, but no peak.
			{Label: "5h", LimitHits: 1, Daily: days(nil, nil, nil)},
			{Label: "5h Spark", Scoped: true, PeakFraction: fraction(0.4), Daily: days(nil, nil, fraction(0.4))},
		}},
		{Key: "claude", Name: "Claude", Windows: []ompHistoryWindow{
			{Label: "5h", LimitHits: 3, PeakFraction: fraction(1.02), Daily: days(fraction(1), fraction(1.02), fraction(0.5))},
			{Label: "7d Fable", Scoped: true, PeakFraction: fraction(0.2), Daily: days(nil, fraction(0.2), nil)},
		}},
	}}
	if !reflect.DeepEqual(history, want) {
		got, _ := json.Marshal(history)
		expected, _ := json.Marshal(want)
		t.Fatalf("history =\n%s\nwant\n%s", got, expected)
	}
	encoded, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"acct-", "example.com", "Some label"} {
		if strings.Contains(string(encoded), private) {
			t.Errorf("%q escaped into history JSON: %s", private, encoded)
		}
	}
}

func TestSummarizeOmpHistoryReportsUnavailableData(t *testing.T) {
	now := time.Date(2026, 9, 27, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		output     []byte
		commandErr error
		want       string
	}{
		{"missing OMP executable", nil, &exec.Error{Name: "omp", Err: exec.ErrNotFound}, "openusage: quota history unavailable (omp not found)"},
		{"failed OMP command", nil, errors.New("private@example.com"), "openusage: quota history unavailable (omp usage failed)"},
		{"malformed JSON", []byte(`{"entries":[`), nil, "openusage: quota history unavailable (invalid omp JSON)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			history := summarizeOmpHistory(test.output, test.commandErr, 7, now)
			if history.Error != test.want || history.Schema != 3 || history.Days != 7 || history.Providers == nil {
				t.Fatalf("history = %#v", history)
			}
		})
	}
}
