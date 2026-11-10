package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jahrulnr/fakeagents/internal/scenario"
)

// Load reads, expands, and validates a configuration file. An empty path loads
// the built-in defaults.
func Load(path string) (*Config, error) {
	cfg := Default()
	if strings.TrimSpace(path) != "" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve config path: %w", err)
		}
		raw, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", abs, err)
		}
		hasScenarios := hasTopLevelKey(raw, filepath.Ext(abs), "scenarios")
		if hasScenarios {
			cfg.Scenarios = nil
		}
		if err := decode(raw, filepath.Ext(abs), cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", abs, err)
		}
		if cfg.ReplaceBuiltinScenarios {
			if !hasScenarios {
				cfg.Scenarios = nil
			}
		} else if hasScenarios {
			cfg.Scenarios = mergeScenarios(defaultScenarios(), cfg.Scenarios)
		}
		cfg.Dir = filepath.Dir(abs)
		cfg.resolveRelativePaths()
		if err := cfg.loadScenarioFiles(); err != nil {
			return nil, err
		}
		if err := cfg.resolveFileReferences(); err != nil {
			return nil, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadReader is useful for tests and for embedding a configuration in another
// program. format may be "json", "yaml", or "yml".
func LoadReader(r io.Reader, format string) (*Config, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	cfg := Default()
	ext := "." + strings.TrimPrefix(strings.ToLower(format), ".")
	hasScenarios := hasTopLevelKey(raw, ext, "scenarios")
	if hasScenarios {
		cfg.Scenarios = nil
	}
	if err := decode(raw, ext, cfg); err != nil {
		return nil, err
	}
	if cfg.ReplaceBuiltinScenarios {
		if !hasScenarios {
			cfg.Scenarios = nil
		}
	} else if hasScenarios {
		cfg.Scenarios = mergeScenarios(defaultScenarios(), cfg.Scenarios)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save writes cfg as indented JSON or YAML.
func Save(path string, cfg *Config, format string) error {
	var data []byte
	var err error
	switch strings.ToLower(strings.TrimPrefix(format, ".")) {
	case "yaml", "yml":
		data, err = yaml.Marshal(cfg)
	default:
		data, err = json.MarshalIndent(cfg, "", "  ")
		if err == nil {
			data = append(data, '\n')
		}
	}
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

func decode(raw []byte, ext string, cfg *Config) error {
	ext = strings.ToLower(ext)
	if ext == ".yaml" || ext == ".yml" {
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil {
			return err
		}
		var trailing any
		if err := dec.Decode(&trailing); err == nil {
			return errors.New("configuration must contain a single YAML document")
		} else if !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err == nil {
		return errors.New("configuration must contain a single JSON value")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (c *Config) resolveRelativePaths() {
	if c.Dir == "" {
		return
	}
	if c.Server.TLS.CertFile != "" && !filepath.IsAbs(c.Server.TLS.CertFile) {
		c.Server.TLS.CertFile = filepath.Join(c.Dir, c.Server.TLS.CertFile)
	}
	if c.Server.TLS.KeyFile != "" && !filepath.IsAbs(c.Server.TLS.KeyFile) {
		c.Server.TLS.KeyFile = filepath.Join(c.Dir, c.Server.TLS.KeyFile)
	}
	if c.Logging.Output != "" && c.Logging.Output != "stdout" && c.Logging.Output != "stderr" && !filepath.IsAbs(c.Logging.Output) {
		c.Logging.Output = filepath.Join(c.Dir, c.Logging.Output)
	}
}

func (c *Config) loadScenarioFiles() error {
	if len(c.ScenarioFiles) == 0 {
		return nil
	}
	var loaded []scenario.Spec
	for _, pattern := range c.ScenarioFiles {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(c.Dir, pattern)
		}
		paths, err := expandGlob(pattern)
		if err != nil {
			return err
		}
		if len(paths) == 0 {
			return fmt.Errorf("scenario_files pattern %q matched no files", pattern)
		}
		for _, path := range paths {
			specs, err := loadScenarioFile(path)
			if err != nil {
				return err
			}
			loaded = append(loaded, specs...)
		}
	}
	c.Scenarios = mergeScenarios(c.Scenarios, loaded)
	return nil
}

func hasTopLevelKey(raw []byte, ext, key string) bool {
	ext = strings.ToLower(ext)
	if ext == ".yaml" || ext == ".yml" {
		var object map[string]any
		if err := yaml.Unmarshal(raw, &object); err != nil {
			return false
		}
		_, ok := object[key]
		return ok
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return false
	}
	_, ok := object[key]
	return ok
}

// mergeScenarios appends extra scenarios and lets them override base scenarios
// with the same name or trigger.
func mergeScenarios(base, extra []scenario.Spec) []scenario.Spec {
	if len(extra) == 0 {
		return base
	}
	reserved := make(map[string]bool)
	for _, spec := range extra {
		reserved[strings.ToLower(spec.Name)] = true
		for _, trigger := range spec.Match.Triggers {
			reserved[strings.ToLower(strings.TrimSpace(trigger))] = true
		}
	}
	merged := make([]scenario.Spec, 0, len(base)+len(extra))
	for _, spec := range base {
		if reserved[strings.ToLower(spec.Name)] {
			continue
		}
		collides := false
		for _, trigger := range spec.Match.Triggers {
			if reserved[strings.ToLower(strings.TrimSpace(trigger))] {
				collides = true
				break
			}
		}
		if collides {
			continue
		}
		merged = append(merged, spec)
	}
	merged = append(merged, extra...)
	return merged
}

func expandGlob(pattern string) ([]string, error) {
	info, err := os.Stat(pattern)
	if err == nil && info.IsDir() {
		entries, err := os.ReadDir(pattern)
		if err != nil {
			return nil, err
		}
		var paths []string
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(entry.Name()))
			if ext == ".json" || ext == ".yaml" || ext == ".yml" {
				paths = append(paths, filepath.Join(pattern, entry.Name()))
			}
		}
		sort.Strings(paths)
		return paths, nil
	}
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid scenario_files pattern %q: %w", pattern, err)
	}
	sort.Strings(paths)
	return paths, nil
}

func loadScenarioFile(path string) ([]scenario.Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scenario file %s: %w", path, err)
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".yaml" || ext == ".yml" {
		var node yaml.Node
		if err := yaml.Unmarshal(raw, &node); err != nil {
			return nil, fmt.Errorf("parse scenario file %s: %w", path, err)
		}
		if len(node.Content) == 0 {
			return nil, nil
		}
		root := node.Content[0]
		switch root.Kind {
		case yaml.SequenceNode:
			var specs []scenario.Spec
			if err := root.Decode(&specs); err != nil {
				return nil, fmt.Errorf("parse scenario file %s: %w", path, err)
			}
			return specs, nil
		case yaml.MappingNode:
			var wrapper struct {
				Scenarios []scenario.Spec `json:"scenarios" yaml:"scenarios"`
			}
			dec := yaml.NewDecoder(bytes.NewReader(raw))
			dec.KnownFields(true)
			if err := dec.Decode(&wrapper); err != nil {
				return nil, fmt.Errorf("parse scenario file %s: %w", path, err)
			}
			return wrapper.Scenarios, nil
		default:
			return nil, fmt.Errorf("scenario file %s must contain an array or an object with a scenarios field", path)
		}
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var specs []scenario.Spec
		if err := json.Unmarshal(trimmed, &specs); err != nil {
			return nil, fmt.Errorf("parse scenario file %s: %w", path, err)
		}
		return specs, nil
	}
	var wrapper struct {
		Scenarios []scenario.Spec `json:"scenarios"`
	}
	if err := json.Unmarshal(trimmed, &wrapper); err != nil {
		return nil, fmt.Errorf("parse scenario file %s: %w", path, err)
	}
	return wrapper.Scenarios, nil
}

func (c *Config) resolveFileReferences() error {
	if c.Dir == "" {
		return nil
	}
	for name, value := range c.Templates {
		resolved, err := c.resolveFileValue(value)
		if err != nil {
			return fmt.Errorf("template %q: %w", name, err)
		}
		c.Templates[name] = resolved
	}
	for i := range c.Scenarios {
		respond := &c.Scenarios[i].Respond
		fields := []*string{&respond.Text, &respond.Reasoning, &respond.RawJSON}
		for _, field := range fields {
			resolved, err := c.resolveFileValue(*field)
			if err != nil {
				return fmt.Errorf("scenario %q: %w", c.Scenarios[i].Name, err)
			}
			*field = resolved
		}
		for j := range respond.ToolCalls {
			resolved, err := c.resolveFileValue(respond.ToolCalls[j].ArgumentsJSON)
			if err != nil {
				return fmt.Errorf("scenario %q tool call %q: %w", c.Scenarios[i].Name, respond.ToolCalls[j].Name, err)
			}
			respond.ToolCalls[j].ArgumentsJSON = resolved
		}
	}
	resolved, err := c.resolveFileValue(c.Defaults.Tooling.ArgumentTemplate)
	if err != nil {
		return fmt.Errorf("defaults.tooling.argument_template: %w", err)
	}
	c.Defaults.Tooling.ArgumentTemplate = resolved
	return nil
}

func (c *Config) resolveFileValue(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "@file:") {
		return value, nil
	}
	path := strings.TrimSpace(strings.TrimPrefix(trimmed, "@file:"))
	if path == "" {
		return "", errors.New("@file: reference is missing a path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.Dir, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(raw), nil
}

// Redacted returns a copy safe to print or expose through debug endpoints.
func (c *Config) Redacted() *Config {
	if c == nil {
		return nil
	}
	copyCfg := *c
	copyCfg.Auth.Keys = make([]APIKey, len(c.Auth.Keys))
	for i, key := range c.Auth.Keys {
		copyCfg.Auth.Keys[i] = key
		if key.Key != "" {
			copyCfg.Auth.Keys[i].Key = redact(key.Key)
		}
	}
	copyCfg.Encryption.Key = redact(c.Encryption.Key)
	return &copyCfg
}

func redact(value string) string {
	if len(value) <= 8 {
		return "***"
	}
	return value[:4] + "***" + value[len(value)-2:]
}
