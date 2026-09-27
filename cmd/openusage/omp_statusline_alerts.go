package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The OMP extension passes each fresh quota summary to `omp-statusline
// alerts` with the state that call returned last time, and shows every alert
// it gets back with OMP's ui.notify. Deciding here, in one pure function,
// keeps "once per episode" consistent across polls, resets and restarts.

const ompAlertStateVersion = 1

type ompAlert struct {
	Level   string `json:"level"` // "warning", or "info" for a reset notice
	Message string `json:"message"`
}

// ompAlertState records what earlier evaluations already announced. The
// extension keeps it opaque and hands it back unchanged.
type ompAlertState struct {
	V int `json:"v"`
	// Windows is keyed by "<provider key>/<window label>", e.g. "claude/5h".
	Windows   map[string]ompWindowAlertState   `json:"windows"`
	Providers map[string]ompProviderAlertState `json:"providers"`
}

type ompWindowAlertState struct {
	ResetsAt int64   `json:"resetsAt,omitempty"`
	Used     float64 `json:"used"`
	// Fired is the highest threshold announced in this window.
	Fired     int  `json:"fired,omitempty"`
	Exhausted bool `json:"exhausted,omitempty"` // "limit reached" was announced
	Pace      bool `json:"pace,omitempty"`      // a run-out pace was announced this episode
}

type ompProviderAlertState struct {
	Disabled int `json:"disabled,omitempty"`
	// ExpiryAlerted is the saved-reset expiry (Unix ms) already announced.
	ExpiryAlerted int64 `json:"expiryAlerted,omitempty"`
}

type ompAlertsInput struct {
	Summary ompQuotaSummary `json:"summary"`
	// State stays raw so an unreadable one starts over instead of failing.
	State json.RawMessage `json:"state"`
}

type ompAlertsOutput struct {
	Schema int           `json:"schema"`
	Alerts []ompAlert    `json:"alerts"`
	State  ompAlertState `json:"state"`
}

func newOmpStatuslineAlertsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "alerts",
		Short: "Decide OMP quota alerts from a quota summary and the previous alert state (JSON on stdin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runOmpStatuslineAlerts(cmd.InOrStdin(), cmd.OutOrStdout(), time.Now())
		},
	}
}

func runOmpStatuslineAlerts(input io.Reader, output io.Writer, now time.Time) error {
	config, err := readOmpStatuslineConfig()
	if err != nil {
		return err
	}
	var payload ompAlertsInput
	if err := json.NewDecoder(io.LimitReader(input, 1<<20)).Decode(&payload); err != nil {
		return fmt.Errorf("read OMP alert input: %w", err)
	}
	var previous ompAlertState
	if json.Unmarshal(payload.State, &previous) != nil {
		previous = ompAlertState{} // drop whatever a failed decode filled in
	}
	alerts, state := evaluateOmpAlerts(payload.Summary, previous, config, now)
	return json.NewEncoder(output).Encode(ompAlertsOutput{Schema: ompExtensionSchema, Alerts: alerts, State: state})
}

// evaluateOmpAlerts decides which ChatGPT and Claude quota alerts to show at
// now and returns the state to pass next time. Disabled alerts still advance
// the state, so turning them back on does not replay old news.
func evaluateOmpAlerts(summary ompQuotaSummary, previous ompAlertState, config ompStatuslineConfig, now time.Time) ([]ompAlert, ompAlertState) {
	state := ompAlertState{
		V:         ompAlertStateVersion,
		Windows:   map[string]ompWindowAlertState{},
		Providers: map[string]ompProviderAlertState{},
	}
	if previous.V == ompAlertStateVersion {
		for key, window := range previous.Windows {
			state.Windows[key] = window
		}
		for key, provider := range previous.Providers {
			state.Providers[key] = provider
		}
	}
	alerts := []ompAlert{}
	for _, entry := range ompStatuslineProviders {
		provider := summary.provider(entry.key)
		// Old numbers could announce a limit that has since reset; a later,
		// fresh report decides instead.
		if provider.Key == "" || ompQuotaStale(provider.FetchedAt, now) {
			continue
		}
		flags := state.Providers[entry.key]
		if provider.Disabled > flags.Disabled {
			alerts = append(alerts, ompAlert{"warning", entry.name + " sign-in was disabled — run /login"})
		}
		flags.Disabled = provider.Disabled
		for _, window := range ompStatuslineWindows(provider) {
			key := entry.key + "/" + window.Label
			prior, known := state.Windows[key]
			alert, next, current := evaluateOmpWindowAlert(entry.name, provider, window, prior, known, config.AlertThresholds, now)
			if !current {
				continue
			}
			state.Windows[key] = next
			if alert != nil {
				alerts = append(alerts, *alert)
			}
		}
		if expiresAt := provider.ResetsExpireAt; expiresAt > 0 && expiresAt != flags.ExpiryAlerted {
			if left := time.UnixMilli(expiresAt).Sub(now); left > 0 && left <= 24*time.Hour {
				alerts = append(alerts, ompAlert{"warning", fmt.Sprintf(
					"%s: a saved rate-limit reset expires in %s — use it with /usage reset",
					entry.name, formatOmpWindowDuration(left))})
				flags.ExpiryAlerted = expiresAt
			}
		}
		state.Providers[entry.key] = flags
	}
	if !config.Alerts {
		alerts = []ompAlert{}
	}
	return alerts, state
}

// evaluateOmpWindowAlert decides at most one alert for a window, the most
// severe first: limit reached, a threshold of 90% or more, a run-out pace,
// then a lower threshold. Announcing one marks the lesser ones it implies.
// current is false when the window's reset has passed since OMP measured it,
// so its numbers say nothing about now and its state stays as it was.
func evaluateOmpWindowAlert(
	name string, provider ompProviderQuota, window ompQuotaWindow,
	prior ompWindowAlertState, known bool, thresholds []int, now time.Time,
) (alert *ompAlert, next ompWindowAlertState, current bool) {
	nowMs := now.UnixMilli()
	if window.ResetsAt > 0 && window.ResetsAt <= nowMs {
		return nil, prior, false
	}
	subject := name + " " + window.Label
	used, percent := window.UsedFraction, ompUsedPercent(window.UsedFraction)
	others := ompOtherWindowsNote(provider, window.Label, nowMs)

	next = prior
	if known && ompWindowWasReset(prior, window, nowMs) {
		next = ompWindowAlertState{ResetsAt: window.ResetsAt, Used: used}
		if prior.Exhausted {
			return &ompAlert{"info", subject + " limit has reset · available again" + others}, next, true
		}
	}
	next.ResetsAt, next.Used = window.ResetsAt, used

	exhausted := used >= 1 || window.Status == "exhausted"
	runsOut := ompRunsOutBeforeReset(window)
	// Re-arm what usage has clearly fallen back from, so it can alert again.
	if next.Fired > 0 && percent < next.Fired-10 {
		next.Fired = ompThresholdAtOrBelow(thresholds, percent)
	}
	if next.Exhausted && !exhausted && percent < 90 {
		next.Exhausted = false
	}
	if next.Pace && !runsOut {
		next.Pace = false
	}

	threshold, highest := 0, 0 // the highest threshold reached but not announced
	for _, candidate := range thresholds {
		highest = max(highest, candidate)
		if percent >= candidate && candidate > next.Fired {
			threshold = max(threshold, candidate)
		}
	}
	thresholdAlert := func() *ompAlert {
		message := fmt.Sprintf("%s at %d%% used", subject, percent) + ompResetsNote(window, now)
		if runsOut {
			message += " · at this pace it runs out in ~" + formatOmpWindowDuration(time.UnixMilli(window.ExhaustsAt).Sub(now))
			next.Pace = true
		}
		next.Fired = threshold
		return &ompAlert{"warning", message}
	}
	switch {
	case exhausted:
		if next.Exhausted {
			break
		}
		message := subject + " limit reached" + ompResetsNote(window, now) + others
		if resets := provider.UsableResets; resets == 1 {
			message += " · 1 saved reset: /usage reset"
		} else if resets > 1 {
			message += fmt.Sprintf(" · %d saved resets: /usage reset", resets)
		}
		alert = &ompAlert{"warning", message}
		next.Exhausted = true
		next.Fired = max(next.Fired, highest)
	case threshold >= 90:
		alert = thresholdAlert()
	case runsOut && used >= 0.5 && !next.Pace:
		alert = &ompAlert{"warning", fmt.Sprintf(
			"%s at %d%% used · at this pace it runs out in ~%s (%s), before the reset at %s",
			subject, percent, formatOmpWindowDuration(time.UnixMilli(window.ExhaustsAt).Sub(now)),
			ompAlertClock(window.ExhaustsAt, now), ompAlertClock(window.ResetsAt, now))}
		next.Pace = true
		next.Fired = max(next.Fired, threshold)
	case threshold > 0 && ompAheadOfEvenPace(window, now):
		alert = thresholdAlert()
	}
	return alert, next, true
}

// ompWindowWasReset reports a window that restarted since the prior
// evaluation: its reset moved on by half a window or more, or the prior
// reset time has passed and usage fell.
func ompWindowWasReset(prior ompWindowAlertState, window ompQuotaWindow, nowMs int64) bool {
	if prior.ResetsAt > 0 && window.ResetsAt > 0 && window.DurationMs > 0 &&
		window.ResetsAt-prior.ResetsAt >= window.DurationMs/2 {
		return true
	}
	return prior.ResetsAt > 0 && nowMs >= prior.ResetsAt && window.UsedFraction < prior.Used
}

// ompAheadOfEvenPace reports usage above the share of the window already
// gone. A threshold under 90% is only worth a notification then: 75% used
// with a day of the week left is on course. Unknown timing counts as ahead.
func ompAheadOfEvenPace(window ompQuotaWindow, now time.Time) bool {
	if window.ResetsAt <= 0 || window.DurationMs <= 0 {
		return true
	}
	elapsed := 1 - float64(window.ResetsAt-now.UnixMilli())/float64(window.DurationMs)
	return window.UsedFraction > elapsed
}

func ompThresholdAtOrBelow(thresholds []int, percent int) int {
	found := 0
	for _, threshold := range thresholds {
		if threshold <= percent {
			found = max(found, threshold)
		}
	}
	return found
}

// ompOtherWindowsNote lists the provider's other shared windows
// (" · 7d at 94% used"): they decide whether a reset frees anything up.
func ompOtherWindowsNote(provider ompProviderQuota, label string, nowMs int64) string {
	var note strings.Builder
	for _, other := range provider.Windows {
		if other.Scoped || other.Label == label || (other.ResetsAt > 0 && other.ResetsAt <= nowMs) {
			continue
		}
		fmt.Fprintf(&note, " · %s at %d%% used", other.Label, ompUsedPercent(other.UsedFraction))
	}
	return note.String()
}

// ompResetsNote is " · resets 15:04 (in 1h13m)", or "" without a reset time.
func ompResetsNote(window ompQuotaWindow, now time.Time) string {
	if window.ResetsAt <= 0 {
		return ""
	}
	return fmt.Sprintf(" · resets %s (in %s)", ompAlertClock(window.ResetsAt, now),
		formatOmpWindowDuration(time.UnixMilli(window.ResetsAt).Sub(now)))
}

// ompAlertClock names a local time: "15:04" today, else "Mon 15:04".
func ompAlertClock(ms int64, now time.Time) string {
	at := time.UnixMilli(ms).In(now.Location())
	atYear, atMonth, atDay := at.Date()
	year, month, day := now.Date()
	if atYear == year && atMonth == month && atDay == day {
		return at.Format("15:04")
	}
	return at.Format("Mon 15:04")
}
