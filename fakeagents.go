// Package fakeagents provides an embeddable HTTP simulator for LLM provider APIs.
//
// A Server implements http.Handler and can be used with httptest.NewServer,
// http.Server, or an application's existing router. Each server owns its own
// sessions, rate limiter, response store, and scenario engine.
package fakeagents

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sync"

	"github.com/jahrulnr/fakeagents/internal/app"
	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/duration"
	"github.com/jahrulnr/fakeagents/internal/httpapi"
	"github.com/jahrulnr/fakeagents/internal/scenario"
	"github.com/jahrulnr/fakeagents/internal/version"
)

// Config is the same configuration used by the CLI.
type Config = config.Config

// Configuration types are available for programmatic construction.
type (
	ServerConfig     = config.Server
	TLS              = config.TLS
	CORS             = config.CORS
	Logging          = config.Logging
	Auth             = config.Auth
	APIKey           = config.APIKey
	HeaderRule       = config.HeaderRule
	Defaults         = config.Defaults
	Thinking         = config.Thinking
	Tooling          = config.Tooling
	Usage            = config.Usage
	Stream           = config.Stream
	Latency          = config.Latency
	StructuredOutput = config.StructuredOutput
	Media            = config.Media
	Model            = config.Model
	RateLimit        = config.RateLimit
	Debug            = config.Debug
	Encryption       = config.Encryption
	Responses        = config.Responses
	Providers        = config.Providers
	OpenAI           = config.OpenAI
	Anthropic        = config.Anthropic
	ACP              = config.ACP
	ValidationError  = config.ValidationError
	Duration         = duration.Duration
	DurationRange    = duration.Range
	NumberRange      = duration.NumberRange
)

// Scenario types configure per-request simulation and fault injection.
type (
	Scenario         = scenario.Spec
	ScenarioMatch    = scenario.Match
	JSONCount        = scenario.JSONCount
	ScenarioResponse = scenario.Respond
	ToolCall         = scenario.ToolCallSpec
	UsageOverride    = scenario.UsageOverride
	LatencyOverride  = scenario.LatencyOverride
	StreamOverride   = scenario.StreamOverride
	ErrorSpec        = scenario.ErrorSpec
)

// Endpoint names can be used in ScenarioMatch.Endpoints.
const (
	EndpointChatCompletions = scenario.EndpointChat
	EndpointResponses       = scenario.EndpointResponses
	EndpointMessages        = scenario.EndpointMessages
	EndpointCountTokens     = scenario.EndpointCount
	EndpointModels          = scenario.EndpointModels
	EndpointImageGeneration = scenario.EndpointImageGeneration
	EndpointImageEdit       = scenario.EndpointImageEdit
	EndpointSpeech          = scenario.EndpointSpeech
	EndpointTranscription   = scenario.EndpointTranscription
	EndpointACPPrompt       = scenario.EndpointACPPrompt
)

// Response types can be used in ScenarioResponse.Type.
const (
	ResponseText      = scenario.TypeText
	ResponseReasoning = scenario.TypeReasoning
	ResponseToolCall  = scenario.TypeToolCall
	ResponseRefusal   = scenario.TypeRefusal
	ResponseEmpty     = scenario.TypeEmpty
	ResponseError     = scenario.TypeError
	ResponseRaw       = scenario.TypeRaw
)

// Server is an isolated FakeAgents instance. Close it after all HTTP requests
// have finished to release background resources.
type Server struct {
	app       *app.App
	closeOnce sync.Once
	closeErr  error
}

// DefaultConfig returns a fresh configuration with the CLI's defaults.
// Configure it before passing it to New.
func DefaultConfig() *Config {
	return config.Default()
}

// LoadConfig loads a JSON or YAML configuration file with the same defaults,
// scenario-file loading, and relative path resolution as the CLI.
func LoadConfig(path string) (*Config, error) {
	return config.Load(path)
}

// New creates an isolated server. A nil configuration uses the CLI defaults.
// The configuration is copied, so later changes to cfg do not alter the server.
// A server does not bind a network port until the caller serves it.
func New(cfg *Config) (*Server, error) {
	if cfg == nil {
		cfg = config.Default()
	}
	// Config is JSON-serializable and this preserves nested maps, slices, and
	// optional values without sharing mutable state between server instances.
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("copy configuration: %w", err)
	}
	var copy config.Config
	if err := json.Unmarshal(data, &copy); err != nil {
		return nil, fmt.Errorf("copy configuration: %w", err)
	}
	copy.Dir = cfg.Dir
	application, err := app.New(&copy, httpapi.BuildInfo{
		Version: version.Version,
		Commit:  version.Commit,
		Date:    version.Date,
		Go:      runtime.Version(),
	})
	if err != nil {
		return nil, err
	}
	return &Server{app: application}, nil
}

// ServeHTTP handles a FakeAgents API request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.app.Server.Handler().ServeHTTP(w, r)
}

// Close releases the server's resources. It is safe to call more than once.
// The caller is responsible for closing its HTTP listener first.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.app.Close()
	})
	return s.closeErr
}
