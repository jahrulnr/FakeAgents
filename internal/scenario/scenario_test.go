package scenario_test

import (
	"testing"

	"github.com/jahrulnr/fakeagents/internal/scenario"
)

func TestNamedScenariosDoNotMatchAutomatically(t *testing.T) {
	set, err := scenario.Compile([]scenario.Spec{{
		Name:  "billing",
		Match: scenario.Match{Triggers: []string{"billing"}},
		Respond: scenario.Respond{
			Type:  scenario.TypeError,
			Error: &scenario.ErrorSpec{Preset: "billing"},
		},
	}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, ok := set.Match(&scenario.Context{Provider: "openai"}); ok {
		t.Fatal("named-only scenario must not match automatically")
	}
	compiled, ok := set.Lookup("BILLING")
	if !ok || compiled.Name() != "billing" {
		t.Fatal("trigger lookup is case insensitive")
	}
}

func TestMatchByModelHeaderAndJSON(t *testing.T) {
	set, err := scenario.Compile([]scenario.Spec{
		{
			Name: "specific",
			Match: scenario.Match{
				Providers:  []string{"openai"},
				Models:     []string{"gpt-5*"},
				Headers:    map[string]string{"X-Test": "yes"},
				JSONEquals: map[string]any{"reasoning.effort": "high"},
				Stream:     new(true),
			},
			Respond: scenario.Respond{Type: scenario.TypeText},
		},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	ctx := &scenario.Context{
		Provider: "openai",
		Model:    "gpt-5.1",
		Stream:   true,
		Headers:  mapHeader("X-Test", "yes"),
		Raw:      map[string]any{"reasoning": map[string]any{"effort": "high"}},
	}
	compiled, ok := set.Match(ctx)
	if !ok || compiled.Name() != "specific" {
		t.Fatal("expected specific scenario to match")
	}
	ctx.Headers = mapHeader("X-Test", "no")
	if _, ok := set.Match(ctx); ok {
		t.Fatal("header mismatch must not match")
	}
}

func TestPriorityOrder(t *testing.T) {
	set, err := scenario.Compile([]scenario.Spec{
		{Name: "low", Match: scenario.Match{Providers: []string{"openai"}}, Priority: 1},
		{Name: "high", Match: scenario.Match{Providers: []string{"openai"}}, Priority: 10},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	compiled, ok := set.Match(&scenario.Context{Provider: "openai"})
	if !ok || compiled.Name() != "high" {
		t.Fatalf("expected high priority, got %v", compiled)
	}
}

func TestDuplicateNamesRejected(t *testing.T) {
	_, err := scenario.Compile([]scenario.Spec{
		{Name: "a", Respond: scenario.Respond{Type: scenario.TypeText}},
		{Name: "a", Respond: scenario.Respond{Type: scenario.TypeText}},
	})
	if err == nil {
		t.Fatal("expected duplicate name error")
	}
}

func TestUnknownPresetRejected(t *testing.T) {
	_, err := scenario.Compile([]scenario.Spec{{
		Name:    "bad",
		Respond: scenario.Respond{Type: scenario.TypeError, Error: &scenario.ErrorSpec{Preset: "nope"}},
	}})
	if err == nil {
		t.Fatal("expected unknown preset error")
	}
}

func TestProbabilityIsDeterministicWithRNG(t *testing.T) {
	set, err := scenario.Compile([]scenario.Spec{{
		Name:    "half",
		Match:   scenario.Match{Probability: 0.5},
		Respond: scenario.Respond{Type: scenario.TypeText},
	}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	ctx := &scenario.Context{Provider: "openai", Random: seeded(42)}
	matches := 0
	for range 200 {
		if _, ok := set.Match(ctx); ok {
			matches++
		}
	}
	if matches < 60 || matches > 140 {
		t.Fatalf("probability distribution looks wrong: %d/200", matches)
	}
}

//go:fix inline
func boolPtr(value bool) *bool { return new(value) }

func TestScriptedStepsValidation(t *testing.T) {
	index := 0
	valid := scenario.Spec{
		Name: "valid-steps",
		Respond: scenario.Respond{
			Steps: []scenario.Respond{
				{ToolCalls: []scenario.ToolCallSpec{{ToolIndex: &index}}},
				{Text: "final"},
			},
		},
	}
	if _, err := scenario.Compile([]scenario.Spec{valid}); err != nil {
		t.Fatalf("valid steps rejected: %v", err)
	}

	bad := scenario.Spec{
		Name:    "bad-step",
		Respond: scenario.Respond{Steps: []scenario.Respond{{ToolCalls: []scenario.ToolCallSpec{{}}}}},
	}
	if _, err := scenario.Compile([]scenario.Spec{bad}); err == nil {
		t.Fatal("empty tool call in step should be rejected")
	}

	nested := scenario.Spec{
		Name: "nested",
		Respond: scenario.Respond{
			Steps: []scenario.Respond{{Steps: []scenario.Respond{{Text: "nested"}}}},
		},
	}
	if _, err := scenario.Compile([]scenario.Spec{nested}); err == nil {
		t.Fatal("nested steps should be rejected")
	}
}

func TestEffectiveType(t *testing.T) {
	if got := (scenario.Respond{ToolCalls: []scenario.ToolCallSpec{{Name: "x"}}}).EffectiveType(); got != scenario.TypeToolCall {
		t.Fatalf("tool call type = %q", got)
	}
	if got := (scenario.Respond{RawJSON: "{}"}).EffectiveType(); got != scenario.TypeRaw {
		t.Fatalf("raw type = %q", got)
	}
	if got := (scenario.Respond{}).EffectiveType(); got != scenario.TypeText {
		t.Fatalf("default type = %q", got)
	}
}
