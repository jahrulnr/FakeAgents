#!/usr/bin/env bash
# Quick smoke test against a running FakeAgents instance.
set -euo pipefail

BASE="${BASE:-http://127.0.0.1:11110}"
SESSION="${SESSION:-smoke-$$}"
PASS=0

check() {
  printf 'PASS  %s\n' "$1"
  PASS=$((PASS + 1))
}

fail() {
  printf 'FAIL  %s\n' "$1"
  exit 1
}

grep_body() {
  local body="$1" needle="$2" name="$3"
  if grep -Fq -- "$needle" <<<"$body"; then
    check "$name"
  else
    fail "$name"
  fi
}

echo "FakeAgents smoke test: $BASE"

health="$(curl -fsS "$BASE/healthz")"
grep_body "$health" '"status":"ok"' "health"

chat="$(curl -fsS "$BASE/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -H "x-fakeagents-session: $SESSION" \
  -d '{"model":"fake-gpt-4o","reasoning_effort":"high","messages":[{"role":"user","content":"smoke chat"}]}')"
grep_body "$chat" '"chat.completion"' "chat completion"
grep_body "$chat" '"reasoning_content"' "chat reasoning"

stream="$(curl -fsS -N "$BASE/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -H "x-fakeagents-session: $SESSION" \
  -d '{"model":"fake-gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"smoke stream"}]}')"
grep_body "$stream" 'chat.completion.chunk' "chat stream"
grep_body "$stream" '[DONE]' "chat terminal event"

responses="$(curl -fsS "$BASE/v1/responses" \
  -H 'Content-Type: application/json' \
  -d '{"model":"fake-gpt-5","input":"smoke responses","reasoning":{"effort":"medium"},"include":["reasoning.encrypted_content"]}')"
grep_body "$responses" '"object":"response"' "responses"
grep_body "$responses" '"encrypted_content"' "encrypted reasoning"

messages="$(curl -fsS "$BASE/v1/messages" \
  -H 'Content-Type: application/json' \
  -H 'anthropic-version: 2023-06-01' \
  -d '{"model":"fake-claude-sonnet","max_tokens":256,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"smoke messages"}]}')"
grep_body "$messages" '"type":"message"' "anthropic messages"
grep_body "$messages" '"signature"' "thinking signature"

billing_status="$(curl -sS -o /tmp/fakeagents-smoke-billing.json -w '%{http_code}' \
  "$BASE/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{"model":"fake-gpt-4o","messages":[{"role":"user","content":"[[fakeagents:billing]]"}]}')"
if [ "$billing_status" = "429" ]; then
  check "billing error status"
else
  fail "billing error status (got $billing_status)"
fi
grep_body "$(cat /tmp/fakeagents-smoke-billing.json)" 'insufficient_quota' "billing error code"

models="$(curl -fsS "$BASE/v1/models")"
grep_body "$models" 'fake-gpt-4o' "model catalog"

echo
echo "All $PASS smoke checks passed."
