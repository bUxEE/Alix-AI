package ragsec

import (
	"os"
	"path/filepath"
	"testing"
)

// fixture reads one committed fixture document from testdata/.
func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

// legitCorpus is the ordinary documents of the demo library: what a question
// should be answered from.
var legitCorpus = []string{"loans.md", "hours.md", "membership.md"}

// loadFixtures writes the named testdata fixtures into the source, each under its
// own name.
func (s *src) loadFixtures(names ...string) {
	s.w.t.Helper()
	for _, n := range names {
		s.write(n, fixture(s.w.t, n))
	}
}

// loadAs writes a testdata fixture under another name in the source.
func (s *src) loadAs(rel, fixtureName string) {
	s.w.t.Helper()
	s.write(rel, fixture(s.w.t, fixtureName))
}
