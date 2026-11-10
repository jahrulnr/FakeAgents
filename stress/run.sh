#!/usr/bin/env bash
#
# End-to-end stress test: three real coding CLIs (Codex, Claude Code, OpenCode)
# are pointed at FakeAgents and must complete a turn. Contract checks exercise
# streaming, thinking, tools, billing errors, required headers, and mid-stream
# failures.
set -uo pipefail

BASE="${FAKEAGENTS_URL:-http://fakeagents:11110}"
LOG_DIR="${LOG_DIR:-/tmp/fakeagents-stress}"
FAILED=0
CHECKS=0

mkdir -p "$LOG_DIR"

pass() {
  CHECKS=$((CHECKS + 1))
  printf 'PASS  %s\n' "$1"
}

fail() {
  CHECKS=$((CHECKS + 1))
  FAILED=$((FAILED + 1))
  printf 'FAIL  %s\n' "$1"
}

wait_for_server() {
  printf 'Waiting for FakeAgents at %s ...\n' "$BASE"
  for _ in $(seq 1 60); do
    if curl -fsS "$BASE/healthz" >/dev/null 2>&1; then
      printf 'FakeAgents is ready.\n\n'
      return 0
    fi
    sleep 1
  done
  return 1
}

run_cli() {
  local name="$1" log="$2"
  shift 2
  printf -- '--- %s ---\n' "$name"
  if timeout 180 "$@" </dev/null >"$log" 2>&1; then
    pass "$name completed a turn (exit 0)"
  else
    local code=$?
    fail "$name failed (exit $code)"
    tail -40 "$log" || true
  fi
}

expect_in_log() {
  local name="$1" log="$2" needle="$3"
  if grep -Fq -- "$needle" "$log"; then
    pass "$name produced expected content"
  else
    fail "$name did not produce '$needle'"
    tail -30 "$log" || true
  fi
}

expect_status() {
  local name="$1" expected="$2" body="$3"
  shift 3
  local code
  code="$(curl -sS -o "$body" -w '%{http_code}' "$@" 2>"$body.err" || echo 000)"
  if [ "$code" = "$expected" ]; then
    pass "$name returned HTTP $expected"
  else
    fail "$name returned HTTP $code (expected $expected)"
    head -c 600 "$body" || true
    printf '\n'
  fi
}

expect_file_contains() {
  local name="$1" file="$2" needle="$3"
  if grep -Fq -- "$needle" "$file"; then
    pass "$name"
  else
    fail "$name"
    head -c 600 "$file" || true
    printf '\n'
  fi
}

check_health() {
  expect_status "health endpoint" 200 "$LOG_DIR/health.json" "$BASE/healthz"
  expect_file_contains "health reports ok" "$LOG_DIR/health.json" '"status":"ok"'
}

check_models() {
  expect_status "OpenAI model list" 200 "$LOG_DIR/models-openai.json" "$BASE/v1/models"
  expect_file_contains "OpenAI catalog contains fake-gpt-4o" "$LOG_DIR/models-openai.json" 'fake-gpt-4o'

  expect_status "Anthropic model list" 200 "$LOG_DIR/models-anthropic.json" \
    -H 'x-api-key: sk-ant-fakeagents' "$BASE/v1/models"
  expect_file_contains "Anthropic catalog contains fake-claude-sonnet" "$LOG_DIR/models-anthropic.json" 'fake-claude-sonnet'
}

check_harness_exec() {
  printf -- '--- Real harness execution (safe pwd) ---\n'
  local prompt='Run pwd in the terminal and report the output.'

  run_cli "Codex CLI real exec" "$LOG_DIR/codex-exec.log" \
    codex exec -s danger-full-access --skip-git-repo-check -m gpt-5.4 "$prompt"
  expect_in_log "Codex CLI real exec" "$LOG_DIR/codex-exec.log" "HARNESS_EXEC_OK"
  expect_in_log "Codex CLI real exec" "$LOG_DIR/codex-exec.log" "/workspace"

  run_cli "Claude Code CLI real exec" "$LOG_DIR/claude-exec.log" \
    claude -p "$prompt" --model claude-sonnet-4-5 --output-format text --max-turns 3 --allowedTools "Bash(pwd)"
  expect_in_log "Claude Code CLI real exec" "$LOG_DIR/claude-exec.log" "HARNESS_EXEC_OK"
  expect_in_log "Claude Code CLI real exec" "$LOG_DIR/claude-exec.log" "/workspace"

  run_cli "OpenCode CLI real exec" "$LOG_DIR/opencode-exec.log" \
    opencode run "$prompt" --model fakeagents/fake-gpt-4o --auto
  expect_in_log "OpenCode CLI real exec" "$LOG_DIR/opencode-exec.log" "HARNESS_EXEC_OK"
  expect_in_log "OpenCode CLI real exec" "$LOG_DIR/opencode-exec.log" "/workspace"
}

check_contracts() {
  local chat_headers=(-H 'Content-Type: application/json' -H 'x-opencode-session: stress-check')
  local anthropic_headers=(-H 'Content-Type: application/json' -H 'anthropic-version: 2023-06-01' -H 'x-api-key: sk-ant-fakeagents')

  expect_status "OpenAI chat completion" 200 "$LOG_DIR/chat.json" \
    "${chat_headers[@]}" \
    -d '{"model":"fake-gpt-4o","messages":[{"role":"user","content":"contract ping"}]}' \
    "$BASE/v1/chat/completions"
  expect_file_contains "chat response has assistant content" "$LOG_DIR/chat.json" '"content":"'

  expect_status "OpenAI thinking" 200 "$LOG_DIR/thinking.json" \
    "${chat_headers[@]}" \
    -d '{"model":"fake-gpt-4o","reasoning_effort":"high","messages":[{"role":"user","content":"think"}]}' \
    "$BASE/v1/chat/completions"
  expect_file_contains "reasoning_content is present" "$LOG_DIR/thinking.json" '"reasoning_content"'
  expect_file_contains "reasoning token usage is present" "$LOG_DIR/thinking.json" '"reasoning_tokens"'

  expect_status "OpenAI tool call" 200 "$LOG_DIR/tool.json" \
    "${chat_headers[@]}" \
    -d '{"model":"fake-gpt-4o","messages":[{"role":"user","content":"weather? [[fakeagents:tool-call]]"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}' \
    "$BASE/v1/chat/completions"
  expect_file_contains "tool_calls is present" "$LOG_DIR/tool.json" '"tool_calls"'
  expect_file_contains "finish_reason is tool_calls" "$LOG_DIR/tool.json" '"finish_reason":"tool_calls"'

  expect_status "OpenAI billing error" 429 "$LOG_DIR/billing.json" \
    "${chat_headers[@]}" \
    -d '{"model":"fake-gpt-4o","messages":[{"role":"user","content":"[[fakeagents:billing]]"}]}' \
    "$BASE/v1/chat/completions"
  expect_file_contains "billing error code" "$LOG_DIR/billing.json" 'insufficient_quota'

  expect_status "required header enforcement" 400 "$LOG_DIR/header.json" \
    -H 'Content-Type: application/json' \
    -d '{"model":"fake-gpt-4o","messages":[{"role":"user","content":"no session header"}]}' \
    "$BASE/v1/chat/completions"
  expect_file_contains "missing header error code" "$LOG_DIR/header.json" 'missing_required_header'

  expect_status "Anthropic messages" 200 "$LOG_DIR/messages.json" \
    "${anthropic_headers[@]}" \
    -d '{"model":"fake-claude-sonnet","max_tokens":256,"messages":[{"role":"user","content":"contract ping"}]}' \
    "$BASE/v1/messages"
  expect_file_contains "messages response has text" "$LOG_DIR/messages.json" '"type":"text"'

  expect_status "Anthropic billing error" 402 "$LOG_DIR/anthropic-billing.json" \
    "${anthropic_headers[@]}" \
    -d '{"model":"fake-claude-sonnet","max_tokens":64,"messages":[{"role":"user","content":"[[fakeagents:billing]]"}]}' \
    "$BASE/v1/messages"
  expect_file_contains "anthropic billing_error" "$LOG_DIR/anthropic-billing.json" 'billing_error'

  expect_status "Responses API" 200 "$LOG_DIR/responses.json" \
    "$BASE/v1/responses" \
    -H 'Content-Type: application/json' \
    -d '{"model":"gpt-5.4","input":"responses contract ping","reasoning":{"effort":"medium"},"include":["reasoning.encrypted_content"]}'
  expect_file_contains "responses output text" "$LOG_DIR/responses.json" '"output_text"'
  expect_file_contains "responses encrypted reasoning" "$LOG_DIR/responses.json" '"encrypted_content"'

  curl -sS -N "$BASE/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -H 'x-opencode-session: stress-check' \
    -d '{"model":"fake-gpt-4o","stream":true,"messages":[{"role":"user","content":"[[fakeagents:mid-stream-error]]"}]}' \
    >"$LOG_DIR/stream-error.log" 2>&1 || true
  expect_file_contains "mid-stream error frame" "$LOG_DIR/stream-error.log" '"error"'

  curl -sS -N "$BASE/v1/responses" \
    -H 'Content-Type: application/json' \
    -d '{"model":"gpt-5.4","input":"stream ping","stream":true}' \
    >"$LOG_DIR/responses-stream.log" 2>&1 || true
  expect_file_contains "Responses terminal event" "$LOG_DIR/responses-stream.log" 'response.completed'
}

printf '=============================================\n'
printf ' FakeAgents CLI stress test\n'
printf '=============================================\n'
printf 'codex:    %s\n' "$(codex --version 2>/dev/null || echo missing)"
printf 'claude:   %s\n' "$(claude --version 2>/dev/null || echo missing)"
printf 'opencode: %s\n\n' "$(opencode --version 2>/dev/null || echo missing)"
printf 'WARNING: the harness executes real tool calls. This suite runs only "pwd"\n'
printf 'inside a disposable container. Never use untrusted scenario files.\n\n' 

if ! wait_for_server; then
  printf 'FakeAgents never became healthy.\n'
  exit 1
fi

git init -q /workspace 2>/dev/null || true

run_cli "Codex CLI" "$LOG_DIR/codex.log" \
  codex exec -s read-only --skip-git-repo-check -m gpt-5.4 \
  "Reply with the token FAKEAGENTS_CODEX_PING and nothing else."
expect_in_log "Codex CLI" "$LOG_DIR/codex.log" "FAKEAGENTS_CODEX_PING"

run_cli "Claude Code CLI" "$LOG_DIR/claude.log" \
  claude -p "Reply with the token FAKEAGENTS_CLAUDE_PING and nothing else." \
  --model claude-sonnet-4-5 --output-format text --max-turns 1
expect_in_log "Claude Code CLI" "$LOG_DIR/claude.log" "FAKEAGENTS_CLAUDE_PING"

run_cli "OpenCode CLI" "$LOG_DIR/opencode.log" \
  opencode run "Reply with the token FAKEAGENTS_OPENCODE_PING and nothing else." \
  --model fakeagents/fake-gpt-4o
expect_in_log "OpenCode CLI" "$LOG_DIR/opencode.log" "FAKEAGENTS_OPENCODE_PING"

check_health
check_models
check_harness_exec
check_contracts

printf '\n=============================================\n'
if [ "$FAILED" -eq 0 ]; then
  printf ' ALL %d CHECKS PASSED\n' "$CHECKS"
  exit 0
fi
printf ' %d OF %d CHECKS FAILED\n' "$FAILED" "$CHECKS"
exit 1
