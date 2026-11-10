package engine_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/engine"
	"github.com/jahrulnr/fakeagents/internal/scenario"
)

func toolResultMessages(plan *engine.Plan, result string) []engine.Message {
	messages := []engine.Message{{
		Role:      engine.RoleAssistant,
		ToolCalls: plan.ToolCalls,
	}}
	for _, call := range plan.ToolCalls {
		messages = append(messages, engine.Message{
			Role:       engine.RoleTool,
			Content:    result,
			ToolCallID: call.ID,
			ToolName:   call.Name,
			ToolResult: true,
		})
	}
	return messages
}

func TestScriptedToolTurns(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Scenarios = append(cfg.Scenarios, scenario.Spec{
			Name:  "scripted",
			Match: scenario.Match{Triggers: []string{"scripted"}},
			Respond: scenario.Respond{
				Thinking: boolPointer(true),
				Steps: []scenario.Respond{
					{ToolCalls: []scenario.ToolCallSpec{{ToolIndex: intPointer(0)}}},
					{ToolCalls: []scenario.ToolCallSpec{{
						Name:          "fakeagents_missing_tool",
						Arguments:     map[string]any{},
						ArgumentsJSON: `{"broken": `,
					}}},
					{Text: "final for {{ .LastToolResult }}"},
				},
			},
		})
	})

	tools := []engine.Tool{{
		Name:   "declared_tool",
		Schema: map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
	}}
	header := http.Header{}
	header.Set("X-FakeAgents-Scenario", "scripted")

	first := baseRequest()
	first.Headers = header
	first.Tools = tools
	plan, err := h.engine.Generate(context.Background(), first)
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if plan.Meta.Step != 0 || len(plan.ToolCalls) != 1 || plan.ToolCalls[0].Name != "declared_tool" {
		t.Fatalf("first plan = %#v", plan)
	}
	if !strings.Contains(plan.ToolCalls[0].Arguments, "city") {
		t.Fatalf("schema arguments not synthesized: %s", plan.ToolCalls[0].Arguments)
	}

	second := baseRequest()
	second.Headers = header
	second.Tools = tools
	second.Messages = append(second.Messages, toolResultMessages(plan, "declared result")...)
	plan, err = h.engine.Generate(context.Background(), second)
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if plan.Meta.Step != 1 || len(plan.ToolCalls) != 1 {
		t.Fatalf("second plan = %#v", plan)
	}
	if plan.ToolCalls[0].Name != "fakeagents_missing_tool" {
		t.Fatalf("second tool = %q", plan.ToolCalls[0].Name)
	}
	if plan.ToolCalls[0].Arguments != `{"broken":` {
		t.Fatalf("malformed arguments were altered: %q", plan.ToolCalls[0].Arguments)
	}

	third := baseRequest()
	third.Headers = header
	third.Tools = tools
	third.Messages = second.Messages
	third.Messages = append(third.Messages, toolResultMessages(plan, "missing tool error")...)
	plan, err = h.engine.Generate(context.Background(), third)
	if err != nil {
		t.Fatalf("third turn: %v", err)
	}
	if plan.Meta.Step != 2 || len(plan.ToolCalls) != 0 {
		t.Fatalf("third plan = %#v", plan)
	}
	if !strings.Contains(plan.Text, "missing tool error") {
		t.Fatalf("final text = %q", plan.Text)
	}
}

func TestAgenticScenarioUsesHarnessTools(t *testing.T) {
	h := newHarness(t, nil)
	tools := []engine.Tool{
		{Name: "alpha", Schema: map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}},
		{Name: "beta", Schema: map[string]any{"type": "object"}},
	}
	header := http.Header{}
	header.Set("X-FakeAgents-Scenario", "agentic")

	first := baseRequest()
	first.Headers = header
	first.Tools = tools
	plan, err := h.engine.Generate(context.Background(), first)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if len(plan.ToolCalls) != 1 || plan.ToolCalls[0].Name != "alpha" || plan.Meta.Step != 0 {
		t.Fatalf("first plan = %#v", plan)
	}

	second := baseRequest()
	second.Headers = header
	second.Tools = tools
	second.Messages = append(second.Messages, toolResultMessages(plan, "alpha result")...)
	plan, err = h.engine.Generate(context.Background(), second)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(plan.ToolCalls) != 1 || plan.ToolCalls[0].Name != "beta" || plan.Meta.Step != 1 {
		t.Fatalf("second plan = %#v", plan)
	}

	third := baseRequest()
	third.Headers = header
	third.Tools = tools
	third.Messages = second.Messages
	third.Messages = append(third.Messages, toolResultMessages(plan, "beta result")...)
	plan, err = h.engine.Generate(context.Background(), third)
	if err != nil {
		t.Fatalf("third: %v", err)
	}
	if len(plan.ToolCalls) != 0 || plan.Meta.Step != 2 {
		t.Fatalf("third plan = %#v", plan)
	}
	if !strings.Contains(plan.Text, "beta result") {
		t.Fatalf("final text = %q", plan.Text)
	}
}

func TestAgentPlanCanCallUndeclaredTool(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Defaults.Tooling.Mode = "agent"
		cfg.Defaults.Tooling.Plan = []string{"alpha", "ghost"}
		cfg.Defaults.Tooling.UnknownTool = "allow"
	})
	tools := []engine.Tool{{Name: "alpha", Schema: map[string]any{"type": "object"}}}
	first := baseRequest()
	first.Tools = tools
	plan, err := h.engine.Generate(context.Background(), first)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if len(plan.ToolCalls) != 1 || plan.ToolCalls[0].Name != "alpha" {
		t.Fatalf("first plan = %#v", plan)
	}
	second := baseRequest()
	second.Tools = tools
	second.Messages = append(second.Messages, toolResultMessages(plan, "alpha result")...)
	plan, err = h.engine.Generate(context.Background(), second)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(plan.ToolCalls) != 1 || plan.ToolCalls[0].Name != "ghost" {
		t.Fatalf("second plan = %#v", plan)
	}
	if plan.ToolCalls[0].Arguments != "{}" {
		t.Fatalf("undeclared tool arguments = %q", plan.ToolCalls[0].Arguments)
	}
}

func TestStepHeaderOverride(t *testing.T) {
	h := newHarness(t, nil)
	header := http.Header{}
	header.Set("X-FakeAgents-Scenario", "tool-error")
	header.Set("X-FakeAgents-Step", "2")
	req := baseRequest()
	req.Headers = header
	req.Tools = []engine.Tool{{Name: "declared", Schema: map[string]any{"type": "object"}}}
	plan, err := h.engine.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if plan.Meta.Step != 2 {
		t.Fatalf("step = %d", plan.Meta.Step)
	}
	if len(plan.ToolCalls) != 0 {
		t.Fatalf("tool calls = %#v", plan.ToolCalls)
	}
}

func TestBuiltinToolErrorScenario(t *testing.T) {
	h := newHarness(t, nil)
	header := http.Header{}
	req := baseRequest()
	req.Headers = header
	req.Messages[0].Content = "please test tool error"
	req.Tools = []engine.Tool{{Name: "declared", Schema: map[string]any{"type": "object"}}}
	plan, err := h.engine.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if plan.Meta.Scenario != "tool-error" || len(plan.ToolCalls) != 1 || plan.ToolCalls[0].Name != "declared" {
		t.Fatalf("first plan = %#v", plan)
	}
}

func intPointer(value int) *int { return &value }

func TestScriptResetsAfterNewUserPrompt(t *testing.T) {
	h := newHarness(t, nil)
	header := http.Header{}
	header.Set("X-FakeAgents-Scenario", "tool-error")
	req := baseRequest()
	req.Headers = header
	req.Tools = []engine.Tool{{Name: "declared", Schema: map[string]any{"type": "object"}}}
	req.Messages = []engine.Message{
		{Role: engine.RoleUser, Content: "please test tool error"},
		{Role: engine.RoleAssistant, ToolCalls: []engine.ToolCall{{ID: "call_1", Name: "declared", Arguments: "{}"}}},
		{Role: engine.RoleTool, Content: "one", ToolCallID: "call_1", ToolName: "declared", ToolResult: true},
		{Role: engine.RoleAssistant, ToolCalls: []engine.ToolCall{{ID: "call_2", Name: "ghost", Arguments: "{}"}}},
		{Role: engine.RoleTool, Content: "two", ToolCallID: "call_2", ToolName: "ghost", ToolResult: true},
		{Role: engine.RoleAssistant, Content: "done"},
		{Role: engine.RoleUser, Content: "please test tool error again"},
	}
	plan, err := h.engine.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if plan.Meta.Step != 0 {
		t.Fatalf("step = %d", plan.Meta.Step)
	}
	if len(plan.ToolCalls) != 1 || plan.ToolCalls[0].Name != "declared" {
		t.Fatalf("tool calls = %#v", plan.ToolCalls)
	}
}

func TestStepsSequenceMode(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Scenarios = append(cfg.Scenarios, scenario.Spec{
			Name:  "queue-script",
			Match: scenario.Match{Triggers: []string{"queue-script"}},
			Respond: scenario.Respond{
				StepsMode: "sequence",
				Steps: []scenario.Respond{
					{Text: "first turn"},
					{Text: "steering: {{ .UserMessage }}"},
				},
			},
		})
	})
	header := http.Header{}
	header.Set("X-FakeAgents-Scenario", "queue-script")

	first := baseRequest()
	first.Headers = header
	first.SessionKey = "queue-room"
	plan, err := h.engine.Generate(context.Background(), first)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if plan.Meta.Step != 0 || plan.Text != "first turn" {
		t.Fatalf("first plan = %#v", plan)
	}

	second := baseRequest()
	second.Headers = header
	second.SessionKey = "queue-room"
	second.Messages[0].Content = "steer left"
	plan, err = h.engine.Generate(context.Background(), second)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if plan.Meta.Step != 1 || !strings.Contains(plan.Text, "steer left") {
		t.Fatalf("second plan = %#v", plan)
	}

	h.engine.ResetScripts()
	third := baseRequest()
	third.Headers = header
	third.SessionKey = "queue-room"
	plan, err = h.engine.Generate(context.Background(), third)
	if err != nil {
		t.Fatalf("third: %v", err)
	}
	if plan.Meta.Step != 0 || plan.Text != "first turn" {
		t.Fatalf("after reset plan = %#v", plan)
	}
}
