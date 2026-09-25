package tool

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif" // register decoders for DecodeConfig
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tegarthegreat/agentium/internal/provider"
)

// MaxImageBytes keeps an image under the providers' per-image limits
// (Anthropic: 5 MB after base64).
const MaxImageBytes = 3_750_000

type sinkKey struct{}

type imageSink struct {
	mu   sync.Mutex
	imgs []provider.Image
}

// WithImageSink returns a context whose tools may attach images, and a
// function returning what they attached.
func WithImageSink(ctx context.Context) (context.Context, func() []provider.Image) {
	s := &imageSink{}
	return context.WithValue(ctx, sinkKey{}, s), func() []provider.Image {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.imgs
	}
}

func attachImage(ctx context.Context, im provider.Image) bool {
	s, ok := ctx.Value(sinkKey{}).(*imageSink)
	if !ok {
		return false
	}
	s.mu.Lock()
	s.imgs = append(s.imgs, im)
	s.mu.Unlock()
	return true
}

// imageType returns the media type of an image the providers accept, or "".
func imageType(path string, head []byte) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
	default:
		return ""
	}
	switch t := http.DetectContentType(head); t {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return t
	}
	return ""
}

// LoadImage reads an image file for sending to a model.
func LoadImage(path string) (provider.Image, error) {
	// Opened without blocking (a FIFO named .png must not hang), checked
	// on the open file, read no further than the limit.
	f, err := os.OpenFile(path, os.O_RDONLY|nonblockFlag, 0)
	if err != nil {
		return provider.Image{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return provider.Image{}, err
	}
	if !st.Mode().IsRegular() {
		return provider.Image{}, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if st.Size() > MaxImageBytes {
		return provider.Image{}, fmt.Errorf("image is %d bytes; the limit is %d (downscale it first)", st.Size(), MaxImageBytes)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxImageBytes+1))
	if err != nil {
		return provider.Image{}, err
	}
	if len(b) > MaxImageBytes {
		return provider.Image{}, fmt.Errorf("image is over the %d-byte limit", MaxImageBytes)
	}
	t := imageType(path, b[:min(len(b), 512)])
	if t == "" {
		return provider.Image{}, fmt.Errorf("%s is not a PNG, JPEG, GIF or WebP image", filepath.Base(path))
	}
	return provider.Image{MediaType: t, Data: b}, nil
}

// describeImage is the text that accompanies an attached image.
func describeImage(path string, im provider.Image) string {
	dims := ""
	if c, _, err := image.DecodeConfig(bytes.NewReader(im.Data)); err == nil {
		dims = fmt.Sprintf(", %dx%d", c.Width, c.Height)
	}
	return fmt.Sprintf("(image %s, %s, %d bytes%s — attached)", filepath.Base(path), im.MediaType, len(im.Data), dims)
}

// readImage handles read on an image file.
func readImage(ctx context.Context, env *Env, p string, head []byte) (string, bool, error) {
	if imageType(p, head) == "" {
		return "", false, nil
	}
	if !env.Vision {
		return fmt.Sprintf("(image file %s; this model cannot view images)", filepath.Base(p)), true, nil
	}
	im, err := LoadImage(p)
	if err != nil {
		return "", true, err
	}
	if !attachImage(ctx, im) {
		return "(image file; images cannot be attached here)", true, nil
	}
	return describeImage(p, im), true, nil
}
