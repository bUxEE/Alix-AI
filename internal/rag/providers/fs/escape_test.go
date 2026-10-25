package fs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

type opResult struct {
	op   string
	err  error
	data []byte
}

// tryAll runs every per-item operation on key and collects what came back.
func (h *harness) tryAll(def source.Definition, key string) []opResult {
	h.t.Helper()
	var out []opResult
	_, err := h.p.Stat(h.ctx, scope(), def, key)
	out = append(out, opResult{op: "Stat", err: err})
	v, err := h.p.FetchVersion(h.ctx, scope(), def, key)
	out = append(out, opResult{op: "FetchVersion", err: err, data: []byte(v.Hash)})
	c, err := h.p.Open(h.ctx, scope(), def, key)
	r := opResult{op: "Open", err: err}
	if err == nil {
		r.data, _ = io.ReadAll(c)
		_ = c.Close()
	}
	return append(out, r)
}

// assertNoLeak fails if anything an operation returned carries the sentinel
// or a host path.
func (h *harness) assertNoLeak(what string, rs []opResult) {
	h.t.Helper()
	for _, r := range rs {
		if strings.Contains(string(r.data), sentinel) {
			h.t.Errorf("%s: %s returned the sentinel", what, r.op)
		}
		var ce *contracts.Error
		if r.err != nil && errors.As(r.err, &ce) && strings.Contains(ce.Message, h.tr.base) {
			h.t.Errorf("%s: %s error message leaks a host path: %q", what, r.op, ce.Message)
		}
	}
}

// SEC-015: path traversal, absolute names, drive, UNC and verbatim prefixes and
// aliases never reach the filesystem.
func TestCanonicalPathEscapeRejected(t *testing.T) {
	h := newHarness(t)
	h.tr.write("ok.md", "fine")
	h.tr.write("private/p.md", "private")
	h.tr.writeOutside("secret.md", sentinel)
	h.tr.put(h.tr.evil, "secret.md", sentinel)
	def := h.tr.def()
	outsideAbs := filepath.Join(h.tr.outside, "secret.md")

	t.Run("hostile keys never reach the filesystem", func(t *testing.T) {
		// Invalid on every host: the key validator refuses them before any
		// lookup, so nothing is statted, opened or hashed.
		forms := []string{
			"../outside/secret.md",
			"a/../../outside/secret.md",
			"../root-evil/secret.md",
			"..",
			".",
			"/etc/passwd",
			"//localhost/share/secret.md",
			filepath.ToSlash(outsideAbs), // the absolute path of the secret
			`..\outside\secret.md`,
			`..\..\outside\secret.md`,
			`sub\..\..\outside\secret.md`,
			`C:\Windows\win.ini`,
			`\\?\C:\Windows\win.ini`,
			`\\?\` + outsideAbs,
			`\\?\UNC\localhost\c$\Windows\win.ini`,
			`\\localhost\c$\Windows\win.ini`,
			`\\.\C:\Windows\win.ini`,
			`\\.\PhysicalDrive0`,
			`\??\C:\Windows\win.ini`,
			"ok.md/../../outside/secret.md",
			"ok.md\x00.png",
			"ok.md\n",
			"",
			"ok.md/",
			"./ok.md",
			"private//p.md",
		}
		for _, key := range forms {
			rs := h.tryAll(def, key)
			for _, r := range rs {
				if r.err == nil || !errors.Is(r.err, source.ErrEscapesRoot) || contracts.CodeOf(r.err) != contracts.ErrScopeViolation {
					t.Errorf("%s(%q) = %v, want a scope_violation wrapping ErrEscapesRoot", r.op, key, r.err)
				}
			}
			h.assertNoLeak(key, rs)
		}
		if got := h.opened(); len(got) != 0 {
			t.Errorf("a rejected key opened files: %v", got)
		}
	})

	t.Run("windows-only forms are refused on Windows and never reach outside the root anywhere", func(t *testing.T) {
		forms := []string{
			"ok.md::$DATA", "ok.md:stream", "C:/Windows/win.ini", "C:x", "ok.md.", "ok.md ", "private./p.md", "private /p.md", "CON", "nul.txt", "COM1", "aux.md", "CONIN$", "ok<.md", "ok?.md",
		}
		for _, key := range forms {
			rs := h.tryAll(def, key)
			for _, r := range rs {
				if r.err == nil {
					t.Errorf("%s(%q) succeeded", r.op, key)
				}
				if runtime.GOOS == "windows" && !errors.Is(r.err, source.ErrEscapesRoot) {
					t.Errorf("%s(%q) = %v, want ErrEscapesRoot on Windows", r.op, key, r.err)
				}
			}
			h.assertNoLeak(key, rs)
		}
	})

	t.Run("a resolver that disagrees with the filesystem is not trusted", func(t *testing.T) {
		real := h.p.evalSymlinks
		defer func() { h.p.evalSymlinks = real }()
		pick := func(target string) {
			h.p.evalSymlinks = func(p string) (string, error) {
				if strings.HasSuffix(filepath.ToSlash(p), "/ok.md") {
					return target, nil
				}
				return real(p)
			}
		}
		for name, target := range map[string]string{
			"outside the root":                  outsideAbs,
			"a sibling that shares the prefix":  filepath.Join(h.tr.evil, "secret.md"),
			"another name inside the root":      filepath.Join(h.tr.root, "private", "p.md"),
			"the root's parent":                 h.tr.base,
			"a verbatim spelling of an outside": `\\?\` + outsideAbs,
		} {
			pick(target)
			h.resetOpens()
			rs := h.tryAll(def, "ok.md")
			for _, r := range rs {
				if !errors.Is(r.err, source.ErrEscapesRoot) {
					t.Errorf("%s: %s = %v, want ErrEscapesRoot", name, r.op, r.err)
				}
			}
			h.assertNoLeak(name, rs)
			if got := h.opened(); len(got) != 0 {
				t.Errorf("%s: opened %v", name, got)
			}
		}
		// Control: the same key resolves normally with the real resolver.
		h.p.evalSymlinks = real
		for _, r := range h.tryAll(def, "ok.md") {
			if r.err != nil {
				t.Errorf("control %s: %v", r.op, r.err)
			}
		}
	})

	t.Run("an excluded directory cannot be reached by spelling it differently", func(t *testing.T) {
		excl := h.tr.def(func(d *ap.RAGSource) { d.Exclude = append(d.Exclude, "private/**") })
		fold := true
		h2 := newHarness(t, func(c *Config) { c.CaseInsensitive = &fold })
		h2.tr = h.tr
		for _, key := range []string{"private/p.md", "Private/p.md", "PRIVATE/P.MD"} {
			for _, r := range h2.tryAll(excl, key) {
				if r.err == nil {
					t.Errorf("%s(%q) read an excluded file", r.op, key)
				}
			}
		}
		if got := h2.opened(); len(got) != 0 {
			t.Errorf("opened %v", got)
		}
	})

	t.Run("roots that are not plain absolute paths are unreachable, never relative to the working directory", func(t *testing.T) {
		sep := string(filepath.Separator)
		for name, root := range map[string]string{
			"relative":        "relative" + sep + "dir",
			"dot":             ".",
			"dot-dot segment": h.tr.root + sep + ".." + sep + "root",
			"empty":           "",
			"device path":     `\\.\` + h.tr.root,
		} {
			d := h.tr.def()
			d.Root.Path = root
			hl, err := h.p.Probe(h.ctx, scope(), d)
			if err != nil {
				t.Errorf("%s: Probe error = %v", name, err)
				continue
			}
			if hl.Availability != source.Unavailable || hl.Reachable() || !hl.CheckedAt.IsZero() {
				t.Errorf("%s: health = %+v, want unavailable and not checked", name, hl)
			}
		}
	})

	t.Run("verbatim prefix on the declared root is understood on Windows", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("verbatim paths exist on Windows only")
		}
		d := h.tr.def()
		d.Root.Path = `\\?\` + h.tr.root
		hl, err := h.p.Probe(h.ctx, scope(), d)
		if err != nil || !hl.Reachable() {
			t.Fatalf("Probe(%q) = %+v, %v", d.Root.Path, hl, err)
		}
		cs := h.discover(d, nil)
		if !equalStrings(keysOf(cs.Added), []string{"ok.md", "private/p.md"}) {
			t.Errorf("added = %v", keysOf(cs.Added))
		}
		d.Root.Path = strings.ToUpper(h.tr.root)
		if hl, err := h.p.Probe(h.ctx, scope(), d); err != nil || !hl.Reachable() {
			t.Errorf("an upper-cased root is the same directory on Windows: %+v, %v", hl, err)
		}
	})

	t.Run("a folder source may not be a whole volume", func(t *testing.T) {
		d := h.tr.def()
		d.Root.Path = filepath.VolumeName(h.tr.root) + string(filepath.Separator)
		if runtime.GOOS != "windows" {
			d.Root.Path = "/"
		}
		_, err := h.p.Probe(h.ctx, scope(), d)
		if contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("Probe(volume root) error = %v, want validation_failed", err)
		}
		if _, err := h.p.Discover(h.ctx, scope(), d, nil); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("Discover(volume root) error = %v, want validation_failed", err)
		}
	})
}

// SEC-015: a link that leads out of the root is never followed, in a scan or by
// key. Real symbolic links need a privilege on Windows, so those cases skip
// there with a message; the junction case and the in-memory cases always run.
func TestSymlinkOutsideRootRejected(t *testing.T) {
	t.Run("link targets are classified without touching the filesystem", func(t *testing.T) {
		type tc struct {
			p         source.Paths
			canon     string
			key, tgt  string
			want      source.Reason
			explained string
		}
		posix, win := source.Paths{}, source.Paths{Windows: true}
		cases := []tc{
			{posix, "/srv/docs", "a.md", "b.md", source.ReasonSymlink, "sibling in the root"},
			{posix, "/srv/docs", "d/a.md", "../b.md", source.ReasonSymlink, "up one, still inside"},
			{posix, "/srv/docs", "d/a.md", "../../x.md", source.ReasonLinkEscape, "up past the root"},
			{posix, "/srv/docs", "a.md", "../outside/secret.md", source.ReasonLinkEscape, "to a sibling of the root"},
			{posix, "/srv/docs", "a.md", "/etc/passwd", source.ReasonLinkEscape, "absolute, elsewhere"},
			{posix, "/srv/docs", "a.md", "/srv/docs/b.md", source.ReasonSymlink, "absolute, inside"},
			{posix, "/srv/docs", "a.md", "/srv/docs2/b.md", source.ReasonLinkEscape, "absolute, prefix collision"},
			{posix, "/srv/docs", "a.md", "", source.ReasonLinkEscape, "empty target"},
			{posix, "/srv/docs", "a.md", "..", source.ReasonLinkEscape, "the parent"},
			{win, `C:\root`, "a.md", `..\outside\secret.md`, source.ReasonLinkEscape, "backslash climb"},
			{win, `C:\root`, "d/a.md", `..\b.md`, source.ReasonSymlink, "backslash, inside"},
			{win, `C:\root`, "a.md", `C:\root\b.md`, source.ReasonSymlink, "drive path, inside"},
			{win, `C:\root`, "a.md", `c:\ROOT\b.md`, source.ReasonSymlink, "drive path, other case"},
			{win, `C:\root`, "a.md", `C:\root2\b.md`, source.ReasonLinkEscape, "drive path, prefix collision"},
			{win, `C:\root`, "a.md", `D:\root\b.md`, source.ReasonLinkEscape, "other drive"},
			{win, `C:\root`, "a.md", `\\?\C:\root\b.md`, source.ReasonSymlink, "verbatim, inside"},
			{win, `C:\root`, "a.md", `\\?\C:\Windows\win.ini`, source.ReasonLinkEscape, "verbatim, outside"},
			{win, `C:\root`, "a.md", `\\nas\share\x`, source.ReasonLinkEscape, "UNC"},
			{win, `C:\root`, "a.md", `\\?\UNC\nas\share\x`, source.ReasonLinkEscape, "verbatim UNC"},
			{win, `C:\root`, "a.md", `\Windows\win.ini`, source.ReasonLinkEscape, "rooted on the current drive"},
		}
		for _, c := range cases {
			if got := classifyLink(c.p, c.canon, c.key, c.tgt); got != c.want {
				t.Errorf("%s: classifyLink(%q -> %q) = %s, want %s", c.explained, c.key, c.tgt, got, c.want)
			}
		}
	})

	t.Run("fake links and special files in a listing are skipped and never opened", func(t *testing.T) {
		h := newHarness(t)
		h.tr.write("ok.md", "fine")
		h.tr.writeOutside("secret.md", sentinel)
		def := h.tr.def()
		h.p.wrapFS = func(base iofs.FS) iofs.FS {
			return withEntries{FS: base, extra: fstest.MapFS{
				"link-out.md":  {Mode: iofs.ModeSymlink | 0o777, Data: []byte("../outside/secret.md")},
				"link-abs.md":  {Mode: iofs.ModeSymlink | 0o777, Data: []byte(filepath.ToSlash(filepath.Join(h.tr.outside, "secret.md")))},
				"pipe.md":      {Mode: iofs.ModeNamedPipe | 0o644},
				"socket.md":    {Mode: iofs.ModeSocket | 0o644},
				"device.md":    {Mode: iofs.ModeDevice | 0o644},
				"reparse.md":   {Mode: iofs.ModeIrregular | 0o644},
				"gone-link.md": {Mode: iofs.ModeSymlink | 0o777, Data: []byte("x")},
			}}
		}
		cs := h.discover(def, nil)
		if !equalStrings(keysOf(cs.Added), []string{"ok.md"}) {
			t.Errorf("added = %v, want only ok.md", keysOf(cs.Added))
		}
		got := skipKeys(cs.Skipped)
		want := map[string]source.Reason{
			// The fake links cannot be read back through the root, and an
			// unreadable link is distrusted.
			"link-out.md": source.ReasonLinkEscape, "link-abs.md": source.ReasonLinkEscape, "gone-link.md": source.ReasonLinkEscape,
			"pipe.md": source.ReasonIrregular, "socket.md": source.ReasonIrregular, "device.md": source.ReasonIrregular, "reparse.md": source.ReasonIrregular,
		}
		for k, r := range want {
			if got[k] != r {
				t.Errorf("skip %s = %q, want %q", k, got[k], r)
			}
		}
		if cs.Counts.Discovered != 8 || cs.Counts.Skipped != 7 {
			t.Errorf("counts = %+v", cs.Counts)
		}
		for _, o := range h.opened() {
			rel, ok := h.p.paths.Rel(h.tr.root, o)
			if !ok {
				t.Errorf("opened %s outside the root", o)
			}
			if rel != "ok.md" {
				t.Errorf("opened a skipped entry: %s", rel)
			}
		}
	})

	t.Run("symbolic links", func(t *testing.T) {
		h := newHarness(t)
		h.tr.write("ok.md", "fine")
		h.tr.writeOutside("secret.md", sentinel)
		h.tr.writeOutside("nested/deep.md", sentinel)
		h.tr.put(h.tr.evil, "secret.md", sentinel)
		links := map[string]string{
			"link-file.md":   filepath.Join(h.tr.outside, "secret.md"),
			"link-dir":       h.tr.outside,
			"link-evil":      h.tr.evil,
			"link-rel.md":    filepath.Join("..", "outside", "secret.md"),
			"link-inside.md": "ok.md",
			"link-dangling":  "nowhere.md",
		}
		for name, target := range links {
			if !trySymlink(t, target, h.tr.abs(name)) {
				t.Skip("cannot create symbolic links here (no privilege); the junction and in-memory subtests cover this on Windows")
			}
		}
		def := h.tr.def()
		cs := h.discover(def, nil)
		if !equalStrings(keysOf(cs.Added), []string{"ok.md"}) {
			t.Errorf("added = %v, want only ok.md", keysOf(cs.Added))
		}
		got := skipKeys(cs.Skipped)
		for _, k := range []string{"link-file.md", "link-dir", "link-evil", "link-rel.md"} {
			if got[k] != source.ReasonLinkEscape {
				t.Errorf("skip %s = %q, want symlink_escape", k, got[k])
			}
		}
		for _, k := range []string{"link-inside.md", "link-dangling"} {
			if got[k] != source.ReasonSymlink {
				t.Errorf("skip %s = %q, want symlink (links are not followed even inside the root)", k, got[k])
			}
		}
		for _, key := range []string{"link-file.md", "link-dir", "link-dir/secret.md", "link-dir/nested/deep.md", "link-evil/secret.md", "link-rel.md", "link-inside.md", "link-dangling"} {
			rs := h.tryAll(def, key)
			for _, r := range rs {
				if !errors.Is(r.err, source.ErrEscapesRoot) {
					t.Errorf("%s(%q) = %v, want ErrEscapesRoot", r.op, key, r.err)
				}
			}
			h.assertNoLeak(key, rs)
		}
		for _, o := range h.opened() {
			if rel, ok := h.p.paths.Rel(h.tr.root, o); !ok || rel != "ok.md" {
				t.Errorf("opened %s", o)
			}
		}
	})

	t.Run("a directory junction", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("junctions exist on Windows only")
		}
		h := newHarness(t)
		h.tr.write("ok.md", "fine")
		h.tr.writeOutside("secret.md", sentinel)
		h.tr.writeOutside("nested/deep.md", sentinel)
		h.tr.put(h.tr.evil, "secret.md", sentinel)
		if !makeJunction(t, h.tr.abs("junc-out"), h.tr.outside) || !makeJunction(t, h.tr.abs("junc-evil"), h.tr.evil) {
			t.Skip("cannot create junctions here")
		}
		def := h.tr.def()
		cs := h.discover(def, nil)
		if !equalStrings(keysOf(cs.Added), []string{"ok.md"}) {
			t.Errorf("added = %v, want only ok.md", keysOf(cs.Added))
		}
		got := skipKeys(cs.Skipped)
		for _, k := range []string{"junc-out", "junc-evil"} {
			if got[k] != source.ReasonIrregular {
				t.Errorf("skip %s = %q, want irregular_file", k, got[k])
			}
		}
		for _, key := range []string{"junc-out", "junc-out/secret.md", "junc-out/nested/deep.md", "junc-evil/secret.md"} {
			rs := h.tryAll(def, key)
			for _, r := range rs {
				if !errors.Is(r.err, source.ErrEscapesRoot) {
					t.Errorf("%s(%q) = %v, want ErrEscapesRoot", r.op, key, r.err)
				}
			}
			h.assertNoLeak(key, rs)
		}
		for _, o := range h.opened() {
			if rel, ok := h.p.paths.Rel(h.tr.root, o); !ok || rel != "ok.md" {
				t.Errorf("opened %s", o)
			}
		}
	})
}

// withEntries is an fs.FS that lists extra entries (fake links and special
// files) in the root directory next to the real ones. Everything else is the
// wrapped file system.
type withEntries struct {
	iofs.FS
	extra fstest.MapFS
}

func (w withEntries) ReadDir(name string) ([]iofs.DirEntry, error) {
	entries, err := iofs.ReadDir(w.FS, name)
	if err != nil || name != "." {
		return entries, err
	}
	// MapFS follows the links it holds when asked to open or stat them, so the
	// entries are taken from its own listing, which keeps their modes.
	extra, err := iofs.ReadDir(w.extra, ".")
	if err != nil {
		return nil, err
	}
	return append(entries, extra...), nil
}

// SEC-015: whatever the scan and the per-item calls are asked, nothing outside
// the declared root is opened, and nothing from outside it comes back.
func TestNeverReadsOutsideDeclaredRoot(t *testing.T) {
	h := newHarness(t)
	h.tr.write("ok.md", "inside one")
	h.tr.write("sub/ok2.md", "inside two")
	h.tr.writeOutside("secret.md", sentinel)
	h.tr.writeOutside("secret.txt", sentinel)
	h.tr.writeOutside("nested/deep.md", sentinel)
	h.tr.put(h.tr.evil, "secret.md", sentinel)
	h.tr.put(h.tr.base, "root-secret.md", sentinel)

	// Links out of the root, of whichever kind this host can make.
	linked := map[string]string{}
	for name, target := range map[string]string{"linkout": h.tr.outside, "linkevil": h.tr.evil} {
		switch {
		case makeJunction(t, h.tr.abs(name), target), trySymlink(t, target, h.tr.abs(name)):
			linked[name] = target
		}
	}
	for name, target := range map[string]string{"linkfile.md": filepath.Join(h.tr.outside, "secret.md")} {
		if trySymlink(t, target, h.tr.abs(name)) {
			linked[name] = target
		}
	}
	t.Logf("links created: %d", len(linked))

	def := h.tr.def()
	sentinelHash := sha256.Sum256([]byte(sentinel))
	badHash := source.HashPrefix + hex.EncodeToString(sentinelHash[:])

	checkChangeSet := func(label string, cs source.ChangeSet) {
		t.Helper()
		for _, it := range append(append([]source.Item{}, cs.Added...), cs.Modified...) {
			if it.Version.Hash == badHash || strings.Contains(it.Key, "secret") || strings.Contains(it.Key, "deep") || strings.HasPrefix(it.Key, "link") {
				t.Errorf("%s: an outside item came back: %+v", label, it)
			}
		}
		for _, r := range cs.Next {
			if r.Hash == badHash || strings.Contains(r.Key, "secret") || strings.Contains(r.Key, "deep") || strings.HasPrefix(r.Key, "link") {
				t.Errorf("%s: an outside record came back: %+v", label, r)
			}
		}
	}

	cs1 := h.discover(def, nil)
	checkChangeSet("first scan", cs1)
	if !equalStrings(keysOf(cs1.Added), []string{"ok.md", "sub/ok2.md"}) {
		t.Errorf("added = %v", keysOf(cs1.Added))
	}
	cs2 := h.discover(def, cs1.Next)
	checkChangeSet("second scan", cs2)

	// Previous records that name things outside the root, as a corrupted or
	// hostile store could hold, are never looked up by that name.
	hostile := append([]source.Record{}, cs1.Next...)
	for _, k := range []string{"../outside/secret.md", `..\outside\secret.md`, "/etc/passwd", "linkout/secret.md", "linkout/nested/deep.md", "linkevil/secret.md"} {
		hostile = append(hostile, source.Record{Key: k, Size: 1, Hash: badHash, MIME: source.MIMEText, SeenAt: clockStart, ModTime: oldTime})
	}
	cs3 := h.discover(def, hostile)
	checkChangeSet("scan with hostile history", cs3)
	dd, err := h.p.DetectDeletions(h.ctx, scope(), def, hostile)
	if err != nil {
		t.Fatalf("DetectDeletions: %v", err)
	}
	checkChangeSet("detect deletions", dd)
	for _, cs := range []source.ChangeSet{cs3, dd} {
		got := tombstoneReasons(cs.Deleted)
		for _, k := range []string{"../outside/secret.md", `..\outside\secret.md`, "/etc/passwd"} {
			if got[k] != source.ReasonUnsafeName {
				t.Errorf("hostile key %q tombstone = %q, want unsafe_name", k, got[k])
			}
		}
		for _, k := range []string{"linkout/secret.md", "linkout/nested/deep.md", "linkevil/secret.md"} {
			if _, ok := linked[strings.SplitN(k, "/", 2)[0]]; !ok {
				continue // no link was created for this name on this host
			}
			if r := got[k]; r != source.ReasonLinkEscape && r != source.ReasonIrregular {
				t.Errorf("a record behind a link %q tombstone = %q, want symlink_escape or irregular_file", k, r)
			}
		}
	}

	keys := []string{
		"../outside/secret.md", "../root-evil/secret.md", "../root-secret.md", "../secret.md", "/etc/passwd", filepath.ToSlash(filepath.Join(h.tr.outside, "secret.md")),
		`..\outside\secret.md`, `\\?\` + filepath.Join(h.tr.outside, "secret.md"), `\\localhost\c$\x`, "sub/../../outside/secret.md",
		"linkout", "linkout/secret.md", "linkout/nested/deep.md", "linkevil/secret.md", "linkfile.md", "secret.md", "nested/deep.md",
	}
	keys = append(keys, keysOf(cs1.Added)...)
	for _, key := range keys {
		h.assertNoLeak(key, h.tryAll(def, key))
	}

	// Layer four on its own: even with every earlier check bypassed, the
	// os.Root the provider reads through refuses a path that leaves the root.
	before := h.opened() // these deliberate bypass attempts are not part of the audit below
	rh, err := h.p.openRoot(def)
	if err != nil {
		t.Fatal(err)
	}
	for name := range linked {
		for _, key := range []string{name + "/secret.md", name + "/nested/deep.md", name} {
			if f, err := rh.r.Open(key); err == nil { // the root itself, without openFile's own checks
				body, _ := io.ReadAll(f)
				f.Close()
				if strings.Contains(string(body), sentinel) || len(body) > 0 {
					t.Errorf("the root let openFile(%q) read %d bytes through a link", key, len(body))
				}
			}
		}
	}
	rh.close()
	h.mu.Lock()
	h.opens = before
	h.mu.Unlock()

	// Every file the provider opened is inside the root, by the same
	// separator-aware prefix check the provider uses.
	opened := h.opened()
	if len(opened) == 0 {
		t.Fatal("the hook saw no opens; the test would pass vacuously")
	}
	for _, o := range opened {
		if !h.p.paths.Within(h.tr.root, o) {
			t.Errorf("opened %s, which is outside %s", o, h.tr.root)
		}
		rel, _ := h.p.paths.Rel(h.tr.root, o)
		if !strings.HasPrefix(rel, "ok.md") && !strings.HasPrefix(rel, "sub/") {
			t.Errorf("opened %s; only ok.md and sub/ok2.md are readable here", rel)
		}
	}
}

// SEC-015: an item swapped after the provider checked it and before it opens
// it (for another file, or for a link into an excluded folder, which os.Root
// follows because it stays inside the root) is not read: the file opened must
// be the file that was checked.
func TestSwapBeforeOpenRefused(t *testing.T) {
	const key = "public/sub/a.md"
	swaps := []struct {
		name string
		swap func(h *harness) bool
	}{
		{"the file for another file", func(h *harness) bool {
			return os.Rename(h.tr.writeOutside("decoy.md", sentinel), h.tr.abs(key)) == nil
		}},
		{"the file for a link into an excluded folder", func(h *harness) bool {
			h.tr.remove(key)
			return trySymlink(h.t, filepath.Join("..", "..", "private", "a.md"), h.tr.abs(key))
		}},
		{"its folder for a link into an excluded folder", func(h *harness) bool {
			if err := os.Rename(h.tr.abs("public/sub"), filepath.Join(h.tr.outside, "moved")); err != nil {
				return false
			}
			return makeJunction(h.t, h.tr.abs("public/sub"), h.tr.abs("private")) || trySymlink(h.t, filepath.Join("..", "private"), h.tr.abs("public/sub"))
		}},
	}
	sum := sha256.Sum256([]byte(sentinel))
	badHash := source.HashPrefix + hex.EncodeToString(sum[:])
	ops := map[string]func(h *harness, def source.Definition) []opResult{
		"Discover": func(h *harness, def source.Definition) []opResult {
			cs, err := h.p.Discover(h.ctx, scope(), def, nil)
			var out []opResult
			for _, it := range append(cs.Added, cs.Modified...) {
				out = append(out, opResult{op: "Discover " + it.Key, err: err, data: []byte(it.Version.Hash)})
			}
			for _, r := range cs.Next {
				out = append(out, opResult{op: "Discover record " + r.Key, err: err, data: []byte(r.Hash)})
			}
			return out
		},
		"FetchVersion": func(h *harness, def source.Definition) []opResult {
			v, err := h.p.FetchVersion(h.ctx, scope(), def, key)
			return []opResult{{op: "FetchVersion", err: err, data: []byte(v.Hash)}}
		},
		"Open": func(h *harness, def source.Definition) []opResult {
			c, err := h.p.Open(h.ctx, scope(), def, key)
			r := opResult{op: "Open", err: err}
			if err == nil {
				r.data, _ = io.ReadAll(c)
				_ = c.Close()
			}
			return []opResult{r}
		},
	}
	for _, sw := range swaps {
		for opName, op := range ops {
			t.Run(sw.name+"/"+opName, func(t *testing.T) {
				h := newHarness(t)
				h.tr.write(key, "public text")
				h.tr.write("private/a.md", sentinel)
				def := h.tr.def(func(d *ap.RAGSource) { d.Exclude = []ap.GlobPattern{"private/**"} })
				var once sync.Once
				swapped := false
				h.p.onOpen = func(string) { once.Do(func() { swapped = sw.swap(h) }) }
				rs := op(h, def)
				if !swapped {
					t.Skip("this host cannot make the swap (symbolic links need a privilege on Windows)")
				}
				for _, r := range rs {
					if string(r.data) == badHash || strings.Contains(string(r.data), sentinel) {
						t.Errorf("%s read the swapped-in file", r.op)
					}
				}
				if opName != "Discover" && rs[0].err == nil {
					t.Errorf("%s succeeded on a swapped item", opName)
				}
			})
		}
	}
}
