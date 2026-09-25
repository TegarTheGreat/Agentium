package tool

import (
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// htmlToMarkdown turns a web page into compact Markdown for the model:
// the main content only (the <main> or <article> when the page has one),
// without navigation, scripts and forms; links as [text](url), code
// blocks fenced with their whitespace intact, lists and tables kept.
func htmlToMarkdown(doc, base string) string {
	baseURL, _ := url.Parse(base)
	toks := tokenizeHTML(doc)
	start, end := mainRegion(toks)
	c := &mdConv{base: baseURL}
	for _, t := range toks[start:end] {
		c.token(t)
	}
	return c.finish()
}

type htmlTok struct {
	text  string // text content (tag == "")
	tag   string // lower-case tag name
	close bool
	self  bool
	attrs map[string]string
}

var attrRe = regexp.MustCompile(`([a-zA-Z_:][-a-zA-Z0-9_:.]*)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+)))?`)

// tokenizeHTML splits a document into text and tags. Comments, doctypes
// and the insides of script/style are dropped here.
func tokenizeHTML(s string) []htmlTok {
	var out []htmlTok
	for len(s) > 0 {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			out = append(out, htmlTok{text: s})
			break
		}
		if i > 0 {
			out = append(out, htmlTok{text: s[:i]})
			s = s[i:]
		}
		switch {
		case strings.HasPrefix(s, "<!--"):
			if j := strings.Index(s, "-->"); j >= 0 {
				s = s[j+3:]
			} else {
				s = ""
			}
			continue
		case strings.HasPrefix(s, "<!") || strings.HasPrefix(s, "<?"):
			if j := strings.IndexByte(s, '>'); j >= 0 {
				s = s[j+1:]
			} else {
				s = ""
			}
			continue
		}
		j := tagEnd(s)
		if j < 0 {
			out = append(out, htmlTok{text: s})
			break
		}
		raw := s[1:j]
		s = s[j+1:]
		t := htmlTok{}
		if strings.HasPrefix(raw, "/") {
			t.close = true
			raw = raw[1:]
		}
		if strings.HasSuffix(raw, "/") {
			t.self = true
			raw = raw[:len(raw)-1]
		}
		name := raw
		if k := strings.IndexAny(raw, " \t\r\n"); k >= 0 {
			name, raw = raw[:k], raw[k:]
		} else {
			raw = ""
		}
		t.tag = strings.ToLower(name)
		if t.tag == "" || !isTagName(t.tag) {
			out = append(out, htmlTok{text: "<"}) // a stray "<" in text
			s = name + raw + ">" + s
			continue
		}
		if raw != "" && !t.close {
			t.attrs = map[string]string{}
			for _, m := range attrRe.FindAllStringSubmatch(raw, -1) {
				t.attrs[strings.ToLower(m[1])] = html.UnescapeString(m[2] + m[3] + m[4])
			}
		}
		out = append(out, t)
		// Raw-text elements: skip to their end tag.
		if !t.close && !t.self && (t.tag == "script" || t.tag == "style" || t.tag == "textarea" || t.tag == "template") {
			k := strings.Index(strings.ToLower(s), "</"+t.tag)
			if k < 0 {
				s = ""
			} else {
				s = s[k:]
			}
		}
	}
	return out
}

// tagEnd finds the '>' closing the tag at the start of s, skipping quoted
// attribute values.
func tagEnd(s string) int {
	var q byte
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case q != 0:
			if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '>':
			return i
		}
	}
	return -1
}

func isTagName(s string) bool {
	for i, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' && i > 0 || r == '-' && i > 0) {
			return false
		}
	}
	return true
}

// mainRegion is the token range of the page's main content: its <main>
// (or role=main), else its first <article> with real content, else
// everything. One pass matches open and close tags (a stack, tolerant of
// unclosed ones), so a malformed page cannot make this quadratic.
func mainRegion(toks []htmlTok) (int, int) {
	closeAt := make([]int, len(toks)) // for an open tag: its close, or -1
	textBefore := make([]int, len(toks)+1)
	type open struct {
		tag string
		i   int
	}
	var stack []open
	for i, t := range toks {
		closeAt[i] = -1
		textBefore[i+1] = textBefore[i]
		switch {
		case t.tag == "":
			textBefore[i+1] += len(strings.TrimSpace(t.text))
		case t.self || voidTag(t.tag):
		case !t.close:
			stack = append(stack, open{t.tag, i})
		default:
			for k := len(stack) - 1; k >= 0; k-- {
				if stack[k].tag == t.tag {
					closeAt[stack[k].i] = i
					stack = stack[:k]
					break
				}
			}
		}
	}
	for _, want := range []func(htmlTok) bool{
		func(t htmlTok) bool { return t.tag == "main" || t.attrs["role"] == "main" },
		func(t htmlTok) bool { return t.tag == "article" },
	} {
		for i, t := range toks {
			if t.tag == "" || t.close || closeAt[i] < 0 || !want(t) {
				continue
			}
			if textBefore[closeAt[i]]-textBefore[i] > 200 { // a real content area, not a stub
				return i, closeAt[i] + 1
			}
		}
	}
	return 0, len(toks)
}

func textLen(toks []htmlTok) int {
	n := 0
	for _, t := range toks {
		if t.tag == "" {
			n += len(strings.TrimSpace(t.text))
		}
	}
	return n
}

// skipTags hold no content worth reading.
var skipTags = map[string]bool{"script": true, "style": true, "noscript": true, "svg": true, "template": true,
	"iframe": true, "head": true, "nav": true, "footer": true, "aside": true, "form": true, "button": true,
	"select": true, "textarea": true, "canvas": true, "dialog": true, "object": true, "video": true, "audio": true}

var blockTags = map[string]bool{"p": true, "div": true, "section": true, "article": true, "main": true, "header": true,
	"ul": true, "ol": true, "dl": true, "dt": true, "dd": true, "table": true, "thead": true, "tbody": true,
	"blockquote": true, "figure": true, "figcaption": true, "details": true, "summary": true, "hr": true, "address": true}

type mdConv struct {
	base  *url.URL
	out   strings.Builder
	skip  []string // open skipped elements (their content is dropped)
	pre   int
	lists []int // open lists: -1 unordered, else the next number
	link  *mdLink
	row   []string // cells of the table row being read
	cell  *strings.Builder
	rows  int // rows written in the current table
	space bool
}

type mdLink struct {
	href string
	text strings.Builder
}

func (c *mdConv) token(t htmlTok) {
	if len(c.skip) > 0 {
		top := c.skip[len(c.skip)-1]
		if t.tag == top && !t.self {
			if t.close {
				c.skip = c.skip[:len(c.skip)-1]
			} else {
				c.skip = append(c.skip, top) // nested same tag
			}
		}
		return
	}
	if t.tag == "" {
		c.text(html.UnescapeString(t.text))
		return
	}
	if !t.close && hiddenElement(t) {
		if !t.self && !voidTag(t.tag) {
			c.skip = append(c.skip, t.tag)
		}
		return
	}
	switch t.tag {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		if t.close {
			c.block()
		} else {
			c.block()
			c.raw(strings.Repeat("#", int(t.tag[1]-'0')) + " ")
		}
	case "br":
		c.raw("\n")
	case "pre":
		if t.close {
			c.pre = max(c.pre-1, 0)
			if c.pre == 0 {
				c.raw("\n```")
				c.block()
			}
		} else {
			if c.pre == 0 {
				c.block()
				c.raw("```" + codeLang(t.attrs["class"]) + "\n")
			}
			c.pre++
		}
	case "code", "kbd", "samp", "tt":
		if c.pre == 0 {
			c.marker("`", t.close)
		} else if lang := codeLang(t.attrs["class"]); !t.close && lang != "" {
			// <pre><code class="language-go">: the fence just opened gets it.
			if o := c.out.String(); strings.HasSuffix(o, "```\n") {
				c.out.Reset()
				c.out.WriteString(o[:len(o)-1] + lang + "\n")
			}
		}
	case "strong", "b":
		if c.pre == 0 && c.cell == nil {
			c.marker("**", t.close)
		}
	case "a":
		if t.close {
			c.endLink()
		} else if c.pre == 0 {
			c.endLink()
			c.flushSpace()
			c.link = &mdLink{href: c.resolve(t.attrs["href"])}
		}
	case "img":
		if alt := strings.TrimSpace(t.attrs["alt"]); alt != "" && c.link == nil && c.pre == 0 {
			c.text("[image: " + alt + "]")
		}
	case "ul", "ol":
		if t.close {
			if len(c.lists) > 0 {
				c.lists = c.lists[:len(c.lists)-1]
			}
			c.block()
		} else {
			n := -1
			if t.tag == "ol" {
				n = 1
				if s, err := strconv.Atoi(t.attrs["start"]); err == nil {
					n = s
				}
			}
			c.lists = append(c.lists, n)
			c.newline()
		}
	case "li":
		if !t.close {
			c.newline()
			depth := max(len(c.lists)-1, 0)
			marker := "- "
			if len(c.lists) > 0 && c.lists[len(c.lists)-1] > 0 {
				marker = strconv.Itoa(c.lists[len(c.lists)-1]) + ". "
				c.lists[len(c.lists)-1]++
			}
			c.raw(strings.Repeat("  ", depth) + marker)
		}
	case "table":
		// Rows and cells may be left unclosed (HTML allows it): the table's
		// end closes them, or the rest of the page would vanish into a cell.
		c.endRow()
		c.block()
		c.rows = 0
	case "tr":
		c.endRow() // also ends a previous row left open
		c.row = nil
	case "td", "th":
		if t.close {
			c.endCell()
		} else {
			c.endCell()
			c.cell = &strings.Builder{}
		}
	case "blockquote":
		c.block()
		if !t.close {
			c.raw("> ")
		}
	default:
		if blockTags[t.tag] {
			c.block()
		}
	}
}

// hiddenElement is an element whose content is not page content:
// scripts, navigation, forms, and anything hidden.
func hiddenElement(t htmlTok) bool {
	if skipTags[t.tag] {
		return true
	}
	if _, ok := t.attrs["hidden"]; ok {
		return true
	}
	switch t.attrs["role"] {
	case "navigation", "banner", "contentinfo", "search", "complementary", "dialog", "menu":
		return true
	}
	style := strings.ReplaceAll(t.attrs["style"], " ", "")
	return t.attrs["aria-hidden"] == "true" || strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden")
}

func voidTag(t string) bool {
	switch t {
	case "br", "img", "hr", "input", "meta", "link", "source", "wbr", "area", "col", "embed", "param", "track":
		return true
	}
	return false
}

var langRe = regexp.MustCompile(`(?:language|lang)-([A-Za-z0-9+#-]+)`)

func codeLang(class string) string {
	if m := langRe.FindStringSubmatch(class); m != nil {
		return m[1]
	}
	return ""
}

func (c *mdConv) resolve(href string) string {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(strings.ToLower(href), "javascript:") {
		return ""
	}
	if c.base != nil {
		if u, err := c.base.Parse(href); err == nil {
			return u.String()
		}
	}
	return href
}

// text writes page text, collapsing whitespace outside <pre>.
func (c *mdConv) text(s string) {
	if c.pre > 0 {
		if strings.HasSuffix(c.out.String(), "```"+"\n") || strings.HasSuffix(c.out.String(), "\n") && c.fenceJustOpened() {
			s = strings.TrimLeft(s, "\r\n") // <pre>\n: the first newline is not content
		}
		c.write(s)
		return
	}
	var b strings.Builder
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' || r == ' ' {
			c.space = true
			continue
		}
		if c.space && !c.atLineStart(b.String()) {
			b.WriteByte(' ')
		}
		c.space = false
		b.WriteRune(r)
	}
	if b.Len() > 0 {
		c.write(b.String())
	}
}

func (c *mdConv) atLineStart(pending string) bool {
	if pending != "" {
		return false
	}
	if c.link != nil {
		return c.link.text.Len() == 0
	}
	if c.cell != nil {
		return c.cell.Len() == 0
	}
	s := c.out.String()
	return s == "" || strings.HasSuffix(s, "\n") || strings.HasSuffix(s, "# ") || strings.HasSuffix(s, "- ") || strings.HasSuffix(s, ". ") || strings.HasSuffix(s, "> ")
}

// write sends text to the open link, table cell or the output.
func (c *mdConv) write(s string) {
	switch {
	case c.link != nil:
		c.link.text.WriteString(s)
	case c.cell != nil:
		c.cell.WriteString(s)
	default:
		c.out.WriteString(s)
	}
}

// fenceJustOpened reports whether the output ends with a code fence
// line and nothing after it yet.
func (c *mdConv) fenceJustOpened() bool {
	o := strings.TrimSuffix(c.out.String(), "\n")
	i := strings.LastIndexByte(o, '\n')
	return strings.HasPrefix(o[i+1:], "```")
}

// marker writes an inline marker (` or **): an opening one after the
// pending space, a closing one before it.
func (c *mdConv) marker(m string, closing bool) {
	if closing {
		c.write(m)
		return
	}
	c.flushSpace()
	c.write(m)
}

// flushSpace writes a pending space (collapsed whitespace) now.
func (c *mdConv) flushSpace() {
	if c.space && !c.atLineStart("") {
		c.write(" ")
	}
	c.space = false
}

func (c *mdConv) raw(s string) {
	c.space = false
	c.write(s)
}

func (c *mdConv) endLink() {
	l := c.link
	if l == nil {
		return
	}
	c.link = nil
	text := strings.TrimSpace(l.text.String())
	switch {
	case text == "":
	case l.href == "" || l.href == text:
		c.write(text)
	default:
		c.write("[" + text + "](" + l.href + ")")
	}
}

func (c *mdConv) endCell() {
	if c.cell == nil {
		return
	}
	c.endLink()
	cell := strings.Join(strings.Fields(c.cell.String()), " ")
	c.cell = nil
	c.row = append(c.row, strings.ReplaceAll(cell, "|", "\\|"))
}

func (c *mdConv) endRow() {
	c.endCell()
	if len(c.row) == 0 {
		return
	}
	c.newline()
	c.out.WriteString("| " + strings.Join(c.row, " | ") + " |\n")
	if c.rows == 0 {
		c.out.WriteString("|" + strings.Repeat(" --- |", len(c.row)) + "\n")
	}
	c.rows++
	c.row = nil
}

func (c *mdConv) newline() {
	c.endLink()
	if c.cell != nil {
		c.cell.WriteByte(' ')
		return
	}
	if s := c.out.String(); s != "" && !strings.HasSuffix(s, "\n") {
		c.out.WriteByte('\n')
	}
	c.space = false
}

func (c *mdConv) block() {
	c.newline()
	if c.cell != nil {
		return
	}
	if s := c.out.String(); s != "" && !strings.HasSuffix(s, "\n\n") {
		c.out.WriteByte('\n')
	}
}

var blankRuns = regexp.MustCompile(`\n{3,}`)

func (c *mdConv) finish() string {
	c.endLink()
	c.endRow()
	if c.pre > 0 {
		c.out.WriteString("\n```\n")
	}
	lines := strings.Split(c.out.String(), "\n")
	inFence := false
	for i, l := range lines {
		if strings.HasPrefix(l, "```") {
			inFence = !inFence
		}
		if !inFence {
			lines[i] = strings.TrimRight(l, " \t")
			if t := strings.TrimSpace(lines[i]); t == "-" || t == "**" || t == "****" || t == "``" {
				lines[i] = "" // an empty list item or emphasis
			}
		}
	}
	return strings.TrimSpace(blankRuns.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}
