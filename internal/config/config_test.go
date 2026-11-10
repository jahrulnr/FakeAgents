package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jahrulnr/fakeagents/internal/config"
)

func TestDefaultIsValid(t *testing.T) {
	cfg := config.Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
	if len(cfg.Models) == 0 {
		t.Fatal("expected default models")
	}
	if len(cfg.Scenarios) == 0 {
		t.Fatal("expected default scenarios")
	}
	if cfg.Server.Port != 11110 {
		t.Fatalf("unexpected port %d", cfg.Server.Port)
	}
}

func TestLoadJSONMergesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"server":{"port":9999},"defaults":{"model":"custom-model"}}`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.Port != 9999 {
		t.Fatalf("port = %d", cfg.Server.Port)
	}
	if cfg.Defaults.Model != "custom-model" {
		t.Fatalf("model = %q", cfg.Defaults.Model)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Fatalf("default host lost: %q", cfg.Server.Host)
	}
	if len(cfg.Models) == 0 || len(cfg.Scenarios) == 0 {
		t.Fatal("defaults were not merged")
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"server":{"porrt":9000}}`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected unknown field error")
	}
}

func TestLoadJSONKeepsBooleanOptionsIndependent(t *testing.T) {
	cfg, err := config.LoadReader(strings.NewReader(`{
		"defaults": {"latency": {"enabled": false}},
		"debug": {"enabled": true, "headers": false}
	}`), "json")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.Latency.IsEnabled() || !cfg.Debug.IsEnabled() || *cfg.Debug.Headers {
		t.Fatalf("unexpected latency or debug settings: %+v %+v", cfg.Defaults.Latency, cfg.Debug)
	}
	if !cfg.Server.CORS.IsEnabled() || !cfg.Encryption.ValidateReplayEnabled() {
		t.Fatal("an omitted option lost its default")
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"server":{"port":70000}}`)
	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "port") {
		t.Fatalf("expected port validation error, got %v", err)
	}
}

func TestLoadYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, "server:\n  port: 8123\ndefaults:\n  model: yaml-model\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load yaml: %v", err)
	}
	if cfg.Server.Port != 8123 || cfg.Defaults.Model != "yaml-model" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestScenarioFilesAndFileReferences(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "scenarios"))
	writeFile(t, filepath.Join(dir, "prompt.txt"), "Hello {{ .UserMessage }}")
	writeFile(t, filepath.Join(dir, "scenarios", "billing.json"), `[
	  {"name":"custom-billing","match":{"triggers":["custom-billing"]},"respond":{"type":"error","error":{"preset":"billing"}}}
	]`)
	writeFile(t, filepath.Join(dir, "config.json"), `{
	  "scenario_files": ["scenarios/*.json"],
	  "templates": {"greeting": "@file:prompt.txt"},
	  "scenarios": [
	    {"name":"inline","respond":{"type":"text","text_template":"greeting"}}
	  ]
	}`)
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Templates["greeting"] != "Hello {{ .UserMessage }}" {
		t.Fatalf("file reference not resolved: %q", cfg.Templates["greeting"])
	}
	names := make(map[string]bool)
	for _, spec := range cfg.Scenarios {
		names[spec.Name] = true
	}
	if !names["inline"] || !names["custom-billing"] {
		t.Fatalf("custom scenarios missing: %v", names)
	}
}

func TestMissingFileReferenceFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.json"), `{"templates":{"x":"@file:missing.txt"}}`)
	if _, err := config.Load(filepath.Join(dir, "config.json")); err == nil {
		t.Fatal("expected missing file error")
	}
}

func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeagents.json")
	cfg := config.Default()
	cfg.Server.Port = 9111
	if err := config.Save(path, cfg, "json"); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Server.Port != 9111 {
		t.Fatalf("port = %d", loaded.Server.Port)
	}
}

func TestRedacted(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Keys = []config.APIKey{{Key: "sk-super-secret-key"}}
	cfg.Encryption.Key = "secret-encryption-key"
	redacted := cfg.Redacted()
	if strings.Contains(redacted.Auth.Keys[0].Key, "super-secret") {
		t.Fatalf("auth key was not redacted: %s", redacted.Auth.Keys[0].Key)
	}
	if strings.Contains(redacted.Encryption.Key, "secret") {
		t.Fatalf("encryption key was not redacted: %s", redacted.Encryption.Key)
	}
	if cfg.Auth.Keys[0].Key != "sk-super-secret-key" {
		t.Fatal("redacted mutated the original config")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func TestScenariosAreAdditive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"scenarios":[{"name":"mine","respond":{"type":"text","text":"custom"}}]}`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	names := make(map[string]bool)
	for _, spec := range cfg.Scenarios {
		names[spec.Name] = true
	}
	if !names["mine"] {
		t.Fatal("custom scenario missing")
	}
	if !names["billing"] {
		t.Fatal("built-in scenario should remain available")
	}
}

func TestReplaceBuiltinScenarios(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{
	  "replace_builtin_scenarios": true,
	  "scenarios": [{"name":"only","respond":{"type":"text","text":"custom"}}]
	}`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Scenarios) != 1 || cfg.Scenarios[0].Name != "only" {
		t.Fatalf("scenarios = %+v", cfg.Scenarios)
	}
}

func TestReplaceBuiltinScenariosAloneClears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"replace_builtin_scenarios": true}`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Scenarios) != 0 {
		t.Fatalf("expected no scenarios, got %d", len(cfg.Scenarios))
	}
}

func TestMultipleJSONValuesRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"server":{"port":9000}} {"server":{"port":9001}}`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected multiple-value error")
	}
}
