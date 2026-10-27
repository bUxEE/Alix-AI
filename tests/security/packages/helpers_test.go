package packagessec

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tests/security/secutil"
	"github.com/buxee/alix-ai/tools/devkeys"
)

const (
	// validMinimalDir is the committed fixture package: complete, valid and signed
	// with the published TEST-ONLY publisher key from 2026-10-01 for ten years.
	validMinimalDir = "examples/packages/valid-minimal"
	appID           = "demo-library"
	coreVersion     = apppkg.DevCoreVersion
)

// tNow is inside the validity window of the fixture's signature.
var tNow = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

// ---- archives ----------------------------------------------------------------

// tarEntry is one entry of a hand-built archive, with exactly the header asked for.
type tarEntry struct {
	name string
	typ  byte
	data []byte
	link string
}

func fileEntry(name, data string) tarEntry {
	return tarEntry{name: name, typ: tar.TypeReg, data: []byte(data)}
}

func linkEntry(name string, typ byte, target string) tarEntry {
	return tarEntry{name: name, typ: typ, link: target}
}

func specialEntry(name string, typ byte) tarEntry { return tarEntry{name: name, typ: typ} }

func writeEntry(tw *tar.Writer, e tarEntry) error {
	size := int64(0)
	if e.typ == tar.TypeReg {
		size = int64(len(e.data))
	}
	mode := int64(0o644)
	if e.typ == tar.TypeDir {
		mode = 0o755
	}
	hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: mode, Size: size, Linkname: e.link, Format: tar.FormatPAX}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header %q: %w", e.name, err)
	}
	if e.typ == tar.TypeReg {
		if _, err := tw.Write(e.data); err != nil {
			return err
		}
	}
	return nil
}

// tarGz builds a .tar.gz of the entries.
func tarGz(t testing.TB, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		if err := writeEntry(tw, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// baseEntries are the files of the committed valid-minimal package (its signature
// manifest included), named the way `tar czf x.tar.gz -C dir .` names them.
func baseEntries(t testing.TB) []tarEntry {
	t.Helper()
	root := filepath.Join(secutil.RepoRoot(t), filepath.FromSlash(validMinimalDir))
	var out []tarEntry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out = append(out, tarEntry{name: "./" + filepath.ToSlash(rel), typ: tar.TypeReg, data: b})
		return nil
	})
	if err != nil {
		t.Fatalf("reading %s: %v", validMinimalDir, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	if len(out) < 10 {
		t.Fatalf("the fixture package has only %d files", len(out))
	}
	return out
}

func with(base []tarEntry, extra ...tarEntry) []tarEntry {
	return append(append([]tarEntry(nil), base...), extra...)
}

// regularNames returns the sorted package-relative names of the regular files of an entry list.
func regularNames(entries []tarEntry) []string {
	var out []string
	for _, e := range entries {
		if e.typ == tar.TypeReg {
			out = append(out, strings.TrimPrefix(e.name, "./"))
		}
	}
	sort.Strings(out)
	return out
}

// ---- the tree walk -----------------------------------------------------------

// node is what the walk records of one path.
type node struct {
	mode fs.FileMode
	size int64
	sum  string
}

type tree map[string]node

// walk records every path below root without following links: mode for every
// path; size and a content digest only for regular files. Directory size is not
// a content signal (on tmpfs/overlayfs/btrfs it changes when entries are added).
func walk(t testing.TB, root string) tree {
	t.Helper()
	out := tree{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		n := node{mode: info.Mode()}
		if info.Mode().IsRegular() {
			n.size = info.Size()
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			s := sha256.Sum256(b)
			n.sum = hex.EncodeToString(s[:])
		}
		out[filepath.ToSlash(rel)] = n
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("the walk of %s found nothing, not even the root", root)
	}
	return out
}

// same reports whether two nodes are the same content. Directories compare by
// mode only: a directory replaced by a file or a link still differs.
func same(a, b node) bool {
	if a.mode.IsDir() && b.mode.IsDir() {
		return a.mode == b.mode
	}
	return a == b
}

// diff lists what differs between two walks: paths that appeared, vanished or changed.
func diff(before, after tree) []string {
	var out []string
	for p, n := range after {
		b, ok := before[p]
		switch {
		case !ok:
			out = append(out, "added "+p)
		case !same(b, n):
			out = append(out, "changed "+p)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			out = append(out, "removed "+p)
		}
	}
	sort.Strings(out)
	return out
}

// unsafeMode reports whether a file mode is a link or a special file: nothing of
// that kind may ever exist in an install tree.
func unsafeMode(m fs.FileMode) bool {
	return m&(fs.ModeSymlink|fs.ModeDevice|fs.ModeCharDevice|fs.ModeNamedPipe|fs.ModeSocket|fs.ModeIrregular) != 0
}

// unsafeNodes lists the links and special files of a walk.
func unsafeNodes(tr tree) []string {
	var out []string
	for p, n := range tr {
		if unsafeMode(n.mode) {
			out = append(out, fmt.Sprintf("%s (%s)", p, n.mode.Type()))
		}
	}
	sort.Strings(out)
	return out
}

// outside lists the changes of a diff that are not the directory prefix (slash
// path), something below it, or a parent directory that was created to hold it.
func outside(changes []string, prefix string) []string {
	var out []string
	for _, c := range changes {
		verb, p, _ := strings.Cut(c, " ")
		switch {
		case p == prefix, strings.HasPrefix(p, prefix+"/"):
		case verb == "added" && strings.HasPrefix(prefix, p+"/"):
		default:
			out = append(out, c)
		}
	}
	return out
}

// ---- the rig -----------------------------------------------------------------

// rig is one throwaway deployment: the data directory the installer writes to, a
// sentinel directory beside it that a traversal would hit, and the installer.
//
//	base/
//	  data/      the installer's data directory
//	  outside/   sentinel.txt, the file every escape is aimed at
type rig struct {
	base, data, outsideDir, sentinel string
	sentinelBytes                    []byte
	db                               *store.DB
	verifier                         *trust.Verifier
	inst                             *apppkg.Installer
	limits                           apppkg.Limits
}

func newRig(t *testing.T, limits apppkg.Limits) *rig {
	t.Helper()
	r := &rig{base: t.TempDir(), limits: limits}
	r.data = filepath.Join(r.base, "data")
	r.outsideDir = filepath.Join(r.base, "outside")
	for _, d := range []string{r.data, r.outsideDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The sentinel's content is random per run: an overwrite cannot be mistaken for
	// content that was already there.
	r.sentinelBytes = []byte("sentinel-" + randomHex(t))
	r.sentinel = filepath.Join(r.outsideDir, "sentinel.txt")
	if err := os.WriteFile(r.sentinel, r.sentinelBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(tNow)
	r.db = storetest.Open(t, nil)
	ts := trust.NewStore(r.db, clk, trust.AllowTestOnlyKeys())
	if _, err := ts.RegisterKey(context.Background(), appID, devkeys.KeyID, devkeys.PublicKey(), "security suite"); err != nil {
		t.Fatal(err)
	}
	r.verifier = trust.NewVerifier(ts, clk)
	inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{
		DB: r.db, Clock: clk, DataDir: r.data, Verifier: r.verifier, CoreVersion: coreVersion, Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.inst = inst
	return r
}

func randomHex(t testing.TB) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := crand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func (r *rig) loadOptions() apppkg.Options {
	return apppkg.Options{Verifier: r.verifier, CoreVersion: coreVersion, Limits: r.limits}
}

func (r *rig) snapshot(t testing.TB) tree { return walk(t, r.base) }

func (r *rig) rows(t testing.TB) int {
	t.Helper()
	var n int
	if err := r.db.QueryRowContext(context.Background(), `SELECT count(*) FROM apppkg_packages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (r *rig) sentinelIntact(t testing.TB) {
	t.Helper()
	got, err := os.ReadFile(r.sentinel)
	if err != nil {
		t.Fatalf("the sentinel is gone: %v", err)
	}
	if !bytes.Equal(got, r.sentinelBytes) {
		t.Fatal("the sentinel file outside the install directory was overwritten")
	}
}

// ---- findings ----------------------------------------------------------------

func criticalIDs(res *apppkg.Result) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range res.Findings {
		if f.Severity == contracts.SeverityCritical && !seen[f.CheckID] {
			seen[f.CheckID] = true
			out = append(out, f.CheckID)
		}
	}
	sort.Strings(out)
	return out
}

func hasCheck(res *apppkg.Result, id string) bool {
	for _, f := range res.Findings {
		if f.CheckID == id {
			return true
		}
	}
	return false
}

// requireRefused fails unless the loader refused the source: no package, no
// content address, not verified, and the critical check wanted.
func requireRefused(t testing.TB, res *apppkg.Result, wantCheck string) {
	t.Helper()
	if res.Package != nil || res.ManifestHash != "" || res.Authenticity == apppkg.AuthVerified {
		t.Fatalf("a hostile archive yielded a package (hash %q, %s)", res.ManifestHash, res.Authenticity)
	}
	if !contains(criticalIDs(res), wantCheck) {
		t.Fatalf("critical checks = %v, want %s", criticalIDs(res), wantCheck)
	}
}

// requireInstallRefused runs the installer and requires a refusal that carries
// the critical check and records nothing.
func requireInstallRefused(t testing.TB, r *rig, src apppkg.Source, wantCheck string) {
	t.Helper()
	_, err := r.inst.Install(context.Background(), src)
	var refused *apppkg.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Install error = %v (%T), want a *apppkg.RefusedError", err, err)
	}
	found := false
	for _, f := range refused.Findings {
		found = found || (f.CheckID == wantCheck && f.Severity == contracts.SeverityCritical)
	}
	if !found {
		t.Fatalf("the refusal carries no critical %s finding: %v", wantCheck, refused.Findings)
	}
	if n := r.rows(t); n != 0 {
		t.Fatalf("%d package rows recorded for a refused archive", n)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- streaming archives ------------------------------------------------------

// counter counts the bytes read through it.
type counter struct {
	r io.Reader
	n atomic.Int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c *counter) Count() int64 { return c.n.Load() }

// stream is a .tar.gz that is produced lazily while it is read: the entries of a
// real package followed by one big file. Nothing is buffered beyond what the
// reader has asked for, so an archive of many gigabytes costs the test nothing,
// and the bytes the loader consumed are exactly what it read.
type stream struct {
	*counter
	pr   *io.PipeReader
	done chan struct{}
}

type fill int

const (
	fillZeros  fill = iota // compresses about 1000 to 1: a decompression bomb
	fillRandom             // does not compress: a plain oversized archive
)

// newStream starts the generator. bigSize is the size the tar header declares for
// the big file; the generator writes that many bytes unless the reader stops
// first. level is the gzip level.
func newStream(t testing.TB, base []tarEntry, bigName string, bigSize int64, kind fill, level int) *stream {
	t.Helper()
	pr, pw := io.Pipe()
	s := &stream{counter: &counter{r: pr}, pr: pr, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		err := func() error {
			zw, err := gzip.NewWriterLevel(pw, level)
			if err != nil {
				return err
			}
			tw := tar.NewWriter(zw)
			for _, e := range base {
				if err := writeEntry(tw, e); err != nil {
					return err
				}
			}
			if err := tw.WriteHeader(&tar.Header{Name: bigName, Typeflag: tar.TypeReg, Mode: 0o644, Size: bigSize, Format: tar.FormatPAX}); err != nil {
				return err
			}
			chunk := make([]byte, 1<<20)
			if kind == fillRandom {
				rand.New(rand.NewSource(7)).Read(chunk)
			}
			for left := bigSize; left > 0; {
				n := int64(len(chunk))
				if n > left {
					n = left
				}
				if _, err := tw.Write(chunk[:n]); err != nil {
					return err
				}
				left -= n
			}
			if err := tw.Close(); err != nil {
				return err
			}
			return zw.Close()
		}()
		_ = pw.CloseWithError(err) // nil closes cleanly; a closed pipe is the reader walking away
	}()
	t.Cleanup(func() {
		_ = pr.CloseWithError(io.ErrClosedPipe)
		select {
		case <-s.done:
		case <-time.After(20 * time.Second):
			t.Error("the archive generator did not stop after the reader went away")
		}
	})
	return s
}

// stop ends the generator and waits for it.
func (s *stream) stop(t testing.TB) {
	t.Helper()
	_ = s.pr.CloseWithError(io.ErrClosedPipe)
	select {
	case <-s.done:
	case <-time.After(20 * time.Second):
		t.Fatal("the archive generator did not stop after the reader went away")
	}
}
