# Agentium

A fast, minimal coding agent for the terminal. It ships as one small static binary with seven tools and a fixed prompt of about 1.1k tokens, and it works with any model provider. It also has an OS sandbox, undo, cross-session memory, and verification built in.

Example session:

```
$ agentium "the tests in ./calc fail, fix them"
› bash go test ./calc/...
› read calc/add.go
› edit calc/add.go
› bash go test ./calc/...
Fixed Add: it subtracted instead of adding. go test ./calc/... passes.
· 4 turns · 4 tools · in 6.1k (cached 4.8k) · out 180 · 5.2s · $0.0213
```

## Why

Popular agents are slow and wordy. Claude Code sends about 33k tokens of prompt and tool definitions before your first word, and OpenCode sends about 7k and can grow to gigabytes of RAM. The 2026 data ([docs/RESEARCH.md](docs/RESEARCH.md), [docs/GAPS.md](docs/GAPS.md)) points the same way. Minimal harnesses match or beat heavy ones. What moves scores is deterministic machinery around the model: verification, lint-gated edits, sandboxing, and stuck detection. Agentium is built from those findings.

| | Agentium (measured, `agentium bench`) |
|---|---|
| Binary | 8.8 MB, static, no runtime |
| Startup | ~5 ms |
| Memory | ~10 MB RSS |
| Prompt + tool schemas | ~1.1k tokens (memory rules, `todo` and `task` included) |
| Tools | `read` `edit` `bash` `search` `fetch` `todo` `task` (+ MCP tools if configured) |

## What it does

**Fast and to the point**
- Terse by default: no preamble or recap, a plan only for big tasks, and "done" means verified.
- Tool calls in the same turn run in parallel. Edits to the same file are serialized.
- The system prompt and tool list are fixed for the session, and Anthropic cache breakpoints are set automatically. Cost is shown per turn.
- Output is clipped head+tail with a hint on how to see the rest.
- **Code map.** A per-project index of definitions and identifiers, cached on disk and refreshed only for changed files (Go's standard library, ~13k files: 3.3 s the first time, under 0.5 s after).
  - `read` on a directory shows a gitignore-aware tree.
  - `read {outline:true}` shows a file's definitions with line numbers. On a large directory it gives a ranked repo map (PageRank over "file uses what another file defines", as in Aider, favoring files you touched) within ~2k tokens.
  - `search {symbol:"Type.Method"}` finds definitions; `search {refs:"Name"}` finds uses with the enclosing function (`main.py:5 [in App.run]`).
  - Go is parsed exactly; Python, JS/TS, Rust, Java, Kotlin, C#, Swift, PHP, C/C++, Ruby and more use line patterns that handle comments, template strings, docstrings and Rust lifetimes.
  - The map is on demand, never pushed into the prompt: 2026 studies found always-injected context and imprecise retrieval neutral or harmful.
- Markdown replies are rendered in the terminal while they stream (bold, `code`, bullets, fenced code untouched). Pipes get raw Markdown.

**Reliable**
- **Lint-gated edits.** An edit that would break a file that parsed before (Go, JSON, Python, shell, JS) is rejected and the file stays untouched. The edit tool tolerates CRLF, trailing-whitespace and indentation differences, and re-indents to the file's style.
- **Verify before finishing.** If the model changed code and ran no build or test afterwards, it is asked once to verify.
- **Stuck detector.** The same call with the same result 3× gets a warning; 5× stops the run.
- **Thinks harder only when needed.** Work starts at the configured reasoning effort. After three failing tool batches in a row, or a stuck warning, effort goes up one level (at most twice per turn), and the next turn starts at the configured level again.
- **Atomic edits.** Every edit goes to a temporary file, is synced, renamed into place and read back to confirm. A crash or a full disk leaves the old file or the new one, never half of each.
- **No duplicate reads.** Re-reading an unchanged file returns a pointer to the earlier result instead of the same text again.
- **Stream robustness.** Truncated replies are continued. Stalled or broken streams are retried, honouring `Retry-After`. A fallback model chain takes over when the main model is down.
- **Context management.** Each model's context window is known. Old tool output and old edit payloads are elided at 55% of the window, and older turns are summarized at 85%.
- **Best of N.** `--best-of N --check "make test"` runs N attempts in parallel git worktrees and applies the passing one with the smallest diff.

**Safe**
- **OS sandbox for shell commands.** Landlock on Linux, `sandbox-exec` on macOS.
  - Only the workspace, temp dirs and build caches are writable. Directories on `PATH` and tool init-script directories are not.
  - TCP is blocked unless a command asks for `net` and you approve it. Kernels that cannot block the network (Linux < 6.7) are reported at startup.
- **Deterministic approval gate.** Risky commands (`rm -rf`, force push, `sudo`, `curl | sh`, deleting via scripts, touching credentials) and reads of SSH keys or cloud credentials need approval. Modes are `ask`, `auto` (default) and `yolo`.
- **No credential leaks.**
  - Shell commands, hooks and MCP servers run without credential-looking environment variables (`sandbox.pass_env` allows specific ones).
  - `fetch` refuses URLs that carry a secret, and refuses localhost, private networks and cloud metadata addresses, including on redirects.
  - `.env` files need approval to read.
- **`.git` guard.** A command that adds a git setting or hook that runs programs (`core.fsmonitor`, `hooksPath`, filters, `!` aliases) is undone and reported, since git would run it later outside the sandbox.
- Commands that hand work to a process outside the sandbox (tmux, docker, systemd-run, at, osascript) count as risky.
- **Checkpoints.** Every turn that changes something is snapshotted in a shadow git repository (your `.git` is untouched). `/undo` or `agentium undo` reverts it, including changes made by shell commands.
- Edits refuse to blindly overwrite an existing file, or to write a file changed on disk since the model read it.
- **Plan mode.** `--plan` or `/plan`: the agent investigates and answers with a plan, and cannot change anything. With the sandbox the workspace is mounted read-only, so any non-destructive command can still run; without it only read-only commands pass. `/go` carries the plan out.

**Remembers**
- **Long-term.**
  - `USER.md` (your preferences) and `MEMORY.md` (per project, shared by every subdirectory of the repo) are small files injected as a frozen snapshot, together with active decisions from `DECISIONS.md` (with `supersedes`).
  - The model saves memory with plain lines in its reply: `@remember`, `@prefer`, `@decide … (supersedes D-003)`, `@forget`.
  - Each entry is dated and cites the files it mentions. Notes whose files are gone, or that nobody confirmed for four months, are hidden and listed for `agentium tidy`. A near-duplicate replaces the older note instead of piling up.
  - When a file is full, the weakest note is forgotten (invalid first, then uncited, then least recently confirmed) and moved to the journal, where recall can still find it.
  - **Learns from mistakes.** When a failing command passes later, the harness records what failed and what fixed it. A lesson whose failure recurs across turns is promoted to `MEMORY.md`.
  - Secrets are redacted and injection-like text is refused. Memory written in a turn that read web pages or MCP output is held for review.
- **Recall.**
  - A local BM25 index over decisions, a per-turn journal (including the errors hit) and past sessions.
  - At most two precise snippets are pushed before a turn.
  - The model can look up more with `search {memory:"…"}`, e.g. "have we seen this error before".
- **Working memory.** The harness keeps a ledger:
  - files read and changed;
  - recent commands with exit codes;
  - the latest unresolved error;
  - a `todo` list for multi-step work.

  It survives compaction verbatim, so the summary never has to reconstruct it.

**Any provider**
- Two wire protocols (OpenAI Chat Completions and Anthropic Messages) plus Bedrock and Vertex clients cover the built-ins and every compatible provider in the [models.dev](https://models.dev) registry (180+).
- Built in: `anthropic`, `openai`, `gemini`, `openrouter`, `groq`, `cerebras`, `deepseek`, `xai`, `mistral`, `together`, `fireworks`, `moonshot`, `zai`, `github` (GitHub Models), `azure`, `bedrock`, `vertex`, `ollama`, `lmstudio`.
- **Images.** `read` on a PNG/JPEG/GIF/WebP shows it to the model, and `@screenshot.png` in a prompt attaches it, for models that accept images (from models.dev, else known multimodal families).
- **Reasoning.** Adaptive thinking and `--effort` on Claude (signed thinking blocks are replayed exactly as the API requires), `reasoning_effort` on OpenAI-compatible models, reasoning replay for DeepSeek-style models, and Gemini thought signatures. `--fast` uses Claude's fast mode where available.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/TegarTheGreat/Agentium/main/install.sh | sh
# or
go install github.com/tegarthegreat/agentium/cmd/agentium@latest
# or from a clone
make build
```

**Extensible**
- **Skills.** A skill is a folder with a `SKILL.md` (the format Claude Code and Codex use). Skills in `~/.agentium/skills`, `~/.claude/skills` and each `.agentium/skills` or `.claude/skills` of the project are listed in one line each; the model reads a skill when a task matches it, and `/name [task]` runs one directly.
- `agentium skills add <dir | git URL | owner/repo[#ref]>` fetches without running anything, pins the commit, lists bundled scripts, and installs only after you confirm. There is no marketplace to trust: you choose the source. Also `skills list|show|remove`.
- **MCP servers**, local (stdio) or remote (`"url"` with `"headers"`, Streamable HTTP or `"type": "sse"`). Their tools appear as `mcp__server__tool`, and a stdio server's stderr goes to `~/.agentium/logs/`.
- **Hooks**: `post_edit` and `stop`.
- **Editors:** `agentium acp` speaks the Agent Client Protocol, so Zed, JetBrains and other ACP clients can use Agentium. Tool calls, plans and permission prompts appear in the editor.

**Works like a team**
- **Sub-agents.** `task {prompt, explore?}` gives a self-contained job to a sub-agent with a fresh context, and only its report comes back. Several run in parallel; `explore` makes one read-only.
- **Background jobs.** `bash {background:true}` keeps dev servers, watchers and REPLs running. The model reads new output, sends input and stops them by job id. Jobs end with the session.
- **Language servers.** After each edit, the project's language server reports the file's errors: type errors, bad imports, calls to things that do not exist. Supported: gopls, pyright, typescript-language-server, rust-analyzer and clangd, when installed.
- **Web search.** `fetch {search:"…"}` uses Brave or Tavily when you have a key, otherwise DuckDuckGo.

## Use

```sh
export ANTHROPIC_API_KEY=...          # or OPENAI_API_KEY, GEMINI_API_KEY, OPENROUTER_API_KEY, ...
agentium                              # interactive
agentium "add a --json flag to main.go"
git diff | agentium -p "review this"  # stdin works too
agentium -c "now update the docs"     # continue the last session here
agentium -m ollama/qwen3-coder        # local model
agentium --effort xhigh --fast "…"    # more thinking, faster output
agentium --json "…"                   # JSON Lines events for CI; exit 0/1/2/130
agentium --best-of 3 --check "go test ./..." "fix the flaky test"
agentium --max-cost 0.50 "…"          # stop at 50 cents
agentium --plan "how would we add OAuth?"   # read-only; answers with a plan
agentium "why does @screenshot.png look broken?"
```

In a session:
- **Commands:** `/plan`, `/go`, `/<skill> [task]`, `/skills`, `/undo`, `/sessions`, `/resume <n>`, `/clear`, `/model <provider/model>`, `/mode ask|auto|yolo|plan`, `/usage`, `/exit`.
- **Keys:** ↑/↓ history, Ctrl-A/E/U/K/W. Pastes keep their newlines. End a line with `\` for a newline. Ctrl-C interrupts a running turn.

Other commands: `agentium providers`, `agentium models [provider] [--refresh]`, `agentium login [--oauth] <provider>`, `agentium logout <provider>`, `agentium undo`, `agentium tidy`, `agentium skills`, `agentium bench [-m model]`. Set `AGENTIUM_RAW=1` (or `NO_COLOR`) for unrendered output.

## Login

```sh
agentium login openai                # API key → OS keychain (or ~/.agentium/auth.json, 0600)
agentium login --oauth openrouter    # browser login (OpenRouter's official PKCE flow)
gh auth login                        # then -m github/<model> works without a key
```

- **Bedrock:** set `AWS_BEARER_TOKEN_BEDROCK`, or set `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` together with `AWS_REGION`.
- **Vertex:** set `GOOGLE_CLOUD_PROJECT` (and optionally `CLOUD_ML_REGION`), then run `gcloud auth application-default login`.
- **Azure:** set `AZURE_OPENAI_ENDPOINT` and `AZURE_OPENAI_API_KEY`.

Subscription logins for Anthropic and Google are not offered, because their terms prohibit use in third-party tools. ChatGPT and Copilot subscription logins need an official client registration and are not included yet.

## Configure

`~/.agentium/config.json` (every field optional):

```json
{
  "model": "anthropic/claude-sonnet-5",
  "fast_model": "anthropic/claude-haiku-4-5",
  "fallback": ["openrouter/anthropic/claude-sonnet-5"],
  "effort": "high",
  "mode": "auto",
  "sandbox": { "network": "ask", "write": ["~/data"] },
  "hooks": { "post_edit": ["gofmt -w {path}"], "stop": ["notify-send agentium done"] },
  "mcp": { "github": { "command": "github-mcp-server", "args": ["stdio"], "env": { "GITHUB_TOKEN": "$GITHUB_TOKEN" } } },
  "providers": { "corp": { "protocol": "openai", "base_url": "https://llm.corp.example/v1", "api_key_env": "CORP_KEY" } },
  "memory": true, "checkpoints": true, "verify": true, "fetch_private": false
}
```

- **Project instructions** come from `AGENTS.md` (or `CLAUDE.md`), read from the repo root down to the current directory, plus `~/.agentium/AGENTS.md`.
- **Memory** lives in `~/.agentium/USER.md` and `~/.agentium/projects/<id>/`.

## Benchmark

```sh
agentium bench                                 # binary size, startup, RSS, prompt overhead
agentium bench -m anthropic/claude-sonnet-5    # + 5 live tasks: pass rate, turns, tokens, time
```

For Terminal-Bench 2.x via Harbor, see [bench/terminalbench](bench/terminalbench/README.md). It compares harnesses on the same model.

## Status

v0.10.0. Linux and macOS are fully supported. On Windows, commands run in Git Bash (or PowerShell) without an OS sandbox. Design principles (brain, natural laws, physics mapped to concrete mechanisms): [docs/DESIGN.md](docs/DESIGN.md). Everything above is implemented and covered by unit and end-to-end tests: fake model servers for every protocol, a fake MCP server, and real pty tests for the line editor. Landlock confinement is tested on Linux, and CI runs Linux and macOS. **Not yet exercised against real model APIs or a real Terminal-Bench run.** Please report what breaks.

## Develop

```sh
make test    # go vet + go test -race
make cross   # linux/darwin/windows binaries in dist/
```
