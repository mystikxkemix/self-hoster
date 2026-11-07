package supervisor

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// stripTime removes the "15:04:05 " prefix added to every line.
func stripTime(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l[9:]
	}
	return out
}

func TestLogBufferSplitsLinesAcrossWrites(t *testing.T) {
	var mirror bytes.Buffer
	b := newLogBuffer("app", &mirror)
	fmt.Fprint(b, "first\nsec")
	fmt.Fprint(b, "ond\r\nthird (no newline yet)")

	got := stripTime(b.Lines())
	if strings.Join(got, "|") != "first|second" {
		t.Fatalf("Lines() = %q, want [first second]", got)
	}
	if mirror.String() != "[app] first\n[app] second\n" {
		t.Fatalf("mirror = %q", mirror.String())
	}
}

func TestLogBufferKeepsLastLinesInOrder(t *testing.T) {
	b := newLogBuffer("app", nil)
	total := logCapacity + 5
	for i := range total {
		fmt.Fprintf(b, "line %d\n", i)
	}
	got := stripTime(b.Lines())
	if len(got) != logCapacity {
		t.Fatalf("len = %d, want %d", len(got), logCapacity)
	}
	if got[0] != "line 5" || got[len(got)-1] != fmt.Sprintf("line %d", total-1) {
		t.Fatalf("first/last = %q/%q", got[0], got[len(got)-1])
	}
}

func TestLogBufferFlushesOverlongLine(t *testing.T) {
	b := newLogBuffer("app", nil)
	b.Write(bytes.Repeat([]byte("x"), maxLineLen+1))
	if got := b.Lines(); len(got) != 1 {
		t.Fatalf("got %d lines, want the overlong partial line flushed", len(got))
	}
}

func TestLogBufferSystemMessage(t *testing.T) {
	b := newLogBuffer("app", nil)
	b.system("deploy %s", "started")
	if got := stripTime(b.Lines()); got[0] != "[self-hoster] deploy started" {
		t.Fatalf("got %q", got)
	}
}
