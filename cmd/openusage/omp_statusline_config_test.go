package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
	result := renderOmpStatus(input, config)
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
	result = renderOmpStatus(input, config)
	if len(result.Quotas) != 2 || strings.Contains(strings.Join(result.Quotas, "\n"), "GPT-6-Sol") {
		t.Fatalf("disabled provider must hide its model row: %#v", result.Quotas)
	}
	config.Segments = []string{"claude"}
	if result = renderOmpStatus(input, config); len(result.Quotas) != 1 {
		t.Fatalf("disabled breakdown must hide model rows: %#v", result.Quotas)
	}
	config.Segments = []string{}
	if result = renderOmpStatus(input, config); result.Status != "" || len(result.Quotas) != 0 {
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
