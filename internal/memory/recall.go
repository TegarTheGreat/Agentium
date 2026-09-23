package memory

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// Doc is one searchable unit of past context.
type Doc struct {
	Source string // e.g. "decision D-003", "journal 2026-09-21 14:03", "session 2026-09-20"
	Text   string
}

// Index is an in-memory BM25 index. Building it over a few MB of text
// takes milliseconds; no database or embedding model is needed.
type Index struct {
	docs  []Doc
	tf    []map[string]int
	df    map[string]int
	lens  []int
	avgdl float64
}

var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be by for from has have in is it its of on or that the this to was were will with
		i you we they he she me my our your do does did not no yes can could should would just so if then than but also into about
		yang dan di ke dari untuk ini itu dengan atau pada tidak ada juga akan bisa sudah saya kamu kita kami agar supaya jadi lagi`) {
		stop[w] = true
	}
}

// tokens lowercases and splits on non-alphanumerics, also splitting
// camelCase and snake_case so "parseConfig" matches "parse config".
func tokens(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 1 {
			w := strings.ToLower(string(cur))
			if !stop[w] {
				out = append(out, w)
			}
		}
		cur = cur[:0]
	}
	var prev rune
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if unicode.IsUpper(r) && unicode.IsLower(prev) {
				flush()
			}
			cur = append(cur, r)
		default:
			flush()
		}
		prev = r
	}
	flush()
	return out
}

// NewIndex builds an index over docs.
func NewIndex(docs []Doc) *Index {
	ix := &Index{docs: docs, df: map[string]int{}}
	total := 0
	for _, d := range docs {
		tf := map[string]int{}
		ts := tokens(d.Source + " " + d.Text)
		for _, t := range ts {
			tf[t]++
		}
		for t := range tf {
			ix.df[t]++
		}
		ix.tf = append(ix.tf, tf)
		ix.lens = append(ix.lens, len(ts))
		total += len(ts)
	}
	if len(docs) > 0 {
		ix.avgdl = float64(total) / float64(len(docs))
	}
	return ix
}

// Hit is a search result.
type Hit struct {
	Doc   Doc
	Score float64
	// Coverage is the share of distinct query terms the doc contains.
	Coverage float64
	Matched  int
}

// Search returns up to k docs ranked by BM25, with a recency tiebreak
// (later docs win). Docs matching only one common query term are dropped.
func (ix *Index) Search(query string, k int) []Hit {
	if ix == nil || len(ix.docs) == 0 {
		return nil
	}
	qs := map[string]bool{}
	for _, t := range tokens(query) {
		qs[t] = true
	}
	if len(qs) == 0 {
		return nil
	}
	const k1, b = 1.2, 0.75
	n := float64(len(ix.docs))
	var hits []Hit
	for i, tf := range ix.tf {
		score, matched := 0.0, 0
		for q := range qs {
			f := float64(tf[q])
			if f == 0 {
				continue
			}
			matched++
			df := float64(ix.df[q])
			idf := math.Log(1 + (n-df+0.5)/(df+0.5))
			score += idf * f * (k1 + 1) / (f + k1*(1-b+b*float64(ix.lens[i])/ix.avgdl))
		}
		if matched == 0 || (matched == 1 && len(qs) > 2 && score < 2) {
			continue
		}
		score += float64(i) / n * 0.05 // recency tiebreak
		hits = append(hits, Hit{Doc: ix.docs[i], Score: score, Matched: matched, Coverage: float64(matched) / float64(len(qs))})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// Docs gathers searchable context from the store: all decisions (with
// status) and journal entries. Callers add past-session docs.
func (s *Store) Docs() []Doc {
	var docs []Doc
	for _, d := range s.Decisions() {
		docs = append(docs, Doc{Source: "decision " + d.ID + " " + d.Date + " (" + d.Status + ")", Text: d.Text})
	}
	for _, e := range s.JournalEntries() {
		head, body, _ := strings.Cut(e, "\n")
		docs = append(docs, Doc{Source: "journal " + head, Text: body})
	}
	return docs
}

// Format renders hits as a compact <recall> block within maxChars.
func Format(hits []Hit, maxChars int) string {
	if len(hits) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<recall note=\"possibly relevant past context, found automatically\">\n")
	for _, h := range hits {
		line := "- [" + h.Doc.Source + "] " + strings.Join(strings.Fields(h.Doc.Text), " ")
		if len(line) > 400 {
			line = strings.ToValidUTF8(line[:400], "") + "…"
		}
		if sb.Len()+len(line) > maxChars {
			break
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString("</recall>")
	return sb.String()
}
