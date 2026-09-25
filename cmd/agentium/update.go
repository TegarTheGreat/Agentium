package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/tegarthegreat/agentium/internal/config"
)

// Self-update from GitHub releases. The update check runs at most once a
// day in the background and only tells; `agentium update` installs.

const releaseRepo = "TegarTheGreat/Agentium"

// releaseBase can be pointed at a test server.
var releaseBase = "https://github.com/" + releaseRepo + "/releases"

// latestVersion asks GitHub which tag "latest" redirects to.
func latestVersion(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", releaseBase+"/latest", nil)
	if err != nil {
		return "", err
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return "", fmt.Errorf("no release found (%s)", resp.Status)
	}
	return strings.TrimPrefix(loc[i+5:], "v"), nil
}

// newer reports whether version a is newer than b ("0.12.1" > "0.12.0").
func newer(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(strings.SplitN(pa[i], "-", 2)[0])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(strings.SplitN(pb[i], "-", 2)[0])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

type updateState struct {
	Checked time.Time `json:"checked"`
	Latest  string    `json:"latest"`
}

func loadUpdateState() updateState {
	var st updateState
	if b, err := os.ReadFile(updatePath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func saveUpdateState(st updateState) {
	b, _ := json.Marshal(st)
	writeSmall(updatePath(), b)
}

// writeSmall replaces a small state file whole (another session may be
// reading it).
func writeSmall(path string, b []byte) {
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
}

// seenPath holds the version that last ran (its own file: older
// versions rewrite update.json without it).
func seenPath() string { return filepath.Join(config.Home(), "seen-version") }

// justUpdated returns the version that ran before this one, once, when
// this one is newer: the welcome then points at /release-notes.
func justUpdated() string {
	if version == "dev" {
		return ""
	}
	b, _ := os.ReadFile(seenPath())
	prev := strings.TrimSpace(string(b))
	if prev == version {
		return ""
	}
	writeSmall(seenPath(), []byte(version+"\n"))
	if prev != "" && newer(version, prev) {
		return prev
	}
	return ""
}

func updatePath() string { return filepath.Join(config.Home(), "update.json") }

// updateNotice returns "0.13.0" when a newer release is known, and
// refreshes that knowledge in the background once a day.
func updateNotice() string {
	if version == "dev" || os.Getenv("AGENTIUM_NO_UPDATE_CHECK") != "" {
		return ""
	}
	st := loadUpdateState()
	if time.Since(st.Checked) > 24*time.Hour {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			v, err := latestVersion(ctx)
			if err != nil {
				return
			}
			now := loadUpdateState() // keep what else was written meanwhile
			now.Checked, now.Latest = time.Now(), v
			saveUpdateState(now)
		}()
	}
	if st.Latest != "" && newer(st.Latest, version) {
		return st.Latest
	}
	return ""
}

// cmdUpdate replaces this binary with the latest release.
func cmdUpdate(args []string) error {
	for _, a := range args {
		if a == "--rollback" {
			return rollback()
		}
	}
	_, err := runUpdate(args)
	return err
}

// runUpdate installs the release and reports whether it replaced the binary.
func runUpdate(args []string) (bool, error) {
	u := &ui{color: isTTY(os.Stderr) && os.Getenv("NO_COLOR") == "" && !dumbTerm()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	want := ""
	if len(args) > 0 {
		want = strings.TrimPrefix(args[0], "v")
	}
	if want == "" {
		v, err := latestVersion(ctx)
		if err != nil {
			return false, fmt.Errorf("checking for updates: %w", err)
		}
		want = v
		if !newer(want, version) {
			u.success("agentium " + version + " is up to date")
			return false, nil
		}
	}
	self, err := os.Executable()
	if err != nil {
		return false, err
	}
	if r, err := filepath.EvalSymlinks(self); err == nil {
		self = r
	}
	if pm := managedInstall(self); pm != "" {
		return false, fmt.Errorf("this agentium was installed by %s; update it there", pm)
	}
	name := fmt.Sprintf("agentium_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name = strings.TrimSuffix(name, ".tar.gz") + ".zip"
	}
	base := releaseBase + "/download/v" + want + "/"
	fmt.Fprintf(os.Stderr, "%s Updating %s → %s\n", u.paint(cCyan, "==>"), version, want)
	archive, err := fetchBytes(ctx, base+name)
	if err != nil {
		return false, err
	}
	sums, err := fetchBytes(ctx, base+"checksums.txt")
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(archive)
	if !checksumListed(string(sums), name, hex.EncodeToString(sum[:])) {
		return false, fmt.Errorf("checksum mismatch for %s; not installed", name)
	}
	u.success("Downloaded and verified " + name)
	bin, err := extractBinary(archive, name)
	if err != nil {
		return false, err
	}
	if err := replaceExecutable(self, bin); err != nil {
		return false, fmt.Errorf("installing to %s: %w", self, err)
	}
	st := loadUpdateState()
	st.Checked, st.Latest = time.Now(), want
	saveUpdateState(st)
	u.success("Updated to agentium " + want + u.paint(cDim, " ("+self+")"))
	return true, nil
}

// fetchBytes downloads url, retrying network errors and 5xx responses.
func fetchBytes(ctx context.Context, url string) ([]byte, error) {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		var b []byte
		var retry bool
		if b, retry, err = fetchOnce(ctx, url); err == nil || !retry {
			return b, err
		}
	}
	return nil, err
}

func fetchOnce(ctx context.Context, url string) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, ctx.Err() == nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	return b, err != nil && ctx.Err() == nil, err
}

func checksumListed(sums, name, sum string) bool {
	for _, line := range strings.Split(sums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name && strings.EqualFold(f[0], sum) {
			return true
		}
	}
	return false
}

// extractBinary returns the agentium executable from a release archive.
func extractBinary(archive []byte, name string) ([]byte, error) {
	if strings.HasSuffix(name, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == "agentium.exe" {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(rc)
			}
		}
		return nil, errors.New("agentium.exe not found in the archive")
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, errors.New("agentium not found in the archive")
		}
		if filepath.Base(h.Name) == "agentium" && h.Typeflag == tar.TypeReg {
			return io.ReadAll(tr)
		}
	}
}

// replaceExecutable swaps in the new binary: written to a temporary file
// next to it, synced, then renamed over it, so a crash never leaves a
// half-written binary. A running executable can be renamed but not
// overwritten on Windows, so the old one is moved aside first (and put
// back if the swap fails).
func replaceExecutable(path string, bin []byte) error {
	tmp := path + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := f.Write(bin); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// The new binary must start before it replaces the working one.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	out, err := exec.CommandContext(ctx, tmp, "version").CombinedOutput()
	cancel()
	if err != nil || !strings.HasPrefix(string(out), "agentium ") {
		os.Remove(tmp)
		return fmt.Errorf("the downloaded binary does not run here (%v); nothing was changed", firstLine(strings.TrimSpace(string(out)+" "+errString(err))))
	}
	// The current binary is kept as .old: `agentium update --rollback`
	// puts it back.
	old := path + ".old"
	_ = os.Remove(old)
	if runtime.GOOS == "windows" {
		if err := os.Rename(path, old); err != nil {
			os.Remove(tmp)
			return err
		}
	} else if err := os.Link(path, old); err != nil {
		if b, rerr := os.ReadFile(path); rerr == nil {
			_ = os.WriteFile(old, b, 0o755)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		if runtime.GOOS == "windows" {
			_ = os.Rename(old, path)
		}
		return err
	}
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// rollback puts back the binary the last update replaced.
func rollback() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if r, err := filepath.EvalSymlinks(self); err == nil {
		self = r
	}
	old := self + ".old"
	b, err := os.ReadFile(old)
	if err != nil {
		return errors.New("no earlier version is kept here (" + old + ")")
	}
	if err := replaceExecutable(self, b); err != nil {
		return err
	}
	out, _ := exec.Command(self, "version").Output()
	fmt.Println("Rolled back to", strings.TrimSpace(string(out)))
	return nil
}

// managedInstall reports a binary installed by a package manager, which
// should update it instead.
func managedInstall(path string) string {
	p := filepath.ToSlash(path)
	switch {
	case strings.Contains(p, "/Cellar/") || strings.Contains(p, "/homebrew/"):
		return "Homebrew (brew upgrade agentium)"
	case strings.HasPrefix(p, "/nix/store/"):
		return "Nix"
	case strings.Contains(p, "/scoop/apps/"):
		return "Scoop (scoop update agentium)"
	}
	return ""
}
