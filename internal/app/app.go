// Package app wires all FakeAgents dependencies together. Construction is
// explicit (no globals) so tests and embedders can replace any component.
package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/jahrulnr/fakeagents/internal/clock"
	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/engine"
	"github.com/jahrulnr/fakeagents/internal/envelope"
	"github.com/jahrulnr/fakeagents/internal/httpapi"
	"github.com/jahrulnr/fakeagents/internal/id"
	"github.com/jahrulnr/fakeagents/internal/logging"
	"github.com/jahrulnr/fakeagents/internal/observe"
	"github.com/jahrulnr/fakeagents/internal/provider/acp"
	"github.com/jahrulnr/fakeagents/internal/provider/anthropic"
	"github.com/jahrulnr/fakeagents/internal/provider/openai"
	"github.com/jahrulnr/fakeagents/internal/ratelimit"
	"github.com/jahrulnr/fakeagents/internal/scenario"
	"github.com/jahrulnr/fakeagents/internal/session"
	"github.com/jahrulnr/fakeagents/internal/store"
	"github.com/jahrulnr/fakeagents/internal/template"
)

// App is a fully constructed FakeAgents instance.
type App struct {
	Config  *config.Config
	Logger  *slog.Logger
	Engine  *engine.Engine
	Server  *httpapi.Server
	ACP     *acp.Agent
	Sealer  envelope.Sealer
	Store   *store.Memory
	logFile io.Closer
}

// New builds an application from a validated configuration.
func New(cfg *config.Config, build httpapi.BuildInfo) (*App, error) {
	if cfg == nil {
		cfg = config.Default()
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	renderer, err := template.New(cfg.Templates, template.Builtins())
	if err != nil {
		return nil, err
	}
	if err := validateTemplates(cfg, renderer); err != nil {
		return nil, err
	}

	scenarios, err := scenario.Compile(cfg.Scenarios)
	if err != nil {
		return nil, err
	}
	if cfg.Defaults.Scenario != "" {
		if _, ok := scenarios.Lookup(cfg.Defaults.Scenario); !ok {
			return nil, fmt.Errorf("defaults.scenario %q does not match any scenario", cfg.Defaults.Scenario)
		}
	}

	sealer, err := envelope.New(cfg.Encryption.Key)
	if err != nil {
		return nil, fmt.Errorf("initialise encryption: %w", err)
	}

	logger, logCloser, err := logging.New(cfg.Logging)
	if err != nil {
		return nil, err
	}

	clk := clock.Real{}
	ids := id.New()
	sessions := session.New(clk)
	responses := store.NewMemory(clk, cfg.Responses.TTL.D(), cfg.Responses.MaxEntries)
	limiter := ratelimit.New(ratelimit.Config{
		Enabled:           cfg.RateLimit.Enabled,
		RequestsPerMinute: cfg.RateLimit.RequestsPerMinute,
		TokensPerMinute:   cfg.RateLimit.TokensPerMinute,
		RetryAfter:        cfg.RateLimit.RetryAfter.D(),
	}, clk)
	recorder := observe.NewRecorder(cfg.Debug.MaxRecordedRequests)

	sim := engine.New(engine.Deps{
		Config:    cfg,
		Clock:     clk,
		IDs:       ids,
		Templates: renderer,
		Scenarios: scenarios,
		Sessions:  sessions,
		Sealer:    sealer,
		Logger:    logger,
	})

	openaiHandler := openai.New(openai.Deps{
		Config:    cfg,
		Engine:    sim,
		Sealer:    sealer,
		Sessions:  sessions,
		Limiter:   limiter,
		Responses: responses,
		Clock:     clk,
		IDs:       ids,
		Logger:    logger,
	})
	anthropicHandler := anthropic.New(anthropic.Deps{
		Config:   cfg,
		Engine:   sim,
		Sealer:   sealer,
		Sessions: sessions,
		Limiter:  limiter,
		Clock:    clk,
		IDs:      ids,
		Logger:   logger,
	})
	server := httpapi.New(httpapi.Deps{
		Config:    cfg,
		Engine:    sim,
		OpenAI:    openaiHandler,
		Anthropic: anthropicHandler,
		Sessions:  sessions,
		Limiter:   limiter,
		Responses: responses,
		Recorder:  recorder,
		Scenarios: scenarios,
		Logger:    logger,
		Clock:     clk,
		IDs:       ids,
		Version:   build,
	})

	acpAgent := acp.New(acp.Deps{
		Config:  cfg,
		Engine:  sim,
		Clock:   clk,
		IDs:     ids,
		Logger:  logger,
		Version: build.Version,
	})

	return &App{
		Config:  cfg,
		Logger:  logger,
		Engine:  sim,
		Server:  server,
		ACP:     acpAgent,
		Sealer:  sealer,
		Store:   responses,
		logFile: logCloser,
	}, nil
}

// Run starts the HTTP server and blocks until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	return a.Server.Run(ctx)
}

// RunACP serves the Agent Client Protocol agent on in/out until the client
// disconnects or ctx ends.
func (a *App) RunACP(ctx context.Context, in io.Reader, out io.Writer) error {
	return a.ACP.Run(ctx, in, out)
}

// Close releases resources held by the application.
func (a *App) Close() error {
	if a.Store != nil {
		a.Store.Close()
	}
	if a.logFile != nil {
		return a.logFile.Close()
	}
	return nil
}

func validateTemplates(cfg *config.Config, renderer template.Renderer) error {
	checkName := func(name, field string) error {
		if name == "" {
			return nil
		}
		if !renderer.Has(name) {
			return fmt.Errorf("%s references unknown template %q", field, name)
		}
		return nil
	}
	checkSource := func(source, field string) error {
		if source == "" {
			return nil
		}
		if err := renderer.Validate(source); err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		return nil
	}
	if err := checkName(cfg.Defaults.Thinking.Template, "defaults.thinking.template"); err != nil {
		return err
	}
	if err := checkName(cfg.Defaults.Tooling.FinalTemplate, "defaults.tooling.final_template"); err != nil {
		return err
	}
	if err := checkSource(cfg.Defaults.Tooling.ArgumentTemplate, "defaults.tooling.argument_template"); err != nil {
		return err
	}
	var checkRespond func(prefix string, respond scenario.Respond) error
	checkRespond = func(prefix string, respond scenario.Respond) error {
		if err := checkName(respond.TextTemplate, prefix+".text_template"); err != nil {
			return err
		}
		if err := checkName(respond.ReasoningTemplate, prefix+".reasoning_template"); err != nil {
			return err
		}
		if err := checkSource(respond.Text, prefix+".text"); err != nil {
			return err
		}
		if err := checkSource(respond.Reasoning, prefix+".reasoning"); err != nil {
			return err
		}
		if err := checkSource(respond.RawJSON, prefix+".raw_json"); err != nil {
			return err
		}
		for i, variant := range respond.Variants {
			if err := checkSource(variant, fmt.Sprintf("%s.variants[%d]", prefix, i)); err != nil {
				return err
			}
		}
		for i, call := range respond.ToolCalls {
			if err := checkSource(call.ArgumentsJSON, fmt.Sprintf("%s.tool_calls[%d].arguments_json", prefix, i)); err != nil {
				return err
			}
		}
		for i, step := range respond.Steps {
			if err := checkRespond(fmt.Sprintf("%s.steps[%d]", prefix, i), step); err != nil {
				return err
			}
		}
		return nil
	}

	for i, spec := range cfg.Scenarios {
		prefix := fmt.Sprintf("scenarios[%d] (%s).respond", i, spec.Name)
		if err := checkRespond(prefix, spec.Respond); err != nil {
			return err
		}
	}
	return nil
}
