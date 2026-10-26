package textchunk

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitShort(t *testing.T) {
	got := Split("Hello.", 100)
	if len(got) != 1 || got[0] != "Hello." {
		t.Fatalf("got %#v", got)
	}
}

func TestSplitSentences(t *testing.T) {
	got := Split("One. Two. Three.", 8)
	if len(got) < 2 {
		t.Fatalf("got %#v", got)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "One") || !strings.Contains(joined, "Three") {
		t.Fatalf("got %#v", got)
	}
	for _, p := range got {
		if utf8.RuneCountInString(p) > 8 {
			t.Fatalf("piece too long: %q", p)
		}
	}
}

func TestSplitEmpty(t *testing.T) {
	if Split("  ", 10) != nil || Split("x", 0) != nil {
		t.Fatal("want nil")
	}
}

func TestSplitBytes(t *testing.T) {
	got := SplitBytes("Hello. World.", 8)
	if len(got) < 2 {
		t.Fatalf("got %#v", got)
	}
	for _, p := range got {
		if len(p) > 8 {
			t.Fatalf("oversized %q", p)
		}
	}
}
