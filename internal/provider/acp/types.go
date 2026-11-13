// Package acp implements an Agent Client Protocol agent on top of the
// simulation engine. ACP clients (editors such as Zed) spawn the agent as a
// subprocess and exchange newline-delimited JSON-RPC 2.0 messages over
// stdio. Wire types here pin to the ACP v1 schema (agentclientprotocol.com,
// schema release v1.31.x); ACP v2 is still alpha and intentionally
// unsupported.
package acp

import "encoding/json"

// JSON-RPC and ACP error codes (https://agentclientprotocol.com/protocol/v1/error).
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	codeCancelled      = -32800
	codeAuthRequired   = -32000
	codeNotFound       = -32002
)

// Method names handled by the agent.
const (
	methodInitialize        = "initialize"
	methodAuthenticate      = "authenticate"
	methodLogout            = "logout"
	methodSessionNew        = "session/new"
	methodSessionPrompt     = "session/prompt"
	methodSessionClose      = "session/close"
	methodSessionDelete     = "session/delete"
	methodSessionCancel     = "session/cancel"
	methodCancelRequest     = "$/cancel_request"
	methodSessionUpdate     = "session/update"
	methodRequestPermission = "session/request_permission"
	methodFSRead            = "fs/read_text_file"
	methodFSWrite           = "fs/write_text_file"
	methodTermCreate        = "terminal/create"
	methodTermOutput        = "terminal/output"
	methodTermWait          = "terminal/wait_for_exit"
	methodTermRelease       = "terminal/release"
	methodTermKill          = "terminal/kill"
)

// Protocol version this agent implements.
const protocolVersion = 1

// Stop reasons reported by session/prompt responses.
const (
	stopEndTurn         = "end_turn"
	stopMaxTokens       = "max_tokens"
	stopMaxTurnRequests = "max_turn_requests"
	stopRefusal         = "refusal"
	stopCancelled       = "cancelled"
)

// Session update kinds emitted via session/update notifications.
const (
	updateThoughtChunk   = "agent_thought_chunk"
	updateMessageChunk   = "agent_message_chunk"
	updateToolCall       = "tool_call"
	updateToolCallUpdate = "tool_call_update"
	updateUsage          = "usage_update"
)

// Tool call statuses.
const (
	toolPending    = "pending"
	toolInProgress = "in_progress"
	toolCompleted  = "completed"
	toolFailed     = "failed"
)

// rpcError is the JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

func rpcErr(code int, message string, data any) *rpcError {
	return &rpcError{Code: code, Message: message, Data: data}
}

// rpcMessage is the decoded form of one JSON-RPC line. A nil ID marks a
// notification; a non-nil ID with a Method is a request; a non-nil ID without
// a Method is a response. Outbound messages omit empty fields.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (m *rpcMessage) isResponse() bool { return len(m.ID) > 0 && m.Method == "" }
func (m *rpcMessage) isRequest() bool  { return len(m.ID) > 0 && m.Method != "" }

// response builds a JSON-RPC response message echoing the request ID. A nil
// ID (parse errors, invalid frames) becomes an explicit null per JSON-RPC.
func response(id json.RawMessage, result any, rpcErr *rpcError) *rpcMessage {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	msg := &rpcMessage{JSONRPC: "2.0", ID: id, Error: rpcErr}
	if rpcErr == nil {
		if result == nil {
			msg.Result = json.RawMessage("null")
		} else if raw, ok := result.(json.RawMessage); ok {
			msg.Result = raw
		} else if data, err := json.Marshal(result); err == nil {
			msg.Result = data
		} else {
			msg.Error = rpcErrInternal("encode result: " + err.Error())
			msg.Result = nil
		}
	}
	return msg
}

func rpcErrInternal(message string) *rpcError {
	return rpcErr(codeInternalError, message, nil)
}

// implementation identifies an agent or client program.
type implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type fsCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

// initializeParams is the client's initialize request.
type initializeParams struct {
	ProtocolVersion    int `json:"protocolVersion"`
	ClientCapabilities struct {
		FS       fsCapabilities   `json:"fs"`
		Terminal bool             `json:"terminal"`
		Session  *json.RawMessage `json:"session"`
	} `json:"clientCapabilities"`
	ClientInfo *implementation `json:"clientInfo"`
	Meta       map[string]any  `json:"_meta"`
}

// clientCaps records the client abilities that gate agent-side calls.
type clientCaps struct {
	fsRead  bool
	fsWrite bool
	term    bool
}

// sessionNewParams is the client's session/new request.
type sessionNewParams struct {
	Cwd                   string            `json:"cwd"`
	AdditionalDirectories []string          `json:"additionalDirectories"`
	MCPServers            []json.RawMessage `json:"mcpServers"`
	Meta                  map[string]any    `json:"_meta"`
}

// promptParams is the client's session/prompt request.
type promptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
	Meta      map[string]any `json:"_meta"`
}

// contentBlock is one prompt content entry. Only the fields FakeAgents reads
// are decoded; unknown block types fall through to the Type marker.
type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Name     string `json:"name"`
	URI      string `json:"uri"`
	MimeType string `json:"mimeType"`
	Resource *struct {
		URI      string `json:"uri"`
		Text     string `json:"text"`
		MimeType string `json:"mimeType"`
	} `json:"resource"`
}

// sessionIDParams covers cancel/close/delete style requests.
type sessionIDParams struct {
	SessionID string `json:"sessionId"`
}

// cancelRequestParams is the $/cancel_request notification payload.
type cancelRequestParams struct {
	ID json.RawMessage `json:"id"`
}

// sessionUpdate is the params of a session/update notification.
type sessionUpdate struct {
	SessionID string         `json:"sessionId"`
	Update    map[string]any `json:"update"`
	Meta      map[string]any `json:"_meta,omitempty"`
}

// permissionOption is one option offered by session/request_permission.
type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// permissionOutcome is the client's answer to a permission request.
type permissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId"`
}

type permissionResponse struct {
	Outcome permissionOutcome `json:"outcome"`
}

// terminalCreateResult is the client's terminal/create response.
type terminalCreateResult struct {
	TerminalID string `json:"terminalId"`
}

// terminalOutputResult is the client's terminal/output response.
type terminalOutputResult struct {
	Output     string `json:"output"`
	Truncated  bool   `json:"truncated"`
	ExitStatus *struct {
		ExitCode *int `json:"exitCode"`
	} `json:"exitStatus"`
}

// fsReadResult is the client's fs/read_text_file response.
type fsReadResult struct {
	Content string `json:"content"`
}
