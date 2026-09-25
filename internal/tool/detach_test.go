package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDetachForeground(t *testing.T) {
	env := &Env{Root: t.TempDir()}
	defer env.KillJobs()
	if env.DetachForeground() {
		t.Fatal("nothing runs: nothing to detach")
	}
	go func() {
		for i := 0; i < 50 && !env.DetachForeground(); i++ {
			time.Sleep(50 * time.Millisecond)
		}
	}()
	start := time.Now()
	args, _ := json.Marshal(map[string]any{"cmd": "echo started; sleep 3; echo finished", "timeout": 30})
	out, err := bashTool.Run(context.Background(), env, args)
	if err != nil || time.Since(start) > 2*time.Second || !strings.Contains(out, "job 1") || !strings.Contains(out, "started") {
		t.Fatalf("detach: %v %q after %s", err, out, time.Since(start))
	}
	got, err := env.jobAction(context.Background(), 1, "", false, 5)
	if err != nil || !strings.Contains(got, "finished") {
		t.Fatalf("job output: %v %q", err, got)
	}
	if _, err := env.jobAction(context.Background(), 1, "x", false, 0); err == nil {
		t.Fatal("stdin to an adopted job should fail cleanly")
	}
}
