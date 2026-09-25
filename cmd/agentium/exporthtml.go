package main

import (
	"encoding/json"
	"fmt"
	"github.com/tegarthegreat/agentium/internal/memory"
	"html"
	"strings"

	"github.com/tegarthegreat/agentium/internal/provider"
	"github.com/tegarthegreat/agentium/internal/session"
)

// exportHTML renders a conversation as one self-contained page (/export
// name.html): your messages, the replies, each step with its output
// folded, and edits as diffs. Nothing is loaded from elsewhere.
func exportHTML(msgs []provider.Message, sess *session.Session) string {
	esc := html.EscapeString
	var b strings.Builder
	title := esc(sess.Label())
	fmt.Fprintf(&b, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s · Agentium</title><style>
:root{--bg:#15161b;--fg:#d8d9de;--dim:#8b8e98;--card:#1d1f26;--line:#2c2f39;--ink:#7aa2f7;--amber:#f2a65a;--add:#16301f;--del:#3a1a1e;--addfg:#8fd19e;--delfg:#f0a0a8}
@media (prefers-color-scheme: light){:root{--bg:#faf8f4;--fg:#1f2328;--dim:#6a6f7a;--card:#fff;--line:#e4e1da;--ink:#2d5bd6;--amber:#b25f12;--add:#e6f4ea;--del:#fbe9eb;--addfg:#1a7f37;--delfg:#b3261e}}
body{background:var(--bg);color:var(--fg);font:15px/1.55 ui-sans-serif,system-ui,sans-serif;margin:0}
main{max-width:860px;margin:0 auto;padding:32px 20px 80px}
h1{font-size:20px;margin:0 0 4px}.meta{color:var(--dim);font-size:13px;margin-bottom:28px}
.you{border-left:3px solid var(--ink);padding:6px 14px;margin:26px 0 12px;white-space:pre-wrap}
.reply{white-space:pre-wrap;margin:10px 0 10px 0}.reply::before{content:"◆ ";color:var(--amber)}
.step{font:13px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace;margin:4px 0 4px 14px}
.chip{display:inline-block;padding:0 7px;border-radius:4px;background:var(--card);border:1px solid var(--line);color:var(--ink);margin-right:6px}
details{margin:2px 0 6px 14px}summary{cursor:pointer;color:var(--dim);font-size:12px}
code{font:13px ui-monospace,Menlo,monospace;background:var(--card);border:1px solid var(--line);border-radius:4px;padding:0 4px}
pre{background:var(--card);border:1px solid var(--line);border-radius:6px;padding:10px 12px;overflow-x:auto;font:12.5px/1.45 ui-monospace,Menlo,monospace;margin:4px 0}
.add{background:var(--add);color:var(--addfg);display:block}.del{background:var(--del);color:var(--delfg);display:block}
footer{color:var(--dim);font-size:12px;margin-top:40px}
</style></head><body><main><h1>%s</h1><div class="meta">%s · %s</div>
`, title, title, esc(sess.Model), esc(shortPath(sess.Cwd)))
	// Each step's output goes under the step.
	results := map[string]string{}
	for _, m := range msgs {
		if m.Role == provider.RoleTool && m.ToolCallID != "" {
			results[m.ToolCallID] = m.Text
		}
	}
	output := func(t string) {
		if t = strings.TrimSpace(memory.Redact(t)); t == "" { // keys and tokens are masked
			return
		}
		if len(t) > 20000 {
			t = strings.ToValidUTF8(t[:20000], "") + "\n… (clipped)"
		}
		fmt.Fprintf(&b, "<details><summary>output · %d lines</summary><pre>%s</pre></details>\n", strings.Count(t, "\n")+1, esc(t))
	}
	rel := func(s string) string {
		if len(sess.Cwd) < 2 {
			return s
		}
		return strings.ReplaceAll(s, strings.TrimSuffix(sess.Cwd, "/")+"/", "")
	}
	for _, m := range msgs {
		switch m.Role {
		case provider.RoleUser:
			if strings.HasPrefix(m.Text, "[agentium]") || strings.HasPrefix(m.Text, "[Summary of the earlier") {
				continue
			}
			fmt.Fprintf(&b, "<div class=\"you\">%s</div>\n", esc(firstNonEmpty(m.Typed, provider.UserWords(m.Text))))
		case provider.RoleAssistant:
			if t := strings.TrimSpace(m.Text); t != "" {
				fmt.Fprintf(&b, "<div class=\"reply\">%s</div>\n", mdHTML(t))
			}
			for _, c := range m.ToolCalls {
				fmt.Fprintf(&b, "<div class=\"step\"><span class=\"chip\">%s</span>%s</div>\n", esc(styleFor(c.Name).label), esc(rel(strings.TrimPrefix(summarizeCall(c), c.Name+" "))))
				if c.Name == "edit" {
					var e struct{ Old, New, Content string }
					if json.Unmarshal(c.Args, &e) == nil && (e.Old != "" || e.New != "" || e.Content != "") {
						b.WriteString("<pre>")
						var old []string
						if e.Old != "" {
							old = strings.Split(e.Old, "\n")
						}
						for _, d := range lineDiff(old, strings.Split(firstNonEmpty(e.New, e.Content), "\n")) {
							switch d.op {
							case '-':
								fmt.Fprintf(&b, "<span class=\"del\">- %s</span>", esc(d.text))
							case '+':
								fmt.Fprintf(&b, "<span class=\"add\">+ %s</span>", esc(d.text))
							default:
								fmt.Fprintf(&b, "<span>  %s</span>\n", esc(d.text))
							}
						}
						b.WriteString("</pre>\n")
						continue // the diff says it all
					}
				}
				output(results[c.ID])
			}
		case provider.RoleTool:
			if m.ToolCallID == "" {
				output(m.Text)
			}
		}
	}
	fmt.Fprintf(&b, "<footer>Exported from Agentium %s</footer></main></body></html>\n", esc(version))
	return b.String()
}

// mdHTML renders the Markdown a reply uses (code blocks, inline code,
// bold, headings, lists) as escaped HTML.
func mdHTML(md string) string {
	esc := html.EscapeString
	var b strings.Builder
	inCode := false
	inline := func(s string) string {
		s = esc(s)
		parts := strings.Split(s, "`")
		for i := 1; i < len(parts); i += 2 {
			if i < len(parts)-1 {
				parts[i] = "<code>" + parts[i] + "</code>"
			} else {
				parts[i] = "`" + parts[i]
			}
		}
		s = strings.Join(parts, "")
		for strings.Count(s, "**") >= 2 {
			s = strings.Replace(s, "**", "<b>", 1)
			s = strings.Replace(s, "**", "</b>", 1)
		}
		return s
	}
	for _, l := range strings.Split(md, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "```"):
			if inCode {
				b.WriteString("</pre>")
			} else {
				b.WriteString("<pre>")
			}
			inCode = !inCode
		case inCode:
			b.WriteString(esc(l) + "\n")
		case strings.HasPrefix(t, "#"):
			b.WriteString("<b>" + inline(strings.TrimLeft(t, "# ")) + "</b>\n")
		case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* "):
			b.WriteString("• " + inline(t[2:]) + "\n")
		default:
			b.WriteString(inline(l) + "\n")
		}
	}
	if inCode {
		b.WriteString("</pre>")
	}
	return strings.TrimRight(b.String(), "\n")
}
