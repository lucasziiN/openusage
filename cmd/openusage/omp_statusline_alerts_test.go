package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEvaluateOmpAlertsAcrossTransitions(t *testing.T) {
	base := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC) // a Sunday
	at := func(offset time.Duration) int64 { return base.Add(offset).UnixMilli() }
	window := func(label string, span time.Duration, used float64, resetsIn time.Duration) ompQuotaWindow {
		return ompQuotaWindow{Label: label, DurationMs: span.Milliseconds(), UsedFraction: used,
			ResetsAt: at(resetsIn), Status: ompLimitStatus("", used), Accounts: 1}
	}
	fiveHour := func(used float64, resetsIn time.Duration) ompQuotaWindow {
		return window("5h", 5*time.Hour, used, resetsIn)
	}
	week := func(used float64, resetsIn time.Duration) ompQuotaWindow {
		return window("7d", 7*24*time.Hour, used, resetsIn)
	}
	// runsOut adds the pace OMP measured: out at exhaustsIn, before the reset.
	runsOut := func(window ompQuotaWindow, exhaustsIn time.Duration) ompQuotaWindow {
		window.ExhaustsAt, window.ProjectedFraction = at(exhaustsIn), 1.5
		return window
	}
	summary := func(providers ...ompProviderQuota) ompQuotaSummary {
		return ompQuotaSummary{Schema: ompExtensionSchema, Providers: providers}
	}
	claude := func(fetchedAt time.Duration, windows ...ompQuotaWindow) ompProviderQuota {
		return ompProviderQuota{Key: "claude", Name: "Claude", Accounts: 1, FetchedAt: at(fetchedAt), Windows: windows}
	}
	chatgpt := func(fetchedAt time.Duration, windows ...ompQuotaWindow) ompProviderQuota {
		return ompProviderQuota{Key: "chatgpt", Name: "ChatGPT", Accounts: 1, FetchedAt: at(fetchedAt),
			UsableResets: 1, Windows: windows}
	}
	withExpiry := func(provider ompProviderQuota, expiresIn time.Duration) ompProviderQuota {
		provider.ResetsExpireAt = at(expiresIn)
		return provider
	}
	disabled := func(count int) ompProviderQuota {
		return ompProviderQuota{Key: "chatgpt", Name: "ChatGPT", Disabled: count, Windows: []ompQuotaWindow{}}
	}

	type step struct {
		now       time.Duration
		summary   ompQuotaSummary
		alertsOff bool
		want      []string // "<level>: <message>"
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{
			name: "first run at 94% announces 90 once",
			steps: []step{
				{now: 0, summary: summary(claude(-time.Minute, fiveHour(0.94, 73*time.Minute), week(0.3, 72*time.Hour))),
					want: []string{"warning: Claude 5h at 94% used · resets 11:13 (in 1h13m)"}},
				{now: time.Minute, summary: summary(claude(-time.Minute, fiveHour(0.94, 73*time.Minute), week(0.3, 72*time.Hour)))},
			},
		},
		{
			name: "75% on an even pace waits until usage gets ahead",
			steps: []step{
				// Six of seven days gone: 80% used is on course.
				{now: 0, summary: summary(claude(-time.Minute, week(0.8, 24*time.Hour)))},
				{now: time.Hour, summary: summary(claude(time.Hour, week(0.88, 24*time.Hour))),
					want: []string{"warning: Claude 7d at 88% used · resets Mon 10:00 (in 23h)"}},
				{now: 2 * time.Hour, summary: summary(claude(2*time.Hour, week(0.89, 24*time.Hour)))},
			},
		},
		{
			name: "90% is announced even on an even pace",
			steps: []step{
				{now: 0, summary: summary(claude(-time.Minute, week(0.92, 6*time.Hour))),
					want: []string{"warning: Claude 7d at 92% used · resets 16:00 (in 6h)"}},
				{now: time.Minute, summary: summary(claude(0, week(0.93, 6*time.Hour)))},
			},
		},
		{
			name: "limit reached, then its reset",
			steps: []step{
				{now: 0, summary: summary(chatgpt(-time.Minute, fiveHour(1, 73*time.Minute), week(0.94, 48*time.Hour))),
					want: []string{
						"warning: ChatGPT 5h limit reached · resets 11:13 (in 1h13m) · 7d at 94% used · 1 saved reset: /usage reset",
						"warning: ChatGPT 7d at 94% used · resets Tue 10:00 (in 2d)",
					}},
				{now: 30 * time.Minute, summary: summary(chatgpt(29*time.Minute, fiveHour(1, 73*time.Minute), week(0.94, 48*time.Hour)))},
				// A report from before the reset says nothing once it has passed.
				{now: 75 * time.Minute, summary: summary(chatgpt(70*time.Minute, fiveHour(1, 73*time.Minute), week(0.94, 48*time.Hour)))},
				{now: 80 * time.Minute, summary: summary(chatgpt(79*time.Minute, fiveHour(0.02, 6*time.Hour+13*time.Minute), week(0.94, 48*time.Hour))),
					want: []string{"info: ChatGPT 5h limit has reset · available again · 7d at 94% used"}},
				{now: 90 * time.Minute, summary: summary(chatgpt(89*time.Minute, fiveHour(0.03, 6*time.Hour+13*time.Minute), week(0.94, 48*time.Hour)))},
			},
		},
		{
			name: "a run-out pace is announced once per episode",
			steps: []step{
				{now: 0, summary: summary(claude(-time.Minute, runsOut(fiveHour(0.6, 2*time.Hour), 40*time.Minute))),
					want: []string{"warning: Claude 5h at 60% used · at this pace it runs out in ~40m (10:40), before the reset at 12:00"}},
				{now: 10 * time.Minute, summary: summary(claude(10*time.Minute, runsOut(fiveHour(0.65, 2*time.Hour), 45*time.Minute)))},
				// Slowing down ends the episode silently.
				{now: 20 * time.Minute, summary: summary(claude(20*time.Minute, fiveHour(0.66, 2*time.Hour)))},
				{now: 30 * time.Minute, summary: summary(claude(30*time.Minute, runsOut(fiveHour(0.7, 2*time.Hour), 70*time.Minute))),
					want: []string{"warning: Claude 5h at 70% used · at this pace it runs out in ~40m (11:10), before the reset at 12:00"}},
			},
		},
		{
			name: "a threshold re-arms once usage falls well below it",
			steps: []step{
				{now: 0, summary: summary(claude(0, fiveHour(0.94, 73*time.Minute))),
					want: []string{"warning: Claude 5h at 94% used · resets 11:13 (in 1h13m)"}},
				// A second account joins the pool: the mean drops, no reset.
				{now: 5 * time.Minute, summary: summary(claude(5*time.Minute, fiveHour(0.7, 73*time.Minute)))},
				{now: 10 * time.Minute, summary: summary(claude(10*time.Minute, fiveHour(0.93, 73*time.Minute))),
					want: []string{"warning: Claude 5h at 93% used · resets 11:13 (in 1h3m)"}},
			},
		},
		{
			name: "numbers older than 15 minutes wait for a fresh report",
			steps: []step{
				{now: 0, summary: summary(claude(-20*time.Minute, fiveHour(0.95, 73*time.Minute)))},
				{now: time.Minute, summary: summary(claude(time.Minute, fiveHour(0.95, 73*time.Minute))),
					want: []string{"warning: Claude 5h at 95% used · resets 11:13 (in 1h12m)"}},
			},
		},
		{
			name: "alerts turned off still record what was seen",
			steps: []step{
				{now: 0, alertsOff: true, summary: summary(claude(0, fiveHour(0.95, 73*time.Minute)))},
				{now: time.Minute, summary: summary(claude(0, fiveHour(0.95, 73*time.Minute)))},
			},
		},
		{
			name: "a saved reset expiring within a day is announced once per expiry",
			steps: []step{
				{now: 0, summary: summary(withExpiry(claude(0), 5*time.Hour)),
					want: []string{"warning: Claude: a saved rate-limit reset expires in 5h — use it with /usage reset"}},
				{now: time.Minute, summary: summary(withExpiry(claude(0), 5*time.Hour))},
				{now: 2 * time.Minute, summary: summary(withExpiry(claude(0), 30*time.Hour))},
				{now: 3 * time.Minute, summary: summary(withExpiry(claude(0), 23*time.Hour)),
					want: []string{"warning: Claude: a saved rate-limit reset expires in 22h57m — use it with /usage reset"}},
			},
		},
		{
			name: "a disabled sign-in is announced when the count grows",
			steps: []step{
				{now: 0, summary: summary(disabled(1)), want: []string{"warning: ChatGPT sign-in was disabled — run /login"}},
				{now: time.Minute, summary: summary(disabled(1))},
				{now: 2 * time.Minute, summary: summary(disabled(2)), want: []string{"warning: ChatGPT sign-in was disabled — run /login"}},
				{now: 3 * time.Minute, summary: summary(disabled(0))},
				{now: 4 * time.Minute, summary: summary(disabled(1)), want: []string{"warning: ChatGPT sign-in was disabled — run /login"}},
			},
		},
		{
			name: "other providers never alert",
			steps: []step{
				{now: 0, summary: summary(ompProviderQuota{Key: "github-copilot", Name: "Github Copilot", Disabled: 1,
					Windows: []ompQuotaWindow{window("Monthly", 0, 1, 24*time.Hour)}})},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var state ompAlertState
			for index, step := range test.steps {
				config := defaultOmpStatuslineConfig()
				config.Alerts = !step.alertsOff
				alerts, next := evaluateOmpAlerts(step.summary, state, config, base.Add(step.now))
				var got []string
				for _, alert := range alerts {
					got = append(got, alert.Level+": "+alert.Message)
				}
				if !reflect.DeepEqual(got, step.want) {
					t.Fatalf("step %d alerts =\n%q\nwant\n%q", index, got, step.want)
				}
				// The extension keeps the state as JSON between calls.
				encoded, err := json.Marshal(next)
				if err != nil {
					t.Fatal(err)
				}
				state = ompAlertState{}
				if err := json.Unmarshal(encoded, &state); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRunOmpStatuslineAlertsStartsOverFromUnreadableState(t *testing.T) {
	t.Setenv("OPENUSAGE_OMP_STATUSLINE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	resetsAt := now.Add(73 * time.Minute).UnixMilli()
	summary := fmt.Sprintf(`{"schema":3,"providers":[{"key":"claude","name":"Claude","accounts":1,"fetchedAt":%d,
		"unreported":0,"disabled":0,"windows":[{"label":"5h","durationMs":18000000,"scoped":false,"usedFraction":0.95,
		"resetsAt":%d,"status":"warning","accounts":1}]}]}`, now.UnixMilli(), resetsAt)
	tests := []struct {
		name       string
		state      string // raw JSON; "" leaves the key out
		wantAlerts int
	}{
		{"no state", "", 1},
		{"null", "null", 1},
		{"not an object", `"garbage"`, 1},
		{"unknown version", `{"v":2,"windows":{"claude/5h":{"resetsAt":` + fmt.Sprint(resetsAt) + `,"used":0.95,"fired":90}}}`, 1},
		{"malformed window", `{"v":1,"windows":{"claude/5h":"oops"}}`, 1},
		{"already announced", `{"v":1,"windows":{"claude/5h":{"resetsAt":` + fmt.Sprint(resetsAt) + `,"used":0.95,"fired":90}}}`, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := `{"summary":` + summary
			if test.state != "" {
				input += `,"state":` + test.state
			}
			input += "}"
			var output bytes.Buffer
			if err := runOmpStatuslineAlerts(strings.NewReader(input), &output, now); err != nil {
				t.Fatal(err)
			}
			var result ompAlertsOutput
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatalf("output %s: %v", output.String(), err)
			}
			if result.Schema != 3 || len(result.Alerts) != test.wantAlerts || result.State.V != 1 ||
				result.State.Windows["claude/5h"].Fired != 90 {
				t.Fatalf("output = %s", output.String())
			}
			if test.wantAlerts == 0 && !strings.Contains(output.String(), `"alerts":[]`) {
				t.Fatalf("no alerts must still be a list: %s", output.String())
			}
		})
	}
}
