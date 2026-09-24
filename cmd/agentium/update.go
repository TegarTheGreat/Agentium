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

func updatePath() string { return filepath.Join(config.Home(), "update.json") }

// updateNotice returns "0.13.0" when a newer release is known, and
// refreshes that knowledge in the background once a day.
func updateNotice() string {
	if version == "dev" || os.Getenv("AGENTIUM_NO_UPDATE_CHECK") != "" {
		return ""
	}
	var st updateState
	if b, err := os.ReadFile(updatePath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if time.Since(st.Checked) > 24*time.Hour {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			v, err := latestVersion(ctx)
			if err != nil {
				return
			}
			b, _ := json.Marshal(updateState{Checked: time.Now(), Latest: v})
			_ = os.MkdirAll(config.Home(), 0o700)
			_ = os.WriteFile(updatePath(), b, 0o600)
		}()
	}
	if st.Latest != "" && newer(st.Latest, version) {
		return st.Latest
	}
	return ""
}

// cmdUpdate replaces this binary with the latest release.
func cmdUpdate(args []string) error {
	u := &ui{color: isTTY(os.Stderr) && os.Getenv("NO_COLOR") == ""}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	want := ""
	if len(args) > 0 {
		want = strings.TrimPrefix(args[0], "v")
	}
	if want == "" {
		v, err := latestVersion(ctx)
		if err != nil {
			return fmt.Errorf("checking for updates: %w", err)
		}
		want = v
		if !newer(want, version) {
			u.success("agentium " + version + " is up to date")
			return nil
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if r, err := filepath.EvalSymlinks(self); err == nil {
		self = r
	}
	name := fmt.Sprintf("agentium_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name = strings.TrimSuffix(name, ".tar.gz") + ".zip"
	}
	base := releaseBase + "/download/v" + want + "/"
	fmt.Fprintf(os.Stderr, "%s Updating %s → %s\n", u.paint(cCyan, "==>"), version, want)
	archive, err := fetchBytes(ctx, base+name)
	if err != nil {
		return err
	}
	sums, err := fetchBytes(ctx, base+"checksums.txt")
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archive)
	if !checksumListed(string(sums), name, hex.EncodeToString(sum[:])) {
		return fmt.Errorf("checksum mismatch for %s; not installed", name)
	}
	u.success("Downloaded and verified " + name)
	bin, err := extractBinary(archive, name)
	if err != nil {
		return err
	}
	if err := replaceExecutable(self, bin); err != nil {
		return fmt.Errorf("installing to %s: %w", self, err)
	}
	b, _ := json.Marshal(updateState{Checked: time.Now(), Latest: want})
	_ = os.WriteFile(updatePath(), b, 0o600)
	u.success("Updated to agentium " + want + u.paint(cDim, " ("+self+")"))
	return nil
}

func fetchBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
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

// replaceExecutable swaps in the new binary. A running executable can be
// renamed but not overwritten on Windows, so the old one is moved aside.
func replaceExecutable(path string, bin []byte) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		old := path + ".old"
		_ = os.Remove(old)
		if err := os.Rename(path, old); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
