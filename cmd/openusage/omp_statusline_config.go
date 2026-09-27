package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

var ompSegmentDefs = []struct{ key, label string }{
	{"session", "Session cost"},
	{"today", "Today's cost"},
	{"block", "5h block cost + time left"},
	{"burn", "Burn rate"},
	{"chatgpt", "ChatGPT quota"},
	{"claude", "Claude quota"},
	{"models", "Per-model session costs"},
}

// ompConfigOptions are the ←/→ rows listed after the segment checkboxes.
var ompConfigOptions = []string{"Color", "Quota alerts", "Show quota as", "Provider status"}

type ompStatuslineConfig struct {
	Segments []string `json:"segments"`
	Color    bool     `json:"color"`
	// Alerts turns quota notifications on; AlertThresholds are the used
	// percentages (ascending, 1–99) that notify before a limit is reached.
	Alerts          bool   `json:"alerts"`
	AlertThresholds []int  `json:"alertThresholds"`
	QuotaDisplay    string `json:"quotaDisplay"` // "used" or "left"
	// ProviderStatus shows vendor status-page incidents in the quota rows.
	ProviderStatus bool `json:"providerStatus"`
}

func defaultOmpStatuslineConfig() ompStatuslineConfig {
	config := ompStatuslineConfig{
		Color: true, Alerts: true, AlertThresholds: []int{75, 90},
		QuotaDisplay: "used", ProviderStatus: true,
	}
	for _, segment := range ompSegmentDefs {
		config.Segments = append(config.Segments, segment.key)
	}
	return config
}

func ompStatuslineConfigPath() string {
	if override := strings.TrimSpace(os.Getenv("OPENUSAGE_OMP_STATUSLINE_CONFIG")); override != "" {
		return override
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".omp", "agent", "openusage-statusline.json")
}

func readOmpStatuslineConfig() (ompStatuslineConfig, error) {
	config := defaultOmpStatuslineConfig()
	data, err := os.ReadFile(ompStatuslineConfigPath())
	if os.IsNotExist(err) {
		return config, nil
	}
	if err != nil {
		return config, err
	}
	// Keys missing from an older file keep their defaults, so alerts and
	// provider status start on for existing users too.
	if err := json.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("read OMP statusline config: %w", err)
	}
	if config.AlertThresholds == nil { // an explicit null
		config.AlertThresholds = defaultOmpStatuslineConfig().AlertThresholds
	}
	// Migrate saved choices from the duplicated model/context layout without
	// changing OMP settings or re-enabling a deliberately empty selection.
	var migrated []string
	legacy := false
	for _, key := range config.Segments {
		switch key {
		case "model", "context", "window5h":
			legacy = true
		default:
			migrated = append(migrated, key)
		}
	}
	if legacy {
		config.Segments = append(migrated, "models")
	}
	return validateOmpStatuslineConfig(config)
}

func validateOmpStatuslineConfig(config ompStatuslineConfig) (ompStatuslineConfig, error) {
	valid := make(map[string]bool, len(ompSegmentDefs))
	for _, segment := range ompSegmentDefs {
		valid[segment.key] = true
	}
	seen := make(map[string]bool, len(config.Segments))
	for _, key := range config.Segments {
		if !valid[key] || seen[key] {
			return config, fmt.Errorf("invalid or repeated OMP statusline segment %q", key)
		}
		seen[key] = true
	}
	for index, threshold := range config.AlertThresholds {
		if threshold < 1 || threshold > 99 || (index > 0 && threshold <= config.AlertThresholds[index-1]) {
			return config, fmt.Errorf("invalid OMP statusline alertThresholds %v: use ascending percentages from 1 to 99", config.AlertThresholds)
		}
	}
	if config.QuotaDisplay != "used" && config.QuotaDisplay != "left" {
		return config, fmt.Errorf("invalid OMP statusline quotaDisplay %q: use \"used\" or \"left\"", config.QuotaDisplay)
	}
	return config, nil
}

func saveOmpStatuslineConfig(config ompStatuslineConfig) error {
	if _, err := validateOmpStatuslineConfig(config); err != nil {
		return err
	}
	return writeJSONObjectWithBackup(ompStatuslineConfigPath(), map[string]any{
		"segments":        config.Segments,
		"color":           config.Color,
		"alerts":          config.Alerts,
		"alertThresholds": config.AlertThresholds,
		"quotaDisplay":    config.QuotaDisplay,
		"providerStatus":  config.ProviderStatus,
	})
}

func newOmpStatuslineInstallCommand() *cobra.Command {
	var segmentNames []string
	var color = true
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Configure the OMP footer with a live preview (interactive on a TTY)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, err := readOmpStatuslineConfig()
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("segments") && !cmd.Flags().Changed("color") && isStdinTerminal() && isStdoutTerminal() {
				model, runErr := tea.NewProgram(newOmpConfigModel(config)).Run()
				if runErr != nil {
					return runErr
				}
				selected := model.(ompConfigModel)
				if selected.cancelled {
					fmt.Fprintln(cmd.OutOrStdout(), "OMP statusline: install cancelled.")
					return nil
				}
				config = selected.config()
			} else {
				if cmd.Flags().Changed("segments") {
					config.Segments = segmentNames
				}
				if cmd.Flags().Changed("color") {
					config.Color = color
				}
			}
			if err := saveOmpStatuslineConfig(config); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Saved OMP statusline choices to %s\n", ompStatuslineConfigPath())
			fmt.Fprintln(cmd.OutOrStdout(), "Restart OMP if the OpenUsage extension is not already loaded.")
			return nil
		},
	}
	keys := make([]string, 0, len(ompSegmentDefs))
	for _, segment := range ompSegmentDefs {
		keys = append(keys, segment.key)
	}
	cmd.Flags().StringSliceVar(&segmentNames, "segments", nil, "segments to show: "+strings.Join(keys, ",")+" (empty shows none)")
	cmd.Flags().BoolVar(&color, "color", true, "colorize the preview and OMP widget")
	return cmd
}

type ompConfigModel struct {
	selected       map[string]bool
	color          bool
	alerts         bool
	quotaDisplay   string
	providerStatus bool
	// thresholds has no row; it is carried so applying keeps a custom list.
	thresholds []int
	cursor     int
	width      int
	done       bool
	cancelled  bool
}

func newOmpConfigModel(config ompStatuslineConfig) ompConfigModel {
	model := ompConfigModel{
		selected: make(map[string]bool), color: config.Color, alerts: config.Alerts,
		quotaDisplay: config.QuotaDisplay, providerStatus: config.ProviderStatus,
		thresholds: config.AlertThresholds, width: 96,
	}
	if model.quotaDisplay != "left" {
		model.quotaDisplay = "used"
	}
	for _, key := range config.Segments {
		model.selected[key] = true
	}
	return model
}

func (model ompConfigModel) config() ompStatuslineConfig {
	config := ompStatuslineConfig{
		Segments: []string{}, Color: model.color, Alerts: model.alerts,
		AlertThresholds: model.thresholds, QuotaDisplay: model.quotaDisplay,
		ProviderStatus: model.providerStatus,
	}
	for _, segment := range ompSegmentDefs {
		if model.selected[segment.key] {
			config.Segments = append(config.Segments, segment.key)
		}
	}
	return config
}

// option is the index into ompConfigOptions under the cursor, if any.
func (model ompConfigModel) option() (int, bool) {
	index := model.cursor - len(ompSegmentDefs)
	return index, index >= 0 && index < len(ompConfigOptions)
}

func (model ompConfigModel) optionValue(index int) string {
	onOff := func(on bool) string {
		if on {
			return "on"
		}
		return "off"
	}
	switch index {
	case 0:
		return onOff(model.color)
	case 1:
		return onOff(model.alerts)
	case 2:
		return model.quotaDisplay
	}
	return onOff(model.providerStatus)
}

func (model *ompConfigModel) toggleOption(index int) {
	switch index {
	case 0:
		model.color = !model.color
	case 1:
		model.alerts = !model.alerts
	case 2:
		if model.quotaDisplay == "left" {
			model.quotaDisplay = "used"
		} else {
			model.quotaDisplay = "left"
		}
	default:
		model.providerStatus = !model.providerStatus
	}
}

func (model ompConfigModel) Init() tea.Cmd { return nil }

func (model ompConfigModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := message.(tea.WindowSizeMsg); ok {
		model.width = size.Width
		return model, nil
	}
	key, ok := message.(tea.KeyMsg)
	if !ok {
		return model, nil
	}
	applyRow := len(ompSegmentDefs) + len(ompConfigOptions)
	switch key.String() {
	case "ctrl+c", "q", "esc":
		model.done, model.cancelled = true, true
		return model, tea.Quit
	case "up", "k":
		if model.cursor > 0 {
			model.cursor--
		}
	case "down", "j":
		if model.cursor < applyRow {
			model.cursor++
		}
	case "left", "h", "right", "l":
		if option, ok := model.option(); ok {
			model.toggleOption(option)
		}
	case " ", "x", "enter":
		if model.cursor == applyRow {
			if key.String() == "enter" {
				model.done = true
				return model, tea.Quit
			}
		} else if option, ok := model.option(); ok {
			model.toggleOption(option)
		} else {
			segment := ompSegmentDefs[model.cursor]
			model.selected[segment.key] = !model.selected[segment.key]
		}
	}
	return model, nil
}

// Keep every separator visible even when the sample is wider than a terminal.
func wrapOmpPreview(line string, width int) string {
	const label = "preview "
	const continuation = "        "
	var wrapped strings.Builder
	wrapped.WriteString(label)
	currentWidth := len(label)
	appendPart := func(separator, part string) {
		if width > 0 && currentWidth+lipgloss.Width(separator+part) > width && currentWidth > len(label) {
			wrapped.WriteString("\n" + continuation)
			currentWidth = len(continuation)
			separator = strings.TrimLeft(separator, " ")
		}
		wrapped.WriteString(separator + part)
		currentWidth += lipgloss.Width(separator + part)
	}
	for index, group := range strings.Split(line, " | ") {
		for pieceIndex, part := range strings.Split(group, " / ") {
			separator := " / "
			if pieceIndex == 0 {
				separator = ""
				if index > 0 {
					separator = " | "
				}
			}
			appendPart(separator, part)
		}
	}
	return wrapped.String()
}

func (model ompConfigModel) View() string {
	if model.done {
		return ""
	}
	accent := lipgloss.NewStyle().Foreground(lipgloss.Color("#FF8433")).Bold(true)
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("#828592"))
	selected := lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF")).Bold(true)
	var view strings.Builder
	view.WriteString(accent.Render("Configure your OMP OpenUsage statusline") + "\n\n")
	sample := previewOmpStatusline(model.config())
	firstLine := sample.Status
	if firstLine == "" && len(sample.Quotas) > 0 {
		firstLine = sample.Quotas[0]
		sample.Quotas = sample.Quotas[1:]
	}
	if firstLine == "" {
		firstLine = "(select at least one segment)"
	}
	view.WriteString(dim.Render("preview ") + strings.TrimPrefix(wrapOmpPreview(firstLine, model.width), "preview ") + "\n")
	for _, quota := range sample.Quotas {
		view.WriteString("        " + quota + "\n")
	}
	view.WriteString("\n")
	for index, segment := range ompSegmentDefs {
		line := checkbox(model.selected[segment.key]) + " " + segment.label
		if index == model.cursor {
			line = accent.Render("› ") + selected.Render(line)
		} else {
			line = "  " + line
		}
		view.WriteString(line + "\n")
	}
	for index, label := range ompConfigOptions {
		line := fmt.Sprintf("%-16s %s", label, accent.Render("‹ "+model.optionValue(index)+" ›"))
		if model.cursor == len(ompSegmentDefs)+index {
			line = accent.Render("› ") + selected.Render(line)
		} else {
			line = "  " + line
		}
		view.WriteString(line + "\n")
	}
	if model.cursor == len(ompSegmentDefs)+len(ompConfigOptions) {
		view.WriteString(accent.Render("› [ Apply ]") + "\n")
	} else {
		view.WriteString("  " + accent.Render("[ Apply ]") + "\n")
	}
	view.WriteString("\n" + dim.Render("↑/↓ move · ←/→ change · space toggle · enter apply · q cancel"))
	return view.String()
}
