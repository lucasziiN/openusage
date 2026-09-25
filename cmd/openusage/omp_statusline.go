package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"
)

const ompUsageTimeout = 10 * time.Second

// OMP returns additional account metadata alongside reports. These types name
// only the report fields needed for a quota status, so metadata never reaches
// the status line.
type ompUsageResponse struct {
	Reports []ompUsageReport `json:"reports"`
}

type ompUsageReport struct {
	Provider string          `json:"provider"`
	Limits   []ompUsageLimit `json:"limits"`
}

type ompUsageLimit struct {
	Window ompUsageWindow `json:"window"`
	Amount ompUsageAmount `json:"amount"`
}

type ompUsageWindow struct {
	Label      string `json:"label"`
	ResetsAt   int64  `json:"resetsAt"`
	DurationMs int64  `json:"durationMs"`
}

type ompUsageAmount struct {
	Limit             *float64 `json:"limit"`
	Remaining         *float64 `json:"remaining"`
	UsedFraction      *float64 `json:"usedFraction"`
	RemainingFraction *float64 `json:"remainingFraction"`
}

type ompQuotaStatus struct {
	window            string
	remainingFraction float64
	resetsAtMillis    int64
}

// The JSON form is intentionally limited to display values. OMP's response
// also contains account metadata, which must never reach an extension status.
type ompQuotaSummary struct {
	Codex       string   `json:"codex"`
	Claude      string   `json:"claude"`
	Codex5hPct  *float64 `json:"codex5hPct,omitempty"`
	Claude5hPct *float64 `json:"claude5hPct,omitempty"`
	Error       string   `json:"error,omitempty"`
}

func newOmpStatuslineCommand() *cobra.Command {
	var jsonOutput, render bool
	command := &cobra.Command{
		Use:   "omp-statusline",
		Short: "Show OMP quotas or configure the OpenUsage OMP footer",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if render {
				return runOmpStatuslineRender(cmd.InOrStdin(), cmd.OutOrStdout(), jsonOutput)
			}
			output, commandErr := runOmpUsage(context.Background())
			if !jsonOutput {
				fmt.Fprintln(cmd.OutOrStdout(), renderOmpStatusline(output, commandErr, time.Now()))
				return nil
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(summarizeOmpQuotas(output, commandErr, time.Now()))
		},
	}
	command.Flags().BoolVar(&jsonOutput, "json", false, "emit display-safe quota fields for the OMP extension")
	command.Flags().BoolVar(&render, "render", false, "render selected OMP status and quota rows from extension JSON on stdin")
	command.AddCommand(newOmpStatuslineInstallCommand(), newOmpStatuslineCostsCommand())
	return command
}

func runOmpUsage(parent context.Context) ([]byte, error) {
	executable, err := exec.LookPath("omp")
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(parent, ompUsageTimeout)
	defer cancel()

	command := exec.CommandContext(ctx, executable, "usage", "--json", "--redact")
	return command.Output()
}

func renderOmpStatusline(output []byte, commandErr error, now time.Time) string {
	if commandErr != nil {
		var executableError *exec.Error
		if errors.As(commandErr, &executableError) && errors.Is(executableError.Err, exec.ErrNotFound) {
			return "openusage: quota unavailable (omp not found)"
		}
		return "openusage: quota unavailable (omp usage failed)"
	}

	var response ompUsageResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return "openusage: quota unavailable (invalid omp JSON)"
	}

	quotas, available := selectOmpQuotas(response)
	if !available[0] && !available[1] {
		return "openusage: quota unavailable (no Codex or Claude data)"
	}

	segments := [2]string{"Codex n/a", "Claude n/a"}
	if available[0] {
		segments[0] = formatOmpQuota("Codex", quotas[0], now)
	}
	if available[1] {
		segments[1] = formatOmpQuota("Claude", quotas[1], now)
	}
	return "🐙 " + strings.Join(segments[:], " | ")
}

func summarizeOmpQuotas(output []byte, commandErr error, now time.Time) ompQuotaSummary {
	summary := ompQuotaSummary{Codex: "Codex n/a", Claude: "Claude n/a"}
	if commandErr != nil {
		summary.Error = renderOmpStatusline(output, commandErr, now)
		return summary
	}
	var response ompUsageResponse
	if err := json.Unmarshal(output, &response); err != nil {
		summary.Error = "openusage: quota unavailable (invalid omp JSON)"
		return summary
	}
	quotas, available := selectOmpQuotas(response)
	if !available[0] && !available[1] {
		summary.Error = "openusage: quota unavailable (no Codex or Claude data)"
		return summary
	}
	if available[0] {
		summary.Codex = formatOmpQuota("Codex", quotas[0], now)
	}
	if available[1] {
		summary.Claude = formatOmpQuota("Claude", quotas[1], now)
	}
	for _, report := range response.Reports {
		index := ompProviderIndex(report.Provider)
		if index < 0 {
			continue
		}
		for _, limit := range report.Limits {
			if ompWindowLabel(limit.Window) != "5h" {
				continue
			}
			fraction, ok := ompRemainingFraction(limit.Amount)
			if !ok {
				continue
			}
			percent := math.Round((1 - fraction) * 100)
			if index == 0 && (summary.Codex5hPct == nil || percent > *summary.Codex5hPct) {
				summary.Codex5hPct = &percent
			}
			if index == 1 && (summary.Claude5hPct == nil || percent > *summary.Claude5hPct) {
				summary.Claude5hPct = &percent
			}
		}
	}
	return summary
}

// Select the tightest valid window independently for each tool. An unrelated
// provider must not displace either quota from the OMP footer.
func selectOmpQuotas(response ompUsageResponse) ([2]ompQuotaStatus, [2]bool) {
	var quotas [2]ompQuotaStatus
	var available [2]bool

	for _, report := range response.Reports {
		index := ompProviderIndex(report.Provider)
		if index < 0 {
			continue
		}
		for _, limit := range report.Limits {
			fraction, ok := ompRemainingFraction(limit.Amount)
			if !ok {
				continue
			}

			if !available[index] || fraction < quotas[index].remainingFraction {
				quotas[index] = ompQuotaStatus{
					window:            ompWindowLabel(limit.Window),
					remainingFraction: fraction,
					resetsAtMillis:    limit.Window.ResetsAt,
				}
				available[index] = true
			}
		}
	}
	return quotas, available
}

func ompProviderIndex(provider string) int {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex", "openai-codex":
		return 0
	case "anthropic", "claude", "claude-code", "claude_code":
		return 1
	default:
		return -1
	}
}

func formatOmpQuota(provider string, quota ompQuotaStatus, now time.Time) string {
	usedPercent := math.Round((1 - quota.remainingFraction) * 100)
	status := fmt.Sprintf("%s %s %.0f%% used", provider, quota.window, usedPercent)
	if quota.resetsAtMillis > 0 {
		status += " (reset " + formatOmpReset(quota.resetsAtMillis, now) + ")"
	}
	return status
}

func ompRemainingFraction(amount ompUsageAmount) (float64, bool) {
	if amount.RemainingFraction != nil && validOmpFraction(*amount.RemainingFraction) {
		return *amount.RemainingFraction, true
	}
	if amount.UsedFraction != nil && validOmpFraction(*amount.UsedFraction) {
		return 1 - *amount.UsedFraction, true
	}
	if amount.Remaining != nil && amount.Limit != nil && *amount.Limit > 0 {
		fraction := *amount.Remaining / *amount.Limit
		if validOmpFraction(fraction) {
			return fraction, true
		}
	}
	return 0, false
}

func validOmpFraction(fraction float64) bool {
	return !math.IsNaN(fraction) && !math.IsInf(fraction, 0) && fraction >= 0 && fraction <= 1
}

func ompWindowLabel(window ompUsageWindow) string {
	switch strings.ToLower(strings.TrimSpace(window.Label)) {
	case "5 hours":
		return "5h"
	case "7 days":
		return "7d"
	}
	if label := safeOmpLabel(window.Label, ""); label != "" {
		return label
	}
	if window.DurationMs > 0 {
		return formatOmpWindowDuration(time.Duration(window.DurationMs) * time.Millisecond)
	}
	return "quota"
}

func safeOmpLabel(value, fallback string) string {
	var label strings.Builder
	for _, char := range strings.TrimSpace(value) {
		if unicode.IsLetter(char) || unicode.IsDigit(char) || strings.ContainsRune(" _-.", char) {
			label.WriteRune(char)
		}
		if label.Len() >= 24 {
			break
		}
	}
	cleaned := strings.TrimSpace(label.String())
	if cleaned == "" {
		return fallback
	}
	return cleaned
}

func formatOmpReset(resetsAtMillis int64, now time.Time) string {
	if resetsAtMillis <= 0 {
		return "unknown"
	}

	resetAt := time.UnixMilli(resetsAtMillis)
	if !resetAt.After(now) {
		return "now"
	}
	return "in " + formatOmpWindowDuration(resetAt.Sub(now))
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
