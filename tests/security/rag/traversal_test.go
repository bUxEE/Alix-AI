package ragsec

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/rag/parse/parsetest"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// tryLink makes a symbolic link, or reports that this host does not allow it
// (Windows needs a privilege for it).
func tryLink(t testing.TB, target, link string) bool {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Logf("cannot create a symbolic link on this host (%s): %v", runtime.GOOS, err)
		return false
	}
	return true
}

// tryJunction makes a Windows directory junction (a reparse point that needs no
// privilege) from link to target, or reports that this host cannot.
func tryJunction(t testing.TB, target, link string) bool {
	t.Helper()
	if runtime.GOOS != "windows" {
		return false
	}
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Logf("cannot create a junction here: %v: %s", err, bytes.TrimSpace(out))
		return false
	}
	return true
}

// outcome is what one per-item provider operation returned.
type outcome struct {
	op   string
	err  error
	data string
}

// tryAll runs every per-item operation of the provider on a key.
func (w *world) tryAll(s *src, key string) []outcome {
	w.t.Helper()
	var out []outcome
	_, err := w.prov.Stat(bg, s.scope, s.def, key)
	out = append(out, outcome{op: "Stat", err: err})
	v, err := w.prov.FetchVersion(bg, s.scope, s.def, key)
	out = append(out, outcome{op: "FetchVersion", err: err, data: v.Hash})
	c, err := w.prov.Open(bg, s.scope, s.def, key)
	o := outcome{op: "Open", err: err}
	if err == nil {
		b, _ := io.ReadAll(c)
		_ = c.Close()
		o.data = string(b)
	}
	return append(out, o)
}

// SEC-015: path traversal reaches files outside the declared source root (".."
// names, absolute names, symlinks, Windows \\?\ and UNC prefixes, zip-slip entry
// names). A canary file with a generated marker sits beside the root; each
// escape form is tried by name against the production provider, as a link inside
// the root, as a reference inside a document and as an archive entry name inside a
// docx ingested by the real worker, and the marker is never read, never indexed,
// never retrieved and never sent to a model.
func TestSecRAGPathTraversal(t *testing.T) {
	canary := "OUTSIDE-ROOT-" + sentinel
	slipMarker := "ZIPSLIP-CONTENT-" + sentinel

	w := newWorld(t)
	sc := scopeFor("tenant-a", "ws-1", "usr-alice")
	s := w.addSource(sc, "library")
	outsideFile := filepath.Join(s.outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte(canary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A sibling directory of the root whose name starts with the root's name: the
	// prefix trap of a check that compares strings.
	evil := s.root + "-evil"
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evil, "secret.txt"), []byte(canary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s.writeString("ok.md", "# Notes\n\nAn ordinary note about the lending desk and its opening hours.\n")
	// References that point outside the root: the parser never follows them.
	s.writeString("links.md", "# Links\n\nSee ![image](../outside/secret.txt) and [the file](../outside/secret.txt) and <file://"+
		filepath.ToSlash(outsideFile)+"> for details about printing.\n")
	s.writeString("page.html", `<html><body><p>The page explains printing at the library.</p><img src="../outside/secret.txt">`+
		`<iframe src="../outside/secret.txt"></iframe><link rel="import" href="../outside/secret.txt">`+
		`<object data="file://`+filepath.ToSlash(outsideFile)+`"></object></body></html>`)
	// A link inside the root to a file outside it, and to the directory beside it.
	linkedFile := tryLink(t, outsideFile, filepath.Join(s.root, "link.txt"))
	linkedDir := tryLink(t, s.outside, filepath.Join(s.root, "linkdir")) || tryJunction(t, s.outside, filepath.Join(s.root, "linkdir"))
	// Archive entries that name places outside the extraction root.
	slipNames := []string{
		"../../zipslip-" + sentinel + ".txt",
		"/tmp/zipslip-" + sentinel + ".txt",
		`..\..\zipslip-` + sentinel + `.txt`,
		`C:\zipslip-` + sentinel + `.txt`,
	}
	var slips []string
	for i, name := range slipNames {
		file := "slip" + itoa(i) + ".docx"
		slips = append(slips, file)
		s.write(file, parsetest.RawZip([]parsetest.RawEntry{
			{Name: "word/document.xml", Data: []byte("<w:document " + parsetest.DocxNS + "><w:body><w:p><w:r><w:t>An ordinary slip document.</w:t></w:r></w:p></w:body></w:document>")},
			{Name: name, Data: []byte(slipMarker)},
		}))
	}
	w.syncOK(s)

	t.Run("hostile item names never reach the filesystem", func(t *testing.T) {
		// Refused on every host before any lookup.
		always := []string{
			"../outside/secret.txt",
			"a/../../outside/secret.txt",
			"../" + filepath.Base(evil) + "/secret.txt",
			"..", ".",
			"/etc/passwd",
			"//localhost/share/secret.txt",
			filepath.ToSlash(outsideFile),
			`..\outside\secret.txt`,
			`..\..\outside\secret.txt`,
			`sub\..\..\outside\secret.txt`,
			`C:\Windows\win.ini`,
			`\\?\C:\Windows\win.ini`,
			`\\?\` + outsideFile,
			`\\?\UNC\localhost\c$\Windows\win.ini`,
			`\\localhost\c$\Windows\win.ini`,
			`\\.\C:\Windows\win.ini`,
			`\\.\PhysicalDrive0`,
			`\??\C:\Windows\win.ini`,
			"ok.md/../../outside/secret.txt",
			"ok.md\x00.png",
			"ok.md\n",
			"",
			"ok.md/",
			"./ok.md",
		}
		for _, key := range always {
			for _, o := range w.tryAll(s, key) {
				if o.err == nil || !errors.Is(o.err, source.ErrEscapesRoot) || contracts.CodeOf(o.err) != contracts.ErrScopeViolation {
					t.Errorf("%s(%q) = %v, want a scope_violation wrapping ErrEscapesRoot", o.op, key, o.err)
				}
				if strings.Contains(o.data, canary) {
					t.Errorf("%s(%q) returned the canary", o.op, key)
				}
				var ce *contracts.Error
				if o.err != nil && errors.As(o.err, &ce) && strings.Contains(ce.Message, w.base) {
					t.Errorf("%s(%q): the error message names a host path: %q", o.op, key, ce.Message)
				}
			}
		}
		// Forms that are only unsafe on Windows: refused there as escapes; on other
		// hosts they are ordinary names of files that do not exist, and still fail.
		for _, key := range []string{"ok.md::$DATA", "ok.md:stream", "C:/Windows/win.ini", "C:x", "ok.md.", "ok.md ", "CON", "nul.txt", "COM1", "ok<.md", "ok?.md"} {
			for _, o := range w.tryAll(s, key) {
				if o.err == nil {
					t.Errorf("%s(%q) succeeded", o.op, key)
				}
				if runtime.GOOS == "windows" && !errors.Is(o.err, source.ErrEscapesRoot) {
					t.Errorf("%s(%q) = %v, want ErrEscapesRoot on Windows", o.op, key, o.err)
				}
				if strings.Contains(o.data, canary) {
					t.Errorf("%s(%q) returned the canary", o.op, key)
				}
			}
		}
		// Control: an ordinary key resolves, so the refusals above are about the names.
		for _, o := range w.tryAll(s, "ok.md") {
			if o.err != nil {
				t.Errorf("control: %s(ok.md) = %v", o.op, o.err)
			}
		}
	})

	t.Run("roots that are not plain absolute paths are unreachable", func(t *testing.T) {
		sep := string(filepath.Separator)
		for name, root := range map[string]string{
			"relative":        "relative" + sep + "dir",
			"dot":             ".",
			"dot-dot segment": s.root + sep + ".." + sep + "outside",
			"empty":           "",
			"device path":     `\\.\` + s.root,
		} {
			d := s.def
			d.Root.Path = root
			h, err := w.prov.Probe(bg, sc, d)
			if err != nil {
				t.Errorf("%s: Probe error = %v", name, err)
				continue
			}
			if h.Availability != source.Unavailable || h.Reachable() || !h.CheckedAt.IsZero() {
				t.Errorf("%s: health = %+v, want unavailable and not checked", name, h)
			}
			if cs, err := w.prov.Discover(bg, sc, d, nil); err == nil || len(cs.Added) != 0 {
				t.Errorf("%s: Discover = %d added, %v: a root that is not a plain absolute path was scanned", name, len(cs.Added), err)
			}
		}
		// The same rules as pure functions, on both host flavours, wherever the
		// suite runs: verbatim and UNC spellings of a plain path are accepted and
		// normalised, device and volume-GUID namespaces and drive-relative or
		// rooted forms are refused.
		win, posix := source.Paths{Windows: true}, source.Paths{}
		for in, want := range map[string]string{`\\?\C:\data\lib`: `C:\data\lib`, `\\?\UNC\srv\share\lib`: `\\srv\share\lib`, `C:/data//lib/.`: `C:\data\lib`, `\??\C:\data`: `C:\data`} {
			if got, err := win.Normalize(in); err != nil || got != want {
				t.Errorf("windows Normalize(%q) = %q, %v, want %q", in, got, err, want)
			}
		}
		for _, in := range []string{`\\.\PhysicalDrive0`, `\\?\Volume{0f2a9f10-0000-0000-0000-100000000000}\x`, `\\?\GLOBALROOT\Device\HarddiskVolume1\x`,
			`data\lib`, `C:data`, `\rooted`, `C:\data\..\other`} {
			if got, err := win.Normalize(in); err == nil {
				t.Errorf("windows Normalize(%q) = %q, want an error", in, got)
			}
		}
		for _, in := range []string{"relative/dir", "/data/../etc", "", "/data/\x00x"} {
			if got, err := posix.Normalize(in); err == nil {
				t.Errorf("posix Normalize(%q) = %q, want an error", in, got)
			}
		}
	})

	t.Run("links are never followed", func(t *testing.T) {
		if !linkedFile && !linkedDir {
			t.Skip("this host does not allow symbolic links without a privilege; the provider's own tests classify link targets in memory")
		}
		vs := w.versions(s)
		if linkedFile {
			if _, ok := vs["link.txt"]; ok {
				t.Errorf("a link to a file outside the root was ingested: %+v", vs["link.txt"])
			}
			for _, o := range w.tryAll(s, "link.txt") {
				if o.err == nil || strings.Contains(o.data, canary) {
					t.Errorf("%s(link.txt) = %v, %q: a link was followed", o.op, o.err, o.data)
				}
			}
		}
		if linkedDir {
			for _, o := range w.tryAll(s, "linkdir/secret.txt") {
				if o.err == nil || strings.Contains(o.data, canary) {
					t.Errorf("%s(linkdir/secret.txt) = %v, %q: a link to a directory was followed", o.op, o.err, o.data)
				}
			}
		}
		if st := w.status(s); st.Counts.Skipped == 0 {
			t.Errorf("the links were not counted as skipped: %+v", st.Counts)
		}
	})

	t.Run("references inside documents are not followed", func(t *testing.T) {
		for _, f := range []string{"links.md", "page.html"} {
			if v := w.versions(s)[f]; v.State != "indexed" {
				t.Errorf("%s: %+v, want indexed (its own text is ordinary)", f, v)
			}
		}
		if hits := w.search(sc, "printing library", false); len(hits) == 0 {
			t.Error("control: the text of the documents that carry the references is not retrievable")
		}
	})

	t.Run("archive entry names are never used as paths", func(t *testing.T) {
		for i, name := range slipNames {
			doc := parsetest.RawZip([]parsetest.RawEntry{
				{Name: "word/document.xml", Data: []byte("<w:document " + parsetest.DocxNS + "><w:body/></w:document>")},
				{Name: name, Data: []byte(slipMarker)},
			})
			_, err := w.parseDoc(wire.FormatDOCX, doc)
			refusalOf(t, "entry "+name, err, wire.ReasonMalformed)
			v := w.versions(s)[slips[i]]
			if v.State != "failed" || v.Code != string(contracts.ErrValidationFailed) || v.Reason != wire.ReasonMalformed {
				t.Errorf("%s (entry %q): %+v, want failed / validation_failed / malformed", slips[i], name, v)
			}
		}
		// Nothing was written where the names pointed.
		wd, _ := os.Getwd()
		for _, name := range slipNames {
			base := filepath.Base(strings.ReplaceAll(name, `\`, "/"))
			for _, dir := range []string{s.root, s.outside, filepath.Dir(s.root), filepath.Dir(filepath.Dir(s.root)), wd, filepath.Dir(wd), os.TempDir(), string(filepath.Separator)} {
				if _, err := os.Stat(filepath.Join(dir, base)); err == nil {
					t.Errorf("a file %q appeared in %s: an archive entry name was used as a path", base, dir)
				}
			}
		}
	})

	t.Run("what lies outside the root is nowhere in the index, the answers or the prompts", func(t *testing.T) {
		for _, m := range []string{canary, slipMarker} {
			for _, include := range []bool{false, true} {
				for _, h := range w.search(sc, m, include) {
					if strings.Contains(h.Text, m) {
						t.Errorf("the index holds %s (hidden included: %v): %q", m, include, h.Text)
					}
				}
			}
		}
		w.model.setScript(obedient())
		for _, q := range []string{"printing library secret outside", "ordinary slip document", "lending desk opening hours"} {
			a := w.mustAsk(sc, q)
			requireNone(t, "the evidence for "+q, a.returned(t), []string{canary, slipMarker})
		}
		w.requireNoPromptContains(canary, "a file outside the source root")
		w.requireNoPromptContains(slipMarker, "an archive entry that names a place outside")
		if st := w.status(s); st.Counts.Indexed != 3 { // ok.md, links.md, page.html
			t.Errorf("counts = %+v, want only the three ordinary documents indexed\n%s", st.Counts, w.dumpJobs())
		}
	})
}
