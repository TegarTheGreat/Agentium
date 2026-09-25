package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var userHome = os.UserHomeDir

const (
	readMaxLines = 2000
	readMaxBytes = 60 * 1024
	readWholeMax = 8 << 20 // larger files are streamed, never loaded whole
)

var readTool = Tool{
	Def: providerDef("read",
		"Read a text file (lines are shown numbered: NUMBER<tab>text; the number is not part of the file), a PDF's text or an image, or show a directory as a tree. Call several in parallel for several files. outline=true returns only definitions with line numbers (a file's shape, or a code map of a whole directory) at a fraction of the tokens.",
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
		if head := fileHead(p); bytes.HasPrefix(head, []byte("%PDF-")) {
			env.markSeen(p)
			return readPDF(ctx, p, a.Offset, a.Limit)
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
		text, enc, ok := decodeText(b)
		if !ok {
			return fmt.Sprintf("(binary file, %d bytes)", len(b)), nil
		}
		// Sending the same unchanged text twice only adds noise (entropy)
		// and tokens; the earlier result is still in the conversation.
		if env.alreadyShown(fmt.Sprintf("%s|%d|%d", p, a.Offset, a.Limit), p) {
			return fmt.Sprintf("(unchanged since you read it earlier in this conversation: the same %s result is above; no need to read it again)", a.Path), nil
		}
		out := sliceLines(text, a.Offset, a.Limit)
		if enc != encUTF8 {
			out += fmt.Sprintf("\n(the file is %s, shown here as UTF-8; edit keeps its encoding)", enc)
		}
		return out, nil
	},
}

// readPDF returns a PDF's text, pages marked, through poppler's pdftotext.
func readPDF(ctx context.Context, p string, offset, limit int) (string, error) {
	text, note, err := pdfText(ctx, p)
	if err != nil || note != "" {
		return note, err
	}
	return sliceLines(text, offset, limit), nil
}

// pdfText is a PDF's text with "--- page N ---" markers; note explains
// instead when there is none (no pdftotext, a scanned document).
func pdfText(ctx context.Context, p string) (text, note string, err error) {
	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		return "", "(PDF file: its text needs pdftotext, which is not installed: poppler-utils on Linux, `brew install poppler` on macOS)", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-layout", "-enc", "UTF-8", "--", p, "-")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", err
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return "", "", err
	}
	b, _ := io.ReadAll(io.LimitReader(out, 8<<20))
	_ = cmd.Process.Kill() // a huge document: the first 8 MB of text is plenty
	_ = cmd.Wait()
	if len(bytes.TrimSpace(b)) == 0 {
		if msg := strings.TrimSpace(errb.String()); msg != "" && ctx.Err() == nil {
			return "", "", fmt.Errorf("pdftotext: %s", firstLineOf(msg))
		}
		return "", "(PDF with no extractable text: probably scanned images)", nil
	}
	pages := strings.Split(strings.TrimRight(string(b), "\f\n"), "\f")
	var sb strings.Builder
	for i, pg := range pages {
		fmt.Fprintf(&sb, "--- page %d ---\n%s\n", i+1, strings.TrimRight(pg, "\n "))
	}
	return sb.String(), "", nil
}

func firstLineOf(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
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
				cut := min(len(lines[i]), max(readMaxBytes-len(prefix)-32, 0))
				sb.WriteString(prefix + strings.ToValidUTF8(lines[i][:cut], "") + "…[line truncated]\n")
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
		// A line is read in pieces: a gigantic one (a minified bundle, a
		// log without newlines) is never held whole.
		var head []byte
		long, n := false, 0
		var err error
		for {
			var chunk []byte
			chunk, err = r.ReadSlice('\n')
			n += len(chunk)
			if line+1 >= offset && len(head) < 2000 {
				head = append(head, chunk[:min(len(chunk), 2000-len(head))]...)
			}
			if len(chunk) > 0 && (len(head) >= 2000 && chunk[len(chunk)-1] != '\n' || long) {
				long = true
			}
			if err != bufio.ErrBufferFull {
				break
			}
		}
		if n > 0 {
			line++
			if line >= offset {
				l := string(head)
				if long || n > 2000 {
					l = strings.ToValidUTF8(strings.TrimRight(l, "\n"), "") + "…[line truncated]\n"
				}
				fmt.Fprintf(&sb, "%d\t%s", line, l)
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
