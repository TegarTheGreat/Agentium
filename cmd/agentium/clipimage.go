package main

import (
	"strings"

	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/tegarthegreat/agentium/internal/config"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// Ctrl-V pastes an image from the clipboard (a screenshot you copied):
// it is saved as a PNG and mentioned in the message, so it is attached.
// Text still pastes through the terminal as usual.

// clipboardImage returns the clipboard's image as PNG bytes.
func clipboardImage() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	try := func(name string, args ...string) []byte {
		if _, err := exec.LookPath(name); err != nil {
			return nil
		}
		out, err := exec.CommandContext(ctx, name, args...).Output()
		if err != nil || !bytes.HasPrefix(out, []byte("\x89PNG")) {
			return nil
		}
		return out
	}
	switch runtime.GOOS {
	case "darwin":
		if b := try("pngpaste", "-"); b != nil {
			return b, nil
		}
		// Without pngpaste: AppleScript writes the clipboard's PNG to a file.
		tmp := filepath.Join(imageDir(), fmt.Sprintf("clip-%d.png", time.Now().UnixNano()))
		script := fmt.Sprintf(`set f to open for access POSIX file %q with write permission
write (the clipboard as «class PNGf») to f
close access f`, tmp)
		err := exec.CommandContext(ctx, "osascript", "-e", script).Run()
		b, rerr := os.ReadFile(tmp)
		os.Remove(tmp) // also when there was no image
		if err == nil && rerr == nil && bytes.HasPrefix(b, []byte("\x89PNG")) {
			return b, nil
		}
	case "windows":
		tmp := filepath.Join(imageDir(), fmt.Sprintf("clip-%d.png", time.Now().UnixNano()))
		// The path goes in through the environment: no quoting to get wrong.
		ps := `Add-Type -AssemblyName System.Windows.Forms; $i=[Windows.Forms.Clipboard]::GetImage(); if ($i) { $i.Save($env:AGENTIUM_CLIP, [Drawing.Imaging.ImageFormat]::Png) }`
		cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-STA", "-Command", ps)
		cmd.Env = append(os.Environ(), "AGENTIUM_CLIP="+tmp)
		if cmd.Run() == nil {
			b, err := os.ReadFile(tmp)
			os.Remove(tmp)
			if err == nil && bytes.HasPrefix(b, []byte("\x89PNG")) {
				return b, nil
			}
		}
	default:
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			if b := try("wl-paste", "--no-newline", "--type", "image/png"); b != nil {
				return b, nil
			}
		}
		if os.Getenv("DISPLAY") != "" {
			if b := try("xclip", "-selection", "clipboard", "-t", "image/png", "-o"); b != nil {
				return b, nil
			}
		}
	}
	return nil, errors.New("no image on the clipboard (over SSH the clipboard is on your own computer: save the image and use @path)")
}

// imageDir is where pasted images are kept: agentium's own folder, not a
// shared temporary one another user could prepare.
func imageDir() string {
	d := filepath.Join(config.Home(), "images")
	_ = os.MkdirAll(d, 0o700)
	return d
}

// pasteImage saves the clipboard image and returns an @mention for it.
func pasteImage() (string, error) {
	b, err := clipboardImage()
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(imageDir(), time.Now().Format("20060102-150405")+"-*.png")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return "", err
	}
	p := f.Name()
	// Old pastes go after a week.
	if ents, err := os.ReadDir(imageDir()); err == nil {
		for _, e := range ents {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 7*24*time.Hour {
				os.Remove(filepath.Join(imageDir(), e.Name()))
			}
		}
	}
	if strings.ContainsAny(p, " \t") {
		return "@\"" + p + "\" ", nil
	}
	return "@" + p + " ", nil
}
