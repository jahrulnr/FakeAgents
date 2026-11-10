# CLI stress test

> **Safety warning:** FakeAgents only emits simulated tool calls. The real CLIs
> in this container **execute** those calls. The bundled test runs only the
> read-only command `pwd`, but editing a scenario can run arbitrary commands
> inside the container. Treat scenario files as executable policy and run this
> only in a disposable container. You run it at your own risk; see
> [../SECURITY.md](../SECURITY.md).

This directory reproduces a realistic integration environment:

- FakeAgents server (built from the repository root)
- Codex CLI `0.167.0`
- Claude Code CLI `2.1.322`
- OpenCode CLI `1.19.5`

All three CLIs are configured to use FakeAgents as their model provider. The test fails if any CLI cannot complete a turn, cannot execute the scripted `pwd` tool call, or if any contract check fails.

```bash
# from the repository root
make stress
```

or directly:

```bash
docker compose -f stress/docker-compose.yml up --build \
  --abort-on-container-exit --exit-code-from clis
```

## What is verified

- Codex completes a Responses API turn.
- Claude Code completes a Messages API turn (streaming).
- OpenCode completes a Chat Completions turn and sends `x-opencode-session`, which the server enforces for `chat.completions`.
- Codex executes `exec_command` with `{"cmd":"pwd"}`, Claude Code executes `Bash` with `{"command":"pwd"}`, and OpenCode executes `bash` with `{"command":"pwd"}`. Each harness sends the real result back and FakeAgents returns the final answer.
- OpenAI Chat: normal response, thinking, tool call, billing error.
- OpenAI Responses: output text, encrypted reasoning, streaming terminal event.
- Anthropic Messages: text response, billing error.
- Both model catalogs.
- Required-header rejection.
- Mid-stream error frame after HTTP 200.

## Layout

| Path | Purpose |
|---|---|
| `Dockerfile` | Node image with the three CLIs and their configs |
| `docker-compose.yml` | FakeAgents + CLI runner |
| `fakeagents.json` | Server config (required header, latency disabled) |
| `codex/config.toml` | Codex custom provider |
| `claude/claude.json` | Claude Code onboarding state |
| `claude/settings.json` | Claude Code model |
| `opencode/opencode.json` | OpenCode custom OpenAI-compatible provider |
| `run.sh` | CLI runs + 42 assertions |

## Notes

- The CLI image downloads npm packages at build time. Rebuild it when changing CLI versions.
- The FakeAgents container binds port `11110`; the host mapping defaults to `18080` and can be changed with `FAKEAGENTS_PORT`.
- The stress config disables latency. Use a different config to test slow streams.
- Claude Code refuses `--dangerously-skip-permissions` when running as root, so the test uses `--allowedTools "Bash(pwd)"`.
- Codex uses `-s danger-full-access` inside the container because nested bubblewrap is unavailable there.
- OpenCode uses `--auto` to approve the scripted `pwd` call.
- Tool names are pinned to the CLI versions in this directory. A future CLI release may rename `exec_command`, `Bash`, or `bash`; update the scenarios if it does.
