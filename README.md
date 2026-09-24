<div align="center">

# Agentium

**A fast, minimal coding agent for the terminal.**

One static binary · any model provider · sandboxed by default

[![CI](https://github.com/TegarTheGreat/Agentium/actions/workflows/ci.yml/badge.svg)](https://github.com/TegarTheGreat/Agentium/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.24-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

</div>

---

Agentium reads, edits, runs and verifies code in your repository. It keeps its prompt small and its output short, and puts the reliability work into the harness: sandboxed commands, edits that cannot break a file, verification before it reports done, undo for every turn, and memory that carries across sessions.

```console
$ agentium "the tests in ./calc fail, fix them"
› bash go test ./calc/...
› read calc/add.go
› edit calc/add.go
› bash go test ./calc/...
Fixed Add: it subtracted instead of adding. go test ./calc/... passes.
· 4 turns · 4 tools · in 6.1k (cached 4.8k) · out 180 · 5.2s · $0.0213
```

## Contents

- [Highlights](#highlights)
- [Installation](#installation)
- [Quick start](#quick-start)
- [Providers and authentication](#providers-and-authentication)
- [Configuration](#configuration)
- [Tools](#tools)
- [Safety](#safety)
- [Reliability](#reliability)
- [Memory](#memory)
- [Extending Agentium](#extending-agentium)
- [Platform support](#platform-support)
- [Benchmarking](#benchmarking)
- [Development](#development)
- [License](#license)

## Highlights

| | |
|---|---|
| **Small** | 8.8 MB static binary, no runtime dependencies |
| **Fast** | ~6 ms startup, ~10 MB memory |
| **Lean prompt** | ~1k tokens of system prompt and tool schemas, fixed for the session so it stays cached |
| **Any provider** | 19 built-in providers plus every compatible provider in the [models.dev](https://models.dev) registry |
| **Sandboxed** | Landlock on Linux, `sandbox-exec` on macOS; network off unless approved |
| **Recoverable** | Every turn is checkpointed; `/undo` reverts it, including changes made by shell commands |
| **Remembers** | Project memory, decisions and lessons learned from past errors, recalled only when relevant |
| **Integrates** | MCP (stdio and HTTP, with OAuth), Agent Client Protocol for editors, `SKILL.md` skills, hooks |

Figures are measured with `agentium bench` on Linux amd64.

## Installation

**With Go** (1.24 or newer):

```sh
go install github.com/tegarthegreat/agentium/cmd/agentium@latest
```

**Prebuilt binary** (Linux and macOS; downloads the latest [release](https://github.com/TegarTheGreat/Agentium/releases)):

```sh
curl -fsSL https://raw.githubusercontent.com/TegarTheGreat/Agentium/main/install.sh | sh
```

**From source:**

```sh
git clone https://github.com/TegarTheGreat/Agentium && cd Agentium
make build
```

## Quick start

```sh
agentium                            # first run: pick a provider, paste a key, choose a model
agentium "add a --json flag to main.go"
```

On first run Agentium asks for a provider and key, then saves them. Environment variables such as `ANTHROPIC_API_KEY` or `DEEPSEEK_API_KEY` also work. While it works you see each step live: running commands show their latest output, and anything you type is queued for when the current turn finishes.

### Common invocations

| Command | Purpose |
|---|---|
| `agentium "task"` | Run a single task and exit |
| `git diff \| agentium -p "review this"` | Use stdin as context |
| `agentium -c "now update the docs"` | Continue the last session in this directory |
| `agentium -m ollama/qwen3-coder` | Use a specific model, including local ones |
| `agentium --plan "how would we add OAuth?"` | Investigate read-only and answer with a plan |
| `agentium --effort xhigh --fast "…"` | More reasoning, faster output where supported |
| `agentium --json "…"` | JSON Lines events for CI (exit codes 0/1/2/130) |
| `agentium --max-cost 0.50 "…"` | Stop once the session has cost $0.50 |
| `agentium --best-of 3 --check "go test ./..." "…"` | Run 3 attempts in parallel worktrees, keep the passing one with the smallest diff |
| `agentium "why does @screenshot.png look broken?"` | Attach an image to the prompt |

### Interactive session

| | |
|---|---|
| **Commands** | `/help` `/model` `/login` `/logout` `/mode` `/effort` `/config` `/plan` `/go` `/undo` `/sessions` `/resume <n>` `/clear` `/usage` `/skills` `/<skill> [task]` `/exit` |
| **Keys** | ↑/↓ history · Ctrl-A/E/U/K/W · pastes keep their newlines · end a line with `\` for a newline · Ctrl-C interrupts a running turn · typing during a turn queues a message |

### Subcommands

| Command | Purpose |
|---|---|
| `agentium login [--oauth] <provider>` / `logout <provider>` | Manage credentials |
| `agentium providers` | List providers and credential status |
| `agentium models [provider] [--refresh]` | List models with context size and price |
| `agentium undo` | Revert the last turn's changes in this directory |
| `agentium tidy [--yes]` | Review and consolidate long-term memory |
| `agentium skills [list\|show\|add\|remove]` | Manage skills |
| `agentium mcp [list\|login\|logout <name>]` | Manage remote MCP servers and their login |
| `agentium acp [-m model]` | Serve the Agent Client Protocol on stdio |
| `agentium bench [-m model]` | Measure performance; with `-m`, run live tasks |
| `agentium update` | Update to the latest release |

Set `AGENTIUM_RAW=1` or `NO_COLOR` for unrendered output. Run `agentium --help` for every flag.

## Providers and authentication

Agentium speaks the OpenAI Chat Completions and Anthropic Messages protocols, with native clients for Bedrock, Vertex and Gemini.

**Built in:** `anthropic` · `openai` · `gemini` · `openrouter` · `groq` · `cerebras` · `deepseek` · `xai` · `mistral` · `together` · `fireworks` · `moonshot` · `zai` · `github` (GitHub Models) · `azure` · `bedrock` · `vertex` · `ollama` · `lmstudio`

```sh
agentium login openai                # API key, stored in the OS keychain (or ~/.agentium/auth.json, mode 0600)
agentium login --oauth openrouter    # browser login (PKCE)
gh auth login                        # enables -m github/<model> without a separate key
```

| Provider | Environment |
|---|---|
| Bedrock | `AWS_BEARER_TOKEN_BEDROCK`, or `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY` + `AWS_REGION` |
| Vertex | `GOOGLE_CLOUD_PROJECT` (optional `CLOUD_ML_REGION`), then `gcloud auth application-default login` |
| Azure OpenAI | `AZURE_OPENAI_ENDPOINT` + `AZURE_OPENAI_API_KEY` |

**Model features:**
- **Reasoning:** `--effort` maps to each provider's reasoning controls, and reasoning state is replayed as each API requires.
- **Images:** `read` on a PNG, JPEG, GIF or WebP shows it to vision-capable models, and `@file.png` in a prompt attaches it.
- **Fallback:** a fallback chain takes over when the main model is unavailable.

Subscription logins are offered only where the provider's terms allow third-party clients.

## Configuration

`~/.agentium/config.json` (every field is optional):

```json
{
  "model": "anthropic/claude-sonnet-5",
  "fast_model": "anthropic/claude-haiku-4-5",
  "fallback": ["openrouter/anthropic/claude-sonnet-5"],
  "effort": "high",
  "mode": "auto",
  "sandbox": { "network": "ask", "write": ["~/data"] },
  "hooks": { "post_edit": ["gofmt -w {path}"], "stop": ["notify-send agentium done"] },
  "mcp": {
    "github": { "command": "github-mcp-server", "args": ["stdio"], "env": { "GITHUB_TOKEN": "$GITHUB_TOKEN" } },
    "docs":   { "url": "https://mcp.example.com/mcp" }
  },
  "providers": {
    "corp": { "protocol": "openai", "base_url": "https://llm.corp.example/v1", "api_key_env": "CORP_KEY" }
  },
  "memory": true,
  "checkpoints": true,
  "verify": true,
  "fetch_private": false
}
```

| File | Purpose |
|---|---|
| `AGENTS.md` (or `CLAUDE.md`) | Project instructions, read from the repository root down to the current directory |
| `~/.agentium/AGENTS.md` | Personal instructions for every project |
| `~/.agentium/USER.md` | Your preferences, maintained by the agent |
| `~/.agentium/projects/<id>/` | Per-project memory, decisions and journal |

## Tools

The model has seven tools, plus any tools from configured MCP servers.

| Tool | What it does |
|---|---|
| `read` | Read files (streams large ones, rejects binary), show a directory as a gitignore-aware tree, outline a file's definitions, or produce a ranked map of a large codebase within ~2k tokens |
| `edit` | Create or change files: tolerant matching (CRLF, whitespace, indentation), lint-gated, atomic |
| `bash` | Run commands in the sandbox; background jobs with input, output and stop controls; pseudo-terminal mode for interactive programs |
| `search` | Text search, symbol definitions (`Type.Method`), references with their enclosing function, and past memory |
| `fetch` | Fetch a URL as text, or search the web (Brave, Tavily or DuckDuckGo) |
| `todo` | Keep a checklist for multi-step work |
| `task` | Hand a self-contained job to a sub-agent with a fresh context; several can run in parallel, optionally read-only |

**Code intelligence.**
- The code index is cached per project and refreshed only for changed files.
- Go is parsed exactly. Python, JavaScript/TypeScript, Rust, Java, Kotlin, C#, Swift, PHP, C/C++, Ruby and others use a scanner that understands comments, strings, docstrings and template literals.
- After each edit, the project's language server reports new errors. Supported: gopls, pyright, typescript-language-server, rust-analyzer, clangd.

## Safety

| Layer | Behavior |
|---|---|
| **OS sandbox** | Shell commands can write only to the workspace, temp directories and build caches, and cannot read credential files (`~/.ssh`, `~/.aws`, …). Servers may listen; outbound connections (localhost included) and, on Linux, UDP/DNS are blocked unless a command requests network access and you approve. |
| **Approval gate** | Risky actions need approval, for example `rm -rf`, force pushes, `sudo`, piping downloads to a shell, credential files and handing work to processes outside the sandbox. Modes: `ask` (every action), `auto` (risky only, default), `yolo` (never), `plan` (read-only). Answering *always* covers the same simple program, or exactly that command, file or host, for the session. |
| **Credential isolation** | Commands, hooks and MCP servers run without credential-like environment variables (`sandbox.pass_env` allows specific ones). `fetch` refuses URLs that carry secrets, and private or metadata addresses. `.env` files need approval to read. |
| **Repository guard** | Git settings or hooks that would run programs outside the sandbox are undone and reported. Edits inside `.git` need approval. |
| **Checkpoints** | Each turn is snapshotted in a shadow repository; your own `.git` is never touched. `/undo` reverts only the files that turn changed, keeping later edits. |
| **Write protection** | Existing files are never overwritten blindly, and a file changed on disk since it was read is not overwritten. |

## Reliability

- **Lint-gated edits:** an edit that would break a file that parsed before is rejected, and the file stays unchanged.
- **Atomic writes:** each edit is written to a temporary file, synced, renamed into place and read back.
- **Verification:** if code changed and nothing was built or tested afterwards, the model is asked to verify before finishing.
- **Stuck detection:** a repeated identical call and result triggers a warning, then stops the run.
- **Adaptive effort:** reasoning effort rises only after repeated failures, and resets on the next turn.
- **Resilient streaming:** truncated replies are continued; stalled streams are retried, honoring `Retry-After`.
- **Context management:** old tool output is elided at 55% of the context window, and older turns are summarized at 85%.

## Memory

| Scope | How it works |
|---|---|
| **Long-term** | `USER.md`, a per-project `MEMORY.md` and `DECISIONS.md`. The model records notes with `@remember`, `@prefer`, `@decide` and `@forget`. Entries are dated and cite the files they concern. |
| **Maintenance** | Near-duplicates replace older notes. Notes whose files are gone, or that nobody has confirmed for four months, are hidden and listed by `agentium tidy`. When memory is full, the weakest note moves to the journal. |
| **Learning** | When a failing command later passes, the failure and its fix are recorded. A lesson whose failure recurs is promoted to project memory. |
| **Recall** | A local BM25 index covers decisions, the journal and past sessions. At most two precise matches are added before a turn; the model can search for more. |
| **Working memory** | Files read and changed, recent commands with exit codes, the latest unresolved error and the todo list survive context compaction verbatim. |
| **Hygiene** | Secrets are redacted, and instruction-like text is refused. Notes written after reading web or MCP content are held for review. |

## Extending Agentium

**Skills.**
- A skill is a directory containing a `SKILL.md` in the Agent Skills format.
- Skills are loaded from `~/.agentium/skills`, `~/.claude/skills`, and the project's `.agentium/skills` and `.claude/skills`.
- Only a one-line index enters the prompt. `/name [task]` runs a skill directly.

```sh
agentium skills add owner/repo#ref     # also a local directory or git URL
```

Installation fetches without executing anything, pins the commit and lists bundled scripts, then asks for confirmation.

**MCP servers.**
- Local servers use stdio. Remote servers use Streamable HTTP or SSE (`"url"`, optional `"headers"`).
- Tools appear as `mcp__<server>__<tool>`.
- Servers that require OAuth are supported: discovery, dynamic client registration and PKCE.

```sh
agentium mcp login docs   # opens the browser; tokens are stored and refreshed automatically
agentium mcp list         # servers and login status
```

**Hooks.** `post_edit` runs after each edit (`{path}` is substituted). `stop` runs when a task finishes.

**Editors.** `agentium acp` implements the [Agent Client Protocol](https://agentclientprotocol.com), so ACP clients such as Zed and JetBrains IDEs can use Agentium. Tool calls, plans and permission prompts appear in the editor.

## Platform support

| Platform | Status |
|---|---|
| **Linux** | Full support. Sandbox via Landlock; network isolation requires kernel 6.7 or newer, and a warning is shown otherwise. |
| **macOS** | Full support. Sandbox via `sandbox-exec`. |
| **Windows** | Supported without an OS sandbox. Commands run in Git Bash, falling back to PowerShell. Child processes are contained in a Job Object and end with Agentium. Use `ask` mode, since the approval gate is the only guard. |

## Benchmarking

```sh
agentium bench                                # binary size, startup time, memory, prompt overhead
agentium bench -m anthropic/claude-sonnet-5   # adds live tasks: pass rate, turns, tokens, time
```

A [Terminal-Bench 2.x adapter](bench/terminalbench/README.md) for Harbor is included.

## Development

```sh
make build   # build ./agentium
make test    # go vet + go test -race
make cross   # binaries for Linux, macOS and Windows in dist/
```

- **Tests:** unit and end-to-end suites run against fake model servers for every protocol, a fake MCP server and real pseudo-terminals.
- **CI:** runs on Linux, macOS and Windows, and validates the ACP server against the official SDK schemas.
- **Releases:** built with GoReleaser from a `v*` tag, or from **Actions → release → Run workflow**. Notes come from [CHANGELOG.md](CHANGELOG.md).

Design notes live in [`docs/`](docs): [DESIGN.md](docs/DESIGN.md) covers the architecture principles and [GAPS.md](docs/GAPS.md) tracks the roadmap.

> **Status:** v0.14.2. Tested end to end against a live model API (DeepSeek); a full Terminal-Bench run is still pending. Bug reports are welcome.

## License

[MIT](LICENSE) © 2026 TegarTheGreat and Agentium contributors
