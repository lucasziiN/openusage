package pi

import (
	"context"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/report"
)

// ActiveLocalBlock computes the current five-hour activity block across local
// Pi and OMP sessions. It withholds the price when any turn in that block has
// tokens but no recorded price; a partial cost would look like a total.
func ActiveLocalBlock(ctx context.Context, now time.Time) (report.Row, bool, error) {
	entries, err := readAllSessions(ctx, resolveSessionsDirs(core.AccountConfig{}))
	if err != nil {
		return report.Row{}, false, err
	}
	events := make([]report.Event, 0, len(entries))
	for _, entry := range entries {
		events = append(events, report.Event{
			Time: entry.Timestamp, Provider: ID, Model: entry.Model,
			Session: entry.SessionID, Input: int(entry.Input), Output: int(entry.Output),
			CacheRead: int(entry.CacheRead), CacheCreate: int(entry.CacheWrite), Cost: entry.CostUSD,
		})
	}
	block, active := report.Build(events, report.Options{Kind: report.KindBlocks, Now: now}).ActiveBlock()
	if !active {
		return report.Row{}, false, nil
	}
	for _, entry := range entries {
		if !entry.Timestamp.Before(block.Start) && entry.Timestamp.Before(block.End) && !entry.HasCost {
			return report.Row{}, false, nil
		}
	}
	return block, true, nil
}
