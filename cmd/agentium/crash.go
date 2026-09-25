package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/tegarthegreat/agentium/internal/config"
)

// writeCrashLog saves a panic with its stack and the version to
// <home>/crash/, so a report can carry what happened; "" when it cannot.
func writeCrashLog(r any) string {
	dir := filepath.Join(config.Home(), "crash")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	p := filepath.Join(dir, time.Now().Format("20060102-150405")+".log")
	body := fmt.Sprintf("agentium %s %s/%s\n%s\n\npanic: %v\n\n%s", version, runtime.GOOS, runtime.GOARCH,
		time.Now().Format(time.RFC3339), r, debug.Stack())
	if os.WriteFile(p, []byte(body), 0o600) != nil {
		return ""
	}
	return p
}
