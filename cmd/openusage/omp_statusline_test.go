package main

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestRenderOmpStatuslineSelectsMostConstrainedQuota(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	resetAt := now.Add(2 * time.Hour).UnixMilli()
	output := []byte(fmt.Sprintf(`{
		"metadata":{"accountEmail":"private@example.com","accountId":"account-secret"},
		"reports":[
			{"provider":"codex","limits":[{"window":{"label":"7d","durationMs":604800000,"resetsAt":%d},"amount":{"remainingFraction":0.81}}]},
			{"provider":"openai-codex","fetchedAt":"private timestamp","limits":[
				{"label":"weekly","scope":"private@example.com","window":{"label":"7d","durationMs":604800000,"resetsAt":%d},"amount":{"remainingFraction":0.72}},
				{"label":"session","scope":"account-secret","window":{"label":"5h","durationMs":18000000,"resetsAt":%d},"amount":{"remainingFraction":0.35}}
			]}
		]
	}`, now.Add(3*time.Hour).UnixMilli(), now.Add(24*time.Hour).UnixMilli(), resetAt))

	status := renderOmpStatusline(output, nil, now)
	if want := "openusage openai-codex 5h 35% left; reset in 2h"; status != want {
		t.Fatalf("status = %q, want %q", status, want)
	}
	for _, privateValue := range []string{"private@example.com", "account-secret", "private timestamp"} {
		if strings.Contains(status, privateValue) {
			t.Errorf("status exposed private value %q: %q", privateValue, status)
		}
	}
}

func TestRenderOmpStatuslineUsesAvailableAmountsAndResetFallbacks(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	output := []byte(`{"reports":[{"provider":"gemini-cli","limits":[{"window":{"durationMs":604800000},"amount":{"usedFraction":0.6}}]}]}`)

	status := renderOmpStatusline(output, nil, now)
	if want := "openusage gemini-cli 7d 40% left; reset unknown"; status != want {
		t.Fatalf("status = %q, want %q", status, want)
	}
}

func TestRenderOmpStatuslineHandlesUnavailableData(t *testing.T) {
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
			name:   "no quota data",
			output: []byte(`{"reports":[{"provider":"codex","limits":[{"window":{"label":"5h"},"amount":{}}]}]}`),
			want:   "openusage: quota unavailable (no quota data)",
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
			status := renderOmpStatusline(test.output, test.commandErr, now)
			if status != test.want {
				t.Fatalf("status = %q, want %q", status, test.want)
			}
			if strings.Contains(status, "account-secret") {
				t.Fatalf("status exposed command error output: %q", status)
			}
		})
	}
}
