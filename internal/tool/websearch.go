package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Web search: Brave or Tavily when the user has an API key for one,
// otherwise DuckDuckGo's HTML endpoint (no key). Results are untrusted
// web content, like fetch.

// Endpoints are variables so tests can point them at a local server.
var (
	braveURL  = "https://api.search.brave.com/res/v1/web/search"
	tavilyURL = "https://api.tavily.com/search"
	ddgURL    = "https://html.duckduckgo.com/html/"
)

const searchResults = 8

type hit struct{ Title, URL, Snippet string }

func webSearch(ctx context.Context, q string, allowPrivate bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	client := fetchClient(allowPrivate)
	var hits []hit
	var err error
	var engine string
	switch {
	case os.Getenv("BRAVE_API_KEY") != "":
		engine = "Brave"
		hits, err = searchBrave(ctx, client, q, os.Getenv("BRAVE_API_KEY"))
	case os.Getenv("TAVILY_API_KEY") != "":
		engine = "Tavily"
		hits, err = searchTavily(ctx, client, q, os.Getenv("TAVILY_API_KEY"))
	default:
		engine = "DuckDuckGo"
		hits, err = searchDDG(ctx, client, q)
	}
	if err != nil {
		return "", fmt.Errorf("%s search failed: %v", engine, err)
	}
	if len(hits) == 0 {
		return fmt.Sprintf("(no %s results for %q)", engine, q), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s results for %q (untrusted web content; fetch a URL to read it):\n", engine, q)
	for i, h := range hits {
		if i == searchResults {
			break
		}
		fmt.Fprintf(&sb, "%d. %s\n   %s\n", i+1, oneLineCmd(h.Title), h.URL)
		if s := strings.Join(strings.Fields(h.Snippet), " "); s != "" {
			if len(s) > 240 {
				s = s[:240] + "…"
			}
			sb.WriteString("   " + s + "\n")
		}
	}
	return sb.String(), nil
}

func getJSON(ctx context.Context, c *http.Client, req *http.Request, v any) error {
	req = req.WithContext(ctx)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, body)
	}
	return json.Unmarshal(body, v)
}

func searchBrave(ctx context.Context, c *http.Client, q, key string) ([]hit, error) {
	req, _ := http.NewRequest(http.MethodGet, braveURL+"?count=8&q="+url.QueryEscape(q), nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)
	var r struct {
		Web struct {
			Results []struct{ Title, URL, Description string } `json:"results"`
		} `json:"web"`
	}
	if err := getJSON(ctx, c, req, &r); err != nil {
		return nil, err
	}
	var out []hit
	for _, x := range r.Web.Results {
		out = append(out, hit{stripTags(x.Title), x.URL, stripTags(x.Description)})
	}
	return out, nil
}

func searchTavily(ctx context.Context, c *http.Client, q, key string) ([]hit, error) {
	body, _ := json.Marshal(map[string]any{"api_key": key, "query": q, "max_results": searchResults})
	req, _ := http.NewRequest(http.MethodPost, tavilyURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	var r struct {
		Results []struct{ Title, URL, Content string } `json:"results"`
	}
	if err := getJSON(ctx, c, req, &r); err != nil {
		return nil, err
	}
	var out []hit
	for _, x := range r.Results {
		out = append(out, hit{x.Title, x.URL, x.Content})
	}
	return out, nil
}

var (
	ddgLink    = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippet = regexp.MustCompile(`(?s)class="result__snippet"[^>]*>(.*?)</a>`)
	tagRe      = regexp.MustCompile(`<[^>]+>`)
)

func stripTags(s string) string { return html.UnescapeString(tagRe.ReplaceAllString(s, "")) }

func searchDDG(ctx context.Context, c *http.Client, q string) ([]hit, error) {
	req, _ := http.NewRequest(http.MethodPost, ddgURL, strings.NewReader("q="+url.QueryEscape(q)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; agentium)")
	req = req.WithContext(ctx)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	links := ddgLink.FindAllStringSubmatch(string(b), -1)
	snips := ddgSnippet.FindAllStringSubmatch(string(b), -1)
	if len(links) == 0 && bytes.Contains(b, []byte("anomaly")) {
		return nil, errors.New("DuckDuckGo refused the request (rate limit); set BRAVE_API_KEY or TAVILY_API_KEY for reliable search")
	}
	var out []hit
	for i, l := range links {
		u := html.UnescapeString(l[1])
		if strings.HasPrefix(u, "//") {
			u = "https:" + u
		}
		// Result links go through a redirect: /l/?uddg=<target>.
		if pu, err := url.Parse(u); err == nil && pu.Query().Get("uddg") != "" {
			u = pu.Query().Get("uddg")
		}
		if strings.Contains(u, "duckduckgo.com/y.js") { // ads
			continue
		}
		h := hit{Title: stripTags(l[2]), URL: u}
		if i < len(snips) {
			h.Snippet = stripTags(snips[i][1])
		}
		out = append(out, h)
	}
	return out, nil
}
