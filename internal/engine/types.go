package engine

import (
	"net/http"
	"slices"
	"time"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/template"
)

// Role is a normalised conversation role.
type Role string

// Normalised roles.
const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Finish is a provider-neutral finish reason.
type Finish string

// Finish reasons.
const (
	FinishStop          Finish = "stop"
	FinishLength        Finish = "length"
	FinishToolCalls     Finish = "tool_calls"
	FinishContentFilter Finish = "content_filter"
	FinishRefusal       Finish = "refusal"
)

// Plan statuses, used by the Responses API.
const (
	StatusCompleted  = "completed"
	StatusIncomplete = "incomplete"
	StatusFailed     = "failed"
)

// Message is one normalised conversation turn.
type Message struct {
	Role       Role
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
	ToolName   string
	ToolResult bool
	IsError    bool
	Reasoning  []ReasoningBlock
}

// Tool describes a callable function.
type Tool struct {
	Name        string
	Description string
	Schema      any
	Strict      bool
}

// ToolCall is a function invocation emitted by the simulated model.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ToolChoice is a normalised tool selection directive.
type ToolChoice struct {
	Mode string // auto | none | required | function
	Name string
}

// ReasoningRequest captures thinking parameters from any provider wire format.
type ReasoningRequest struct {
	Requested    bool
	Effort       string
	Summary      string // auto | concise | detailed | ""
	Display      string // anthropic summarized | omitted | ""
	BudgetTokens int
}

// ResponseFormat captures structured-output requests.
type ResponseFormat struct {
	Type   string // text | json_object | json_schema
	Schema any
	Name   string
}

// Request is the normalised request handed to the simulation engine.
type Request struct {
	Provider string
	Endpoint string
	Model    string
	Stream   bool

	System          string
	Messages        []Message
	Tools           []Tool
	ToolChoice      *ToolChoice
	MaxOutputTokens int
	Temperature     *float64
	TopP            *float64
	StopSequences   []string

	Reasoning      ReasoningRequest
	ResponseFormat *ResponseFormat
	CacheHint      bool

	PreviousResponseID string
	IncludeEncrypted   bool

	RequestID  string
	SessionKey string
	Turn       int
	TurnSet    bool
	Step       int
	StepSet    bool
	Headers    http.Header
	RawBody    []byte
	Raw        map[string]any
	Metadata   map[string]string

	// Derived by Normalize.
	LastUserMessage  string
	LastRole         Role
	HasToolResults   bool
	LastToolResult   string
	LastToolName     string
	StructuredSchema any
}

// Normalize derives the request summary fields used by scenarios and templates.
func (r *Request) Normalize() {
	r.LastRole = ""
	r.LastUserMessage = ""
	r.HasToolResults = false
	r.LastToolResult = ""
	r.LastToolName = ""
	for _, message := range slices.Backward(r.Messages) {

		if r.LastRole == "" {
			r.LastRole = message.Role
		}
		if r.LastRole == RoleTool && message.ToolResult {
			if !r.HasToolResults {
				r.HasToolResults = true
				r.LastToolResult = message.Content
				r.LastToolName = message.ToolName
				if r.LastToolName == "" {
					r.LastToolName = message.ToolCallID
				}
			}
		}
		if message.Role == RoleUser && r.LastUserMessage == "" {
			r.LastUserMessage = message.Content
		}
	}
	if r.ResponseFormat != nil {
		r.StructuredSchema = r.ResponseFormat.Schema
	}
}

// Usage is normalised token accounting. InputTokens excludes cached and
// cache-write tokens; adapters recombine them according to provider semantics.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	ReasoningTokens  int
	CachedTokens     int
	CacheWriteTokens int
}

// Total returns provider-independent total tokens.
func (u Usage) Total() int {
	return u.InputTokens + u.OutputTokens + u.CachedTokens + u.CacheWriteTokens
}

// ReasoningBlock is one reasoning/thinking segment.
type ReasoningBlock struct {
	ID           string
	Text         string
	Signature    string
	Encrypted    string
	Redacted     bool
	RedactedData string
}

// Latency is the planned timing for a response.
type Latency struct {
	FirstToken time.Duration
	Chunk      time.Duration
	Total      time.Duration
	ChunkSize  int
	ChunkUnit  string
}

// StreamPlan describes deliberate streaming behavior, including failures.
type StreamPlan struct {
	Error            *apierr.Error
	ErrorAfterChunks int
	AbortAfterChunks int
	OmitUsage        bool
	OmitDone         bool
	SendPings        bool
}

// Meta is diagnostic metadata attached to a plan.
type Meta struct {
	Scenario  string
	Session   string
	Turn      int
	Step      int
	Seed      int64
	Thinking  bool
	ToolCalls int
	RequestID string
}

// Plan is the fully resolved simulation output. Provider adapters encode it
// into their native wire format; all decisions live here.
type Plan struct {
	ID           string
	Model        string
	Provider     string
	Endpoint     string
	CreatedAt    time.Time
	Reasoning    []ReasoningBlock
	Text         string
	Refusal      string
	ToolCalls    []ToolCall
	Finish       Finish
	Status       string
	Incomplete   string
	StopSequence string
	Usage        Usage
	Latency      Latency
	Stream       StreamPlan
	Headers      map[string]string
	RawJSON      []byte
	RawStatus    int
	Meta         Meta
}

// Data builds the template context for this plan.
func (p *Plan) Data(req *Request, effort string) *template.Data {
	data := &template.Data{
		Provider:       p.Provider,
		Endpoint:       p.Endpoint,
		Model:          p.Model,
		Stream:         req.Stream,
		Session:        req.SessionKey,
		Scenario:       p.Meta.Scenario,
		Turn:           req.Turn,
		Step:           req.Step,
		RequestID:      req.RequestID,
		Effort:         effort,
		Seed:           p.Meta.Seed,
		UserMessage:    req.LastUserMessage,
		System:         req.System,
		LastToolResult: req.LastToolResult,
		LastToolName:   req.LastToolName,
	}
	data.Messages = make([]template.Message, 0, len(req.Messages))
	for _, message := range req.Messages {
		converted := template.Message{Role: string(message.Role), Content: message.Content}
		for _, call := range message.ToolCalls {
			converted.ToolCalls = append(converted.ToolCalls, template.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
		}
		data.Messages = append(data.Messages, converted)
	}
	data.Tools = make([]template.Tool, 0, len(req.Tools))
	for _, tool := range req.Tools {
		data.Tools = append(data.Tools, template.Tool{Name: tool.Name, Description: tool.Description})
	}
	return data
}
