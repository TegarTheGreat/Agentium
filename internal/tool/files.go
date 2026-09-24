package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

var userHome = os.UserHomeDir

const (
	readMaxLines = 2000
	readMaxBytes = 60 * 1024
	readWholeMax = 8 << 20 // larger files are streamed, never loaded whole
)

var readTool = Tool{
	Def: providerDef("read",
		"Read a text file (lines are shown numbered: NUMBER<tab>text; the number is not part of the file) or an image, or show a directory as a tree. Call several in parallel for several files. outline=true returns only definitions with line numbers (a file's shape, or a code map of a whole directory) at a fraction of the tokens.",
		`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","description":"1-based start line"},"limit":{"type":"integer"},"outline":{"type":"boolean"}},"required":["path"]}`),
	Run: func(ctx context.Context, env *Env, raw json.RawMessage) (string, error) {
		var a struct {
			Path    string `json:"path"`
			Offset  int    `json:"offset"`
			Limit   int    `json:"limit"`
			Outline bool   `json:"outline"`
		}
		if err := decode(raw, &a); err != nil {
			return "", err
		}
		if a.Path == "" {
			return "", errors.New("path is required")
		}
		p := real(env.abs(a.Path))
		if env.Gate != nil {
			if ok, why := env.Gate.Read(p); !ok {
				return "", fmt.Errorf("denied (%s)", why)
			}
		}
		st, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		if a.Outline {
			if st.IsDir() {
				return env.outlineDir(ctx, p), nil
			}
			if !st.Mode().IsRegular() {
				return "", fmt.Errorf("%s is not a regular file", a.Path)
			}
			return outlineFile(p)
		}
		if st.IsDir() {
			return tree(ctx, p)
		}
		// Before anything opens it: opening a FIFO blocks, and a device
		// like /dev/zero never ends.
		if !st.Mode().IsRegular() {
			return "", fmt.Errorf("%s is not a regular file (device, pipe or socket); use bash if you really need it", a.Path)
		}
		if st.Size() <= MaxImageBytes*4 {
			if head := fileHead(p); len(head) > 0 {
				if out, ok, err := readImage(ctx, env, p, head); ok {
					return out, err
				}
			}
		}
		if st.Size() > readWholeMax {
			// Never load a multi-GB log to show 60 KB of it.
			return readLarge(p, st.Size(), a.Offset, a.Limit)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		env.markSeen(p)
		if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
			return fmt.Sprintf("(binary file, %d bytes)", len(b)), nil
		}
		// Sending the same unchanged text twice only adds noise (entropy)
		// and tokens; the earlier result is still in the conversation.
		if env.alreadyShown(fmt.Sprintf("%s|%d|%d", p, a.Offset, a.Limit), p) {
			return fmt.Sprintf("(unchanged since you read it earlier in this conversation: the same %s result is above; no need to read it again)", a.Path), nil
		}
		return sliceLines(string(b), a.Offset, a.Limit), nil
	},
}

func fileHead(p string) []byte {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	b := make([]byte, 512)
	n, _ := f.Read(b)
	return b[:n]
}

func listDir(p string) (string, error) {
	ents, err := os.ReadDir(p)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, e := range ents {
		if i >= 500 {
			fmt.Fprintf(&sb, "[... %d more entries]\n", len(ents)-i)
			break
		}
		sb.WriteString(e.Name())
		if e.IsDir() {
			sb.WriteByte('/')
		}
		sb.WriteByte('\n')
	}
	if sb.Len() == 0 {
		return "(empty directory)", nil
	}
	return sb.String(), nil
}

func sliceLines(s string, offset, limit int) string {
	if s == "" {
		return "(empty file)"
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	start := 0
	if offset > 1 {
		start = offset - 1
	}
	if start >= total {
		return fmt.Sprintf("(offset %d is past end of file: %d lines)", offset, total)
	}
	end := total
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	if end-start > readMaxLines {
		end = start + readMaxLines
	}
	// Lines are numbered like cat -n, so line numbers in errors and test
	// output can be matched without another command.
	numW := len(strconv.Itoa(end))
	var sb strings.Builder
	for i := start; i < end; i++ {
		prefix := fmt.Sprintf("%*d\t", numW, i+1)
		if sb.Len()+len(prefix)+len(lines[i]) > readMaxBytes {
			if sb.Len() == 0 {
				// A single huge line (minified code): show its start.
				sb.WriteString(prefix + strings.ToValidUTF8(lines[i][:readMaxBytes], "") + "…[line truncated]\n")
				end = i + 1
			} else {
				end = i
			}
			break
		}
		sb.WriteString(prefix + lines[i])
	}
	out := sb.String()
	if start > 0 || end < total {
		out += fmt.Sprintf("\n[lines %d-%d of %d; use offset to read more]", start+1, end, total)
	}
	return out
}

// readLarge streams the requested lines of a big file.
func readLarge(p string, size int64, offset, limit int) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 || limit > readMaxLines {
		limit = readMaxLines
	}
	r := bufio.NewReaderSize(f, 64*1024)
	if head, _ := r.Peek(8000); bytes.IndexByte(head, 0) >= 0 {
		return fmt.Sprintf("(binary file, %d MB)", size>>20), nil
	}
	var sb strings.Builder
	line, shown := 0, 0
	for shown < limit && sb.Len() < readMaxBytes {
		l, err := r.ReadString('\n')
		if len(l) > 0 {
			line++
			if line >= offset {
				if len(l) > 2000 {
					l = strings.ToValidUTF8(l[:2000], "") + "…[line truncated]\n"
				}
				sb.WriteString(l)
				shown++
			}
		}
		if err != nil {
			break
		}
	}
	if shown == 0 {
		return fmt.Sprintf("(offset %d is past the end of this %d MB file)", offset, size>>20), nil
	}
	return fmt.Sprintf("(%d MB file; lines %d-%d shown; use offset/limit, or bash grep/tail, for other parts)\n%s", size>>20, offset, offset+shown-1, sb.String()), nil
}
