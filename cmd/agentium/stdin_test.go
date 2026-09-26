package main

import (
	"strings"
	"testing"
)

func TestAttachPiped(t *testing.T) {
	if got := attachPiped("why?", "  \n"); got != "why?" {
		t.Fatalf("blank stdin changed the prompt: %q", got)
	}
	got := attachPiped("why?", "ERROR: x\n")
	if !strings.HasPrefix(got, "why?\n\n<stdin>\nERROR: x\n</stdin>") {
		t.Fatalf("got %q", got)
	}
	big := strings.Repeat("a", maxPiped+10)
	got = attachPiped("p", big)
	if !strings.Contains(got, "all of it is in ") || len(got) > maxPiped+400 {
		t.Fatalf("big stdin: %d bytes, tail %q", len(got), got[len(got)-200:])
	}
}
