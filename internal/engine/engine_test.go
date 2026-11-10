package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/clock"
	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/engine"
	"github.com/jahrulnr/fakeagents/internal/envelope"
	"github.com/jahrulnr/fakeagents/internal/id"
	"github.com/jahrulnr/fakeagents/internal/scenario"
	"github.com/jahrulnr/fakeagents/internal/session"
	"github.com/jahrulnr/fakeagents/internal/template"
	"github.com/jahrulnr/fakeagents/internal/token"
)

type harness struct {
	engine *engine.Engine
	sealer envelope.Sealer
	cfg    *config.Config
}

func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()
	cfg := config.Default()
	disabled := false
	cfg.Defaults.Latency.Enabled = &disabled
	cfg.Encryption.Key = "unit-test-key"
	cfg.Logging.Level = "error"
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	renderer, err := template.New(cfg.Templates, template.Builtins())
	if err != nil {
		t.Fatalf("templates: %v", err)
	}
	scenarios, err := scenario.Compile(cfg.Scenarios)
	if err != nil {
		t.Fatalf("scenarios: %v", err)
	}
	sealer, err := envelope.New(cfg.Encryption.Key)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	sim := engine.New(engine.Deps{
		Config:    cfg,
		Clock:     clk,
		IDs:       id.NewFake(),
		Tokens:    token.New(),
		Templates: renderer,
		Scenarios: scenarios,
		Sessions:  session.New(clk),
		Sealer:    sealer,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return &harness{engine: sim, sealer: sealer, cfg: cfg}
}

func baseRequest() *engine.Request {
	return &engine.Request{
		Provider:        "openai",
		Endpoint:        scenario.EndpointChat,
		Model:           "fake-gpt-4o",
		Messages:        []engine.Message{{Role: engine.RoleUser, Content: "hello"}},
		Turn:            0,
		TurnSet:         true,
		Headers:         http.Header{},
		MaxOutputTokens: 1024,
	}
}

func TestGenerateDefaultText(t *testing.T) {
	h := newHarness(t, nil)
	plan, err := h.engine.Generate(context.Background(), baseRequest())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(plan.Text, "hello") {
		t.Fatalf("text = %q", plan.Text)
	}
	if plan.Finish != engine.FinishStop || plan.Status != engine.StatusCompleted {
		t.Fatalf("unexpected finish/status: %s/%s", plan.Finish, plan.Status)
	}
	if plan.Usage.InputTokens <= 0 || plan.Usage.OutputTokens <= 0 {
		t.Fatalf("usage = %+v", plan.Usage)
	}
}

func TestThinkingMirrorsRequest(t *testing.T) {
	h := newHarness(t, nil)
	req := baseRequest()
	req.Reasoning = engine.ReasoningRequest{Requested: true, Effort: "high", Display: "summarized"}
	plan, err := h.engine.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(plan.Reasoning) != 1 || plan.Reasoning[0].Text == "" {
		t.Fatalf("expected reasoning block, got %#v", plan.Reasoning)
	}
	if plan.Usage.ReasoningTokens <= 0 {
		t.Fatalf("reasoning tokens = %d", plan.Usage.ReasoningTokens)
	}

	req = baseRequest()
	plan, err = h.engine.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(plan.Reasoning) != 0 {
		t.Fatalf("unexpected reasoning: %#v", plan.Reasoning)
	}
}

func TestThinkingOmittedDisplay(t *testing.T) {
	h := newHarness(t, nil)
	req := baseRequest()
	req.Provider = "anthropic"
	req.Endpoint = scenario.EndpointMessages
	req.Model = "fake-claude-sonnet"
	req.Reasoning = engine.ReasoningRequest{Requested: true, Effort: "medium", Display: "omitted"}
	plan, err := h.engine.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(plan.Reasoning) != 1 {
		t.Fatalf("expected reasoning block")
	}
	if plan.Reasoning[0].Text != "" {
		t.Fatalf("omitted display must hide text: %q", plan.Reasoning[0].Text)
	}
	if plan.Reasoning[0].Signature == "" {
		t.Fatal("omitted display must still carry a signature")
	}
	if plan.Usage.ReasoningTokens <= 0 {
		t.Fatal("hidden reasoning must still be billed")
	}
}

func TestEncryptedReasoningRoundTrip(t *testing.T) {
	h := newHarness(t, nil)
	req := baseRequest()
	req.Endpoint = scenario.EndpointResponses
	req.IncludeEncrypted = true
	req.Reasoning = engine.ReasoningRequest{Requested: true, Effort: "low", Summary: "auto"}
	plan, err := h.engine.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	block := plan.Reasoning[0]
	if block.Encrypted == "" || block.Signature == "" {
		t.Fatalf("missing encrypted content/signature: %#v", block)
	}
	payload, err := h.sealer.Open("openai.reasoning", block.Encrypted)
	if err != nil {
		t.Fatalf("open encrypted: %v", err)
	}
	if payload["text"] != block.Text {
		t.Fatalf("payload text mismatch: %v", payload["text"])
	}
}

func TestToolChoiceRequiredAndFinalTurn(t *testing.T) {
	h := newHarness(t, nil)
	req := baseRequest()
	req.Tools = []engine.Tool{{
		Name:        "get_weather",
		Description: "Get weather",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{"type": "string"},
			},
			"required": []any{"city"},
		},
	}}
	req.ToolChoice = &engine.ToolChoice{Mode: "required"}
	plan, err := h.engine.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(plan.ToolCalls) != 1 || plan.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("tool calls = %#v", plan.ToolCalls)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(plan.ToolCalls[0].Arguments), &args); err != nil {
		t.Fatalf("arguments are not JSON: %v", err)
	}
	if args["city"] != "example" {
		t.Fatalf("arguments = %v", args)
	}
	if plan.Finish != engine.FinishToolCalls {
		t.Fatalf("finish = %s", plan.Finish)
	}

	followUp := baseRequest()
	followUp.Messages = []engine.Message{
		{Role: engine.RoleUser, Content: "weather?"},
		{Role: engine.RoleAssistant, ToolCalls: []engine.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: "{}"}}},
		{Role: engine.RoleTool, Content: "sunny, 31C", ToolCallID: "call_1", ToolName: "get_weather", ToolResult: true},
	}
	followUp.Tools = req.Tools
	followUp.MaxOutputTokens = 1024
	plan, err = h.engine.Generate(context.Background(), followUp)
	if err != nil {
		t.Fatalf("generate follow-up: %v", err)
	}
	if len(plan.ToolCalls) != 0 {
		t.Fatalf("final turn must not call tools: %#v", plan.ToolCalls)
	}
	if !strings.Contains(plan.Text, "sunny, 31C") || !strings.Contains(plan.Text, "get_weather") {
		t.Fatalf("final text = %q", plan.Text)
	}
	if plan.Finish != engine.FinishStop {
		t.Fatalf("finish = %s", plan.Finish)
	}
}

func TestToolScenario(t *testing.T) {
	h := newHarness(t, nil)
	req := baseRequest()
	req.Messages[0].Content = "weather? [[fakeagents:tool-call]]"
	req.Tools = []engine.Tool{{Name: "lookup", Schema: map[string]any{"type": "object"}}}
	plan, err := h.engine.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(plan.ToolCalls) != 1 || plan.ToolCalls[0].Name != "lookup" {
		t.Fatalf("tool calls = %#v", plan.ToolCalls)
	}
	if strings.Contains(plan.Text, "fakeagents") {
		t.Fatalf("directive leaked into text: %q", plan.Text)
	}
}

func TestScenarioTriggersAndErrors(t *testing.T) {
	h := newHarness(t, nil)
	req := baseRequest()
	req.Messages[0].Content = "trigger [[fakeagents:billing]]"
	_, err := h.engine.Generate(context.Background(), req)
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected api error, got %v", err)
	}
	if apiErr.Status != http.StatusTooManyRequests || apiErr.Code != "insufficient_quota" {
		t.Fatalf("unexpected error: %+v", apiErr)
	}

	req = baseRequest()
	req.Headers.Set("X-FakeAgents-Scenario", "unknown")
	_, err = h.engine.Generate(context.Background(), req)
	if !errors.As(err, &apiErr) || apiErr.Code != "scenario_not_found" {
		t.Fatalf("expected scenario_not_found, got %v", err)
	}
}

func TestTruncationByMaxTokens(t *testing.T) {
	h := newHarness(t, nil)
	req := baseRequest()
	req.Headers.Set("X-FakeAgents-Scenario", "long")
	req.MaxOutputTokens = 12
	plan, err := h.engine.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if plan.Finish != engine.FinishLength || plan.Status != engine.StatusIncomplete {
		t.Fatalf("finish/status = %s/%s", plan.Finish, plan.Status)
	}
	if plan.Usage.OutputTokens > 12 {
		t.Fatalf("output tokens = %d, want <= 12", plan.Usage.OutputTokens)
	}
	if len([]rune(plan.Text)) >= 500 {
		t.Fatalf("text was not truncated: %d runes", len([]rune(plan.Text)))
	}
}

func TestUsageOverride(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Scenarios = append(cfg.Scenarios, scenario.Spec{
			Name:  "usage",
			Match: scenario.Match{Triggers: []string{"usage"}},
			Respond: scenario.Respond{
				Type: scenario.TypeText,
				Usage: &scenario.UsageOverride{
					Mode:            "fixed",
					InputTokens:     new(11),
					OutputTokens:    new(22),
					ReasoningTokens: new(3),
					CachedTokens:    new(4),
				},
			},
		})
	})
	req := baseRequest()
	req.Headers.Set("X-FakeAgents-Scenario", "usage")
	plan, err := h.engine.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if plan.Usage.InputTokens != 11 || plan.Usage.OutputTokens != 22 || plan.Usage.ReasoningTokens != 3 || plan.Usage.CachedTokens != 4 {
		t.Fatalf("usage = %+v", plan.Usage)
	}
}

func TestTurnIncrement(t *testing.T) {
	h := newHarness(t, nil)
	req := baseRequest()
	req.TurnSet = false
	req.Turn = 0
	req.SessionKey = "session-1"
	first, err := h.engine.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	second, err := h.engine.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if first.Meta.Turn != 0 || second.Meta.Turn != 1 {
		t.Fatalf("turns = %d, %d", first.Meta.Turn, second.Meta.Turn)
	}
}

func TestStrictModelValidation(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Defaults.StrictModels = true
		cfg.Models[0].Aliases = []string{"gpt-4o"}
	})
	resolved, apiErr := h.engine.ResolveModel("openai", "gpt-4o")
	if apiErr != nil {
		t.Fatalf("alias should resolve: %v", apiErr)
	}
	if resolved != "fake-gpt-4o" {
		t.Fatalf("resolved = %q", resolved)
	}
	_, apiErr = h.engine.ResolveModel("openai", "unknown-model")
	if apiErr == nil || apiErr.Status != http.StatusNotFound {
		t.Fatalf("expected 404, got %v", apiErr)
	}
}

func TestCountInputTokens(t *testing.T) {
	h := newHarness(t, nil)
	req := baseRequest()
	req.System = "You are helpful."
	req.Tools = []engine.Tool{{Name: "t", Schema: map[string]any{"type": "object"}}}
	if tokens := h.engine.CountInputTokens(req); tokens < 10 {
		t.Fatalf("input tokens = %d", tokens)
	}
}

//go:fix inline
func intPtr(value int) *int { return new(value) }

func TestToolCallsCanSuppressReasoning(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Defaults.Tooling.IncludeReasoning = new(false)
	})
	req := baseRequest()
	req.Reasoning = engine.ReasoningRequest{Requested: true, Effort: "high"}
	req.Tools = []engine.Tool{{Name: "lookup", Schema: map[string]any{"type": "object"}}}
	req.ToolChoice = &engine.ToolChoice{Mode: "required"}
	plan, err := h.engine.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(plan.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", plan.ToolCalls)
	}
	if len(plan.Reasoning) != 0 {
		t.Fatalf("reasoning should be suppressed: %#v", plan.Reasoning)
	}
	if plan.Usage.ReasoningTokens != 0 {
		t.Fatalf("reasoning tokens = %d", plan.Usage.ReasoningTokens)
	}
}

//go:fix inline
func boolPointer(value bool) *bool { return new(value) }
