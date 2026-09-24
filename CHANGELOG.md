# Changelog

All notable changes to Agentium are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

## [0.14.0] - 2026-09-24

A full audit of the code base (terminal UI, tools and sandbox, agent loop and providers, storage) with every verified finding fixed, plus a regression run against a live model.

### Security

- **Local-port exemption removed.** v0.13.0 let sandboxed commands reach any port that was listening locally. That also opened a way out through a local proxy (for example `HTTPS_PROXY=127.0.0.1:…`) or through a port a command opened itself, because port rules cannot tell hosts apart. Without `net`, no outbound connection is allowed again, localhost included; servers may still listen and be opened from a browser.
- **UDP and DNS blocked on Linux.** When network is off, UDP and raw sockets are refused with seccomp, so DNS lookups cannot carry data out. io_uring is refused too.
- **Credentials unreadable in the sandbox.** This covers `~/.ssh`, `~/.aws`, `~/.gnupg`, `.netrc`, gh/gcloud config and Agentium's own auth and config files, even when the workspace is the home directory.
- **API keys stay in the agent process.** On Linux, commands can no longer read the agent's environment from `/proc/<pid>/environ`.
- **No reaching outside services from the sandbox.** Abstract unix sockets and signals to processes outside the sandbox are blocked (Landlock ABI 6+). On macOS, outbound localhost and DNS through mDNSResponder are blocked.
- **"Always" approvals are narrower.**
  - For simple commands, *always* covers the same program.
  - Compound commands and wrappers (`cd … &&`, `sudo`, `sh -c`, `xargs` …) are approved verbatim.
  - Network access is approved per exact command.
  - A write outside the workspace or into git internals is approved per file.
- **Approval prompts are clearer.** They show the whole command, wrapped and stripped of control characters. They accept an answer only after a pause, so text you are typing cannot answer them.
- **More risky commands need approval.** This now includes any tmux or screen command, docker with global flags, `--unix-socket`, `busctl`, `dbus-send`, `systemctl` and `rm --recursive/--force`. `.GIT` is treated as `.git`.
- **Git hooks outside `.git` are protected.** The git guard follows `core.hooksPath` (Husky, lefthook) and included config files, and edits to them need approval.
- **No leftover processes.** Processes a command leaves behind are stopped when it returns (servers belong in background jobs).

### Reliability

- **No broken sessions after a cut-off reply or a model switch.** Anthropic requests stay valid when:
  - a reply is cut off at the output limit (thinking and text are kept, the cut-off tool call is dropped);
  - a fallback model or `/model` switch happens mid-round (thinking pauses for that round);
  - a partial reply was only whitespace;
  - tool-call ids came from another provider.
- **Prompt-too-long recovers.** It triggers compaction and one retry. The size estimate now counts replayed blocks, reasoning and tool schemas, and elided arguments really shrink the request.
- **`max_tokens` and thinking budgets respect each model's output limit.**
- **Other provider fixes:**
  - OpenAI-compatible providers: tool calls streamed without an index are kept apart, and reasoning is replayed in the field the model used.
  - Azure and GitHub Models get `max_completion_tokens`.
  - Error chunks with a 429 or 5xx code are retried.
- **Shared state is safe with several Agentium processes at once.** Config, auth, MCP OAuth tokens, memory and checkpoints use cross-process locks and atomic writes. MCP refresh tokens are never spent twice, and memory decisions are never lost.
- **Undo reverts only its own turn.** Files you created or edited afterwards are kept. Unusual file names are handled, a stale git lock no longer disables checkpoints, and checkpoint history is trimmed.
- **MCP calls never hang.** A reply stream that ends without a reply fails the call, and calls time out after 10 minutes. The legacy SSE transport sends OAuth tokens.
- **LSP diagnostics are current.** Diagnostics for an older version of a file are ignored.
- **Storage stays bounded.** The journal is pruned after a year and sessions are pruned by count or age.
- **ACP:**
  - editor-supplied values are used literally;
  - idle sessions are closed (at most 8);
  - creating a session no longer blocks the others.

### Terminal

- **Terminal state is always restored.** Raw mode ends on SIGTERM, SIGHUP, a crash or any exit.
- **Type-ahead handles pastes.** A pasted block stays one message, and tabs and CRLF are handled.
- **No screen corruption.** The live area never grows taller than the screen, menus and the key prompt never wrap, and the repeat counter cannot overwrite your prompt.
- **Durations exclude approval waits.** Step times no longer include time spent waiting for your approval.
- **Sub-agent tokens and cost count in each turn's summary.**
- **`/login` checks a new key before saving it,** so a rejected key never replaces one that works.
- **`agentium update` is safer.** It installs through a synced atomic swap with rollback, and refuses to replace a package-manager install.

### Install

```sh
agentium update
# or
curl -fsSL https://raw.githubusercontent.com/TegarTheGreat/Agentium/main/install.sh | sh
```

## [0.13.0] - 2026-09-24

Fixes found by using Agentium on real tasks with a live model: building and serving a web app, installing packages, and answering questions about a large codebase.

### Added

- **`agentium update`** installs the latest release in place (checksum verified). The session banner mentions a newer release; the check runs in the background at most once a day, and `AGENTIUM_NO_UPDATE_CHECK=1` turns it off.
- **Remote sessions:** over SSH, the agent gives URLs with the server's address (`http://<ip>:PORT`) instead of `localhost`.
- On exit, Agentium lists the background jobs it stops, such as a dev server.

### Changed

- **Local servers work in the sandbox.** Commands may listen on any port. Ports that something on this machine listens on (a dev server, a local database) can be reached without network approval. Outbound connections elsewhere, and to common remote ports (22, 80, 443 and similar), still need `net`.
- **"Always" is scoped.** Answering *always* approves the same program (`npm`), all file changes, or the same host for the rest of the session, instead of switching the whole session to yolo mode.
- **Word wrap:** replies wrap at word boundaries, list items and quotes keep their indent, and code blocks are never wrapped.
- Long prompts are shown in full after Enter. Multi-line commands show their first line and a line count. Approval prompts no longer repeat `cd <workspace> &&`.
- An interrupted step shows as *interrupted*, and the model is told the user stopped it.
- The model writes files with `edit` rather than shell heredocs, and runs servers as background jobs and reports their URL.
- The installer's closing hint now points to the in-app setup.

### Install

```sh
agentium update
# or
curl -fsSL https://raw.githubusercontent.com/TegarTheGreat/Agentium/main/install.sh | sh
```

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

[0.14.0]: https://github.com/TegarTheGreat/Agentium/releases/tag/v0.14.0
[0.13.0]: https://github.com/TegarTheGreat/Agentium/releases/tag/v0.13.0
[0.12.0]: https://github.com/TegarTheGreat/Agentium/releases/tag/v0.12.0
[0.11.0]: https://github.com/TegarTheGreat/Agentium/releases/tag/v0.11.0
