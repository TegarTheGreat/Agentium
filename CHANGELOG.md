# Changelog

All notable changes to Agentium are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

## [0.17.0] - 2026-09-25

### Added

- **Sessions you can name and branch.** `/rename` names a conversation, `/resume` opens a picker (or takes a number or a name), and `/fork` continues in a copy while the original stays resumable. Named sessions are never pruned.
- **Built-in prompts:** `/init` writes `AGENTS.md` for the repository, `/review` reviews the current changes, `/commit` commits them in the repository's style, and `/pr` pushes the branch and opens a pull request.
- **Your own commands.** Markdown files in `.agentium/commands/` or `.claude/commands/` (in the project or your home folder) become `/name` commands, and `$ARGUMENTS` is replaced by what follows. They cannot replace a built-in command.
- **More hooks.**
  - `pre_tool` sees each tool call as JSON and can block it (exit code 2); its reason goes to the model.
  - `user_prompt` adds context to a message or stops it.
  - `session_start` adds context to the first message.
  - Hooks come only from your own config, never from a repository.
- **Approvals you keep.** `p` at an approval keeps it for the project. It is stored in Agentium's data folder, never in the repository. `/permissions` lists what runs without asking and revokes it.
- **`/doctor`** checks the model and key (with a live call), updates, git, ripgrep, the editor, the sandbox, the terminal, project instructions and MCP servers. **`/mcp`** shows each server's state, tools, errors and log. **`/tools`** lists what the agent can use.
- **Suggested next message.** After a reply, a guess at your next message shows dimmed in the empty composer, and `Tab` takes it. It is on for inexpensive models or when `fast_model` is set; `"suggest"` turns it on or off.
- **Status line.** Shows the git branch, commits ahead and changed files. `"status_line": "<command>"` shows your own line instead.
- **Conversation viewer.** In `Ctrl-O`, `t` shows the whole conversation, `/` searches it (`n`/`N`), `[` and `]` jump between your messages, and `e` opens it in `$EDITOR`.
- **Editing comforts.**
  - `Ctrl-K`, `Ctrl-U` and `Ctrl-W` cut; `Ctrl-Y` pastes back; `Alt-D` cuts the next word.
  - `Ctrl-_` undoes.
  - `Ctrl-S` puts a draft aside and brings it back.
  - `↑`/`↓` move between the lines of a long message before going through history.
  - `Alt-P` and `Alt-T` open the model and effort pickers.
- **`--worktree NAME`** runs the session in its own git worktree on branch `agentium/NAME`, so several sessions can work on one repository at once. The worktree lives in Agentium's data folder. On exit it is removed if nothing changed, or kept with the commands to merge or remove it.
- **`subagent_model`** runs sub-agents on another (usually cheaper) model, with its own price, context size and your reasoning effort.

### Changed

- **Colors over SSH.** Agentium asks the terminal whether it can show 24-bit color, since `COLORTERM` rarely survives SSH. Remote sessions now get the full palette instead of the 256-color fallback.
- **The turn receipt shows what was actually spent**, including sub-agents on their own model.
- **Compaction keeps your two latest requests word for word**, without recalled memory or hook output.

### Fixed

- A server started through `npx` or a wrapper script no longer hangs Agentium's exit, and its log is capped at 2 MiB.
- A hook that leaves a child process running no longer blocks the turn past its timeout.
- Branch names and `status_line` output cannot send control sequences to the terminal.
- The status line's git check never takes `.git/index.lock` from your own git commands.
- In a linked git worktree, the sandbox lets commands commit (the repository's shared `.git` is outside the worktree).

## [0.16.1] - 2026-09-25

### Fixed

- **A new project no longer inherits another one's memory.**
  - A git repository at the home directory or the filesystem root (dotfiles, a container image) no longer makes every folder below it one project. Before, all of them shared the same notes, instructions and skills.
  - `@prefer`, which writes preferences for every project, is now reserved for how you like to work, never facts about one project. The prompt labels the two kinds of memory clearly.
- **Agentium knows itself.** It knows where its settings, keys, instructions, memory, skills, sessions and checkpoints are, which commands you have, and the exact formats for MCP servers, skills, hooks and providers. It answers from these instead of guessing.

### Added

- **`/memory`** shows what Agentium remembers (preferences for every project, this project's notes and decisions) and where each is kept. `/memory edit` and `/memory edit project` open them in `$EDITOR`.

## [0.16.0] - 2026-09-25

### Added

- **Easier to read.**
  - Every kind of step has its own colored label: Read, Search, Edit, Run, Web, Staff, Plan.
  - Command output sits in a gutter under its step.
  - Agentium's own notes are marked ℹ and set apart from the model's words.
  - A sub-agent's steps carry its name and color.
- **Steer a running turn.**
  - Enter sends your message to the agent at its next step, as in Claude Code, Codex and Amp.
  - Tab queues it for after the turn.
  - ↑ takes a pending message back.
- **More commands, from the other agent CLIs:**
  - `!command` runs a shell command yourself; the agent sees the output with your next message.
  - `/diff` shows the working tree's changes.
  - `/context` shows what fills the context window.
  - `/compact` summarizes the older conversation.
  - `/btw` asks a side question without adding it to the conversation.
  - `/theme` switches the palette and saves the choice.
- **`AGENTIUM_STREAM_PROGRESS_MINUTES`** raises the 10-minute limit on streams that send only keep-alives.

### Fixed

From two independent audits of the new interface:

- **Could freeze or crash:**
  - The screen could freeze for good on a malformed escape sequence in the output.
  - Ctrl-G with the suggestion popup open could crash.
- **Paste:** multi-line pastes were split into several messages in the full screen, because bracketed paste was never switched on there.
- **Terminal left unusable:**
  - Ctrl-C during startup or between turns left the terminal in the full screen.
  - SIGTERM left echo off.
  - A frame could be drawn onto the normal screen when quitting.
- **Approval previews:**
  - A preview of a whole-file write now shows what it deletes.
  - With two pending changes to one file, no preview is shown, since it could show the other change.
- **Refusals:**
  - Runs without a terminal no longer tell the model "the user declined".
  - A reason given for one refusal can no longer reach another.
- **Suggestions and startup input:**
  - Files with non-ASCII names completed to a quoted path.
  - Listing files could freeze typing in a huge repository.
  - The inline popup drew over the prompt at the bottom of the screen.
  - A slow terminal's color reply could appear as typed text.
- **Layout:**
  - Styled text was cut short. Among other things, the status line lost the model and mode on narrow terminals.
  - The composer stayed "busy" after a note printed between turns.
  - Input redrew wrongly after Ctrl-T or a resize.

## [0.15.0] - 2026-09-25

A new interface, designed from a study of Claude Code, Codex CLI, Gemini CLI, opencode, Crush, Amp, Cursor CLI and Mobbin references.

### Added

- **Full-screen workspace (Linux and macOS).**
  - Conversation on the left.
  - An ops panel on the right, toggled with `Ctrl-T`, with four parts:
    - **Office:** Agentium and its sub-agents work at pixel-art desks, each showing what they are doing.
    - **Plan:** the todo list, with progress.
    - **Changes:** files changed so far, with +/− counts.
    - **Context:** a meter of the context window.
  - A composer whose border shows what is running.
  - A status line with model, mode, tokens and cost.
  - Scroll with PgUp/PgDn or the wheel. On exit, the conversation is printed to the terminal.
  - `--classic` or `"ui": "classic"` keeps the inline interface.
- **Night Shift look, in both interfaces.**
  - A truecolor palette that follows the terminal's light or dark background (`"theme"` overrides it).
  - Replies open with ◆, and your messages carry a ▌ bar.
  - Edits show diff cards with line numbers, and commands show the end of their output.
  - Code blocks are drawn as cards, and each turn ends with a receipt.
- **Typing comforts.**
  - `/` pops up commands with what they do.
  - `@` fuzzy-finds project files.
  - Big pastes become chips.
  - `Ctrl-J`/`Shift-Enter` insert a new line.
  - `Ctrl-R` searches earlier messages.
  - `Ctrl-G` opens `$EDITOR`.
  - `?` lists commands and keys.
- **Control.**
  - `Esc` stops a turn.
  - `Esc Esc` or `/rewind` reverts file changes back to a chosen turn.
  - `Shift-Tab` cycles approval modes.
  - `Ctrl-O` shows the full output of recent steps.
  - `/copy` puts the last reply on the clipboard, via OSC 52 as well, so it works over SSH.
- **Approvals.**
  - File changes show their diff before you answer.
  - A new `t` answer declines and tells Agentium why; the model gets your words instead of a bare "denied".
- **Attention.**
  - The terminal title shows working, needs you or ready.
  - A bell or desktop notification says when Agentium needs an answer or has finished a long turn.
- **Sub-agent tasks take a short title**, shown instead of the start of the prompt.

### Fixed

- **Sub-agents that run out of steps still report.** Before, a sub-agent that hit its limit returned nothing, and its work was invisible to the main agent. It is now asked for a report of what is done and what is left. The limit is also higher: 60 steps, up from 40.
- **`agentium update` retries** on network errors and server errors (such as 502) before giving up.

## [0.14.2] - 2026-09-24

### Added

- **`/update` inside Agentium.** It installs the latest release without leaving the session. Typing `agentium update` at the prompt does the same, instead of sending it to the model.

### Fixed

- **A turn no longer hangs on "Thinking…" forever.** Some providers (DeepSeek among them) keep a queued request open by sending only keep-alive comments. Those used to reset the stall timer, so a turn could wait for hours. A stream that sends nothing but keep-alives or pings for 10 minutes is now dropped and retried.
- **Repeated checks on a background job collapse into one line.** They show as `Bash job 17 ×5 54.1s` instead of a new line for every check.

## [0.14.1] - 2026-09-24

Fixes from a second review of the v0.14.0 security code.

### Security

- **More network escapes closed (Linux).** Without network access, the seccomp filter now also blocks:
  - TCP Fast Open (data sent along with the connect);
  - MPTCP and any non-TCP protocol on INET sockets;
  - `AF_PACKET` and `SOCK_PACKET` sockets.
- **Unsupported architectures are reported.** Where the filter is unavailable, the sandbox status says UDP is not blocked.
- **Credentials stay protected through symlinks.** A symlinked `~/.ssh` or `~/.config` (dotfile managers), `AGENTIUM_HOME`, or a home directory behind a symlink no longer exposes them. Directories that hold a credential can still be listed.
- **Sandboxed commands cannot read Agentium's lock files, checkpoints or sessions.** Locks moved to `~/.agentium/locks`, so a command can no longer hold a lock to stall the agent.
- **"Always" is narrower again:**
  - Broad scope only for a bare program in ask mode.
  - `./go`, `/tmp/x/go` and `VAR=… cmd` are approved verbatim.
  - Commands flagged for their own risk (force push, `rm -rf`) are approved verbatim, so approving `git status` never approves `git push --force`.
- **Typed text can't answer an approval prompt by accident.** A key counts as an answer only on its own, with a pause before and nothing right after, so words you were typing cannot answer it. Esc and Ctrl-C always answer no, and ignored keys show a hint.
- **An MCP token refresh gives up within 20 seconds.** Waiting processes therefore never retry with the same single-use refresh token.

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

[0.14.1]: https://github.com/TegarTheGreat/Agentium/releases/tag/v0.14.1
[0.14.0]: https://github.com/TegarTheGreat/Agentium/releases/tag/v0.14.0
[0.13.0]: https://github.com/TegarTheGreat/Agentium/releases/tag/v0.13.0
[0.12.0]: https://github.com/TegarTheGreat/Agentium/releases/tag/v0.12.0
[0.11.0]: https://github.com/TegarTheGreat/Agentium/releases/tag/v0.11.0
