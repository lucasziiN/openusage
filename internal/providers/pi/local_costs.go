package pi

import (
	"context"
	"sort"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/report"
)

// LocalCostOptions selects what LocalCostSummary adds to the active block,
// today and the last hour.
type LocalCostOptions struct {
	// HistoryDays > 0 fills Days (that many local days, oldest first, ending
	// today), Hours, Models, Projects, Sessions, the token totals and the
	// month figures.
	HistoryDays int
	// Windows asks for the recorded spend since each window's start.
	Windows []SpendWindow
	// TopSessions caps Sessions; 0 means 8.
	TopSessions int
}

// SpendWindow is a quota window to total recorded spend for.
type SpendWindow struct {
	Key   string // echoed back, e.g. "claude/5h"
	Since time.Time
	// Match reports whether turns recorded under an OMP provider id bill
	// against this window; nil matches every provider. It is called once per
	// distinct provider.
	Match func(provider string) bool
}

// WindowSpend is the recorded spend since a SpendWindow's start.
type WindowSpend struct {
	Key     string
	CostUSD float64
	Turns   int
}

// TokenTotals sums the token buckets of recorded turns.
type TokenTotals struct{ Input, Output, CacheRead, CacheWrite int64 }

func (totals *TokenTotals) add(entry piModelEntry) {
	totals.Input += entry.Input
	totals.Output += entry.Output
	totals.CacheRead += entry.CacheRead
	totals.CacheWrite += entry.CacheWrite
}

// SessionCost is a session's recorded spend over the last seven local days,
// today included, with the subagent and /tan transcripts it spawned rolled in.
type SessionCost struct {
	ID, Title, Project string // Title may be ""; Project is the workspace label
	CostUSD            float64
	TodayUSD           float64
	// SubagentUSD is the part of CostUSD recorded in transcripts under the
	// session's artifacts directory.
	SubagentUSD  float64
	Turns        int
	LastActivity time.Time
}

// LocalCosts is the recorded Pi/OMP spend behind the OMP statusline and its
// /usage view. The active block and today's total are withheld when a turn
// they cover has usage but no recorded price, since a partial sum would read
// as the total; the other figures add up the prices that were recorded.
type LocalCosts struct {
	// Block is the active five-hour activity block; valid when HasBlock.
	Block    report.Row
	HasBlock bool
	// Today covers the local calendar day of now; valid when TodayKnown.
	Today      float64
	TodayKnown bool
	// LastHour is the spend recorded in (now-60m, now].
	LastHour float64
	// Windows has one entry per requested SpendWindow, in the same order.
	Windows []WindowSpend

	// The rest is filled only when history was requested.
	Days []DayCost
	// Hours is today's recorded spend per local clock hour, 0 through 23.
	Hours    []float64
	Models   []CostShare
	Projects []CostShare
	// Sessions are sorted by CostUSD, highest first; only sessions that
	// spent in the last seven days, at most TopSessions of them.
	Sessions    []SessionCost
	TodayTokens TokenTotals
	WeekTokens  TokenTotals // last seven local days, today included
	// Month is the spend since the local 1st of the month, 00:00.
	Month float64
	// MonthProjected scales Month to the whole month at the pace so far; 0
	// until two local days of the month have passed, when a pace means little.
	MonthProjected float64
}

// DayCost is the recorded spend of one local calendar day.
type DayCost struct {
	Date     string // YYYY-MM-DD
	CostUSD  float64
	Turns    int
	Unpriced int // turns with usage but no recorded price
}

// CostShare is a model's or project's recorded spend today and over the last
// seven local days, today included.
type CostShare struct {
	Provider string // models only
	Name     string
	TodayUSD float64
	WeekUSD  float64
}

const defaultTopSessions = 8

// LocalCostSummary reads local Pi and OMP sessions once. Calendar days follow
// now's location.
func LocalCostSummary(ctx context.Context, now time.Time, opts LocalCostOptions) (LocalCosts, error) {
	entries, err := readAllSessions(ctx, resolveSessionsDirs(core.AccountConfig{}))
	if err != nil {
		return LocalCosts{}, err
	}
	return summarizeLocalCosts(entries, now, opts), nil
}

func summarizeLocalCosts(entries []piModelEntry, now time.Time, opts LocalCostOptions) LocalCosts {
	var costs LocalCosts
	costs.Block, costs.HasBlock = activeBlock(entries, now)

	location := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	weekStart := today.AddDate(0, 0, -6)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, location)
	hourAgo := now.Add(-time.Hour)
	history := opts.HistoryDays > 0

	if len(opts.Windows) > 0 {
		costs.Windows = make([]WindowSpend, len(opts.Windows))
		for i, window := range opts.Windows {
			costs.Windows[i].Key = window.Key
		}
	}
	windowMatches := make(map[string][]bool)
	matchWindows := func(provider string) []bool {
		matches, ok := windowMatches[provider]
		if !ok {
			matches = make([]bool, len(opts.Windows))
			for i, window := range opts.Windows {
				matches[i] = window.Match == nil || window.Match(provider)
			}
			windowMatches[provider] = matches
		}
		return matches
	}

	var historyStart time.Time
	dayIndex := make(map[string]int)
	if history {
		historyStart = today.AddDate(0, 0, 1-opts.HistoryDays)
		for offset := opts.HistoryDays - 1; offset >= 0; offset-- {
			date := today.AddDate(0, 0, -offset).Format(time.DateOnly)
			dayIndex[date] = len(costs.Days)
			costs.Days = append(costs.Days, DayCost{Date: date})
		}
		costs.Hours = make([]float64, 24)
	}
	models := make(map[string]*CostShare)
	projects := make(map[string]*CostShare)
	share := func(shares map[string]*CostShare, key, provider, name string) *CostShare {
		value, ok := shares[key]
		if !ok {
			value = &CostShare{Provider: provider, Name: name}
			shares[key] = value
		}
		return value
	}
	sessions := make(map[*piTranscript]*SessionCost)

	todayUnpriced := false
	for _, entry := range entries {
		at := entry.Timestamp
		if at.After(hourAgo) && !at.After(now) {
			costs.LastHour += entry.CostUSD
		}
		if len(opts.Windows) > 0 {
			for i, matches := range matchWindows(entry.Provider) {
				if matches && !at.Before(opts.Windows[i].Since) {
					costs.Windows[i].CostUSD += entry.CostUSD
					costs.Windows[i].Turns++
				}
			}
		}
		local := at.In(location)
		isToday := !local.Before(today)
		if isToday {
			costs.Today += entry.CostUSD
			todayUnpriced = todayUnpriced || !entry.HasCost
		}
		if !history {
			continue
		}
		if !local.Before(monthStart) {
			costs.Month += entry.CostUSD
		}
		if isToday {
			costs.Hours[local.Hour()] += entry.CostUSD
			costs.TodayTokens.add(entry)
		}
		if !local.Before(historyStart) {
			if index, ok := dayIndex[local.Format(time.DateOnly)]; ok {
				day := &costs.Days[index]
				day.Turns++
				day.CostUSD += entry.CostUSD
				if !entry.HasCost {
					day.Unpriced++
				}
			}
		}
		if local.Before(weekStart) {
			continue
		}
		costs.WeekTokens.add(entry)
		addSessionCost(sessions, entry, isToday)
		if entry.CostUSD == 0 {
			continue
		}
		project := entry.WorkspaceLabel
		if project == "" {
			project = "(no project)"
		}
		for _, target := range []*CostShare{
			share(models, entry.Provider+"/"+entry.Model, entry.Provider, entry.Model),
			share(projects, project, "", project),
		} {
			target.WeekUSD += entry.CostUSD
			if isToday {
				target.TodayUSD += entry.CostUSD
			}
		}
	}
	costs.TodayKnown = !todayUnpriced
	if todayUnpriced {
		costs.Today = 0
	}
	if !history {
		return costs
	}
	costs.Models = sortedCostShares(models)
	costs.Projects = sortedCostShares(projects)
	costs.Sessions = topSessions(sessions, opts.TopSessions)
	// Calendar days, not 48 hours: a DST switch makes two local days 47 or 49 hours.
	if !now.Before(monthStart.AddDate(0, 0, 2)) {
		month := monthStart.AddDate(0, 1, 0).Sub(monthStart)
		costs.MonthProjected = costs.Month * float64(month) / float64(now.Sub(monthStart))
	}
	return costs
}

// activeBlock returns the five-hour activity block now falls in, withheld
// when any turn in it has usage but no recorded price.
func activeBlock(entries []piModelEntry, now time.Time) (report.Row, bool) {
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
		return report.Row{}, false
	}
	for _, entry := range entries {
		if !entry.HasCost && !entry.Timestamp.Before(block.Start) && entry.Timestamp.Before(block.End) {
			return report.Row{}, false
		}
	}
	return block, true
}

// addSessionCost bills a turn from the last seven days to the session the
// user started: subagent and /tan transcripts roll up into it.
func addSessionCost(sessions map[*piTranscript]*SessionCost, entry piModelEntry, isToday bool) {
	transcript := entry.transcript
	if transcript == nil {
		return
	}
	root := transcript.root
	if root == nil {
		root = transcript
	}
	session, ok := sessions[root]
	if !ok {
		session = &SessionCost{ID: root.Meta.SessionID, Title: root.Meta.Title, Project: root.Meta.WorkspaceLabel}
		sessions[root] = session
	}
	session.CostUSD += entry.CostUSD
	session.Turns++
	if isToday {
		session.TodayUSD += entry.CostUSD
	}
	if transcript != root {
		session.SubagentUSD += entry.CostUSD
	}
	if entry.Timestamp.After(session.LastActivity) {
		session.LastActivity = entry.Timestamp
	}
}

func topSessions(sessions map[*piTranscript]*SessionCost, limit int) []SessionCost {
	if limit <= 0 {
		limit = defaultTopSessions
	}
	out := make([]SessionCost, 0, len(sessions))
	for _, session := range sessions {
		if session.CostUSD > 0 {
			out = append(out, *session)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CostUSD != out[j].CostUSD {
			return out[i].CostUSD > out[j].CostUSD
		}
		if !out[i].LastActivity.Equal(out[j].LastActivity) {
			return out[i].LastActivity.After(out[j].LastActivity)
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func sortedCostShares(shares map[string]*CostShare) []CostShare {
	if len(shares) == 0 {
		return nil
	}
	out := make([]CostShare, 0, len(shares))
	for _, share := range shares {
		out = append(out, *share)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WeekUSD != out[j].WeekUSD {
			return out[i].WeekUSD > out[j].WeekUSD
		}
		if out[i].TodayUSD != out[j].TodayUSD {
			return out[i].TodayUSD > out[j].TodayUSD
		}
		return out[i].Provider+out[i].Name < out[j].Provider+out[j].Name
	})
	return out
}
