// Package scan is the deterministic leg of integration readiness discovery (master plan 9.5,
// ADR 0037): a bounded, read-only walk over a project directory the operator names, plus a set
// of rule-based detectors that turn what the project holds into readiness findings against the
// requirement catalog.
//
// There is no model, no network and no process execution here, and the package imports only the
// standard library, the generated readiness types and the catalog (TestScannerImportsNoNetworkOrExec).
// Project content is hostile data: every read is capped, nothing is executed, no package manager
// runs, no $ref is resolved, no symlink or junction is followed, and secret-looking files are
// recorded as present and never opened. Secret-looking values inside readable files are redacted
// before they reach a finding, an excerpt or an error.
//
// The same tree always produces byte-identical output: entries are sorted, findings are ordered
// by catalog order, there are no timestamps and no absolute paths. The scan is advisory: it
// certifies nothing, and a finding that says "found" means a rule saw evidence, which a verifier
// re-reads before it counts.
//
// Every package-level identifier here starts with scan or Scan.
package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/readiness/catalog"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

// ScanOptions configure one scan.
type ScanOptions struct {
	// Limits bound the walk; zero fields take their defaults.
	Limits ScanLimits
}

// ScanOperation is one API operation a project declares.
type ScanOperation struct {
	Method      string `json:"method,omitempty"`
	Path        string `json:"path,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	Line        int    `json:"line"`
	Typed       bool   `json:"typed_response"`
}

// ScanAPIDoc is one OpenAPI or Swagger document.
type ScanAPIDoc struct {
	Path         string          `json:"path"`
	Format       string          `json:"format"`
	Operations   []ScanOperation `json:"operations"`
	ExternalRefs int             `json:"external_refs_not_resolved"`
}

// ScanInventory is what the detectors recognised, beside the findings.
type ScanInventory struct {
	PackageRoot  string       `json:"package_root,omitempty"`
	Capabilities []string     `json:"capabilities"`
	OpenAPI      []ScanAPIDoc `json:"openapi"`
	MCPTools     []string     `json:"mcp_tools"`
	Ports        []string     `json:"published_ports"`
	Migrations   []string     `json:"migrations"`
	ModelFiles   []string     `json:"model_files"`
}

// ScanExcerpt is the redacted text of the first line of a finding's evidence, kept so a later
// stage can quote it without re-reading the file.
type ScanExcerpt struct {
	FindingID string `json:"finding_id"`
	Path      string `json:"path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Text      string `json:"text"`
}

// ScanResult is the complete, deterministic outcome of one scan.
type ScanResult struct {
	CatalogVersion int                 `json:"catalog_version"`
	CatalogSHA256  string              `json:"catalog_sha256"`
	InputsSHA256   string              `json:"inputs_sha256"`
	Limits         ScanLimits          `json:"limits"`
	Truncated      bool                `json:"truncated"`
	Files          []ScanFile          `json:"files"`
	Skipped        []ScanSkip          `json:"skipped"`
	SecretFiles    []string            `json:"secret_files_present_not_read"`
	Inventory      ScanInventory       `json:"inventory"`
	Findings       []readiness.Finding `json:"findings"`
	Excerpts       []ScanExcerpt       `json:"excerpts"`
}

// ScanDir scans the directory the operator named. It refuses a drive root, a system directory
// and a path that is not a directory, and it reports what it skipped and why.
func ScanDir(dir string, opt ScanOptions) (*ScanResult, error) {
	fsys, err := scanOpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer fsys.Close()
	return scanRun(fsys, opt)
}

func scanRun(fsys scanFS, opt ScanOptions) (*ScanResult, error) {
	cat, err := catalog.CatalogLoad()
	if err != nil {
		return nil, fmt.Errorf("readiness scan: %w", err)
	}
	lim := opt.Limits.scanWithDefaults()
	w := scanWalk(fsys, lim)
	st := newScanState(w.files)
	st.detectAll()
	findings, excerpts, err := st.buildFindings(cat)
	if err != nil {
		return nil, err
	}
	res := &ScanResult{
		CatalogVersion: catalog.CatalogVersion,
		CatalogSHA256:  cat.SHA256(),
		Limits:         lim,
		Truncated:      w.truncated,
		Skipped:        append(w.skipped, st.notes...),
		SecretFiles:    w.secretFiles,
		Inventory:      st.inventory(),
		Findings:       findings,
		Excerpts:       excerpts,
	}
	sort.Slice(res.Skipped, func(i, j int) bool {
		if res.Skipped[i].Path != res.Skipped[j].Path {
			return res.Skipped[i].Path < res.Skipped[j].Path
		}
		return res.Skipped[i].Reason < res.Skipped[j].Reason
	})
	h := sha256.New()
	for _, f := range w.files {
		res.Files = append(res.Files, f.ScanFile)
		if f.Kind != scanKindSecret {
			fmt.Fprintf(h, "%s\x00%s\x00%d\n", f.Path, f.SHA256, f.Size)
		} else {
			fmt.Fprintf(h, "%s\x00secret\n", f.Path)
		}
	}
	res.InputsSHA256 = hex.EncodeToString(h.Sum(nil))
	if res.Files == nil {
		res.Files = []ScanFile{}
	}
	if res.Skipped == nil {
		res.Skipped = []ScanSkip{}
	}
	if res.SecretFiles == nil {
		res.SecretFiles = []string{}
	}
	return res, nil
}

// scanSig is one thing a detector saw: a group name, where, and a short redacted detail.
type scanSig struct {
	Group  string
	Path   string
	Start  int
	End    int
	Detail string
}

const scanMaxSigsPerGroup = 64

type scanState struct {
	files    []*scanFileRec
	byPath   map[string]*scanFileRec
	sigs     map[string][]scanSig
	seen     map[string]bool
	notes    []ScanSkip
	pkgRoot  string
	pkgFound bool
	caps     []string
	capSet   map[string]bool
	apis     []ScanAPIDoc
	tools    map[string]bool
	ports    map[string]bool
	migs     map[string]bool
	models   []string
	// facts the deciders need beyond signal groups
	ragDecls          []scanRagDecl
	exposes           map[string]scanPortFact // Dockerfile path -> exposed ports
	composes          map[string]scanPortFact // compose path -> published container ports
	docFlags          map[string]uint8        // documentation file -> scanDoc* bits
	capsAreCandidates bool
	capDocCovered     int
	capCCCovered      int
}

func newScanState(files []*scanFileRec) *scanState {
	st := &scanState{
		files: files, byPath: map[string]*scanFileRec{}, sigs: map[string][]scanSig{}, seen: map[string]bool{},
		capSet: map[string]bool{}, tools: map[string]bool{}, ports: map[string]bool{}, migs: map[string]bool{},
		exposes: map[string]scanPortFact{}, composes: map[string]scanPortFact{}, docFlags: map[string]uint8{},
	}
	for _, f := range files {
		st.byPath[f.Path] = f
	}
	return st
}

// add records a signal. A span outside the file, or in a file with no lines, is dropped, so a
// signal can always be turned into evidence the verifier will accept.
func (s *scanState) add(group string, f *scanFileRec, start, end int, detail string) {
	if f == nil || f.SHA256 == "" || f.lines < 1 || start < 1 {
		return
	}
	if end < start {
		end = start
	}
	if end > f.lines {
		end = f.lines
	}
	if start > f.lines {
		return
	}
	key := fmt.Sprintf("%s\x00%s\x00%d", group, f.Path, start)
	if s.seen[key] || len(s.sigs[group]) >= scanMaxSigsPerGroup {
		return
	}
	s.seen[key] = true
	s.sigs[group] = append(s.sigs[group], scanSig{Group: group, Path: f.Path, Start: start, End: end, Detail: scanExcerpt(detail, 160)})
}

func (s *scanState) has(groups ...string) bool {
	for _, g := range groups {
		if len(s.sigs[g]) > 0 {
			return true
		}
	}
	return false
}

func (s *scanState) note(path, reason string) {
	s.notes = append(s.notes, ScanSkip{Path: path, Reason: reason})
}

func (s *scanState) inventory() ScanInventory {
	inv := ScanInventory{PackageRoot: "", Capabilities: append([]string{}, s.caps...), OpenAPI: append([]ScanAPIDoc{}, s.apis...)}
	if s.pkgFound {
		inv.PackageRoot = s.pkgRoot
		if inv.PackageRoot == "" {
			inv.PackageRoot = "."
		}
	}
	sortedKeys := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	inv.MCPTools = sortedKeys(s.tools)
	inv.Ports = sortedKeys(s.ports)
	inv.Migrations = sortedKeys(s.migs)
	inv.ModelFiles = append([]string{}, s.models...)
	sort.Strings(inv.ModelFiles)
	if inv.ModelFiles == nil {
		inv.ModelFiles = []string{}
	}
	sort.Slice(inv.OpenAPI, func(i, j int) bool { return inv.OpenAPI[i].Path < inv.OpenAPI[j].Path })
	return inv
}

func (s *scanState) detectAll() {
	s.detectPackage()
	s.detectCapabilities()
	for _, f := range s.files {
		if f.content == nil {
			continue
		}
		lines := scanLines(f.content)
		s.detectDocs(f, lines)
		s.detectMCPAndNative(f, lines)
		s.detectOpenAPI(f, lines)
		s.detectOperationsText(f, lines)
		s.detectScopeAndPermissions(f, lines)
		s.detectRAG(f, lines)
		s.detectModelAndDeploy(f, lines)
	}
	s.detectPathSignals()
	s.detectCoverage()
	s.detectDeployConsistency()
}

// scanEvidenceKey is the canonical text a finding id is derived from.
func scanEvidenceKey(ev []readiness.FindingEvidence) string {
	var b strings.Builder
	for _, e := range ev {
		fmt.Fprintf(&b, "%s:%d:%d:%s\n", e.Path, e.LineStart, e.LineEnd, e.FileSHA256)
	}
	return b.String()
}
