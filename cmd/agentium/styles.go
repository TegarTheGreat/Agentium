package main

import "strings"

// Output styles (Claude Code's /output-style): how the agent talks, added
// to the system prompt. "style" in the config, or /style.
var outputStyles = []struct{ name, hint, prompt string }{
	{"default", "direct and brief", ""},
	{"explanatory", "explains its choices and the codebase as it works", `

# Style: explanatory
Besides doing the work, teach: before and after meaningful steps, add a short "Insight" (2-3 lines) about why this approach, the trade-offs, or how this part of the codebase works. Keep the work itself as efficient as usual.`},
	{"learning", "leaves small parts for you to write, to learn by doing", `

# Style: learning
The user wants to learn by doing. Do the scaffolding and the routine parts, but for one or two meaningful pieces (a key function body, a tricky condition, a test case) leave a clear TODO(human) in the code with what to write and why, and ask the user to write it. Review what they write when they say it is done.`},
	{"terse", "the shortest useful answers", `

# Style: terse
Answer in as few words as possible: results and facts only, no explanations unless asked. One line when one line will do.`},
}

func stylePrompt(name string) string {
	for _, s := range outputStyles {
		if strings.EqualFold(s.name, name) {
			return s.prompt
		}
	}
	return ""
}
