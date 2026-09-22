# Agentium

A fast, minimal coding agent for the terminal. One small static binary, five tools, a system prompt under 1k tokens, and any model provider.

Example session:

```
$ agentium "the tests in ./calc fail, fix them"
› bash go test ./calc/...
› read calc/add.go
› edit calc/add.go
› bash go test ./calc/...
Fixed Add: it subtracted instead of adding. go test ./calc/... passes.
· 4 turns · 4 tools · in 6.1k (cached 4.8k) · out 180 · 5.2s
```

## Why

Popular agents are slow and wordy. Claude Code sends about 33k tokens of prompt and tool definitions before your first word; OpenCode sends about 7k and can grow to gigabytes of RAM. The 2026 data (see [docs/RESEARCH.md](docs/RESEARCH.md)) shows minimal harnesses match or beat heavy ones. Agentium is built for that:

| | Agentium v0.1 (measured) |
|---|---|
| Binary | 6.6 MB, static, no runtime |
| Startup | ~2.5 ms |
| Memory | ~7 MB RSS |
| Prompt + tool schemas | ~600 tokens |
| Tools | `read` `edit` `bash` `search` `fetch` |

Reproduce with `agentium bench`.

What keeps it fast and to the point:
- **Terse by design:** no preamble, no recap, plan only for big tasks, done means verified.
- **Parallel tools:** every tool call in a turn runs concurrently; edits to the same file are serialized.
- **Cache-friendly:** the system prompt and tools never change mid-session, and Anthropic cache breakpoints are set automatically.
- **Lean context:** tool output is clipped head+tail. Past a budget, old outputs are elided in one batch, so the prompt cache survives.
- **Deterministic safety:** risky commands (`rm -rf`, force push, `sudo`, `curl | sh`, …) and writes outside the workspace need your approval. That decision is made in code, not by the model.

## Install

```sh
go install github.com/tegarthegreat/agentium/cmd/agentium@latest
# or from a clone:
make build   # ./agentium
```

## Use

```sh
export ANTHROPIC_API_KEY=...          # or OPENAI_API_KEY, GEMINI_API_KEY, OPENROUTER_API_KEY, ...
agentium                              # interactive
agentium "add a --json flag to main.go"
git diff | agentium -p "review this"  # stdin works too
agentium -c "now update the docs"     # continue the last session here
agentium -m ollama/qwen3-coder        # local model
```

Interactive commands: `/clear`, `/model <provider/model>`, `/mode ask|auto|yolo`, `/usage`, `/exit`. Ctrl-C interrupts the current turn.

Flags: `-m provider/model`, `-p prompt`, `-c`, `--mode ask|auto|yolo` (default `auto`: only risky actions ask), `--yolo`, `-q`, `--max-turns N`.

## Providers & login

Agentium implements wire protocols, not vendors. Anything that speaks the OpenAI Chat Completions or Anthropic Messages protocol works.

Built in: `anthropic`, `openai`, `gemini`, `openrouter`, `groq`, `cerebras`, `deepseek`, `xai`, `mistral`, `together`, `fireworks`, `moonshot`, `zai`, `ollama`, `lmstudio`. Run `agentium providers` to see which are ready.

```sh
agentium login openai        # stores the key in ~/.agentium/auth.json (0600)
agentium logout openai
```

Keys are read from env vars first, then `auth.json`. With no `-m`, the first provider that has a key is used.

Add any other endpoint in `~/.agentium/config.json`:

```json
{
  "model": "corp/big-model",
  "mode": "auto",
  "providers": {
    "corp": { "protocol": "openai", "base_url": "https://llm.corp.example/v1", "api_key_env": "CORP_KEY" },
    "vllm": { "base_url": "http://gpu-box:8000/v1" }
  }
}
```

Project instructions come from `AGENTS.md` (or `CLAUDE.md`), read from the repo root down to the current directory, plus `~/.agentium/AGENTS.md`.

## Benchmark

```sh
agentium bench                                 # binary size, startup, RSS, prompt overhead
agentium bench -m anthropic/claude-sonnet-5    # + live tasks: pass rate, turns, tokens, time
agentium bench -m openai/gpt-5.5 -json
```

## Roadmap

Planned (see [docs/RESEARCH.md](docs/RESEARCH.md)):
- **v0.2 memory:** `USER.md`/`MEMORY.md` frozen snapshots, SQLite FTS5 auto-recall (<20 ms, no tool call needed), `@remember` / `@decide`.
- **v0.3:** automatic verification after edits, OS sandbox, per-model adaptive harness.
- **v0.4:** native Gemini protocol, the full models.dev registry, ChatGPT and GitHub Copilot OAuth, Bedrock/Vertex/Azure, fallback models.
- **v0.5:** observational memory, temporal facts, `agentium tidy` consolidation.
- **v0.6:** Terminal-Bench 2.1 runs against other agents on the same model.

## Develop

```sh
make test    # go vet + go test -race
make cross   # linux/darwin/windows binaries in dist/
```
