# Chat templates

Templates control what the fake assistant says. They are standard Go [`text/template`](https://pkg.go.dev/text/template) sources with a small function library. Use them to change tone, length, formatting, and structure per scenario so tests do not all look identical.

```json
{
  "templates": {
    "default": "{{ .UserMessage }}\n\n— simulated by FakeAgents ({{ .Model }})",
    "brief": "Noted.",
    "markdown": "## Answer\n\n{{ .UserMessage }}\n\n- model: `{{ .Model }}`"
  },
  "scenarios": [
    {
      "name": "markdown-mode",
      "match": {"triggers": ["markdown"]},
      "respond": {"text_template": "markdown"}
    }
  ]
}
```

Templates can also be inline:

```json
{"respond": {"text": "Echo: {{ upper .UserMessage }}"}}
```

Or loaded from a file:

```json
{"templates": {"default": "@file:templates/default.md"}}
```

## Built-in templates

| Name | Purpose |
|---|---|
| `default` | Short answer that echoes the user message and identifies the model |
| `echo` | Exact user message |
| `reasoning` | Reasoning text, scaled by `.Effort` |
| `final` | Final answer after a tool result |
| `refusal` | Safety refusal |
| `json` | Valid JSON object |
| `long` | Six paragraphs for truncation/scroll tests |
| `markdown` | Headings, list, fenced block |
| `code` | Fenced Go example |
| `unicode` | CJK, Cyrillic, Arabic, and emoji |
| `empty` | Empty string |

Override any built-in by defining a template with the same name.

## Data

| Field | Type | Description |
|---|---|---|
| `.Provider` | string | `openai` or `anthropic` |
| `.Endpoint` | string | `chat.completions`, `responses`, `messages` |
| `.Model` | string | Resolved model ID |
| `.Stream` | bool | Streaming request |
| `.Session` | string | Session key |
| `.Scenario` | string | Selected scenario name (`default` when none) |
| `.Turn` | int | 0-based turn for this session |
| `.Step` | int | Active agent/script step (assistant tool-call turns so far) |
| `.RequestID` | string | Request correlation ID |
| `.Effort` | string | `low`, `medium`, `high`, `xhigh`, `max`, or `medium` by default |
| `.Seed` | int64 | Deterministic RNG seed |
| `.Now` | time.Time | Request time |
| `.Random` | `*rand.Rand` | Seeded random source (`math/rand/v2`) |
| `.UserMessage` | string | Last user message, directives removed |
| `.System` | string | System/instructions |
| `.LastToolResult` | string | Content of the most recent tool result |
| `.LastToolName` | string | Name of the tool that produced it |
| `.ToolName` | string | Current tool, inside argument templates |
| `.ToolDescription` | string | Current tool description |
| `.Messages` | array | `{Role, Content, ToolCalls}` history |
| `.Tools` | array | `{Name, Description}` declarations |

```go
{{ range .Messages }}
{{ .Role }}: {{ .Content }}
{{ end }}
```

## Functions

| Function | Example | Result |
|---|---|---|
| `upper` / `lower` / `title` | `{{ upper "hi" }}` | `HI` |
| `trim` | `{{ trim "  x  " }}` | `x` |
| `trimPrefix` / `trimSuffix` | `{{ trimPrefix "a" "abc" }}` | `bc` |
| `replace` | `{{ replace "a" "b" "banana" }}` | `bbnbnb` |
| `repeat` | `{{ repeat 3 "ab" }}` | `ababab` |
| `join` | `{{ join "," (list "a" "b") }}` | `a,b` |
| `split` | `{{ split "," "a,b" }}` | `[a b]` |
| `contains` / `hasPrefix` / `hasSuffix` | `{{ contains "ell" .UserMessage }}` | bool |
| `truncate` | `{{ truncate 50 .UserMessage }}` | first 50 runes + `…` |
| `indent` | `{{ indent 2 "x" }}` | `  x` |
| `json` | `{{ json .UserMessage }}` | `"hello"` |
| `jsonPretty` | `{{ jsonPretty .Messages }}` | indented JSON |
| `quote` | `{{ quote "x" }}` | `"x"` |
| `default` | `{{ default "none" .LastToolResult }}` | fallback if empty |
| `coalesce` | `{{ coalesce "" .UserMessage "x" }}` | first non-empty |
| `list` | `{{ list 1 2 3 }}` | `[1 2 3]` |
| `dict` | `{{ dict "a" 1 "b" 2 }}` | map |
| `add` / `sub` / `mul` / `div` / `mod` | `{{ add 2 3 }}` | `5` |
| `min` / `max` | `{{ max 2 9 }}` | `9` |
| `ternary` | `{{ ternary .Stream "stream" "buffered" }}` | string |
| `now` | `{{ now.Format "2006-01-02" }}` | date |
| `uuid` | `{{ uuid }}` | v4 UUID |
| `randInt` | `{{ randInt 1 6 }}` | seeded integer |
| `pick` | `{{ pick "a" "b" "c" }}` | seeded choice |
| `shuffle` | `{{ shuffle (list 1 2 3) }}` | shuffled copy |
| `lorem` | `{{ lorem 20 }}` | 20 filler words |
| `codeblock` | `{{ codeblock "go" "fmt.Println(1)" }}` | fenced block |
| `pluralize` | `{{ pluralize 2 "item" "items" }}` | `items` |

`randInt`, `pick`, `shuffle`, and `lorem` use the request-seeded RNG, so they are reproducible for the same session and turn.

## Conditionals

```gotemplate
{{- if eq .Effort "high" -}}
Step 1: identify the constraint.
Step 2: verify the tool contract.
Step 3: answer.
{{- else -}}
The answer is intentionally short.
{{- end -}}
```

```gotemplate
{{- if .LastToolResult -}}
The tool returned:
{{ .LastToolResult }}
{{- else -}}
I need to call a tool first.
{{- end -}}
```

## JSON output

Always build JSON with `json` rather than string concatenation, otherwise quotes and newlines will break:

```json
{
  "templates": {
    "json": "{\"answer\":{{ json .UserMessage }},\"stream\":{{ .Stream }}}"
  }
}
```

For schema-driven JSON, prefer leaving the text empty and letting `structured_output.mode: "auto"` synthesize a valid object from the request's schema. Explicit text always wins, which is useful for testing invalid JSON edge cases:

```json
{
  "name": "invalid-json",
  "match": {"triggers": ["invalid-json"]},
  "respond": {"text": "{\"broken\": "}
}
```

## Tool argument templates

`defaults.tooling.argument_template` and `respond.tool_calls[].arguments_json` are templates too. They receive `.ToolName` and `.ToolDescription`:

```json
{
  "defaults": {
    "tooling": {
      "argument_strategy": "template",
      "argument_template": "{\"query\":{{ json .UserMessage }},\"limit\":5}"
    }
  }
}
```

## Strictness

Unknown data fields fail at render time (`missingkey=error`). All templates are parsed at startup by `fakeagents validate`, so syntax errors are caught before traffic arrives. Function execution is side-effect free except for randomness, UUIDs, and the clock.
