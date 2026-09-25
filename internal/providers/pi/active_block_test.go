package pi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestActiveLocalBlockRequiresPricedTurns(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	root := filepath.Join(home, ".omp", "agent", "sessions")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "session.jsonl")
	priced := `{"type":"session","id":"omp-1"}
{"type":"message","timestamp":"2026-01-02T10:00:00Z","message":{"role":"assistant","model":"gpt","usage":{"input":100,"cost":{"total":1}}}}
{"type":"message","timestamp":"2026-01-02T11:00:00Z","message":{"role":"assistant","model":"claude","usage":{"input":200,"cost":{"total":2}}}}
`
	if err := os.WriteFile(path, []byte(priced), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	block, ok, err := ActiveLocalBlock(context.Background(), now)
	if err != nil || !ok {
		t.Fatalf("active block = %v, %v, %v", block, ok, err)
	}
	if block.Cost != 3 || block.TimeRemaining != 3*time.Hour || block.BurnRateUSDPerHour != 3 {
		t.Fatalf("wrong active block: cost %v remaining %v burn %v", block.Cost, block.TimeRemaining, block.BurnRateUSDPerHour)
	}
	unpriced := `{"type":"message","timestamp":"2026-01-02T11:30:00Z","message":{"role":"assistant","model":"gpt","usage":{"input":100}}}
`
	if err := os.WriteFile(path, []byte(priced+unpriced), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ActiveLocalBlock(context.Background(), now); err != nil || ok {
		t.Fatalf("incomplete block must be hidden: available=%v error=%v", ok, err)
	}
}
