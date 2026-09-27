package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestOmpStatuslineConfigPersistsSelectionsAndCancelDoesNotSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "statusline.json")
	t.Setenv("OPENUSAGE_OMP_STATUSLINE_CONFIG", path)
	model := newOmpConfigModel(defaultOmpStatuslineConfig())
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeySpace})
	model = updated.(ompConfigModel)
	if model.selected["session"] || strings.Contains(model.View(), "$12.40 sess") {
		t.Fatal("toggling session should remove its cost from the live preview")
	}
	resized, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = resized.(ompConfigModel)
	if preview := model.View(); !strings.Contains(preview, "\n        🐙 ChatGPT") ||
		!strings.Contains(preview, "\n        🐙 Claude") {
		t.Fatalf("wrapped preview lost quota details: %q", preview)
	}
	cancelled, _ := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !cancelled.(ompConfigModel).cancelled {
		t.Fatal("escape should cancel")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cancel should leave config untouched: %v", err)
	}

	config := model.config()
	if err := saveOmpStatuslineConfig(config); err != nil {
		t.Fatal(err)
	}
	loaded, err := readOmpStatuslineConfig()
	if err != nil || !reflect.DeepEqual(loaded, config) {
		t.Fatalf("loaded = %#v error = %v; want %#v", loaded, err, config)
	}
}

func TestOmpModelCostsStayUnderTheirProviderAndRespectSelection(t *testing.T) {
	price, zero := 7.20, 0.0
	input := ompLineInput{
		ModelCosts: []ompModelCost{
			{Provider: "openai-codex", Model: "GPT-6-Sol", CostUSD: &price},
			{Provider: "anthropic", Model: "Opus", CostUSD: nil},
			{Provider: "anthropic", Model: "Sonnet", CostUSD: &zero},
			{Provider: "gemini", Model: "Other", CostUSD: &price},
		},
	}
	config := defaultOmpStatuslineConfig()
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	result := renderOmpStatus(input, config, now)
	if len(result.Quotas) != 4 ||
		!strings.Contains(result.Quotas[1], "GPT-6-Sol $7.20") ||
		!strings.Contains(result.Quotas[3], "Opus n/a · Sonnet $0.00") {
		t.Fatalf("provider/model grouping: %#v", result.Quotas)
	}
	if strings.Contains(strings.Join(result.Quotas, "\n"), "Other") {
		t.Fatal("unrelated provider was attributed to ChatGPT or Claude")
	}
	if strings.Contains(result.Status, "🤖") || strings.Contains(result.Status, "🧠") {
		t.Fatal("OMP's native model/context must not be duplicated")
	}
	config.Segments = []string{"claude", "models"}
	result = renderOmpStatus(input, config, now)
	if len(result.Quotas) != 2 || strings.Contains(strings.Join(result.Quotas, "\n"), "GPT-6-Sol") {
		t.Fatalf("disabled provider must hide its model row: %#v", result.Quotas)
	}
	config.Segments = []string{"claude"}
	if result = renderOmpStatus(input, config, now); len(result.Quotas) != 1 {
		t.Fatalf("disabled breakdown must hide model rows: %#v", result.Quotas)
	}
	config.Segments = []string{}
	if result = renderOmpStatus(input, config, now); result.Status != "" || len(result.Quotas) != 0 {
		t.Fatalf("empty selection must show nothing: %#v", result)
	}
}

func TestOmpConfigMigratesDuplicatedNativeSegments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "statusline.json")
	t.Setenv("OPENUSAGE_OMP_STATUSLINE_CONFIG", path)
	if err := os.WriteFile(path, []byte(`{"segments":["model","context","window5h","session","claude"],"color":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := readOmpStatuslineConfig()
	if err != nil || !reflect.DeepEqual(config.Segments, []string{"session", "claude", "models"}) || config.Color {
		t.Fatalf("migration lost user choices: %#v %v", config, err)
	}
}

func TestOmpConfigOptionRowsToggleAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "statusline.json")
	t.Setenv("OPENUSAGE_OMP_STATUSLINE_CONFIG", path)
	start := defaultOmpStatuslineConfig()
	start.AlertThresholds = []int{80}
	model := newOmpConfigModel(start)
	press := func(keys ...tea.KeyMsg) {
		for _, key := range keys {
			updated, _ := model.Update(key)
			model = updated.(ompConfigModel)
		}
	}
	down := tea.KeyMsg{Type: tea.KeyDown}
	for range ompSegmentDefs {
		press(down) // to the Color row
	}
	press(down, tea.KeyMsg{Type: tea.KeyRight}) // Quota alerts off
	press(down, tea.KeyMsg{Type: tea.KeyLeft})  // Show quota as left
	if view := model.View(); !strings.Contains(view, "5h 85% left") || !strings.Contains(view, "⚠ Elevated error rates for Codex") {
		t.Fatalf("preview should show quota left and the sample incident: %q", view)
	}
	press(down, tea.KeyMsg{Type: tea.KeySpace}) // Provider status off
	if view := model.View(); strings.Contains(view, "⚠") {
		t.Fatalf("provider status off must drop the incident from the preview: %q", view)
	}
	press(down, tea.KeyMsg{Type: tea.KeyEnter}) // Apply
	if !model.done || model.cancelled {
		t.Fatal("enter on Apply should finish")
	}

	want := start
	want.Alerts, want.QuotaDisplay, want.ProviderStatus = false, "left", false
	if config := model.config(); !reflect.DeepEqual(config, want) {
		t.Fatalf("config = %#v, want %#v", config, want)
	}
	if err := saveOmpStatuslineConfig(model.config()); err != nil {
		t.Fatal(err)
	}
	if loaded, err := readOmpStatuslineConfig(); err != nil || !reflect.DeepEqual(loaded, want) {
		t.Fatalf("loaded = %#v error = %v; want %#v", loaded, err, want)
	}
}

func TestReadOmpStatuslineConfigDefaultsAndValidation(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		edit    func(*ompStatuslineConfig) // applied to the defaults
		wantErr bool
	}{
		{
			name: "a file from before these options keeps alerts and provider status on",
			file: `{"segments":["claude"],"color":false}`,
			edit: func(config *ompStatuslineConfig) { config.Segments, config.Color = []string{"claude"}, false },
		},
		{
			name: "explicit choices",
			file: `{"alerts":false,"alertThresholds":[50,80,95],"quotaDisplay":"left","providerStatus":false}`,
			edit: func(config *ompStatuslineConfig) {
				config.Alerts, config.AlertThresholds = false, []int{50, 80, 95}
				config.QuotaDisplay, config.ProviderStatus = "left", false
			},
		},
		{name: "null thresholds take the defaults", file: `{"alertThresholds":null}`},
		{
			name: "no thresholds",
			file: `{"alertThresholds":[]}`,
			edit: func(config *ompStatuslineConfig) { config.AlertThresholds = []int{} },
		},
		{name: "descending thresholds", file: `{"alertThresholds":[90,75]}`, wantErr: true},
		{name: "repeated threshold", file: `{"alertThresholds":[75,75]}`, wantErr: true},
		{name: "threshold 0", file: `{"alertThresholds":[0,50]}`, wantErr: true},
		{name: "threshold 100", file: `{"alertThresholds":[100]}`, wantErr: true},
		{name: "fractional threshold", file: `{"alertThresholds":[75.5]}`, wantErr: true},
		{name: "unknown display", file: `{"quotaDisplay":"remaining"}`, wantErr: true},
		{name: "empty display", file: `{"quotaDisplay":""}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "statusline.json")
			t.Setenv("OPENUSAGE_OMP_STATUSLINE_CONFIG", path)
			if err := os.WriteFile(path, []byte(test.file), 0o600); err != nil {
				t.Fatal(err)
			}
			config, err := readOmpStatuslineConfig()
			if test.wantErr {
				if err == nil {
					t.Fatalf("accepted %s as %#v", test.file, config)
				}
				return
			}
			want := defaultOmpStatuslineConfig()
			if test.edit != nil {
				test.edit(&want)
			}
			if err != nil || !reflect.DeepEqual(config, want) {
				t.Fatalf("config = %#v error = %v; want %#v", config, err, want)
			}
		})
	}
}
