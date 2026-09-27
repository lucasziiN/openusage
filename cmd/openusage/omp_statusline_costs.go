package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/janekbaraniewski/openusage/internal/providers/pi"
	"github.com/spf13/cobra"
)

// ompCosts is the recorded local Pi/OMP spend the extension reads.
type ompCosts struct {
	Schema      int              `json:"schema"`
	Block       *ompBlockDetails `json:"block,omitempty"`
	TodayUSD    *float64         `json:"todayUSD,omitempty"`
	LastHourUSD float64          `json:"lastHourUSD"`
	Windows     []ompWindowSpend `json:"windows,omitempty"`
	// Embedded so the history fields sit at the top level and are all
	// absent without --days, instead of each reading as empty.
	*ompCostHistory
}

type ompCostHistory struct {
	Days []ompDayCost `json:"days"`
	// Hours is today's spend per local clock hour (index 0 = 00:00).
	Hours             []float64        `json:"hours"`
	Models            []ompCostShare   `json:"models"`
	Projects          []ompCostShare   `json:"projects"`
	Sessions          []ompSessionCost `json:"sessions"`
	Tokens            ompTokens        `json:"tokens"`
	MonthUSD          float64          `json:"monthUSD"`
	MonthProjectedUSD float64          `json:"monthProjectedUSD,omitempty"`
}

type ompWindowSpend struct {
	Key     string  `json:"key"`
	CostUSD float64 `json:"costUSD"`
	Turns   int     `json:"turns"`
}

type ompDayCost struct {
	Date     string  `json:"date"`
	CostUSD  float64 `json:"costUSD"`
	Turns    int     `json:"turns"`
	Unpriced int     `json:"unpriced"`
}

type ompCostShare struct {
	Provider string `json:"provider,omitempty"`
	// Subscription is the quota key ("chatgpt", "claude") a model bills against.
	Subscription string  `json:"subscription,omitempty"`
	Name         string  `json:"name"`
	TodayUSD     float64 `json:"todayUSD"`
	WeekUSD      float64 `json:"weekUSD"`
}

type ompSessionCost struct {
	ID             string  `json:"id"`
	Title          string  `json:"title"`
	Project        string  `json:"project"`
	CostUSD        float64 `json:"costUSD"`
	TodayUSD       float64 `json:"todayUSD"`
	SubagentUSD    float64 `json:"subagentUSD"`
	Turns          int     `json:"turns"`
	LastActivityAt int64   `json:"lastActivityAt"`
}

type ompTokens struct {
	Today ompTokenCounts `json:"today"`
	Week  ompTokenCounts `json:"week"`
}

type ompTokenCounts struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
}

// ompMaxSpendWindows bounds --window; the extension asks for one per quota
// window it shows.
const ompMaxSpendWindows = 16

func newOmpStatuslineCostsCommand() *cobra.Command {
	var historyDays int
	var windowFlags []string
	command := &cobra.Command{
		Use:   "costs",
		Short: "Return recorded Pi/OMP costs (active block, today, quota-window spend, optional history) as JSON",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if historyDays < 0 || historyDays > 366 {
				return fmt.Errorf("--days must be between 0 and 366")
			}
			windows, err := parseOmpSpendWindows(windowFlags)
			if err != nil {
				return err
			}
			local, err := pi.LocalCostSummary(cmd.Context(), time.Now(), pi.LocalCostOptions{
				HistoryDays: historyDays, Windows: windows,
			})
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(ompCostsFromLocal(local, historyDays > 0))
		},
	}
	command.Flags().IntVar(&historyDays, "days", 0,
		"also return this many local days of spend, with per-model, per-project and per-session totals")
	command.Flags().StringArrayVar(&windowFlags, "window", nil,
		"also return the spend since a quota window's start, as KEY=SINCE_MS with KEY <subscription>/<label> "+
			"and SINCE_MS in Unix milliseconds (repeatable, at most 16)")
	return command
}

// parseOmpSpendWindows reads --window KEY=SINCE_MS values. A turn bills
// against the window when its provider maps to KEY's subscription, the part
// before the first "/".
func parseOmpSpendWindows(values []string) ([]pi.SpendWindow, error) {
	if len(values) > ompMaxSpendWindows {
		return nil, fmt.Errorf("--window may be given at most %d times", ompMaxSpendWindows)
	}
	var windows []pi.SpendWindow
	for _, value := range values {
		separator := strings.LastIndex(value, "=")
		if separator < 0 {
			return nil, fmt.Errorf("--window %q: want KEY=SINCE_MS", value)
		}
		key, since := value[:separator], value[separator+1:]
		subscription, label, _ := strings.Cut(key, "/")
		if subscription == "" || strings.TrimSpace(label) == "" {
			return nil, fmt.Errorf("--window %q: KEY must be <subscription>/<label>", value)
		}
		millis, err := strconv.ParseInt(since, 10, 64)
		if err != nil || millis < 0 {
			return nil, fmt.Errorf("--window %q: SINCE_MS must be Unix milliseconds", value)
		}
		windows = append(windows, pi.SpendWindow{
			Key:   key,
			Since: time.UnixMilli(millis),
			Match: func(provider string) bool {
				providerKey, _ := ompProviderIdentity(provider)
				return providerKey == subscription
			},
		})
	}
	return windows, nil
}

func ompCostsFromLocal(local pi.LocalCosts, withHistory bool) ompCosts {
	costs := ompCosts{Schema: ompExtensionSchema, LastHourUSD: local.LastHour}
	if local.HasBlock {
		costs.Block = &ompBlockDetails{
			CostUSD: local.Block.Cost, BurnUSDHour: local.Block.BurnRateUSDPerHour,
			StartedAt: local.Block.Start.UnixMilli(), EndsAt: local.Block.End.UnixMilli(),
			LastActivityAt: local.Block.LastActivity.UnixMilli(),
		}
		// Without a burn rate the report's projection is just the cost so far,
		// which would read as a forecast.
		if local.Block.BurnRateUSDPerHour > 0 {
			costs.Block.ProjectedUSD = local.Block.ProjectedCost
		}
	}
	if local.TodayKnown {
		today := local.Today
		costs.TodayUSD = &today
	}
	for _, window := range local.Windows {
		costs.Windows = append(costs.Windows, ompWindowSpend{Key: window.Key, CostUSD: window.CostUSD, Turns: window.Turns})
	}
	if !withHistory {
		return costs
	}

	history := &ompCostHistory{
		Days:     make([]ompDayCost, 0, len(local.Days)),
		Hours:    local.Hours,
		Models:   make([]ompCostShare, 0, len(local.Models)),
		Projects: make([]ompCostShare, 0, len(local.Projects)),
		Sessions: make([]ompSessionCost, 0, len(local.Sessions)),
		Tokens: ompTokens{
			Today: ompTokenCounts(local.TodayTokens),
			Week:  ompTokenCounts(local.WeekTokens),
		},
		MonthUSD:          local.Month,
		MonthProjectedUSD: local.MonthProjected,
	}
	for _, day := range local.Days {
		history.Days = append(history.Days, ompDayCost{
			Date: day.Date, CostUSD: day.CostUSD, Turns: day.Turns, Unpriced: day.Unpriced,
		})
	}
	for _, model := range local.Models {
		subscription, _ := ompProviderIdentity(model.Provider)
		history.Models = append(history.Models, ompCostShare{
			Provider: safeOmpLabel(model.Provider, ""), Subscription: subscription,
			Name:     ompCostText(model.Name, "Unknown model"),
			TodayUSD: model.TodayUSD, WeekUSD: model.WeekUSD,
		})
	}
	for _, project := range local.Projects {
		history.Projects = append(history.Projects, ompCostShare{
			Name: ompCostText(project.Name, "(no project)"), TodayUSD: project.TodayUSD, WeekUSD: project.WeekUSD,
		})
	}
	for _, session := range local.Sessions {
		history.Sessions = append(history.Sessions, ompSessionCost{
			ID: ompCostText(session.ID, ""), Title: ompCostText(session.Title, ""),
			Project: ompCostText(session.Project, "(no project)"),
			CostUSD: session.CostUSD, TodayUSD: session.TodayUSD, SubagentUSD: session.SubagentUSD,
			Turns: session.Turns, LastActivityAt: session.LastActivity.UnixMilli(),
		})
	}
	costs.ompCostHistory = history
	return costs
}

// ompCostTextRunes caps titles, projects and model names in the /usage view.
const ompCostTextRunes = 60

// ompCostText makes a transcript-supplied name safe to show: non-printable
// runes (terminal escapes, bidi overrides, line breaks) become spaces,
// whitespace runs collapse, and the result is cut to ompCostTextRunes runes,
// ending in "…" when cut.
func ompCostText(value, fallback string) string {
	printable := strings.Map(func(char rune) rune {
		if unicode.IsPrint(char) {
			return char
		}
		return ' '
	}, value)
	text := strings.Join(strings.Fields(printable), " ")
	if text == "" {
		return fallback
	}
	if runes := []rune(text); len(runes) > ompCostTextRunes {
		text = strings.TrimRight(string(runes[:ompCostTextRunes-1]), " ") + "…"
	}
	return text
}
