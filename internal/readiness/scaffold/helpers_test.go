package scaffold

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/buxee/alix-ai/internal/readiness/scan"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

// scaffoldCanary is text that only a finding's summary or remediation carries: it must never reach an output file.
const scaffoldCanary = "SUMMARY-CANARY-7f3a"

func scaffoldSum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// scaffoldWorld is a scanned project (an in-memory file system), a report over it and an inventory.
type scaffoldWorld struct {
	files map[string]string
	fsys  fstest.MapFS
	in    ScaffoldInput
}

func scaffoldFinding(id string, req readiness.RequirementID, st readiness.Status, src readiness.Source, files map[string]string, paths ...string) readiness.Finding {
	f := readiness.Finding{
		SchemaVersion: 1, FindingID: readiness.FindingID("fnd_" + id), RequirementID: req, Status: st, Source: src,
		Remediation: readiness.RemediationText{En: readiness.Text(scaffoldCanary + " remediation")},
	}
	t := readiness.Text(scaffoldCanary + " summary " + id)
	f.Summary = &t
	for _, p := range paths {
		f.Evidence = append(f.Evidence, readiness.FindingEvidence{Path: readiness.RelativePath(p), LineStart: 1, LineEnd: 1, FileSHA256: readiness.SHA256(scaffoldSum(files[p]))})
	}
	return f
}

// scaffoldNewWorld builds the standard world: three verified findings and five that must never be used.
func scaffoldNewWorld() *scaffoldWorld { return scaffoldBuildWorld("") }

// scaffoldBuildWorld is scaffoldNewWorld with extra text in docs/guide.md, the file the verified capability finding cites.
func scaffoldBuildWorld(guideExtra string) *scaffoldWorld {
	files := map[string]string{
		"docs/guide.md":                  "The capabilities are catalog.find_title and reservations.create.\n" + guideExtra,
		"documents/loan-policies.md":     "Loans last three weeks.\n",
		"notes/unverified-notes.md":      "unverified.cap is mentioned here.\n",
		"notes/contradicted.md":          "contra.cap is mentioned here.\n",
		"notes/model-claim.md":           "model.cap is mentioned here.\n",
		"notes/illegal.md":               "illegal.cap is mentioned here.\n",
		"notes/stale.md":                 "stale.cap is mentioned here.\n",
		"api/mcp.md":                     "MCP tool find_title and the tool look_up_title are declared.\n",
		"README.md":                      "A project.\n",
		"docs/unrelated-but-verified.md": "Nothing about names.\n",
	}
	w := &scaffoldWorld{files: files, fsys: fstest.MapFS{}}
	for p, c := range files {
		w.fsys[p] = &fstest.MapFile{Data: []byte(c)}
	}
	fnd := func(id string, req readiness.RequirementID, st readiness.Status, src readiness.Source, paths ...string) readiness.Finding {
		return scaffoldFinding(id, req, st, src, files, paths...)
	}
	stale := fnd("0000000000000007", "req.surface.control_surface", readiness.StatusFound, readiness.SourceDeterministic, "notes/stale.md")
	stale.Evidence[0].FileSHA256 = readiness.SHA256(scaffoldSum("the file said something else when it was scanned"))
	w.in = ScaffoldInput{
		Report: readiness.Report{
			SchemaVersion: 1, ReportID: "rpt_0001",
			Findings: []readiness.Finding{
				fnd("0000000000000001", "req.surface.control_surface", readiness.StatusFound, readiness.SourceDeterministic, "docs/guide.md"),
				fnd("0000000000000002", "req.rag.source_candidates", readiness.StatusPartial, readiness.SourceModelVerified, "documents/loan-policies.md"),
				fnd("0000000000000003", "req.data.read", readiness.StatusUnverified, readiness.SourceDeterministic, "notes/unverified-notes.md"),
				fnd("0000000000000004", "req.permissions.model", readiness.StatusContradicted, readiness.SourceDeterministic, "notes/contradicted.md"),
				fnd("0000000000000005", "req.data.commit", readiness.StatusUnverified, readiness.SourceModelUnverified, "notes/model-claim.md"),
				// schema-illegal, and still never used: status and source are checked separately
				fnd("0000000000000006", "req.data.commit", readiness.StatusFound, readiness.SourceModelUnverified, "notes/illegal.md"),
				stale,
				fnd("0000000000000008", "req.mcp.tool_list", readiness.StatusFound, readiness.SourceDeterministic, "api/mcp.md"),
			},
			Checklist: []readiness.ReportChecklistItem{
				{RequirementID: "req.surface.control_surface", CheckID: "package.inventory", Level: readiness.LevelMandatory, Status: readiness.StatusFound},
				{RequirementID: "req.data.commit", CheckID: "application.deterministic_results", Level: readiness.LevelMandatory, Status: readiness.StatusFound},
			},
		},
		Inventory: scan.ScanInventory{
			Capabilities: []string{"catalog.find_title", "reservations.create", "unverified.cap", "contra.cap", "model.cap", "illegal.cap", "stale.cap", "../x", "con", "Bad.Id", "ghost.cap"},
			MCPTools:     []string{"find_title", "look_up_title", "unknown_tool"},
		},
		Source:        w.fsys,
		ApplicationID: "demo-app",
	}
	return w
}

// scaffoldReadTree returns every file under dir (relative slash path to content). A missing dir is an empty tree.
func scaffoldReadTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			out[rel+"/"] = ""
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = string(b)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return out
}

func scaffoldNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// scaffoldUncomment drops comments, so a test can ask what a file says as opposed to what it remarks.
func scaffoldUncomment(s string) string {
	var b strings.Builder
	for _, l := range strings.Split(s, "\n") {
		if i := strings.Index(l, " #"); i >= 0 {
			l = l[:i]
		}
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		b.WriteString(l + "\n")
	}
	return b.String()
}
