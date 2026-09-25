package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/tegarthegreat/agentium/internal/agent"
	"github.com/tegarthegreat/agentium/internal/provider"
)

// After a reply, a cheap call guesses the message the user is likely to
// send next; it shows dimmed in the empty composer and tab takes it.

const suggestPrompt = `Here is the end of a session with a coding agent. Predict the user's next message to the agent: a short instruction in the user's own language and style, at most 12 words, that the user would plausibly type next (e.g. "run the tests", "commit this", "now add a test for the empty case"). If nothing obvious follows, answer NONE. Answer with the message only.`

type suggester struct {
	mu   sync.Mutex
	text string
	gen  int // bumps when a new turn starts; stale guesses are dropped
}

func (s *suggester) get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text
}

// clear drops the current guess (a new message is being sent).
func (s *suggester) clear() {
	s.mu.Lock()
	s.text = ""
	s.gen++
	s.mu.Unlock()
}

// guess asks the model in the background.
func (s *suggester) guess(a *agent.Agent, request, reply string) {
	request, reply = strings.TrimSpace(provider.UserWords(request)), strings.TrimSpace(reply)
	if request == "" || reply == "" {
		return
	}
	s.mu.Lock()
	gen := s.gen
	s.mu.Unlock()
	client, model := a.Client, a.Model
	if a.Fast != nil {
		client, model = a.Fast, a.FastModel
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		text := "USER: " + clipMiddle(request, 1500) + "\n\nAGENT: " + clipMiddle(reply, 2500) + "\n\n---\n" + suggestPrompt
		resp, err := client.Stream(ctx, provider.Request{
			Model:     model,
			Messages:  []provider.Message{{Role: provider.RoleUser, Text: text}},
			MaxTokens: 1500, // reasoning models think first
		}, nil)
		if err != nil {
			return
		}
		a.Charge(resp.Usage, a.Fast == nil)
		g := strings.Trim(strings.TrimSpace(firstLine(strings.TrimSpace(resp.Text))), "\"'`")
		if g == "" || strings.EqualFold(strings.TrimRight(g, "."), "none") || len([]rune(g)) > 100 {
			return
		}
		s.mu.Lock()
		if s.gen == gen {
			s.text = g
		}
		s.mu.Unlock()
	}()
}

// clipMiddle keeps the start and the end of s.
func clipMiddle(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n/3], "") + "\n…\n" + strings.ToValidUTF8(s[len(s)-2*n/3:], "")
}
