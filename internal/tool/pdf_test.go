package tool

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// tinyPDF builds a valid PDF with one line of text per page.
func tinyPDF(pages ...string) string {
	objs := []string{"<< /Type /Catalog /Pages 2 0 R >>"}
	var kids []string
	for i := range pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", 3+2*i))
	}
	objs = append(objs, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)))
	font := 3 + 2*len(pages)
	for i, t := range pages {
		st := fmt.Sprintf("BT /F1 18 Tf 20 100 Td (%s) Tj ET", t)
		objs = append(objs,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 200] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>", 4+2*i, font),
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(st), st))
	}
	objs = append(objs, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	out := "%PDF-1.4\n"
	var offs []int
	for i, o := range objs {
		offs = append(offs, len(out))
		out += fmt.Sprintf("%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	x := len(out)
	out += fmt.Sprintf("xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		out += fmt.Sprintf("%010d 00000 n \n", o)
	}
	return out + fmt.Sprintf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, x)
}

func TestReadPDF(t *testing.T) {
	e := env(t)
	write(t, e, "doc.pdf", tinyPDF("Invoice total 42", "Second page terms"))
	out, err := call(t, readTool, e, `{"path":"doc.pdf"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("pdftotext"); err != nil {
		if !strings.Contains(out, "needs pdftotext") {
			t.Fatalf("no pdftotext: %q", out)
		}
		return
	}
	for _, want := range []string{"--- page 1 ---", "Invoice total 42", "--- page 2 ---", "Second page terms"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
