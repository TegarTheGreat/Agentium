# Changelog

All notable changes to Agentium are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

## [0.12.0] - 2026-09-23

A redesigned terminal experience, tested end to end against a live model (DeepSeek).

### Added

- **Setup inside the app.** On first run Agentium walks you through choosing a provider, pasting an API key (masked and checked against the provider) and picking a model from the provider's live model list, with context size and price. No separate `login` step is needed.
- **Live progress.** A spinner shows while the model is thinking. Each running command shows its elapsed time and its latest output lines, so long installs and builds never look frozen. Finished steps are recorded as `✓`/`✗` lines with their duration.
- **Type while the agent works.** Messages typed during a turn are queued and sent when it finishes; queued `/commands` run as commands. Ctrl-C clears the typed text, or interrupts the turn.
- **New commands:** `/login`, `/logout`, `/effort`, `/config` and `/help`. `/model` and `/mode` open arrow-key menus with type-to-filter, and the chosen model is saved as the default.
- **Clearer approvals:** single-key prompts such as "Run this command?" and "Change this file?".
- **Friendlier installer:** step-by-step output, a real download progress bar (percent, size, speed), retries, and a source build when no release binary exists.

### Changed

- A new session banner, colored prompt and a compact summary after each turn.
- Repeated identical steps (such as several edits to one file) collapse into one line with a count.
- Paths in step lines are shown relative to the workspace.
- The model is told that commands already start in the workspace, so it stops prefixing them with `cd`.

### Fixed

- `-m deepseek` (a provider without a model) now uses that provider's default model.
- The DeepSeek default model is now `deepseek-flash`, which the API accepts.
- `bash {kill: <job id>}`, as some models send it, now stops the job instead of failing.
- A lone Esc key no longer swallows the next keystrokes.

### Install

```sh
curl -fsSL https://raw.githubusercontent.com/TegarTheGreat/Agentium/main/install.sh | sh
# or
go install github.com/tegarthegreat/agentium/cmd/agentium@v0.12.0
```

## [0.11.0] - 2026-09-23

First public release. Agentium is a fast, minimal coding agent for the terminal, distributed as a single static binary for Linux, macOS and Windows.

### Highlights

- **Small and fast:** 8.8 MB static binary, ~6 ms startup, ~10 MB memory, ~1k-token prompt.
- **Any provider:** 19 built-in providers, including Anthropic, OpenAI, Gemini, OpenRouter, Bedrock, Vertex, Azure, Ollama and LM Studio, plus every compatible provider in models.dev.
- **Sandboxed by default:** shell commands run under Landlock (Linux) or `sandbox-exec` (macOS), and network access requires approval.
- **Undo for every turn:** checkpoints in a shadow repository, including changes made by shell commands.
- **Memory across sessions:** project notes, decisions and lessons learned from past errors, recalled only when relevant.

### Tools

- `read`: files, directory trees, outlines, and a ranked map of large codebases.
- `edit`: lint-gated, atomic edits with whitespace-tolerant matching.
- `bash`: sandboxed commands; background jobs with input, output and stop controls; pseudo-terminal mode for interactive programs.
- `search`: text, symbol definitions, references and past memory.
- `fetch`: web pages, and web search via Brave, Tavily or DuckDuckGo.
- `todo`: a checklist for multi-step work.
- `task`: sub-agents with a fresh context, run in parallel.

### Reliability

- Edits that would break a file that parsed before are rejected.
- The model verifies its changes with a build or test before finishing.
- Stuck detection, and reasoning effort that rises only after repeated failures.
- Truncated and stalled streams are recovered, and a fallback model chain takes over when the main model is unavailable.
- Language-server diagnostics after each edit: gopls, pyright, typescript-language-server, rust-analyzer, clangd.
- `--best-of N --check CMD` runs parallel attempts in git worktrees and keeps the best passing one.

### Integrations

- **MCP servers** over stdio, Streamable HTTP and SSE, with OAuth login (`agentium mcp login`).
- **Agent Client Protocol** (`agentium acp`) for editors such as Zed and JetBrains IDEs, validated against the official SDK schemas.
- **Skills** in the `SKILL.md` format, with a reviewed and pinned installer.
- **Hooks** after each edit and at the end of a task.
- **Headless mode** (`--json`) for CI, with a cost cap (`--max-cost`).

### Security

- Risky commands and credential reads need approval. Modes: `ask`, `auto`, `yolo`, `plan`.
- Credential-like environment variables are withheld from commands, hooks and MCP servers.
- `fetch` refuses secret-bearing URLs and private or metadata addresses.
- Git settings and hooks that would run programs outside the sandbox are reverted.

### Platform notes

- Linux and macOS are fully supported.
- Windows is supported without an OS sandbox. Commands run in Git Bash or PowerShell, and child processes end with Agentium. `ask` mode is recommended.

### Install

```sh
curl -fsSL https://raw.githubusercontent.com/TegarTheGreat/Agentium/main/install.sh | sh
# or
go install github.com/tegarthegreat/agentium/cmd/agentium@v0.11.0
```

Windows users can download the `.zip` archive below.

[0.12.0]: https://github.com/TegarTheGreat/Agentium/releases/tag/v0.12.0
[0.11.0]: https://github.com/TegarTheGreat/Agentium/releases/tag/v0.11.0
