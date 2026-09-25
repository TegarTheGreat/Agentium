package tool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Jupyter notebooks (.ipynb) are JSON; the model reads and edits their
// cells as text instead.

func isNotebook(p string) bool { return strings.HasSuffix(strings.ToLower(p), ".ipynb") }

type notebook struct {
	doc   map[string]any
	cells []map[string]any
	lang  string
}

func parseNotebook(b []byte) (*notebook, error) {
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("not a valid notebook: %v", err)
	}
	raw, _ := doc["cells"].([]any)
	nb := &notebook{doc: doc, lang: "python"}
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			nb.cells = append(nb.cells, m)
		}
	}
	if md, ok := doc["metadata"].(map[string]any); ok {
		if li, ok := md["language_info"].(map[string]any); ok {
			if n, ok := li["name"].(string); ok && n != "" {
				nb.lang = n
			}
		}
	}
	return nb, nil
}

// joinSource reads a cell's source (a string or a list of lines).
func joinSource(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []any:
		var b strings.Builder
		for _, l := range s {
			if t, ok := l.(string); ok {
				b.WriteString(t)
			}
		}
		return b.String()
	}
	return ""
}

// splitSource writes source back as Jupyter does: lines with their "\n".
func splitSource(s string) []any {
	out := []any{}
	for _, l := range strings.SplitAfter(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// render shows the notebook as text: each cell with its number and type,
// code cells with (trimmed) text outputs.
func (nb *notebook) render() string {
	var b strings.Builder
	for i, c := range nb.cells {
		kind, _ := c["cell_type"].(string)
		head := fmt.Sprintf("── cell %d · %s", i, kind)
		if kind == "code" {
			head += " · " + nb.lang
			if n, ok := c["execution_count"].(float64); ok {
				head += fmt.Sprintf(" · [%d]", int(n))
			}
		}
		b.WriteString(head + "\n")
		src := joinSource(c["source"])
		b.WriteString(src)
		if !strings.HasSuffix(src, "\n") {
			b.WriteString("\n")
		}
		if kind == "code" {
			if out := cellOutputs(c); out != "" {
				b.WriteString("── output\n" + out)
			}
		}
	}
	if len(nb.cells) == 0 {
		b.WriteString("(empty notebook)\n")
	}
	return b.String()
}

// cellOutputs is a code cell's text output, clipped; images are noted.
func cellOutputs(c map[string]any) string {
	outs, _ := c["outputs"].([]any)
	var b strings.Builder
	for _, o := range outs {
		m, ok := o.(map[string]any)
		if !ok {
			continue
		}
		switch m["output_type"] {
		case "stream":
			b.WriteString(joinSource(m["text"]))
		case "error":
			en, _ := m["ename"].(string)
			ev, _ := m["evalue"].(string)
			b.WriteString(en + ": " + ev + "\n")
		case "execute_result", "display_data":
			data, _ := m["data"].(map[string]any)
			if t := joinSource(data["text/plain"]); t != "" {
				b.WriteString(t + "\n")
			}
			for k := range data {
				if strings.HasPrefix(k, "image/") {
					b.WriteString("[" + k + " image]\n")
				}
			}
		}
	}
	s := b.String()
	if len(s) > 2000 {
		s = s[:2000] + "\n[… output cut]\n"
	}
	return s
}

// encode writes the notebook back the way Jupyter does (sorted keys,
// one-space indent, trailing newline), so diffs stay small.
func (nb *notebook) encode() ([]byte, error) {
	cells := make([]any, len(nb.cells))
	for i, c := range nb.cells {
		cells[i] = c
	}
	nb.doc["cells"] = cells
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(nb.doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// editNotebook applies an edit to a notebook's cells: old/new inside the
// one cell holding old, or a whole cell by number (replace, insert before
// it, or delete). An edited code cell's outputs are cleared, since they no
// longer match its code.
func editNotebook(before []byte, old, new string, all bool, cell *int, mode, cellType string) ([]byte, string, error) {
	nb, err := parseNotebook(before)
	if err != nil {
		return nil, "", err
	}
	clear := func(c map[string]any) {
		if c["cell_type"] == "code" {
			c["outputs"] = []any{}
			c["execution_count"] = nil
		}
	}
	if cell == nil {
		if old == "" {
			return nil, "", errors.New("in a notebook, give old (text inside a cell) or cell (its number from read)")
		}
		found := -1
		for i, c := range nb.cells {
			if strings.Contains(joinSource(c["source"]), old) {
				if found >= 0 && !all {
					return nil, "", fmt.Errorf("old appears in cells %d and %d; give cell, or more context", found, i)
				}
				found = i
				src := joinSource(c["source"])
				if all {
					src = strings.ReplaceAll(src, old, new)
				} else {
					src = strings.Replace(src, old, new, 1)
				}
				c["source"] = splitSource(src)
				clear(c)
			}
		}
		if found < 0 {
			return nil, "", errors.New("old is not in any cell (compare with read's output)")
		}
		out, err := nb.encode()
		return out, fmt.Sprintf("cell %d", found), err
	}
	i := *cell
	switch mode {
	case "", "replace":
		if i < 0 || i >= len(nb.cells) {
			return nil, "", fmt.Errorf("no cell %d (the notebook has %d)", i, len(nb.cells))
		}
		c := nb.cells[i]
		src := joinSource(c["source"])
		switch {
		case old == "":
			src = new
		case !strings.Contains(src, old):
			return nil, "", fmt.Errorf("old is not in cell %d", i)
		case all:
			src = strings.ReplaceAll(src, old, new)
		default:
			src = strings.Replace(src, old, new, 1)
		}
		c["source"] = splitSource(src)
		if cellType != "" && cellType != c["cell_type"] {
			c["cell_type"] = cellType
			if cellType == "markdown" {
				delete(c, "outputs")
				delete(c, "execution_count")
			}
		}
		clear(c)
		out, err := nb.encode()
		return out, fmt.Sprintf("cell %d replaced", i), err
	case "insert":
		if i < 0 || i > len(nb.cells) {
			return nil, "", fmt.Errorf("cannot insert at %d (the notebook has %d cells)", i, len(nb.cells))
		}
		if cellType == "" {
			cellType = "code"
		}
		c := map[string]any{"cell_type": cellType, "metadata": map[string]any{}, "source": splitSource(new)}
		if cellType == "code" {
			c["outputs"], c["execution_count"] = []any{}, nil
		}
		nb.cells = append(nb.cells[:i], append([]map[string]any{c}, nb.cells[i:]...)...)
		out, err := nb.encode()
		return out, fmt.Sprintf("cell %d inserted", i), err
	case "delete":
		if i < 0 || i >= len(nb.cells) {
			return nil, "", fmt.Errorf("no cell %d (the notebook has %d)", i, len(nb.cells))
		}
		nb.cells = append(nb.cells[:i], nb.cells[i+1:]...)
		out, err := nb.encode()
		return out, fmt.Sprintf("cell %d deleted", i), err
	}
	return nil, "", fmt.Errorf("cell_mode must be replace, insert or delete")
}
