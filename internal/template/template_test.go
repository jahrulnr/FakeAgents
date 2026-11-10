package template_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jahrulnr/fakeagents/internal/template"
)

func newRenderer(t *testing.T) *template.Engine {
	t.Helper()
	engine, err := template.New(map[string]string{
		"custom": `{{ upper .UserMessage }} {{ .Model }}`,
	}, template.Builtins())
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}
	return engine
}

func TestRenderDefault(t *testing.T) {
	engine := newRenderer(t)
	data := &template.Data{UserMessage: "hello", Model: "fake-gpt-4o", Turn: 2, Now: time.Now()}
	out, err := engine.Render("default", data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "fake-gpt-4o") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestCustomTemplate(t *testing.T) {
	engine := newRenderer(t)
	out, err := engine.Render("custom", &template.Data{UserMessage: "hello", Model: "m"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out != "HELLO m" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestFunctions(t *testing.T) {
	engine := newRenderer(t)
	cases := map[string]string{
		`{{ json .UserMessage }}`:       `"hello"`,
		`{{ truncate 3 .UserMessage }}`: `hel…`,
		`{{ repeat 2 "ab" }}`:           `abab`,
		`{{ join "," (list "a" "b") }}`: `a,b`,
		`{{ add 2 3 }}`:                 `5`,
	}
	for source, expected := range cases {
		out, err := engine.RenderSource(source, &template.Data{UserMessage: "hello"})
		if err != nil {
			t.Fatalf("render %q: %v", source, err)
		}
		if out != expected {
			t.Fatalf("%q = %q, want %q", source, out, expected)
		}
	}
}

func TestMissingKeyFails(t *testing.T) {
	engine := newRenderer(t)
	_, err := engine.RenderSource(`{{ .DoesNotExist }}`, &template.Data{UserMessage: "hi"})
	if err == nil {
		t.Fatal("expected missing key error")
	}
}

func TestInvalidTemplateRejected(t *testing.T) {
	_, err := template.New(map[string]string{"bad": `{{ if }}`}, template.Builtins())
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestReasoningTemplateUsesEffort(t *testing.T) {
	engine := newRenderer(t)
	low, err := engine.Render("reasoning", &template.Data{UserMessage: "x", Effort: "low"})
	if err != nil {
		t.Fatalf("render low: %v", err)
	}
	high, err := engine.Render("reasoning", &template.Data{UserMessage: "x", Effort: "high"})
	if err != nil {
		t.Fatalf("render high: %v", err)
	}
	if low == high {
		t.Fatal("expected effort-specific reasoning text")
	}
}
