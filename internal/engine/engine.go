// Package engine turns a normalised request into a deterministic simulation
// plan. It owns scenario resolution, template rendering, tool-call generation,
// token accounting, and timing. Provider adapters only translate wire formats.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/jahrulnr/fakeagents/internal/apierr"
	"github.com/jahrulnr/fakeagents/internal/clock"
	"github.com/jahrulnr/fakeagents/internal/config"
	"github.com/jahrulnr/fakeagents/internal/envelope"
	"github.com/jahrulnr/fakeagents/internal/id"
	"github.com/jahrulnr/fakeagents/internal/jsonschema"
	"github.com/jahrulnr/fakeagents/internal/scenario"
	"github.com/jahrulnr/fakeagents/internal/session"
	"github.com/jahrulnr/fakeagents/internal/template"
	"github.com/jahrulnr/fakeagents/internal/token"
)

var directivePattern = regexp.MustCompile(`(?i)\[\[fakeagents:([a-z0-9][a-z0-9._-]*)\]\]`)

// Deps are the explicit dependencies of the simulation engine.
type Deps struct {
	Config    *config.Config
	Clock     clock.Clock
	IDs       id.Generator
	Tokens    token.Counter
	Templates template.Renderer
	Scenarios *scenario.Set
	Sessions  session.Tracker
	Sealer    envelope.Sealer
	Logger    *slog.Logger
}

// Engine is the default simulator.
type Engine struct {
	cfg       *config.Config
	clk       clock.Clock
	ids       id.Generator
	tokens    token.Counter
	templates template.Renderer
	scenarios *scenario.Set
	sessions  session.Tracker
	sealer    envelope.Sealer
	logger    *slog.Logger

	scriptMu sync.Mutex
	scripts  map[string]int
}

// New builds an engine, filling safe defaults for optional dependencies.
func New(deps Deps) *Engine {
	cfg := deps.Config
	if cfg == nil {
		cfg = config.Default()
	}
	clk := deps.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	ids := deps.IDs
	if ids == nil {
		ids = id.New()
	}
	tokens := deps.Tokens
	if tokens == nil {
		tokens = token.New()
	}
	templates := deps.Templates
	if templates == nil {
		engine, err := template.New(nil, template.Builtins())
		if err != nil {
			panic("fakeagents: built-in templates are invalid: " + err.Error())
		}
		templates = engine
	}
	scenarios := deps.Scenarios
	if scenarios == nil {
		compiled, err := scenario.Compile(nil)
		if err != nil {
			panic("fakeagents: empty scenario set failed to compile: " + err.Error())
		}
		scenarios = compiled
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{
		cfg:       cfg,
		clk:       clk,
		ids:       ids,
		tokens:    tokens,
		templates: templates,
		scenarios: scenarios,
		sessions:  deps.Sessions,
		sealer:    deps.Sealer,
		logger:    logger,
		scripts:   make(map[string]int),
	}
}

// Config exposes the immutable configuration used by the engine.
func (e *Engine) Config() *config.Config { return e.cfg }

// Generate produces a plan or a provider-native error.
func (e *Engine) Generate(ctx context.Context, req *Request) (*Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	requestedMax := req.MaxOutputTokens
	e.prepare(req)
	if e.cfg.Defaults.StrictModels {
		if model := e.findModel(req.Provider, req.Model); model != nil {
			if apiErr := e.applyModelLimits(req, model, requestedMax); apiErr != nil {
				return nil, apiErr
			}
		}
	}

	compiled, scenarioName, scenarioErr := e.resolveScenario(req)
	if scenarioErr != nil {
		return nil, scenarioErr
	}
	compiled = e.applyStep(req, compiled)
	seed := e.seed(req, scenarioName)
	rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)^0x9e3779b97f4a7c15))

	plan := &Plan{
		ID:        e.planID(req),
		Model:     req.Model,
		Provider:  req.Provider,
		Endpoint:  req.Endpoint,
		CreatedAt: e.clk.Now(),
		Status:    StatusCompleted,
		Meta: Meta{
			Scenario:  scenarioName,
			Session:   req.SessionKey,
			Turn:      req.Turn,
			Step:      req.Step,
			Seed:      seed,
			RequestID: req.RequestID,
		},
	}

	if compiled != nil && compiled.Type == scenario.TypeError {
		apiErr := buildScenarioError(compiled.Spec.Respond.Error, req.Provider)
		for key, value := range compiled.Spec.Respond.Headers {
			if apiErr.Headers == nil {
				apiErr.Headers = map[string]string{}
			}
			apiErr.Headers[key] = value
		}
		return nil, apiErr
	}

	data := e.dataFor(req, plan)
	if compiled != nil && compiled.Type == scenario.TypeRaw {
		if req.Stream {
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "unsupported_parameter",
				"Raw scenario responses cannot be streamed.").WithParam("stream")
		}
		rendered, err := e.templates.RenderSource(compiled.Spec.Respond.RawJSON, data)
		if err != nil {
			return nil, internalError("render raw scenario response: " + err.Error())
		}
		status := compiled.Spec.Respond.Status
		if status == 0 {
			status = http.StatusOK
		}
		plan.RawJSON = []byte(rendered)
		plan.RawStatus = status
		plan.Headers = cloneHeaders(compiled.Spec.Respond.Headers)
		return plan, nil
	}

	thinking := e.wantThinking(req, compiled)
	plan.Meta.Thinking = thinking

	var reasoningText string
	if thinking {
		block, full, err := e.buildReasoning(req, compiled, data)
		if err != nil {
			return nil, err
		}
		plan.Reasoning = []ReasoningBlock{block}
		reasoningText = full
	}

	toolCalls, toolErr := e.buildToolCalls(req, compiled, data, rng)
	if toolErr != nil {
		return nil, toolErr
	}
	if len(toolCalls) > 0 && !e.cfg.Defaults.Tooling.IncludeReasoningEnabled() {
		plan.Reasoning = nil
		reasoningText = ""
		plan.Meta.Thinking = false
	}
	plan.ToolCalls = toolCalls
	plan.Meta.ToolCalls = len(toolCalls)

	text, refusal, _, textErr := e.buildText(req, compiled, data, rng, len(toolCalls) > 0)
	if textErr != nil {
		return nil, textErr
	}
	plan.Text = text
	plan.Refusal = refusal

	finish, stopSequence := resolveFinish(compiled, len(toolCalls) > 0, refusal != "")
	explicitFinish := compiled != nil && strings.TrimSpace(compiled.Spec.Respond.Finish) != ""
	if finish == FinishStop && !explicitFinish && stopSequence == "" {
		stopAt := len(text)
		for _, sequence := range req.StopSequences {
			if sequence != "" {
				if index := strings.Index(text, sequence); index >= 0 && index < stopAt {
					stopAt, stopSequence = index, sequence
				}
			}
		}
		text = text[:stopAt]
		plan.Text = text
	}
	hiddenReasoning := len(plan.Reasoning) > 0 && (plan.Reasoning[0].Redacted || plan.Reasoning[0].Text == "")

	reasoningTokens := e.tokens.Count(reasoningText)
	textTokens := e.tokens.Count(text) + e.tokens.Count(refusal)
	callTokens := countToolCalls(e.tokens, toolCalls)

	if e.cfg.Defaults.RespectMaxTokensEnabled() && !explicitFinish && len(toolCalls) == 0 && req.MaxOutputTokens > 0 {
		budget := req.MaxOutputTokens
		if reasoningTokens+textTokens > budget {
			remaining := budget
			allowedReasoning := min(reasoningTokens, remaining)
			if hiddenReasoning {
				reasoningTokens = allowedReasoning
			} else {
				reasoningText = e.tokens.Truncate(reasoningText, allowedReasoning)
				if len(plan.Reasoning) > 0 {
					plan.Reasoning[0].Text = reasoningText
				}
				reasoningTokens = e.tokens.Count(reasoningText)
			}
			remaining -= allowedReasoning
			if remaining < 0 {
				remaining = 0
			}
			if remaining == 0 {
				plan.Text = ""
				plan.Refusal = ""
			} else if plan.Text != "" {
				plan.Text = e.tokens.Truncate(plan.Text, remaining)
			}
			textTokens = e.tokens.Count(plan.Text) + e.tokens.Count(plan.Refusal)
			finish = FinishLength
			stopSequence = ""
		}
	}

	for i := range plan.Reasoning {
		if err := e.finalizeReasoning(req, plan, &plan.Reasoning[i], reasoningText); err != nil {
			return nil, err
		}
	}

	plan.Finish = finish
	plan.StopSequence = stopSequence
	if finish == FinishLength {
		plan.Status = StatusIncomplete
		plan.Incomplete = "max_output_tokens"
	}

	plan.Usage = e.computeUsage(req, compiled, reasoningTokens, textTokens, callTokens)
	plan.Latency = e.buildTiming(req, compiled, plan.Usage.OutputTokens, rng)
	plan.Stream = e.buildStreamPlan(req, compiled)
	plan.Headers = cloneHeaders(scenarioHeaders(compiled))
	return plan, nil
}

func (e *Engine) prepare(req *Request) {
	if req.Provider == "" {
		req.Provider = e.cfg.Defaults.Provider
		if req.Provider == "" {
			req.Provider = apierr.ProviderOpenAI
		}
	}
	if req.Model == "" {
		req.Model = e.cfg.Defaults.Model
	}
	if req.MaxOutputTokens <= 0 {
		req.MaxOutputTokens = e.cfg.Defaults.MaxOutputTokens
	}
	if req.Headers == nil {
		req.Headers = http.Header{}
	}
	if !req.TurnSet {
		if e.sessions != nil && req.SessionKey != "" {
			req.Turn = e.sessions.Next(req.SessionKey)
		} else {
			req.Turn = 0
		}
	}
	req.Normalize()
}

func (e *Engine) planID(req *Request) string {
	switch req.Endpoint {
	case scenario.EndpointChat:
		return e.ids.New("chatcmpl-")
	case scenario.EndpointResponses:
		return e.ids.New("resp_")
	case scenario.EndpointMessages, scenario.EndpointCount:
		return e.ids.New("msg_")
	default:
		return e.ids.New("fake-")
	}
}

// applyStep activates a scripted turn. Selection is stateless: the number of
// assistant tool-call turns already present in the conversation determines the
// next step. X-FakeAgents-Step overrides it for deterministic tests.
func (e *Engine) applyStep(req *Request, compiled *scenario.Compiled) *scenario.Compiled {
	step := priorToolSteps(req)
	forced, hasForced := headerStep(req.Headers)
	if hasForced {
		step = forced
		req.StepSet = true
	}
	if compiled == nil {
		req.Step = max(step, 0)
		return nil
	}
	steps := compiled.Spec.Respond.Steps
	if len(steps) == 0 {
		req.Step = max(step, 0)
		return compiled
	}
	if !hasForced {
		switch strings.ToLower(strings.TrimSpace(compiled.Spec.Respond.StepsMode)) {
		case "request":
			step = req.Turn
		case "sequence":
			step = e.nextScriptStep(req.SessionKey, compiled.Name())
		}
	}
	if step < 0 {
		step = 0
	}
	req.Step = step
	index := step
	if index >= len(steps) {
		index = len(steps) - 1
	}
	return compiled.WithRespond(mergeStepRespond(compiled.Spec.Respond, steps[index]))
}

func (e *Engine) nextScriptStep(sessionKey, scenarioName string) int {
	if sessionKey == "" {
		sessionKey = "anonymous"
	}
	key := sessionKey + "\x00" + scenarioName
	e.scriptMu.Lock()
	defer e.scriptMu.Unlock()
	if e.scripts == nil {
		e.scripts = make(map[string]int)
	}
	step := e.scripts[key]
	e.scripts[key] = step + 1
	return step
}

// ResetScripts clears stateful steps_mode=sequence cursors.
func (e *Engine) ResetScripts() {
	e.scriptMu.Lock()
	defer e.scriptMu.Unlock()
	e.scripts = make(map[string]int)
}

func priorToolSteps(req *Request) int {
	start := 0
	for i, message := range req.Messages {
		if message.Role != RoleUser {
			continue
		}
		// Text accompanying a tool_result (Anthropic) or function_call_output
		// (Responses) belongs to the same agent turn and must not restart the
		// script.
		if i+1 < len(req.Messages) && req.Messages[i+1].Role == RoleTool {
			continue
		}
		start = i + 1
	}
	count := 0
	for _, message := range req.Messages[start:] {
		if message.Role == RoleAssistant && len(message.ToolCalls) > 0 {
			count++
		}
	}
	if count == 0 {
		for _, message := range req.Messages[start:] {
			if message.Role == RoleTool && message.ToolResult {
				return 1
			}
		}
	}
	return count
}

func headerStep(headers http.Header) (int, bool) {
	raw := strings.TrimSpace(headers.Get("X-FakeAgents-Step"))
	if raw == "" {
		return 0, false
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, false
	}
	return value, true
}

func mergeStepRespond(outer, step scenario.Respond) scenario.Respond {
	merged := step
	if merged.Thinking == nil {
		merged.Thinking = outer.Thinking
	}
	if merged.Reasoning == "" {
		merged.Reasoning = outer.Reasoning
	}
	if merged.ReasoningTemplate == "" {
		merged.ReasoningTemplate = outer.ReasoningTemplate
	}
	if merged.RedactedReasoning == nil {
		merged.RedactedReasoning = outer.RedactedReasoning
	}
	if merged.Finish == "" {
		merged.Finish = outer.Finish
	}
	if merged.StopSequence == "" {
		merged.StopSequence = outer.StopSequence
	}
	if merged.Usage == nil {
		merged.Usage = outer.Usage
	}
	if merged.Latency == nil {
		merged.Latency = outer.Latency
	}
	if merged.Stream == nil {
		merged.Stream = outer.Stream
	}
	if merged.Agentic == nil {
		merged.Agentic = outer.Agentic
	}
	if len(merged.ToolPlan) == 0 {
		merged.ToolPlan = outer.ToolPlan
	}
	if merged.UnknownTool == "" {
		merged.UnknownTool = outer.UnknownTool
	}
	if merged.StepsMode == "" {
		merged.StepsMode = outer.StepsMode
	}
	merged.Headers = mergeHeaderMaps(outer.Headers, step.Headers)
	return merged
}

func mergeHeaderMaps(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	merged := make(map[string]string, len(base)+len(override))
	maps.Copy(merged, base)
	maps.Copy(merged, override)
	return merged
}

func (e *Engine) resolveScenario(req *Request) (*scenario.Compiled, string, *apierr.Error) {
	if name := strings.TrimSpace(req.Headers.Get("X-FakeAgents-Scenario")); name != "" {
		compiled, ok := e.scenarios.Lookup(name)
		if !ok {
			return nil, name, e.scenarioNotFound(name)
		}
		e.stripDirectives(req)
		return compiled, compiled.Name(), nil
	}

	if name := directiveName(req.LastUserMessage); name != "" {
		compiled, ok := e.scenarios.Lookup(name)
		if !ok {
			return nil, name, e.scenarioNotFound(name)
		}
		e.stripDirectives(req)
		return compiled, compiled.Name(), nil
	}

	if name := strings.TrimSpace(e.cfg.Defaults.Scenario); name != "" {
		compiled, ok := e.scenarios.Lookup(name)
		if !ok {
			return nil, name, e.scenarioNotFound(name)
		}
		e.stripDirectives(req)
		return compiled, compiled.Name(), nil
	}

	ctx := &scenario.Context{
		Provider:          req.Provider,
		Endpoint:          req.Endpoint,
		Model:             req.Model,
		Stream:            req.Stream,
		ThinkingRequested: req.Reasoning.Requested,
		HasTools:          len(req.Tools) > 0,
		HasToolResults:    req.HasToolResults,
		LastRole:          string(req.LastRole),
		LastUserMessage:   req.LastUserMessage,
		System:            req.System,
		Headers:           req.Headers,
		Raw:               req.Raw,
		Turn:              req.Turn,
		Session:           req.SessionKey,
		Random:            rand.New(rand.NewPCG(uint64(e.seed(req, "")), 1)),
	}
	if compiled, ok := e.scenarios.Match(ctx); ok {
		return compiled, compiled.Name(), nil
	}
	return nil, "default", nil
}

func (e *Engine) scenarioNotFound(name string) *apierr.Error {
	names := e.scenarios.Names()
	available := strings.Join(names, ", ")
	if available == "" {
		available = "(none configured)"
	}
	message := fmt.Sprintf("Unknown scenario %q. Available scenarios: %s", name, available)
	return apierr.New(http.StatusBadRequest, "invalid_request_error", "scenario_not_found", message)
}

func (e *Engine) stripDirectives(req *Request) {
	req.LastUserMessage = strings.TrimSpace(directivePattern.ReplaceAllString(req.LastUserMessage, ""))
	for i := range req.Messages {
		if req.Messages[i].Content == "" {
			continue
		}
		cleaned := directivePattern.ReplaceAllString(req.Messages[i].Content, "")
		req.Messages[i].Content = strings.TrimSpace(cleaned)
	}
	req.Normalize()
}

func directiveName(text string) string {
	matches := directivePattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

func (e *Engine) wantThinking(req *Request, compiled *scenario.Compiled) bool {
	on := req.Reasoning.Requested
	switch strings.ToLower(e.cfg.Defaults.Thinking.Mode) {
	case "always":
		on = true
	case "never":
		on = false
	}
	if compiled != nil {
		respond := compiled.Spec.Respond
		if respond.Reasoning != "" || respond.ReasoningTemplate != "" {
			on = true
		}
		if respond.Thinking != nil {
			on = *respond.Thinking
		}
		if compiled.Type == scenario.TypeReasoning {
			on = true
		}
	}
	return on
}

func (e *Engine) buildReasoning(req *Request, compiled *scenario.Compiled, data *template.Data) (ReasoningBlock, string, error) {
	block := ReasoningBlock{ID: e.ids.New("rs_")}
	var source string
	rendered := false
	switch {
	case compiled != nil && compiled.Spec.Respond.Reasoning != "":
		source = compiled.Spec.Respond.Reasoning
	case compiled != nil && compiled.Spec.Respond.ReasoningTemplate != "":
		result, err := e.templates.Render(compiled.Spec.Respond.ReasoningTemplate, data)
		if err != nil {
			return block, "", internalError("render reasoning template: " + err.Error())
		}
		source = result
		rendered = true
	case compiled != nil && compiled.Type == scenario.TypeReasoning && compiled.Spec.Respond.Text != "":
		source = compiled.Spec.Respond.Text
	default:
		name := e.cfg.Defaults.Thinking.Template
		if name == "" {
			name = "reasoning"
		}
		result, err := e.templates.Render(name, data)
		if err != nil {
			return block, "", internalError("render reasoning: " + err.Error())
		}
		source = result
		rendered = true
	}
	if !rendered {
		result, err := e.templates.RenderSource(source, data)
		if err != nil {
			return block, "", internalError("render reasoning text: " + err.Error())
		}
		source = result
	}
	if strings.TrimSpace(source) == "" {
		source = "Simulated reasoning for the current request."
	}
	redacted := e.cfg.Defaults.Thinking.Redacted
	if compiled != nil && compiled.Spec.Respond.RedactedReasoning != nil {
		redacted = *compiled.Spec.Respond.RedactedReasoning
	}
	block.Redacted = redacted
	block.Text = source
	if req.Provider == apierr.ProviderAnthropic && strings.EqualFold(req.Reasoning.Display, "omitted") {
		block.Text = ""
	}
	return block, source, nil
}

func (e *Engine) finalizeReasoning(req *Request, plan *Plan, block *ReasoningBlock, fullText string) error {
	if e.sealer == nil {
		return nil
	}
	base := map[string]any{
		"v":           1,
		"provider":    req.Provider,
		"model":       req.Model,
		"response_id": plan.ID,
		"text":        fullText,
		"created_at":  plan.CreatedAt.Unix(),
	}
	if block.Redacted {
		payload := clonePayload(base)
		payload["kind"] = "redacted_thinking"
		data, err := e.sealer.Seal("anthropic.redacted", payload)
		if err != nil {
			return internalError("seal redacted reasoning: " + err.Error())
		}
		block.RedactedData = data
		block.Text = ""
		block.Signature = ""
		block.Encrypted = ""
		return nil
	}
	thinkingPayload := clonePayload(base)
	thinkingPayload["kind"] = "thinking"
	thinkingPayload["display_text"] = block.Text
	signature, err := e.sealer.Seal("anthropic.thinking", thinkingPayload)
	if err != nil {
		return internalError("sign reasoning: " + err.Error())
	}
	block.Signature = signature

	encryptedPayload := clonePayload(base)
	encryptedPayload["kind"] = "reasoning"
	encryptedPayload["summary"] = block.Text
	encrypted, err := e.sealer.Seal("openai.reasoning", encryptedPayload)
	if err != nil {
		return internalError("encrypt reasoning: " + err.Error())
	}
	block.Encrypted = encrypted
	return nil
}

func clonePayload(input map[string]any) map[string]any {
	out := make(map[string]any, len(input)+2)
	maps.Copy(out, input)
	return out
}

func (e *Engine) buildToolCalls(req *Request, compiled *scenario.Compiled, data *template.Data, rng *rand.Rand) ([]ToolCall, *apierr.Error) {
	prefix := toolCallPrefix(req)
	respond := scenario.Respond{}
	if compiled != nil {
		respond = compiled.Spec.Respond
	}

	if len(respond.ToolCalls) > 0 {
		specs := respond.ToolCalls
		limit := e.cfg.Defaults.Tooling.MaxToolCalls
		if limit <= 0 {
			limit = 8
		}
		if len(specs) > limit {
			specs = specs[:limit]
		}
		calls := make([]ToolCall, 0, len(specs))
		for _, spec := range specs {
			name, tool := resolveToolSpec(req, spec)
			callSpec := spec
			callSpec.Name = name
			arguments, err := e.argumentsFor(req, tool, &callSpec, data, rng)
			if err != nil {
				return nil, err
			}
			callID := spec.ID
			if callID == "" {
				callID = e.ids.New(prefix)
			}
			calls = append(calls, ToolCall{ID: callID, Name: name, Arguments: arguments})
		}
		return calls, nil
	}

	if req.ToolChoice != nil && req.ToolChoice.Mode == "none" {
		return nil, nil
	}

	mode := strings.ToLower(e.cfg.Defaults.Tooling.Mode)
	agentic := mode == "agent" || (respond.Agentic != nil && *respond.Agentic)
	if agentic && (req.ToolChoice == nil || req.ToolChoice.Mode != "function") {
		return e.agentToolCalls(req, respond, data, rng)
	}

	if len(req.Tools) == 0 {
		if req.ToolChoice != nil && req.ToolChoice.Mode == "required" {
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_tool_choice",
				"tool_choice is required, but no tools were provided.")
		}
		return nil, nil
	}
	if req.HasToolResults && e.cfg.Defaults.Tooling.FinalEnabled() {
		return nil, nil
	}

	forceScenarioTool := compiled != nil && compiled.Type == scenario.TypeToolCall
	if mode == "never" && !forceScenarioTool {
		return nil, nil
	}

	var selected *Tool
	switch {
	case req.ToolChoice != nil && req.ToolChoice.Mode == "function":
		selected = findTool(req.Tools, req.ToolChoice.Name)
		if selected == nil {
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_tool_choice",
				fmt.Sprintf("Invalid tool_choice: tool %q was not provided in the request.", req.ToolChoice.Name))
		}
	case req.ToolChoice != nil && req.ToolChoice.Mode == "required":
		selected = &req.Tools[0]
	case mode == "always" || forceScenarioTool:
		selected = &req.Tools[0]
	default:
		return nil, nil
	}

	arguments, err := e.argumentsFor(req, selected, nil, data, rng)
	if err != nil {
		return nil, err
	}
	return []ToolCall{{
		ID:        e.ids.New(prefix),
		Name:      selected.Name,
		Arguments: arguments,
	}}, nil
}

// agentToolCalls advances a harness-driven tool plan by one step. The step is
// derived from prior assistant tool-call turns, so the loop is stateless.
func (e *Engine) agentToolCalls(req *Request, respond scenario.Respond, data *template.Data, rng *rand.Rand) ([]ToolCall, *apierr.Error) {
	plan := respond.ToolPlan
	if len(plan) == 0 {
		plan = e.cfg.Defaults.Tooling.Plan
	}
	if len(plan) == 0 {
		for _, tool := range req.Tools {
			plan = append(plan, tool.Name)
		}
	}
	if len(plan) == 0 {
		if req.ToolChoice != nil && req.ToolChoice.Mode == "required" {
			return nil, apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_tool_choice",
				"tool_choice is required, but no tools or tool plan were provided.")
		}
		return nil, nil
	}
	step := req.Step
	if step < 0 {
		step = 0
	}
	if step >= len(plan) {
		return nil, nil
	}
	name := strings.TrimSpace(plan[step])
	tool := findTool(req.Tools, name)

	behavior := strings.ToLower(strings.TrimSpace(respond.UnknownTool))
	if behavior == "" {
		behavior = strings.ToLower(strings.TrimSpace(e.cfg.Defaults.Tooling.UnknownTool))
	}
	if behavior == "" {
		behavior = "allow"
	}
	if tool == nil {
		switch behavior {
		case "skip":
			for index := step + 1; index < len(plan); index++ {
				candidate := strings.TrimSpace(plan[index])
				if found := findTool(req.Tools, candidate); found != nil {
					name, tool = candidate, found
					break
				}
			}
			if tool == nil {
				return nil, nil
			}
		case "error":
			apiErr := apierr.New(http.StatusBadRequest, "invalid_request_error", "tool_not_declared",
				fmt.Sprintf("Tool %q is not declared in the request's tools array.", name))
			apiErr.Param = "tools"
			return nil, apiErr
		}
	}

	spec := &scenario.ToolCallSpec{Name: name}
	arguments, err := e.argumentsFor(req, tool, spec, data, rng)
	if err != nil {
		return nil, err
	}
	return []ToolCall{{
		ID:        e.ids.New(toolCallPrefix(req)),
		Name:      name,
		Arguments: arguments,
	}}, nil
}

func toolCallPrefix(req *Request) string {
	if req.Provider == apierr.ProviderAnthropic {
		return "toolu_"
	}
	return "call_"
}

func resolveToolSpec(req *Request, spec scenario.ToolCallSpec) (string, *Tool) {
	if strings.TrimSpace(spec.Name) != "" {
		return spec.Name, findTool(req.Tools, spec.Name)
	}
	if spec.ToolIndex != nil {
		index := *spec.ToolIndex
		if index >= 0 && index < len(req.Tools) {
			tool := &req.Tools[index]
			return tool.Name, tool
		}
		return fmt.Sprintf("fakeagents_missing_tool_%d", index), nil
	}
	return "", nil
}

func (e *Engine) argumentsFor(req *Request, tool *Tool, spec *scenario.ToolCallSpec, data *template.Data, rng *rand.Rand) (string, *apierr.Error) {
	toolName := ""
	if tool != nil {
		toolName = tool.Name
	}
	if spec != nil && spec.Name != "" {
		toolName = spec.Name
	}
	local := *data
	local.ToolName = toolName
	if tool != nil {
		local.ToolDescription = tool.Description
	}

	if spec != nil {
		if strings.TrimSpace(spec.ArgumentsJSON) != "" {
			rendered, err := e.templates.RenderSource(spec.ArgumentsJSON, &local)
			if err != nil {
				return "", internalError("render tool arguments: " + err.Error())
			}
			if strings.TrimSpace(rendered) == "" {
				return "{}", nil
			}
			return strings.TrimSpace(rendered), nil
		}
		if spec.Arguments != nil {
			b, err := json.Marshal(spec.Arguments)
			if err != nil {
				return "", internalError("encode tool arguments: " + err.Error())
			}
			return string(b), nil
		}
	}

	settings := e.cfg.Defaults.Tooling
	strategy := strings.ToLower(settings.ArgumentStrategy)
	if strategy == "" {
		strategy = "schema"
	}
	if static, ok := settings.Arguments[toolName]; ok && strategy != "template" {
		b, err := json.Marshal(static)
		if err != nil {
			return "", internalError("encode static tool arguments: " + err.Error())
		}
		return string(b), nil
	}
	if strategy == "template" {
		if strings.TrimSpace(settings.ArgumentTemplate) == "" {
			return "{}", nil
		}
		rendered, err := e.templates.RenderSource(settings.ArgumentTemplate, &local)
		if err != nil {
			return "", internalError("render tool argument template: " + err.Error())
		}
		if strings.TrimSpace(rendered) == "" {
			return "{}", nil
		}
		return strings.TrimSpace(rendered), nil
	}
	if tool == nil || tool.Schema == nil {
		return "{}", nil
	}
	sampled, err := jsonschema.SampleJSON(tool.Schema, rng)
	if err != nil {
		return "", internalError("synthesize tool arguments: " + err.Error())
	}
	if strings.TrimSpace(sampled) == "" || sampled == "null" {
		return "{}", nil
	}
	return sampled, nil
}

func (e *Engine) buildText(req *Request, compiled *scenario.Compiled, data *template.Data, rng *rand.Rand, hasToolCalls bool) (string, string, bool, *apierr.Error) {
	if compiled != nil {
		switch compiled.Type {
		case scenario.TypeEmpty:
			return "", "", true, nil
		case scenario.TypeRefusal:
			text, explicit, err := e.renderRespondText(compiled, data, rng)
			if err != nil {
				return "", "", false, err
			}
			if !explicit || text == "" {
				rendered, renderErr := e.templates.Render("refusal", data)
				if renderErr != nil {
					return "", "", false, internalError("render refusal: " + renderErr.Error())
				}
				text = rendered
			}
			return "", text, true, nil
		}
	}

	text, explicit, err := e.renderRespondText(compiled, data, rng)
	if err != nil {
		return "", "", false, err
	}
	if explicit {
		return text, "", true, nil
	}
	if hasToolCalls {
		return "", "", false, nil
	}

	if req.ResponseFormat != nil && req.ResponseFormat.Type != "" && req.ResponseFormat.Type != "text" {
		if e.cfg.Defaults.StructuredOutput.Mode != "text" {
			if schema := req.ResponseFormat.Schema; schema != nil {
				sampled, sampleErr := jsonschema.SampleJSON(schema, rng)
				if sampleErr != nil {
					return "", "", false, internalError("synthesize structured output: " + sampleErr.Error())
				}
				return sampled, "", false, nil
			}
			if req.ResponseFormat.Type == "json_object" {
				payload := map[string]any{
					"result":    req.LastUserMessage,
					"simulated": true,
					"model":     req.Model,
				}
				b, marshalErr := json.Marshal(payload)
				if marshalErr != nil {
					return "", "", false, internalError("encode json output: " + marshalErr.Error())
				}
				return string(b), "", false, nil
			}
		}
	}

	if req.HasToolResults && e.cfg.Defaults.Tooling.FinalEnabled() {
		name := e.cfg.Defaults.Tooling.FinalTemplate
		if name == "" {
			name = "final"
		}
		rendered, renderErr := e.templates.Render(name, data)
		if renderErr != nil {
			return "", "", false, internalError("render final template: " + renderErr.Error())
		}
		return rendered, "", false, nil
	}

	rendered, renderErr := e.templates.Render("default", data)
	if renderErr != nil {
		return "", "", false, internalError("render default template: " + renderErr.Error())
	}
	return rendered, "", false, nil
}

func (e *Engine) renderRespondText(compiled *scenario.Compiled, data *template.Data, rng *rand.Rand) (string, bool, *apierr.Error) {
	if compiled == nil {
		return "", false, nil
	}
	respond := compiled.Spec.Respond
	if respond.TextTemplate != "" {
		rendered, err := e.templates.Render(respond.TextTemplate, data)
		if err != nil {
			return "", false, internalError("render text template: " + err.Error())
		}
		return rendered, true, nil
	}
	if respond.Text != "" {
		rendered, err := e.templates.RenderSource(respond.Text, data)
		if err != nil {
			return "", false, internalError("render text: " + err.Error())
		}
		return rendered, true, nil
	}
	if len(respond.Variants) > 0 {
		choice := respond.Variants[rng.IntN(len(respond.Variants))]
		rendered, err := e.templates.RenderSource(choice, data)
		if err != nil {
			return "", false, internalError("render variant: " + err.Error())
		}
		return rendered, true, nil
	}
	return "", false, nil
}

func (e *Engine) dataFor(req *Request, plan *Plan) *template.Data {
	data := plan.Data(req, effortOf(req))
	data.Random = rand.New(rand.NewPCG(uint64(plan.Meta.Seed), uint64(plan.Meta.Seed)^0x2545f4914f6cdd1d))
	return data
}

func effortOf(req *Request) string {
	effort := strings.ToLower(strings.TrimSpace(req.Reasoning.Effort))
	if effort == "" {
		return "medium"
	}
	return effort
}

func resolveFinish(compiled *scenario.Compiled, hasToolCalls, hasRefusal bool) (Finish, string) {
	finish := FinishStop
	if hasToolCalls {
		finish = FinishToolCalls
	}
	if hasRefusal {
		finish = FinishRefusal
	}
	if compiled == nil {
		return finish, ""
	}
	respond := compiled.Spec.Respond
	stopSequence := respond.StopSequence
	switch strings.ToLower(strings.TrimSpace(respond.Finish)) {
	case "":
		return finish, stopSequence
	case "stop", "end_turn":
		if hasToolCalls {
			return FinishToolCalls, stopSequence
		}
		return FinishStop, stopSequence
	case "length", "max_tokens":
		return FinishLength, stopSequence
	case "tool_calls", "tool_use":
		return FinishToolCalls, stopSequence
	case "content_filter":
		return FinishContentFilter, stopSequence
	case "refusal":
		return FinishRefusal, stopSequence
	default:
		return finish, stopSequence
	}
}

func buildScenarioError(spec *scenario.ErrorSpec, provider string) *apierr.Error {
	if spec == nil {
		return apierr.PresetOrGeneric(provider, "invalid_request")
	}
	var apiErr *apierr.Error
	if spec.Preset != "" {
		apiErr = apierr.MustPreset(provider, spec.Preset)
	} else {
		apiErr = &apierr.Error{
			Status:  http.StatusBadRequest,
			Type:    "invalid_request_error",
			Message: spec.Message,
		}
	}
	if spec.Status != 0 {
		apiErr.Status = spec.Status
	}
	if spec.Type != "" {
		apiErr.Type = spec.Type
	}
	if spec.Code != "" {
		apiErr.Code = spec.Code
	}
	if spec.Param != "" {
		apiErr.Param = spec.Param
	}
	if spec.Message != "" {
		apiErr.Message = spec.Message
	}
	if spec.RetryAfter != nil {
		apiErr.RetryAfter = spec.RetryAfter.D()
	}
	if len(spec.Headers) > 0 {
		if apiErr.Headers == nil {
			apiErr.Headers = map[string]string{}
		}
		maps.Copy(apiErr.Headers, spec.Headers)
	}
	return apiErr
}

func scenarioHeaders(compiled *scenario.Compiled) map[string]string {
	if compiled == nil {
		return nil
	}
	return compiled.Spec.Respond.Headers
}

func cloneHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	maps.Copy(out, headers)
	return out
}

func findTool(tools []Tool, name string) *Tool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

func countToolCalls(counter token.Counter, calls []ToolCall) int {
	total := 0
	for _, call := range calls {
		total += 6 + counter.Count(call.Name) + counter.Count(call.Arguments)
	}
	return total
}

func (e *Engine) seed(req *Request, scenarioName string) int64 {
	h := sha256.New()
	write := func(value string) {
		h.Write([]byte(value))
		h.Write([]byte{0})
	}
	write(req.Provider)
	write(req.Endpoint)
	write(req.Model)
	write(req.SessionKey)
	write(fmt.Sprintf("%d", req.Turn))
	write(fmt.Sprintf("%d", req.Step))
	write(scenarioName)
	write(req.LastUserMessage)
	write(req.System)
	write(req.Reasoning.Effort)
	for _, tool := range req.Tools {
		write(tool.Name)
	}
	sum := h.Sum(nil)
	return int64(binary.LittleEndian.Uint64(sum[:8]))
}

func internalError(message string) *apierr.Error {
	return apierr.New(http.StatusInternalServerError, "api_error", "internal_error", message)
}

// CountInputTokens estimates prompt tokens for a request.
func (e *Engine) CountInputTokens(req *Request) int {
	parts := make([]token.MessagePart, 0, len(req.Messages)+1)
	if req.System != "" {
		parts = append(parts, token.MessagePart{Text: req.System})
	}
	for _, message := range req.Messages {
		var text strings.Builder
		text.WriteString(message.Content)
		for _, call := range message.ToolCalls {
			text.WriteString(" " + call.Name + " " + call.Arguments)
		}
		for _, reasoning := range message.Reasoning {
			text.WriteString(" " + reasoning.Text)
		}
		parts = append(parts, token.MessagePart{Text: text.String()})
	}
	total := e.tokens.CountMessages(parts)
	schemas := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		if tool.Schema == nil {
			schemas = append(schemas, "{}")
			continue
		}
		b, err := json.Marshal(tool.Schema)
		if err != nil {
			continue
		}
		schemas = append(schemas, string(b))
	}
	total += e.tokens.CountToolSchemas(schemas)
	return total
}

// ResolveModel applies strict model validation and alias resolution.
func (e *Engine) ResolveModel(provider, model string) (string, *apierr.Error) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = e.cfg.Defaults.Model
	}
	if !e.cfg.Defaults.StrictModels {
		return model, nil
	}
	if entry := e.findModel(provider, model); entry != nil {
		return entry.ID, nil
	}
	apiErr := apierr.MustPreset(provider, "model_not_found")
	apiErr.Message = fmt.Sprintf("The model %q does not exist or you do not have access to it.", model)
	apiErr.Param = "model"
	return "", apiErr
}

func (e *Engine) findModel(provider, model string) *config.Model {
	for i := range e.cfg.Models {
		entry := &e.cfg.Models[i]
		if !entry.AppliesTo(provider) {
			continue
		}
		if strings.EqualFold(entry.ID, model) {
			return entry
		}
		for _, alias := range entry.Aliases {
			if strings.EqualFold(alias, model) {
				return entry
			}
		}
	}
	return nil
}

func (e *Engine) applyModelLimits(req *Request, model *config.Model, requestedMax int) *apierr.Error {
	if model.ContextWindow > 0 {
		input := e.CountInputTokens(req)
		if input >= model.ContextWindow {
			apiErr := apierr.MustPreset(req.Provider, "context_length")
			apiErr.Message = fmt.Sprintf("Model %q has a context window of %d tokens; the estimated input uses %d tokens.", model.ID, model.ContextWindow, input)
			return apiErr
		}
		req.MaxOutputTokens = min(req.MaxOutputTokens, model.ContextWindow-input)
	}
	if model.MaxOutputTokens > 0 {
		if requestedMax > model.MaxOutputTokens {
			return apierr.New(http.StatusBadRequest, "invalid_request_error", "invalid_parameter",
				fmt.Sprintf("Model %q supports at most %d output tokens.", model.ID, model.MaxOutputTokens))
		}
		req.MaxOutputTokens = min(req.MaxOutputTokens, model.MaxOutputTokens)
	}
	if len(model.Capabilities) > 0 && (len(req.Tools) > 0 || req.ToolChoice != nil && req.ToolChoice.Mode != "none") && !model.HasCapability("tools") {
		return apierr.New(http.StatusBadRequest, "invalid_request_error", "unsupported_parameter",
			fmt.Sprintf("Model %q does not support tools.", model.ID)).WithParam("tools")
	}
	return nil
}

// Models returns catalog entries for a provider.
func (e *Engine) Models(provider string) []config.Model {
	out := make([]config.Model, 0, len(e.cfg.Models))
	for _, model := range e.cfg.Models {
		if model.AppliesTo(provider) {
			out = append(out, model)
		}
	}
	return out
}
