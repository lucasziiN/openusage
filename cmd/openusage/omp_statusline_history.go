package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type ompHistoryResponse struct {
	// Entries decode one by one so a single odd snapshot cannot hide the rest.
	Entries []json.RawMessage `json:"entries"`
}

// ompHistoryEntry is one recorded snapshot of one account's limit. OMP also
// records emails and account ids; they are not decoded, and the account key
// only tells snapshot series apart. It is never emitted.
type ompHistoryEntry struct {
	RecordedAt   float64  `json:"recordedAt"`
	Provider     string   `json:"provider"`
	AccountKey   string   `json:"accountKey"`
	LimitID      string   `json:"limitId"`
	WindowLabel  string   `json:"windowLabel"`
	UsedFraction *float64 `json:"usedFraction"`
	Status       string   `json:"status"`
	ResetsAt     float64  `json:"resetsAt"`
}

// ompQuotaHistory is the display-safe history JSON for the extension.
type ompQuotaHistory struct {
	Schema    int                  `json:"schema"`
	Days      int                  `json:"days"`
	Providers []ompHistoryProvider `json:"providers"`
	Error     string               `json:"error,omitempty"`
}

type ompHistoryProvider struct {
	Key     string             `json:"key"`
	Name    string             `json:"name"`
	Windows []ompHistoryWindow `json:"windows"`
}

type ompHistoryWindow struct {
	Label  string `json:"label"`
	Scoped bool   `json:"scoped"`
	// LimitHits counts the times an account ran into this limit.
	LimitHits    int             `json:"limitHits"`
	PeakFraction *float64        `json:"peakFraction,omitempty"`
	Daily        []ompHistoryDay `json:"daily"` // one per requested day, oldest first
}

type ompHistoryDay struct {
	Date string `json:"date"` // local YYYY-MM-DD
	// PeakFraction is absent on a day without a recorded fraction.
	PeakFraction *float64 `json:"peakFraction,omitempty"`
}

func newOmpStatuslineHistoryCommand() *cobra.Command {
	var days int
	command := &cobra.Command{
		Use:   "history",
		Short: "Summarize OMP's recorded quota history (limit hits, daily peaks) as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if days < 1 || days > 366 {
				return fmt.Errorf("--days must be between 1 and 366")
			}
			output, err := runOmp(context.Background(),
				"usage", "--history", "--days", strconv.Itoa(days), "--json", "--redact")
			return json.NewEncoder(cmd.OutOrStdout()).Encode(summarizeOmpHistory(output, err, days, time.Now()))
		},
	}
	command.Flags().IntVar(&days, "days", 7, "local days to summarize, today included")
	return command
}

// summarizeOmpHistory groups OMP's usage snapshots by subscription and window
// label over the last days local days, today included.
func summarizeOmpHistory(output []byte, commandErr error, days int, now time.Time) ompQuotaHistory {
	history := ompQuotaHistory{Schema: ompExtensionSchema, Days: days, Providers: []ompHistoryProvider{}}
	if commandErr != nil {
		history.Error = "openusage: quota history unavailable (" + ompCommandFailure(commandErr) + ")"
		return history
	}
	var response ompHistoryResponse
	if err := json.Unmarshal(output, &response); err != nil {
		history.Error = "openusage: quota history unavailable (invalid omp JSON)"
		return history
	}
	entries := make([]ompHistoryEntry, 0, len(response.Entries))
	for _, raw := range response.Entries {
		var entry ompHistoryEntry
		if json.Unmarshal(raw, &entry) == nil && ompMillis(entry.RecordedAt) > 0 {
			entries = append(entries, entry)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].RecordedAt < entries[j].RecordedAt })

	dates := make([]string, days)
	dayIndex := make(map[string]int, days)
	year, month, day := now.Date()
	for index := range dates {
		dates[index] = time.Date(year, month, day-(days-1-index), 0, 0, 0, 0, now.Location()).Format(time.DateOnly)
		dayIndex[dates[index]] = index
	}

	type windowTotals struct {
		label  string
		span   time.Duration
		scoped bool
		hits   int
		peak   float64   // -1 until a snapshot has a fraction
		daily  []float64 // -1 on days without one
	}
	type providerTotals struct {
		key, name string
		windows   map[string]*windowTotals
	}
	type seriesPoint struct {
		exhausted bool
		resetsAt  int64
	}
	providers := map[string]*providerTotals{}
	series := map[[2]string]seriesPoint{}
	for _, entry := range entries {
		recordedAt := ompMillis(entry.RecordedAt)
		used := -1.0
		if entry.UsedFraction != nil && ompNonNegative(*entry.UsedFraction) {
			used = *entry.UsedFraction
		}
		exhausted := used >= 1 || entry.Status == "exhausted"
		// A hit starts an episode at the limit: the series' first snapshot, one
		// after a snapshot below the limit, or one after an exhausted window
		// that had reset by then. Snapshots before the first day only set up
		// this comparison.
		seriesKey := [2]string{entry.AccountKey, entry.LimitID}
		previous, seen := series[seriesKey]
		series[seriesKey] = seriesPoint{exhausted: exhausted, resetsAt: ompMillis(entry.ResetsAt)}
		hit := exhausted && (!seen || !previous.exhausted || (previous.resetsAt > 0 && previous.resetsAt <= recordedAt))

		index, inRange := dayIndex[time.UnixMilli(recordedAt).In(now.Location()).Format(time.DateOnly)]
		key, name := ompProviderIdentity(entry.Provider)
		if !inRange || key == "" {
			continue
		}
		provider, ok := providers[key]
		if !ok {
			provider = &providerTotals{key: key, name: name, windows: map[string]*windowTotals{}}
			providers[key] = provider
		}
		label, span, scoped := ompHistoryLabel(entry.LimitID, entry.WindowLabel)
		window, ok := provider.windows[label]
		if !ok {
			window = &windowTotals{label: label, span: span, scoped: scoped, peak: -1, daily: make([]float64, days)}
			for slot := range window.daily {
				window.daily[slot] = -1
			}
			provider.windows[label] = window
		}
		if hit {
			window.hits++
		}
		if used >= 0 {
			window.peak = max(window.peak, used)
			window.daily[index] = max(window.daily[index], used)
		}
	}

	for _, provider := range providers {
		windows := make([]*windowTotals, 0, len(provider.windows))
		for _, window := range provider.windows {
			windows = append(windows, window)
		}
		sort.Slice(windows, func(i, j int) bool {
			left, right := windows[i], windows[j]
			if left.span != right.span {
				return left.span < right.span
			}
			if left.scoped != right.scoped {
				return !left.scoped
			}
			return left.label < right.label
		})
		summary := ompHistoryProvider{Key: provider.key, Name: provider.name, Windows: []ompHistoryWindow{}}
		for _, window := range windows {
			row := ompHistoryWindow{
				Label: window.label, Scoped: window.scoped, LimitHits: window.hits,
				PeakFraction: ompOptionalFraction(window.peak), Daily: make([]ompHistoryDay, days),
			}
			for index, date := range dates {
				row.Daily[index] = ompHistoryDay{Date: date, PeakFraction: ompOptionalFraction(window.daily[index])}
			}
			summary.Windows = append(summary.Windows, row)
		}
		history.Providers = append(history.Providers, summary)
	}
	sort.Slice(history.Providers, func(i, j int) bool {
		left, right := ompProviderRank(history.Providers[i].Key), ompProviderRank(history.Providers[j].Key)
		if left != right {
			return left < right
		}
		return history.Providers[i].Name < history.Providers[j].Name
	})
	return history
}

// ompOptionalFraction is nil for the "no fraction recorded" sentinel -1.
func ompOptionalFraction(value float64) *float64 {
	if value < 0 {
		return nil
	}
	return &value
}

// ompHistoryLabel names a recorded limit the way the live summary does ("5h",
// "7d Fable"), from its window label first. Limit ids read
// "<provider>:<window>", or "<provider>:<window>:<scope>" for a scoped bucket,
// except Codex meters, which put the window key last:
// "openai-codex:spark:primary" is the Spark meter.
func ompHistoryLabel(limitID, windowLabel string) (label string, span time.Duration, scoped bool) {
	parts := strings.Split(limitID, ":")
	windowPart, scope := "", ""
	if len(parts) >= 2 {
		windowPart = parts[1]
	}
	if len(parts) >= 3 {
		scope = parts[2]
		if key := strings.ToLower(parts[2]); key == "primary" || key == "secondary" {
			windowPart, scope = parts[2], parts[1]
		}
	}
	span = ompTextSpan(windowLabel)
	if span == 0 {
		span = ompTextSpan(windowPart)
	}
	label = safeOmpLabel(windowLabel, safeOmpLabel(windowPart, "quota"))
	if span > 0 {
		label = formatOmpWindowSpan(span)
	}
	if title := ompTitle(scope); title != "" {
		return label + " " + title, span, true
	}
	return label, span, false
}
