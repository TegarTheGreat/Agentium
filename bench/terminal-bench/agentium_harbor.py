"""Run Agentium on Terminal-Bench 2.x through Harbor.

    harbor run -d terminal-bench@2.0 \
        -a bench.terminal-bench.agentium_harbor:Agentium \
        -m anthropic/claude-sonnet-5 \
        --ae ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY

(From the repo root; or copy this file into a package on PYTHONPATH and
pass its module path.) Follows Harbor's documented BaseInstalledAgent
interface: https://docs.harborframework.com/core-concepts/agents/custom-agents.md

Honest benchmarking: the task container is already isolated, so Agentium
runs with --yolo --no-sandbox. Harbor keeps the tests away from the agent
(they are copied in only for verification), which is what makes the score
comparable to other harnesses on the same model.
"""

import shlex

from harbor.agents.installed.base import BaseInstalledAgent, with_prompt_template
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

INSTALL = (
    "set -e; "
    "command -v curl >/dev/null || (apt-get update -qq && apt-get install -y -qq curl ca-certificates) "
    "|| (apk add --no-cache curl ca-certificates); "
    "curl -fsSL https://raw.githubusercontent.com/TegarTheGreat/Agentium/main/install.sh "
    "| AGENTIUM_BIN_DIR=/usr/local/bin sh"
)


class Agentium(BaseInstalledAgent):
    @staticmethod
    def name() -> str:
        return "agentium"

    async def install(self, environment: BaseEnvironment) -> None:
        await self.exec_as_root(environment, command=INSTALL)

    @with_prompt_template
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        model = getattr(self, "model_name", None) or ""
        args = ["agentium", "--json", "--yolo", "--no-sandbox", "--max-turns", "200"]
        if model:
            args += ["-m", model]
        cmd = " ".join(shlex.quote(a) for a in args)
        # The instruction goes on stdin so its length and quoting never matter;
        # JSON Lines events are kept in the agent log for later analysis.
        await self.exec_as_agent(
            environment,
            command=f"printf %s {shlex.quote(instruction)} | {cmd} | tee /tmp/agentium-events.jsonl",
        )
