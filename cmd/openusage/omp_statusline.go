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
	provider          string
	window            string
	remainingFraction float64
	resetsAtMillis    int64
}

func newOmpStatuslineCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "omp-statusline",
		Short: "Print OMP quota remaining and reset time for a status line",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			output, err := runOmpUsage(context.Background())
			fmt.Fprintln(cmd.OutOrStdout(), renderOmpStatusline(output, err, time.Now()))
		},
	}
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

	status, found := selectOmpQuota(response)
	if !found {
		return "openusage: quota unavailable (no quota data)"
	}

	remainingPercent := math.Round(status.remainingFraction * 100)
	reset := formatOmpReset(status.resetsAtMillis, now)
	return fmt.Sprintf("openusage %s %s %.0f%% left; reset %s", status.provider, status.window, remainingPercent, reset)
}

func selectOmpQuota(response ompUsageResponse) (ompQuotaStatus, bool) {
	var selected ompQuotaStatus
	found := false

	for _, report := range response.Reports {
		for _, limit := range report.Limits {
			fraction, ok := ompRemainingFraction(limit.Amount)
			if !ok {
				continue
			}

			candidate := ompQuotaStatus{
				provider:          safeOmpLabel(report.Provider, "provider"),
				window:            ompWindowLabel(limit.Window),
				remainingFraction: fraction,
				resetsAtMillis:    limit.Window.ResetsAt,
			}
			if !found || candidate.remainingFraction < selected.remainingFraction {
				selected = candidate
				found = true
			}
		}
	}

	return selected, found
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
