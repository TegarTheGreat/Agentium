package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/tool"
)

var imageMention = regexp.MustCompile(`(?i)(?:^|\s)@("[^"]+\.(?:png|jpe?g|gif|webp)"|\S+\.(?:png|jpe?g|gif|webp))`)

// mentionedImages loads the image files named with @path in a prompt.
// Mentions that are not existing files are left alone (they may be
// memory directives or plain text). notes are for the user.
func mentionedImages(input, cwd string, vision bool) (imgs []provider.Image, notes []string) {
	for _, m := range imageMention.FindAllStringSubmatch(input, 8) {
		p := strings.Trim(m[1], `"`)
		if strings.HasPrefix(p, "~/") {
			if h, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(h, p[2:])
			}
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
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
