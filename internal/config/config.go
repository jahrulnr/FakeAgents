// Package config defines the FakeAgents configuration model and loader.
package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jahrulnr/fakeagents/internal/duration"
	"github.com/jahrulnr/fakeagents/internal/scenario"
)

// Config is the root configuration document.
type Config struct {
	Media           Media             `json:"media" yaml:"media,omitempty"`
	Schema          string            `json:"$schema,omitempty" yaml:"$schema,omitempty"`
	Server          Server            `json:"server" yaml:"server"`
	Logging         Logging           `json:"logging" yaml:"logging"`
	Auth            Auth              `json:"auth" yaml:"auth"`
	RequiredHeaders []HeaderRule      `json:"required_headers,omitempty" yaml:"required_headers,omitempty"`
	Defaults        Defaults          `json:"defaults" yaml:"defaults"`
	Models          []Model           `json:"models,omitempty" yaml:"models,omitempty"`
	Templates       map[string]string `json:"templates,omitempty" yaml:"templates,omitempty"`
	Scenarios       []scenario.Spec   `json:"scenarios,omitempty" yaml:"scenarios,omitempty"`
	ScenarioFiles   []string          `json:"scenario_files,omitempty" yaml:"scenario_files,omitempty"`
	// ReplaceBuiltinScenarios disables the additive built-in scenario list.
	// By default, scenarios from a file are appended to the built-ins.
	ReplaceBuiltinScenarios bool       `json:"replace_builtin_scenarios,omitempty" yaml:"replace_builtin_scenarios,omitempty"`
	RateLimit               RateLimit  `json:"rate_limit" yaml:"rate_limit"`
	Debug                   Debug      `json:"debug" yaml:"debug"`
	Encryption              Encryption `json:"encryption" yaml:"encryption"`
	Responses               Responses  `json:"responses" yaml:"responses"`
	Providers               Providers  `json:"providers" yaml:"providers"`

	// Dir is the absolute directory of the configuration file. It is used to
	// resolve relative @file: references and scenario globs.
	Dir string `json:"-" yaml:"-"`
}

// Server configures the HTTP listener.
type Server struct {
	Host              string            `json:"host" yaml:"host"`
	Port              int               `json:"port" yaml:"port"`
	BasePath          string            `json:"base_path,omitempty" yaml:"base_path,omitempty"`
	TLS               TLS               `json:"tls" yaml:"tls,omitempty"`
	ReadHeaderTimeout duration.Duration `json:"read_header_timeout,omitempty" yaml:"read_header_timeout,omitempty"`
	ReadTimeout       duration.Duration `json:"read_timeout,omitempty" yaml:"read_timeout,omitempty"`
	WriteTimeout      duration.Duration `json:"write_timeout,omitempty" yaml:"write_timeout,omitempty"`
	IdleTimeout       duration.Duration `json:"idle_timeout,omitempty" yaml:"idle_timeout,omitempty"`
	ShutdownTimeout   duration.Duration `json:"shutdown_timeout,omitempty" yaml:"shutdown_timeout,omitempty"`
	MaxBodyBytes      int64             `json:"max_body_bytes,omitempty" yaml:"max_body_bytes,omitempty"`
	CORS              CORS              `json:"cors" yaml:"cors"`
}

// TLS enables HTTPS when both files are provided.
type TLS struct {
	CertFile string `json:"cert_file,omitempty" yaml:"cert_file,omitempty"`
	KeyFile  string `json:"key_file,omitempty" yaml:"key_file,omitempty"`
}

// Enabled reports whether TLS is configured.
func (t TLS) Enabled() bool { return t.CertFile != "" && t.KeyFile != "" }

// CORS configures cross-origin access for browser based test harnesses.
type CORS struct {
	Enabled          *bool             `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	AllowedOrigins   []string          `json:"allowed_origins,omitempty" yaml:"allowed_origins,omitempty"`
	AllowedMethods   []string          `json:"allowed_methods,omitempty" yaml:"allowed_methods,omitempty"`
	AllowedHeaders   []string          `json:"allowed_headers,omitempty" yaml:"allowed_headers,omitempty"`
	ExposedHeaders   []string          `json:"exposed_headers,omitempty" yaml:"exposed_headers,omitempty"`
	AllowCredentials bool              `json:"allow_credentials,omitempty" yaml:"allow_credentials,omitempty"`
	MaxAge           duration.Duration `json:"max_age,omitempty" yaml:"max_age,omitempty"`
}

// IsEnabled reports whether CORS is active (default true).
func (c CORS) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// Logging configures the structured logger.
type Logging struct {
	Level     string `json:"level,omitempty" yaml:"level,omitempty"`
	Format    string `json:"format,omitempty" yaml:"format,omitempty"`
	AccessLog *bool  `json:"access_log,omitempty" yaml:"access_log,omitempty"`
	Output    string `json:"output,omitempty" yaml:"output,omitempty"`
}

// AccessLogEnabled reports whether per-request logs are emitted (default true).
func (l Logging) AccessLogEnabled() bool { return l.AccessLog == nil || *l.AccessLog }

// Auth configures API key validation.
type Auth struct {
	Mode        string   `json:"mode,omitempty" yaml:"mode,omitempty"` // off | optional | required
	AcceptAny   bool     `json:"accept_any,omitempty" yaml:"accept_any,omitempty"`
	Keys        []APIKey `json:"keys,omitempty" yaml:"keys,omitempty"`
	ExemptPaths []string `json:"exempt_paths,omitempty" yaml:"exempt_paths,omitempty"`
}

// APIKey is one accepted credential.
type APIKey struct {
	Key       string   `json:"key" yaml:"key"`
	Name      string   `json:"name,omitempty" yaml:"name,omitempty"`
	Providers []string `json:"providers,omitempty" yaml:"providers,omitempty"`
}

// HeaderRule requires a header before a request reaches a provider handler.
// This is how FakeAgents reproduces clients such as OpenCode that mandate
// x-opencode-session.
type HeaderRule struct {
	Name        string   `json:"name" yaml:"name"`
	Enabled     *bool    `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Providers   []string `json:"providers,omitempty" yaml:"providers,omitempty"`
	Endpoints   []string `json:"endpoints,omitempty" yaml:"endpoints,omitempty"`
	Paths       []string `json:"paths,omitempty" yaml:"paths,omitempty"`
	Methods     []string `json:"methods,omitempty" yaml:"methods,omitempty"`
	Status      int      `json:"status,omitempty" yaml:"status,omitempty"`
	Type        string   `json:"type,omitempty" yaml:"type,omitempty"`
	Code        string   `json:"code,omitempty" yaml:"code,omitempty"`
	Message     string   `json:"message,omitempty" yaml:"message,omitempty"`
	AllowEmpty  bool     `json:"allow_empty,omitempty" yaml:"allow_empty,omitempty"`
	ExemptPaths []string `json:"exempt_paths,omitempty" yaml:"exempt_paths,omitempty"`
}

// IsEnabled reports whether the rule is active (default true).
func (r HeaderRule) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// Defaults holds model and simulation defaults shared by providers.
type Defaults struct {
	Provider         string           `json:"provider,omitempty" yaml:"provider,omitempty"`
	Model            string           `json:"model,omitempty" yaml:"model,omitempty"`
	MaxOutputTokens  int              `json:"max_output_tokens,omitempty" yaml:"max_output_tokens,omitempty"`
	StrictModels     bool             `json:"strict_models,omitempty" yaml:"strict_models,omitempty"`
	RespectMaxTokens *bool            `json:"respect_max_tokens,omitempty" yaml:"respect_max_tokens,omitempty"`
	SessionHeaders   []string         `json:"session_headers,omitempty" yaml:"session_headers,omitempty"`
	Scenario         string           `json:"scenario,omitempty" yaml:"scenario,omitempty"`
	Thinking         Thinking         `json:"thinking" yaml:"thinking"`
	Tooling          Tooling          `json:"tooling" yaml:"tooling"`
	Usage            Usage            `json:"usage" yaml:"usage"`
	Stream           Stream           `json:"stream" yaml:"stream"`
	Latency          Latency          `json:"latency" yaml:"latency"`
	StructuredOutput StructuredOutput `json:"structured_output" yaml:"structured_output"`
}

// RespectMaxTokensEnabled reports the default true behavior.
func (d Defaults) RespectMaxTokensEnabled() bool {
	return d.RespectMaxTokens == nil || *d.RespectMaxTokens
}

// Thinking controls reasoning emission.
type Thinking struct {
	Mode     string `json:"mode,omitempty" yaml:"mode,omitempty"` // mirror | always | never
	Template string `json:"template,omitempty" yaml:"template,omitempty"`
	Redacted bool   `json:"redacted,omitempty" yaml:"redacted,omitempty"`
}

// Tooling controls automatic tool-call generation.
type Tooling struct {
	Mode                  string                    `json:"mode,omitempty" yaml:"mode,omitempty"` // smart | always | never | agent
	Plan                  []string                  `json:"plan,omitempty" yaml:"plan,omitempty"`
	UnknownTool           string                    `json:"unknown_tool,omitempty" yaml:"unknown_tool,omitempty"` // allow | skip | error
	ArgumentStrategy      string                    `json:"argument_strategy,omitempty" yaml:"argument_strategy,omitempty"`
	Arguments             map[string]map[string]any `json:"arguments,omitempty" yaml:"arguments,omitempty"`
	ArgumentTemplate      string                    `json:"argument_template,omitempty" yaml:"argument_template,omitempty"`
	MaxToolCalls          int                       `json:"max_tool_calls,omitempty" yaml:"max_tool_calls,omitempty"`
	IncludeReasoning      *bool                     `json:"include_reasoning,omitempty" yaml:"include_reasoning,omitempty"`
	FinalAfterToolResults *bool                     `json:"final_after_tool_results,omitempty" yaml:"final_after_tool_results,omitempty"`
	FinalTemplate         string                    `json:"final_template,omitempty" yaml:"final_template,omitempty"`
}

// FinalEnabled reports the default true behavior.
func (t Tooling) FinalEnabled() bool {
	return t.FinalAfterToolResults == nil || *t.FinalAfterToolResults
}

// IncludeReasoningEnabled reports the default true behavior.
func (t Tooling) IncludeReasoningEnabled() bool {
	return t.IncludeReasoning == nil || *t.IncludeReasoning
}

// Usage controls token accounting.
type Usage struct {
	Mode             string  `json:"mode,omitempty" yaml:"mode,omitempty"` // computed | zero | fixed
	InputTokens      int     `json:"input_tokens,omitempty" yaml:"input_tokens,omitempty"`
	OutputTokens     int     `json:"output_tokens,omitempty" yaml:"output_tokens,omitempty"`
	ReasoningTokens  int     `json:"reasoning_tokens,omitempty" yaml:"reasoning_tokens,omitempty"`
	CachedTokens     int     `json:"cached_tokens,omitempty" yaml:"cached_tokens,omitempty"`
	CacheWriteTokens int     `json:"cache_write_tokens,omitempty" yaml:"cache_write_tokens,omitempty"`
	Multiplier       float64 `json:"multiplier,omitempty" yaml:"multiplier,omitempty"`
	SimulateCache    *bool   `json:"simulate_cache,omitempty" yaml:"simulate_cache,omitempty"`
	CacheTokens      int     `json:"cache_tokens,omitempty" yaml:"cache_tokens,omitempty"`
}

// SimulateCacheEnabled reports the default true behavior.
func (u Usage) SimulateCacheEnabled() bool { return u.SimulateCache == nil || *u.SimulateCache }

// Stream controls chunking and terminal SSE frames.
type Stream struct {
	ChunkSize    int            `json:"chunk_size,omitempty" yaml:"chunk_size,omitempty"`
	ChunkUnit    string         `json:"chunk_unit,omitempty" yaml:"chunk_unit,omitempty"` // runes | words
	ChunkDelay   duration.Range `json:"chunk_delay" yaml:"chunk_delay,omitempty"`
	IncludeUsage *bool          `json:"include_usage,omitempty" yaml:"include_usage,omitempty"`
	SendDone     *bool          `json:"send_done,omitempty" yaml:"send_done,omitempty"`
}

// SendDoneEnabled reports the default true behavior.
func (s Stream) SendDoneEnabled() bool { return s.SendDone == nil || *s.SendDone }

// Latency controls simulated generation speed.
type Latency struct {
	Enabled          *bool                `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	TimeToFirstToken duration.Range       `json:"time_to_first_token" yaml:"time_to_first_token,omitempty"`
	TokensPerSecond  duration.NumberRange `json:"tokens_per_second" yaml:"tokens_per_second,omitempty"`
	PerChunk         duration.Range       `json:"per_chunk" yaml:"per_chunk,omitempty"`
}

// IsEnabled reports the default true behavior.
func (l Latency) IsEnabled() bool { return l.Enabled == nil || *l.Enabled }

// StructuredOutput controls JSON-schema synthesis.
type StructuredOutput struct {
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"` // auto | text
}

// Media configures deterministic media fixtures.
type Media struct {
	ImageFile     string            `json:"image_file,omitempty" yaml:"image_file,omitempty"`
	SpeechFiles   map[string]string `json:"speech_files,omitempty" yaml:"speech_files,omitempty"`
	Transcription string            `json:"transcription,omitempty" yaml:"transcription,omitempty"`
}

// Model is a catalog entry returned by GET /v1/models.
type Model struct {
	ID              string            `json:"id" yaml:"id"`
	Provider        string            `json:"provider,omitempty" yaml:"provider,omitempty"` // openai | anthropic | both
	DisplayName     string            `json:"display_name,omitempty" yaml:"display_name,omitempty"`
	OwnedBy         string            `json:"owned_by,omitempty" yaml:"owned_by,omitempty"`
	Created         int64             `json:"created,omitempty" yaml:"created,omitempty"`
	ContextWindow   int               `json:"context_window,omitempty" yaml:"context_window,omitempty"`
	MaxOutputTokens int               `json:"max_output_tokens,omitempty" yaml:"max_output_tokens,omitempty"`
	Capabilities    []string          `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Aliases         []string          `json:"aliases,omitempty" yaml:"aliases,omitempty"`
	Reasoning       bool              `json:"reasoning,omitempty" yaml:"reasoning,omitempty"`
	Pricing         map[string]string `json:"pricing,omitempty" yaml:"pricing,omitempty"`
}

// HasCapability reports whether the catalog entry advertises a capability.
func (m Model) HasCapability(capability string) bool {
	if capability == "reasoning" && m.Reasoning {
		return true
	}
	for _, value := range m.Capabilities {
		if strings.EqualFold(value, capability) {
			return true
		}
	}
	return false
}

// AppliesTo reports whether the model belongs to the given provider.
func (m Model) AppliesTo(provider string) bool {
	if m.Provider == "" || m.Provider == "both" || m.Provider == "all" {
		return true
	}
	return strings.EqualFold(m.Provider, provider)
}

// RateLimit configures request and token throttling.
type RateLimit struct {
	Enabled           bool              `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Scope             string            `json:"scope,omitempty" yaml:"scope,omitempty"` // session | global
	RequestsPerMinute int               `json:"requests_per_minute,omitempty" yaml:"requests_per_minute,omitempty"`
	TokensPerMinute   int               `json:"tokens_per_minute,omitempty" yaml:"tokens_per_minute,omitempty"`
	RetryAfter        duration.Duration `json:"retry_after,omitempty" yaml:"retry_after,omitempty"`
	IncludeHeaders    *bool             `json:"include_headers,omitempty" yaml:"include_headers,omitempty"`
	ExemptPaths       []string          `json:"exempt_paths,omitempty" yaml:"exempt_paths,omitempty"`
}

// HeadersEnabled reports the default true behavior.
func (r RateLimit) HeadersEnabled() bool { return r.IncludeHeaders == nil || *r.IncludeHeaders }

// Debug configures inspection endpoints and response headers.
type Debug struct {
	Enabled             *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Headers             *bool `json:"headers,omitempty" yaml:"headers,omitempty"`
	RecordTools         *bool `json:"record_tools,omitempty" yaml:"record_tools,omitempty"`
	MaxRecordedRequests int   `json:"max_recorded_requests,omitempty" yaml:"max_recorded_requests,omitempty"`
}

// IsEnabled reports the default true behavior.
func (d Debug) IsEnabled() bool { return d.Enabled == nil || *d.Enabled }

// HeadersEnabled reports the default true behavior.
func (d Debug) HeadersEnabled() bool { return d.Headers == nil || *d.Headers }

// RecordToolsEnabled reports whether harness tool schemas are captured.
func (d Debug) RecordToolsEnabled() bool { return d.RecordTools != nil && *d.RecordTools }

// Encryption configures opaque reasoning payloads and signatures.
type Encryption struct {
	Key            string `json:"key,omitempty" yaml:"key,omitempty"`
	ValidateReplay *bool  `json:"validate_replay,omitempty" yaml:"validate_replay,omitempty"`
}

// ValidateReplayEnabled reports the default true behavior.
func (e Encryption) ValidateReplayEnabled() bool {
	return e.ValidateReplay == nil || *e.ValidateReplay
}

// Responses configures the Responses API store.
type Responses struct {
	Store      *bool             `json:"store,omitempty" yaml:"store,omitempty"`
	TTL        duration.Duration `json:"ttl,omitempty" yaml:"ttl,omitempty"`
	MaxEntries int               `json:"max_entries,omitempty" yaml:"max_entries,omitempty"`
}

// StoreEnabled reports the default true behavior.
func (r Responses) StoreEnabled() bool { return r.Store == nil || *r.Store }

// Providers holds per-provider fidelity switches.
type Providers struct {
	OpenAI    OpenAI    `json:"openai" yaml:"openai"`
	Anthropic Anthropic `json:"anthropic" yaml:"anthropic"`
	ACP       ACP       `json:"acp" yaml:"acp"`
}

// ACP configures the Agent Client Protocol (ACP) surface exposed by the
// `fakeagents acp` stdio agent.
type ACP struct {
	Title          string `json:"title,omitempty" yaml:"title,omitempty"`
	Permission     string `json:"permission,omitempty" yaml:"permission,omitempty"` // never | mutating | always
	UseClientTools *bool  `json:"use_client_tools,omitempty" yaml:"use_client_tools,omitempty"`
	MaxTurns       int    `json:"max_turns,omitempty" yaml:"max_turns,omitempty"`
}

// PermissionMode reports the effective permission policy (default mutating).
func (a ACP) PermissionMode() string {
	mode := strings.ToLower(strings.TrimSpace(a.Permission))
	if mode == "" {
		return "mutating"
	}
	return mode
}

// ClientToolsEnabled reports whether simulated tool calls may delegate to
// client-advertised fs/* and terminal/* methods (default true).
func (a ACP) ClientToolsEnabled() bool {
	return a.UseClientTools == nil || *a.UseClientTools
}

// MaxIterations bounds engine iterations inside one prompt turn (default 8).
func (a ACP) MaxIterations() int {
	if a.MaxTurns <= 0 {
		return 8
	}
	return a.MaxTurns
}

// OpenAI configures OpenAI-compatible behavior.
type OpenAI struct {
	ChatReasoningField       string `json:"chat_reasoning_field,omitempty" yaml:"chat_reasoning_field,omitempty"` // reasoning_content | reasoning | both | none
	SystemFingerprint        string `json:"system_fingerprint,omitempty" yaml:"system_fingerprint,omitempty"`
	ValidateEncryptedContent *bool  `json:"validate_encrypted_content,omitempty" yaml:"validate_encrypted_content,omitempty"`
}

// ValidateEncryptedEnabled reports the default true behavior.
func (o OpenAI) ValidateEncryptedEnabled() bool {
	return o.ValidateEncryptedContent == nil || *o.ValidateEncryptedContent
}

// Anthropic configures Anthropic-compatible behavior.
type Anthropic struct {
	RequireVersion     *bool  `json:"require_version,omitempty" yaml:"require_version,omitempty"`
	APIVersion         string `json:"api_version,omitempty" yaml:"api_version,omitempty"`
	StreamPing         *bool  `json:"stream_ping,omitempty" yaml:"stream_ping,omitempty"`
	ValidateSignatures *bool  `json:"validate_signatures,omitempty" yaml:"validate_signatures,omitempty"`
}

// RequireVersionEnabled reports the default true behavior.
func (a Anthropic) RequireVersionEnabled() bool {
	return a.RequireVersion == nil || *a.RequireVersion
}

// StreamPingEnabled reports the default true behavior.
func (a Anthropic) StreamPingEnabled() bool {
	return a.StreamPing == nil || *a.StreamPing
}

// ValidateSignaturesEnabled reports the default true behavior.
func (a Anthropic) ValidateSignaturesEnabled() bool {
	return a.ValidateSignatures == nil || *a.ValidateSignatures
}

// Default returns a complete, ready-to-run configuration.
func Default() *Config {
	return &Config{
		Server: Server{
			Host:              "127.0.0.1",
			Port:              11110,
			ReadHeaderTimeout: duration.Duration(10 * time.Second),
			IdleTimeout:       duration.Duration(120 * time.Second),
			ShutdownTimeout:   duration.Duration(15 * time.Second),
			MaxBodyBytes:      32 << 20,
			CORS: CORS{
				Enabled:        new(true),
				AllowedOrigins: []string{"*"},
				AllowedMethods: []string{"GET", "POST", "DELETE", "OPTIONS"},
				AllowedHeaders: []string{"*"},
				ExposedHeaders: []string{"*"},
				MaxAge:         duration.Duration(10 * time.Minute),
			},
		},
		Logging: Logging{Level: "info", Format: "text", AccessLog: new(true)},
		Auth: Auth{
			Mode:        "off",
			ExemptPaths: []string{"/healthz", "/_fakeagents/*"},
		},
		Defaults: Defaults{
			Provider:         "openai",
			Model:            "fake-gpt-4o",
			MaxOutputTokens:  1024,
			RespectMaxTokens: new(true),
			SessionHeaders: []string{
				"x-fakeagents-session",
				"x-opencode-session",
				"x-session-id",
				"x-conversation-id",
			},
			Thinking: Thinking{Mode: "mirror", Template: "reasoning"},
			Tooling: Tooling{
				Mode:             "smart",
				UnknownTool:      "allow",
				ArgumentStrategy: "schema",
				MaxToolCalls:     8,
				FinalTemplate:    "final",
			},
			Usage: Usage{
				Mode:          "computed",
				Multiplier:    1,
				SimulateCache: new(true),
				CacheTokens:   1024,
			},
			Stream: Stream{ChunkSize: 24, ChunkUnit: "runes"},
			Latency: Latency{
				Enabled:          new(true),
				TimeToFirstToken: duration.Range{Min: duration.Duration(20 * time.Millisecond), Max: duration.Duration(60 * time.Millisecond)},
				TokensPerSecond:  duration.NumberRange{Min: 150, Max: 400},
			},
			StructuredOutput: StructuredOutput{Mode: "auto"},
		},
		Models:    defaultModels(),
		Scenarios: defaultScenarios(),
		RateLimit: RateLimit{
			Enabled:           false,
			Scope:             "session",
			RequestsPerMinute: 60,
			TokensPerMinute:   20000,
			RetryAfter:        duration.Duration(time.Second),
			IncludeHeaders:    new(true),
			ExemptPaths:       []string{"/healthz", "/_fakeagents/*"},
		},
		Debug: Debug{
			Enabled:             new(true),
			Headers:             new(true),
			MaxRecordedRequests: 200,
		},
		Encryption: Encryption{ValidateReplay: new(true)},
		Responses: Responses{
			Store:      new(true),
			TTL:        duration.Duration(time.Hour),
			MaxEntries: 1000,
		},
		Providers: Providers{
			OpenAI: OpenAI{
				ChatReasoningField:       "reasoning_content",
				SystemFingerprint:        "fp_fakeagents",
				ValidateEncryptedContent: new(true),
			},
			Anthropic: Anthropic{
				RequireVersion:     new(true),
				APIVersion:         "2023-06-01",
				StreamPing:         new(true),
				ValidateSignatures: new(true),
			},
			ACP: ACP{
				Permission:     "mutating",
				UseClientTools: new(true),
				MaxTurns:       8,
			},
		},
	}
}

//go:fix inline
func boolPtr(value bool) *bool { return new(value) }

//go:fix inline
func intPtr(value int) *int { return new(value) }

func defaultScenarios() []scenario.Spec {
	return []scenario.Spec{
		{
			Name:        "thinking",
			Description: "Force a reasoning/thinking block before the answer.",
			Match:       scenario.Match{Triggers: []string{"reasoning", "thinking"}},
			Respond: scenario.Respond{
				Type:              scenario.TypeText,
				Thinking:          new(true),
				ReasoningTemplate: "reasoning",
			},
		},
		{
			Name:        "tool-call",
			Description: "Emit a schema-generated tool call for the first declared tool, then a final answer after the tool result.",
			Match:       scenario.Match{Triggers: []string{"tool", "tools", "tool-call"}},
			Respond: scenario.Respond{
				Type:     scenario.TypeToolCall,
				Thinking: new(true),
			},
		},
		{
			Name:        "agentic",
			Description: "Multi-turn tool loop driven by the harness tools[] array; final answer after the plan is exhausted.",
			Match:       scenario.Match{Triggers: []string{"agent", "agentic"}},
			Respond: scenario.Respond{
				Agentic:  new(true),
				Thinking: new(true),
			},
		},
		{
			Name:        "steering",
			Description: "Slow first turn, then acknowledges queued/steering messages. Uses a stateful sequence keyed by session.",
			Match:       scenario.Match{Triggers: []string{"steering", "queue"}},
			Respond: scenario.Respond{
				StepsMode: "sequence",
				Latency: &scenario.LatencyOverride{
					TimeToFirstToken: &duration.Range{Min: duration.Duration(500 * time.Millisecond), Max: duration.Duration(800 * time.Millisecond)},
					TokensPerSecond:  &duration.NumberRange{Min: 30, Max: 30},
				},
				Steps: []scenario.Respond{
					{Text: "Working on the first turn. This response is intentionally slow so another message can arrive while it streams.\n\n{{ lorem 150 }}"},
					{Text: "Steering message received: {{ .UserMessage }}\n\nI adjusted the plan.", Latency: &scenario.LatencyOverride{
						TimeToFirstToken: &duration.Range{},
						TokensPerSecond:  &duration.NumberRange{Min: 10000, Max: 10000},
					}},
					{Text: "Queue drained. Final turn.", Latency: &scenario.LatencyOverride{
						TimeToFirstToken: &duration.Range{},
						TokensPerSecond:  &duration.NumberRange{Min: 10000, Max: 10000},
					}},
				},
			},
		},
		{
			Name:        "tool-error",
			Description: "Three-turn script: declared tool -> undeclared tool with malformed arguments -> final answer. Triggers on the phrase test tool error.",
			Match: scenario.Match{
				Triggers:  []string{"tool-error", "tool-error-loop"},
				UserRegex: `(?i)test tool error`,
			},
			Respond: scenario.Respond{
				Thinking: new(true),
				Steps: []scenario.Respond{
					{
						ToolCalls: []scenario.ToolCallSpec{{ToolIndex: new(0)}},
					},
					{
						ToolCalls: []scenario.ToolCallSpec{{
							Name:          "fakeagents_missing_tool",
							Arguments:     map[string]any{},
							ArgumentsJSON: `{"broken": `,
						}},
					},
					{
						Text: "Tool result: {{ .LastToolResult }}\n\nFinal answer after the scripted tool error.",
					},
				},
			},
		},
		{
			Name:        "billing",
			Description: "Billing quota exhausted (OpenAI 429 insufficient_quota, Anthropic 402 billing_error).",
			Match:       scenario.Match{Triggers: []string{"billing", "quota", "insufficient_quota"}},
			Respond: scenario.Respond{
				Type:  scenario.TypeError,
				Error: &scenario.ErrorSpec{Preset: "billing"},
			},
		},
		{
			Name:        "rate-limit",
			Description: "Rate limit response with Retry-After.",
			Match:       scenario.Match{Triggers: []string{"rate-limit", "ratelimit", "429"}},
			Respond: scenario.Respond{
				Type:  scenario.TypeError,
				Error: &scenario.ErrorSpec{Preset: "rate_limit", RetryAfter: durationPtr(2 * time.Second)},
			},
		},
		{
			Name:        "overloaded",
			Description: "Provider overload (Anthropic 529 overloaded_error, OpenAI 503).",
			Match:       scenario.Match{Triggers: []string{"overloaded", "529"}},
			Respond: scenario.Respond{
				Type:  scenario.TypeError,
				Error: &scenario.ErrorSpec{Preset: "overloaded"},
			},
		},
		{
			Name:        "server-error",
			Description: "Internal server error.",
			Match:       scenario.Match{Triggers: []string{"server-error", "500"}},
			Respond: scenario.Respond{
				Type:  scenario.TypeError,
				Error: &scenario.ErrorSpec{Preset: "server_error"},
			},
		},
		{
			Name:        "auth-error",
			Description: "Invalid API key.",
			Match:       scenario.Match{Triggers: []string{"auth-error", "401"}},
			Respond: scenario.Respond{
				Type:  scenario.TypeError,
				Error: &scenario.ErrorSpec{Preset: "authentication"},
			},
		},
		{
			Name:        "context-length",
			Description: "Context window exceeded.",
			Match:       scenario.Match{Triggers: []string{"context-length", "context"}},
			Respond: scenario.Respond{
				Type:  scenario.TypeError,
				Error: &scenario.ErrorSpec{Preset: "context_length"},
			},
		},
		{
			Name:        "slow",
			Description: "Very low throughput (2 tokens/sec, 2s time to first token).",
			Match:       scenario.Match{Triggers: []string{"slow", "low-tps"}},
			Respond: scenario.Respond{
				Type: scenario.TypeText,
				Latency: &scenario.LatencyOverride{
					TimeToFirstToken: &duration.Range{Min: duration.Duration(2 * time.Second), Max: duration.Duration(2 * time.Second)},
					TokensPerSecond:  &duration.NumberRange{Min: 2, Max: 2},
				},
			},
		},
		{
			Name:        "fast",
			Description: "Maximum throughput with no perceptible delay.",
			Match:       scenario.Match{Triggers: []string{"fast", "high-tps"}},
			Respond: scenario.Respond{
				Type: scenario.TypeText,
				Latency: &scenario.LatencyOverride{
					TimeToFirstToken: &duration.Range{},
					TokensPerSecond:  &duration.NumberRange{Min: 10000, Max: 10000},
				},
			},
		},
		{
			Name:        "empty",
			Description: "Empty assistant content with a normal stop reason.",
			Match:       scenario.Match{Triggers: []string{"empty"}},
			Respond:     scenario.Respond{Type: scenario.TypeEmpty},
		},
		{
			Name:        "truncated",
			Description: "Output truncated by max tokens.",
			Match:       scenario.Match{Triggers: []string{"truncated", "length"}},
			Respond:     scenario.Respond{Type: scenario.TypeText, Finish: "length"},
		},
		{
			Name:        "refusal",
			Description: "Safety refusal.",
			Match:       scenario.Match{Triggers: []string{"refusal", "content-filter"}},
			Respond:     scenario.Respond{Type: scenario.TypeRefusal},
		},
		{
			Name:        "mid-stream-error",
			Description: "SSE stream fails after three frames (HTTP 200 already sent).",
			Match:       scenario.Match{Triggers: []string{"mid-stream-error", "stream-error"}},
			Respond: scenario.Respond{
				Type: scenario.TypeText,
				Stream: &scenario.StreamOverride{
					Error:           &scenario.ErrorSpec{Preset: "overloaded"},
					FailAfterChunks: 3,
				},
			},
		},
		{
			Name:        "mid-stream-abort",
			Description: "SSE stream closes abruptly after two frames without a terminal event.",
			Match:       scenario.Match{Triggers: []string{"mid-stream-abort", "stream-abort"}},
			Respond: scenario.Respond{
				Type:   scenario.TypeText,
				Stream: &scenario.StreamOverride{AbortAfterChunks: 2},
			},
		},
		{
			Name:        "unicode",
			Description: "Multilingual and emoji content for chunk-boundary tests.",
			Match:       scenario.Match{Triggers: []string{"unicode", "emoji"}},
			Respond:     scenario.Respond{Type: scenario.TypeText, TextTemplate: "unicode"},
		},
		{
			Name:        "json",
			Description: "Valid JSON object output.",
			Match:       scenario.Match{Triggers: []string{"json"}},
			Respond:     scenario.Respond{Type: scenario.TypeText, TextTemplate: "json"},
		},
		{
			Name:        "long",
			Description: "Long multi-paragraph output.",
			Match:       scenario.Match{Triggers: []string{"long"}},
			Respond:     scenario.Respond{Type: scenario.TypeText, TextTemplate: "long"},
		},
	}
}

func durationPtr(value time.Duration) *duration.Duration {
	converted := duration.Duration(value)
	return &converted
}

func defaultModels() []Model {
	created := int64(1700000000)
	return []Model{
		{
			ID: "fake-gpt-4o", Provider: "openai", DisplayName: "Fake GPT-4o",
			OwnedBy: "fakeagents", Created: created, ContextWindow: 128000,
			MaxOutputTokens: 16384, Capabilities: []string{"chat", "tools", "vision", "structured_output"},
		},
		{
			ID: "fake-gpt-5", Provider: "openai", DisplayName: "Fake GPT-5",
			OwnedBy: "fakeagents", Created: created, ContextWindow: 400000,
			MaxOutputTokens: 128000, Reasoning: true,
			Capabilities: []string{"chat", "tools", "vision", "reasoning", "structured_output"},
		},
		{
			ID: "fake-o4-mini", Provider: "openai", DisplayName: "Fake o4-mini",
			OwnedBy: "fakeagents", Created: created, ContextWindow: 200000,
			MaxOutputTokens: 100000, Reasoning: true,
			Capabilities: []string{"chat", "tools", "reasoning"},
		},
		{
			ID: "fake-claude-sonnet", Provider: "anthropic", DisplayName: "Fake Claude Sonnet",
			OwnedBy: "fakeagents", Created: created, ContextWindow: 200000,
			MaxOutputTokens: 64000, Reasoning: true,
			Capabilities: []string{"chat", "tools", "vision", "reasoning", "structured_output", "prompt_caching"},
		},
		{
			ID: "fake-claude-opus", Provider: "anthropic", DisplayName: "Fake Claude Opus",
			OwnedBy: "fakeagents", Created: created, ContextWindow: 200000,
			MaxOutputTokens: 32000, Reasoning: true,
			Capabilities: []string{"chat", "tools", "vision", "reasoning", "prompt_caching"},
		},
	}
}

// Validate checks the configuration for semantic errors and applies derived
// defaults. It returns a joined error describing every problem found.
func (c *Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if c.Server.Port < 0 || c.Server.Port > 65535 {
		add("server.port must be between 0 and 65535")
	}
	if c.Server.MaxBodyBytes < 0 {
		add("server.max_body_bytes must be >= 0")
	}
	if c.Server.TLS.CertFile != "" && c.Server.TLS.KeyFile == "" {
		add("server.tls.key_file is required when cert_file is set")
	}
	if c.Server.TLS.KeyFile != "" && c.Server.TLS.CertFile == "" {
		add("server.tls.cert_file is required when key_file is set")
	}
	if c.Server.BasePath != "" {
		if !strings.HasPrefix(c.Server.BasePath, "/") {
			add("server.base_path must start with '/'")
		}
		if strings.HasSuffix(c.Server.BasePath, "/") && c.Server.BasePath != "/" {
			add("server.base_path must not end with '/'")
		}
	}
	if c.Server.ShutdownTimeout.D() <= 0 {
		add("server.shutdown_timeout must be > 0")
	}

	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "warning", "error":
	default:
		add("logging.level must be debug, info, warn, or error")
	}
	switch strings.ToLower(c.Logging.Format) {
	case "text", "json", "logfmt":
	default:
		add("logging.format must be text or json")
	}

	switch strings.ToLower(c.Auth.Mode) {
	case "", "off", "optional", "required":
	default:
		add("auth.mode must be off, optional, or required")
	}
	if strings.EqualFold(c.Auth.Mode, "required") && !c.Auth.AcceptAny && len(c.Auth.Keys) == 0 {
		add("auth.keys must not be empty when auth.mode is required (or set auth.accept_any)")
	}
	for i, key := range c.Auth.Keys {
		if strings.TrimSpace(key.Key) == "" {
			add("auth.keys[%d].key must not be empty", i)
		}
	}

	for i, rule := range c.RequiredHeaders {
		if strings.TrimSpace(rule.Name) == "" {
			add("required_headers[%d].name must not be empty", i)
			continue
		}
		if strings.ContainsAny(rule.Name, " :\t\n") {
			add("required_headers[%d].name %q is not a valid header name", i, rule.Name)
		}
		for _, endpoint := range rule.Endpoints {
			if !knownEndpoint(endpoint) {
				add("required_headers[%d].endpoints contains unknown endpoint %q", i, endpoint)
			}
		}
		if rule.Status != 0 && (rule.Status < 100 || rule.Status > 599) {
			add("required_headers[%d].status must be a valid HTTP status", i)
		}
	}

	switch strings.ToLower(c.Defaults.Thinking.Mode) {
	case "", "mirror", "always", "never":
	default:
		add("defaults.thinking.mode must be mirror, always, or never")
	}
	switch strings.ToLower(c.Defaults.Tooling.Mode) {
	case "", "smart", "always", "never", "agent":
	default:
		add("defaults.tooling.mode must be smart, always, never, or agent")
	}
	switch strings.ToLower(c.Defaults.Tooling.UnknownTool) {
	case "", "allow", "skip", "error":
	default:
		add("defaults.tooling.unknown_tool must be allow, skip, or error")
	}
	for i, name := range c.Defaults.Tooling.Plan {
		if strings.TrimSpace(name) == "" {
			add("defaults.tooling.plan[%d] must not be empty", i)
		}
	}
	switch strings.ToLower(c.Defaults.Tooling.ArgumentStrategy) {
	case "", "schema", "static", "template":
	default:
		add("defaults.tooling.argument_strategy must be schema, static, or template")
	}
	switch strings.ToLower(c.Defaults.Usage.Mode) {
	case "", "computed", "zero", "fixed":
	default:
		add("defaults.usage.mode must be computed, zero, or fixed")
	}
	if c.Defaults.Usage.Multiplier < 0 {
		add("defaults.usage.multiplier must be >= 0")
	}
	if c.Defaults.Usage.CacheTokens < 0 {
		add("defaults.usage.cache_tokens must be >= 0")
	}
	if c.Defaults.Tooling.MaxToolCalls < 0 {
		add("defaults.tooling.max_tool_calls must be >= 0")
	}
	switch strings.ToLower(c.Defaults.Stream.ChunkUnit) {
	case "", "runes", "words":
	default:
		add("defaults.stream.chunk_unit must be runes or words")
	}
	if c.Defaults.Stream.ChunkSize < 0 {
		add("defaults.stream.chunk_size must be >= 0")
	}
	switch strings.ToLower(c.Defaults.StructuredOutput.Mode) {
	case "", "auto", "text":
	default:
		add("defaults.structured_output.mode must be auto or text")
	}
	if c.Defaults.MaxOutputTokens <= 0 {
		add("defaults.max_output_tokens must be > 0")
	}
	if c.Defaults.Latency.IsEnabled() {
		if c.Defaults.Latency.TokensPerSecond.Min < 0 || c.Defaults.Latency.TokensPerSecond.Max < 0 {
			add("defaults.latency.tokens_per_second must be >= 0")
		}
		if c.Defaults.Latency.TimeToFirstToken.Min < 0 || c.Defaults.Latency.TimeToFirstToken.Max < 0 {
			add("defaults.latency.time_to_first_token must be >= 0")
		}
	}

	switch strings.ToLower(c.Providers.OpenAI.ChatReasoningField) {
	case "", "reasoning_content", "reasoning", "both", "none":
	default:
		add("providers.openai.chat_reasoning_field must be reasoning_content, reasoning, both, or none")
	}
	if c.Providers.Anthropic.APIVersion == "" {
		add("providers.anthropic.api_version must not be empty")
	}
	switch strings.ToLower(strings.TrimSpace(c.Providers.ACP.Permission)) {
	case "", "never", "mutating", "always":
	default:
		add("providers.acp.permission must be never, mutating, or always")
	}
	if c.Providers.ACP.MaxTurns < 0 {
		add("providers.acp.max_turns must be >= 0")
	}

	switch strings.ToLower(c.RateLimit.Scope) {
	case "", "session", "global":
	default:
		add("rate_limit.scope must be session or global")
	}
	if c.RateLimit.Enabled {
		if c.RateLimit.RequestsPerMinute < 0 || c.RateLimit.TokensPerMinute < 0 {
			add("rate_limit limits must be >= 0")
		}
		if c.RateLimit.RequestsPerMinute == 0 && c.RateLimit.TokensPerMinute == 0 {
			add("rate_limit requires requests_per_minute or tokens_per_minute when enabled")
		}
	}
	if c.Responses.TTL.D() < 0 {
		add("responses.ttl must be >= 0")
	}
	if c.Responses.MaxEntries < 0 {
		add("responses.max_entries must be >= 0")
	}
	if c.Debug.MaxRecordedRequests < 0 {
		add("debug.max_recorded_requests must be >= 0")
	}

	seenModels := make(map[string]bool)
	for i, model := range c.Models {
		if strings.TrimSpace(model.ID) == "" {
			add("models[%d].id must not be empty", i)
			continue
		}
		key := strings.ToLower(model.ID)
		if seenModels[key] {
			add("duplicate model id %q", model.ID)
		}
		seenModels[key] = true
		for unit, price := range model.Pricing {
			value, err := strconv.ParseFloat(price, 64)
			if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value*1e6, 0) {
				add("models[%d].pricing.%s must be a non-negative decimal string", i, unit)
			}
		}
		switch strings.ToLower(model.Provider) {
		case "", "openai", "anthropic", "both", "all":
		default:
			add("models[%d].provider must be openai, anthropic, or both", i)
		}
	}

	seenScenarios := make(map[string]bool)
	for i, spec := range c.Scenarios {
		if err := spec.Validate(); err != nil {
			add("scenarios[%d]: %v", i, err)
		}
		key := strings.ToLower(spec.Name)
		if seenScenarios[key] {
			add("duplicate scenario name %q", spec.Name)
		}
		seenScenarios[key] = true
	}

	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

func knownEndpoint(endpoint string) bool {
	switch endpoint {
	case scenario.EndpointChat, scenario.EndpointResponses, scenario.EndpointMessages, scenario.EndpointCount, scenario.EndpointModels, scenario.EndpointImageGeneration, scenario.EndpointImageEdit, scenario.EndpointSpeech, scenario.EndpointTranscription, scenario.EndpointACPPrompt:
		return true
	default:
		return false
	}
}

// ValidationError aggregates configuration problems.
type ValidationError struct {
	Problems []string
}

// Error implements error.
func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return "invalid configuration: " + e.Problems[0]
	}
	var b strings.Builder
	b.WriteString("invalid configuration:")
	for _, problem := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(problem)
	}
	return b.String()
}
