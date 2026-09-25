package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tags/v0.20.0", "/latest":
			w.Write([]byte(`{"tag_name":"v0.20.0","body":"### Added\n- styles","html_url":"https://github.com/x/y"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	defer func(s string) { releaseAPI = s }(releaseAPI)
	releaseAPI = srv.URL
	for _, v := range []string{"0.20.0", "v0.20.0", "latest"} {
		r, err := fetchRelease(context.Background(), v)
		if err != nil || r.Tag != "v0.20.0" || r.Body == "" {
			t.Fatalf("%s: %+v %v", v, r, err)
		}
	}
	if _, err := fetchRelease(context.Background(), "9.9.9"); err == nil || err.Error() != "no release 9.9.9 on GitHub" {
		t.Fatalf("missing: %v", err)
	}
}

func TestJustUpdated(t *testing.T) {
	t.Setenv("AGENTIUM_HOME", t.TempDir())
	defer func(v string) { version = v }(version)
	version = "0.20.0"
	if p := justUpdated(); p != "" {
		t.Fatalf("first run: %q", p)
	}
	version = "0.21.0"
	if p := justUpdated(); p != "0.20.0" {
		t.Fatalf("after update: %q", p)
	}
	if p := justUpdated(); p != "" {
		t.Fatalf("said twice: %q", p)
	}
	version = "0.20.0" // a downgrade is not news
	if p := justUpdated(); p != "" {
		t.Fatalf("downgrade: %q", p)
	}
}
