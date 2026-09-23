package codemap

import (
	"bytes"
	"encoding/gob"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
)

// Index caches outlines and identifier sets per file, keyed by size and
// modification time, so a large repository is parsed once and later
// sessions only re-read files that changed.
type Index struct {
	Files map[string]*FileEntry // path relative to Root
	Root  string

	mu sync.RWMutex
}

// FileEntry is one indexed file.
type FileEntry struct {
	Size   int64
	Mod    int64
	Lines  int
	Syms   []Symbol
	Idents []string // distinct identifiers used in the file (sorted)
}

const (
	indexVersion   = 1
	maxIndexFile   = 1 << 20 // larger files are usually generated
	maxIdents      = 4000
	maxIndexedFile = 50000
)

type diskIndex struct {
	Version int
	Root    string
	Files   map[string]*FileEntry
}

// LoadIndex reads a cached index, or returns an empty one.
func LoadIndex(cachePath, root string) *Index {
	ix := &Index{Files: map[string]*FileEntry{}, Root: root}
	f, err := os.Open(cachePath)
	if err != nil {
		return ix
	}
	defer f.Close()
	var d diskIndex
	if gob.NewDecoder(f).Decode(&d) == nil && d.Version == indexVersion && d.Root == root && d.Files != nil {
		ix.Files = d.Files
	}
	return ix
}

// Save writes the index atomically.
func (ix *Index) Save(cachePath string) error {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		return err
	}
	tmp := cachePath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(diskIndex{Version: indexVersion, Root: ix.Root, Files: ix.Files}); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, cachePath)
}

// Update brings the index in line with files (absolute paths under Root):
// new or changed files are parsed in parallel, vanished ones dropped.
// It reports whether anything changed.
func (ix *Index) Update(files []string) bool {
	if len(files) > maxIndexedFile {
		files = files[:maxIndexedFile]
	}
	type job struct {
		rel string
		abs string
		st  os.FileInfo
	}
	var jobs []job
	want := make(map[string]bool, len(files))
	ix.mu.RLock()
	for _, abs := range files {
		rel, err := filepath.Rel(ix.Root, abs)
		if err != nil {
			continue
		}
		want[rel] = true
		st, err := os.Stat(abs)
		if err != nil || !st.Mode().IsRegular() || st.Size() > maxIndexFile {
			continue
		}
		if e := ix.Files[rel]; e != nil && e.Size == st.Size() && e.Mod == st.ModTime().UnixNano() {
			continue
		}
		jobs = append(jobs, job{rel, abs, st})
	}
	stale := 0
	for rel := range ix.Files {
		if !want[rel] {
			stale++
		}
	}
	ix.mu.RUnlock()
	if len(jobs) == 0 && stale == 0 {
		return false
	}

	results := make([]*FileEntry, len(jobs))
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				j := jobs[i]
				b, err := os.ReadFile(j.abs)
				if err != nil || bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
					continue
				}
				results[i] = &FileEntry{Size: j.st.Size(), Mod: j.st.ModTime().UnixNano(),
					Lines: bytes.Count(b, []byte("\n")) + 1, Syms: Outline(j.abs, b), Idents: Identifiers(b)}
			}
		}()
	}
	for i := range jobs {
		next <- i
	}
	close(next)
	wg.Wait()

	ix.mu.Lock()
	defer ix.mu.Unlock()
	for rel := range ix.Files {
		if !want[rel] {
			delete(ix.Files, rel)
		}
	}
	for i, j := range jobs {
		if results[i] != nil {
			ix.Files[j.rel] = results[i]
		} else {
			delete(ix.Files, j.rel)
		}
	}
	return true
}

// Each calls fn for every indexed file in path order.
func (ix *Index) Each(fn func(rel string, e *FileEntry)) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	rels := make([]string, 0, len(ix.Files))
	for r := range ix.Files {
		rels = append(rels, r)
	}
	sort.Strings(rels)
	for _, r := range rels {
		fn(r, ix.Files[r])
	}
}

// Uses reports whether the file uses identifier name.
func (e *FileEntry) Uses(name string) bool {
	i := sort.SearchStrings(e.Idents, name)
	return i < len(e.Idents) && e.Idents[i] == name
}

// Enclosing returns the innermost function-like symbol at or before line,
// used to say where a reference sits ("in Agent.Run").
func (e *FileEntry) Enclosing(line int) *Symbol {
	var best *Symbol
	for i := range e.Syms {
		s := &e.Syms[i]
		if s.Line > line {
			break
		}
		switch s.Kind {
		case "func", "method", "class", "impl", "type":
			best = s
		}
	}
	return best
}

// Identifiers returns the distinct identifiers (length >= 3, not
// starting with a digit) in src, sorted and capped.
func Identifiers(src []byte) []string {
	seen := map[string]bool{}
	start := -1
	flush := func(end int) {
		if start >= 0 && end-start >= 3 && end-start <= 64 && len(seen) < maxIdents {
			seen[string(src[start:end])] = true
		}
		start = -1
	}
	for i := 0; i < len(src); i++ {
		c := src[i]
		word := c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
		switch {
		case word && start < 0:
			if c >= '0' && c <= '9' {
				// skip numbers
				for i < len(src) && (src[i] == '_' || src[i] >= '0' && src[i] <= '9' || src[i] >= 'a' && src[i] <= 'z' || src[i] >= 'A' && src[i] <= 'Z') {
					i++
				}
				i--
				continue
			}
			start = i
		case !word && start >= 0:
			flush(i)
		}
	}
	flush(len(src))
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
