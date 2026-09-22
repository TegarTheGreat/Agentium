package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/tegarthegreat/agentium/internal/bench"
	"github.com/tegarthegreat/agentium/internal/config"
	"github.com/tegarthegreat/agentium/internal/provider"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// readSecret reads a line without echo when stdin is a terminal.
func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	tty := isTTY(os.Stdin)
	if tty {
		if err := exec.Command("stty", "-F", "/dev/tty", "-echo").Run(); err != nil {
			_ = exec.Command("stty", "-f", "/dev/tty", "-echo").Run() // macOS
		}
		defer func() {
			if err := exec.Command("stty", "-F", "/dev/tty", "echo").Run(); err != nil {
				_ = exec.Command("stty", "-f", "/dev/tty", "echo").Run()
			}
			fmt.Fprintln(os.Stderr)
		}()
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func cmdLogin(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	specs := provider.Specs(cfg)
	if len(args) != 1 {
		return fmt.Errorf("usage: agentium login <provider>\nproviders: %s", strings.Join(provider.IDs(specs), ", "))
	}
	id := args[0]
	s, ok := specs[id]
	if !ok {
		return fmt.Errorf("unknown provider %q; add it under \"providers\" in %s/config.json", id, config.Home())
	}
	if s.NoKey {
		fmt.Fprintf(os.Stderr, "%s needs no key (local server at %s)\n", id, s.BaseURL)
		return nil
	}
	key, err := readSecret(fmt.Sprintf("API key for %s: ", id))
	if err != nil {
		return err
	}
	if key == "" {
		return errors.New("empty key")
	}
	auth, err := config.LoadAuth()
	if err != nil {
		return err
	}
	auth[id] = config.Credential{APIKey: key}
	if err := config.SaveAuth(auth); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "saved %s credentials to %s/auth.json\n", id, config.Home())
	return nil
}

func cmdLogout(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: agentium logout <provider>")
	}
	auth, err := config.LoadAuth()
	if err != nil {
		return err
	}
	delete(auth, args[0])
	return config.SaveAuth(auth)
}

func cmdProviders() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	auth, err := config.LoadAuth()
	if err != nil {
		return err
	}
	specs := provider.Specs(cfg)
	for _, id := range provider.IDs(specs) {
		s := specs[id]
		status := "no key"
		switch {
		case s.NoKey:
			status = "local"
		case provider.Key(s, auth) != "":
			status = "ready"
		}
		def := s.Default
		if def == "" {
			def = "-"
		}
		fmt.Printf("%-11s %-7s %-9s %-28s %s\n", id, status, s.Protocol, def, s.BaseURL)
	}
	return nil
}

func cmdBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	modelRef := fs.String("m", "", "model for live tasks")
	runs := fs.Int("runs", 20, "startup runs")
	asJSON := fs.Bool("json", false, "JSON output")
	only := fs.String("task", "", "run only this live task")
	if err := fs.Parse(args); err != nil {
		return err
	}
	off, err := bench.MeasureOffline(*runs)
	if err != nil {
		return err
	}
	out := map[string]any{"offline": off}
	if !*asJSON {
		fmt.Printf("binary        %.1f MB\n", float64(off.BinaryBytes)/1e6)
		fmt.Printf("startup       median %s · p90 %s (%d runs)\n", off.StartupMedian.Round(10*time.Microsecond), off.StartupP90.Round(10*time.Microsecond), *runs)
		if off.MaxRSSBytes > 0 {
			fmt.Printf("max RSS       %.1f MB\n", float64(off.MaxRSSBytes)/1e6)
		}
		fmt.Printf("prompt        %d chars system + %d chars tools ≈ %d tokens overhead\n", off.PromptChars, off.ToolChars, off.OverheadTokens)
	}
	if *modelRef == "" {
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(out)
		}
		fmt.Println("\nlive tasks: pass -m provider/model")
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	auth, err := config.LoadAuth()
	if err != nil {
		return err
	}
	res, err := provider.Resolve(*modelRef, cfg, auth)
	if err != nil {
		return err
	}
	tasks := bench.Tasks
	if *only != "" {
		tasks = nil
		for _, t := range bench.Tasks {
			if t.Name == *only {
				tasks = append(tasks, t)
			}
		}
		if tasks == nil {
			return fmt.Errorf("unknown task %q", *only)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if !*asJSON {
		fmt.Printf("\nlive: %s/%s\n", res.Provider, res.Model)
	}
	results, err := bench.RunLive(ctx, res.Client, res.Model, tasks, func(r bench.Result) {
		if *asJSON {
			return
		}
		mark := "PASS"
		if !r.Pass {
			mark = "FAIL"
		}
		u := r.Usage
		fmt.Printf("  %-12s %s  %d turns · %d tools · in %s (cached %s) · out %s · %.1fs", r.Name, mark, r.Turns, r.ToolCalls,
			fmtK(u.Input+u.CacheRead+u.CacheWrite), fmtK(u.CacheRead), fmtK(u.Output), r.Elapsed.Seconds())
		if r.Error != "" {
			fmt.Printf("  — %s", firstLine(r.Error))
		}
		fmt.Println()
	})
	var pass, turns, tokIn, tokOut int
	var total time.Duration
	for _, r := range results {
		if r.Pass {
			pass++
		}
		turns += r.Turns
		tokIn += r.Usage.Input + r.Usage.CacheRead + r.Usage.CacheWrite
		tokOut += r.Usage.Output
		total += r.Elapsed
	}
	if *asJSON {
		out["model"] = res.Provider + "/" + res.Model
		out["live"] = results
		if e := json.NewEncoder(os.Stdout).Encode(out); e != nil {
			return e
		}
	} else if len(results) > 0 {
		fmt.Printf("  total        %d/%d pass · %d turns · in %s · out %s · %.1fs\n", pass, len(results), turns, fmtK(tokIn), fmtK(tokOut), total.Seconds())
	}
	return err
}
