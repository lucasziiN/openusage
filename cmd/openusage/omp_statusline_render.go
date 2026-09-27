package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
)

// ompBlockDetails is the active local activity block. Times are Unix
// milliseconds so countdowns are computed when a row is rendered.
type ompBlockDetails struct {
	CostUSD        float64 `json:"costUSD"`
	BurnUSDHour    float64 `json:"burnUSDHour"`
	ProjectedUSD   float64 `json:"projectedUSD"`
	StartedAt      int64   `json:"startedAt"`
	EndsAt         int64   `json:"endsAt"`
	LastActivityAt int64   `json:"lastActivityAt"`
}

type ompModelCost struct {
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	CostUSD  *float64 `json:"costUSD"`
}

// ompVendorStatus is a provider's status-page incident as the extension
// read it.
type ompVendorStatus struct {
	Indicator string `json:"indicator"` // minor, major, critical or maintenance
	Title     string `json:"title"`
}

type ompLineInput struct {
	SessionCostUSD *float64         `json:"sessionCostUSD"`
	TodayCostUSD   *float64         `json:"todayCostUSD"`
	Block          *ompBlockDetails `json:"block"`
	ModelCosts     []ompModelCost   `json:"modelCosts"`
	Quotas         *ompQuotaSummary `json:"quotas"`
	// Statuses is keyed by quota provider key ("chatgpt", "claude").
	Statuses map[string]ompVendorStatus `json:"statuses"`
}

type ompRenderedStatus struct {
	Schema int      `json:"schema"`
	Status string   `json:"status"`
	Quotas []string `json:"quotas"`
	// CellStatus maps a quota row cell ("🕔 5h 95% used …") to "warning",
	// "exhausted" or "stale" so the extension can colour it by theme.
	CellStatus map[string]string `json:"cellStatus,omitempty"`
	Color      bool              `json:"color"`
	// QuotaDisplay ("used" or "left") and ProviderStatus echo the config, so
	// the extension words its own views alike and skips status-page polling
	// nobody sees.
	QuotaDisplay   string `json:"quotaDisplay"`
	ProviderStatus bool   `json:"providerStatus"`
}

func renderOmpStatus(in ompLineInput, config ompStatuslineConfig, now time.Time) ompRenderedStatus {
	selected := config.Segments
	if len(selected) == 0 {
		selected = []string{"none"} // statuslineOptions uses nil/empty to mean all
	}
	options := statuslineOptions{
		color: config.Color, contextMedium: 50, contextHigh: 80,
		segments: selected, onlyKnownCosts: true,
	}
	values := statuslineValues{}
	if in.SessionCostUSD != nil && *in.SessionCostUSD >= 0 {
		values.sessionCost, values.sessionKnown = *in.SessionCostUSD, true
	}
	if in.TodayCostUSD != nil && *in.TodayCostUSD >= 0 {
		values.todayCost, values.todayKnown = *in.TodayCostUSD, true
		values.todayLabel = "today est"
	}
	if block := in.Block; block != nil && block.CostUSD >= 0 {
		if left := time.UnixMilli(block.EndsAt).Sub(now); left > 0 {
			values.haveBlock = true
			values.blockCost = block.CostUSD
			values.blockLeft = left
			values.burn = block.BurnUSDHour
		}
	}
	display := config.QuotaDisplay
	if display != "left" {
		display = "used"
	}
	result := ompRenderedStatus{
		Schema:         ompExtensionSchema,
		Status:         assembleStatusline(values, options),
		Quotas:         []string{},
		Color:          config.Color,
		QuotaDisplay:   display,
		ProviderStatus: config.ProviderStatus,
	}
	mark := func(cell, status string) {
		if result.CellStatus == nil {
			result.CellStatus = map[string]string{}
		}
		result.CellStatus[cell] = status
	}
	var quotas ompQuotaSummary
	if in.Quotas != nil {
		quotas = *in.Quotas
	}
	for _, entry := range ompStatuslineProviders {
		if !options.segmentEnabled(entry.key) {
			continue
		}
		provider := quotas.provider(entry.key)
		cells := []string{"🐙 " + entry.name}
		windows := ompStatuslineWindows(provider)
		for _, window := range windows {
			icon := "🕔"
			if window.DurationMs >= int64(24*time.Hour/time.Millisecond) {
				icon = "📅"
			}
			cell := icon + " " + formatOmpQuotaWindow(window, display, provider.FetchedAt, now)
			switch status := ompDisplayStatus(window, provider.FetchedAt, now); status {
			case "warning", "exhausted", "stale":
				mark(cell, status)
			}
			cells = append(cells, cell)
		}
		if len(windows) == 0 {
			cell := ompNoWindowText(provider)
			if provider.Disabled > 0 {
				mark(cell, "warning")
			}
			cells = append(cells, cell)
		}
		if config.ProviderStatus {
			if cell, status := ompVendorStatusCell(in.Statuses[entry.key]); cell != "" {
				mark(cell, status)
				cells = append(cells, cell)
			}
		}
		result.Quotas = append(result.Quotas, strings.Join(cells, " | "))
		if options.segmentEnabled("models") {
			var costs []string
			for _, model := range in.ModelCosts {
				if key, _ := ompProviderIdentity(model.Provider); key != entry.key {
					continue
				}
				price := "n/a"
				if model.CostUSD != nil && *model.CostUSD >= 0 {
					price = fmt.Sprintf("$%.2f", *model.CostUSD)
				}
				costs = append(costs, safeOmpLabel(model.Model, "Unknown model")+" "+price)
			}
			if len(costs) > 0 {
				result.Quotas = append(result.Quotas, "   "+strings.Join(costs, " · "))
			}
		}
	}
	return result
}

// ompVendorStatusCell is the "⚠ <incident>" cell for a status-page
// indicator, levelled like a quota cell so an outage reads as exhausted.
// Unknown indicators (such as "none") show nothing.
func ompVendorStatusCell(status ompVendorStatus) (cell, level string) {
	indicator := strings.ToLower(strings.TrimSpace(status.Indicator))
	switch indicator {
	case "minor", "maintenance":
		level = "warning"
	case "major", "critical":
		level = "exhausted"
	default:
		return "", ""
	}
	title := ompCellText(status.Title, 32)
	if title == "" {
		title = "service issue"
		if indicator == "maintenance" {
			title = "maintenance"
		}
	}
	return "⚠ " + title, level
}

// ompCellText keeps free text such as an incident title printable, on one
// line and clear of the " | " cell separator, cut to limit runes.
func ompCellText(value string, limit int) string {
	cleaned := strings.Map(func(char rune) rune {
		switch {
		case char == '|':
			return '/'
		case unicode.IsSpace(char):
			return ' '
		case !unicode.IsPrint(char):
			return -1
		}
		return char
	}, value)
	runes := []rune(strings.Join(strings.Fields(cleaned), " "))
	if len(runes) <= limit {
		return string(runes)
	}
	return strings.TrimRight(string(runes[:limit-1]), " ") + "…"
}

// previewOmpStatusline renders the configurator's sample at a fixed instant.
func previewOmpStatusline(config ompStatuslineConfig) ompRenderedStatus {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	session, today := 12.40, 18.70
	sol, codex, opus, sonnet := 7.20, 1.10, 3.60, 0.50
	window := func(label string, span time.Duration, used float64, resetIn time.Duration) ompQuotaWindow {
		return ompQuotaWindow{
			Label: label, DurationMs: span.Milliseconds(), UsedFraction: used,
			ResetsAt: now.Add(resetIn).UnixMilli(), Status: ompLimitStatus("", used), Accounts: 1,
		}
	}
	return renderOmpStatus(ompLineInput{
		SessionCostUSD: &session, TodayCostUSD: &today,
		Block: &ompBlockDetails{
			CostUSD: 8.30, BurnUSDHour: 1.20,
			StartedAt: now.Add(-2*time.Hour - 19*time.Minute).UnixMilli(),
			EndsAt:    now.Add(2*time.Hour + 41*time.Minute).UnixMilli(),
		},
		Quotas: &ompQuotaSummary{Providers: []ompProviderQuota{
			{Key: "chatgpt", Name: "ChatGPT", Accounts: 1, Windows: []ompQuotaWindow{
				window("5h", 5*time.Hour, 0.15, 3*time.Hour+10*time.Minute),
				window("7d", 7*24*time.Hour, 0.42, 4*24*time.Hour+6*time.Hour),
			}},
			{Key: "claude", Name: "Claude", Accounts: 1, Windows: []ompQuotaWindow{
				window("5h", 5*time.Hour, 0.28, 1*time.Hour+45*time.Minute),
				window("7d", 7*24*time.Hour, 0.61, 2*24*time.Hour+9*time.Hour),
			}},
		}},
		// Shows what the provider status option adds to a row.
		Statuses: map[string]ompVendorStatus{
			"chatgpt": {Indicator: "minor", Title: "Elevated error rates for Codex"},
		},
		ModelCosts: []ompModelCost{
			{Provider: "openai-codex", Model: "GPT-6-Sol", CostUSD: &sol},
			{Provider: "openai-codex", Model: "GPT-5.3 Codex", CostUSD: &codex},
			{Provider: "anthropic", Model: "Opus 4.8", CostUSD: &opus},
			{Provider: "anthropic", Model: "Sonnet", CostUSD: &sonnet},
		},
	}, config, now)
}

func runOmpStatuslineRender(input io.Reader, output io.Writer, jsonOutput bool) error {
	config, err := readOmpStatuslineConfig()
	if err != nil {
		return err
	}
	var payload ompLineInput
	if err := json.NewDecoder(io.LimitReader(input, 1<<20)).Decode(&payload); err != nil {
		return fmt.Errorf("read OMP statusline values: %w", err)
	}
	rendered := renderOmpStatus(payload, config, time.Now())
	if jsonOutput {
		return json.NewEncoder(output).Encode(rendered)
	}
	if rendered.Status != "" {
		if _, err := fmt.Fprintln(output, rendered.Status); err != nil {
			return err
		}
	}
	for _, quota := range rendered.Quotas {
		if _, err := fmt.Fprintln(output, quota); err != nil {
			return err
		}
	}
	return nil
}
