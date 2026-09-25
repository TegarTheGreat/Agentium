package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// AskTool lets the model put a question to the user mid-task, with
// options to pick from, and carry on with the answer (instead of ending
// its turn to ask). Offered only when someone is there to answer.
var AskTool = Tool{
	Def: providerDef("ask",
		"Ask the user a question when you cannot proceed well without their decision (a real choice between approaches, missing information you cannot find). Give 2-6 short options when there are clear choices; the user may also type their own answer. Do not ask about things you can find out or reasonably decide yourself.",
		`{"type":"object","required":["question"],"properties":{"question":{"type":"string"},"options":{"type":"array","items":{"type":"string"},"maxItems":6}}}`),
	Run: func(ctx context.Context, env *Env, raw json.RawMessage) (string, error) {
		var a struct {
			Question string   `json:"question"`
			Options  []string `json:"options"`
		}
		if err := decode(raw, &a); err != nil {
			return "", err
		}
		a.Question = strings.TrimSpace(a.Question)
		if a.Question == "" {
			return "", errors.New("question is required")
		}
		if len(a.Options) > 6 {
			a.Options = a.Options[:6]
		}
		if env.Ask == nil {
			return "(no one can answer here: decide yourself, and state what you assumed in your final answer)", nil
		}
		ans, err := env.Ask(a.Question, a.Options)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(ans) == "" {
			return "(the user skipped the question: decide yourself, and say what you chose)", nil
		}
		return "The user answered: " + ans, nil
	},
}
