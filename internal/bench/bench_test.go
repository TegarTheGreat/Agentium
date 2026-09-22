package bench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tegarthegreat/agentium/internal/provider"
)

// solver is a fake model that solves the "write" task in two turns.
type solver struct{ turn int }

func (s *solver) Stream(_ context.Context, req provider.Request, _ func(string)) (provider.Response, error) {
	s.turn++
	if s.turn == 1 {
		return provider.Response{ToolCalls: []provider.ToolCall{{
			ID: "1", Name: "edit", Args: json.RawMessage(`{"path":"hello.txt","new":"hello agentium\n"}`),
		}}, Usage: provider.Usage{Input: 600, Output: 30}}, nil
	}
	return provider.Response{Text: "Done.", Usage: provider.Usage{Input: 20, CacheRead: 600, Output: 3}}, nil
}

func TestRunLive(t *testing.T) {
	res, err := RunLive(context.Background(), &solver{}, "fake", Tasks[:1], nil)
	if err != nil {
		t.Fatal(err)
	}
	r := res[0]
	if !r.Pass || r.Turns != 2 || r.ToolCalls != 1 || r.Usage.CacheRead != 600 {
		t.Fatalf("result = %+v", r)
	}
	// A model that does nothing fails the check.
	res, _ = RunLive(context.Background(), noop{}, "fake", Tasks[:2], nil)
	for _, r := range res {
		if r.Pass {
			t.Fatalf("%s passed without doing anything", r.Name)
		}
	}
}

type noop struct{}

func (noop) Stream(context.Context, provider.Request, func(string)) (provider.Response, error) {
	return provider.Response{Text: "ok"}, nil
}

func TestTaskChecksRejectUnsolvedSetup(t *testing.T) {
	for _, task := range Tasks {
		dir := t.TempDir()
		for p, c := range task.Setup {
			fp := filepath.Join(dir, p)
			os.MkdirAll(filepath.Dir(fp), 0o755)
			os.WriteFile(fp, []byte(c), 0o644)
		}
		if task.Check(dir, "") == nil {
			t.Errorf("%s: check passes on the untouched setup", task.Name)
		}
	}
}
