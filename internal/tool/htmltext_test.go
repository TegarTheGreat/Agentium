package tool

import (
	"strings"
	"testing"
)

func TestHTMLToMarkdown(t *testing.T) {
	page := `<!doctype html><html><head><title>T</title><style>.x{}</style><script>var a="<p>no</p>";</script></head>
<body><nav><a href="/">Home</a> <a href="/docs">Docs</a></nav>
<main>
<h1>Package <code>errgroup</code></h1>
<p>Package errgroup provides   synchronization,
error propagation. See <a href="/pkg/context">context</a> and <a href="https://go.dev">go.dev</a>.</p>
<div hidden>secret banner</div><span aria-hidden="true">icon</span>
<pre><code class="language-go">func main() {
	g := new(errgroup.Group)
	g.Go(f)   // two spaces kept
}</code></pre>
<ul><li>first <b>bold</b></li><li>second<ol><li>nested one</li><li>nested two</li></ol></li></ul>
<table><tr><th>Name</th><th>Type</th></tr><tr><td>Go</td><td>func(f)</td></tr></table>
<p>AT&amp;T &lt;tag&gt; x&nbsp;y</p>
</main>
<footer>Copyright</footer></body></html>`
	got := htmlToMarkdown(page, "https://pkg.go.dev/golang.org/x/sync/errgroup")
	for _, want := range []string{
		"# Package `errgroup`",
		"Package errgroup provides synchronization, error propagation. See [context](https://pkg.go.dev/pkg/context) and [go.dev](https://go.dev).",
		"```go\nfunc main() {\n\tg := new(errgroup.Group)\n\tg.Go(f)   // two spaces kept\n}\n```",
		"- first **bold**",
		"  1. nested one\n  2. nested two",
		"| Name | Type |\n| --- | --- |\n| Go | func(f) |",
		"AT&T <tag> x y",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"Home", "Copyright", "secret", "icon", "var a", ".x{}"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q should be dropped:\n%s", bad, got)
		}
	}
}
