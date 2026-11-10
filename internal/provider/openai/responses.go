package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/engine"
	"github.com/jahrulnr/fakeagents/internal/envelope"
	"github.com/jahrulnr/fakeagents/internal/httpx"
	"github.com/jahrulnr/fakeagents/internal/provider/shared"
	"github.com/jahrulnr/fakeagents/internal/ratelimit"
	"github.com/jahrulnr/fakeagents/internal/store"
	"github.com/jahrulnr/fakeagents/internal/wire"
)

// ResponsesRequest is the OpenAI Responses API request body.
type ResponsesRequest struct {
	Model              string              `json:"model"`
	Input              json.RawMessage     `json:"input"`
	Instructions       *string             `json:"instructions"`
	Tools              []ResponsesTool     `json:"tools"`
	ToolChoice         json.RawMessage     `json:"tool_choice"`
	PreviousResponseID string              `json:"previous_response_id"`
	Store              *bool               `json:"store"`
	MaxOutputTokens    int                 `json:"max_output_tokens"`
	Reasoning          *ResponsesReasoning `json:"reasoning"`
	Text               *ResponsesText      `json:"text"`
	Include            []string            `json:"include"`
	Stream             bool                `json:"stream"`
	PromptCacheKey     string              `json:"prompt_cache_key"`
	ParallelToolCalls  *bool               `json:"parallel_tool_calls"`
	Temperature        *float64            `json:"temperature"`
	TopP               *float64            `json:"top_p"`
	Metadata           map[string]string   `json:"metadata"`
	Truncation         string              `json:"truncation"`
	Background         *bool               `json:"background"`
	ServiceTier        string              `json:"service_tier"`
	User               string              `json:"user"`
}

// ResponsesReasoning is the reasoning configuration object.
type ResponsesReasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary"`
}

// ResponsesText configures output formatting.
type ResponsesText struct {
	Format    *ResponsesFormat `json:"format"`
	Verbosity string           `json:"verbosity"`
}

// ResponsesFormat is a structured-output format descriptor.
type ResponsesFormat struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Schema any    `json:"schema"`
	Strict bool   `json:"strict"`
}

// ResponsesTool is a function or built-in tool declaration.
type ResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

// Responses handles POST /v1/responses.
func (h *Handler) Responses(w http.ResponseWriter, r *http.Request) {
	opts := h.options(r)
	raw, apiErr := httpx.ReadBody(w, r, h.cfg.Server.MaxBodyBytes)
	if apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return
	}
	var body ResponsesRequest
	if apiErr := httpx.DecodeBytes(raw, &body); apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return
	}
	rawMap := httpx.DecodeMap(raw)
	if body.Background != nil && *body.Background {
		h.fail(w, r, opts, apierr.New(http.StatusBadRequest, "invalid_request_error", "unsupported_parameter",
			"FakeAgents does not support background responses; use stream or a synchronous request.").WithParam("background"))
		return
	}

	combined, apiErr := h.combineInput(&body)
	if apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return
	}

	sessionKey, turn, turnSet := shared.Session(h.cfg, r, h.sessions, body.PromptCacheKey, body.User)
	req, apiErr := buildResponsesRequest(h.cfg, h.sealer, &body, combined, rawMap, shared.RequestID(r), sessionKey, turn, turnSet)
	if apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return
	}
	req.Headers = r.Header
	req.RawBody = raw
	shared.RecordTools(r, h.cfg.Debug.RecordToolsEnabled(), req.Tools)

	model, apiErr := h.engine.ResolveModel(apierr.ProviderOpenAI, req.Model)
	if apiErr != nil {
		h.fail(w, r, opts, apiErr)
		return
	}
	req.Model = model
	shared.Record(r, apierr.ProviderOpenAI, "responses", model, "", sessionKey, turn, body.Stream)

	requestLimit := ratelimit.DecisionFrom(r.Context())
	tokenLimit := h.limiter.AdmitTokens(sessionKey, h.engine.CountInputTokens(req))
	if !tokenLimit.Allowed {
		wire.SetRateLimitHeaders(w, apierr.ProviderOpenAI, requestLimit, tokenLimit)
		apiErr := apierr.MustPreset(apierr.ProviderOpenAI, "rate_limit")
		apiErr.RetryAfter = tokenLimit.RetryAfter
		h.fail(w, r, opts, apiErr)
		return
	}
	if h.cfg.RateLimit.HeadersEnabled() {
		wire.SetRateLimitHeaders(w, apierr.ProviderOpenAI, requestLimit, tokenLimit)
	}

	plan, err := h.engine.Generate(r.Context(), req)
	if err != nil {
		h.failGenerate(w, r, opts, err)
		return
	}
	shared.Record(r, apierr.ProviderOpenAI, "responses", plan.Model, plan.Meta.Scenario, sessionKey, plan.Meta.Turn, body.Stream)
	shared.RecordStep(r, plan.Meta.Step)
	shared.RecordUsage(r, plan.Usage.InputTokens+plan.Usage.CachedTokens+plan.Usage.CacheWriteTokens, plan.Usage.OutputTokens)

	if plan.RawJSON != nil {
		if err := h.clock.Sleep(r.Context(), plan.Latency.Total); err != nil {
			return
		}
		h.writeRaw(w, plan)
		return
	}

	includeEncrypted := containsString(body.Include, "reasoning.encrypted_content")
	items := buildResponseItems(h.ids, plan, includeEncrypted)
	storeEnabled := h.cfg.Responses.StoreEnabled()
	if body.Store != nil {
		storeEnabled = *body.Store
	}
	response := buildResponsesPayload(req, plan, items, rawMap, storeEnabled)

	if storeEnabled && h.responses != nil {
		responseJSON, marshalErr := json.Marshal(response)
		if marshalErr != nil {
			h.fail(w, r, opts, apierr.New(http.StatusInternalServerError, "server_error", "server_error", "Unable to encode response."))
			return
		}
		outputJSON := make([]json.RawMessage, 0, len(items))
		for _, item := range items {
			encoded, _ := json.Marshal(fullResponseItem(item))
			outputJSON = append(outputJSON, encoded)
		}
		h.responses.Put(&store.Record{
			ID:         plan.ID,
			CreatedAt:  plan.CreatedAt,
			Model:      plan.Model,
			Provider:   apierr.ProviderOpenAI,
			SessionKey: sessionKey,
			Input:      combined,
			Output:     outputJSON,
			Response:   responseJSON,
		})
	}

	if body.Stream {
		h.streamResponses(w, r, req, plan, response, items)
		return
	}
	if err := h.clock.Sleep(r.Context(), plan.Latency.Total); err != nil {
		return
	}
	h.prepareHeaders(w, plan)
	if plan.Latency.Total > 0 {
		w.Header().Set("openai-processing-ms", formatMillis(plan.Latency.Total))
	}
	if err := httpx.WriteJSON(w, http.StatusOK, response); err != nil {
		h.logger.Error("write responses payload", "error", err, "request_id", plan.Meta.RequestID)
	}
}

func (h *Handler) combineInput(body *ResponsesRequest) ([]json.RawMessage, *apierr.Error) {
	var combined []json.RawMessage
	if body.PreviousResponseID != "" {
		if h.responses == nil {
			return nil, previousResponseError(body.PreviousResponseID)
		}
		record, ok := h.responses.Get(body.PreviousResponseID)
		if !ok {
			return nil, previousResponseError(body.PreviousResponseID)
		}
		combined = append(combined, record.Input...)
		combined = append(combined, record.Output...)
	}
	trimmed := strings.TrimSpace(string(body.Input))
	if trimmed == "" || trimmed == "null" {
		if len(combined) == 0 {
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "missing_required_parameter",
				"Missing required parameter: 'input'.").WithParam("input")
		}
		return combined, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var items []json.RawMessage
		if err := json.Unmarshal(body.Input, &items); err != nil {
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_input",
				"input must be a string or an array of input items.").WithParam("input")
		}
		combined = append(combined, items...)
		return combined, nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		combined = append(combined, body.Input)
		return combined, nil
	}
	return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_input",
		"input must be a string or an array of input items.").WithParam("input")
}

func previousResponseError(id string) *apierr.Error {
	apiErr := apierr.New(http.StatusNotFound, "invalid_request_error", "previous_response_id_not_found",
		fmt.Sprintf("Previous response with id %q was not found. It may have expired or been deleted.", id))
	apiErr.Param = "previous_response_id"
	return apiErr
}

func buildResponsesRequest(cfg *config.Config, sealer envelope.Sealer, body *ResponsesRequest, input []json.RawMessage, raw map[string]any, requestID, sessionKey string, turn int, turnSet bool) (*engine.Request, *apierr.Error) {
	req := &engine.Request{
		Provider:           apierr.ProviderOpenAI,
		Endpoint:           "responses",
		Model:              strings.TrimSpace(body.Model),
		Stream:             body.Stream,
		Temperature:        body.Temperature,
		TopP:               body.TopP,
		MaxOutputTokens:    body.MaxOutputTokens,
		PreviousResponseID: body.PreviousResponseID,
		RequestID:          requestID,
		SessionKey:         sessionKey,
		Turn:               turn,
		TurnSet:            turnSet,
		Raw:                raw,
		Metadata:           body.Metadata,
		IncludeEncrypted:   containsString(body.Include, "reasoning.encrypted_content"),
	}
	if body.Instructions != nil {
		req.System = *body.Instructions
	}

	messages, systems, apiErr := parseResponsesInput(cfg, sealer, input)
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

	for i, tool := range body.Tools {
		if tool.Type != "" && tool.Type != "function" {
			// Built-in tools are accepted but never invoked by the simulator.
			continue
		}
		if strings.TrimSpace(tool.Name) == "" {
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
				fmt.Sprintf("tools[%d].name is required.", i)).WithParam(fmt.Sprintf("tools[%d].name", i))
		}
		var schema any = map[string]any{"type": "object", "properties": map[string]any{}}
		if len(tool.Parameters) > 0 && string(tool.Parameters) != "null" {
			if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
				return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
					fmt.Sprintf("tools[%d].parameters is not valid JSON.", i))
			}
		}
		req.Tools = append(req.Tools, engine.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			Schema:      schema,
			Strict:      tool.Strict,
		})
	}

	choice, apiErr := parseResponsesToolChoice(body.ToolChoice)
	if apiErr != nil {
		return nil, apiErr
	}
	req.ToolChoice = choice

	reasoning := engine.ReasoningRequest{}
	if body.Reasoning != nil {
		reasoning.Effort = strings.ToLower(strings.TrimSpace(body.Reasoning.Effort))
		reasoning.Summary = strings.ToLower(strings.TrimSpace(body.Reasoning.Summary))
		reasoning.Requested = reasoning.Effort != "" && reasoning.Effort != "none"
		if reasoning.Summary != "" {
			switch reasoning.Summary {
			case "auto", "concise", "detailed":
			default:
				return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
					fmt.Sprintf("Invalid reasoning summary %q.", reasoning.Summary)).WithParam("reasoning.summary")
			}
		}
		if reasoning.Effort != "" {
			switch reasoning.Effort {
			case "none":
				reasoning.Requested = false
			case "minimal", "low", "medium", "high", "xhigh", "max":
			default:
				return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
					fmt.Sprintf("Invalid reasoning effort %q.", reasoning.Effort)).WithParam("reasoning.effort")
			}
		}
	}
	if req.IncludeEncrypted {
		reasoning.Requested = true
	}
	req.Reasoning = reasoning

	format, apiErr := parseResponsesFormat(body.Text)
	if apiErr != nil {
		return nil, apiErr
	}
	req.ResponseFormat = format
	req.CacheHint = strings.TrimSpace(body.PromptCacheKey) != "" || body.PreviousResponseID != ""
	return req, nil
}

func parseResponsesInput(cfg *config.Config, sealer envelope.Sealer, input []json.RawMessage) ([]engine.Message, []string, *apierr.Error) {
	var messages []engine.Message
	var systems []string
	toolNames := make(map[string]string)
	for index, raw := range input {
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "" || trimmed == "null" {
			continue
		}
		if strings.HasPrefix(trimmed, "\"") {
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_input",
					fmt.Sprintf("input[%d] is not a valid string.", index))
			}
			messages = append(messages, engine.Message{Role: engine.RoleUser, Content: text})
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_input",
				fmt.Sprintf("input[%d] is not a valid object.", index))
		}
		typ, _ := item["type"].(string)
		if typ == "" {
			if _, ok := item["role"]; ok {
				typ = "message"
			}
		}
		switch typ {
		case "message":
			role, _ := item["role"].(string)
			content, err := parseResponsesContent(item["content"])
			if err != nil {
				return nil, nil, err
			}
			switch strings.ToLower(role) {
			case "system", "developer":
				if content != "" {
					systems = append(systems, content)
				}
			case "user":
				messages = append(messages, engine.Message{Role: engine.RoleUser, Content: content})
			case "assistant":
				messages = appendAssistant(messages, engine.Message{Role: engine.RoleAssistant, Content: content})
			default:
				return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_role",
					fmt.Sprintf("Invalid message role %q at input[%d].", role, index))
			}
		case "function_call":
			name, _ := item["name"].(string)
			if name == "" {
				return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_input",
					fmt.Sprintf("input[%d].name is required for function_call items.", index))
			}
			callID, _ := item["call_id"].(string)
			arguments, _ := item["arguments"].(string)
			toolNames[callID] = name
			messages = appendAssistant(messages, engine.Message{
				Role: engine.RoleAssistant,
				ToolCalls: []engine.ToolCall{{
					ID:        callID,
					Name:      name,
					Arguments: arguments,
				}},
			})
		case "function_call_output":
			callID, _ := item["call_id"].(string)
			name, pending := toolNames[callID]
			if callID == "" || !pending {
				return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_tool_result",
					fmt.Sprintf("No tool call found for function call output with call_id %s.", callID)).WithParam(fmt.Sprintf("input[%d].call_id", index))
			}
			delete(toolNames, callID)
			output, err := parseResponsesContent(item["output"])
			if err != nil {
				return nil, nil, err
			}
			messages = append(messages, engine.Message{
				Role:       engine.RoleTool,
				Content:    output,
				ToolCallID: callID,
				ToolName:   name,
				ToolResult: true,
			})
		case "reasoning":
			text := reasoningSummaryText(item["summary"])
			encrypted, _ := item["encrypted_content"].(string)
			if encrypted != "" && sealer != nil && cfg.Encryption.ValidateReplayEnabled() && cfg.Providers.OpenAI.ValidateEncryptedEnabled() {
				if _, err := sealer.Open("openai.reasoning", encrypted); err != nil {
					return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_encrypted_content",
						fmt.Sprintf("The encrypted content at input[%d] could not be verified.", index)).WithParam(fmt.Sprintf("input[%d].encrypted_content", index))
				}
			}
			if text != "" {
				if len(messages) > 0 && messages[len(messages)-1].Role == engine.RoleAssistant {
					messages[len(messages)-1].Reasoning = append(messages[len(messages)-1].Reasoning, engine.ReasoningBlock{Text: text})
				} else {
					messages = append(messages, engine.Message{
						Role:      engine.RoleAssistant,
						Reasoning: []engine.ReasoningBlock{{Text: text}},
					})
				}
			}
		case "item_reference":
			return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "unsupported_input_item",
				"item_reference input items are not supported by FakeAgents; send the full history instead.").WithParam(fmt.Sprintf("input[%d]", index))
		case "web_search_call", "file_search_call", "code_interpreter_call", "computer_call", "image_generation_call", "mcp_call", "mcp_approval_request":
			// Built-in tool activity is accepted and ignored.
			continue
		default:
			return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "unknown_input_item",
				fmt.Sprintf("Unknown input item type %q at input[%d].", typ, index)).WithParam(fmt.Sprintf("input[%d].type", index))
		}
	}
	if len(messages) == 0 {
		return nil, nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_input",
			"input must contain at least one message or item.")
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

func parseResponsesContent(value any) (string, *apierr.Error) {
	switch content := value.(type) {
	case nil:
		return "", nil
	case string:
		return content, nil
	case []any:
		var builder strings.Builder
		for _, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := part["type"].(string)
			switch typ {
			case "input_text", "output_text", "text":
				text, _ := part["text"].(string)
				builder.WriteString(text)
			case "refusal":
				text, _ := part["refusal"].(string)
				builder.WriteString(text)
			case "input_image", "image_url":
				builder.WriteString("[image]")
			case "input_file", "file":
				builder.WriteString("[file]")
			case "input_audio", "audio":
				builder.WriteString("[audio]")
			default:
				return "", apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_content",
					fmt.Sprintf("Unsupported input content part type %q.", typ))
			}
		}
		return builder.String(), nil
	default:
		return "", apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_content",
			"Content must be a string or an array of content parts.")
	}
}

func reasoningSummaryText(value any) string {
	parts, ok := value.([]any)
	if !ok {
		return ""
	}
	var builder strings.Builder
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		text, _ := part["text"].(string)
		builder.WriteString(text)
	}
	return builder.String()
}

func parseResponsesToolChoice(raw json.RawMessage) (*engine.ToolChoice, *apierr.Error) {
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
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_tool_choice",
			"tool_choice must be a string or an object.").WithParam("tool_choice")
	}
	switch strings.ToLower(object.Type) {
	case "auto", "none", "required":
		return &engine.ToolChoice{Mode: strings.ToLower(object.Type)}, nil
	case "function":
		if object.Name == "" {
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_tool_choice",
				"tool_choice.name is required.").WithParam("tool_choice.name")
		}
		return &engine.ToolChoice{Mode: "function", Name: object.Name}, nil
	default:
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_tool_choice",
			fmt.Sprintf("Invalid tool_choice type %q.", object.Type)).WithParam("tool_choice.type")
	}
}

func parseResponsesFormat(text *ResponsesText) (*engine.ResponseFormat, *apierr.Error) {
	if text == nil || text.Format == nil {
		return nil, nil
	}
	switch strings.ToLower(text.Format.Type) {
	case "", "text":
		return nil, nil
	case "json_object":
		return &engine.ResponseFormat{Type: "json_object"}, nil
	case "json_schema":
		if text.Format.Schema == nil {
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
				"text.format.schema is required for json_schema.").WithParam("text.format.schema")
		}
		return &engine.ResponseFormat{Type: "json_schema", Schema: text.Format.Schema, Name: text.Format.Name}, nil
	default:
		return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
			fmt.Sprintf("Invalid text.format.type %q.", text.Format.Type)).WithParam("text.format.type")
	}
}

func containsString(values []string, target string) bool {
	return slices.Contains(values, target)
}

// Retrieve handles GET /v1/responses/{id}.
func (h *Handler) Retrieve(w http.ResponseWriter, r *http.Request) {
	if h.responses == nil {
		h.fail(w, r, h.options(r), responseNotFound(r.PathValue("id")))
		return
	}
	record, ok := h.responses.Get(r.PathValue("id"))
	if !ok {
		h.fail(w, r, h.options(r), responseNotFound(r.PathValue("id")))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(record.Response)
}

// Delete handles DELETE /v1/responses/{id}.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if h.responses == nil || !h.responses.Delete(id) {
		h.fail(w, r, h.options(r), responseNotFound(id))
		return
	}
	_ = httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id":      id,
		"object":  "response.deleted",
		"deleted": true,
	})
}

func responseNotFound(id string) *apierr.Error {
	return apierr.New(http.StatusNotFound, "invalid_request_error", "not_found",
		fmt.Sprintf("Response with id %q was not found.", id))
}

// responseItem is a provider-neutral output item ready for encoding.
type responseItem struct {
	Type      string
	ID        string
	CallID    string
	Name      string
	Text      string
	Arguments string
	Encrypted string
	Refusal   bool
}

func buildResponseItems(ids interface{ New(string) string }, plan *engine.Plan, includeEncrypted bool) []responseItem {
	var items []responseItem
	for _, block := range plan.Reasoning {
		if block.Redacted {
			continue
		}
		if block.Text == "" && (!includeEncrypted || block.Encrypted == "") {
			continue
		}
		encrypted := ""
		if includeEncrypted {
			encrypted = block.Encrypted
		}
		items = append(items, responseItem{
			Type:      "reasoning",
			ID:        block.ID,
			Text:      block.Text,
			Encrypted: encrypted,
		})
	}
	if plan.Refusal != "" {
		items = append(items, responseItem{
			Type:    "message",
			ID:      ids.New("msg_"),
			Text:    plan.Refusal,
			Refusal: true,
		})
	} else if plan.Text != "" {
		items = append(items, responseItem{
			Type: "message",
			ID:   ids.New("msg_"),
			Text: plan.Text,
		})
	}
	for _, call := range plan.ToolCalls {
		items = append(items, responseItem{
			Type:      "function_call",
			ID:        ids.New("fc_"),
			CallID:    call.ID,
			Name:      call.Name,
			Arguments: call.Arguments,
		})
	}
	return items
}

func fullResponseItem(item responseItem) map[string]any {
	switch item.Type {
	case "reasoning":
		summary := []any{}
		if item.Text != "" {
			summary = append(summary, map[string]any{"type": "summary_text", "text": item.Text})
		}
		out := map[string]any{
			"type":    "reasoning",
			"id":      item.ID,
			"summary": summary,
			"status":  "completed",
		}
		if item.Encrypted != "" {
			out["encrypted_content"] = item.Encrypted
		}
		return out
	case "message":
		var part map[string]any
		if item.Refusal {
			part = map[string]any{"type": "refusal", "refusal": item.Text}
		} else {
			part = map[string]any{"type": "output_text", "text": item.Text, "annotations": []any{}, "logprobs": []any{}}
		}
		return map[string]any{
			"type":    "message",
			"id":      item.ID,
			"status":  "completed",
			"role":    "assistant",
			"content": []any{part},
		}
	default:
		return map[string]any{
			"type":      "function_call",
			"id":        item.ID,
			"call_id":   item.CallID,
			"name":      item.Name,
			"arguments": item.Arguments,
			"status":    "completed",
		}
	}
}

func emptyResponseItem(item responseItem) map[string]any {
	switch item.Type {
	case "reasoning":
		return map[string]any{
			"type":    "reasoning",
			"id":      item.ID,
			"summary": []any{},
			"status":  "in_progress",
		}
	case "message":
		return map[string]any{
			"type":    "message",
			"id":      item.ID,
			"status":  "in_progress",
			"role":    "assistant",
			"content": []any{},
		}
	default:
		return map[string]any{
			"type":      "function_call",
			"id":        item.ID,
			"call_id":   item.CallID,
			"name":      item.Name,
			"arguments": "",
			"status":    "in_progress",
		}
	}
}

func buildResponsesPayload(req *engine.Request, plan *engine.Plan, items []responseItem, raw map[string]any, storeEnabled bool) map[string]any {
	output := make([]any, 0, len(items))
	for _, item := range items {
		output = append(output, fullResponseItem(item))
	}
	payload := map[string]any{
		"id":                   plan.ID,
		"object":               "response",
		"created_at":           plan.CreatedAt.Unix(),
		"status":               plan.Status,
		"error":                nil,
		"incomplete_details":   nil,
		"model":                plan.Model,
		"output":               output,
		"parallel_tool_calls":  rawBool(raw, "parallel_tool_calls", true),
		"previous_response_id": nilIfEmpty(req.PreviousResponseID),
		"reasoning":            raw["reasoning"],
		"store":                storeEnabled,
		"text":                 raw["text"],
		"tool_choice":          rawOrDefault(raw, "tool_choice", "auto"),
		"tools":                rawOrDefault(raw, "tools", []any{}),
		"truncation":           rawOrDefault(raw, "truncation", "disabled"),
		"usage":                buildResponsesUsage(plan.Usage),
		"metadata":             rawOrDefault(raw, "metadata", map[string]any{}),
		"service_tier":         "default",
	}
	if payload["text"] == nil {
		payload["text"] = map[string]any{"format": map[string]any{"type": "text"}}
	}
	if req.ResponseFormat != nil {
		switch req.ResponseFormat.Type {
		case "json_object":
			payload["text"] = map[string]any{"format": map[string]any{"type": "json_object"}}
		case "json_schema":
			format := map[string]any{"type": "json_schema", "schema": req.ResponseFormat.Schema}
			if req.ResponseFormat.Name != "" {
				format["name"] = req.ResponseFormat.Name
			}
			payload["text"] = map[string]any{"format": format}
		}
	}
	if plan.Status == engine.StatusIncomplete {
		payload["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if req.System != "" {
		payload["instructions"] = req.System
	} else {
		payload["instructions"] = nil
	}
	if req.MaxOutputTokens > 0 {
		payload["max_output_tokens"] = req.MaxOutputTokens
	} else {
		payload["max_output_tokens"] = nil
	}
	if value, ok := raw["temperature"]; ok {
		payload["temperature"] = value
	} else {
		payload["temperature"] = 1.0
	}
	if value, ok := raw["top_p"]; ok {
		payload["top_p"] = value
	} else {
		payload["top_p"] = 1.0
	}
	if user, ok := raw["user"]; ok {
		payload["user"] = user
	} else {
		payload["user"] = nil
	}
	return payload
}

func buildResponsesUsage(usage engine.Usage) map[string]any {
	input := usage.InputTokens + usage.CachedTokens + usage.CacheWriteTokens
	return map[string]any{
		"input_tokens": input,
		"input_tokens_details": map[string]any{
			"cached_tokens": usage.CachedTokens,
		},
		"output_tokens": usage.OutputTokens,
		"output_tokens_details": map[string]any{
			"reasoning_tokens": usage.ReasoningTokens,
		},
		"total_tokens": input + usage.OutputTokens,
	}
}

func rawOrDefault(raw map[string]any, key string, fallback any) any {
	if value, ok := raw[key]; ok && value != nil {
		return value
	}
	return fallback
}

func rawBool(raw map[string]any, key string, fallback bool) bool {
	if value, ok := raw[key].(bool); ok {
		return value
	}
	return fallback
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
