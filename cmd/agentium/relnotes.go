package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// /release-notes shows what changed in a release, from its GitHub page
// (the release notes are the CHANGELOG's section for it).

// releaseAPI can be pointed at a test server.
var releaseAPI = "https://api.github.com/repos/" + releaseRepo + "/releases"

type releaseInfo struct {
	Tag  string `json:"tag_name"`
	Body string `json:"body"`
	URL  string `json:"html_url"`
}

// fetchRelease gets the release for ver ("0.20.0"), or the latest.
func fetchRelease(ctx context.Context, ver string) (releaseInfo, error) {
	u := releaseAPI + "/latest"
	if ver != "" && ver != "latest" && ver != "dev" {
		u = releaseAPI + "/tags/v" + url.PathEscape(strings.TrimPrefix(ver, "v"))
	}
	var r releaseInfo
	b, err := fetchBytes(ctx, u)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return r, errors.New("no release " + ver + " on GitHub")
		}
		return r, err
	}
	if len(b) > 1<<20 {
		return r, errors.New("the release page is too big")
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("release: %v", err)
	}
	return r, nil
}

func showReleaseNotes(u *ui, arg string) {
	ver := strings.TrimSpace(arg)
	if ver == "" {
		ver = version
	}
	u.note("fetching the notes for " + sanitize(ver) + "…")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r, err := fetchRelease(ctx, ver)
	if err != nil {
		u.failure(sanitize(err.Error()))
		return
	}
	fmt.Fprintln(os.Stderr, "\n  "+u.paint(cBold, "Agentium "+sanitize(r.Tag)))
	body := strings.TrimSpace(strings.ReplaceAll(r.Body, "\r\n", "\n"))
	if body == "" {
		body = "(no notes)"
	}
	var clean []string
	for _, l := range strings.Split(body, "\n") {
		clean = append(clean, sanitize(l))
	}
	body = strings.Join(clean, "\n") + "\n"
	if u.md != nil {
		u.md.Write(body)
		u.md.End()
	} else {
		fmt.Print(body)
	}
	if r.URL != "" && strings.HasPrefix(r.URL, "https://github.com/") {
		fmt.Fprintln(os.Stderr, u.paint(cDim, "  "+fileLinkURL(r.URL, r.URL)))
	}
}
