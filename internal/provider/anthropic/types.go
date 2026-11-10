package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/engine"
	"github.com/jahrulnr/fakeagents/internal/envelope"
)

// MessagesRequest is the Anthropic Messages API request body.
type MessagesRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	Messages      []Message       `json:"messages"`
	System        json.RawMessage `json:"system"`
	Tools         []Tool          `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	Thinking      *Thinking       `json:"thinking"`
	OutputConfig  *OutputConfig   `json:"output_config"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	TopK          *int            `json:"top_k"`
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
	Metadata      *Metadata       `json:"metadata"`
	CacheControl  any             `json:"cache_control"`
	ServiceTier   string          `json:"service_tier"`
}

// Message is one request turn.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Tool is a custom tool declaration.
type Tool struct {
	Type         string          `json:"type"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl any             `json:"cache_control"`
}

// Thinking configures extended reasoning.
type Thinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
	Display      string `json:"display"`
}

// OutputConfig configures effort and structured output.
type OutputConfig struct {
	Effort string  `json:"effort"`
	Format *Format `json:"format"`
}

// Format is a structured-output descriptor.
type Format struct {
	Type   string `json:"type"`
	Schema any    `json:"schema"`
}

// Metadata carries a stable user identifier.
type Metadata struct {
	UserID string `json:"user_id"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	Data      string          `json:"data"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Title     string          `json:"title"`
}

func buildMessagesRequest(cfg *config.Config, sealer envelope.Sealer, body *MessagesRequest, raw map[string]any, cacheHint bool, requestID, sessionKey string, turn int, turnSet bool, requireMaxTokens bool) (*engine.Request, *apierr.Error) {
	if len(body.Messages) == 0 {
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
			"messages: Field required")
	}
	if requireMaxTokens && body.MaxTokens <= 0 {
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
			"max_tokens: Field required")
	}
	if body.MaxTokens < 0 {
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
			"max_tokens: must be greater than 0")
	}

	req := &engine.Request{
		Provider:        apierr.ProviderAnthropic,
		Endpoint:        "messages",
		Model:           strings.TrimSpace(body.Model),
		Stream:          body.Stream,
		Temperature:     body.Temperature,
		TopP:            body.TopP,
		MaxOutputTokens: body.MaxTokens,
		StopSequences:   body.StopSequences,
		RequestID:       requestID,
		SessionKey:      sessionKey,
		Turn:            turn,
		TurnSet:         turnSet,
		Raw:             raw,
		CacheHint:       cacheHint,
	}
	if strings.TrimSpace(body.Model) == "" {
		req.Model = cfg.Defaults.Model
	}

	system, apiErr := parseSystem(body.System)
	if apiErr != nil {
		return nil, apiErr
	}
	req.System = system

	messages, systems, apiErr := parseMessages(cfg, sealer, body.Messages)
	if apiErr != nil {
		return nil, apiErr
	}
	req.Messages = messages
	if len(systems) > 0 {
		joined := strings.Join(systems, "\n\n")
		if req.System != "" {
			req.System = joined + "\n\n" + req.System
		} else {
			req.System = joined
		}
	}

	for index, tool := range body.Tools {
		if tool.Type != "" && tool.Type != "custom" {
			// Server tools are accepted but never invoked.
			continue
		}
		if strings.TrimSpace(tool.Name) == "" {
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
				fmt.Sprintf("tools.%d.name: Field required", index))
		}
		var schema any = map[string]any{"type": "object", "properties": map[string]any{}}
		if len(tool.InputSchema) > 0 && string(tool.InputSchema) != "null" {
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
					fmt.Sprintf("tools.%d.input_schema: must be a valid JSON schema", index))
			}
		}
		req.Tools = append(req.Tools, engine.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			Schema:      schema,
		})
	}

	choice, apiErr := parseToolChoice(body.ToolChoice)
	if apiErr != nil {
		return nil, apiErr
	}
	req.ToolChoice = choice

	reasoning, apiErr := parseThinking(body)
	if apiErr != nil {
		return nil, apiErr
	}
	req.Reasoning = reasoning

	if body.OutputConfig != nil && body.OutputConfig.Format != nil {
		switch strings.ToLower(body.OutputConfig.Format.Type) {
		case "", "text":
		case "json_schema":
			if body.OutputConfig.Format.Schema == nil {
				return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
					"output_config.format.schema: Field required")
			}
			req.ResponseFormat = &engine.ResponseFormat{Type: "json_schema", Schema: body.OutputConfig.Format.Schema}
		default:
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
				fmt.Sprintf("output_config.format.type: unsupported value %q", body.OutputConfig.Format.Type))
		}
	}

	return req, nil
}

func parseSystem(raw json.RawMessage) (string, *apierr.Error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
			"system: must be a string or an array of text blocks")
	}
	var builder strings.Builder
	for i, block := range blocks {
		if block.Type != "" && block.Type != "text" {
			return "", apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
				fmt.Sprintf("system.%d.type: only text blocks are supported", i))
		}
		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString(block.Text)
	}
	return builder.String(), nil
}

func parseMessages(cfg *config.Config, sealer envelope.Sealer, input []Message) ([]engine.Message, []string, *apierr.Error) {
	var messages []engine.Message
	var systems []string
	var pendingTools map[string]string

	for index, message := range input {
		toolNames := pendingTools
		pendingTools = nil
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == "system" {
			text, err := parseTextContent(message.Content, fmt.Sprintf("messages.%d.content", index))
			if err != nil {
				return nil, nil, err
			}
			if text != "" {
				systems = append(systems, text)
			}
			continue
		}
		if role != "user" && role != "assistant" {
			return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
				fmt.Sprintf("messages.%d.role: unexpected value %q", index, message.Role))
		}

		text, blocks, apiErr := splitContent(message.Content, fmt.Sprintf("messages.%d.content", index))
		if apiErr != nil {
			return nil, nil, apiErr
		}

		converted := engine.Message{Role: engine.Role(role), Content: text}
		var toolMessages []engine.Message
		for blockIndex, block := range blocks {
			switch block.Type {
			case "text":
				converted.Content += block.Text
			case "image", "document":
				converted.Content += "[image]"
			case "tool_use":
				if block.Name == "" {
					return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
						fmt.Sprintf("messages.%d.content.%d.name: Field required", index, blockIndex))
				}
				arguments := "{}"
				if len(block.Input) > 0 && string(block.Input) != "null" {
					arguments = string(block.Input)
				}
				converted.ToolCalls = append(converted.ToolCalls, engine.ToolCall{
					ID:        block.ID,
					Name:      block.Name,
					Arguments: arguments,
				})
				if role == "assistant" {
					if pendingTools == nil {
						pendingTools = make(map[string]string)
					}
					pendingTools[block.ID] = block.Name
				}
			case "tool_result":
				name, pending := toolNames[block.ToolUseID]
				if role != "user" || block.ToolUseID == "" || !pending {
					return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
						fmt.Sprintf("messages.%d.content.%d.tool_use_id: no matching tool_use in the previous assistant message", index, blockIndex))
				}
				delete(toolNames, block.ToolUseID)
				content, err := parseToolResult(block.Content, fmt.Sprintf("messages.%d.content.%d.content", index, blockIndex))
				if err != nil {
					return nil, nil, err
				}
				toolMessages = append(toolMessages, engine.Message{
					Role:       engine.RoleTool,
					Content:    content,
					ToolCallID: block.ToolUseID,
					ToolName:   name,
					ToolResult: true,
					IsError:    block.IsError,
				})
			case "thinking":
				if block.Signature == "" {
					return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
						fmt.Sprintf("messages.%d.content.%d.signature: Field required", index, blockIndex))
				}
				if sealer != nil && cfg.Providers.Anthropic.ValidateSignaturesEnabled() && cfg.Encryption.ValidateReplayEnabled() {
					payload, err := sealer.Open("anthropic.thinking", block.Signature)
					displayText, hasDisplayText := payload["display_text"].(string)
					// Older signatures contain the full text only, including for omitted display.
					if !hasDisplayText && block.Thinking != "" {
						displayText, hasDisplayText = payload["text"].(string)
					}
					if err != nil || (hasDisplayText && displayText != block.Thinking) {
						return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
							fmt.Sprintf("messages.%d.content.%d: thinking blocks cannot be modified", index, blockIndex))
					}
				}
				converted.Reasoning = append(converted.Reasoning, engine.ReasoningBlock{
					Text:      block.Thinking,
					Signature: block.Signature,
				})
			case "redacted_thinking":
				if block.Data == "" {
					return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
						fmt.Sprintf("messages.%d.content.%d.data: Field required", index, blockIndex))
				}
				if sealer != nil && cfg.Providers.Anthropic.ValidateSignaturesEnabled() && cfg.Encryption.ValidateReplayEnabled() {
					if _, err := sealer.Open("anthropic.redacted", block.Data); err != nil {
						return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
							fmt.Sprintf("messages.%d.content.%d: redacted thinking blocks cannot be modified", index, blockIndex))
					}
				}
				converted.Reasoning = append(converted.Reasoning, engine.ReasoningBlock{Redacted: true, RedactedData: block.Data})
			case "search_result":
				if converted.Content != "" {
					converted.Content += "\n"
				}
				converted.Content += block.Title
			case "container_upload":
				continue
			default:
				if strings.HasSuffix(block.Type, "_tool_result") || block.Type == "server_tool_use" {
					continue
				}
				return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
					fmt.Sprintf("messages.%d.content.%d.type: unsupported content block %q", index, blockIndex, block.Type))
			}
		}

		if role == "assistant" {
			messages = appendAssistant(messages, converted)
			continue
		}
		if converted.Content != "" {
			messages = append(messages, converted)
		}
		messages = append(messages, toolMessages...)
	}

	if len(messages) == 0 {
		return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
			"messages: at least one user or assistant message is required")
	}
	return messages, systems, nil
}

func appendAssistant(messages []engine.Message, message engine.Message) []engine.Message {
	if len(messages) > 0 && messages[len(messages)-1].Role == engine.RoleAssistant {
		last := &messages[len(messages)-1]
		last.Content += message.Content
		last.ToolCalls = append(last.ToolCalls, message.ToolCalls...)
		last.Reasoning = append(last.Reasoning, message.Reasoning...)
		return messages
	}
	return append(messages, message)
}

func splitContent(raw json.RawMessage, param string) (string, []contentBlock, *apierr.Error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
			param+": must be a string or an array of content blocks")
	}
	return "", blocks, nil
}

func parseTextContent(raw json.RawMessage, param string) (string, *apierr.Error) {
	text, blocks, err := splitContent(raw, param)
	if err != nil {
		return "", err
	}
	if text != "" {
		return text, nil
	}
	var builder strings.Builder
	for _, block := range blocks {
		switch block.Type {
		case "text":
			builder.WriteString(block.Text)
		case "image", "document":
			builder.WriteString("[image]")
		case "tool_result":
			content, err := parseToolResult(block.Content, param)
			if err != nil {
				return "", err
			}
			builder.WriteString(content)
		default:
			return "", apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
				param+": unsupported content block "+block.Type)
		}
	}
	return builder.String(), nil
}

func parseToolResult(raw json.RawMessage, param string) (string, *apierr.Error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
			param+": must be a string or an array of content blocks")
	}
	var builder strings.Builder
	for i, block := range blocks {
		switch block.Type {
		case "text":
			builder.WriteString(block.Text)
		case "image", "document":
			builder.WriteString("[image]")
		default:
			return "", apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
				fmt.Sprintf("%s.%d.type: unsupported tool_result content %q", param, i, block.Type))
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
		case "auto", "none", "required", "any":
			return &engine.ToolChoice{Mode: normalizeChoice(mode)}, nil
		default:
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
				"tool_choice: invalid value "+mode)
		}
	}
	var object struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
			"tool_choice: must be an object")
	}
	switch strings.ToLower(object.Type) {
	case "auto", "none", "any", "required":
		return &engine.ToolChoice{Mode: normalizeChoice(object.Type)}, nil
	case "tool":
		if object.Name == "" {
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
				"tool_choice.name: Field required")
		}
		return &engine.ToolChoice{Mode: "function", Name: object.Name}, nil
	default:
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
			fmt.Sprintf("tool_choice.type: unexpected value %q", object.Type))
	}
}

func normalizeChoice(value string) string {
	switch strings.ToLower(value) {
	case "any", "required":
		return "required"
	case "none":
		return "none"
	default:
		return "auto"
	}
}

func parseThinking(body *MessagesRequest) (engine.ReasoningRequest, *apierr.Error) {
	reasoning := engine.ReasoningRequest{}
	if body.Thinking != nil {
		switch strings.ToLower(body.Thinking.Type) {
		case "enabled":
			reasoning.Requested = true
			if body.Thinking.BudgetTokens < 1024 {
				return reasoning, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
					"thinking.budget_tokens: must be at least 1024")
			}
			if body.MaxTokens > 0 && body.Thinking.BudgetTokens >= body.MaxTokens {
				return reasoning, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
					"thinking.budget_tokens: must be less than max_tokens")
			}
			reasoning.BudgetTokens = body.Thinking.BudgetTokens
		case "adaptive":
			reasoning.Requested = true
		case "disabled":
			reasoning.Requested = false
		default:
			return reasoning, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
				fmt.Sprintf("thinking.type: unexpected value %q", body.Thinking.Type))
		}
		switch strings.ToLower(body.Thinking.Display) {
		case "", "summarized", "omitted", "updates":
			reasoning.Display = strings.ToLower(body.Thinking.Display)
		default:
			return reasoning, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
				fmt.Sprintf("thinking.display: unexpected value %q", body.Thinking.Display))
		}
		if reasoning.Display == "" {
			reasoning.Display = "summarized"
		}
	}
	if body.OutputConfig != nil && body.OutputConfig.Effort != "" {
		effort := strings.ToLower(body.OutputConfig.Effort)
		switch effort {
		case "low", "medium", "high", "xhigh", "max":
			reasoning.Effort = effort
		default:
			return reasoning, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_request",
				fmt.Sprintf("output_config.effort: unexpected value %q", body.OutputConfig.Effort))
		}
	}
	if reasoning.Requested && reasoning.Effort == "" {
		reasoning.Effort = "medium"
	}
	return reasoning, nil
}
