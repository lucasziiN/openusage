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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/janekbaraniewski/openusage/internal/fileutil"
)

// ompUsageTimeout bounds every omp call. OMP gives each credential about 10 s
// of its own, so a slow provider plus OMP's start-up has to fit in this.
const ompUsageTimeout = 25 * time.Second

// ompQuotaCacheTTL is how long one `omp usage` result serves every OMP
// instance. Each instance polls about once a minute, so concurrent instances
// share one spawn instead of each paying for their own.
const ompQuotaCacheTTL = 45 * time.Second

// ompQuotaStaleAfter is the age past which a provider's numbers no longer
// describe now: OMP keeps serving a provider's last good report while its
// usage endpoint fails.
const ompQuotaStaleAfter = 15 * time.Minute

// ompExtensionSchema versions the JSON exchanged with the OMP extension. The
// extension checks it so a stale openusage.exe or extension says so instead
// of silently showing n/a. Bump it whenever that JSON changes shape.
const ompExtensionSchema = 3

// OMP returns account metadata (emails, account and org ids) alongside its
// reports. These types decode only provider, window, amount, status and plan
// fields, so identifying metadata never reaches the statusline or extension.
// Numbers decode as float64 because any provider may report fractional
// milliseconds or counts, and one such value must not fail the whole reply.
type ompUsageResponse struct {
	GeneratedAt float64 `json:"generatedAt"`
	// Entries decode one by one so a single odd report cannot hide the rest.
	Reports              []json.RawMessage `json:"reports"`
	AccountsWithoutUsage []json.RawMessage `json:"accountsWithoutUsage"`
	DisabledCredentials  []json.RawMessage `json:"disabledCredentials"`
}

type ompUsageReport struct {
	Provider     string           `json:"provider"`
	FetchedAt    float64          `json:"fetchedAt"`
	Limits       []ompUsageLimit  `json:"limits"`
	ResetCredits *ompResetCredits `json:"resetCredits"`
	Metadata     struct {
		// Metadata is provider-defined; only a string names a plan.
		PlanType any `json:"planType"`
	} `json:"metadata"`
}

// ompAccountRef is an accountsWithoutUsage or disabledCredentials entry,
// decoded for its provider only: never its identity or disable cause.
type ompAccountRef struct {
	Provider string `json:"provider"`
}

type ompUsageLimit struct {
	Scope  ompUsageScope  `json:"scope"`
	Window ompUsageWindow `json:"window"`
	Amount ompUsageAmount `json:"amount"`
	Status string         `json:"status"`
}

type ompUsageScope struct {
	WindowID    string `json:"windowId"`
	Tier        string `json:"tier"`
	ModelID     string `json:"modelId"`
	SharedGroup string `json:"sharedGroup"`
}

type ompUsageWindow struct {
	ID         string  `json:"id"`
	Label      string  `json:"label"`
	ResetsAt   float64 `json:"resetsAt"`
	DurationMs float64 `json:"durationMs"`
}

type ompUsageAmount struct {
	Used              *float64 `json:"used"`
	Limit             *float64 `json:"limit"`
	Remaining         *float64 `json:"remaining"`
	UsedFraction      *float64 `json:"usedFraction"`
	RemainingFraction *float64 `json:"remainingFraction"`
	Unit              string   `json:"unit"`
}

type ompResetCredits struct {
	AvailableCount  float64          `json:"availableCount"`
	RedeemableCount *float64         `json:"redeemableCount"`
	Credits         []ompResetCredit `json:"credits"`
}

type ompResetCredit struct {
	ExpiresAt      string   `json:"expiresAt"` // ISO timestamp
	Status         string   `json:"status"`    // available, redeemed, expired, paused, unavailable
	Usable         *bool    `json:"usable"`
	RemainingCount *float64 `json:"remainingCount"`
}

// ompQuotaSummary is the display-safe quota JSON shared with the extension:
// every provider OMP reports, each with all of its fraction-based windows.
type ompQuotaSummary struct {
	Schema    int                `json:"schema"`
	Providers []ompProviderQuota `json:"providers"`
	Error     string             `json:"error,omitempty"`
}

type ompProviderQuota struct {
	Key  string `json:"key"` // "chatgpt", "claude", or the sanitized OMP provider id
	Name string `json:"name"`
	Plan string `json:"plan,omitempty"`
	// Accounts is the number of authenticated accounts reporting.
	Accounts int `json:"accounts"`
	// FetchedAt is the oldest report's fetch time in Unix milliseconds.
	FetchedAt    int64 `json:"fetchedAt,omitempty"`
	Resets       int   `json:"resets,omitempty"`       // saved rate-limit resets
	UsableResets int   `json:"usableResets,omitempty"` // of those, redeemable now
	// ResetsExpireAt (Unix ms) is when the first usable saved reset lapses.
	ResetsExpireAt int64 `json:"resetsExpireAt,omitempty"`
	// Unreported counts signed-in accounts OMP got no usage for; Disabled
	// counts sign-ins OMP turned off, for example after an expired grant.
	Unreported int              `json:"unreported"`
	Disabled   int              `json:"disabled"`
	Extra      *ompExtraUsage   `json:"extra,omitempty"`
	Windows    []ompQuotaWindow `json:"windows"`
}

// ompExtraUsage is pay-as-you-go spend beyond the plan, such as Claude extra
// usage. It never resets on a window, so it is reported beside the windows.
type ompExtraUsage struct {
	UsedUSD float64 `json:"usedUSD"`
	// LimitUSD and UsedFraction are absent when the spend is uncapped.
	LimitUSD     *float64 `json:"limitUSD,omitempty"`
	UsedFraction *float64 `json:"usedFraction,omitempty"`
}

type ompQuotaWindow struct {
	Label      string `json:"label"` // "5h", "7d", or "7d Fable" for a scoped bucket
	DurationMs int64  `json:"durationMs,omitempty"`
	// Scoped marks a tier- or model-specific bucket beside the shared window.
	Scoped bool `json:"scoped"`
	// UsedFraction is the mean across accounts; above 1 means overage.
	UsedFraction float64 `json:"usedFraction"`
	// ResetsAt is the most-used account's reset in Unix milliseconds.
	ResetsAt int64  `json:"resetsAt,omitempty"`
	Status   string `json:"status"` // ok, warning or exhausted
	Accounts int    `json:"accounts"`
	// ProjectedFraction is the usage a single account reaches by the reset at
	// the pace measured so far; ExhaustsAt (Unix ms) is set when that pace
	// runs out before the reset. Both are empty too early in a window.
	ProjectedFraction float64 `json:"projectedFraction,omitempty"`
	ExhaustsAt        int64   `json:"exhaustsAt,omitempty"`
}

func newOmpStatuslineCommand() *cobra.Command {
	var jsonOutput, render, fresh bool
	command := &cobra.Command{
		Use:   "omp-statusline",
		Short: "Show OMP quotas or configure the OpenUsage OMP footer",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if render {
				return runOmpStatuslineRender(cmd.InOrStdin(), cmd.OutOrStdout(), jsonOutput)
			}
			now := time.Now()
			summary := loadOmpQuotaSummary(context.Background(), runOmp, ompQuotaCachePath(), fresh, now)
			if !jsonOutput {
				fmt.Fprintln(cmd.OutOrStdout(), renderOmpStatusline(summary, now))
				return nil
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(summary)
		},
	}
	command.Flags().BoolVar(&jsonOutput, "json", false, "emit display-safe quota fields for the OMP extension")
	command.Flags().BoolVar(&render, "render", false, "render selected OMP status and quota rows from extension JSON on stdin")
	command.Flags().BoolVar(&fresh, "fresh", false, "drop OMP's cached usage and the shared 45 s summary before fetching")
	command.AddCommand(
		newOmpStatuslineInstallCommand(),
		newOmpStatuslineCostsCommand(),
		newOmpStatuslineAlertsCommand(),
		newOmpStatuslineHistoryCommand(),
	)
	return command
}

// ompRunner runs `omp <args>` and returns its standard output.
type ompRunner func(ctx context.Context, args ...string) ([]byte, error)

func runOmp(parent context.Context, args ...string) ([]byte, error) {
	executable, err := exec.LookPath("omp")
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(parent, ompUsageTimeout)
	defer cancel()

	return exec.CommandContext(ctx, executable, args...).Output()
}

// ompCommandFailure says why an omp call failed without echoing its output,
// which can carry account details.
func ompCommandFailure(err error) string {
	var executableError *exec.Error
	if errors.As(err, &executableError) && errors.Is(executableError.Err, exec.ErrNotFound) {
		return "omp not found"
	}
	return "omp usage failed"
}

// ompQuotaCache is the on-disk summary shared by every OMP instance.
type ompQuotaCache struct {
	SavedAt int64           `json:"savedAt"` // Unix ms
	Summary ompQuotaSummary `json:"summary"`
}

func ompQuotaCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "openusage", "omp-quota-summary.json")
}

// loadOmpQuotaSummary reuses a summary cached under ompQuotaCacheTTL ago, so
// OMP instances polling side by side spawn `omp usage` once between them.
// fresh skips that cache and first asks OMP to drop its own cached reports.
func loadOmpQuotaSummary(ctx context.Context, run ompRunner, cachePath string, fresh bool, now time.Time) ompQuotaSummary {
	if fresh {
		// Best effort: without the invalidate, the fetch still works.
		_, _ = run(ctx, "usage", "invalidate")
	} else if cached, ok := readOmpQuotaCache(cachePath, now); ok {
		return cached
	}
	output, err := run(ctx, "usage", "--json", "--redact")
	summary := summarizeOmpQuotas(output, err, now)
	if summary.Error == "" {
		writeOmpQuotaCache(cachePath, summary, now)
	}
	return summary
}

func readOmpQuotaCache(path string, now time.Time) (ompQuotaSummary, bool) {
	if path == "" {
		return ompQuotaSummary{}, false
	}
	data, err := fileutil.ReadFile(path)
	if err != nil {
		return ompQuotaSummary{}, false
	}
	var cache ompQuotaCache
	if json.Unmarshal(data, &cache) != nil {
		return ompQuotaSummary{}, false
	}
	// A cache from the future (clock change) or another schema is not reused.
	age := now.Sub(time.UnixMilli(cache.SavedAt))
	summary := cache.Summary
	if age < 0 || age >= ompQuotaCacheTTL || summary.Schema != ompExtensionSchema ||
		summary.Error != "" || summary.Providers == nil {
		return ompQuotaSummary{}, false
	}
	return summary, true
}

// writeOmpQuotaCache ignores failures: the cache only saves work.
func writeOmpQuotaCache(path string, summary ompQuotaSummary, now time.Time) {
	if path == "" || summary.Error != "" {
		return
	}
	data, err := json.Marshal(ompQuotaCache{SavedAt: now.UnixMilli(), Summary: summary})
	if err != nil {
		return
	}
	_ = fileutil.WriteFileAtomic(path, data, 0o600)
}

// renderOmpStatusline is the quota-only diagnostic printed by a bare
// `openusage omp-statusline`.
func renderOmpStatusline(summary ompQuotaSummary, now time.Time) string {
	if summary.Error != "" {
		return summary.Error
	}
	segments := make([]string, 0, len(ompStatuslineProviders))
	available := false
	for _, entry := range ompStatuslineProviders {
		provider := summary.provider(entry.key)
		var windows []string
		for _, window := range ompStatuslineWindows(provider) {
			windows = append(windows, formatOmpQuotaWindow(window, "used", provider.FetchedAt, now))
		}
		if len(windows) == 0 {
			text := ompNoWindowText(provider)
			available = available || text != "n/a"
			segments = append(segments, entry.name+" "+text)
			continue
		}
		available = true
		segments = append(segments, entry.name+" "+strings.Join(windows, ", "))
	}
	if !available {
		return "openusage: quota unavailable (no ChatGPT or Claude data)"
	}
	return "🐙 " + strings.Join(segments, " | ")
}

// ompNoWindowText explains a provider row that has no quota window to show.
func ompNoWindowText(provider ompProviderQuota) string {
	switch {
	case provider.Disabled > 0:
		return "sign-in disabled (/login)"
	case provider.Unreported > 0:
		return "no usage reported"
	}
	return "n/a"
}

// ompStatuslineProviders are the subscriptions with statusline rows.
var ompStatuslineProviders = [...]struct{ key, name string }{
	{"chatgpt", "ChatGPT"},
	{"claude", "Claude"},
}

func (summary ompQuotaSummary) provider(key string) ompProviderQuota {
	for _, provider := range summary.Providers {
		if provider.Key == key {
			return provider
		}
	}
	return ompProviderQuota{}
}

func summarizeOmpQuotas(output []byte, commandErr error, now time.Time) ompQuotaSummary {
	summary := ompQuotaSummary{Schema: ompExtensionSchema, Providers: []ompProviderQuota{}}
	if commandErr != nil {
		summary.Error = "openusage: quota unavailable (" + ompCommandFailure(commandErr) + ")"
		return summary
	}
	var response ompUsageResponse
	if err := json.Unmarshal(output, &response); err != nil {
		summary.Error = "openusage: quota unavailable (invalid omp JSON)"
		return summary
	}
	generatedAt := ompMillis(response.GeneratedAt)

	type windowTotals struct {
		window     ompQuotaWindow
		usedSum    float64
		worstUsed  float64
		measuredAt int64 // when OMP fetched the most-used account's numbers
		statuses   []string
		firstIndex int
	}
	type providerTotals struct {
		quota   ompProviderQuota
		windows map[string]*windowTotals
		// Extra usage sums across accounts; the cap only while all are capped.
		extra       *ompExtraUsage
		extraLimit  float64
		extraCapped bool
	}
	providers := map[string]*providerTotals{}
	providerFor := func(id string) *providerTotals {
		key, name := ompProviderIdentity(id)
		if key == "" {
			return nil
		}
		totals, ok := providers[key]
		if !ok {
			totals = &providerTotals{
				quota:   ompProviderQuota{Key: key, Name: name, Windows: []ompQuotaWindow{}},
				windows: map[string]*windowTotals{},
			}
			providers[key] = totals
		}
		return totals
	}
	for _, raw := range response.Reports {
		var report ompUsageReport
		if json.Unmarshal(raw, &report) != nil {
			continue
		}
		totals := providerFor(report.Provider)
		if totals == nil {
			continue
		}
		quota := &totals.quota
		quota.Accounts++
		if plan, ok := report.Metadata.PlanType.(string); ok && quota.Plan == "" {
			quota.Plan = ompTitle(plan)
		}
		fetchedAt := ompMillis(report.FetchedAt)
		if fetchedAt > 0 && (quota.FetchedAt == 0 || fetchedAt < quota.FetchedAt) {
			quota.FetchedAt = fetchedAt
		}
		measuredAt := fetchedAt
		if measuredAt == 0 {
			measuredAt = generatedAt
		}
		if credits := report.ResetCredits; credits != nil {
			if available := ompCount(credits.AvailableCount); available > 0 {
				quota.Resets += available
				usable := available
				if credits.RedeemableCount != nil {
					usable = min(ompCount(*credits.RedeemableCount), available)
				}
				quota.UsableResets += usable
			}
			for _, credit := range credits.Credits {
				if expiresAt := ompUsableCreditExpiry(credit, now); expiresAt > 0 &&
					(quota.ResetsExpireAt == 0 || expiresAt < quota.ResetsExpireAt) {
					quota.ResetsExpireAt = expiresAt
				}
			}
		}

		// Routing-specific copies of one upstream quota share a group; count it once.
		sharedGroups := map[string]bool{}
		for _, limit := range report.Limits {
			if group := limit.Scope.SharedGroup; group != "" {
				if sharedGroups[group] {
					continue
				}
				sharedGroups[group] = true
			}
			if ompIsExtraUsage(limit) {
				usedUSD, limitUSD, ok := ompExtraAmount(limit.Amount)
				if !ok {
					continue
				}
				if totals.extra == nil {
					totals.extra, totals.extraCapped = &ompExtraUsage{}, true
				}
				totals.extra.UsedUSD += usedUSD
				totals.extraLimit += limitUSD
				totals.extraCapped = totals.extraCapped && limitUSD > 0
				continue
			}
			used, ok := ompUsedFraction(limit.Amount)
			if !ok {
				continue
			}
			label, span, scoped := ompWindowLabel(limit)
			window, ok := totals.windows[label]
			if !ok {
				window = &windowTotals{
					window:     ompQuotaWindow{Label: label, DurationMs: span.Milliseconds(), Scoped: scoped},
					worstUsed:  -1,
					firstIndex: len(totals.windows),
				}
				totals.windows[label] = window
			}
			window.window.Accounts++
			window.usedSum += used
			window.statuses = append(window.statuses, ompLimitStatus(limit.Status, used))
			if used > window.worstUsed {
				window.worstUsed = used
				window.window.ResetsAt = ompMillis(limit.Window.ResetsAt)
				window.measuredAt = measuredAt
			}
		}
	}
	for _, raw := range response.AccountsWithoutUsage {
		var account ompAccountRef
		if json.Unmarshal(raw, &account) == nil {
			if totals := providerFor(account.Provider); totals != nil {
				totals.quota.Unreported++
			}
		}
	}
	for _, raw := range response.DisabledCredentials {
		var account ompAccountRef
		if json.Unmarshal(raw, &account) == nil {
			if totals := providerFor(account.Provider); totals != nil {
				totals.quota.Disabled++
			}
		}
	}

	for _, totals := range providers {
		if extra := totals.extra; extra != nil {
			if totals.extraCapped {
				limit, fraction := totals.extraLimit, extra.UsedUSD/totals.extraLimit
				extra.LimitUSD, extra.UsedFraction = &limit, &fraction
			}
			totals.quota.Extra = extra
		}
		windows := make([]*windowTotals, 0, len(totals.windows))
		for _, window := range totals.windows {
			window.window.UsedFraction = window.usedSum / float64(window.window.Accounts)
			window.window.Status = aggregateOmpStatus(window.statuses)
			// A pooled window's accounts each run at their own pace.
			if window.window.Accounts == 1 {
				window.window.ProjectedFraction, window.window.ExhaustsAt = ompPace(window.window, window.measuredAt)
			}
			windows = append(windows, window)
		}
		sort.Slice(windows, func(i, j int) bool {
			left, right := windows[i].window, windows[j].window
			if left.DurationMs != right.DurationMs {
				return left.DurationMs < right.DurationMs
			}
			if left.Scoped != right.Scoped {
				return !left.Scoped
			}
			return windows[i].firstIndex < windows[j].firstIndex
		})
		for _, window := range windows {
			totals.quota.Windows = append(totals.quota.Windows, window.window)
		}
		summary.Providers = append(summary.Providers, totals.quota)
	}
	sort.Slice(summary.Providers, func(i, j int) bool {
		left, right := ompProviderRank(summary.Providers[i].Key), ompProviderRank(summary.Providers[j].Key)
		if left != right {
			return left < right
		}
		return summary.Providers[i].Name < summary.Providers[j].Name
	})
	return summary
}

// ompMillis rounds an OMP timestamp or duration to whole milliseconds. OMP
// providers may report fractions; negative or absurd values mean unknown.
func ompMillis(value float64) int64 {
	if math.IsNaN(value) || value <= 0 || value >= 1<<53 {
		return 0
	}
	return int64(math.Round(value))
}

// ompCount reads a count OMP reports as a JSON number.
func ompCount(value float64) int {
	if math.IsNaN(value) || value < 1 {
		return 0
	}
	return int(min(value, 1_000_000))
}

// ompUsableCreditExpiry is a saved reset's expiry (Unix ms) while it can
// still be spent: OMP lists redeemed, expired, paused and not-yet-usable
// credits too, and their expiry is no reason to act. It is 0 otherwise.
func ompUsableCreditExpiry(credit ompResetCredit, now time.Time) int64 {
	if credit.Usable != nil && !*credit.Usable {
		return 0
	}
	if status := strings.ToLower(strings.TrimSpace(credit.Status)); status != "" && status != "available" {
		return 0
	}
	if credit.RemainingCount != nil && !(*credit.RemainingCount > 0) {
		return 0
	}
	expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(credit.ExpiresAt))
	if err != nil || !expiresAt.After(now) {
		return 0
	}
	return expiresAt.UnixMilli()
}

// ompIsExtraUsage recognises pay-as-you-go spend such as Claude extra usage:
// OMP tags it with the "extra" window id, and any dollar amount without a
// window is the same kind of limit. It is money spent, not a rate-limit
// window to count down, so it never becomes one.
func ompIsExtraUsage(limit ompUsageLimit) bool {
	if strings.EqualFold(limit.Scope.WindowID, "extra") {
		return true
	}
	window := limit.Window
	return strings.EqualFold(limit.Amount.Unit, "usd") &&
		window.ID == "" && window.Label == "" && ompMillis(window.DurationMs) == 0
}

// ompExtraAmount reads dollars spent and the cap (0 when uncapped).
func ompExtraAmount(amount ompUsageAmount) (usedUSD, limitUSD float64, ok bool) {
	if amount.Limit != nil && ompNonNegative(*amount.Limit) {
		limitUSD = *amount.Limit
	}
	switch {
	case amount.Used != nil && ompNonNegative(*amount.Used):
		return *amount.Used, limitUSD, true
	case limitUSD > 0 && amount.UsedFraction != nil && ompNonNegative(*amount.UsedFraction):
		return *amount.UsedFraction * limitUSD, limitUSD, true
	}
	return 0, 0, false
}

func ompProviderRank(key string) int {
	for index, provider := range ompStatuslineProviders {
		if provider.key == key {
			return index
		}
	}
	return len(ompStatuslineProviders)
}

// ompProviderIdentity maps an OMP provider id to a stable key and display name.
func ompProviderIdentity(provider string) (key, name string) {
	id := strings.ToLower(strings.TrimSpace(provider))
	switch id {
	case "codex", "openai-codex":
		return "chatgpt", "ChatGPT"
	case "anthropic", "claude", "claude-code", "claude_code":
		return "claude", "Claude"
	}
	key = safeOmpLabel(id, "")
	return key, ompTitle(key)
}

// ompStatuslineWindows returns the windows a statusline row shows: every
// shared window, plus a scoped (tier or model) bucket only while it is the
// binding constraint for its window length. Lengths compare by their rounded
// label because providers report one length a little differently: Codex
// sends 17 940 s for its shared 5h and 18 000 s for a model meter.
func ompStatuslineWindows(provider ompProviderQuota) []ompQuotaWindow {
	shared := map[string]float64{}
	for _, window := range provider.Windows {
		if !window.Scoped {
			length := ompWindowLength(window)
			if used, ok := shared[length]; !ok || window.UsedFraction > used {
				shared[length] = window.UsedFraction
			}
		}
	}
	var windows []ompQuotaWindow
	for _, window := range provider.Windows {
		if sharedUsed, ok := shared[ompWindowLength(window)]; window.Scoped && ok && window.UsedFraction <= sharedUsed {
			continue
		}
		windows = append(windows, window)
	}
	return windows
}

// ompWindowLength is a window's length as labelled ("5h"), "" when unknown.
func ompWindowLength(window ompQuotaWindow) string {
	if window.DurationMs <= 0 {
		return ""
	}
	return formatOmpWindowSpan(time.Duration(window.DurationMs) * time.Millisecond)
}

// formatOmpQuotaWindow renders "5h 42% used (reset in 1h13m)", or with
// display "left" "5h 58% left (reset in 1h13m)". It adds ", out in ~28m" when
// the measured pace runs out before the reset, unless the provider's numbers
// are over 15 minutes old: then ", as of 2h ago" says so instead. A window
// whose reset has passed since OMP reported it no longer has a meaningful usage.
func formatOmpQuotaWindow(window ompQuotaWindow, display string, fetchedAt int64, now time.Time) string {
	if window.ResetsAt > 0 && !time.UnixMilli(window.ResetsAt).After(now) {
		return window.Label + " reset"
	}
	percent := ompUsedPercent(window.UsedFraction)
	text := fmt.Sprintf("%s %d%% used", window.Label, percent)
	if display == "left" {
		text = fmt.Sprintf("%s %d%% left", window.Label, max(0, 100-percent))
	}
	var notes []string
	if window.ResetsAt > 0 {
		notes = append(notes, "reset in "+formatOmpWindowDuration(time.UnixMilli(window.ResetsAt).Sub(now)))
	}
	switch {
	case ompQuotaStale(fetchedAt, now):
		notes = append(notes, "as of "+formatOmpWindowDuration(now.Sub(time.UnixMilli(fetchedAt)))+" ago")
	case ompRunsOutBeforeReset(window):
		notes = append(notes, "out in ~"+formatOmpWindowDuration(time.UnixMilli(window.ExhaustsAt).Sub(now)))
	}
	if len(notes) > 0 {
		text += " (" + strings.Join(notes, ", ") + ")"
	}
	return text
}

// ompQuotaStale reports numbers fetched too long ago to describe now.
func ompQuotaStale(fetchedAt int64, now time.Time) bool {
	return fetchedAt > 0 && now.Sub(time.UnixMilli(fetchedAt)) > ompQuotaStaleAfter
}

// ompDisplayStatus is the window's status as shown at now: a window whose
// reset has passed is no longer pressured, old numbers are "stale" unless
// the limit was reached, and one on pace to run out before its reset is a
// warning even below OMP's 90% threshold.
func ompDisplayStatus(window ompQuotaWindow, fetchedAt int64, now time.Time) string {
	if window.ResetsAt > 0 && !time.UnixMilli(window.ResetsAt).After(now) {
		return "ok"
	}
	if ompQuotaStale(fetchedAt, now) {
		if window.Status == "exhausted" {
			return "exhausted"
		}
		return "stale"
	}
	if window.Status == "ok" && ompRunsOutBeforeReset(window) {
		return "warning"
	}
	return window.Status
}

func ompRunsOutBeforeReset(window ompQuotaWindow) bool {
	return window.ExhaustsAt > 0 && window.ExhaustsAt < window.ResetsAt && window.UsedFraction < 1
}

// ompPace projects one account's usage linearly from the share of its window
// that had elapsed when OMP measured it. Early in a window (under 5%, or
// under 15 minutes) the rate is mostly noise, so nothing is projected.
// Example: 88% used with 3h20m of a 5h window gone projects 132% by the
// reset; the remaining 12% lasts about 27 minutes at that rate.
func ompPace(window ompQuotaWindow, measuredAtMs int64) (projected float64, exhaustsAtMs int64) {
	if window.DurationMs <= 0 || window.ResetsAt <= measuredAtMs || measuredAtMs <= 0 ||
		window.UsedFraction <= 0 || window.UsedFraction >= 1 {
		return 0, 0
	}
	elapsedMs := window.DurationMs - (window.ResetsAt - measuredAtMs)
	minimumMs := max(window.DurationMs/20, (15 * time.Minute).Milliseconds())
	if elapsedMs < minimumMs || elapsedMs > window.DurationMs {
		return 0, 0
	}
	projected = window.UsedFraction * float64(window.DurationMs) / float64(elapsedMs)
	if projected >= 1 {
		exhaustsAtMs = measuredAtMs + int64((1-window.UsedFraction)/window.UsedFraction*float64(elapsedMs))
	}
	return projected, exhaustsAtMs
}

// ompUsedPercent rounds for display without claiming a limit is reached, or
// untouched, before it is.
func ompUsedPercent(used float64) int {
	percent := int(math.Round(used * 100))
	switch {
	case percent >= 100 && used < 1:
		return 99
	case percent > 999:
		return 999
	}
	return percent
}

func ompUsedFraction(amount ompUsageAmount) (float64, bool) {
	if amount.UsedFraction != nil && ompNonNegative(*amount.UsedFraction) {
		return *amount.UsedFraction, true
	}
	if amount.RemainingFraction != nil {
		if remaining := *amount.RemainingFraction; !math.IsNaN(remaining) && remaining >= 0 && remaining <= 1 {
			return 1 - remaining, true
		}
	}
	if amount.Remaining != nil && amount.Limit != nil && *amount.Limit > 0 {
		used := (*amount.Limit - *amount.Remaining) / *amount.Limit
		if ompNonNegative(used) {
			return used, true
		}
	}
	return 0, false
}

// ompNonNegative accepts overage fractions (above 1) and amounts, but not
// negative or non-finite values.
func ompNonNegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

// ompLimitStatus keeps OMP's own verdict and falls back to its thresholds.
func ompLimitStatus(status string, used float64) string {
	switch status {
	case "ok", "warning", "exhausted":
		return status
	}
	switch {
	case used >= 1:
		return "exhausted"
	case used >= 0.9:
		return "warning"
	}
	return "ok"
}

// aggregateOmpStatus mirrors OMP's account aggregate: a mix of healthy and
// pressured accounts reads as a warning, not as the worst account's status.
func aggregateOmpStatus(statuses []string) string {
	var ok, warning, exhausted bool
	for _, status := range statuses {
		switch status {
		case "ok":
			ok = true
		case "warning":
			warning = true
		case "exhausted":
			exhausted = true
		}
	}
	switch {
	case ok && (warning || exhausted):
		return "warning"
	case ok:
		return "ok"
	case warning:
		return "warning"
	case exhausted:
		return "exhausted"
	}
	return "ok"
}

// The count is capped at four digits so the span cannot overflow a Duration.
var ompWindowText = regexp.MustCompile(`^(\d{1,4})\s*(m|mins?|minutes?|h|hrs?|hours?|d|days?|w|wks?|weeks?)$`)

// ompTextSpan reads a window length from text such as "5 Hour", "7d" or
// "30 days"; it is zero when the text names no length.
func ompTextSpan(text string) time.Duration {
	match := ompWindowText.FindStringSubmatch(strings.ToLower(strings.TrimSpace(text)))
	if match == nil {
		return 0
	}
	count, err := strconv.Atoi(match[1])
	if err != nil || count <= 0 {
		return 0
	}
	unit := time.Minute
	switch match[2][0] {
	case 'h':
		unit = time.Hour
	case 'd':
		unit = 24 * time.Hour
	case 'w':
		unit = 7 * 24 * time.Hour
	}
	return time.Duration(count) * unit
}

// ompWindowLabel names a limit by its length ("5h", "7d") so every provider
// reads alike, suffixed with its tier or model when the bucket is scoped. The
// span comes from durationMs, else from a label such as "5 Hour"; it is zero
// when neither says.
func ompWindowLabel(limit ompUsageLimit) (label string, span time.Duration, scoped bool) {
	window := limit.Window
	if durationMs := ompMillis(window.DurationMs); durationMs > 0 {
		span = time.Duration(durationMs) * time.Millisecond
	}
	for _, text := range []string{window.ID, window.Label} {
		if span > 0 {
			break
		}
		span = ompTextSpan(text)
	}
	if span > 0 {
		label = formatOmpWindowSpan(span)
	} else {
		label = safeOmpLabel(window.Label, safeOmpLabel(window.ID, "quota"))
	}
	if tier := ompTitle(limit.Scope.Tier); tier != "" {
		return label + " " + tier, span, true
	}
	if model := safeOmpLabel(limit.Scope.ModelID, ""); model != "" {
		return label + " " + model, span, true
	}
	return label, span, false
}

// formatOmpWindowSpan names a window length in its largest whole unit.
func formatOmpWindowSpan(span time.Duration) string {
	day := 24 * time.Hour
	switch {
	case span >= day && span%day == 0:
		return fmt.Sprintf("%dd", span/day)
	case span >= time.Hour && span%time.Hour == 0:
		return fmt.Sprintf("%dh", span/time.Hour)
	case span >= day:
		return fmt.Sprintf("%dd", int(math.Round(span.Hours()/24)))
	case span >= time.Hour:
		return fmt.Sprintf("%dh", int(math.Round(span.Hours())))
	}
	return fmt.Sprintf("%dm", max(1, int(math.Round(span.Minutes()))))
}

// ompTitle turns an id such as "fable" or "pro_lite" into "Fable" / "Pro Lite".
func ompTitle(value string) string {
	words := strings.FieldsFunc(safeOmpLabel(value, ""), func(char rune) bool {
		return char == ' ' || char == '_' || char == '-'
	})
	for index, word := range words {
		runes := []rune(word)
		runes[0] = unicode.ToUpper(runes[0])
		words[index] = string(runes)
	}
	return strings.Join(words, " ")
}

// safeOmpLabel keeps a label printable and free of the statusline's own
// separators (" | ", " / ", " · "), capped at 40 characters.
func safeOmpLabel(value, fallback string) string {
	var label strings.Builder
	count := 0
	for _, char := range strings.TrimSpace(value) {
		if count >= 40 {
			break
		}
		if unicode.IsLetter(char) || unicode.IsDigit(char) || strings.ContainsRune(" _-.()+:@'", char) {
			label.WriteRune(char)
			count++
		}
	}
	cleaned := strings.Join(strings.Fields(label.String()), " ")
	if cleaned == "" {
		return fallback
	}
	return cleaned
}

func formatOmpWindowDuration(duration time.Duration) string {
	minutes := int(math.Ceil(duration.Minutes()))
	if minutes < 1 {
		return "1m"
	}
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}

	hours := minutes / 60
	minutes %= 60
	if hours < 24 {
		if minutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, minutes)
	}

	days := hours / 24
	hours %= 24
	if hours == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, hours)
}
