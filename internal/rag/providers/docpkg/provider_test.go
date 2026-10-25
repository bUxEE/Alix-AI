package docpkg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

var bg = context.Background()

// The provider lists and opens the files of the declared folder of the application's package, and
// no other: not the package's manifest, signatures or declarations, not another folder of
// documentation/, not what the exclude rules, the accepted formats or the include rules keep out.
// Versions are content hashes, keys are the paths below the folder, and a read changes nothing.
func TestDocPackageSourceListsAndOpensOnlyThePackagesFiles(t *testing.T) {
	h := newHarness(t)
	pkg := h.activateV1()
	def := defOf(t, hashV1, decl(app))
	sc := scopeOf(app)
	before := tree(t, h.w.base)

	if got := h.p.Kinds(); len(got) != 1 || got[0] != ap.RAGSourceTypeDocumentationPackage {
		t.Fatalf("Kinds = %v, want only documentation_package", got)
	}
	health, err := h.p.Probe(bg, sc, def)
	if err != nil || !health.Reachable() {
		t.Fatalf("Probe = %+v, %v; want the package reachable", health, err)
	}

	cs, err := h.p.Discover(bg, sc, def, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(cs.Added); !equalStrings(got, policyKeys) {
		t.Fatalf("Added = %v, want exactly the policies %v", got, policyKeys)
	}
	if len(cs.Modified)+len(cs.Deleted)+len(cs.Failed) != 0 {
		t.Fatalf("a first scan reported %d modified, %d deleted, %d failed", len(cs.Modified), len(cs.Deleted), len(cs.Failed))
	}
	if cs.Counts.Discovered != 3 || len(cs.Next) != 3 {
		t.Errorf("counts = %+v, next = %d records; want 3 items", cs.Counts, len(cs.Next))
	}
	for _, it := range cs.Added {
		content := packageV1["documentation/policies/"+it.Key]
		if want := hashOf(content); it.Version.Hash != want {
			t.Errorf("%s: version %s, want the content hash %s", it.Key, it.Version.Hash, want)
		}
		if it.Version.Size != int64(len(content)) || it.Version.MIME != source.MIMEMarkdown {
			t.Errorf("%s: version %+v", it.Key, it.Version)
		}
	}
	for _, rec := range cs.Next {
		if strings.Contains(rec.Key, secret) || strings.Contains(rec.Key, "..") || filepath.IsAbs(rec.Key) {
			t.Errorf("a record key that is not a path below the folder: %q", rec.Key)
		}
	}

	for _, key := range policyKeys {
		want := packageV1["documentation/policies/"+key]
		info, err := h.p.Stat(bg, sc, def, key)
		if err != nil || info.Key != key || info.Size != int64(len(want)) {
			t.Errorf("Stat(%q) = %+v, %v", key, info, err)
		}
		v, err := h.p.FetchVersion(bg, sc, def, key)
		if err != nil || v.Hash != hashOf(want) {
			t.Errorf("FetchVersion(%q) = %+v, %v", key, v, err)
		}
		c, err := h.p.Open(bg, sc, def, key)
		if err != nil {
			t.Fatalf("Open(%q): %v", key, err)
		}
		if c.MIME != source.MIMEMarkdown || c.Info.Key != key {
			t.Errorf("Open(%q) = %+v", key, c.Info)
		}
		if got := readAll(t, c); got != want {
			t.Errorf("Open(%q) delivered %q, want %q", key, got, want)
		}
	}

	// What the rules keep out is not there to open, and says why: an excluded draft, a file that is
	// not markdown, a file of the package outside the folder, and one that does not exist.
	for key, want := range map[string]error{
		"drafts/wip.md":                          source.ErrNotAdmitted,
		"notes.txt":                              source.ErrNotAdmitted,
		"sub/ignored.pdf":                        source.ErrNotAdmitted,
		"sub":                                    source.ErrNotAdmitted,
		"documentation/policies/booking.md":      source.ErrNotFound,
		"README.md":                              source.ErrNotFound,
		"documentation/other/secret.md":          source.ErrNotFound,
		"other/secret.md":                        source.ErrNotFound,
		"missing.md":                             source.ErrNotFound,
		"signatures/signature-manifest.md":       source.ErrNotFound,
		"rag-sources/clinic-policies.md":         source.ErrNotFound,
		"sub/../../other/secret.md":              source.ErrEscapesRoot,
		"../other/secret.md":                     source.ErrEscapesRoot,
		"../../signatures/signature-manifest.md": source.ErrEscapesRoot,
	} {
		_, err := h.p.Open(bg, sc, def, key)
		if !errors.Is(err, want) {
			t.Errorf("Open(%q) = %v, want %v", key, err, want)
		}
		h.w.noHostPath(t, "Open "+key, err)
	}

	// A scan that finds the files as they were reports nothing new, and nothing was created,
	// changed or removed in the data directory by any of the reads: the provider is read-only.
	again, err := h.p.Discover(bg, sc, def, cs.Next)
	if err != nil || len(again.Added)+len(again.Modified)+len(again.Deleted) != 0 || again.Counts.Unchanged != 3 {
		t.Fatalf("the second scan = %+v, %v; want 3 unchanged", again.Counts, err)
	}
	after := tree(t, h.w.base)
	for p, was := range before {
		if now, ok := after[p]; !ok {
			t.Errorf("%s was removed", p)
		} else if now != was {
			t.Errorf("%s was changed", p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("%s was created", p)
		}
	}

	// A file that left the package is a tombstone for exactly that key.
	if err := os.Remove(filepath.Join(pkg.Dir, "documentation", "policies", "sub", "deep.md")); err != nil {
		t.Fatal(err)
	}
	gone, err := h.p.DetectDeletions(bg, sc, def, cs.Next)
	if err != nil {
		t.Fatal(err)
	}
	if got := tombstoneKeys(gone.Deleted); !equalStrings(got, []string{"sub/deep.md"}) || gone.Deleted[0].Reason != source.ReasonDeleted {
		t.Fatalf("DetectDeletions = %+v, want a tombstone for sub/deep.md only", gone.Deleted)
	}
}

// The folder a source reads is the declaration's, and it is hostile input: it must lie below
// documentation/, and a key is a path below it. Neither a folder nor a key may name the rest of the
// package, another application's files or anything outside the data directory.
//
// Mutation-style (security-relevant): the folder rule is the only thing between a declaration and the
// manifest, the signatures and every other file of the package. The test removes it (the unexported
// folderRule seam) and shows that each hostile folder that is a plain relative name then serves the
// secret, so the refusal below is the rule's doing; the folders that are not plain names are still
// refused by the filesystem provider beneath it.
func TestDocPackageSourceRefusesPathsOutsideThePackage(t *testing.T) {
	sc := scopeOf(app)

	t.Run("a key outside the folder", func(t *testing.T) {
		h := newHarness(t)
		h.activateV1()
		def := defOf(t, hashV1, decl(app))
		keys := []string{
			"", ".", "..", "../", "../booking.md", "../other/secret.md", "../../signatures/signature-manifest.md",
			"sub/../../other/secret.md", "sub/../booking.md", "./booking.md", "booking.md/", "booking.md/..",
			"/booking.md", "/etc/passwd", "//server/share/booking.md", `\booking.md`, `..\other\secret.md`, `sub\deep.md`,
			"C:/booking.md", `C:\booking.md`, "C:booking.md", "booking.md:stream", "booking.md\x00.txt", "bo\noking.md",
			"%2e%2e/other/secret.md", "..%2fother%2fsecret.md", strings.Repeat("a/", 3000) + "x.md",
		}
		for _, key := range keys {
			name := strings.NewReplacer("\x00", "NUL", "\n", "LF").Replace(key)
			if len(name) > 40 {
				name = name[:40]
			}
			t.Run(name, func(t *testing.T) {
				calls := []struct {
					what string
					do   func() (string, error)
				}{
					{"Stat", func() (string, error) { i, err := h.p.Stat(bg, sc, def, key); return i.Key, err }},
					{"FetchVersion", func() (string, error) { v, err := h.p.FetchVersion(bg, sc, def, key); return v.Hash, err }},
					{"Open", func() (string, error) {
						c, err := h.p.Open(bg, sc, def, key)
						if err != nil {
							return "", err
						}
						return readAll(t, c), nil
					}},
				}
				for _, c := range calls {
					got, err := c.do()
					if err == nil {
						t.Errorf("%s(%q) succeeded with %q", c.what, key, got)
						continue
					}
					if strings.Contains(got, secret) {
						t.Errorf("%s(%q) delivered the secret", c.what, key)
					}
					if code(err) == contracts.ErrInternal || code(err) == contracts.ErrUnavailable {
						t.Errorf("%s(%q) = %v: a hostile name is not an outage", c.what, key, err)
					}
					h.w.noHostPath(t, c.what+" "+key, err)
				}
			})
		}
	})

	// A symbolic link inside the folder that leads out of it is never followed, even where the link
	// is allowed to exist (a package the installer wrote has none; a tampered data directory might).
	t.Run("a link inside the folder", func(t *testing.T) {
		h := newHarness(t)
		pkg := h.activateV1()
		def := defOf(t, hashV1, decl(app))
		folder := filepath.Join(pkg.Dir, "documentation", "policies")
		if err := os.Symlink(filepath.Join(h.w.outside, "secret.md"), filepath.Join(folder, "leak.md")); err != nil {
			t.Skipf("cannot create a symbolic link here: %v", err)
		}
		if err := os.Symlink(h.w.outside, filepath.Join(folder, "leakdir")); err != nil {
			t.Skipf("cannot create a symbolic link here: %v", err)
		}
		cs, err := h.p.Discover(bg, sc, def, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := keysOf(cs.Added); !equalStrings(got, policyKeys) {
			t.Errorf("Added = %v; a link must add nothing to %v", got, policyKeys)
		}
		for _, key := range []string{"leak.md", "leakdir/secret.md"} {
			c, err := h.p.Open(bg, sc, def, key)
			if err == nil {
				t.Errorf("Open(%q) followed a link: %q", key, readAll(t, c))
				continue
			}
			if !errors.Is(err, source.ErrEscapesRoot) {
				t.Errorf("Open(%q) = %v, want a refusal as an escape", key, err)
			}
			h.w.noHostPath(t, "Open "+key, err)
		}
	})

	// The declared folder.
	plain := []string{ // plain relative names outside documentation/<folder>: only the rule refuses them
		"signatures", "rag-sources", "documentation", "documentation/", "Documentation/policies", "alixai.yaml",
		"policies", "docs/policies", "", ".", "documentation-policies/x",
	}
	hostile := []string{ // refused by the rule and, being no plain names, by the layer beneath it
		"..", "../documentation/policies", "documentation/..", "documentation/../signatures", "documentation/../../outside",
		"documentation/policies/../../signatures", "documentation//policies", "documentation/./policies",
		"/documentation/policies", "/etc", `documentation\policies`, `\documentation\policies`, "C:/documentation/policies",
		`C:\documentation\policies`, "//server/share/documentation/policies", "documentation/policies\x00",
	}
	// leaky are the plain names that, with the rule removed, are places of the package the source
	// would serve from: the signatures, the declarations and the whole of documentation/.
	leaky := map[string]bool{"signatures": true, "rag-sources": true, "documentation": true, "documentation/": true}
	for _, folder := range append(append([]string{}, plain...), hostile...) {
		name := strings.NewReplacer("\x00", "NUL", "/", "_", `\`, "_").Replace(folder)
		t.Run("folder "+name, func(t *testing.T) {
			h := newHarness(t)
			h.activateV1()
			def := defOf(t, hashV1, decl(app, rootAt(folder)))

			// served reports whether the provider served anything from the folder, by any call, and
			// whether what it served is the secret.
			served := func(p *Provider) (what string, leaked bool) {
				if cs, err := p.Discover(bg, sc, def, nil); err == nil && len(cs.Added) > 0 {
					what = "Discover served " + strings.Join(keysOf(cs.Added), ", ")
					for _, it := range cs.Added {
						if c, err := p.Open(bg, sc, def, it.Key); err == nil && strings.Contains(readAll(t, c), secret) {
							leaked = true
						}
					}
					return what, leaked
				}
				if health, err := p.Probe(bg, sc, def); err == nil && health.Reachable() {
					return "Probe reached the folder", false
				}
				for _, key := range []string{"booking.md", "signature-manifest.md", "secret.md", "policies/booking.md", "other/secret.md"} {
					if c, err := p.Open(bg, sc, def, key); err == nil {
						text := readAll(t, c)
						return "Open(" + key + ") served " + text, strings.Contains(text, secret)
					}
				}
				return "", false
			}

			// The rule, in place: refused as an escape (not as an outage a retry would hide), and
			// before the package is looked up at all.
			if what, _ := served(h.p); what != "" {
				t.Errorf("the folder %q is served: %s", folder, what)
			}
			if _, err := h.p.Discover(bg, sc, def, nil); !errors.Is(err, source.ErrEscapesRoot) {
				t.Errorf("Discover(%q) = %v, want a refusal as an escape", folder, err)
			}
			if _, err := h.p.Probe(bg, sc, def); !errors.Is(err, source.ErrEscapesRoot) {
				t.Errorf("Probe(%q) = %v, want a refusal as an escape, not a health", folder, err)
			}
			if n := h.res.called(); n != 0 {
				t.Errorf("the package was looked up %d times for the folder %q", n, folder)
			}

			// The mutation: the rule removed.
			mutant := New(Config{Packages: h.res})
			mutant.folderRule = func(declared string) (string, error) { return strings.TrimSuffix(declared, "/"), nil }
			what, leaked := served(mutant)
			switch {
			case leaky[folder]:
				if !leaked {
					t.Fatalf("without the rule the folder %q serves no secret (%q): the test would not notice the rule missing", folder, what)
				}
			case !slices.Contains(plain, folder):
				// Not a plain name: the filesystem provider beneath the rule refuses it on its own.
				if what != "" {
					t.Errorf("without the rule, the folder %q is served by the layer beneath: %s", folder, what)
				}
			}
		})
	}
}

// A replaced package changes the version: the provider follows the application's active package, and
// a registration that names the old one is not served from the new one.
func TestDocPackageSourceFollowsAPackageReplacement(t *testing.T) {
	h := newHarness(t)
	h.activateV1()
	sc := scopeOf(app)
	def1 := defOf(t, hashV1, decl(app))
	first, err := h.p.Discover(bg, sc, def1, nil)
	if err != nil || len(first.Added) != 3 {
		t.Fatalf("the first scan: %+v, %v", first.Counts, err)
	}
	oldVersion := map[string]string{}
	for _, it := range first.Added {
		oldVersion[it.Key] = it.Version.Hash
	}

	// The package is replaced: the new content is the active one.
	h.activateV2()

	// The registration built from the old package is not served from the new one, by any call, and
	// nothing it says is a deletion or the new text.
	t.Run("a registration of the replaced package", func(t *testing.T) {
		health, err := h.p.Probe(bg, sc, def1)
		if err != nil || health.Availability != source.Unavailable || health.Err == nil || code(health.Err) != contracts.ErrUnavailable || health.Reachable() {
			t.Errorf("Probe = %+v, %v; want unavailable", health, err)
		}
		cs, err := h.p.Discover(bg, sc, def1, first.Next)
		if code(err) != contracts.ErrUnavailable || len(cs.Deleted)+len(cs.Added)+len(cs.Modified) != 0 || len(cs.Next) != 0 {
			t.Errorf("Discover = %+v, %v; want unavailable and no change at all", cs.Counts, err)
		}
		cs, err = h.p.DetectDeletions(bg, sc, def1, first.Next)
		if code(err) != contracts.ErrUnavailable || len(cs.Deleted) != 0 {
			t.Errorf("DetectDeletions = %+v, %v; want unavailable and no tombstone", cs.Deleted, err)
		}
		if _, err := h.p.Stat(bg, sc, def1, "booking.md"); code(err) != contracts.ErrUnavailable {
			t.Errorf("Stat = %v, want unavailable", err)
		}
		if _, err := h.p.FetchVersion(bg, sc, def1, "booking.md"); code(err) != contracts.ErrUnavailable {
			t.Errorf("FetchVersion = %v, want unavailable", err)
		}
		if c, err := h.p.Open(bg, sc, def1, "booking.md"); err == nil {
			t.Errorf("Open served %q to a registration of the replaced package", readAll(t, c))
		} else if code(err) != contracts.ErrUnavailable || errors.Is(err, source.ErrNotFound) {
			t.Errorf("Open = %v, want unavailable, never gone", err)
		}
	})

	// The registration rebuilt from the new package sees the change against what the old scan found.
	t.Run("a registration of the new package", func(t *testing.T) {
		def2 := defOf(t, hashV2, decl(app))
		cs, err := h.p.Discover(bg, sc, def2, first.Next)
		if err != nil {
			t.Fatal(err)
		}
		if got := keysOf(cs.Added); !equalStrings(got, []string{"renewals.md"}) {
			t.Errorf("Added = %v, want renewals.md", got)
		}
		if got := keysOf(cs.Modified); !equalStrings(got, []string{"booking.md"}) {
			t.Fatalf("Modified = %v, want booking.md", got)
		}
		newText := packageV2["documentation/policies/booking.md"]
		if v := cs.Modified[0].Version; v.Hash != hashOf(newText) || v.Hash == oldVersion["booking.md"] {
			t.Errorf("booking.md version = %s, want the hash of the new text and not %s", v.Hash, oldVersion["booking.md"])
		}
		if got := tombstoneKeys(cs.Deleted); !equalStrings(got, []string{"sub/deep.md"}) || cs.Deleted[0].Reason != source.ReasonDeleted {
			t.Errorf("Deleted = %+v, want a tombstone for sub/deep.md", cs.Deleted)
		}
		if cs.Counts.Unchanged != 1 || len(cs.Next) != 3 {
			t.Errorf("counts = %+v, next = %d; want booking.it.md unchanged and 3 live records", cs.Counts, len(cs.Next))
		}

		c, err := h.p.Open(bg, sc, def2, "booking.md")
		if err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, c); got != newText {
			t.Errorf("Open served %q, want the new package's text", got)
		}
		if _, err := h.p.Open(bg, sc, def2, "sub/deep.md"); !errors.Is(err, source.ErrNotFound) {
			t.Errorf("Open of a file the new package no longer holds = %v, want gone", err)
		}
		if _, err := h.p.Stat(bg, sc, def2, "README.md"); !errors.Is(err, source.ErrNotFound) {
			t.Errorf("Stat of a file of the old package outside the folder = %v, want not found", err)
		}
	})

	// The audit builds a definition from the declaration alone, with no package version: it is
	// served from the package the resolver names, which is the one under audit.
	t.Run("a definition with no version", func(t *testing.T) {
		def := defOf(t, "", decl(app))
		cs, err := h.p.Discover(bg, sc, def, nil)
		if err != nil || len(cs.Added) != 3 {
			t.Fatalf("Discover = %+v, %v", cs.Counts, err)
		}
	})
}

// An unreachable package is an outage and never a deletion: with no active package, a resolver that
// fails, an install directory that is gone or a folder the package no longer holds, the provider
// answers unavailable, tombstones nothing and says nothing was checked.
func TestDocPackageSourceUnreachablePackageIsNotADeletion(t *testing.T) {
	sc := scopeOf(app)
	setup := func(t *testing.T) (*harness, source.Definition, source.ChangeSet) {
		h := newHarness(t)
		h.activateV1()
		def := defOf(t, hashV1, decl(app))
		cs, err := h.p.Discover(bg, sc, def, nil)
		if err != nil || len(cs.Next) != 3 {
			t.Fatalf("the first scan: %+v, %v", cs.Counts, err)
		}
		return h, def, cs
	}
	for name, break_ := range map[string]func(h *harness){
		"no active package": func(h *harness) {
			h.res.fail(contracts.New(contracts.ErrQuarantined, "the application has no active package"))
		},
		"a resolver that fails": func(h *harness) {
			h.res.fail(errors.New("open " + h.w.base + ": the store is busy"))
		},
		"an install directory that is gone": func(h *harness) {
			if err := os.RemoveAll(h.w.dir(app, hashV1)); err != nil {
				h.t.Fatal(err)
			}
		},
		"a folder the package no longer holds": func(h *harness) {
			if err := os.RemoveAll(filepath.Join(h.w.dir(app, hashV1), "documentation", "policies")); err != nil {
				h.t.Fatal(err)
			}
		},
		"a relative package directory": func(h *harness) {
			h.res.activate(Package{ApplicationID: app, ManifestHash: hashV1, Dir: "packages/clinic-booking/" + hashV1})
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, def, first := setup(t)
			break_(h)
			if health, err := h.p.Probe(bg, sc, def); err != nil || health.Reachable() || health.Availability != source.Unavailable {
				t.Errorf("Probe = %+v, %v; want unavailable", health, err)
			} else if health.Err != nil {
				h.w.noHostPath(t, name+" (health)", health.Err)
			}
			cs, err := h.p.Discover(bg, sc, def, first.Next)
			if code(err) != contracts.ErrUnavailable || len(cs.Deleted) != 0 {
				t.Errorf("Discover = %+v, %v; want unavailable and no tombstone", cs.Counts, err)
			}
			cs, err = h.p.DetectDeletions(bg, sc, def, first.Next)
			if code(err) != contracts.ErrUnavailable || len(cs.Deleted) != 0 {
				t.Errorf("DetectDeletions = %+v, %v; want unavailable and no tombstone", cs.Deleted, err)
			}
			for _, call := range []func() error{
				func() error { _, err := h.p.Stat(bg, sc, def, "booking.md"); return err },
				func() error { _, err := h.p.FetchVersion(bg, sc, def, "booking.md"); return err },
				func() error { _, err := h.p.Open(bg, sc, def, "booking.md"); return err },
			} {
				err := call()
				if code(err) != contracts.ErrUnavailable || errors.Is(err, source.ErrNotFound) {
					t.Errorf("an item of an unreachable package: %v, want unavailable and never gone", err)
				}
				h.w.noHostPath(t, name, err)
			}
		})
	}

	t.Run("no resolver", func(t *testing.T) {
		p := New(Config{})
		def := defOf(t, hashV1, decl(app))
		if health, err := p.Probe(bg, sc, def); err != nil || health.Reachable() {
			t.Errorf("Probe = %+v, %v", health, err)
		}
		if _, err := p.Discover(bg, sc, def, nil); code(err) != contracts.ErrUnavailable {
			t.Errorf("Discover = %v, want unavailable", err)
		}
	})
}

// The source belongs to one application, and one tenant and workspace where its binding says so: the
// scope is checked before the package is looked up, another application's package is never read,
// and two applications' packages stay apart.
func TestDocPackageSourceIsScopedToTheApplication(t *testing.T) {
	h := newHarness(t)
	h.activateV1()
	def := defOf(t, hashV1, decl(app))

	const other = "library-loans"
	otherFiles := map[string]string{"documentation/policies/loans.md": "# Loans\n\nA loan lasts three weeks.\n"}
	otherPkg := Package{ApplicationID: other, ManifestHash: hashV2, Dir: h.w.install(other, hashV2, otherFiles, installed2)}
	h.res.activate(otherPkg)

	t.Run("the scope of another application", func(t *testing.T) {
		before := h.res.called()
		if _, err := h.p.Discover(bg, scopeOf(other), def, nil); code(err) != contracts.ErrScopeViolation {
			t.Errorf("Discover for another application = %v, want scope_violation", err)
		}
		if _, err := h.p.Open(bg, scopeOf(other), def, "booking.md"); code(err) != contracts.ErrScopeViolation {
			t.Errorf("Open for another application = %v, want scope_violation", err)
		}
		if health, err := h.p.Probe(bg, scopeOf(other), def); err == nil && health.Reachable() {
			t.Error("Probe reached the source for another application")
		}
		if h.res.called() != before {
			t.Error("the package was looked up for a scope the source does not admit")
		}
	})

	t.Run("a tenant the registration does not serve", func(t *testing.T) {
		bound, err := source.FromDeclaration(decl(app, func(d *ap.RAGSource) {
			d.Scope.Tenant = ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeRequest}
			d.Scope.Workspace = ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeRequest}
		}), source.Binding{SourceID: def.SourceID, TenantID: "tenant-1", WorkspaceID: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		bound.ManifestHash = hashV1
		foreign := scopeOf(app)
		foreign.TenantID = "tenant-2"
		before := h.res.called()
		if _, err := h.p.Discover(bg, foreign, bound, nil); code(err) != contracts.ErrScopeViolation {
			t.Errorf("Discover for another tenant = %v, want scope_violation", err)
		}
		foreign = scopeOf(app)
		foreign.WorkspaceID = "ws-2"
		if _, err := h.p.Stat(bg, foreign, bound, "booking.md"); code(err) != contracts.ErrScopeViolation {
			t.Errorf("Stat for another workspace = %v, want scope_violation", err)
		}
		if h.res.called() != before {
			t.Error("the package was looked up for a tenant the source does not serve")
		}
		h.activateV1()
		if cs, err := h.p.Discover(bg, scopeOf(app), bound, nil); err != nil || len(cs.Added) != 3 {
			t.Errorf("Discover for the registered tenant = %+v, %v", cs.Counts, err)
		}
	})

	t.Run("a resolver that answers with another application's package", func(t *testing.T) {
		h.res.mu.Lock()
		h.res.pkgs[app] = Package{ApplicationID: other, ManifestHash: hashV1, Dir: otherPkg.Dir}
		h.res.mu.Unlock()
		if _, err := h.p.Discover(bg, scopeOf(app), def, nil); code(err) != contracts.ErrScopeViolation {
			t.Errorf("Discover = %v, want scope_violation: another application's files must not be read", err)
		}
		if c, err := h.p.Open(bg, scopeOf(app), def, "loans.md"); err == nil {
			t.Errorf("Open served %q from another application's package", readAll(t, c))
		}
	})

	t.Run("two applications side by side", func(t *testing.T) {
		h.activateV1()
		h.res.activate(otherPkg)
		otherDef := defOf(t, hashV2, decl(other))
		cs, err := h.p.Discover(bg, scopeOf(other), otherDef, nil)
		if err != nil || !equalStrings(keysOf(cs.Added), []string{"loans.md"}) {
			t.Fatalf("the other application's scan = %+v, %v", cs.Counts, err)
		}
		if _, err := h.p.Open(bg, scopeOf(other), otherDef, "booking.md"); !errors.Is(err, source.ErrNotFound) {
			t.Errorf("a clinic file opened through the library's source = %v, want not found", err)
		}
		if _, err := h.p.Open(bg, scopeOf(app), defOf(t, hashV1, decl(app)), "loans.md"); !errors.Is(err, source.ErrNotFound) {
			t.Errorf("a library file opened through the clinic's source = %v, want not found", err)
		}
	})

	t.Run("a source of another kind", func(t *testing.T) {
		before := h.res.called()
		folder, err := source.FromDeclaration(decl(app, func(d *ap.RAGSource) {
			d.Type = ap.RAGSourceTypeLocalFolder
			d.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootFolder{Kind: "folder", Path: h.w.base}}
		}), source.Binding{SourceID: def.SourceID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.p.Discover(bg, scopeOf(app), folder, nil); code(err) != contracts.ErrValidationFailed {
			t.Errorf("Discover of a folder source = %v, want validation_failed", err)
		}
		if _, err := h.p.Probe(bg, scopeOf(app), source.Definition{}); err == nil {
			t.Error("Probe of an empty definition succeeded")
		}
		if h.res.called() != before {
			t.Error("the package was looked up for a source this provider does not serve")
		}
	})
}

// The source is bounded by its declaration: an item over max_file_bytes is not served, a source over
// max_files is a resource error that applies nothing, and a cancelled call reads nothing.
func TestDocPackageSourceIsBounded(t *testing.T) {
	h := newHarness(t)
	h.activateV1()
	sc := scopeOf(app)

	limit := int64(len(packageV1["documentation/policies/booking.md"])) // the smallest of the three
	small := defOf(t, hashV1, decl(app, func(d *ap.RAGSource) { d.Limits.MaxFileBytes = limit }))
	cs, err := h.p.Discover(bg, sc, small, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := keysOf(cs.Added); !equalStrings(got, []string{"booking.md"}) || cs.Counts.Skipped != 2 {
		t.Errorf("a %d-byte limit served %v and skipped %d, want booking.md alone and 2 skipped", limit, got, cs.Counts.Skipped)
	}
	if _, err := h.p.Open(bg, sc, small, "booking.it.md"); !errors.Is(err, source.ErrNotAdmitted) {
		t.Errorf("Open of an item over the limit = %v, want not admitted", err)
	}

	few := defOf(t, hashV1, decl(app, func(d *ap.RAGSource) { d.Limits.MaxFiles = 2 }))
	cs, err = h.p.Discover(bg, sc, few, nil)
	if code(err) != contracts.ErrResourceExhausted || len(cs.Added)+len(cs.Next) != 0 {
		t.Errorf("a source over max_files = %+v, %v; want resource_exhausted and nothing to apply", cs.Counts, err)
	}

	ctx, cancel := context.WithCancel(bg)
	cancel()
	before := h.res.called()
	if _, err := h.p.Discover(ctx, sc, defOf(t, hashV1, decl(app)), nil); code(err) != contracts.ErrTimeout {
		t.Errorf("Discover on a cancelled context = %v, want timeout", err)
	}
	if h.res.called() != before {
		t.Error("a cancelled call looked the package up")
	}
}

// An error of the provider carries no host path, though the filesystem provider beneath it wraps an
// outage with the operating system's own words: those are dropped, the code, the message and the
// details stay, and what a caller matches on (gone, not admitted, an escape) is untouched.
func TestDocPackageSourceErrorsCarryNoHostPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "packages", "clinic-booking")
	osErr := &os.PathError{Op: "GetFileAttributesEx", Path: dir, Err: errors.New("the system cannot find the file specified")}
	outage := contracts.Wrap(contracts.ErrUnavailable, osErr, "the root cannot be reached").WithDetails("why", "gone")

	got := tidy(outage)
	if strings.Contains(got.Error(), dir) || code(got) != contracts.ErrUnavailable {
		t.Errorf("tidy(outage) = %v", got)
	}
	var ce *contracts.Error
	if !errors.As(got, &ce) || ce.Message != "the root cannot be reached" || ce.Details["why"] != "gone" {
		t.Errorf("tidy dropped the message or the details: %+v", ce)
	}

	cs, err := tidyChanges(source.ChangeSet{Failed: []source.Failure{{Key: "a.md", Err: outage}}}, outage)
	if strings.Contains(err.Error(), dir) || strings.Contains(cs.Failed[0].Err.Error(), dir) {
		t.Errorf("a scan's errors carry the host path: %v / %v", err, cs.Failed[0].Err)
	}

	for _, keep := range []error{
		nil, source.ErrNotAdmitted, source.ErrEscapesRoot, contracts.New(contracts.ErrTimeout, "interrupted"),
		fmt.Errorf("%w: %q", source.ErrNotFound, "a.md"), contracts.New(contracts.ErrUnavailable, "no cause"),
	} {
		if tidy(keep) != keep {
			t.Errorf("tidy changed %v", keep)
		}
	}
	if !errors.Is(tidy(fmt.Errorf("%w: %q", source.ErrNotFound, "a.md")), source.ErrNotFound) {
		t.Error("an item that is gone no longer reads as gone")
	}
}
