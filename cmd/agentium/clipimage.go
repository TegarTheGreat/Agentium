package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
		tmp := filepath.Join(os.TempDir(), fmt.Sprintf("agentium-clip-%d.png", time.Now().UnixNano()))
		script := fmt.Sprintf(`set f to open for access POSIX file %q with write permission
write (the clipboard as «class PNGf») to f
close access f`, tmp)
		if exec.CommandContext(ctx, "osascript", "-e", script).Run() == nil {
			b, err := os.ReadFile(tmp)
			os.Remove(tmp)
			if err == nil && bytes.HasPrefix(b, []byte("\x89PNG")) {
				return b, nil
			}
		}
	case "windows":
		tmp := filepath.Join(os.TempDir(), fmt.Sprintf("agentium-clip-%d.png", time.Now().UnixNano()))
		ps := fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms; $i=[Windows.Forms.Clipboard]::GetImage(); if ($i) { $i.Save('%s', [Drawing.Imaging.ImageFormat]::Png) }`, tmp)
		if exec.CommandContext(ctx, "powershell", "-NoProfile", "-STA", "-Command", ps).Run() == nil {
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

// pasteImage saves the clipboard image and returns an @mention for it.
func pasteImage() (string, error) {
	b, err := clipboardImage()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(os.TempDir(), "agentium-images")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, time.Now().Format("20060102-150405")+".png")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return "", err
	}
	return "@" + p + " ", nil
}
