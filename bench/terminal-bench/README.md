# Terminal-Bench with Agentium

Run the same benchmark other harnesses report, on the same model:

```sh
pip install harbor            # Harbor, the Terminal-Bench 2.x runner
harbor run -d terminal-bench@2.0 \
  -a bench.terminal-bench.agentium_harbor:Agentium \
  -m anthropic/claude-sonnet-5 \
  --ae ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY
```

- The adapter installs the latest Agentium release in each task container
  (`install.sh`) and runs it headless with `--json`. It follows Harbor's
  documented `BaseInstalledAgent` interface. It has not been run end to end
  from this repo yet, so expect to adjust it to your Harbor version.
- Compare against Terminus 2, Codex CLI, Claude Code, etc. **on the same
  model**. Model-vs-model comparisons say nothing about the harness.
- Keep the defaults honest: Harbor copies tests in only for verification.
  Don't give the agent answer keys through AGENTS.md or the task image.
  Leaked answers inflated some 2026 leaderboard entries by ~10 points.

For quick local checks without Harbor: `agentium bench -m <model>`.
