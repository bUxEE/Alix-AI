package apppkg

import (
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/trust"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

type ckey struct{ cap, lang string }

type capEntry struct {
	path string
	c    ap.Capability
}

type contractEntry struct {
	path string
	c    ap.ConversationContract
}

type policyEntry struct {
	path string
	p    *policy.HostPolicy
}

type termEntry struct {
	path string
	t    ap.TerminologyTerm
}

type sourceEntry struct {
	path string
	s    ap.RAGSource
}

type suiteEntry struct {
	path string
	t    ap.PackageTests
}

// model is the package as the checks see it: the parsed files by kind.
type model struct {
	snap  *Snapshot
	c     *collector
	lim   Limits
	opts  Options
	fatal error

	m         ap.Manifest
	langs     map[string]bool
	caps      map[string]*capEntry
	contracts map[ckey]*contractEntry
	policies  []*policyEntry
	terms     map[string]*termEntry
	docs      *ap.DocumentationIndex
	sources   map[string]*sourceEntry
	suites    []*suiteEntry
}

// capIDs returns the loaded capability IDs, sorted.
func (md *model) capIDs() []string {
	ids := make([]string, 0, len(md.caps))
	for id := range md.caps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (md *model) sourceIDs() []string {
	ids := make([]string, 0, len(md.sources))
	for id := range md.sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// topDir returns the first path segment and the rest.
func topDir(p string) (dir, rest string) {
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}

func isYAMLFile(p string) bool { return strings.HasSuffix(p, ".yaml") }

// flat reports whether rest is a file directly inside its directory.
func flat(rest string) bool { return !strings.Contains(rest, "/") }

// loadFiles parses every structured file of the package and registers it.
func (md *model) loadFiles() {
	md.langs = map[string]bool{}
	for _, l := range md.m.Languages.Supported {
		md.langs[string(l)] = true
	}
	md.caps = map[string]*capEntry{}
	md.contracts = map[ckey]*contractEntry{}
	md.terms = map[string]*termEntry{}
	md.sources = map[string]*sourceEntry{}

	md.loadCapabilities()
	md.loadSources()

	unknown := map[string]int{}
	rootStray := []string{}
	for _, p := range md.snap.Paths() {
		dir, rest := topDir(p)
		switch dir {
		case "":
			if p != trust.ManifestFile {
				rootStray = append(rootStray, p)
			}
		case "capabilities", "rag-sources", "schemas", "signatures":
			// listed files are loaded above; unlisted ones were reported there
		case "documentation":
			if p == "documentation/index.yaml" {
				md.loadDocIndex(p)
			}
		case "conversation-contracts":
			md.loadFlatYAML(p, rest, md.loadContract)
		case "policies":
			md.loadFlatYAML(p, rest, md.loadPolicy)
		case "terminology":
			md.loadFlatYAML(p, rest, md.loadTerminology)
		case "tests":
			if flat(rest) && isYAMLFile(p) {
				md.loadSuite(p)
			}
		default:
			unknown[dir]++
		}
	}
	for _, p := range rootStray {
		md.c.add(chkUnexpectedFile, "", p, "the file is outside the package layout and is never read")
	}
	dirs := make([]string, 0, len(unknown))
	for d := range unknown {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		md.c.add(chkUnexpectedFile, "", d+"/", "the directory %s is not part of the package layout; its %d file(s) are signed but never read", q(d), unknown[d])
	}
	if md.docs == nil {
		md.c.add(chkDocIndexMissing, "", "documentation/index.yaml", "the package has no documentation index, so no capability is documented")
	}
	if len(md.suites) == 0 {
		md.c.add(chkNoTests, "", "tests", "the package has no tests/*.yaml suite")
	}
}

func (md *model) loadFlatYAML(p, rest string, load func(string)) {
	if !flat(rest) || !isYAMLFile(p) {
		md.c.add(chkUnexpectedFile, "", p, "only %s files directly below this directory are read", ".yaml")
		return
	}
	load(p)
}

func (md *model) read(p string) []byte {
	b, _ := md.snap.bytes(p)
	return b
}

func (md *model) loadCapabilities() {
	listed := map[string]string{}
	ids := make([]string, 0, len(md.m.Capabilities))
	for id := range md.m.Capabilities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		path := md.m.Capabilities[id]
		listed[path] = id
		if !md.snap.Has(path) {
			md.c.add(chkCapFileMissing, id, path, "the manifest lists capability %s at %s, but the file does not exist", q(id), q(path))
			continue
		}
		var c ap.Capability
		if !md.parseFile(path, md.read(path), kindCapability, &c) {
			continue
		}
		if string(c.ID) != id {
			md.c.add(chkCapIDMismatch, id, path, "the file declares id %s but the manifest lists it under %s", q(string(c.ID)), q(id))
			continue
		}
		md.caps[id] = &capEntry{path: path, c: c}
	}
	for _, p := range md.snap.Paths() {
		if strings.HasPrefix(p, "capabilities/") {
			if _, ok := listed[p]; !ok {
				md.c.add(chkCapUnlisted, "", p, "the file is not listed in the manifest's capabilities, so it is not a capability")
			}
		}
	}
}

func (md *model) loadSources() {
	listed := map[string]string{}
	ids := make([]string, 0, len(md.m.RAGSources))
	for id := range md.m.RAGSources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		path := md.m.RAGSources[id]
		listed[path] = id
		if !md.snap.Has(path) {
			md.c.add(chkRAGFileMissing, "", path, "the manifest lists RAG source %s at %s, but the file does not exist", q(id), q(path))
			continue
		}
		var s ap.RAGSource
		if !md.parseFile(path, md.read(path), kindRAGSource, &s) {
			continue
		}
		if string(s.ID) != id {
			md.c.add(chkRAGIDMismatch, "", path, "the file declares id %s but the manifest lists it under %s", q(string(s.ID)), q(id))
			continue
		}
		md.sources[id] = &sourceEntry{path: path, s: s}
	}
	for _, p := range md.snap.Paths() {
		if strings.HasPrefix(p, "rag-sources/") {
			if _, ok := listed[p]; !ok {
				md.c.add(chkRAGUnlisted, "", p, "the file is not listed in the manifest's rag_sources, so the source is undeclared")
			}
		}
	}
}

func (md *model) loadDocIndex(p string) {
	var idx ap.DocumentationIndex
	if md.parseFile(p, md.read(p), kindDocIndex, &idx) {
		md.docs = &idx
	}
}

func (md *model) loadContract(p string) {
	var c ap.ConversationContract
	if !md.parseFile(p, md.read(p), kindContract, &c) {
		return
	}
	k := ckey{string(c.CapabilityID), string(c.Language)}
	if prev, dup := md.contracts[k]; dup {
		md.c.add(chkDuplicate, k.cap, p, "conversation contract for capability %s in language %s is already defined in %s", q(k.cap), q(k.lang), q(prev.path))
		return
	}
	md.contracts[k] = &contractEntry{path: p, c: c}
}

func (md *model) loadPolicy(p string) {
	js, ok := md.toJSON(p, md.read(p), kindPolicy)
	if !ok {
		return
	}
	hp, err := policy.ParseHostPolicy(js)
	if err != nil {
		md.c.add(chkPolicyInvalid, "", p, "the policy runtime parser refuses the document: %s", clipMsg(err.Error()))
		return
	}
	md.policies = append(md.policies, &policyEntry{path: p, p: hp})
}

func (md *model) loadTerminology(p string) {
	var t ap.Terminology
	if !md.parseFile(p, md.read(p), kindTerminology, &t) {
		return
	}
	for _, term := range t.Terms {
		id := string(term.ID)
		if prev, dup := md.terms[id]; dup {
			md.c.add(chkDuplicate, "", p, "term %s is already defined in %s", q(id), q(prev.path))
			continue
		}
		md.terms[id] = &termEntry{path: p, t: term}
	}
}

func (md *model) loadSuite(p string) {
	var t ap.PackageTests
	if !md.parseFile(p, md.read(p), kindTests, &t) {
		return
	}
	for _, prev := range md.suites {
		if prev.t.Suite == t.Suite {
			md.c.add(chkDuplicate, "", p, "test suite %s is already defined in %s", q(string(t.Suite)), q(prev.path))
			return
		}
	}
	md.suites = append(md.suites, &suiteEntry{path: p, t: t})
}
