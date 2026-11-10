package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/engine"
)

// ChatRequest is the OpenAI Chat Completions request body.
type ChatRequest struct {
	Model               string              `json:"model"`
	Messages            []ChatMessage       `json:"messages"`
	Tools               []ChatTool          `json:"tools"`
	ToolChoice          json.RawMessage     `json:"tool_choice"`
	Stream              bool                `json:"stream"`
	StreamOptions       *ChatStreamOptions  `json:"stream_options"`
	MaxTokens           int                 `json:"max_tokens"`
	MaxCompletionTokens int                 `json:"max_completion_tokens"`
	Temperature         *float64            `json:"temperature"`
	TopP                *float64            `json:"top_p"`
	N                   *int                `json:"n"`
	ResponseFormat      *ChatResponseFormat `json:"response_format"`
	ReasoningEffort     string              `json:"reasoning_effort"`
	Reasoning           *ChatReasoning      `json:"reasoning"`
	Thinking            *ChatThinking       `json:"thinking"`
	EnableThinking      *bool               `json:"enable_thinking"`
	PromptCacheKey      string              `json:"prompt_cache_key"`
	ServiceTier         string              `json:"service_tier"`
	Stop                json.RawMessage     `json:"stop"`
	User                string              `json:"user"`
	Metadata            map[string]string   `json:"metadata"`
	Store               *bool               `json:"store"`
}

// ChatStreamOptions configures usage reporting for streams.
type ChatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatReasoning is the Responses-style reasoning object accepted by some
// OpenAI-compatible chat endpoints.
type ChatReasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary"`
}

// ChatThinking is the enable/disable shape used by several compatible servers.
type ChatThinking struct {
	Type string `json:"type"`
}

// ChatResponseFormat describes structured output.
type ChatResponseFormat struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name   string `json:"name"`
		Schema any    `json:"schema"`
		Strict bool   `json:"strict"`
	} `json:"json_schema"`
}

// ChatMessage is one request message.
type ChatMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	Name             string          `json:"name"`
	ToolCalls        []ChatToolCall  `json:"tool_calls"`
	ToolCallID       string          `json:"tool_call_id"`
	ReasoningContent string          `json:"reasoning_content"`
	Reasoning        string          `json:"reasoning"`
	Refusal          *string         `json:"refusal"`
}

// ChatToolCall is an assistant-emitted function call.
type ChatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ChatTool is a function declaration.
type ChatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      bool            `json:"strict"`
	} `json:"function"`
}

func buildChatRequest(cfg *config.Config, body *ChatRequest, raw map[string]any, requestID, sessionKey string, turn int, turnSet bool) (*engine.Request, *apierr.Error) {
	if len(body.Messages) == 0 {
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "missing_required_parameter",
			"Missing required parameter: 'messages'.").WithParam("messages")
	}
	if body.N != nil && *body.N > 1 {
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "unsupported_parameter",
			"FakeAgents currently supports n = 1 only.").WithParam("n")
	}
	if body.MaxTokens > 0 && body.MaxCompletionTokens > 0 {
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
			"Both max_tokens and max_completion_tokens were provided; send only one.").WithParam("max_tokens")
	}

	req := &engine.Request{
		Provider:    apierr.ProviderOpenAI,
		Endpoint:    "chat.completions",
		Model:       strings.TrimSpace(body.Model),
		Stream:      body.Stream,
		Temperature: body.Temperature,
		TopP:        body.TopP,
		RequestID:   requestID,
		SessionKey:  sessionKey,
		Turn:        turn,
		TurnSet:     turnSet,
		Headers:     nil,
		Raw:         raw,
		Metadata:    body.Metadata,
	}
	if body.MaxCompletionTokens > 0 {
		req.MaxOutputTokens = body.MaxCompletionTokens
	} else {
		req.MaxOutputTokens = body.MaxTokens
	}

	var systems []string
	var pendingTools map[string]struct{}
	for i, message := range body.Messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role != "tool" {
			pendingTools = nil
		}
		content, err := parseChatContent(message.Content, fmt.Sprintf("messages[%d].content", i))
		if err != nil {
			return nil, err
		}
		switch role {
		case "system", "developer":
			if content != "" {
				systems = append(systems, content)
			}
		case "user":
			req.Messages = append(req.Messages, engine.Message{Role: engine.RoleUser, Content: content})
		case "assistant":
			converted := engine.Message{Role: engine.RoleAssistant, Content: content}
			if len(message.ToolCalls) > 0 {
				pendingTools = make(map[string]struct{}, len(message.ToolCalls))
			}
			for j, call := range message.ToolCalls {
				if strings.TrimSpace(call.Function.Name) == "" {
					return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
						fmt.Sprintf("messages[%d].tool_calls[%d].function.name is required.", i, j))
				}
				converted.ToolCalls = append(converted.ToolCalls, engine.ToolCall{
					ID:        call.ID,
					Name:      call.Function.Name,
					Arguments: call.Function.Arguments,
				})
				pendingTools[call.ID] = struct{}{}
			}
			reasoning := message.ReasoningContent
			if reasoning == "" {
				reasoning = message.Reasoning
			}
			if reasoning != "" {
				converted.Reasoning = []engine.ReasoningBlock{{Text: reasoning}}
			}
			req.Messages = append(req.Messages, converted)
		case "tool", "function":
			if role == "tool" {
				if _, ok := pendingTools[message.ToolCallID]; !ok || message.ToolCallID == "" {
					return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_tool_result",
						fmt.Sprintf("messages[%d].tool_call_id does not match a pending assistant tool call.", i)).WithParam(fmt.Sprintf("messages[%d].tool_call_id", i))
				}
				delete(pendingTools, message.ToolCallID)
			}
			converted := engine.Message{
				Role:       engine.RoleTool,
				Content:    content,
				ToolCallID: message.ToolCallID,
				ToolName:   message.Name,
				ToolResult: true,
			}
			req.Messages = append(req.Messages, converted)
		default:
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_role",
				fmt.Sprintf("Invalid role %q at messages[%d].", message.Role, i)).WithParam(fmt.Sprintf("messages[%d].role", i))
		}
	}
	req.System = strings.Join(systems, "\n\n")

	for i, tool := range body.Tools {
		if tool.Type != "" && tool.Type != "function" {
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
				fmt.Sprintf("tools[%d].type must be \"function\".", i)).WithParam(fmt.Sprintf("tools[%d].type", i))
		}
		if strings.TrimSpace(tool.Function.Name) == "" {
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
				fmt.Sprintf("tools[%d].function.name is required.", i)).WithParam(fmt.Sprintf("tools[%d].function.name", i))
		}
		var schema any = map[string]any{"type": "object", "properties": map[string]any{}}
		if len(tool.Function.Parameters) > 0 && string(tool.Function.Parameters) != "null" {
			if err := json.Unmarshal(tool.Function.Parameters, &schema); err != nil {
				return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
					fmt.Sprintf("tools[%d].function.parameters is not valid JSON.", i))
			}
		}
		req.Tools = append(req.Tools, engine.Tool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Schema:      schema,
			Strict:      tool.Function.Strict,
		})
	}

	choice, err := parseToolChoice(body.ToolChoice)
	if err != nil {
		return nil, err
	}
	req.ToolChoice = choice

	reasoning, err := parseReasoning(body)
	if err != nil {
		return nil, err
	}
	req.Reasoning = reasoning

	format, err := parseResponseFormat(body.ResponseFormat)
	if err != nil {
		return nil, err
	}
	req.ResponseFormat = format

	stop, err := parseStop(body.Stop)
	if err != nil {
		return nil, err
	}
	req.StopSequences = stop

	req.CacheHint = strings.TrimSpace(body.PromptCacheKey) != "" || strings.TrimSpace(body.User) != ""
	return req, nil
}

func parseChatContent(raw json.RawMessage, param string) (string, *apierr.Error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Refusal  string          `json:"refusal"`
		ImageURL json.RawMessage `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_content",
			fmt.Sprintf("%s must be a string or an array of content parts.", param)).WithParam(param)
	}
	var builder strings.Builder
	for i, part := range parts {
		switch part.Type {
		case "text", "input_text", "output_text":
			builder.WriteString(part.Text)
		case "refusal":
			builder.WriteString(part.Refusal)
		case "image_url", "input_image":
			builder.WriteString("[image]")
		case "input_audio", "audio":
			builder.WriteString("[audio]")
		case "file":
			builder.WriteString("[file]")
		default:
			return "", apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_content",
				fmt.Sprintf("Unsupported content part type %q at %s[%d].", part.Type, param, i)).WithParam(param)
		}
	}
	return builder.String(), nil
}

func parseToolChoice(raw json.RawMessage) (*engine.ToolChoice, *apierr.Error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		switch strings.ToLower(mode) {
		case "auto", "none", "required":
			return &engine.ToolChoice{Mode: strings.ToLower(mode)}, nil
		default:
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_tool_choice",
				fmt.Sprintf("Invalid tool_choice %q.", mode)).WithParam("tool_choice")
		}
	}
	var object struct {
		Type     string `json:"type"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_tool_choice",
			"tool_choice must be a string or an object.").WithParam("tool_choice")
	}
	switch strings.ToLower(object.Type) {
	case "auto", "none", "required":
		return &engine.ToolChoice{Mode: strings.ToLower(object.Type)}, nil
	case "function":
		if object.Function == nil || strings.TrimSpace(object.Function.Name) == "" {
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_tool_choice",
				"tool_choice.function.name is required.").WithParam("tool_choice.function.name")
		}
		return &engine.ToolChoice{Mode: "function", Name: object.Function.Name}, nil
	default:
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_tool_choice",
			fmt.Sprintf("Invalid tool_choice type %q.", object.Type)).WithParam("tool_choice.type")
	}
}

func parseReasoning(body *ChatRequest) (engine.ReasoningRequest, *apierr.Error) {
	request := engine.ReasoningRequest{Effort: strings.ToLower(strings.TrimSpace(body.ReasoningEffort))}
	if body.Reasoning != nil {
		if body.Reasoning.Effort != "" {
			request.Effort = strings.ToLower(strings.TrimSpace(body.Reasoning.Effort))
		}
		request.Summary = strings.ToLower(strings.TrimSpace(body.Reasoning.Summary))
	}
	request.Requested = request.Effort != "" && request.Effort != "none"
	if body.Thinking != nil {
		switch strings.ToLower(body.Thinking.Type) {
		case "enabled", "adaptive", "on", "true":
			request.Requested = true
			if request.Effort == "" {
				request.Effort = "medium"
			}
		case "disabled", "off", "false", "":
			request.Requested = false
		default:
			return request, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
				fmt.Sprintf("Invalid thinking.type %q.", body.Thinking.Type)).WithParam("thinking.type")
		}
	}
	if body.EnableThinking != nil {
		request.Requested = *body.EnableThinking
		if request.Requested && request.Effort == "" {
			request.Effort = "medium"
		}
	}
	if request.Effort != "" {
		switch request.Effort {
		case "none":
			request.Requested = false
		case "minimal", "low", "medium", "high", "xhigh", "max":
		default:
			return request, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
				fmt.Sprintf("Invalid reasoning effort %q.", request.Effort)).WithParam("reasoning_effort")
		}
	}
	if request.Summary != "" {
		switch request.Summary {
		case "auto", "concise", "detailed":
		default:
			return request, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
				fmt.Sprintf("Invalid reasoning summary %q.", request.Summary)).WithParam("reasoning.summary")
		}
	}
	return request, nil
}

func parseResponseFormat(format *ChatResponseFormat) (*engine.ResponseFormat, *apierr.Error) {
	if format == nil {
		return nil, nil
	}
	switch strings.ToLower(format.Type) {
	case "", "text":
		return nil, nil
	case "json_object":
		return &engine.ResponseFormat{Type: "json_object"}, nil
	case "json_schema":
		if format.JSONSchema == nil {
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
				"response_format.json_schema is required when type is json_schema.").WithParam("response_format.json_schema")
		}
		if format.JSONSchema.Schema == nil {
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
				"response_format.json_schema.schema is required.").WithParam("response_format.json_schema.schema")
		}
		return &engine.ResponseFormat{Type: "json_schema", Schema: format.JSONSchema.Schema, Name: format.JSONSchema.Name}, nil
	default:
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
			fmt.Sprintf("Invalid response_format.type %q.", format.Type)).WithParam("response_format.type")
	}
}

func parseStop(raw json.RawMessage) ([]string, *apierr.Error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return nil, nil
		}
		return []string{single}, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
			"stop must be a string or an array of strings.").WithParam("stop")
	}
	if len(list) > 4 {
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
			"stop supports at most 4 sequences.").WithParam("stop")
	}
	return list, nil
}
