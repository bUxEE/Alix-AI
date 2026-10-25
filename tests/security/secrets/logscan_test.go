package secretssec

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// The log scanner of SEC-082: given canaries, it looks for each of them (and, for the long
// ones, for each of their pieces) in what the logs hold. It reads in-memory sinks, files and
// whole directories, and it fails the test that uses it when it was given nothing to read, so a
// scan can never pass by looking at nothing.

// canaries are the secrets of one run, by the family they stand for. Every value is random.
type canaries map[string]string

// newCanary returns a random value that is unmistakable in a log line: a family prefix, the
// run-unique random part, a fixed tail. It is built at run time, never a literal.
func newCanary(t testing.TB) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "cnry" + hex.EncodeToString(b) + "z9"
}

// leak is one sighting of a canary.
type leak struct {
	Family string
	Where  string // the sink, file or directory entry
	Line   int
	Piece  bool // only a piece of the value was seen
}

func (l leak) String() string {
	kind := "the value"
	if l.Piece {
		kind = "a piece of the value"
	}
	return fmt.Sprintf("%s (%s) in %s line %d", l.Family, kind, l.Where, l.Line)
}

// source is something the scanner reads: a name for reports and its bytes.
type source struct {
	name string
	data []byte
}

// minPiece is the shortest piece of a canary that counts as the canary: a fragment shorter than
// this is a coincidence of the alphabet, not a leak.
const minPiece = 16

// pieces are the parts of a value the scanner also looks for: every dot-separated segment of a
// token (a JWT leaks piece by piece) of at least minPiece characters.
func pieces(v string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(v, func(r rune) bool { return r == '.' }) {
		if len(p) >= minPiece && p != v {
			out = append(out, p)
		}
	}
	return out
}

// scanSources looks for every canary in every source and returns the sightings, in order of
// source and line. It reports an error when it read nothing at all.
func scanSources(c canaries, srcs ...source) ([]leak, error) {
	var total int
	for _, s := range srcs {
		total += len(s.data)
	}
	if len(srcs) == 0 || total == 0 {
		return nil, fmt.Errorf("the log scan read nothing (%d source(s), %d bytes): a scan of nothing proves nothing", len(srcs), total)
	}
	families := make([]string, 0, len(c))
	for f := range c {
		families = append(families, f)
	}
	sort.Strings(families)

	var found []leak
	for _, s := range srcs {
		for i, line := range bytes.Split(s.data, []byte("\n")) {
			text := string(line)
			for _, f := range families {
				v := c[f]
				if v == "" {
					continue
				}
				if strings.Contains(text, v) {
					found = append(found, leak{Family: f, Where: s.name, Line: i + 1})
					continue
				}
				for _, p := range pieces(v) {
					if strings.Contains(text, p) {
						found = append(found, leak{Family: f, Where: s.name, Line: i + 1, Piece: true})
						break
					}
				}
			}
		}
	}
	return found, nil
}

// readLogFiles reads the file or every regular file below the directory.
func readLogFiles(path string) ([]source, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		b, err := os.ReadFile(path)
		return []source{{name: path, data: b}}, err
	}
	var out []source
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(path, p)
		out = append(out, source{name: filepath.ToSlash(rel), data: b})
		return nil
	})
	return out, err
}

// scanLogPath scans a log file or a directory of log files for the canaries.
func scanLogPath(c canaries, path string) ([]leak, error) {
	srcs, err := readLogFiles(path)
	if err != nil {
		return nil, err
	}
	return scanSources(c, srcs...)
}

// sink is a log destination that several goroutines write: it keeps everything in memory and, when
// it has a file, writes the same bytes there, so the scan reads both the sink and the log file.
type sink struct {
	mu   sync.Mutex
	b    bytes.Buffer
	file *os.File
}

func newSink(t testing.TB, path string) *sink {
	t.Helper()
	s := &sink{}
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		s.file = f
		t.Cleanup(func() { _ = f.Close() })
	}
	return s
}

func (s *sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		if _, err := s.file.Write(p); err != nil {
			return 0, err
		}
	}
	return s.b.Write(p)
}

func (s *sink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *sink) source(name string) source { return source{name: name, data: []byte(s.String())} }

// TestSecLogScannerSeesCanariesInSinksFilesAndDirectories is the control of the log inspection: the
// scanner finds a canary, and a piece of a long one, in an in-memory sink, in a log file and in a file
// deep in a log directory, reports where, and finds nothing in clean logs. A scan of an empty
// directory or of empty logs is an error, not a pass.
//
// SEC-082
func TestSecLogScannerSeesCanariesInSinksFilesAndDirectories(t *testing.T) {
	c := canaries{
		"password": newCanary(t),
		"token":    newCanary(t) + "." + newCanary(t) + "." + newCanary(t),
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "rotated", "2026"), 0o700); err != nil {
		t.Fatal(err)
	}
	clean := []byte(`{"msg":"request","status":200}` + "\n")
	write := func(rel string, data []byte) string {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("alixai.log", clean)
	write("rotated/alixai.1.log", clean)
	if got, err := scanLogPath(c, dir); err != nil || len(got) != 0 {
		t.Fatalf("clean logs: leaks %v, err %v", got, err)
	}

	// Planted: the password whole, in a file deep in the directory; a segment of the token, in the top file.
	write("rotated/2026/alixai.2.log", append(append([]byte{}, clean...), []byte(`{"msg":"login","detail":"`+c["password"]+`"}`+"\n")...))
	seg := strings.Split(c["token"], ".")[1]
	write("alixai.log", append(append([]byte{}, clean...), []byte(`{"msg":"x","t":"`+seg+`"}`+"\n")...))
	got, err := scanLogPath(c, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"password in rotated/2026/alixai.2.log line 2": false, "token piece in alixai.log line 2": false}
	for _, l := range got {
		switch {
		case l.Family == "password" && l.Where == "rotated/2026/alixai.2.log" && l.Line == 2 && !l.Piece:
			want["password in rotated/2026/alixai.2.log line 2"] = true
		case l.Family == "token" && l.Where == "alixai.log" && l.Line == 2 && l.Piece:
			want["token piece in alixai.log line 2"] = true
		default:
			t.Errorf("unexpected sighting: %s", l)
		}
	}
	for what, seen := range want {
		if !seen {
			t.Errorf("the scanner did not find the planted %s (sightings: %v)", what, got)
		}
	}

	// A single file, and an in-memory sink.
	if got, err := scanLogPath(c, filepath.Join(dir, "alixai.log")); err != nil || len(got) != 1 {
		t.Errorf("single file: %v, %v", got, err)
	}
	s := newSink(t, filepath.Join(dir, "sink.log"))
	_, _ = s.Write([]byte(`{"msg":"` + c["token"] + `"}` + "\n"))
	if got, err := scanSources(c, s.source("sink")); err != nil || len(got) == 0 {
		t.Errorf("in-memory sink: %v, %v", got, err)
	}
	if got, err := scanLogPath(c, filepath.Join(dir, "sink.log")); err != nil || len(got) == 0 {
		t.Errorf("the sink's file does not hold what the sink held: %v, %v", got, err)
	}

	// Nothing to read is an error.
	if _, err := scanLogPath(c, t.TempDir()); err == nil {
		t.Error("the scan of an empty directory passed")
	}
	if _, err := scanSources(c, source{name: "empty"}); err == nil {
		t.Error("the scan of an empty log passed")
	}
	if _, err := scanLogPath(c, filepath.Join(dir, "no-such-file")); err == nil {
		t.Error("the scan of a missing path passed")
	}
}
