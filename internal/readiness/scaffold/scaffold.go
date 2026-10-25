package scaffold

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/readiness/catalog"
	"github.com/buxee/alix-ai/internal/readiness/scan"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

// Limits of one scaffold.
const (
	scaffoldMaxFileBytes = 4 << 20
	scaffoldMaxCaps      = 64
	scaffoldMaxRAG       = 16
	scaffoldMaxRefs      = 4
	scaffoldMinNameLen   = 3
)

// ScaffoldInput is everything a scaffold is made from.
type ScaffoldInput struct {
	// Report is the stored readiness report (already through the verifier). Its findings are re-checked here.
	Report readiness.Report
	// Inventory is the scan inventory of the same scan: names only, never a source of fields by itself.
	Inventory scan.ScanInventory
	// Source is the scanned directory, read-only. It is how cited evidence is re-read and re-hashed. Required.
	Source fs.FS
	// ScannedRoot is the scanned directory on disk. When set, OUTDIR may not be inside it.
	ScannedRoot string
	// ApplicationID is the application id the operator named (-app), or empty. It is written as application.id.
	ApplicationID string
}

// ScaffoldResult says what was written.
type ScaffoldResult struct {
	// OutDir is the directory that was created.
	OutDir string
	// Files are the files written, relative to OutDir, in order.
	Files []string
	// Capabilities are the capability ids that have a stub.
	Capabilities []string
	// RAGSources are the RAG source ids that have a stub.
	RAGSources []string
	// Candidates are verified names that could not become a capability id; they are listed in the TODO file only.
	Candidates []string
	// FindingsUsed and FindingsDropped count the report findings used and not used.
	FindingsUsed, FindingsDropped int
}

// scaffoldFact is one confirmed evidence entry of a used finding.
type scaffoldFact struct {
	Path        string
	Start, End  int64
	SHA         string
	Requirement readiness.RequirementID
	content     []byte
}

func (f scaffoldFact) ref() string {
	return fmt.Sprintf("%s lines %d-%d sha256 %s", f.Path, f.Start, f.End, f.SHA)
}

// scaffoldVerified is what survived the re-check.
type scaffoldVerified struct {
	facts []scaffoldFact
	reqs  map[readiness.RequirementID]bool
	found map[readiness.RequirementID]bool // the requirements a re-checked finding says are found
	used  int
	drop  int
}

// scaffoldEligible is the first filter: status, source and the presence of evidence. Status and source are checked
// separately: the schema forbids model_unverified together with found, and this does not rely on that.
func scaffoldEligible(f readiness.Finding) bool {
	if f.Status != readiness.StatusFound && f.Status != readiness.StatusPartial {
		return false
	}
	if f.Source != readiness.SourceDeterministic && f.Source != readiness.SourceModelVerified {
		return false
	}
	return len(f.Evidence) > 0
}

// scaffoldSafePath accepts the relative slash paths of the schema and nothing else.
func scaffoldSafePath(p string) bool {
	if p == "" || len(p) > 512 || strings.ContainsAny(p, "\\:\x00") || !fs.ValidPath(p) || p == "." {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

type scaffoldFile struct {
	content []byte
	sha     string
	lines   int64
	ok      bool
}

func scaffoldLoad(src fs.FS, cache map[string]scaffoldFile, p string) scaffoldFile {
	if c, ok := cache[p]; ok {
		return c
	}
	var out scaffoldFile
	if scaffoldSafePath(p) {
		if fi, err := fs.Stat(src, p); err == nil && fi.Mode().IsRegular() && fi.Size() <= scaffoldMaxFileBytes {
			if b, err := fs.ReadFile(src, p); err == nil && int64(len(b)) <= scaffoldMaxFileBytes {
				sum := sha256.Sum256(b)
				n := int64(bytes.Count(b, []byte{'\n'}))
				if len(b) > 0 && b[len(b)-1] != '\n' {
					n++
				}
				out = scaffoldFile{content: b, sha: hex.EncodeToString(sum[:]), lines: n, ok: true}
			}
		}
	}
	cache[p] = out
	return out
}

// scaffoldVerify keeps the findings that are eligible and whose every cited span still holds on disk.
func scaffoldVerify(in ScaffoldInput) (*scaffoldVerified, error) {
	if in.Source == nil {
		return nil, errors.New("readiness scaffold: the scanned files are required to re-check the evidence")
	}
	if in.Report.SchemaVersion != 1 {
		return nil, errors.New("readiness scaffold: the report is not a version 1 report")
	}
	v := &scaffoldVerified{reqs: map[readiness.RequirementID]bool{}, found: map[readiness.RequirementID]bool{}}
	cache := map[string]scaffoldFile{}
	seen := map[string]bool{}
	for _, f := range in.Report.Findings {
		if !scaffoldEligible(f) {
			v.drop++
			continue
		}
		var facts []scaffoldFact
		ok := true
		for _, e := range f.Evidence {
			file := scaffoldLoad(in.Source, cache, string(e.Path))
			if !file.ok || file.sha != string(e.FileSHA256) || e.LineStart < 1 || e.LineEnd < e.LineStart || e.LineEnd > file.lines {
				ok = false
				break
			}
			facts = append(facts, scaffoldFact{Path: string(e.Path), Start: e.LineStart, End: e.LineEnd, SHA: file.sha, Requirement: f.RequirementID, content: file.content})
		}
		if !ok {
			v.drop++
			continue
		}
		v.used++
		v.reqs[f.RequirementID] = true
		if f.Status == readiness.StatusFound {
			v.found[f.RequirementID] = true
		}
		for _, fa := range facts {
			k := fmt.Sprintf("%s|%d|%d|%s", fa.Path, fa.Start, fa.End, fa.Requirement)
			if !seen[k] {
				seen[k] = true
				v.facts = append(v.facts, fa)
			}
		}
	}
	sort.SliceStable(v.facts, func(i, j int) bool {
		a, b := v.facts[i], v.facts[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Start < b.Start
	})
	return v, nil
}

// refsFor returns the confirmed spans of the files that contain name, at most scaffoldMaxRefs, one per file.
func (v *scaffoldVerified) refsFor(name string) []scaffoldFact {
	if len(name) < scaffoldMinNameLen {
		return nil
	}
	var out []scaffoldFact
	seen := map[string]bool{}
	for _, f := range v.facts {
		if seen[f.Path] || !bytes.Contains(f.content, []byte(name)) {
			continue
		}
		seen[f.Path] = true
		out = append(out, f)
		if len(out) == scaffoldMaxRefs {
			break
		}
	}
	return out
}

var (
	scaffoldCapIDRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}(\.[a-z][a-z0-9_]{0,31}){0,3}$`)
	scaffoldSlugRE  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	scaffoldAppIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	scaffoldNameRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
)

// scaffoldCapability is one capability stub.
type scaffoldCapability struct {
	ID   string
	File string // capabilities/cap-001.yaml
	Doc  string // documentation/cap-001.md
	Refs []scaffoldFact
}

// scaffoldRAG is one RAG source stub.
type scaffoldRAG struct {
	ID   string
	File string
	Dir  string
	Refs []scaffoldFact
}

// scaffoldPlan is everything the files are rendered from.
type scaffoldPlan struct {
	appID       string
	appIDGiven  bool
	transport   string // mcp, openapi or native
	bridge      string // native only: the bridge language of the manifest (typescript, python, go or other)
	transportBy []scaffoldFact
	caps        []scaffoldCapability
	placeholder bool
	rag         []scaffoldRAG
	candidates  []scaffoldCandidate
	gaps        []readiness.ReportChecklistItem
	reportID    string
}

type scaffoldCandidate struct {
	Name string
	Kind string
	Refs []scaffoldFact
}

func (v *scaffoldVerified) plan(in ScaffoldInput) scaffoldPlan {
	p := scaffoldPlan{reportID: string(in.Report.ReportID)}
	if scaffoldAppIDRE.MatchString(in.ApplicationID) {
		p.appID, p.appIDGiven = in.ApplicationID, true
	} else {
		p.appID = "todo-application"
	}

	// the transport the verified evidence points at
	pick := func(reqs ...readiness.RequirementID) []scaffoldFact {
		var out []scaffoldFact
		seen := map[string]bool{}
		for _, f := range v.facts {
			for _, r := range reqs {
				if f.Requirement == r && len(out) < scaffoldMaxRefs {
					if k := f.ref(); !seen[k] {
						seen[k] = true
						out = append(out, f)
					}
				}
			}
		}
		return out
	}
	if by := pick("req.mcp.server", "req.mcp.tool_list"); len(by) > 0 {
		p.transport, p.transportBy = "mcp", by
	} else if by := pick("req.openapi.description"); len(by) > 0 {
		p.transport, p.transportBy = "openapi", by
	} else if by := pick("req.native.adapter"); len(by) > 0 {
		p.transport, p.transportBy, p.bridge = "native", by, scaffoldBridgeOf(by)
	} else {
		p.transport = "mcp"
	}

	// capability stubs: an inventory capability id that occurs in a cited, re-hashed file
	seenCap := map[string]bool{}
	for _, id := range in.Inventory.Capabilities {
		if len(p.caps) == scaffoldMaxCaps || seenCap[id] || !scaffoldCapIDRE.MatchString(id) {
			continue
		}
		refs := v.refsFor(id)
		if len(refs) == 0 {
			continue
		}
		seenCap[id] = true
		n := len(p.caps) + 1
		p.caps = append(p.caps, scaffoldCapability{ID: id, File: fmt.Sprintf("capabilities/cap-%03d.yaml", n), Doc: fmt.Sprintf("documentation/cap-%03d.md", n), Refs: refs})
	}
	if len(p.caps) == 0 {
		p.placeholder = true
		p.caps = []scaffoldCapability{{ID: "todo.capability", File: "capabilities/cap-001.yaml", Doc: "documentation/cap-001.md"}}
	}

	// tool and operation names: listed for the owner, never fields
	seenName := map[string]bool{}
	addCand := func(kind, name string) {
		if seenName[name] || seenCap[name] || !scaffoldNameRE.MatchString(name) || len(p.candidates) >= 128 {
			return
		}
		if refs := v.refsFor(name); len(refs) > 0 {
			seenName[name] = true
			p.candidates = append(p.candidates, scaffoldCandidate{Name: name, Kind: kind, Refs: refs})
		}
	}
	for _, t := range in.Inventory.MCPTools {
		addCand("MCP tool", t)
	}
	for _, d := range in.Inventory.OpenAPI {
		for _, op := range d.Operations {
			addCand("OpenAPI operation", op.OperationID)
		}
	}
	for _, id := range in.Inventory.Capabilities {
		addCand("capability candidate (not a valid capability id)", id)
	}

	// RAG source candidates: the folder of a cited document of the RAG candidate requirement
	seenDir := map[string]bool{}
	seenID := map[string]bool{}
	for _, f := range v.facts {
		if f.Requirement != "req.rag.source_candidates" || !scaffoldIsDocument(f.Path) || len(p.rag) == scaffoldMaxRAG {
			continue
		}
		dir := path.Dir(f.Path)
		if dir == "." || seenDir[dir] {
			continue
		}
		seenDir[dir] = true
		n := len(p.rag) + 1
		base := scaffoldSlug(path.Base(dir), n)
		id := base
		for k := 2; seenID[id]; k++ {
			id = fmt.Sprintf("%s-%d", base, k)
		}
		seenID[id] = true
		p.rag = append(p.rag, scaffoldRAG{ID: id, File: fmt.Sprintf("rag-sources/src-%03d.yaml", n), Dir: dir, Refs: []scaffoldFact{f}})
	}

	// requirements without verified evidence: by id only, never the report's claim. Once verified evidence meets one
	// transport, the requirements of the alternative transports are no gap (catalog.CatalogOtherTransport).
	catOf := map[readiness.RequirementID]readiness.Category{}
	if cat, err := catalog.CatalogLoad(); err == nil {
		for _, r := range cat.Requirements {
			catOf[r.ID] = r.Category
		}
	}
	used := map[readiness.Category]bool{}
	for id := range v.found {
		if c := catOf[id]; catalog.CatalogIsTransport(c) {
			used[c] = true
		}
	}
	for _, c := range in.Report.Checklist {
		if !v.reqs[c.RequirementID] && !catalog.CatalogOtherTransport(catOf[c.RequirementID], used) {
			p.gaps = append(p.gaps, c)
		}
	}
	return p
}

// scaffoldBridgeOf is the bridge language of a native endpoint: the language of the first verified file that cites the
// native adapter, or other.
func scaffoldBridgeOf(facts []scaffoldFact) string {
	for _, f := range facts {
		switch strings.ToLower(path.Ext(f.Path)) {
		case ".go":
			return "go"
		case ".ts", ".tsx", ".mts", ".cts", ".js", ".mjs", ".cjs":
			return "typescript"
		case ".py":
			return "python"
		}
	}
	return "other"
}

func scaffoldIsDocument(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".txt", ".rst", ".pdf", ".docx", ".html", ".htm":
		return true
	}
	return false
}

// scaffoldSlug turns a folder name into a RAG source id, or a fixed one when the name does not fit.
func scaffoldSlug(name string, n int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-_")
	if len(s) > 48 {
		s = s[:48]
	}
	if !scaffoldSlugRE.MatchString(s) {
		return fmt.Sprintf("source-%d", n)
	}
	return s
}
