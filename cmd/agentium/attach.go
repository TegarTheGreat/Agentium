package main

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/tool"
)

// droppedImage is an image path as a terminal pastes a dragged file:
// quoted, with backslash escapes, or as a file:// URL.
var droppedImage = regexp.MustCompile(`(?i)(?:^|[\s(\[<])('(?:/|~/|[A-Za-z]:\\)[^']+\.(?:png|jpe?g|gif|webp)'|"(?:/|~/|[A-Za-z]:\\)[^"]+\.(?:png|jpe?g|gif|webp)"|file://\S+?\.(?:png|jpe?g|gif|webp)|(?:/|~/|[A-Za-z]:\\)(?:\\.|\S)+?\.(?:png|jpe?g|gif|webp))[.,;:!?)\]>]?(?:\s|$)`)

var shellEscape = regexp.MustCompile(`\\(.)`)

// droppedPaths finds dropped image paths; one right after another counts
// too (the separating space is not used up).
// leading counts the paths the message starts with (only spaces between
// them), where a drag puts them.
func droppedPaths(input string) (paths []string, leading int) {
	run := true
	prev := 0
	for off := 0; off < len(input) && len(paths) < 8; {
		m := droppedImage.FindStringSubmatchIndex(input[off:])
		if m == nil {
			break
		}
		raw := input[off+m[2] : off+m[3]]
		gap := input[prev : off+m[2]]
		run = run && strings.Trim(gap, " \t\n([<)]>.,;:!?") == ""
		prev = off + m[3]
		off += m[3]
		p := raw
		switch {
		case strings.HasPrefix(raw, "'") || strings.HasPrefix(raw, `"`):
			p = strings.Trim(raw, `"'`)
		case strings.HasPrefix(strings.ToLower(raw), "file://"):
			u, err := url.Parse(raw)
			if err != nil || (u.Host != "" && u.Host != "localhost") {
				continue
			}
			p = u.Path
		case runtime.GOOS != "windows":
			p = shellEscape.ReplaceAllString(raw, "$1") // "Screen\ Shot\ \(1\).png"
		}
		paths = append(paths, p)
		if run {
			leading++
		}
	}
	return paths, leading
}

var imageMention = regexp.MustCompile(`(?i)(?:^|\s)@("[^"]+\.(?:png|jpe?g|gif|webp)"|\S+\.(?:png|jpe?g|gif|webp))`)

// mentionedImages loads the image files named with @path in a prompt.
// Mentions that are not existing files are left alone (they may be
// memory directives or plain text). notes are for the user.
func mentionedImages(input, cwd string, vision bool, inside func(string) bool) (imgs []provider.Image, notes []string) {
	var paths []string
	for _, m := range imageMention.FindAllStringSubmatch(input, 8) {
		paths = append(paths, strings.Trim(m[1], `"`))
	}
	// A file dragged into the terminal arrives as its full path: an image
	// there is attached too, when it is in the workspace or the message
	// starts with it (as a drag leaves it). A path merely mentioned in
	// pasted text elsewhere on the disk is not sent.
	dropped, leading := droppedPaths(input)
	var extra []string
	for i, p := range dropped {
		abs := p
		if strings.HasPrefix(abs, "~/") {
			if h, err := os.UserHomeDir(); err == nil {
				abs = filepath.Join(h, abs[2:])
			}
		}
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			abs = r // the gate judges the file that is really read
		}
		if i < leading || inside == nil || inside(abs) {
			extra = append(extra, p)
		}
	}
	if !vision && len(imageMention.FindAllString(input, 1)) == 0 {
		extra = nil // no "cannot view images" notes for paths merely mentioned
	}
	paths = append(paths, extra...)
	seen := map[string]bool{}
	for _, p := range paths {
		if strings.HasPrefix(p, "~/") {
			if h, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(h, p[2:])
			}
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if _, err := os.Stat(p); err != nil || seen[p] {
			continue
		}
		seen[p] = true
		if !vision {
			notes = append(notes, fmt.Sprintf("%s not attached: this model cannot view images", filepath.Base(p)))
			continue
		}
		im, err := tool.LoadImage(p)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s not attached: %v", filepath.Base(p), err))
			continue
		}
		imgs = append(imgs, im)
		notes = append(notes, fmt.Sprintf("attached %s (%d KB)", filepath.Base(p), len(im.Data)/1024))
	}
	return imgs, notes
}

var (
	fileMention = regexp.MustCompile(`(?:^|\s)@([^\s"@]+)`)
	lineRange   = regexp.MustCompile(`^(.+?):(\d+)(?:-(\d+))?$`)
)

const (
	mentionMaxBytes = 60 * 1024 // a bigger file is left for the read tool
	mentionMaxTotal = 150 * 1024
)

// mentionedFiles puts the text of files named with @path (or a line
// range, @path:10-40) and the listing of @dir/ into the message, as the
// other agent CLIs do, saving a read step. Images, missing paths and
// large or binary files are left to the tools. allow is asked before a
// file is read (credential files need approval).
func mentionedFiles(input, cwd string, allow func(string) bool) (block string, names []string) {
	var sb strings.Builder
	seen := map[string]bool{}
	for _, mm := range fileMention.FindAllStringSubmatch(input, 10) {
		tok := strings.TrimRight(mm[1], ",.;:!?)")
		m := []string{tok, tok, "", ""}
		if r := lineRange.FindStringSubmatch(tok); r != nil {
			m = []string{tok, r[1], r[2], r[3]}
		}
		rel := m[1]
		if imageExt(rel) || seen[tok] {
			continue
		}
		seen[tok] = true
		p := rel
		if strings.HasPrefix(p, "~/") {
			if h, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(h, p[2:])
			}
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r // the gate judges the file that is really read
		}
		st, err := os.Stat(p)
		if err != nil || (allow != nil && !allow(p)) {
			continue
		}
		if st.IsDir() {
			ents, err := os.ReadDir(p)
			if err != nil {
				continue
			}
			var list []string
			for i, e := range ents {
				if i == 200 {
					list = append(list, fmt.Sprintf("… %d more", len(ents)-200))
					break
				}
				n := e.Name()
				if e.IsDir() {
					n += "/"
				}
				list = append(list, n)
			}
			fmt.Fprintf(&sb, "<mentioned-dir path=%q>\n%s\n</mentioned-dir>\n", rel, strings.ReplaceAll(strings.Join(list, "\n"), "</mentioned-", "<\\/mentioned-"))
			names = append(names, rel)
			continue
		}
		if !st.Mode().IsRegular() || st.Size() > mentionMaxBytes || sb.Len()+int(st.Size()) > mentionMaxTotal {
			continue
		}
		b, err := readCapped(p, mentionMaxBytes)
		if err != nil {
			continue // not a plain file any more
		}
		if strings.ContainsRune(string(b[:min(len(b), 8000)]), 0) || bytes.HasPrefix(b, []byte("%PDF-")) {
			// Binary: say so, so the model knows what was pointed at.
			fmt.Fprintf(&sb, "<mentioned-file path=%q>(binary file, %d bytes: use the read tool; it extracts a PDF's text)</mentioned-file>\n", rel, st.Size())
			names = append(names, rel)
			continue
		}
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		from, to := 1, len(lines)
		if m[2] != "" {
			fmt.Sscan(m[2], &from)
			to = from
			if m[3] != "" {
				fmt.Sscan(m[3], &to)
			}
			from, to = max(from, 1), min(to, len(lines))
			if from > to {
				continue
			}
		}
		var body strings.Builder
		for i := from; i <= to; i++ {
			fmt.Fprintf(&body, "%d\t%s\n", i, lines[i-1])
		}
		// The file's text cannot close the block early (and pass as the
		// user's own words).
		text := strings.ReplaceAll(body.String(), "</mentioned-", "<\\/mentioned-")
		fmt.Fprintf(&sb, "<mentioned-file path=%q lines=\"%d-%d\">\n%s</mentioned-file>\n", rel, from, to, text)
		label := rel
		if m[2] != "" {
			label += fmt.Sprintf(":%d-%d", from, to)
		}
		names = append(names, label)
	}
	return sb.String(), names
}

func imageExt(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}

// readCapped reads at most n bytes of a regular file, without blocking
// on a FIFO or device swapped in after the check.
func readCapped(p string, n int) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscallNonblock, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	return io.ReadAll(io.LimitReader(f, int64(n)))
}
