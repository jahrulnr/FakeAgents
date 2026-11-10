// Package template renders scenario and chat content. Templates are standard
// Go text/templates with a small, side-effect free function library.
//
// Templates are parsed per render so helper functions can share a request
// scoped random source. They are validated at startup, so a syntax error is a
// configuration error rather than a runtime surprise.
package template

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	mrand "math/rand/v2"
	"sort"
	"strings"
	"text/template"
	"time"
	"unicode"
	"unicode/utf8"
)

// Data is the template context exposed to every template.
type Data struct {
	Provider  string
	Endpoint  string
	Model     string
	Stream    bool
	Session   string
	Scenario  string
	Turn      int
	Step      int
	RequestID string
	Effort    string
	Seed      int64
	Now       time.Time
	Random    *mrand.Rand

	UserMessage     string
	System          string
	LastToolResult  string
	LastToolName    string
	ToolName        string
	ToolDescription string
	Messages        []Message
	Tools           []Tool
}

// Message is a rendered conversation turn.
type Message struct {
	Role      string
	Content   string
	ToolCalls []ToolCall
}

// Tool is a rendered tool definition.
type Tool struct {
	Name        string
	Description string
}

// ToolCall is a rendered tool invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// Renderer renders named or inline templates.
type Renderer interface {
	Render(name string, data *Data) (string, error)
	RenderSource(source string, data *Data) (string, error)
	Has(name string) bool
	Names() []string
	Validate(source string) error
}

// Engine is the default Renderer.
type Engine struct {
	sources  map[string]string
	builtins map[string]string
}

// New builds a renderer. User templates override built-ins with the same name.
func New(user map[string]string, builtins map[string]string) (*Engine, error) {
	sources := make(map[string]string, len(builtins)+len(user))
	maps.Copy(sources, builtins)
	for name, source := range user {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("template name must not be empty")
		}
		sources[name] = source
	}
	engine := &Engine{sources: sources, builtins: builtins}
	for name, source := range sources {
		if err := engine.Validate(source); err != nil {
			return nil, fmt.Errorf("template %q: %w", name, err)
		}
	}
	return engine, nil
}

// Has implements Renderer.
func (e *Engine) Has(name string) bool {
	_, ok := e.sources[name]
	return ok
}

// Names implements Renderer.
func (e *Engine) Names() []string {
	names := make([]string, 0, len(e.sources))
	for name := range e.sources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Render implements Renderer.
func (e *Engine) Render(name string, data *Data) (string, error) {
	source, ok := e.sources[name]
	if !ok {
		return "", fmt.Errorf("unknown template %q", name)
	}
	return e.RenderSource(source, data)
}

// RenderSource implements Renderer.
func (e *Engine) RenderSource(source string, data *Data) (string, error) {
	if data == nil {
		data = &Data{}
	}
	data.prepare()
	tmpl, err := template.New("fakeagents").
		Option("missingkey=error").
		Funcs(funcMap(data)).
		Parse(source)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}

// Validate reports whether source parses.
func (e *Engine) Validate(source string) error {
	data := &Data{Now: time.Now(), Random: mrand.New(mrand.NewPCG(1, 2))}
	data.prepare()
	_, err := template.New("validate").
		Option("missingkey=error").
		Funcs(funcMap(data)).
		Parse(source)
	return err
}

func (d *Data) prepare() {
	if d.Now.IsZero() {
		d.Now = time.Now()
	}
	if d.Random == nil {
		d.Random = mrand.New(mrand.NewPCG(uint64(d.Now.UnixNano()), uint64(d.Turn)+1))
	}
}

func funcMap(data *Data) template.FuncMap {
	return template.FuncMap{
		"upper":      strings.ToUpper,
		"lower":      strings.ToLower,
		"title":      title,
		"trim":       strings.TrimSpace,
		"trimPrefix": strings.TrimPrefix,
		"trimSuffix": strings.TrimSuffix,
		"replace":    strings.ReplaceAll,
		"repeat":     repeat,
		"join":       join,
		"split":      strings.Split,
		"contains":   strings.Contains,
		"hasPrefix":  strings.HasPrefix,
		"hasSuffix":  strings.HasSuffix,
		"count":      strings.Count,
		"truncate":   truncate,
		"indent":     indent,
		"json":       toJSON,
		"jsonPretty": toJSONPretty,
		"quote":      quote,
		"default":    defaultValue,
		"coalesce":   coalesce,
		"list":       func(values ...any) []any { return values },
		"dict":       dict,
		"add":        func(a, b int) int { return a + b },
		"sub":        func(a, b int) int { return a - b },
		"mul":        func(a, b int) int { return a * b },
		"div": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"mod": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a % b
		},
		"min": func(a, b int) int {
			if a < b {
				return a
			}
			return b
		},
		"max": func(a, b int) int {
			if a > b {
				return a
			}
			return b
		},
		"ternary": func(cond bool, a, b any) any {
			if cond {
				return a
			}
			return b
		},
		"now":       func() time.Time { return data.Now },
		"uuid":      newUUID,
		"randInt":   func(min, max int) int { return randInt(data, min, max) },
		"pick":      func(values ...any) any { return pick(data, values) },
		"shuffle":   func(values []any) []any { return shuffle(data, values) },
		"lorem":     func(words int) string { return lorem(data, words) },
		"codeblock": codeBlock,
		"pluralize": pluralize,
	}
}

func title(s string) string {
	upperNext := true
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			upperNext = true
			return r
		}
		if upperNext {
			upperNext = false
			return unicode.ToTitle(r)
		}
		return r
	}, s)
}

func repeat(count int, s string) string {
	if count < 0 {
		count = 0
	}
	if count > 1000 {
		count = 1000
	}
	return strings.Repeat(s, count)
}

func join(sep string, values any) string {
	switch list := values.(type) {
	case []string:
		return strings.Join(list, sep)
	case []any:
		parts := make([]string, 0, len(list))
		for _, value := range list {
			parts = append(parts, fmt.Sprint(value))
		}
		return strings.Join(parts, sep)
	default:
		return fmt.Sprint(values)
	}
}

func truncate(limit int, s string) string {
	if limit <= 0 || utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit]) + "…"
}

func indent(spaces int, s string) string {
	if spaces <= 0 {
		return s
	}
	prefix := strings.Repeat(" ", spaces)
	lines := strings.Split(s, "\n")
	for i := range lines {
		if lines[i] != "" {
			lines[i] = prefix + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

func toJSONPretty(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "null"
	}
	return string(b)
}

func quote(s string) string { return toJSON(s) }

func defaultValue(fallback, value any) any {
	if isEmpty(value) {
		return fallback
	}
	return value
}

func coalesce(values ...any) any {
	for _, value := range values {
		if !isEmpty(value) {
			return value
		}
	}
	return nil
}

func isEmpty(value any) bool {
	if value == nil {
		return true
	}
	switch v := value.(type) {
	case string:
		return v == ""
	case bool:
		return !v
	case int:
		return v == 0
	case int64:
		return v == 0
	case float64:
		return v == 0
	case []any:
		return len(v) == 0
	case []string:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	default:
		return false
	}
}

func dict(values ...any) (map[string]any, error) {
	if len(values)%2 != 0 {
		return nil, errors.New("dict requires an even number of arguments")
	}
	out := make(map[string]any, len(values)/2)
	for i := 0; i < len(values); i += 2 {
		key, ok := values[i].(string)
		if !ok {
			return nil, errors.New("dict keys must be strings")
		}
		out[key] = values[i+1]
	}
	return out, nil
}

func newUUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(buf)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func randInt(data *Data, min, max int) int {
	if max <= min {
		return min
	}
	return min + data.Random.IntN(max-min+1)
}

func pick(data *Data, values []any) any {
	if len(values) == 0 {
		return ""
	}
	return values[data.Random.IntN(len(values))]
}

func shuffle(data *Data, values []any) []any {
	out := make([]any, len(values))
	copy(out, values)
	data.Random.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

var fillerWords = []string{
	"baseline", "signal", "steady", "clear", "measured", "compact", "reliable",
	"flow", "context", "buffer", "stream", "result", "sample", "stable",
}

func lorem(data *Data, words int) string {
	if words <= 0 {
		return ""
	}
	if words > 500 {
		words = 500
	}
	out := make([]string, words)
	for i := range out {
		out[i] = fillerWords[data.Random.IntN(len(fillerWords))]
	}
	return strings.Join(out, " ")
}

func codeBlock(language, body string) string {
	return "```" + language + "\n" + strings.TrimRight(body, "\n") + "\n```"
}

func pluralize(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

// Builtins returns the default chat templates shipped with FakeAgents.
func Builtins() map[string]string {
	return map[string]string{
		"default": `{{- $msg := .UserMessage -}}
{{- if $msg -}}
Here is a simulated response to: "{{ $msg }}"

{{ end -}}
This reply was produced by FakeAgents for model {{ .Model }} (turn {{ .Turn }}). No external model was called.`,
		"echo": `{{ .UserMessage }}`,
		"reasoning": `{{- if or (eq .Effort "high") (eq .Effort "xhigh") (eq .Effort "max") -}}
The request asks: "{{ .UserMessage }}". I need to consider the constraints, check which tools are available, and keep the final answer concise. First I will identify the core intent. Then I will outline the relevant steps and verify that the response does not depend on an external service. Because this is a simulated environment, the reasoning is illustrative rather than a real chain of thought.
{{- else if eq .Effort "low" -}}
The user wants a short answer. I will respond directly.
{{- else -}}
The user is asking about "{{ .UserMessage }}". I will answer concisely and state any assumptions explicitly.
{{- end -}}`,
		"final": `{{- if .LastToolResult -}}
Based on the tool result{{ if .LastToolName }} from ` + "`" + `{{ .LastToolName }}` + "`" + `{{ end }}, here is the outcome:

{{ .LastToolResult }}
{{- else -}}
I have enough information to answer now.
{{- end -}}`,
		"refusal": `I'm sorry, but I can't help with that request.`,
		"json":    `{"result":{{ json .UserMessage }},"simulated":true,"model":{{ json .Model }},"turn":{{ .Turn }}}`,
		"long": `{{ range $i := list 1 2 3 4 5 6 }}Section {{ $i }}. {{ lorem 24 }}

{{ end }}This long response exists to exercise truncation, chunking, and scroll behavior.`,
		"markdown": "## Simulated answer\n\n**Request:** {{ .UserMessage }}\n\n- Model: `{{ .Model }}`\n- Provider: `{{ .Provider }}`\n- Turn: {{ .Turn }}\n\n```text\nDeterministic fixture content produced by FakeAgents.\n```",
		"code":     "Here is a small example:\n\n```go\npackage main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println({{ quote .UserMessage }})\n}\n```",
		"unicode":  "Simulated multilingual response: こんにちは世界 🌏 — привет — مرحبا — 你好，世界。\n\nEmoji boundary check: 😀😃😄😁😆😅😂🤣🥲☺️",
		"empty":    ``,
	}
}
