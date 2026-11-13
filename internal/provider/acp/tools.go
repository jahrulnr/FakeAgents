package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/jahrulnr/fakeagents/internal/engine"
)

// toolOutcome tells the prompt loop whether the turn continues.
type toolOutcome int

const (
	toolRan       toolOutcome = iota // tool reported completed or failed; turn continues
	toolCancelled                    // the client cancelled the turn during permission
)

// execToolCall reports a tool call through the ACP lifecycle — pending,
// optional permission, in_progress, completed/failed — and resolves execution
// by delegating to client fs/terminal methods when advertised, otherwise by
// fabricating deterministic output. The returned text is stored in session
// history as the tool result the next engine iteration sees.
func (a *Agent) execToolCall(ctx context.Context, emit *emitter, call engine.ToolCall) (string, toolOutcome) {
	kind := toolKind(call.Name)
	arguments, argsErr := decodeArguments(call.Arguments)
	rawInput := any(arguments)
	if argsErr != nil {
		rawInput = call.Arguments
	}

	emit.update(map[string]any{
		"sessionUpdate": updateToolCall,
		"toolCallId":    call.ID,
		"title":         toolTitle(call.Name, arguments),
		"name":          call.Name,
		"kind":          kind,
		"status":        toolPending,
		"rawInput":      rawInput,
	})

	if argsErr != nil {
		return a.finishTool(emit, call, fmt.Sprintf("malformed arguments: %s", call.Arguments), true)
	}

	if a.needsPermission(kind) {
		if emit.sess.allowRules["deny:"+kind] {
			return a.finishTool(emit, call, "user rejected the tool call (always)", true)
		}
		if !sessAllows(emit.sess, kind) {
			rejection, cancelled := a.requestPermission(ctx, emit, call, kind, rawInput)
			if cancelled {
				return "", toolCancelled
			}
			if rejection != "" {
				return a.finishTool(emit, call, rejection, true)
			}
		}
	}

	emit.update(map[string]any{
		"sessionUpdate": updateToolCallUpdate,
		"toolCallId":    call.ID,
		"status":        toolInProgress,
	})

	text, failed := a.execute(ctx, emit, call, kind, arguments)
	return a.finishTool(emit, call, text, failed)
}

// finishTool reports the terminal status and returns history text.
func (a *Agent) finishTool(emit *emitter, call engine.ToolCall, text string, failed bool) (string, toolOutcome) {
	status := toolCompleted
	if failed {
		status = toolFailed
	}
	emit.update(map[string]any{
		"sessionUpdate": updateToolCallUpdate,
		"toolCallId":    call.ID,
		"status":        status,
		"content": []any{map[string]any{
			"type":    "content",
			"content": map[string]any{"type": "text", "text": text},
		}},
		"rawOutput": map[string]any{"text": text, "simulated": true},
	})
	return text, toolRan
}

// needsPermission applies providers.acp.permission policy.
func (a *Agent) needsPermission(kind string) bool {
	switch a.cfg.Providers.ACP.PermissionMode() {
	case "always":
		return true
	case "mutating":
		return mutatingKind(kind)
	default:
		return false
	}
}

func mutatingKind(kind string) bool {
	switch kind {
	case "edit", "delete", "move", "execute", "other":
		return true
	default:
		return false
	}
}

func sessAllows(sess *acpSession, kind string) bool {
	return sess.allowRules[kind]
}

// requestPermission asks the client for authorization. It returns a
// rejection reason, or cancelled=true when the turn was cancelled; empty
// rejection means proceed.
func (a *Agent) requestPermission(ctx context.Context, emit *emitter, call engine.ToolCall, kind string, rawInput any) (string, bool) {
	options := []permissionOption{
		{OptionID: "allow-once", Name: "Allow once", Kind: "allow_once"},
		{OptionID: "allow-always", Name: "Always allow " + call.Name, Kind: "allow_always"},
		{OptionID: "reject-once", Name: "Reject", Kind: "reject_once"},
		{OptionID: "reject-always", Name: "Always reject " + call.Name, Kind: "reject_always"},
	}
	params := map[string]any{
		"sessionId": emit.sess.id,
		"toolCall": map[string]any{
			"toolCallId": call.ID,
			"title":      toolTitle(call.Name, nil),
			"name":       call.Name,
			"kind":       kind,
			"status":     toolPending,
			"rawInput":   rawInput,
		},
		"options": options,
	}
	var result permissionResponse
	if rpcErr := a.conn.call(ctx, methodRequestPermission, params, &result); rpcErr != nil {
		if rpcErr.Code == codeCancelled {
			return "", true
		}
		a.logger.Warn("acp: permission request failed", "error", rpcErr.Message, "session", emit.sess.id)
		return fmt.Sprintf("permission request failed: %s (code %d)", rpcErr.Message, rpcErr.Code), false
	}
	switch result.Outcome.Outcome {
	case "cancelled":
		return "", true
	case "selected":
		switch result.Outcome.OptionID {
		case "allow-always":
			emit.sess.allowRules[kind] = true
			return "", false
		case "allow-once":
			return "", false
		case "reject-always":
			emit.sess.allowRules["deny:"+kind] = true
			return "user rejected the tool call (always)", false
		default:
			return "user rejected the tool call", false
		}
	default:
		return "unrecognized permission outcome", false
	}
}

// execute runs the simulated tool: delegate to client capabilities when
// possible, else fabricate a deterministic result.
func (a *Agent) execute(ctx context.Context, emit *emitter, call engine.ToolCall, kind string, arguments map[string]any) (string, bool) {
	if a.cfg.Providers.ACP.ClientToolsEnabled() {
		if text, failed, handled := a.executeViaClient(ctx, emit, call, kind, arguments); handled {
			return text, failed
		}
	}
	return fmt.Sprintf("Simulated %s output for %s.", kind, call.Name), false
}

// executeViaClient maps simulated tool semantics onto client-side methods.
// The third return value reports whether delegation handled the call; when
// false the caller falls back to fabrication.
func (a *Agent) executeViaClient(ctx context.Context, emit *emitter, call engine.ToolCall, kind string, arguments map[string]any) (string, bool, bool) {
	sess := emit.sess
	caps := a.capabilities()
	absPath := func(arguments map[string]any) string {
		return resolveToolPath(sess.cwd, stringArg(arguments, "path", "file_path", "filePath", "file", "filename"))
	}
	switch kind {
	case "read":
		path := absPath(arguments)
		if !caps.fsRead || path == "" {
			return "", false, false
		}
		var result fsReadResult
		params := map[string]any{"sessionId": sess.id, "path": path}
		if rpcErr := a.conn.call(ctx, methodFSRead, params, &result); rpcErr != nil {
			return fmt.Sprintf("fs/read_text_file failed: %s", rpcErr.Message), true, true
		}
		return result.Content, false, true

	case "edit":
		path := absPath(arguments)
		content := stringArg(arguments, "content", "text", "new_str", "newStr", "newText", "body")
		if !caps.fsWrite || path == "" {
			return "", false, false
		}
		var oldText *string
		if caps.fsRead {
			var read fsReadResult
			if rpcErr := a.conn.call(ctx, methodFSRead, map[string]any{"sessionId": sess.id, "path": path}, &read); rpcErr == nil {
				old := read.Content
				oldText = &old
			}
		}
		if rpcErr := a.conn.call(ctx, methodFSWrite, map[string]any{
			"sessionId": sess.id, "path": path, "content": content,
		}, nil); rpcErr != nil {
			return fmt.Sprintf("fs/write_text_file failed: %s", rpcErr.Message), true, true
		}
		// Surface the write as a diff so clients can render the change.
		emit.update(map[string]any{
			"sessionUpdate": updateToolCallUpdate,
			"toolCallId":    call.ID,
			"content": []any{map[string]any{
				"type":    "diff",
				"path":    path,
				"oldText": oldText,
				"newText": content,
			}},
		})
		return fmt.Sprintf("wrote %d bytes to %s", len(content), path), false, true

	case "execute":
		command := stringArg(arguments, "command", "cmd", "script", "cmdline")
		if !caps.term || command == "" {
			return "", false, false
		}
		var created terminalCreateResult
		if rpcErr := a.conn.call(ctx, methodTermCreate, map[string]any{
			"sessionId": sess.id, "command": command, "args": stringSliceArg(arguments, "args"), "cwd": sess.cwd,
		}, &created); rpcErr != nil {
			return fmt.Sprintf("terminal/create failed: %s", rpcErr.Message), true, true
		}
		// Embed the live terminal before releasing it, per ACP rules.
		emit.update(map[string]any{
			"sessionUpdate": updateToolCallUpdate,
			"toolCallId":    call.ID,
			"content":       []any{map[string]any{"type": "terminal", "terminalId": created.TerminalID}},
		})
		var exited struct {
			ExitCode *int `json:"exitCode"`
		}
		waitErr := a.conn.call(ctx, methodTermWait, map[string]any{"sessionId": sess.id, "terminalId": created.TerminalID}, &exited)
		if waitErr != nil {
			a.logger.Debug("acp: terminal/wait_for_exit failed", "error", waitErr.Message, "session", sess.id)
			// The command is still running (or the turn was cancelled): kill it
			// so the client does not leak a live terminal.
			_ = a.conn.call(context.WithoutCancel(ctx), methodTermKill, map[string]any{
				"sessionId": sess.id, "terminalId": created.TerminalID,
			}, nil)
		}
		var output terminalOutputResult
		if outErr := a.conn.call(ctx, methodTermOutput, map[string]any{"sessionId": sess.id, "terminalId": created.TerminalID}, &output); outErr != nil {
			a.logger.Debug("acp: terminal/output failed", "error", outErr.Message, "session", sess.id)
		}
		// Release is attempted even after failures; WithoutCancel survives the
		// cancelled turn so the client can always reclaim the terminal.
		if relErr := a.conn.call(context.WithoutCancel(ctx), methodTermRelease, map[string]any{
			"sessionId": sess.id, "terminalId": created.TerminalID,
		}, nil); relErr != nil {
			a.logger.Debug("acp: terminal/release failed", "error", relErr.Message, "session", sess.id)
		}
		text := strings.TrimSpace(output.Output)
		if text == "" {
			text = fmt.Sprintf("command %q finished", command)
		}
		if exited.ExitCode != nil {
			text += fmt.Sprintf("\n(exit code %d)", *exited.ExitCode)
		}
		return text, exited.ExitCode != nil && *exited.ExitCode != 0, true
	}
	return "", false, false
}

// decodeArguments parses tool call arguments; malformed JSON is reported
// verbatim so fault scenarios (e.g. the tool-error script) stay observable.
func decodeArguments(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, err
	}
	return args, nil
}

// resolveToolPath anchors a relative tool path at the session cwd.
// ACP paths are client protocol strings, not host filesystem paths. filepath
// rewrites a slash-rooted cwd such as /tmp/work into \tmp\work on Windows,
// and that result is not absolute there. A slash-rooted cwd stays
// slash-separated on every OS; other cwd forms use the host path package.
func resolveToolPath(cwd, p string) string {
	if p == "" || isProtocolAbs(p) {
		return p
	}
	if strings.HasPrefix(cwd, "/") {
		return path.Join(cwd, p)
	}
	return filepath.Join(cwd, p)
}

func isProtocolAbs(p string) bool {
	return strings.HasPrefix(p, "/") || filepath.IsAbs(p)
}

func stringArg(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := args[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func stringSliceArg(args map[string]any, keys ...string) []string {
	for _, key := range keys {
		if list, ok := args[key].([]any); ok {
			out := make([]string, 0, len(list))
			for _, item := range list {
				if s, ok := item.(string); ok {
					out = append(out, s)
				}
			}
			return out
		}
	}
	return nil
}

// toolTitle renders a short human title for the call.
func toolTitle(name string, args map[string]any) string {
	if args == nil {
		return name
	}
	for _, key := range []string{"command", "cmd", "path", "file_path"} {
		if value := stringArg(args, key); value != "" {
			return name + " " + value
		}
	}
	return name
}

// toolKind maps a tool name onto an ACP ToolKind for presentation and the
// permission policy.
func toolKind(name string) string {
	n := strings.ToLower(name)
	contains := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(n, w) {
				return true
			}
		}
		return false
	}
	switch {
	case contains("bash", "shell", "exec", "command", "cmd", "terminal", "run_"):
		return "execute"
	case contains("read", "cat", "view", "load", "open"):
		return "read"
	case contains("write", "edit", "patch", "replace", "create", "save", "update", "append"):
		return "edit"
	case contains("delete", "remove", "rm_", "_rm"):
		return "delete"
	case contains("move", "rename", "mv"):
		return "move"
	case contains("search", "grep", "find", "glob", "list", "ls"):
		return "search"
	case contains("fetch", "http", "url", "curl", "download", "web"):
		return "fetch"
	case contains("think", "plan", "reason"):
		return "think"
	default:
		return "other"
	}
}
