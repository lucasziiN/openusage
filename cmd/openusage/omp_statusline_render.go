package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/janekbaraniewski/openusage/internal/providers/pi"
	"github.com/spf13/cobra"
)

type ompBlockDetails struct {
	CostUSD     float64 `json:"costUSD"`
	SecondsLeft int64   `json:"secondsLeft"`
	BurnUSDHour float64 `json:"burnUSDHour"`
}

type ompModelCost struct {
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	CostUSD  *float64 `json:"costUSD"`
}

type ompLineInput struct {
	SessionCostUSD *float64         `json:"sessionCostUSD"`
	TodayCostUSD   *float64         `json:"todayCostUSD"`
	Block          *ompBlockDetails `json:"block"`
	ModelCosts     []ompModelCost   `json:"modelCosts"`
	ChatGPTQuota   string           `json:"chatgptQuota"`
	ClaudeQuota    string           `json:"claudeQuota"`
}

type ompRenderedStatus struct {
	Status string   `json:"status"`
	Quotas []string `json:"quotas"`
	Color  bool     `json:"color"`
}

func renderOmpStatus(in ompLineInput, config ompStatuslineConfig) ompRenderedStatus {
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
	if in.Block != nil && in.Block.SecondsLeft > 0 && in.Block.CostUSD >= 0 {
		values.haveBlock = true
		values.blockCost = in.Block.CostUSD
		values.blockLeft = time.Duration(in.Block.SecondsLeft) * time.Second
		values.burn = in.Block.BurnUSDHour
	}
	result := ompRenderedStatus{
		Status: assembleStatusline(values, options),
		Quotas: []string{},
		Color:  config.Color,
	}
	providers := [...]struct{ key, name, prefix, quota string }{
		{"chatgpt", "ChatGPT", "Codex", in.ChatGPTQuota},
		{"claude", "Claude", "Claude", in.ClaudeQuota},
	}
	for index, provider := range providers {
		if !options.segmentEnabled(provider.key) {
			continue
		}
		quota := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(provider.quota), provider.prefix))
		if quota == "" {
			quota = "n/a"
		}
		if quota != "n/a" {
			quota = "🕔 " + quota
		}
		result.Quotas = append(result.Quotas, "🐙 "+provider.name+" | "+quota)
		if options.segmentEnabled("models") {
			var costs []string
			for _, model := range in.ModelCosts {
				if ompProviderIndex(model.Provider) != index {
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

func previewOmpStatusline(config ompStatuslineConfig) ompRenderedStatus {
	session, today, blockCost := 12.40, 18.70, 8.30
	sol, codex, opus, sonnet := 7.20, 1.10, 3.60, 0.50
	return renderOmpStatus(ompLineInput{
		SessionCostUSD: &session, TodayCostUSD: &today,
		Block:        &ompBlockDetails{CostUSD: blockCost, SecondsLeft: 2*3600 + 41*60, BurnUSDHour: 1.20},
		ChatGPTQuota: "Codex 5h 15% used",
		ClaudeQuota:  "Claude 5h 28% used",
		ModelCosts: []ompModelCost{
			{Provider: "openai-codex", Model: "GPT-6-Sol", CostUSD: &sol},
			{Provider: "openai-codex", Model: "GPT-5.3 Codex", CostUSD: &codex},
			{Provider: "anthropic", Model: "Opus 4.8", CostUSD: &opus},
			{Provider: "anthropic", Model: "Sonnet", CostUSD: &sonnet},
		},
	}, config)
}

func runOmpStatuslineRender(input io.Reader, output io.Writer, jsonOutput bool) error {
	config, err := readOmpStatuslineConfig()
	if err != nil {
		return err
	}
	var payload ompLineInput
	if err := json.NewDecoder(io.LimitReader(input, 16*1024)).Decode(&payload); err != nil {
		return fmt.Errorf("read OMP statusline values: %w", err)
	}
	rendered := renderOmpStatus(payload, config)
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

func newOmpStatuslineCostsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "costs",
		Short: "Return the priced active Pi/OMP activity block as safe JSON",
		RunE: func(cmd *cobra.Command, _ []string) error {
			now := time.Now()
			block, available, err := pi.ActiveLocalBlock(context.Background(), now)
			if err != nil {
				return err
			}
			if !available {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(struct{}{})
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(ompBlockDetails{
				CostUSD: block.Cost, SecondsLeft: int64(block.TimeRemaining.Seconds()),
				BurnUSDHour: block.BurnRateUSDPerHour,
			})
		},
	}
}
