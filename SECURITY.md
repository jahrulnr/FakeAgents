# Security and safe testing

FakeAgents is a **test double**. It never executes a tool, runs a shell command, or
calls an external model. It only emits provider-shaped HTTP responses.

## The harness executes tools for real

When you point a real agent harness (Codex CLI, Claude Code, OpenCode, an SDK
agent, or your own loop) at FakeAgents, that harness may execute the tool calls
FakeAgents returns. FakeAgents is fake; the harness is not.

A scenario is therefore **executable policy**. A template or scenario step can
instruct the harness to run any command it has permission to run. Treat scenario
files, prompt templates, and `respond.steps` as code from an untrusted source.

**You run harnesses and scenarios at your own risk.** The FakeAgents authors are
not responsible for commands that you or your templates cause a harness to
execute, or for any damage, data loss, cost, or side effect that results.

## Recommended isolation

- Run the harness in a disposable container or VM.
- Use a non-root user and a read-only or throwaway workspace.
- Do not mount production secrets, SSH keys, cloud credentials, or a real home
  directory into the container.
- Prefer explicit command allowlists (`Bash(pwd)`, `Bash(echo *)`) over blanket
  approval.
- Disable or tightly scope network access when the test does not need it.
- Inspect third-party scenario files before running them.

## Bundled stress test

`stress/` runs three real CLIs against FakeAgents inside a disposable Docker
container. Its scripted tool call is intentionally limited to the read-only
command `pwd`. The suite uses:

- Codex `-s danger-full-access` because nested bubblewrap is unavailable inside
  the container;
- Claude Code `--allowedTools "Bash(pwd)"`;
- OpenCode `--auto`.

Those flags grant real execution inside that container. Do not reuse them
against a workspace you care about.

## Reporting

If you find a vulnerability in FakeAgents itself, open a private security report
with the repository maintainers rather than a public issue. Do not include
credentials, tokens, or sensitive prompts in reports.
