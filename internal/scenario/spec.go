// Package scenario defines the declarative rules that decide how FakeAgents
// answers a request. Scenarios can match on provider, endpoint, model, request
// shape, headers, JSON fields, turn number, and trigger directives.
package scenario

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strings"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/duration"
)

// Endpoint names used in matching and configuration.
const (
	EndpointChat            = "chat.completions"
	EndpointResponses       = "responses"
	EndpointMessages        = "messages"
	EndpointCount           = "count_tokens"
	EndpointModels          = "models"
	EndpointImageGeneration = "images.generations"
	EndpointImageEdit       = "images.edits"
	EndpointSpeech          = "audio.speech"
	EndpointTranscription   = "audio.transcriptions"
)

// Response types.
const (
	TypeText      = "text"
	TypeReasoning = "reasoning"
	TypeToolCall  = "tool_call"
	TypeRefusal   = "refusal"
	TypeEmpty     = "empty"
	TypeError     = "error"
	TypeRaw       = "raw"
)

// Spec is a scenario as authored in configuration files.
type Spec struct {
	Name        string  `json:"name" yaml:"name"`
	Description string  `json:"description,omitempty" yaml:"description,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Priority    int     `json:"priority,omitempty" yaml:"priority,omitempty"`
	Match       Match   `json:"match" yaml:"match,omitempty"`
	Respond     Respond `json:"respond" yaml:"respond,omitempty"`
}

// Match selects requests. All populated criteria must match; values inside a
// list are OR-ed together.
type Match struct {
	Providers      []string          `json:"providers,omitempty" yaml:"providers,omitempty"`
	Endpoints      []string          `json:"endpoints,omitempty" yaml:"endpoints,omitempty"`
	Models         []string          `json:"models,omitempty" yaml:"models,omitempty"`
	Roles          []string          `json:"roles,omitempty" yaml:"roles,omitempty"`
	Stream         *bool             `json:"stream,omitempty" yaml:"stream,omitempty"`
	Thinking       *bool             `json:"thinking,omitempty" yaml:"thinking,omitempty"`
	HasTools       *bool             `json:"has_tools,omitempty" yaml:"has_tools,omitempty"`
	HasToolResults *bool             `json:"has_tool_results,omitempty" yaml:"has_tool_results,omitempty"`
	Headers        map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	HeaderRegex    map[string]string `json:"header_regex,omitempty" yaml:"header_regex,omitempty"`
	HeaderPresent  []string          `json:"header_present,omitempty" yaml:"header_present,omitempty"`
	JSONEquals     map[string]any    `json:"json_equals,omitempty" yaml:"json_equals,omitempty"`
	JSONRegex      map[string]string `json:"json_regex,omitempty" yaml:"json_regex,omitempty"`
	JSONExists     []string          `json:"json_exists,omitempty" yaml:"json_exists,omitempty"`
	JSONCount      []JSONCount       `json:"json_count,omitempty" yaml:"json_count,omitempty"`
	UserRegex      string            `json:"user_regex,omitempty" yaml:"user_regex,omitempty"`
	SystemRegex    string            `json:"system_regex,omitempty" yaml:"system_regex,omitempty"`
	Turn           *int              `json:"turn,omitempty" yaml:"turn,omitempty"`
	Probability    float64           `json:"probability,omitempty" yaml:"probability,omitempty"`
	// Triggers are alternative names usable through the
	// X-FakeAgents-Scenario header or a [[fakeagents:name]] directive.
	Triggers []string `json:"triggers,omitempty" yaml:"triggers,omitempty"`
}

// JSONCount matches the number of selected JSON values satisfying its filters.
type JSONCount struct {
	Path   string         `json:"path" yaml:"path"`
	Where  map[string]any `json:"where,omitempty" yaml:"where,omitempty"`
	Exists []string       `json:"exists,omitempty" yaml:"exists,omitempty"`
	Empty  []string       `json:"empty,omitempty" yaml:"empty,omitempty"`
	Min    *int           `json:"min,omitempty" yaml:"min,omitempty"`
	Max    *int           `json:"max,omitempty" yaml:"max,omitempty"`
}

// Respond describes the simulated answer.
type Respond struct {
	Type              string            `json:"type,omitempty" yaml:"type,omitempty"`
	Text              string            `json:"text,omitempty" yaml:"text,omitempty"`
	TextTemplate      string            `json:"text_template,omitempty" yaml:"text_template,omitempty"`
	Variants          []string          `json:"variants,omitempty" yaml:"variants,omitempty"`
	Reasoning         string            `json:"reasoning,omitempty" yaml:"reasoning,omitempty"`
	ReasoningTemplate string            `json:"reasoning_template,omitempty" yaml:"reasoning_template,omitempty"`
	RedactedReasoning *bool             `json:"redacted_reasoning,omitempty" yaml:"redacted_reasoning,omitempty"`
	Thinking          *bool             `json:"thinking,omitempty" yaml:"thinking,omitempty"`
	ToolCalls         []ToolCallSpec    `json:"tool_calls,omitempty" yaml:"tool_calls,omitempty"`
	Finish            string            `json:"finish,omitempty" yaml:"finish,omitempty"`
	Usage             *UsageOverride    `json:"usage,omitempty" yaml:"usage,omitempty"`
	Latency           *LatencyOverride  `json:"latency,omitempty" yaml:"latency,omitempty"`
	Stream            *StreamOverride   `json:"stream,omitempty" yaml:"stream,omitempty"`
	Error             *ErrorSpec        `json:"error,omitempty" yaml:"error,omitempty"`
	Headers           map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	RawJSON           string            `json:"raw_json,omitempty" yaml:"raw_json,omitempty"`
	Status            int               `json:"status,omitempty" yaml:"status,omitempty"`
	StopSequence      string            `json:"stop_sequence,omitempty" yaml:"stop_sequence,omitempty"`

	// Steps turns a scenario into a multi-turn script. StepsMode selects how
	// the active step advances: auto (assistant tool-call turns), request
	// (session request index), or sequence (stateful per-session cursor).
	Steps []Respond `json:"steps,omitempty" yaml:"steps,omitempty"`
	// StepsMode is auto (default), request, or sequence.
	StepsMode string `json:"steps_mode,omitempty" yaml:"steps_mode,omitempty"`

	// Agentic enables multi-turn tool use driven by the harness tools[] array
	// (or ToolPlan) for this response.
	Agentic *bool `json:"agentic,omitempty" yaml:"agentic,omitempty"`
	// ToolPlan lists tool names to call on successive assistant turns. Unknown
	// names exercise client handling of undeclared tools.
	ToolPlan []string `json:"tool_plan,omitempty" yaml:"tool_plan,omitempty"`
	// UnknownTool overrides defaults.tooling.unknown_tool for this response.
	// Values: allow, skip, error.
	UnknownTool string `json:"unknown_tool,omitempty" yaml:"unknown_tool,omitempty"`
}

// ToolCallSpec describes one tool invocation. Name may be omitted when
// ToolIndex selects a tool from the request's tools[] array. Arguments are
// either a static object, a JSON template, or generated from the tool schema.
type ToolCallSpec struct {
	Name          string         `json:"name,omitempty" yaml:"name,omitempty"`
	ToolIndex     *int           `json:"tool_index,omitempty" yaml:"tool_index,omitempty"`
	Arguments     map[string]any `json:"arguments,omitempty" yaml:"arguments,omitempty"`
	ArgumentsJSON string         `json:"arguments_json,omitempty" yaml:"arguments_json,omitempty"`
	ID            string         `json:"id,omitempty" yaml:"id,omitempty"`
}

// UsageOverride customises token accounting. Nil fields inherit the computed
// value; setting Mode to "fixed" zeroes fields that are not supplied.
type UsageOverride struct {
	Mode             string   `json:"mode,omitempty" yaml:"mode,omitempty"`
	InputTokens      *int     `json:"input_tokens,omitempty" yaml:"input_tokens,omitempty"`
	OutputTokens     *int     `json:"output_tokens,omitempty" yaml:"output_tokens,omitempty"`
	ReasoningTokens  *int     `json:"reasoning_tokens,omitempty" yaml:"reasoning_tokens,omitempty"`
	CachedTokens     *int     `json:"cached_tokens,omitempty" yaml:"cached_tokens,omitempty"`
	CacheWriteTokens *int     `json:"cache_write_tokens,omitempty" yaml:"cache_write_tokens,omitempty"`
	Multiplier       *float64 `json:"multiplier,omitempty" yaml:"multiplier,omitempty"`
}

// LatencyOverride customises timing for one scenario.
type LatencyOverride struct {
	TimeToFirstToken *duration.Range       `json:"time_to_first_token,omitempty" yaml:"time_to_first_token,omitempty"`
	TokensPerSecond  *duration.NumberRange `json:"tokens_per_second,omitempty" yaml:"tokens_per_second,omitempty"`
	ChunkDelay       *duration.Range       `json:"chunk_delay,omitempty" yaml:"chunk_delay,omitempty"`
}

// StreamOverride customises streaming behavior, including deliberate failures.
type StreamOverride struct {
	ChunkSize        *int            `json:"chunk_size,omitempty" yaml:"chunk_size,omitempty"`
	ChunkUnit        string          `json:"chunk_unit,omitempty" yaml:"chunk_unit,omitempty"`
	ChunkDelay       *duration.Range `json:"chunk_delay,omitempty" yaml:"chunk_delay,omitempty"`
	Error            *ErrorSpec      `json:"error,omitempty" yaml:"error,omitempty"`
	FailAfterChunks  int             `json:"fail_after_chunks,omitempty" yaml:"fail_after_chunks,omitempty"`
	AbortAfterChunks int             `json:"abort_after_chunks,omitempty" yaml:"abort_after_chunks,omitempty"`
	OmitUsage        *bool           `json:"omit_usage,omitempty" yaml:"omit_usage,omitempty"`
	OmitDone         *bool           `json:"omit_done,omitempty" yaml:"omit_done,omitempty"`
	SendPings        *bool           `json:"send_pings,omitempty" yaml:"send_pings,omitempty"`
}

// ErrorSpec configures a provider error. Preset provides realistic defaults;
// explicit fields override them.
type ErrorSpec struct {
	Preset     string             `json:"preset,omitempty" yaml:"preset,omitempty"`
	Status     int                `json:"status,omitempty" yaml:"status,omitempty"`
	Type       string             `json:"type,omitempty" yaml:"type,omitempty"`
	Code       string             `json:"code,omitempty" yaml:"code,omitempty"`
	Param      string             `json:"param,omitempty" yaml:"param,omitempty"`
	Message    string             `json:"message,omitempty" yaml:"message,omitempty"`
	RetryAfter *duration.Duration `json:"retry_after,omitempty" yaml:"retry_after,omitempty"`
	Headers    map[string]string  `json:"headers,omitempty" yaml:"headers,omitempty"`
}

// Context is the normalised request view used by matchers. The scenario
// package deliberately does not depend on provider or engine packages.
type Context struct {
	Provider          string
	Endpoint          string
	Model             string
	Stream            bool
	ThinkingRequested bool
	HasTools          bool
	HasToolResults    bool
	LastRole          string
	LastUserMessage   string
	System            string
	Headers           http.Header
	Raw               map[string]any
	Turn              int
	Session           string
	Random            *rand.Rand
}

var validEndpoints = map[string]struct{}{
	EndpointImageGeneration: {}, EndpointImageEdit: {}, EndpointSpeech: {}, EndpointTranscription: {},
	EndpointChat: {}, EndpointResponses: {}, EndpointMessages: {}, EndpointCount: {}, EndpointModels: {},
}

// Validate checks a spec's semantic correctness. Provider-dependent details are
// validated later by the engine.
func (s *Spec) Validate() error {
	var problems []string
	if strings.TrimSpace(s.Name) == "" {
		problems = append(problems, "name is required")
	}
	if s.Match.Probability < 0 || s.Match.Probability > 1 {
		problems = append(problems, "match.probability must be between 0 and 1")
	}
	for _, endpoint := range s.Match.Endpoints {
		if _, ok := validEndpoints[endpoint]; !ok {
			problems = append(problems, fmt.Sprintf("unknown endpoint %q", endpoint))
		}
	}
	if s.Match.Turn != nil && *s.Match.Turn < 0 {
		problems = append(problems, "match.turn must be >= 0")
	}
	for i, rule := range s.Match.JSONCount {
		if strings.TrimSpace(rule.Path) == "" || (rule.Min == nil && rule.Max == nil) ||
			(rule.Min != nil && *rule.Min < 0) || (rule.Max != nil && *rule.Max < 0) ||
			(rule.Min != nil && rule.Max != nil && *rule.Min > *rule.Max) {
			problems = append(problems, fmt.Sprintf("match.json_count[%d] requires a path and valid min/max bounds", i))
		}
	}
	if _, err := regexp.Compile(s.Match.UserRegex); err != nil {
		problems = append(problems, "match.user_regex: "+err.Error())
	}
	if _, err := regexp.Compile(s.Match.SystemRegex); err != nil {
		problems = append(problems, "match.system_regex: "+err.Error())
	}
	for name, pattern := range s.Match.HeaderRegex {
		if _, err := regexp.Compile(pattern); err != nil {
			problems = append(problems, fmt.Sprintf("match.header_regex[%s]: %v", name, err))
		}
	}
	for path, pattern := range s.Match.JSONRegex {
		if _, err := regexp.Compile(pattern); err != nil {
			problems = append(problems, fmt.Sprintf("match.json_regex[%s]: %v", path, err))
		}
	}
	problems = append(problems, validateRespond("respond", s.Respond, true)...)
	if len(problems) > 0 {
		return fmt.Errorf("scenario %q: %s", s.Name, strings.Join(problems, "; "))
	}
	return nil
}

// EffectiveType returns the response type after inferring it from populated
// fields. An explicit Type always wins.
func (r Respond) EffectiveType() string {
	typ := strings.ToLower(strings.TrimSpace(r.Type))
	if typ != "" {
		return typ
	}
	switch {
	case r.Error != nil:
		return TypeError
	case len(r.ToolCalls) > 0:
		return TypeToolCall
	case strings.TrimSpace(r.RawJSON) != "":
		return TypeRaw
	default:
		return TypeText
	}
}

func validateRespond(label string, r Respond, allowSteps bool) []string {
	var problems []string
	typ := r.EffectiveType()
	switch typ {
	case TypeText, TypeReasoning, TypeRefusal, TypeEmpty, TypeError, TypeRaw, TypeToolCall:
	default:
		problems = append(problems, fmt.Sprintf("%s.type: unknown response type %q", label, r.Type))
	}
	if len(r.Steps) > 0 {
		if !allowSteps {
			problems = append(problems, label+".steps: nested scripts are not supported")
		}
		for i, step := range r.Steps {
			problems = append(problems, validateRespond(fmt.Sprintf("%s.steps[%d]", label, i), step, false)...)
		}
	}
	for i, call := range r.ToolCalls {
		if strings.TrimSpace(call.Name) == "" && call.ToolIndex == nil {
			problems = append(problems, fmt.Sprintf("%s.tool_calls[%d]: name or tool_index is required", label, i))
		}
		if call.ToolIndex != nil && *call.ToolIndex < 0 {
			problems = append(problems, fmt.Sprintf("%s.tool_calls[%d].tool_index must be >= 0", label, i))
		}
	}
	switch strings.ToLower(strings.TrimSpace(r.Finish)) {
	case "", "stop", "length", "tool_calls", "content_filter", "refusal", "end_turn", "max_tokens", "tool_use":
	default:
		problems = append(problems, fmt.Sprintf("%s.finish: unknown value %q", label, r.Finish))
	}
	if r.Error != nil {
		if err := r.Error.Validate(); err != nil {
			problems = append(problems, label+".error: "+err.Error())
		}
	}
	if r.Stream != nil && r.Stream.Error != nil {
		if err := r.Stream.Error.Validate(); err != nil {
			problems = append(problems, label+".stream.error: "+err.Error())
		}
	}
	if r.Stream != nil {
		if r.Stream.ChunkSize != nil && *r.Stream.ChunkSize <= 0 {
			problems = append(problems, label+".stream.chunk_size must be > 0")
		}
		if r.Stream.FailAfterChunks < 0 || r.Stream.AbortAfterChunks < 0 {
			problems = append(problems, label+".stream failure counters must be >= 0")
		}
		if r.Stream.Error != nil && r.Stream.FailAfterChunks == 0 {
			problems = append(problems, label+".stream.error requires fail_after_chunks > 0")
		}
	}
	if typ == TypeError && r.Error == nil {
		problems = append(problems, label+".error is required when type is error")
	}
	if typ == TypeRaw && strings.TrimSpace(r.RawJSON) == "" {
		problems = append(problems, label+".raw_json is required when type is raw")
	}
	switch strings.ToLower(strings.TrimSpace(r.StepsMode)) {
	case "", "auto", "request", "sequence":
	default:
		problems = append(problems, fmt.Sprintf("%s.steps_mode: unknown value %q", label, r.StepsMode))
	}
	switch strings.ToLower(strings.TrimSpace(r.UnknownTool)) {
	case "", "allow", "skip", "error":
	default:
		problems = append(problems, fmt.Sprintf("%s.unknown_tool: unknown value %q", label, r.UnknownTool))
	}
	for i, name := range r.ToolPlan {
		if strings.TrimSpace(name) == "" {
			problems = append(problems, fmt.Sprintf("%s.tool_plan[%d] must not be empty", label, i))
		}
	}
	return problems
}

// Validate checks an error spec.
func (e *ErrorSpec) Validate() error {
	if e.Preset != "" && !apierr.KnownPreset(e.Preset) {
		return fmt.Errorf("unknown error preset %q", e.Preset)
	}
	if e.Status != 0 && (e.Status < 100 || e.Status > 599) {
		return fmt.Errorf("status must be a valid HTTP status")
	}
	if e.Preset == "" && e.Message == "" {
		return fmt.Errorf("message is required when preset is empty")
	}
	return nil
}
