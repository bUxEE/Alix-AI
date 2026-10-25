package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// writeAt is write with an explicit modification time, for a change the
// size-and-time shortcut of the provider must see.
func (f *fixture) writeAt(rel, content string, mt time.Time) {
	f.t.Helper()
	p := filepath.Join(f.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chtimes(p, mt, mt); err != nil {
		f.t.Fatal(err)
	}
}

// para returns a paragraph of about 350 characters that starts with tag and
// differs from every other tag: long enough to be a child chunk of its own.
func para(tag string) string {
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet", "kilo", "lima"}
	var b strings.Builder
	b.WriteString(tag)
	for i := 0; b.Len() < 350; i++ {
		b.WriteString(" ")
		b.WriteString(words[i%len(words)])
	}
	return b.String()
}

// doc joins paragraphs into a document.
func doc(title string, paras ...string) string {
	return "# " + title + "\n\n" + strings.Join(paras, "\n\n")
}

// corpus writes n documents of three long paragraphs each.
func corpus(f *fixture, n int) {
	for i := 0; i < n; i++ {
		f.write(fmt.Sprintf("doc%d.md", i), doc(fmt.Sprintf("Title %d", i),
			para(fmt.Sprintf("p%da", i)), para(fmt.Sprintf("p%db", i)), para(fmt.Sprintf("p%dc", i))))
	}
}

type counters struct{ parses, embedCalls, embedInputs int64 }

func (f *fixture) counters() counters {
	return counters{f.parser.calls.Load(), f.emb.calls.Load(), f.emb.inputs.Load()}
}

func (f *fixture) countRows(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRowContext(bg, query, args...).Scan(&n); err != nil {
		f.t.Fatalf("count %q: %v", query, err)
	}
	return n
}
