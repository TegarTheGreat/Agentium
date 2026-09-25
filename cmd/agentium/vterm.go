package main

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// vterm is a small terminal emulator for the full-screen UI. Everything
// the inline UI writes (tool lines, the live area, Markdown, prompts) is
// interpreted into a scrollback of styled cells, which the compositor
// then shows in the conversation pane. It understands what Agentium
// itself emits: text, CR/LF/BS/TAB, cursor moves (A B C D G), erase
// (K J) and colors (SGR). Anything else is ignored.
type vterm struct {
	width    int
	lines    [][]vcell
	row, col int
	st       vstyle
	pending  []byte // an incomplete UTF-8 sequence or escape
	maxLines int
	dropped  int            // lines trimmed from the top so far
	links    []string       // hyperlink targets (OSC 8), by vstyle.link-1
	linkIDs  map[string]int // target → index+1
}

type vcell struct {
	r  rune
	w  int8 // columns; 0 for the right half of a wide character
	st vstyle
}

// vstyle is an SGR state: colors as 0 (default), 1<<24|n (palette n) or
// 2<<24|rgb, and attribute bits.
type vstyle struct {
	fg, bg uint32
	attr   uint8
	link   uint16 // hyperlink: 1 + index into vterm.links; 0 for none
}

const (
	aBold = 1 << iota
	aDim
	aItalic
	aUnder
	aReverse
)

func newVterm(width int) *vterm {
	return &vterm{width: max(width, 10), lines: [][]vcell{nil}, maxLines: 5000}
}

func (v *vterm) Write(p []byte) (int, error) {
	n := len(p)
	if len(v.pending) > 0 {
		p = append(v.pending, p...)
		v.pending = nil
	}
	for len(p) > 0 {
		b := p[0]
		switch {
		case b == 0x1b:
			used, ok := v.escape(p)
			if !ok {
				v.pending = append([]byte(nil), p...)
				return n, nil
			}
			p = p[used:]
			continue
		case b == '\r':
			v.col = 0
		case b == '\n':
			v.newline()
		case b == '\b':
			if v.col > 0 {
				v.col--
			}
		case b == '\t':
			v.col = min((v.col/8+1)*8, v.width-1)
		case b < 0x20 || b == 0x7f:
		default:
			if !utf8.FullRune(p) {
				v.pending = append([]byte(nil), p...)
				return n, nil
			}
			r, size := utf8.DecodeRune(p)
			v.put(r)
			p = p[size:]
			continue
		}
		p = p[1:]
	}
	return n, nil
}

func (v *vterm) newline() {
	v.st.link = 0 // links never run across lines
	v.row++
	v.col = 0
	for len(v.lines) <= v.row {
		v.lines = append(v.lines, nil)
	}
	if len(v.lines) > v.maxLines {
		drop := len(v.lines) - v.maxLines + v.maxLines/5
		v.lines = append([][]vcell(nil), v.lines[drop:]...)
		v.row -= drop
		v.dropped += drop
	}
}

func (v *vterm) put(r rune) {
	w := runeWidth(r)
	if w == 0 {
		return
	}
	if v.col+w > v.width {
		link := v.st.link
		v.newline()
		v.st.link = link // a wrapped link continues on the next row
	}
	line := v.lines[v.row]
	for len(line) < v.col+w {
		line = append(line, vcell{r: ' ', w: 1})
	}
	// Overwriting half of a wide character blanks its other half.
	if v.col > 0 && line[v.col].w == 0 {
		line[v.col-1] = vcell{r: ' ', w: 1, st: line[v.col-1].st}
	}
	if end := v.col + w; end < len(line) && line[end].w == 0 {
		line[end] = vcell{r: ' ', w: 1, st: line[end].st}
	}
	line[v.col] = vcell{r: r, w: int8(w), st: v.st}
	if w == 2 {
		line[v.col+1] = vcell{w: 0, st: v.st}
	}
	v.lines[v.row] = line
	v.col += w
}

// escape handles the sequence at the start of p; ok is false when it is
// incomplete.
func (v *vterm) escape(p []byte) (used int, ok bool) {
	if len(p) < 2 {
		return 0, false
	}
	switch p[1] {
	case '[':
		i := 2
		for i < len(p) && (p[i] < 0x40 || p[i] > 0x7e) {
			i++
		}
		if i >= len(p) {
			if len(p) > 64 {
				return len(p), true // a runaway sequence: drop it
			}
			return 0, false
		}
		v.csi(string(p[2:i]), p[i])
		return i + 1, true
	case ']': // OSC: up to BEL or ST
		for i := 2; i < len(p); i++ {
			end := 0
			if p[i] == 0x07 {
				end = i + 1
			} else if p[i] == 0x1b && i+1 < len(p) && p[i+1] == '\\' {
				end = i + 2
			}
			if end > 0 {
				v.osc(string(p[2:i]))
				return end, true
			}
		}
		if len(p) > 4096 {
			return len(p), true // an unterminated OSC: drop it
		}
		return 0, false
	}
	return 2, true
}

// osc handles OSC 8 hyperlinks ("8;params;URI"); other OSCs are ignored.
func (v *vterm) osc(body string) {
	rest, ok := strings.CutPrefix(body, "8;")
	if !ok {
		return
	}
	_, uri, _ := strings.Cut(rest, ";")
	if uri == "" {
		v.st.link = 0
		return
	}
	if v.linkIDs == nil {
		v.linkIDs = map[string]int{}
	}
	id, seen := v.linkIDs[uri]
	if !seen {
		if len(v.links) >= 4096 || len(uri) > 2048 {
			return // enough: further links show as plain text
		}
		v.links = append(v.links, uri)
		id = len(v.links)
		v.linkIDs[uri] = id
	}
	v.st.link = uint16(id)
}

func (v *vterm) csi(params string, final byte) {
	if strings.HasPrefix(params, "?") || strings.HasPrefix(params, "<") || strings.HasPrefix(params, ">") {
		return // modes (cursor, paste): the compositor owns them
	}
	nums := func(def int) []int {
		var out []int
		for _, f := range strings.Split(params, ";") {
			n, err := strconv.Atoi(f)
			if err != nil {
				n = def
			}
			out = append(out, n)
		}
		return out
	}
	arg := func() int {
		n := nums(1)[0]
		if n < 1 {
			n = 1
		}
		return n
	}
	switch final {
	case 'A':
		v.row = max(v.row-arg(), 0)
	case 'B':
		for i := arg(); i > 0; i-- {
			col := v.col
			v.newline()
			v.col = col
		}
	case 'C':
		v.col = min(v.col+arg(), v.width-1)
	case 'D':
		v.col = max(v.col-arg(), 0)
	case 'G':
		v.col = min(arg()-1, v.width-1)
	case 'K':
		line := v.lines[v.row]
		switch nums(0)[0] {
		case 0:
			if v.col < len(line) {
				v.lines[v.row] = line[:v.col]
			}
		case 1:
			for i := 0; i <= v.col && i < len(line); i++ {
				line[i] = vcell{r: ' ', w: 1}
			}
		case 2:
			v.lines[v.row] = nil
		}
	case 'J':
		if nums(0)[0] == 0 {
			if line := v.lines[v.row]; v.col < len(line) {
				v.lines[v.row] = line[:v.col]
			}
			v.lines = v.lines[:v.row+1]
		}
	case 'm':
		v.sgr(nums(0))
	}
}

func (v *vterm) sgr(ps []int) {
	for i := 0; i < len(ps); i++ {
		switch p := ps[i]; {
		case p == 0:
			v.st = vstyle{link: v.st.link} // colors reset; a link goes on
		case p == 1:
			v.st.attr |= aBold
		case p == 2:
			v.st.attr |= aDim
		case p == 3:
			v.st.attr |= aItalic
		case p == 4:
			v.st.attr |= aUnder
		case p == 7:
			v.st.attr |= aReverse
		case p == 22:
			v.st.attr &^= aBold | aDim
		case p == 23:
			v.st.attr &^= aItalic
		case p == 24:
			v.st.attr &^= aUnder
		case p == 27:
			v.st.attr &^= aReverse
		case p >= 30 && p <= 37:
			v.st.fg = 1<<24 | uint32(p-30)
		case p >= 90 && p <= 97:
			v.st.fg = 1<<24 | uint32(p-90+8)
		case p == 39:
			v.st.fg = 0
		case p >= 40 && p <= 47:
			v.st.bg = 1<<24 | uint32(p-40)
		case p >= 100 && p <= 107:
			v.st.bg = 1<<24 | uint32(p-100+8)
		case p == 49:
			v.st.bg = 0
		case p == 38 || p == 48:
			var c uint32
			if i+2 < len(ps) && ps[i+1] == 5 {
				c = 1<<24 | uint32(ps[i+2]&0xff)
				i += 2
			} else if i+4 < len(ps) && ps[i+1] == 2 {
				c = 2<<24 | uint32(ps[i+2]&0xff)<<16 | uint32(ps[i+3]&0xff)<<8 | uint32(ps[i+4]&0xff)
				i += 4
			} else {
				continue
			}
			if p == 38 {
				v.st.fg = c
			} else {
				v.st.bg = c
			}
		}
	}
}

// end is the number of lines with content (or the cursor).
func (v *vterm) end() int {
	n := len(v.lines)
	for n > v.row+1 && len(v.lines[n-1]) == 0 {
		n--
	}
	return n
}

// render returns line i as text with SGR codes, cut to width columns.
func (v *vterm) render(i, width int) string {
	s, _ := v.renderW(i, width)
	return s
}

// renderW is render that also returns the width in columns.
func (v *vterm) renderW(i, width int) (string, int) {
	if i < 0 || i >= len(v.lines) {
		return "", 0
	}
	var sb strings.Builder
	cur := vstyle{}
	link := uint16(0)
	cols := 0
	for _, c := range v.lines[i] {
		if c.w == 0 {
			continue
		}
		if cols+int(c.w) > width {
			break
		}
		if c.st.link != link {
			if link != 0 {
				sb.WriteString("\x1b]8;;\x1b\\")
			}
			if c.st.link != 0 && int(c.st.link) <= len(v.links) {
				sb.WriteString("\x1b]8;;" + v.links[c.st.link-1] + "\x1b\\")
			}
			link = c.st.link
		}
		st := c.st
		st.link = 0
		if st != cur {
			sb.WriteString(sgrFor(st))
			cur = st
		}
		sb.WriteRune(c.r)
		cols += int(c.w)
	}
	if link != 0 {
		sb.WriteString("\x1b]8;;\x1b\\")
	}
	if cur != (vstyle{}) {
		sb.WriteString("\x1b[0m")
	}
	return sb.String(), cols
}

// plain returns line i without styling.
func (v *vterm) plain(i int) string {
	if i < 0 || i >= len(v.lines) {
		return ""
	}
	var sb strings.Builder
	for _, c := range v.lines[i] {
		if c.w != 0 {
			sb.WriteRune(c.r)
		}
	}
	return strings.TrimRight(sb.String(), " ")
}

func sgrFor(s vstyle) string {
	codes := []string{"0"}
	if s.attr&aBold != 0 {
		codes = append(codes, "1")
	}
	if s.attr&aDim != 0 {
		codes = append(codes, "2")
	}
	if s.attr&aItalic != 0 {
		codes = append(codes, "3")
	}
	if s.attr&aUnder != 0 {
		codes = append(codes, "4")
	}
	if s.attr&aReverse != 0 {
		codes = append(codes, "7")
	}
	color := func(c uint32, base int) {
		switch c >> 24 {
		case 1:
			n := int(c & 0xff)
			switch {
			case n < 8:
				codes = append(codes, strconv.Itoa(base+n))
			case n < 16:
				codes = append(codes, strconv.Itoa(base+60+n-8))
			default:
				codes = append(codes, strconv.Itoa(base+8)+";5;"+strconv.Itoa(n))
			}
		case 2:
			codes = append(codes, strconv.Itoa(base+8)+";2;"+strconv.Itoa(int(c>>16&0xff))+";"+strconv.Itoa(int(c>>8&0xff))+";"+strconv.Itoa(int(c&0xff)))
		}
	}
	color(s.fg, 30)
	color(s.bg, 40)
	return "\x1b[" + strings.Join(codes, ";") + "m"
}
