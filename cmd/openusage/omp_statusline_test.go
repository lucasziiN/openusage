package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSummarizeOmpQuotasKeepsEveryWindowUnderUniformLabels(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	ms := func(offset time.Duration) int64 { return now.Add(offset).UnixMilli() }
	output := []byte(fmt.Sprintf(`{"reports":[
		{"provider":"openai-codex","fetchedAt":%d,"limits":[
			{"scope":{"windowId":"5h"},"window":{"id":"5h","label":"5 hours","durationMs":18000000,"resetsAt":%d},
			 "amount":{"usedFraction":1,"remainingFraction":0},"status":"exhausted"},
			{"scope":{"windowId":"7d"},"window":{"id":"7d","label":"7 days","durationMs":604800000,"resetsAt":%d},
			 "amount":{"usedFraction":0.18},"status":"ok"}
		],"resetCredits":{"availableCount":2,"redeemableCount":1},
		 "metadata":{"planType":"plus","email":"private@example.com","accountId":"account-secret"}},
		{"provider":"anthropic","limits":[
			{"window":{"id":"5h","label":"5 Hour","resetsAt":%d},"amount":{"used":80,"limit":100,"remaining":20},"status":"ok"},
			{"window":{"id":"7d","label":"7 Day","durationMs":604800000,"resetsAt":%d},"amount":{"usedFraction":0.91},"status":"warning"},
			{"scope":{"tier":"fable"},"window":{"id":"7d","label":"7 Day","durationMs":604800000,"resetsAt":%d},"amount":{"usedFraction":0}}
		],"metadata":{"email":"private@example.com","orgName":"secret org"}},
		{"provider":"github-copilot","limits":[{"window":{"id":"monthly","label":"Monthly"},"amount":{"remainingFraction":0.75}}]}
	]}`, ms(-2*time.Minute), ms(73*time.Minute), ms(6*24*time.Hour+20*time.Hour),
		ms(2*time.Hour+33*time.Minute), ms(11*time.Hour+5*time.Minute), ms(11*time.Hour+5*time.Minute)))

	summary := summarizeOmpQuotas(output, nil, now)
	want := []ompProviderQuota{
		{Key: "chatgpt", Name: "ChatGPT", Plan: "Plus", Accounts: 1, FetchedAt: ms(-2 * time.Minute),
			Resets: 2, UsableResets: 1, Windows: []ompQuotaWindow{
				{Label: "5h", DurationMs: 18000000, UsedFraction: 1, ResetsAt: ms(73 * time.Minute), Status: "exhausted", Accounts: 1},
				{Label: "7d", DurationMs: 604800000, UsedFraction: 0.18, ResetsAt: ms(6*24*time.Hour + 20*time.Hour), Status: "ok", Accounts: 1},
			}},
		{Key: "claude", Name: "Claude", Accounts: 1, Windows: []ompQuotaWindow{
			{Label: "5h", DurationMs: 18000000, UsedFraction: 0.8, ResetsAt: ms(2*time.Hour + 33*time.Minute), Status: "ok", Accounts: 1},
			{Label: "7d", DurationMs: 604800000, UsedFraction: 0.91, ResetsAt: ms(11*time.Hour + 5*time.Minute), Status: "warning", Accounts: 1},
			{Label: "7d Fable", DurationMs: 604800000, Scoped: true, ResetsAt: ms(11*time.Hour + 5*time.Minute), Status: "ok", Accounts: 1},
		}},
		{Key: "github-copilot", Name: "Github Copilot", Accounts: 1, Windows: []ompQuotaWindow{
			{Label: "Monthly", UsedFraction: 0.25, Status: "ok", Accounts: 1},
		}},
	}
	if summary.Error != "" || !reflect.DeepEqual(summary.Providers, want) {
		t.Fatalf("providers =\n%#v\nwant\n%#v", summary.Providers, want)
	}

	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private@example.com", "account-secret", "secret org"} {
		if strings.Contains(string(encoded), private) {
			t.Errorf("account metadata %q escaped into display JSON: %s", private, encoded)
		}
	}

	// The binding-free Fable bucket stays out of the statusline.
	status := renderOmpStatusline(summary, now)
	want5h := "🐙 ChatGPT 5h 100% used (reset in 1h13m), 7d 18% used (reset in 6d20h) | " +
		"Claude 5h 80% used (reset in 2h33m), 7d 91% used (reset in 11h5m)"
	if status != want5h {
		t.Fatalf("status = %q\nwant     %q", status, want5h)
	}
}

func TestSummarizeOmpQuotasCountsAccountsCreditsAndExtraUsageWithoutIdentities(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	ms := func(offset time.Duration) int64 { return now.Add(offset).UnixMilli() }
	iso := func(offset time.Duration) string { return now.Add(offset).Format(time.RFC3339Nano) }
	// Fractional milliseconds, an odd planType and a malformed second report
	// must not cost the rest of the reply.
	output := []byte(fmt.Sprintf(`{"generatedAt":%d.7,"reports":[
		{"provider":"anthropic","fetchedAt":%d.5,"limits":[
			{"id":"anthropic:5h","scope":{"windowId":"5h","shared":true},
			 "window":{"id":"5h","label":"5 Hour","durationMs":18000000.25,"resetsAt":%d.4},
			 "amount":{"usedFraction":0.4,"unit":"percent"},"status":"ok"},
			{"id":"anthropic:extra","label":"Claude Extra Usage","scope":{"provider":"anthropic","windowId":"extra"},
			 "amount":{"used":46,"limit":50,"remaining":4,"usedFraction":0.92,"remainingFraction":0.08,"unit":"usd"},"status":"warning"}
		],"resetCredits":{"availableCount":3,"redeemableCount":1,"credits":[
			{"id":"later","status":"available","usable":true,"remainingCount":1,"expiresAt":%q},
			{"id":"sooner","status":"available","usable":true,"remainingCount":1,"expiresAt":%q},
			{"id":"blocked","status":"unavailable","usable":false,"remainingCount":1,"expiresAt":%q},
			{"id":"spent","status":"redeemed","remainingCount":0,"expiresAt":%q},
			{"id":"lapsed","status":"available","expiresAt":%q}
		]},"metadata":{"email":"private@example.com","planType":{"odd":"shape"}}},
		{"provider":"anthropic","fetchedAt":"not a number"}
	],
	"accountsWithoutUsage":[{"provider":"anthropic","type":"oauth","email":"second@example.com"}],
	"disabledCredentials":[
		{"id":7,"provider":"openai-codex","type":"oauth","email":"third@example.com","cause":"refresh token revoked"},
		{"id":8,"provider":"openai-codex","type":"oauth","cause":"refresh token revoked"}
	]}`, ms(-time.Minute), ms(-time.Minute), ms(4*time.Hour+50*time.Minute),
		iso(30*time.Hour), iso(20*time.Hour), iso(2*time.Hour), iso(time.Hour), iso(-time.Hour)))

	summary := summarizeOmpQuotas(output, nil, now)
	limit, fraction := 50.0, 0.92
	want := []ompProviderQuota{
		// Known only from its disabled sign-ins, ChatGPT still gets a row.
		{Key: "chatgpt", Name: "ChatGPT", Disabled: 2, Windows: []ompQuotaWindow{}},
		{Key: "claude", Name: "Claude", Accounts: 1, FetchedAt: ms(-time.Minute) + 1,
			Resets: 3, UsableResets: 1, ResetsExpireAt: ms(20 * time.Hour), Unreported: 1,
			// Extra usage is money beside the windows, not a "quota" window.
			Extra: &ompExtraUsage{UsedUSD: 46, LimitUSD: &limit, UsedFraction: &fraction},
			Windows: []ompQuotaWindow{
				{Label: "5h", DurationMs: 18000000, UsedFraction: 0.4, ResetsAt: ms(4*time.Hour + 50*time.Minute), Status: "ok", Accounts: 1},
			}},
	}
	if summary.Error != "" || !reflect.DeepEqual(summary.Providers, want) {
		t.Fatalf("providers =\n%#v\nwant\n%#v", summary.Providers, want)
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"example.com", "revoked", "oauth", "sooner"} {
		if strings.Contains(string(encoded), private) {
			t.Errorf("%q escaped into display JSON: %s", private, encoded)
		}
	}
}

func TestOmpStatuslineWindowsShowScopedBucketOnlyWhenBinding(t *testing.T) {
	tests := []struct {
		name    string
		windows []ompQuotaWindow
		want    []string
	}{
		{
			name: "binding model bucket",
			windows: []ompQuotaWindow{
				{Label: "5h", DurationMs: 18000000, UsedFraction: 0.3},
				{Label: "7d", DurationMs: 604800000, UsedFraction: 0.5},
				{Label: "7d Opus", DurationMs: 604800000, Scoped: true, UsedFraction: 0.95},
				{Label: "7d Sonnet", DurationMs: 604800000, Scoped: true, UsedFraction: 0.2},
			},
			want: []string{"5h", "7d", "7d Opus"},
		},
		{
			// Codex reports 17 940 s for its shared 5h and 18 000 s for a meter.
			name: "meter a minute longer than the shared window it trails",
			windows: []ompQuotaWindow{
				{Label: "5h", DurationMs: 17940000, UsedFraction: 0.5},
				{Label: "5h Spark", DurationMs: 18000000, Scoped: true, UsedFraction: 0.2},
			},
			want: []string{"5h"},
		},
		{
			name: "meter a minute longer than the shared window it outruns",
			windows: []ompQuotaWindow{
				{Label: "5h", DurationMs: 17940000, UsedFraction: 0.5},
				{Label: "5h Spark", DurationMs: 18000000, Scoped: true, UsedFraction: 0.7},
			},
			want: []string{"5h", "5h Spark"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var labels []string
			for _, window := range ompStatuslineWindows(ompProviderQuota{Windows: test.windows}) {
				labels = append(labels, window.Label)
			}
			if !reflect.DeepEqual(labels, test.want) {
				t.Fatalf("statusline windows = %v, want %v", labels, test.want)
			}
		})
	}
}

func TestSummarizeOmpQuotasAggregatesAccountsLikeOmp(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	soon, later := now.Add(time.Hour).UnixMilli(), now.Add(3*time.Hour).UnixMilli()
	output := []byte(fmt.Sprintf(`{"reports":[
		{"provider":"anthropic","limits":[{"window":{"id":"5h","durationMs":18000000,"resetsAt":%d},"amount":{"usedFraction":0.2},"status":"ok"}]},
		{"provider":"anthropic","limits":[
			{"scope":{"sharedGroup":"g"},"window":{"id":"5h","durationMs":18000000,"resetsAt":%d},"amount":{"usedFraction":1},"status":"exhausted"},
			{"scope":{"sharedGroup":"g"},"window":{"id":"5h","durationMs":18000000,"resetsAt":%d},"amount":{"usedFraction":1},"status":"exhausted"}
		]}
	]}`, later, soon, soon))

	claude := summarizeOmpQuotas(output, nil, now).provider("claude")
	if claude.Accounts != 2 || len(claude.Windows) != 1 {
		t.Fatalf("claude = %#v", claude)
	}
	window := claude.Windows[0]
	// Mean pressure across the pooled accounts, the busiest account's reset,
	// and a mixed healthy/exhausted pool reads as a warning.
	if window.UsedFraction != 0.6 || window.ResetsAt != soon || window.Status != "warning" || window.Accounts != 2 {
		t.Fatalf("aggregated window = %#v", window)
	}
}

func TestSummarizeOmpQuotasProjectsPaceFromWhenOmpMeasured(t *testing.T) {
	measured := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	ms := func(offset time.Duration) int64 { return measured.Add(offset).UnixMilli() }
	output := []byte(fmt.Sprintf(`{"reports":[{"provider":"anthropic","fetchedAt":%d,"limits":[
		{"window":{"id":"5h","durationMs":18000000,"resetsAt":%d},"amount":{"usedFraction":0.88},"status":"ok"},
		{"window":{"id":"7d","durationMs":604800000,"resetsAt":%d},"amount":{"usedFraction":0.5},"status":"ok"},
		{"scope":{"tier":"early"},"window":{"id":"7d","durationMs":604800000,"resetsAt":%d},"amount":{"usedFraction":0.2}}
	]}]}`, ms(0), ms(100*time.Minute), ms(3*24*time.Hour), ms(163*time.Hour)))

	windows := summarizeOmpQuotas(output, nil, measured).provider("claude").Windows
	if len(windows) != 3 {
		t.Fatalf("windows = %#v", windows)
	}
	fiveHour, week, early := windows[0], windows[1], windows[2]
	// 88% with 3h20m of 5h gone projects 132%: the last 12% lasts ~27 minutes.
	if math.Abs(fiveHour.ProjectedFraction-1.32) > 1e-9 ||
		time.UnixMilli(fiveHour.ExhaustsAt).Sub(measured).Round(time.Second) != 27*time.Minute+16*time.Second {
		t.Fatalf("5h pace = %v, runs out %v after measuring", fiveHour.ProjectedFraction,
			time.UnixMilli(fiveHour.ExhaustsAt).Sub(measured))
	}
	// Half used with four of seven days gone stays under the limit.
	if math.Abs(week.ProjectedFraction-0.875) > 1e-9 || week.ExhaustsAt != 0 {
		t.Fatalf("7d pace = %v exhausts %v", week.ProjectedFraction, week.ExhaustsAt)
	}
	// Five hours into a week is too early to extrapolate.
	if early.ProjectedFraction != 0 || early.ExhaustsAt != 0 {
		t.Fatalf("early window projected %v", early.ProjectedFraction)
	}

	// Rendered ten minutes later: the run-out counts down and the cell warns
	// although OMP still rates 88% as ok.
	later := measured.Add(10 * time.Minute)
	fetchedAt := measured.UnixMilli()
	if got := formatOmpQuotaWindow(fiveHour, "used", fetchedAt, later); got != "5h 88% used (reset in 1h30m, out in ~18m)" {
		t.Fatalf("cell = %q", got)
	}
	if got := ompDisplayStatus(fiveHour, fetchedAt, later); got != "warning" {
		t.Fatalf("status = %q, want warning", got)
	}
	if got := formatOmpQuotaWindow(week, "used", fetchedAt, later); strings.Contains(got, "out in") {
		t.Fatalf("on-pace window claims to run out: %q", got)
	}
}

func TestOmpPaceStartsAfterFivePercentOrFifteenMinutes(t *testing.T) {
	measured := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		span, elapsed time.Duration
		wantExhausts  time.Duration // after measuring; 0 means no projection
	}{
		{"5h window 14 minutes in", 5 * time.Hour, 14 * time.Minute, 0},
		// 20% in 15 minutes runs out 60 minutes later.
		{"5h window 15 minutes in", 5 * time.Hour, 15 * time.Minute, time.Hour},
		{"7d window 8 hours in", 7 * 24 * time.Hour, 8 * time.Hour, 0},
		{"7d window 9 hours in", 7 * 24 * time.Hour, 9 * time.Hour, 36 * time.Hour},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			window := ompQuotaWindow{
				DurationMs: test.span.Milliseconds(), UsedFraction: 0.2,
				ResetsAt: measured.Add(test.span - test.elapsed).UnixMilli(),
			}
			projected, exhaustsAt := ompPace(window, measured.UnixMilli())
			if test.wantExhausts == 0 {
				if projected != 0 || exhaustsAt != 0 {
					t.Fatalf("projected %v / %v this early", projected, exhaustsAt)
				}
				return
			}
			if got := time.UnixMilli(exhaustsAt).Sub(measured); got != test.wantExhausts {
				t.Fatalf("runs out %v after measuring, want %v (projected %v)", got, test.wantExhausts, projected)
			}
		})
	}
}

func TestFormatOmpQuotaWindowNeverOverstatesOrShowsStaleUsage(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	ago := func(offset time.Duration) int64 { return now.Add(-offset).UnixMilli() }
	resetIn := now.Add(73 * time.Minute).UnixMilli()
	runningOut := ompQuotaWindow{Label: "5h", UsedFraction: 0.42, ResetsAt: resetIn, Status: "ok",
		ExhaustsAt: now.Add(20 * time.Minute).UnixMilli()}
	tests := []struct {
		name      string
		window    ompQuotaWindow
		display   string
		fetchedAt int64
		want      string
		status    string
	}{
		{"nearly full is not full", ompQuotaWindow{Label: "5h", UsedFraction: 0.996, Status: "warning"}, "used", 0, "5h 99% used", "warning"},
		{"full", ompQuotaWindow{Label: "5h", UsedFraction: 1, Status: "exhausted"}, "used", 0, "5h 100% used", "exhausted"},
		{"overage", ompQuotaWindow{Label: "7d", UsedFraction: 1.2, Status: "exhausted"}, "used", 0, "7d 120% used", "exhausted"},
		{"countdown", ompQuotaWindow{Label: "7d", UsedFraction: 0.4, ResetsAt: now.Add(26*time.Hour + 30*time.Second).UnixMilli(), Status: "ok"}, "used", 0, "7d 40% used (reset in 1d2h)", "ok"},
		{"reset since reported", ompQuotaWindow{Label: "5h", UsedFraction: 1, ResetsAt: now.Add(-time.Minute).UnixMilli(), Status: "exhausted"}, "used", ago(2 * time.Hour), "5h reset", "ok"},
		{"left", ompQuotaWindow{Label: "5h", UsedFraction: 0.42, ResetsAt: resetIn, Status: "ok"}, "left", 0, "5h 58% left (reset in 1h13m)", "ok"},
		{"left never claims room that is not there", ompQuotaWindow{Label: "5h", UsedFraction: 0.996, Status: "warning"}, "left", 0, "5h 1% left", "warning"},
		{"left in overage", ompQuotaWindow{Label: "7d", UsedFraction: 1.2, Status: "exhausted"}, "left", 0, "7d 0% left", "exhausted"},
		{"15 minutes old is current", runningOut, "used", ago(15 * time.Minute), "5h 42% used (reset in 1h13m, out in ~20m)", "warning"},
		{"older numbers say so instead of projecting", runningOut, "used", ago(2 * time.Hour), "5h 42% used (reset in 1h13m, as of 2h ago)", "stale"},
		{"stale without a reset time", ompQuotaWindow{Label: "5h", UsedFraction: 0.95, Status: "warning"}, "left", ago(16 * time.Minute), "5h 5% left (as of 16m ago)", "stale"},
		{"stale but exhausted", ompQuotaWindow{Label: "5h", UsedFraction: 1, ResetsAt: resetIn, Status: "exhausted"}, "used", ago(2 * time.Hour), "5h 100% used (reset in 1h13m, as of 2h ago)", "exhausted"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := formatOmpQuotaWindow(test.window, test.display, test.fetchedAt, now); got != test.want {
				t.Fatalf("text = %q, want %q", got, test.want)
			}
			if got := ompDisplayStatus(test.window, test.fetchedAt, now); got != test.status {
				t.Fatalf("status = %q, want %q", got, test.status)
			}
		})
	}
}

func TestSummarizeOmpQuotasReportsUnavailableData(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		output     []byte
		commandErr error
		want       string
	}{
		{
			name:   "malformed JSON",
			output: []byte(`{"reports":[`),
			want:   "openusage: quota unavailable (invalid omp JSON)",
		},
		{
			name:   "no fraction for either subscription",
			output: []byte(`{"reports":[{"provider":"codex","limits":[{"window":{"label":"5h"},"amount":{}}]}]}`),
			want:   "openusage: quota unavailable (no ChatGPT or Claude data)",
		},
		{
			name:       "missing OMP executable",
			commandErr: &exec.Error{Name: "omp", Err: exec.ErrNotFound},
			want:       "openusage: quota unavailable (omp not found)",
		},
		{
			name:       "failed OMP command",
			commandErr: fmt.Errorf("failed with private output: %s", "account-secret"),
			want:       "openusage: quota unavailable (omp usage failed)",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			summary := summarizeOmpQuotas(test.output, test.commandErr, now)
			if status := renderOmpStatusline(summary, now); status != test.want {
				t.Fatalf("status = %q, want %q", status, test.want)
			}
			if encoded, _ := json.Marshal(summary); strings.Contains(string(encoded), "account-secret") {
				t.Fatalf("summary exposed command error output: %s", encoded)
			}
		})
	}
}

func TestLoadOmpQuotaSummarySharesOneFetchFor45Seconds(t *testing.T) {
	base := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	output := []byte(fmt.Sprintf(`{"reports":[{"provider":"anthropic","fetchedAt":%d,"limits":[
		{"window":{"id":"5h","durationMs":18000000,"resetsAt":%d},"amount":{"usedFraction":0.5},"status":"ok"}]}]}`,
		base.UnixMilli(), base.Add(time.Hour).UnixMilli()))
	path := filepath.Join(t.TempDir(), "openusage", "omp-quota-summary.json")
	var calls []string
	failing := false
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if failing {
			return nil, errors.New("omp failed")
		}
		return output, nil
	}
	fetch := []string{"usage --json --redact"}
	steps := []struct {
		name      string
		at        time.Duration
		fresh     bool
		fail      bool
		prepare   func(t *testing.T)
		wantCalls []string
		wantError bool
	}{
		{name: "first call fetches", at: 0, wantCalls: fetch},
		{name: "reused just under 45 s", at: 44 * time.Second},
		{name: "refetched at 45 s", at: 45 * time.Second, wantCalls: fetch},
		{name: "fresh drops OMP's cache and skips the young summary", at: 50 * time.Second, fresh: true,
			wantCalls: []string{"usage invalidate", "usage --json --redact"}},
		{name: "fresh result is shared", at: 60 * time.Second},
		{name: "a failed fetch is reported", at: 100 * time.Second, fail: true, wantCalls: fetch, wantError: true},
		{name: "and not reused", at: 101 * time.Second, wantCalls: fetch},
		{name: "a cache from another schema is ignored", at: 102 * time.Second, wantCalls: fetch,
			prepare: func(t *testing.T) {
				data := fmt.Sprintf(`{"savedAt":%d,"summary":{"schema":2,"providers":[]}}`, base.Add(102*time.Second).UnixMilli())
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}},
	}
	want := summarizeOmpQuotas(output, nil, base)
	for _, step := range steps {
		if step.prepare != nil {
			step.prepare(t)
		}
		calls, failing = nil, step.fail
		summary := loadOmpQuotaSummary(context.Background(), run, path, step.fresh, base.Add(step.at))
		if !reflect.DeepEqual(calls, step.wantCalls) {
			t.Fatalf("%s: omp calls = %q, want %q", step.name, calls, step.wantCalls)
		}
		if step.wantError {
			if summary.Error == "" {
				t.Fatalf("%s: error summary missing: %#v", step.name, summary)
			}
			continue
		}
		if !reflect.DeepEqual(summary, want) {
			t.Fatalf("%s: summary =\n%#v\nwant\n%#v", step.name, summary, want)
		}
	}
}

func TestRenderOmpStatusShowsBothWindowsAndLiveCountdowns(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	today := 628.77
	input := ompLineInput{
		TodayCostUSD: &today,
		Block: &ompBlockDetails{
			CostUSD: 111.7, BurnUSDHour: 58.23,
			StartedAt: now.Add(-110 * time.Minute).UnixMilli(), EndsAt: now.Add(3*time.Hour + 10*time.Minute).UnixMilli(),
		},
		Quotas: &ompQuotaSummary{Providers: []ompProviderQuota{
			{Key: "chatgpt", Name: "ChatGPT", Windows: []ompQuotaWindow{
				{Label: "5h", DurationMs: 18000000, UsedFraction: 1, ResetsAt: now.Add(73 * time.Minute).UnixMilli(), Status: "exhausted"},
				{Label: "7d", DurationMs: 604800000, UsedFraction: 0.18, ResetsAt: now.Add(159 * time.Hour).UnixMilli(), Status: "ok"},
			}},
		}},
	}
	config := defaultOmpStatuslineConfig()
	config.Color = false
	result := renderOmpStatus(input, config, now)
	if want := "💰 $628.77 today est / $111.70 block (3h10m left) | 🔥 $58.23/hr"; result.Status != want {
		t.Fatalf("status = %q, want %q", result.Status, want)
	}
	chatgpt := "🐙 ChatGPT | 🕔 5h 100% used (reset in 1h13m) | 📅 7d 18% used (reset in 6d15h)"
	if len(result.Quotas) != 2 || result.Quotas[0] != chatgpt || result.Quotas[1] != "🐙 Claude | n/a" {
		t.Fatalf("quota rows = %q", result.Quotas)
	}
	if want := map[string]string{"🕔 5h 100% used (reset in 1h13m)": "exhausted"}; !reflect.DeepEqual(result.CellStatus, want) {
		t.Fatalf("cell status = %v, want %v", result.CellStatus, want)
	}

	// The same payload re-rendered later counts down; an elapsed block is gone.
	later := renderOmpStatus(input, config, now.Add(3*time.Hour+11*time.Minute))
	if strings.Contains(later.Status, "block") || !strings.Contains(later.Quotas[0], "🕔 5h reset | 📅 7d 18% used (reset in 6d11h)") {
		t.Fatalf("later render = %q / %q", later.Status, later.Quotas)
	}
}

func TestRenderOmpStatusExplainsMissingWindowsStaleNumbersAndIncidents(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	ms := func(offset time.Duration) int64 { return now.Add(offset).UnixMilli() }
	window := func(label string, span time.Duration, used float64, status string) ompQuotaWindow {
		return ompQuotaWindow{Label: label, DurationMs: span.Milliseconds(), UsedFraction: used,
			ResetsAt: ms(73 * time.Minute), Status: status, Accounts: 1}
	}
	fiveHour := window("5h", 5*time.Hour, 0.42, "ok")
	outage := map[string]ompVendorStatus{"claude": {Indicator: "major", Title: "Claude is down"}}
	tests := []struct {
		name      string
		claude    ompProviderQuota
		statuses  map[string]ompVendorStatus
		configure func(*ompStatuslineConfig)
		wantRow   string
		wantCells map[string]string
	}{
		{
			name:      "sign-in disabled",
			claude:    ompProviderQuota{Key: "claude", Disabled: 1, Unreported: 1, Windows: []ompQuotaWindow{}},
			wantRow:   "🐙 Claude | sign-in disabled (/login)",
			wantCells: map[string]string{"sign-in disabled (/login)": "warning"},
		},
		{
			name:    "account without usage",
			claude:  ompProviderQuota{Key: "claude", Unreported: 1, Windows: []ompQuotaWindow{}},
			wantRow: "🐙 Claude | no usage reported",
		},
		{name: "no data", wantRow: "🐙 Claude | n/a"},
		{
			name:      "quota left",
			claude:    ompProviderQuota{Key: "claude", FetchedAt: ms(-time.Minute), Windows: []ompQuotaWindow{fiveHour}},
			configure: func(config *ompStatuslineConfig) { config.QuotaDisplay = "left" },
			wantRow:   "🐙 Claude | 🕔 5h 58% left (reset in 1h13m)",
		},
		{
			name: "numbers two hours old",
			claude: ompProviderQuota{Key: "claude", FetchedAt: ms(-2 * time.Hour), Windows: []ompQuotaWindow{
				window("5h", 5*time.Hour, 0.95, "warning"), window("7d", 7*24*time.Hour, 1, "exhausted"),
			}},
			wantRow: "🐙 Claude | 🕔 5h 95% used (reset in 1h13m, as of 2h ago) | 📅 7d 100% used (reset in 1h13m, as of 2h ago)",
			wantCells: map[string]string{
				"🕔 5h 95% used (reset in 1h13m, as of 2h ago)":  "stale",
				"📅 7d 100% used (reset in 1h13m, as of 2h ago)": "exhausted",
			},
		},
		{
			name:   "incident title cut to 32 runes",
			claude: ompProviderQuota{Key: "claude", FetchedAt: ms(-time.Minute), Windows: []ompQuotaWindow{fiveHour}},
			statuses: map[string]ompVendorStatus{"claude": {
				Indicator: "minor", Title: "Elevated errors on Claude.ai | API and\nClaude Code logins",
			}},
			wantRow:   "🐙 Claude | 🕔 5h 42% used (reset in 1h13m) | ⚠ Elevated errors on Claude.ai /…",
			wantCells: map[string]string{"⚠ Elevated errors on Claude.ai /…": "warning"},
		},
		{
			name:      "outage",
			statuses:  outage,
			wantRow:   "🐙 Claude | n/a | ⚠ Claude is down",
			wantCells: map[string]string{"⚠ Claude is down": "exhausted"},
		},
		{
			name:      "provider status turned off",
			statuses:  outage,
			configure: func(config *ompStatuslineConfig) { config.ProviderStatus = false },
			wantRow:   "🐙 Claude | n/a",
		},
		{
			name:     "all systems operational",
			statuses: map[string]ompVendorStatus{"claude": {Indicator: "none", Title: "All Systems Operational"}},
			wantRow:  "🐙 Claude | n/a",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := defaultOmpStatuslineConfig()
			config.Segments = []string{"claude"}
			if test.configure != nil {
				test.configure(&config)
			}
			input := ompLineInput{
				Quotas:   &ompQuotaSummary{Providers: []ompProviderQuota{test.claude}},
				Statuses: test.statuses,
			}
			result := renderOmpStatus(input, config, now)
			if len(result.Quotas) != 1 || result.Quotas[0] != test.wantRow {
				t.Fatalf("rows = %q\nwant   %q", result.Quotas, test.wantRow)
			}
			if !reflect.DeepEqual(result.CellStatus, test.wantCells) {
				t.Fatalf("cell status = %v, want %v", result.CellStatus, test.wantCells)
			}
			if result.Schema != 3 || result.QuotaDisplay != config.QuotaDisplay || result.ProviderStatus != config.ProviderStatus {
				t.Fatalf("schema/quotaDisplay/providerStatus = %d/%q/%v", result.Schema, result.QuotaDisplay, result.ProviderStatus)
			}
		})
	}
}
