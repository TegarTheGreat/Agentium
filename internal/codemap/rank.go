package codemap

import (
	"math"
	"path/filepath"
	"sort"
	"strings"
)

// Ranked is one file of a ranked repository map.
type Ranked struct {
	Rel  string
	Rank float64
	Syms []Symbol // most referenced definitions first
	File *FileEntry
}

// Rank orders the indexed files under prefix by how central they are,
// the way Aider's repo map does: files are nodes, and a file that uses an
// identifier defined in another file links to it. PageRank over that
// graph, personalized towards focus files (e.g. those touched this
// session), puts the code everything depends on — and the code near the
// current work — first. Each file's definitions are ordered by how many
// other files use them.
func (ix *Index) Rank(prefix string, focus map[string]bool) []Ranked {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	var rels []string
	for rel := range ix.Files {
		if prefix == "" || rel == prefix || strings.HasPrefix(rel, prefix+string(filepath.Separator)) {
			rels = append(rels, rel)
		}
	}
	sort.Strings(rels)
	n := len(rels)
	if n == 0 {
		return nil
	}
	// Who defines each identifier.
	definers := map[string][]int{}
	for i, r := range rels {
		seen := map[string]bool{}
		for _, s := range ix.Files[r].Syms {
			if len(s.Name) >= 3 && !seen[s.Name] {
				seen[s.Name] = true
				definers[s.Name] = append(definers[s.Name], i)
			}
		}
	}
	type edge struct {
		to int
		w  float64
	}
	out := make([][]edge, n)
	users := map[string]int{} // identifier -> number of files using it
	for i, r := range rels {
		agg := map[int]float64{}
		for _, ident := range ix.Files[r].Idents {
			ds := definers[ident]
			if len(ds) == 0 {
				continue
			}
			users[ident]++
			w := identWeight(ident, len(ds))
			for _, d := range ds {
				if d != i {
					agg[d] += w
				}
			}
		}
		for d, w := range agg {
			out[i] = append(out[i], edge{d, w})
		}
		sort.Slice(out[i], func(a, b int) bool { return out[i][a].to < out[i][b].to })
	}
	// Personalization: uniform, strongly tilted towards focus files.
	pers := make([]float64, n)
	total := 0.0
	for i, r := range rels {
		pers[i] = 1
		if focus[r] {
			pers[i] = 100
		}
		total += pers[i]
	}
	for i := range pers {
		pers[i] /= total
	}
	rank := append([]float64(nil), pers...)
	const damping = 0.85
	for iter := 0; iter < 30; iter++ {
		next := make([]float64, n)
		dangling := 0.0
		for i := range out {
			sum := 0.0
			for _, e := range out[i] {
				sum += e.w
			}
			if sum == 0 {
				dangling += rank[i]
				continue
			}
			for _, e := range out[i] {
				next[e.to] += damping * rank[i] * e.w / sum
			}
		}
		diff := 0.0
		for i := range next {
			next[i] += (1-damping)*pers[i] + damping*dangling*pers[i]
			diff += math.Abs(next[i] - rank[i])
		}
		rank = next
		if diff < 1e-9 {
			break
		}
	}
	res := make([]Ranked, n)
	for i, r := range rels {
		f := ix.Files[r]
		syms := append([]Symbol(nil), f.Syms...)
		sort.SliceStable(syms, func(a, b int) bool {
			ua, ub := users[syms[a].Name], users[syms[b].Name]
			if ua != ub {
				return ua > ub
			}
			return syms[a].Line < syms[b].Line
		})
		res[i] = Ranked{Rel: r, Rank: rank[i], Syms: syms, File: f}
	}
	sort.SliceStable(res, func(a, b int) bool { return res[a].Rank > res[b].Rank })
	return res
}

// identWeight follows Aider's heuristics: long, distinctive identifiers
// say more about a dependency than short or ubiquitous ones.
func identWeight(ident string, nDefiners int) float64 {
	w := 1.0
	if len(ident) >= 8 && (strings.ContainsAny(ident, "_-") || hasInnerUpper(ident)) {
		w *= 10
	}
	if strings.HasPrefix(ident, "_") {
		w *= 0.1
	}
	if nDefiners > 5 {
		w *= 0.1
	}
	return w
}

func hasInnerUpper(s string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' && s[i-1] >= 'a' && s[i-1] <= 'z' {
			return true
		}
	}
	return false
}
