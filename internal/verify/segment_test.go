package verify

import (
	"strings"
	"testing"
)

func TestSplitSegmentsEnglish(t *testing.T) {
	ready, rest := SplitSegments("Hello there. More", LangEN)
	if len(ready) != 1 || ready[0] != "Hello there. " || rest != "More" {
		t.Fatalf("got ready=%q rest=%q", ready, rest)
	}
	ready, rest = SplitSegments("Dr. Smith arrived. Done.", LangEN)
	if len(ready) != 2 || !strings.HasPrefix(ready[0], "Dr. Smith") || rest != "" {
		t.Fatalf("abbr: ready=%q rest=%q", ready, rest)
	}
	ready, rest = SplitSegments("Price is 3.14 euros. Next", LangEN)
	if len(ready) != 1 || !strings.Contains(ready[0], "3.14") || rest != "Next" {
		t.Fatalf("decimal: ready=%q rest=%q", ready, rest)
	}
}

func TestSplitSegmentsItalian(t *testing.T) {
	ready, rest := SplitSegments("Il Dott. Rossi è arrivato. Fine", LangIT)
	if len(ready) != 1 || !strings.Contains(ready[0], "Dott.") || rest != "Fine" {
		t.Fatalf("it abbr: ready=%q rest=%q", ready, rest)
	}
}

func TestSplitSegmentsFenceAndTable(t *testing.T) {
	buf := "Intro.\n```go\nfmt.Println(1.2)\n```\nAfter."
	ready, rest := SplitSegments(buf, LangEN)
	joined := strings.Join(ready, "") + rest
	if joined != buf {
		t.Fatalf("round-trip fence: %q", joined)
	}
	if len(ready) < 1 || !strings.Contains(strings.Join(ready, ""), "```") {
		// Fence should come out as its own segment once closed.
		found := false
		for _, s := range ready {
			if strings.Contains(s, "```") {
				found = true
			}
		}
		if !found && !strings.Contains(rest, "```") {
			t.Fatalf("fence not kept: ready=%q rest=%q", ready, rest)
		}
	}

	table := "| A | B |\n| --- | --- |\n| 1 | 2 |\n\n"
	ready, rest = SplitSegments(table+"Tail", LangEN)
	if strings.TrimLeft(rest, "\n") != "Tail" || len(ready) != 1 || !strings.Contains(ready[0], "| A |") {
		t.Fatalf("table: ready=%q rest=%q", ready, rest)
	}
}

func TestSplitSegmentsListHeld(t *testing.T) {
	buf := "- one\n- two\n\nDone."
	ready, rest := SplitSegments(buf, LangEN)
	if len(ready) < 1 || !strings.Contains(ready[0], "- one") {
		t.Fatalf("list: ready=%q rest=%q", ready, rest)
	}
}

func TestFlushSegments(t *testing.T) {
	got := FlushSegments("No terminator yet", LangEN)
	if len(got) != 1 || got[0] != "No terminator yet" {
		t.Fatalf("flush=%q", got)
	}
}
