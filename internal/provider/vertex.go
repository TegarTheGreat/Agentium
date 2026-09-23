package provider

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Vertex runs Anthropic models on Google Cloud Vertex AI (streamRawPredict)
// using an access token from GOOGLE_OAUTH_ACCESS_TOKEN or the gcloud CLI.
type Vertex struct {
	Project, Region string
	// Endpoint overrides the aiplatform host (tests).
	Endpoint string
	// Token overrides token lookup (tests).
	Token func(context.Context) (string, error)

	mu      sync.Mutex
	tok     string
	expires time.Time
}

func (v *Vertex) token(ctx context.Context) (string, error) {
	if v.Token != nil {
		return v.Token(ctx)
	}
	if t := os.Getenv("GOOGLE_OAUTH_ACCESS_TOKEN"); t != "" {
		return t, nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.tok != "" && time.Now().Before(v.expires) {
		return v.tok, nil
	}
	if _, err := exec.LookPath("gcloud"); err != nil {
		return "", fmt.Errorf("vertex: set GOOGLE_OAUTH_ACCESS_TOKEN or install gcloud and run `gcloud auth application-default login`")
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "gcloud", "auth", "application-default", "print-access-token").Output()
	if err != nil {
		out, err = exec.CommandContext(cctx, "gcloud", "auth", "print-access-token").Output()
	}
	if err != nil {
		return "", fmt.Errorf("vertex: gcloud could not print an access token: %v", err)
	}
	v.tok = strings.TrimSpace(string(out))
	v.expires = time.Now().Add(45 * time.Minute) // tokens live ~60 minutes
	return v.tok, nil
}

func (v *Vertex) url(model string) string {
	host := v.Endpoint
	if host == "" {
		host = "https://" + v.Region + "-aiplatform.googleapis.com"
		if v.Region == "global" {
			host = "https://aiplatform.googleapis.com"
		}
	}
	return fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:streamRawPredict",
		strings.TrimRight(host, "/"), v.Project, v.Region, model)
}

// Stream implements Client.
func (v *Vertex) Stream(ctx context.Context, req Request, onText func(string)) (Response, error) {
	a := &Anthropic{URL: v.url(req.Model), Version: "vertex-2023-10-16", Token: v.token}
	return a.Stream(ctx, req, onText)
}
