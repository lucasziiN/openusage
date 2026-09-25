package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestRenderOmpStatuslineShowsCodexAndClaudeIndependently(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	output := []byte(fmt.Sprintf(`{
		"metadata":{"accountEmail":"private@example.com","accountId":"account-secret"},
		"reports":[
			{"provider":"gemini-cli","limits":[{"window":{"label":"5h"},"amount":{"remainingFraction":0.01}}]},
			{"provider":"codex","limits":[{"window":{"label":"5h"},"amount":{"remainingFraction":0.8}}]},
			{"provider":"openai-codex","limits":[
				{"window":{"label":"7 days","resetsAt":%d},"amount":{"remainingFraction":0.72}},
				{"window":{"label":"5h","resetsAt":%d},"amount":{"remainingFraction":0.35}}
			]},
			{"provider":"claude_code","limits":[{"window":{"label":"5h"},"amount":{"remainingFraction":0.3}}]},
			{"provider":"anthropic","metadata":{"email":"private@example.com"},"limits":[
				{"window":{"label":"7d","resetsAt":%d},"amount":{"remainingFraction":0.9}},
				{"window":{"label":"5 hours","resetsAt":%d},"amount":{"usedFraction":0.82}}
			]}
		]
	}`, now.Add(24*time.Hour).UnixMilli(), now.Add(2*time.Hour).UnixMilli(),
		now.Add(24*time.Hour).UnixMilli(), now.Add(time.Hour).UnixMilli()))

	status := renderOmpStatusline(output, nil, now)
	if want := "🐙 Codex 5h 65% used (reset in 2h) | Claude 5h 82% used (reset in 1h)"; status != want {
		t.Fatalf("status = %q, want %q", status, want)
	}
	for _, privateValue := range []string{"private@example.com", "account-secret"} {
		if strings.Contains(status, privateValue) {
			t.Errorf("status exposed private value %q: %q", privateValue, status)
		}
	}
}

func TestRenderOmpStatuslineDistinguishesMissingClaudeFromZeroUsage(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	output := []byte(`{"reports":[
		{"provider":"openai-codex","limits":[{"window":{"label":"7 days"},"amount":{"usedFraction":0.6}}]},
		{"provider":"claude_code","limits":[{"window":{"label":"5h"},"amount":{"remainingFraction":1.5}}]}
	]}`)

	status := renderOmpStatusline(output, nil, now)
	if want := "🐙 Codex 7d 60% used | Claude n/a"; status != want {
		t.Fatalf("status = %q, want %q", status, want)
	}
}

func TestRenderOmpStatuslineShowsClaudeWithoutCodex(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	output := []byte(`{"reports":[{"provider":"claude","limits":[
		{"window":{"durationMs":18000000},"amount":{"remaining":80,"limit":100}}
	]}]}`)

	status := renderOmpStatusline(output, nil, now)
	if want := "🐙 Codex n/a | Claude 5h 20% used"; status != want {
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
			want:   "openusage: quota unavailable (no Codex or Claude data)",
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

func TestSummarizeOmpQuotasKeepsFiveHourSeparateFromTightestWindow(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	raw := []byte(`{"metadata":{"email":"private@example.com"},"reports":[
		{"provider":"codex","limits":[
			{"window":{"label":"7 days"},"amount":{"remainingFraction":0.4}},
			{"window":{"label":"5 hours"},"amount":{"remainingFraction":0.85}}
		]},
		{"provider":"anthropic","limits":[
			{"window":{"label":"5 hours"},"amount":{"remainingFraction":1}}
		]}
	]}`)
	summary := summarizeOmpQuotas(raw, nil, now)
	if summary.Codex != "Codex 7d 60% used" || summary.Claude != "Claude 5h 0% used" {
		t.Fatalf("provider windows = %#v", summary)
	}
	if summary.Codex5hPct == nil || *summary.Codex5hPct != 15 ||
		summary.Claude5hPct == nil || *summary.Claude5hPct != 0 {
		t.Fatalf("five-hour windows = %#v", summary)
	}
	encoded, err := json.Marshal(summary)
	if err != nil || strings.Contains(string(encoded), "private@example.com") {
		t.Fatalf("private metadata escaped into display JSON: %q, error %v", encoded, err)
	}
}
