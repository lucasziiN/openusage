package pi

import (
	"context"

	"github.com/janekbaraniewski/openusage/internal/core"
)

// ItemizedUsage returns one event per recorded turn (assistant messages and
// OMP side calls), reusing the same transcript parsing as Fetch. Reports then
// bucket turns by their real local timestamps instead of the snapshot's UTC
// day series.
func (p *Provider) ItemizedUsage() ([]core.UsageEvent, error) {
	entries, err := readAllSessions(context.Background(), resolveSessionsDirs(core.AccountConfig{}))
	if err != nil {
		return nil, err
	}
	out := make([]core.UsageEvent, 0, len(entries))
	for _, e := range entries {
		out = append(out, core.UsageEvent{
			Time:                e.Timestamp,
			ProviderID:          p.ID(),
			Model:               e.Model,
			Project:             e.WorkspaceLabel,
			Session:             e.SessionID,
			InputTokens:         int(e.Input),
			OutputTokens:        int(e.Output),
			CacheReadTokens:     int(e.CacheRead),
			CacheCreationTokens: int(e.CacheWrite),
			CostUSD:             e.CostUSD,
			HasCost:             e.HasCost,
		})
	}
	return out, nil
}
