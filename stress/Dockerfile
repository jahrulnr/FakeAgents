# syntax=docker/dockerfile:1
FROM node:22-bookworm-slim

ARG CODEX_VERSION=0.167.0
ARG CLAUDE_CODE_VERSION=2.1.322
ARG OPENCODE_VERSION=1.19.5

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        git \
        jq \
        procps \
        ripgrep \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g \
        "@openai/codex@${CODEX_VERSION}" \
        "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
        "opencode-ai@${OPENCODE_VERSION}" \
    && npm cache clean --force

RUN mkdir -p /root/.codex /root/.claude /root/.config/opencode /workspace

COPY codex/config.toml /root/.codex/config.toml
COPY claude/claude.json /root/.claude.json
COPY claude/settings.json /root/.claude/settings.json
COPY opencode/opencode.json /root/.config/opencode/opencode.json
COPY opencode/opencode.json /workspace/opencode.json
COPY run.sh /usr/local/bin/run-stress.sh
RUN chmod +x /usr/local/bin/run-stress.sh

ENV HOME=/root \
    OPENAI_API_KEY=sk-fakeagents \
    FAKEAGENTS_API_KEY=sk-fakeagents \
    ANTHROPIC_BASE_URL=http://fakeagents:11110 \
    ANTHROPIC_AUTH_TOKEN=sk-ant-fakeagents \
    ANTHROPIC_API_KEY=sk-ant-fakeagents \
    ANTHROPIC_MODEL=claude-sonnet-4-5 \
    ANTHROPIC_DEFAULT_SONNET_MODEL=claude-sonnet-4-5 \
    ANTHROPIC_DEFAULT_OPUS_MODEL=claude-sonnet-4-5 \
    ANTHROPIC_SMALL_FAST_MODEL=claude-sonnet-4-5 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    DISABLE_TELEMETRY=1

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/run-stress.sh"]
