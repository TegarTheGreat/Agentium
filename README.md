# Agentium

A fast, minimal coding agent for the terminal. It ships as one small static binary with five tools and a system prompt under 1k tokens, and it works with any model provider. It also has an OS sandbox, undo, cross-session memory, and verification built in.

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
| Binary | 7.8 MB, static, no runtime |
| Startup | ~3 ms |
| Memory | ~7 MB RSS |
| Prompt + tool schemas | ~750 tokens (memory rules included) |
| Tools | `read` `edit` `bash` `search` `fetch` (+ MCP tools if configured) |

## What it does

**Fast and to the point**
- Terse by default: no preamble or recap, a plan only for big tasks, and "done" means verified.
- Tool calls in the same turn run in parallel. Edits to the same file are serialized.
- The system prompt and tool list are fixed for the session, and Anthropic cache breakpoints are set automatically. Cost is shown per turn.
- Output is clipped head+tail with a hint on how to see the rest.

**Reliable**
- **Lint-gated edits.** An edit that would break a file that parsed before (Go, JSON, Python, shell, JS) is rejected and the file stays untouched. The edit tool tolerates CRLF, trailing-whitespace and indentation differences, and re-indents to the file's style.
- **Verify before finishing.** If the model changed code and ran no build or test afterwards, it is asked once to verify.
- **Stuck detector.** The same call with the same result 3× gets a warning; 5× stops the run.
- **Stream robustness.** Truncated replies are continued. Stalled or broken streams are retried, honouring `Retry-After`. A fallback model chain takes over when the main model is down.
- **Context management.** Each model's context window is known. Old tool output and old edit payloads are elided at 55% of the window, and older turns are summarized at 85%.
- **Best of N.** `--best-of N --check "make test"` runs N attempts in parallel git worktrees and applies the passing one with the smallest diff.

**Safe**
- **OS sandbox for shell commands.** Landlock on Linux, `sandbox-exec` on macOS. Only the workspace, temp dirs and build caches are writable, and TCP is blocked unless a command asks for `net` and you approve it.
- **Deterministic approval gate.** Risky commands (`rm -rf`, force push, `sudo`, `curl | sh`, deleting via scripts, touching credentials) and reads of SSH keys or cloud credentials need approval. Modes are `ask`, `auto` (default) and `yolo`.
- `fetch` refuses localhost, private networks and cloud metadata addresses, including on redirects.
- **Checkpoints.** Every turn that changes something is snapshotted in a shadow git repository (your `.git` is untouched). `/undo` or `agentium undo` reverts it, including changes made by shell commands.
- Edits refuse to blindly overwrite an existing file, or to write a file changed on disk since the model read it.

**Remembers**
- `USER.md` (your preferences) and `MEMORY.md` (per project) are small, capped files injected as a frozen snapshot, together with active decisions from `DECISIONS.md`.
- The model saves memory with plain lines in its reply: `@remember`, `@prefer`, `@decide … (supersedes D-003)`, `@forget`. Entries are redacted for secrets and refused if they look like prompt injection.
- **Automatic recall.** A local BM25 index over decisions, a per-turn journal and past sessions is searched before every turn. Relevant snippets arrive as a small `<recall>` block. Nothing depends on the model remembering to call a tool.
- `agentium tidy` consolidates memory (shows a diff first).

**Any provider**
- Two wire protocols (OpenAI Chat Completions and Anthropic Messages) plus Bedrock and Vertex clients cover the built-ins and every compatible provider in the [models.dev](https://models.dev) registry (180+).
- Built in: `anthropic`, `openai`, `gemini`, `openrouter`, `groq`, `cerebras`, `deepseek`, `xai`, `mistral`, `together`, `fireworks`, `moonshot`, `zai`, `github` (GitHub Models), `azure`, `bedrock`, `vertex`, `ollama`, `lmstudio`.
- **Reasoning.** Adaptive thinking and `--effort` on Claude (signed thinking blocks are replayed exactly as the API requires), `reasoning_effort` on OpenAI-compatible models, reasoning replay for DeepSeek-style models, and Gemini thought signatures. `--fast` uses Claude's fast mode where available.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/TegarTheGreat/Agentium/main/install.sh | sh
# or
go install github.com/tegarthegreat/agentium/cmd/agentium@latest
# or from a clone
make build
```

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
```

In a session:
- **Commands:** `/undo`, `/sessions`, `/resume <n>`, `/clear`, `/model <provider/model>`, `/mode ask|auto|yolo`, `/usage`, `/exit`.
- **Keys:** ↑/↓ history, Ctrl-A/E/U/K/W. Pastes keep their newlines. End a line with `\` for a newline. Ctrl-C interrupts a running turn.

Other commands: `agentium providers`, `agentium models [provider] [--refresh]`, `agentium login [--oauth] <provider>`, `agentium logout <provider>`, `agentium undo`, `agentium tidy`, `agentium bench [-m model]`.

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

v0.6.0. Everything above is implemented and covered by unit and end-to-end tests: fake model servers for every protocol, a fake MCP server, and real pty tests for the line editor. Landlock confinement is tested on Linux, and CI runs Linux and macOS. **Not yet exercised against real model APIs or a real Terminal-Bench run.** Please report what breaks.

## Develop

```sh
make test    # go vet + go test -race
make cross   # linux/darwin/windows binaries in dist/
```
