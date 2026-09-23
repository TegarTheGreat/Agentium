// Package codemap extracts an outline (definitions with line numbers) from
// source files: exact for Go via go/parser, line-based patterns for other
// languages. An outline lets the model see a file's shape for a fraction
// of the tokens a full read costs, and find definitions without grepping.
package codemap

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
)

// Symbol is one definition.
type Symbol struct {
	Name  string // bare name; methods also match "Type.Name"
	Recv  string // receiver or enclosing type, when known
	Kind  string // func, method, type, class, const, var, ...
	Line  int
	Depth int    // nesting (indentation) level for display
	Sig   string // one-line signature
}

// Supported reports whether path has a language codemap understands.
func Supported(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".go" || langs[ext] != nil
}

// Outline returns the definitions in src, in file order.
func Outline(path string, src []byte) []Symbol {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".go" {
		if syms, err := goOutline(src); err == nil {
			return syms
		}
		return nil
	}
	if l := langs[ext]; l != nil {
		return regexOutline(l, src)
	}
	return nil
}

// Format renders symbols one per line as "line: signature".
func Format(syms []Symbol) string {
	var sb strings.Builder
	for _, s := range syms {
		fmt.Fprintf(&sb, "%d: %s%s\n", s.Line, strings.Repeat("  ", min(s.Depth, 4)), s.Sig)
	}
	return sb.String()
}

// Match reports whether s is the definition query names: "Name" or
// "Type.Name".
func (s Symbol) Match(query string) bool {
	if s.Name == query {
		return true
	}
	if t, n, ok := strings.Cut(query, "."); ok {
		return n == s.Name && strings.TrimLeft(s.Recv, "*") == t
	}
	return false
}

const maxSig = 160

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxSig {
		s = s[:maxSig] + "…"
	}
	return s
}

func goOutline(src []byte) ([]Symbol, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil && f == nil {
		return nil, err
	}
	text := func(from, to token.Pos) string {
		a, b := fset.Position(from).Offset, fset.Position(to).Offset
		if a < 0 || b > len(src) || a >= b {
			return ""
		}
		return string(src[a:b])
	}
	var out []Symbol
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			end := d.End()
			if d.Body != nil {
				end = d.Body.Lbrace
			}
			s := Symbol{Name: d.Name.Name, Kind: "func", Line: fset.Position(d.Pos()).Line, Sig: clip(text(d.Pos(), end))}
			if d.Recv != nil && len(d.Recv.List) > 0 {
				s.Kind = "method"
				s.Recv = recvName(d.Recv.List[0].Type)
			}
			out = append(out, s)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					kind := "type"
					sig := "type " + sp.Name.Name
					switch t := sp.Type.(type) {
					case *ast.StructType:
						sig += " struct"
					case *ast.InterfaceType:
						sig += " interface"
						out = append(out, Symbol{Name: sp.Name.Name, Kind: kind, Line: fset.Position(sp.Pos()).Line, Sig: sig})
						for _, m := range t.Methods.List {
							for _, n := range m.Names {
								out = append(out, Symbol{Name: n.Name, Recv: sp.Name.Name, Kind: "method", Depth: 1,
									Line: fset.Position(m.Pos()).Line, Sig: clip(text(m.Pos(), m.End()))})
							}
						}
						continue
					default:
						sig = clip("type " + text(sp.Pos(), sp.End()))
					}
					out = append(out, Symbol{Name: sp.Name.Name, Kind: kind, Line: fset.Position(sp.Pos()).Line, Sig: sig})
				case *ast.ValueSpec:
					kind := d.Tok.String()
					for _, n := range sp.Names {
						if n.Name == "_" {
							continue
						}
						out = append(out, Symbol{Name: n.Name, Kind: kind, Line: fset.Position(n.Pos()).Line, Sig: clip(kind + " " + text(sp.Pos(), sp.End()))})
					}
				}
			}
		}
	}
	return collapseValues(out), nil
}

// collapseValues shows long const/var blocks as one line per run of
// consecutive specs, keeping the symbols searchable.
func collapseValues(in []Symbol) []Symbol {
	var out []Symbol
	for i := 0; i < len(in); {
		j := i
		for j < len(in) && (in[j].Kind == "const" || in[j].Kind == "var") && in[j].Kind == in[i].Kind {
			j++
		}
		if j-i <= 3 {
			if j == i {
				j++
			}
			out = append(out, in[i:j]...)
			i = j
			continue
		}
		names := make([]string, 0, j-i)
		for _, s := range in[i:j] {
			names = append(names, s.Name)
		}
		out = append(out, Symbol{Name: in[i].Name, Kind: in[i].Kind, Line: in[i].Line, Sig: clip(in[i].Kind + " " + strings.Join(names, ", "))})
		i = j
	}
	return out
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return "*" + recvName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	}
	return ""
}

type pattern struct {
	re   *regexp.Regexp // must have a group named "name"
	kind string
}

type lang struct {
	pats []pattern
	// indent-scoped languages nest by indentation; brace languages by
	// the depth of open braces.
	braces bool
}

func p(kind, re string) pattern { return pattern{regexp.MustCompile(re), kind} }

var (
	cLike = []pattern{
		p("type", `^\s*(typedef\s+)?(struct|class|enum|union)\s+(?P<name>\w+)\s*[:{]?[^;]*$`),
		p("func", `^(?:[A-Za-z_][\w\s\*&:<>,~]*?[\s\*&])?(?P<name>[A-Za-z_~][\w:~]*)\s*\([^;]*\)\s*(const)?\s*(noexcept)?\s*(override)?\s*\{?\s*$`),
	}
	jsLike = []pattern{
		p("func", `^\s*(export\s+)?(default\s+)?(async\s+)?function\s*\*?\s*(?P<name>[\w$]+)`),
		p("class", `^\s*(export\s+)?(default\s+)?(abstract\s+)?class\s+(?P<name>[\w$]+)`),
		p("type", `^\s*(export\s+)?(declare\s+)?(interface|type|enum)\s+(?P<name>[\w$]+)`),
		p("func", `^\s*(export\s+)?(const|let|var)\s+(?P<name>[\w$]+)\s*(:[^=]+)?=\s*(async\s+)?(function\b|\([^)]*\)\s*(:[^=]+)?=>|[\w$]+\s*=>)`),
		p("method", `^\s+(?:(?:public|private|protected|static|readonly|async|override|get|set)\s+)*(?P<name>[\w$]+)\s*(<[^>]*>)?\([^)]*\)\s*(:\s*[^{]+)?\{\s*$`),
	}
	langs = map[string]*lang{
		".py": {pats: []pattern{
			p("func", `^\s*(async\s+)?def\s+(?P<name>\w+)`),
			p("class", `^\s*class\s+(?P<name>\w+)`),
		}},
		".rb": {pats: []pattern{
			p("func", `^\s*def\s+(self\.)?(?P<name>\w+[?!=]?)`),
			p("class", `^\s*(class|module)\s+(?P<name>[\w:]+)`),
		}},
		".rs": {braces: true, pats: []pattern{
			p("func", `^\s*(pub(\([^)]*\))?\s+)?(default\s+)?(const\s+)?(async\s+)?(unsafe\s+)?(extern\s+"[^"]*"\s+)?fn\s+(?P<name>\w+)`),
			p("type", `^\s*(pub(\([^)]*\))?\s+)?(struct|enum|trait|type|union|mod)\s+(?P<name>\w+)`),
			p("impl", `^\s*impl\b(<[^>]*>)?\s*(?:[\w:<>, ]+\s+for\s+)?(?P<name>\w+)`),
			p("macro", `^\s*macro_rules!\s*(?P<name>\w+)`),
		}},
		".java":  {braces: true, pats: jvm()},
		".kt":    {braces: true, pats: append(jvm(), p("func", `^\s*(\w+\s+)*fun\s+(<[^>]*>\s*)?([\w.]+\.)?(?P<name>\w+)`))},
		".cs":    {braces: true, pats: jvm()},
		".scala": {braces: true, pats: append(jvm(), p("func", `^\s*(\w+\s+)*def\s+(?P<name>\w+)`))},
		".swift": {braces: true, pats: append(jvm(), p("func", `^\s*(\w+\s+)*func\s+(?P<name>\w+)`))},
		".php": {braces: true, pats: []pattern{
			p("class", `^\s*(abstract\s+|final\s+)?(class|interface|trait|enum)\s+(?P<name>\w+)`),
			p("func", `^\s*(\w+\s+)*function\s+&?(?P<name>\w+)`),
		}},
		".c": {braces: true, pats: cLike}, ".h": {braces: true, pats: cLike}, ".cc": {braces: true, pats: cLike},
		".cpp": {braces: true, pats: cLike}, ".hpp": {braces: true, pats: cLike}, ".cxx": {braces: true, pats: cLike},
		".js": {braces: true, pats: jsLike}, ".jsx": {braces: true, pats: jsLike}, ".mjs": {braces: true, pats: jsLike},
		".cjs": {braces: true, pats: jsLike}, ".ts": {braces: true, pats: jsLike}, ".tsx": {braces: true, pats: jsLike},
		".lua": {pats: []pattern{p("func", `^\s*(local\s+)?function\s+(?P<name>[\w.:]+)`)}},
		".sh":  {pats: []pattern{p("func", `^\s*(function\s+)?(?P<name>[\w-]+)\s*\(\)\s*\{?`)}},
	}
)

func jvm() []pattern {
	return []pattern{
		p("class", `^\s*(?:(?:public|private|protected|internal|static|final|abstract|sealed|open|data|partial|export|override|inline|value|enum|annotation)\s+)*(class|interface|enum|struct|record|object|protocol|trait|extension)\s+(?P<name>\w+)`),
		p("method", `^\s*(?:(?:public|private|protected|internal|static|final|override|abstract|async|virtual|synchronized|native|default)\s+)+[\w<>\[\],.?\s]*?\s(?P<name>\w+)\s*\([^;]*$`),
	}
}

var notNames = map[string]bool{"if": true, "for": true, "while": true, "switch": true, "catch": true, "return": true,
	"else": true, "do": true, "try": true, "with": true, "new": true, "sizeof": true, "function": true}

func regexOutline(l *lang, src []byte) []Symbol {
	var out []Symbol
	depth := 0
	var cols []int // indentation columns of enclosing definitions
	inBlockComment := false
	lines := bytes.Split(src, []byte("\n"))
	for i, raw := range lines {
		line := string(raw)
		trim := strings.TrimSpace(line)
		if inBlockComment {
			if strings.Contains(trim, "*/") {
				inBlockComment = false
			}
			continue
		}
		if strings.HasPrefix(trim, "/*") && !strings.Contains(trim, "*/") {
			inBlockComment = true
			continue
		}
		if trim == "" || strings.HasPrefix(trim, "//") || strings.HasPrefix(trim, "#") || strings.HasPrefix(trim, "*") {
			continue
		}
		level := depth
		col := indentCols(line)
		if !l.braces {
			for len(cols) > 0 && cols[len(cols)-1] >= col {
				cols = cols[:len(cols)-1]
			}
			level = len(cols)
		}
		for _, pt := range l.pats {
			m := pt.re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			name := m[pt.re.SubexpIndex("name")]
			if name == "" || notNames[name] {
				continue
			}
			out = append(out, Symbol{Name: name, Kind: pt.kind, Line: i + 1, Depth: level, Sig: clip(strings.TrimRight(trim, "{ "))})
			if !l.braces {
				cols = append(cols, col)
			}
			break
		}
		if l.braces {
			depth += braceDelta(line)
			if depth < 0 {
				depth = 0
			}
		}
	}
	// Record the enclosing type for methods so "Type.method" matches.
	var stack []Symbol
	for i := range out {
		for len(stack) > 0 && stack[len(stack)-1].Depth >= out[i].Depth {
			stack = stack[:len(stack)-1]
		}
		if len(stack) > 0 {
			out[i].Recv = stack[len(stack)-1].Name
		}
		switch out[i].Kind {
		case "class", "type", "impl":
			stack = append(stack, out[i])
		}
	}
	return out
}

func indentCols(line string) int {
	n := 0
	for _, r := range line {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}

// braceDelta counts braces outside strings and line comments, roughly.
func braceDelta(line string) int {
	d := 0
	var quote rune
	prev := rune(0)
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote && prev != '\\' {
				quote = 0
			}
		case r == '"' || r == '\'' || r == '`':
			quote = r
		case r == '/' && prev == '/':
			return d
		case r == '{':
			d++
		case r == '}':
			d--
		}
		prev = r
	}
	return d
}
