package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/readiness/scan"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

// rdyCanary stands for a secret value in a fixture. It is TEST-ONLY and not a real credential.
const rdyCanary = "TESTONLY-canary-0000000000000000"

// rdyWrite writes a fixture file.
func rdyWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

const rdyReadme = "# Clinic\n\nThe clinic exposes appointments over MCP.\nTools: availability_list, appointments_book_prepare.\nSee docs/api.md for the operations.\n"

// rdyProject builds a small project, scans it and returns the root and the scan.
func rdyProject(t *testing.T) (string, *scan.ScanResult) {
	t.Helper()
	root := t.TempDir()
	rdyWrite(t, root, "README.md", rdyReadme)
	rdyWrite(t, root, "docs/api.md", "# API\n\nGET /slots lists free slots.\nPOST /bookings/prepare prepares a booking.\n")
	rdyWrite(t, root, ".env", "API_PASSWORD="+rdyCanary+"\n")
	res, err := scan.ScanDir(root, scan.ScanOptions{})
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}
	return root, res
}

func rdyFileSHA(t *testing.T, res *scan.ScanResult, path string) string {
	t.Helper()
	for _, f := range res.Files {
		if f.Path == path {
			return f.SHA256
		}
	}
	t.Fatalf("no recorded file %q", path)
	return ""
}

func rdyEv(t *testing.T, res *scan.ScanResult, path string, a, b int64) readiness.FindingEvidence {
	t.Helper()
	return readiness.FindingEvidence{Path: readiness.RelativePath(path), LineStart: a, LineEnd: b, FileSHA256: readiness.SHA256(rdyFileSHA(t, res, path))}
}

func rdyFinding(req readiness.RequirementID, st readiness.Status, ev ...readiness.FindingEvidence) readiness.Finding {
	return readiness.Finding{SchemaVersion: 1, FindingID: "fnd_0000000000000000", RequirementID: req, Status: st, Source: readiness.SourceDeterministic, Evidence: ev}
}

// rdyRecordFS wraps the real root and records every open.
type rdyRecordFS struct {
	inner  rdyFS
	opened []string
}

func (r *rdyRecordFS) Lstat(name string) (os.FileInfo, error) { return r.inner.Lstat(name) }
func (r *rdyRecordFS) Open(name string) (rdyFile, error) {
	r.opened = append(r.opened, name)
	return r.inner.Open(name)
}
func (r *rdyRecordFS) Close() error { return r.inner.Close() }

func rdyOpen(t *testing.T, root string, res *scan.ScanResult) (*RdyVerifier, *rdyRecordFS) {
	t.Helper()
	real, err := RdyNew(root, res, nil)
	if err != nil {
		t.Fatalf("RdyNew: %v", err)
	}
	rec := &rdyRecordFS{inner: real.fsys}
	v, err := rdyNewWithFS(rec, res, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v, rec
}

func rdyOne(t *testing.T, v *RdyVerifier, f readiness.Finding) RdyVerified {
	t.Helper()
	res, err := v.Verify([]readiness.Finding{f}, nil)
	if err != nil || len(res.Verified) != 1 {
		t.Fatalf("Verify: %v %+v", err, res)
	}
	return res.Verified[0]
}

func rdyProposeOne(t *testing.T, v *RdyVerifier, p RdyProposal) (RdyVerified, *RdyResult) {
	t.Helper()
	res, err := v.Verify(nil, []RdyProposal{p})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(res.Verified) != 1 {
		return RdyVerified{}, res
	}
	return res.Verified[0], res
}

func rdyHasReason(v RdyVerified, r RdyReason) bool { return slices.Contains(v.Reasons(), r) }

func TestACleanScanOfTheClinicFixtureVerifiesWithoutDemotions(t *testing.T) {
	root := filepath.Join("..", "testdata", "clinic")
	res, err := scan.ScanDir(root, scan.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v, err := RdyNew(root, res, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	out, err := v.Verify(res.Findings, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Verified) != len(res.Findings) || len(out.Verified) == 0 {
		t.Fatalf("verified %d of %d findings", len(out.Verified), len(res.Findings))
	}
	found := 0
	for i, vf := range out.Verified {
		f := vf.Finding()
		if !vf.Confirmed() || len(vf.Reasons()) != 0 || f.Status != res.Findings[i].Status || vf.FromModel() {
			t.Errorf("%s: confirmed=%v reasons=%v status %s (scanner said %s)", f.RequirementID, vf.Confirmed(), vf.Reasons(), f.Status, res.Findings[i].Status)
		}
		if f.Status == readiness.StatusFound {
			found++
		}
	}
	if found < 10 {
		t.Fatalf("only %d found findings in the clinic fixture; the test would prove little", found)
	}
}

func TestFindingWithoutConfirmedEvidenceIsNeverFound(t *testing.T) {
	root, res := rdyProject(t)
	v, _ := rdyOpen(t, root, res)
	good := rdyEv(t, res, "README.md", 3, 4)
	bad := good
	bad.LineEnd = 99 // past the end of the file

	for _, st := range []readiness.Status{readiness.StatusFound, readiness.StatusPartial, readiness.StatusContradicted} {
		t.Run("no evidence "+string(st), func(t *testing.T) {
			vf := rdyOne(t, v, rdyFinding("req.mcp.server", st))
			if vf.Confirmed() || vf.Finding().Status != readiness.StatusUnverified || !rdyHasReason(vf, RdyReasonNoEvidence) {
				t.Fatalf("a %s finding with no evidence stands: %+v", st, vf.Finding())
			}
			if vf.Finding().UnverifiedReason == nil || *vf.Finding().UnverifiedReason == "" {
				t.Fatal("the demoted finding does not say why")
			}
		})
		t.Run("one entry does not confirm "+string(st), func(t *testing.T) {
			vf := rdyOne(t, v, rdyFinding("req.mcp.server", st, good, bad))
			if vf.Confirmed() || vf.Finding().Status != readiness.StatusUnverified || !rdyHasReason(vf, RdyReasonSpan) {
				t.Fatalf("a finding with a bad entry stands: %+v", vf.Finding())
			}
		})
	}

	// The same finding with every entry confirmed stands, and keeps its status.
	vf := rdyOne(t, v, rdyFinding("req.mcp.server", readiness.StatusFound, good))
	if !vf.Confirmed() || vf.Finding().Status != readiness.StatusFound {
		t.Fatalf("a confirmed finding was demoted: %+v %v", vf.Finding(), vf.Reasons())
	}

	// A rule that found nothing stays missing and cites nothing.
	miss := rdyOne(t, v, rdyFinding("req.mcp.server", readiness.StatusMissing, good))
	if miss.Finding().Status != readiness.StatusMissing || len(miss.Finding().Evidence) != 0 {
		t.Fatalf("a missing finding: %+v", miss.Finding())
	}

	// More evidence than a finding may carry is not accepted.
	many := make([]readiness.FindingEvidence, 33)
	for i := range many {
		many[i] = good
	}
	if vf := rdyOne(t, v, rdyFinding("req.mcp.server", readiness.StatusFound, many...)); vf.Finding().Status == readiness.StatusFound {
		t.Fatal("33 evidence entries were accepted")
	}

	// A model proposal with no evidence is model_unverified, never covered.
	mv, _ := rdyProposeOne(t, v, RdyProposal{Finding: rdyFinding("req.mcp.server", readiness.StatusFound)})
	if mv.Finding().Source != readiness.SourceModelUnverified || mv.Finding().Status != readiness.StatusUnverified || mv.Confirmed() {
		t.Fatalf("a model proposal with no evidence: %+v", mv.Finding())
	}
}

// rdyBadPaths are citations that must be refused, with the reason expected for each.
var rdyBadPaths = []struct {
	name, path string
	reason     RdyReason
}{
	{"parent", "../README.md", RdyReasonOutsideRoot},
	{"nested parent", "docs/../../README.md", RdyReasonOutsideRoot},
	{"absolute", "/etc/passwd", RdyReasonOutsideRoot},
	{"drive letter", "C:/Windows/win.ini", RdyReasonOutsideRoot},
	{"backslash", `docs\api.md`, RdyReasonOutsideRoot},
	{"dot segment", "./README.md", RdyReasonOutsideRoot},
	{"empty segment", "docs//api.md", RdyReasonOutsideRoot},
	{"stream", "README.md:hidden", RdyReasonOutsideRoot},
	{"control character", "README.md\x00", RdyReasonOutsideRoot},
	{"never scanned", "docs/other.md", RdyReasonNotScanned},
	{"case variant", "readme.md", RdyReasonNotScanned},
	{"secret file", ".env", RdyReasonSecretFile},
}

func TestEvidenceOutsideTheRootOrOnASecretFileIsRefused(t *testing.T) {
	root, res := rdyProject(t)
	// A file created after the scan is not in the inventory either.
	rdyWrite(t, root, "docs/other.md", "# other\nnot scanned\n")
	v, rec := rdyOpen(t, root, res)

	sha := rdyFileSHA(t, res, "README.md")
	envSum := sha256.Sum256([]byte("API_PASSWORD=" + rdyCanary + "\n"))
	for _, c := range rdyBadPaths {
		t.Run(c.name, func(t *testing.T) {
			ev := readiness.FindingEvidence{Path: readiness.RelativePath(c.path), LineStart: 1, LineEnd: 1, FileSHA256: readiness.SHA256(sha)}
			if c.path == ".env" {
				ev.FileSHA256 = readiness.SHA256(hex.EncodeToString(envSum[:])) // even with the right hash
			}
			vf := rdyOne(t, v, rdyFinding("req.mcp.server", readiness.StatusFound, ev))
			if vf.Confirmed() || vf.Finding().Status == readiness.StatusFound || !rdyHasReason(vf, c.reason) {
				t.Fatalf("%q: status %s reasons %v, want %s", c.path, vf.Finding().Status, vf.Reasons(), c.reason)
			}
			mv, _ := rdyProposeOne(t, v, RdyProposal{Finding: rdyFinding("req.mcp.server", readiness.StatusFound, ev), Quotes: []string{"The clinic exposes"}})
			if mv.Finding().Source != readiness.SourceModelUnverified || len(mv.Finding().Evidence) != 0 {
				t.Fatalf("%q: the model proposal was accepted or echoed its citation: %+v", c.path, mv.Finding())
			}
		})
	}
	for _, p := range rec.opened {
		if p == ".env" || strings.Contains(p, "..") || strings.HasPrefix(p, "/") || p == "docs/other.md" {
			t.Errorf("the verifier opened %q", p)
		}
	}
}

func TestACitedPathThatBecameALinkIsRefused(t *testing.T) {
	root, res := rdyProject(t)
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte(rdyReadme), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "README.md")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	v, _ := rdyOpen(t, root, res)
	vf := rdyOne(t, v, rdyFinding("req.mcp.server", readiness.StatusFound, rdyEv(t, res, "README.md", 3, 3)))
	if vf.Confirmed() || !rdyHasReason(vf, RdyReasonLink) {
		t.Fatalf("a link was followed: %+v %v", vf.Finding(), vf.Reasons())
	}
}

func TestTamperedFileDemotesTheFinding(t *testing.T) {
	root, res := rdyProject(t)
	ev := rdyEv(t, res, "README.md", 3, 4)

	// Change one byte outside the cited lines, keeping the line count: the span still exists and
	// the cited text is untouched, so only the hash can notice.
	tampered := strings.Replace(rdyReadme, "# Clinic", "# Clinix", 1)
	if len(tampered) != len(rdyReadme) {
		t.Fatal("fixture bug")
	}
	rdyWrite(t, root, "README.md", tampered)

	v, _ := rdyOpen(t, root, res)
	vf := rdyOne(t, v, rdyFinding("req.mcp.server", readiness.StatusFound, ev))
	if vf.Confirmed() || vf.Finding().Status != readiness.StatusUnverified || !rdyHasReason(vf, RdyReasonChanged) {
		t.Fatalf("a finding on a tampered file stands: %+v %v", vf.Finding(), vf.Reasons())
	}
	if r := vf.Finding().UnverifiedReason; r == nil || !strings.Contains(string(*r), "hash") {
		t.Fatalf("the reason does not say the hash changed: %v", r)
	}

	// A model proposal quoting the unchanged lines is refused for the same reason.
	mv, _ := rdyProposeOne(t, v, RdyProposal{Finding: rdyFinding("req.mcp.server", readiness.StatusFound, ev), Quotes: []string{"Tools: availability_list"}})
	if mv.Confirmed() || mv.Finding().Source != readiness.SourceModelUnverified || !rdyHasReason(mv, RdyReasonChanged) {
		t.Fatalf("a proposal on a tampered file stands: %+v %v", mv.Finding(), mv.Reasons())
	}

	// A hash the model made up for an untouched file does not confirm either.
	fresh, _ := rdyOpen(t, root, res)
	rdyWrite(t, root, "README.md", rdyReadme)
	wrong := ev
	wrong.FileSHA256 = readiness.SHA256(strings.Repeat("0", 64))
	if vf := rdyOne(t, fresh, rdyFinding("req.mcp.server", readiness.StatusFound, wrong)); vf.Confirmed() {
		t.Fatal("a made-up hash confirmed")
	}
}

func TestAModelProposalConfirmsOnlyWithAQuoteThatIsInTheSpan(t *testing.T) {
	root, res := rdyProject(t)
	v, _ := rdyOpen(t, root, res)
	ev := rdyEv(t, res, "README.md", 3, 4)
	prop := func(quote string, st readiness.Status) RdyProposal {
		f := rdyFinding("req.mcp.server", st, ev)
		f.Source = readiness.SourceDeterministic // a model cannot choose its own source
		f.Category, f.CheckID = readiness.CategoryDocumentation, "package.authenticity"
		return RdyProposal{Finding: f, Quotes: []string{quote}}
	}

	ok, _ := rdyProposeOne(t, v, prop("Tools:   availability_list,\tappointments_book_prepare.", readiness.StatusFound))
	f := ok.Finding()
	if !ok.Confirmed() || f.Source != readiness.SourceModelVerified || f.Status != readiness.StatusFound || !ok.FromModel() {
		t.Fatalf("a good proposal: %+v %v", f, ok.Reasons())
	}
	if f.Category != readiness.CategoryMCP || f.CheckID == "package.authenticity" {
		t.Fatalf("the category and check id came from the proposal: %s %s", f.Category, f.CheckID)
	}
	if !strings.HasPrefix(string(f.FindingID), "fnd_") || len(f.FindingID) != 20 {
		t.Fatalf("finding id %q", f.FindingID)
	}

	cases := []struct {
		name   string
		p      RdyProposal
		reason RdyReason
	}{
		{"quote from another line", prop("See docs/api.md for the operations.", readiness.StatusFound), RdyReasonQuoteMismatch},
		{"invented quote", prop("The clinic never closes.", readiness.StatusFound), RdyReasonQuoteMismatch},
		{"too short", prop("Tools", readiness.StatusFound), RdyReasonQuoteMismatch},
		{"no quote", prop("", readiness.StatusFound), RdyReasonQuoteMissing},
		{"model claims missing", prop("Tools: availability_list", readiness.StatusMissing), RdyReasonClaimNotAllowed},
		{"model claims contradicted", prop("Tools: availability_list", readiness.StatusContradicted), RdyReasonClaimNotAllowed},
		{"model claims unverified", prop("Tools: availability_list", readiness.StatusUnverified), RdyReasonClaimNotAllowed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mv, _ := rdyProposeOne(t, v, c.p)
			if mv.Confirmed() || mv.Finding().Source != readiness.SourceModelUnverified || mv.Finding().Status != readiness.StatusUnverified || !rdyHasReason(mv, c.reason) {
				t.Fatalf("%+v %v, want %s", mv.Finding(), mv.Reasons(), c.reason)
			}
		})
	}

	// A proposal with no matching quote entry has no quote.
	noQuotes := prop("x", readiness.StatusFound)
	noQuotes.Quotes = nil
	if mv, _ := rdyProposeOne(t, v, noQuotes); mv.Confirmed() {
		t.Fatal("a proposal without quotes confirmed")
	}

	// Unknown requirements and repeats are dropped, not turned into findings.
	unknown := prop("Tools: availability_list", readiness.StatusFound)
	unknown.Finding.RequirementID = "req.not.in.the.catalog"
	res2, err := v.Verify(nil, []RdyProposal{unknown, prop("Tools: availability_list", readiness.StatusFound), prop("Tools: availability_list", readiness.StatusFound)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Verified) != 1 || len(res2.Dropped) != 2 || res2.Dropped[0].Reason != RdyReasonUnknownReq || res2.Dropped[1].Reason != RdyReasonDuplicate {
		t.Fatalf("verified %d dropped %+v", len(res2.Verified), res2.Dropped)
	}
	// A rule finding for an unknown requirement is corrupt input.
	if _, err := v.Verify([]readiness.Finding{rdyFinding("req.not.in.the.catalog", readiness.StatusMissing)}, nil); err == nil {
		t.Fatal("a rule finding for an unknown requirement was accepted")
	}
}

func TestFreeTextFromAModelIsRedactedAndBounded(t *testing.T) {
	root, res := rdyProject(t)
	v, _ := rdyOpen(t, root, res)
	sum := readiness.Text("password = " + rdyCanary + "\nline two " + strings.Repeat("x", 2000))
	rem := readiness.Text("set token=" + rdyCanary)
	f := rdyFinding("req.mcp.server", readiness.StatusFound, rdyEv(t, res, "README.md", 3, 3))
	f.Summary, f.Remediation = &sum, readiness.RemediationText{En: rem}
	mv, _ := rdyProposeOne(t, v, RdyProposal{Finding: f, Quotes: []string{"The clinic exposes appointments"}})
	got := string(*mv.Finding().Summary) + " " + string(mv.Finding().Remediation.En)
	if strings.Contains(got, rdyCanary) || strings.ContainsAny(got, "\n\r") || len([]rune(string(*mv.Finding().Summary))) > rdyMaxSummaryRunes {
		t.Fatalf("model text was not cleaned: %q", got)
	}
}

// rdyViolations runs every security property against a verifier and lists the ones that fail.
// The real verifier has none; a verifier with a rule removed must have at least one.
func rdyViolations(t *testing.T, root string, res *scan.ScanResult, mutate func(*RdyVerifier)) []string {
	t.Helper()
	real, err := RdyNew(root, res, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := &rdyRecordFS{inner: real.fsys}
	v, err := rdyNewWithFS(rec, res, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if mutate != nil {
		mutate(v)
	}
	var bad []string
	run := func(f readiness.Finding) RdyVerified {
		out, err := v.Verify([]readiness.Finding{f}, nil)
		if err != nil || len(out.Verified) != 1 {
			t.Fatalf("Verify: %v", err)
		}
		return out.Verified[0]
	}
	sha := rdyFileSHA(t, res, "README.md")
	ok := readiness.FindingEvidence{Path: "README.md", LineStart: 3, LineEnd: 4, FileSHA256: readiness.SHA256(sha)}

	if vf := run(rdyFinding("req.mcp.server", readiness.StatusFound, ok)); !vf.Confirmed() {
		bad = append(bad, "an untouched citation does not confirm")
	}
	wrongHash := ok
	wrongHash.FileSHA256 = readiness.SHA256(strings.Repeat("a", 64))
	if vf := run(rdyFinding("req.mcp.server", readiness.StatusFound, wrongHash)); vf.Confirmed() || !rdyHasReason(vf, RdyReasonChanged) {
		bad = append(bad, "a wrong hash confirmed")
	}
	escape := ok
	escape.Path = "../README.md"
	if vf := run(rdyFinding("req.mcp.server", readiness.StatusFound, escape)); vf.Confirmed() || !rdyHasReason(vf, RdyReasonOutsideRoot) {
		bad = append(bad, "a path outside the root was not refused as such")
	}
	secret := ok
	secret.Path = ".env"
	if vf := run(rdyFinding("req.mcp.server", readiness.StatusFound, secret)); vf.Confirmed() || !rdyHasReason(vf, RdyReasonSecretFile) {
		bad = append(bad, "a secret file was not refused as such")
	}
	if slices.Contains(rec.opened, ".env") {
		bad = append(bad, "a secret file was opened")
	}
	out, err := v.Verify(nil, []RdyProposal{{Finding: rdyFinding("req.mcp.server", readiness.StatusFound, ok), Quotes: []string{"The clinic never closes."}}})
	if err != nil || (len(out.Verified) == 1 && out.Verified[0].Confirmed()) {
		bad = append(bad, "a quote that is not in the span confirmed")
	}
	return bad
}

func TestVerifierMutantsAreCaught(t *testing.T) {
	root, res := rdyProject(t)
	if bad := rdyViolations(t, root, res, nil); len(bad) != 0 {
		t.Fatalf("the real verifier breaks its own properties: %v", bad)
	}
	mutants := map[string]func(*RdyVerifier){
		"skips the hash check": func(v *RdyVerifier) { v.hashMatches = func(string, string) bool { return true } },
		"reads secret files":   func(v *RdyVerifier) { v.refuseKind = func(string) bool { return false } },
		"accepts any path":     func(v *RdyVerifier) { v.pathOK = func(string) bool { return true } },
		"accepts any quote":    func(v *RdyVerifier) { v.quoteIn = func([]string, string) bool { return true } },
	}
	for name, m := range mutants {
		t.Run(name, func(t *testing.T) {
			if bad := rdyViolations(t, root, res, m); len(bad) == 0 {
				t.Fatalf("the mutant that %s was not caught", name)
			}
		})
	}
}

func TestEveryScannerSecretFileIsRefusedByName(t *testing.T) {
	// The verifier relies on the scan's own inventory to know a file is a secret. This keeps the
	// two in step: whatever the scanner lists as present-and-not-read is refused here.
	root := t.TempDir()
	names := []string{".env", ".env.production", "id_rsa", "server.pem", "credentials.json", "tls.key", "token.txt", "service-account-prod.json"}
	for _, n := range names {
		rdyWrite(t, root, n, "TESTONLY\n")
	}
	rdyWrite(t, root, "README.md", rdyReadme)
	res, err := scan.ScanDir(root, scan.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SecretFiles) != len(names) {
		t.Fatalf("the scanner recorded %v as secret files, want %d", res.SecretFiles, len(names))
	}
	v, rec := rdyOpen(t, root, res)
	for _, n := range res.SecretFiles {
		ev := readiness.FindingEvidence{Path: readiness.RelativePath(n), LineStart: 1, LineEnd: 1, FileSHA256: readiness.SHA256(strings.Repeat("b", 64))}
		if vf := rdyOne(t, v, rdyFinding("req.mcp.server", readiness.StatusFound, ev)); !rdyHasReason(vf, RdyReasonSecretFile) {
			t.Errorf("%s: %v", n, vf.Reasons())
		}
	}
	if len(rec.opened) != 0 {
		t.Fatalf("opened %v", rec.opened)
	}
}

func TestOnlyExcerptsOfConfirmedFindingsAtTheirOwnEvidenceSurvive(t *testing.T) {
	root, res := rdyProject(t)
	v, _ := rdyOpen(t, root, res)
	ev := rdyEv(t, res, "README.md", 3, 3)
	good := rdyFinding("req.mcp.server", readiness.StatusFound, ev)
	good.FindingID = "fnd_1111111111111111"
	badEv := ev
	badEv.FileSHA256 = readiness.SHA256(strings.Repeat("0", 64))
	bad := rdyFinding("req.mcp.tool_list", readiness.StatusFound, badEv)
	bad.FindingID = "fnd_2222222222222222"
	out, err := v.Verify([]readiness.Finding{good, bad}, nil)
	if err != nil {
		t.Fatal(err)
	}
	x := func(id string, a, b int, text string) scan.ScanExcerpt {
		return scan.ScanExcerpt{FindingID: id, Path: "README.md", LineStart: a, LineEnd: b, Text: text}
	}
	in := []scan.ScanExcerpt{
		x("fnd_1111111111111111", 3, 3, "The clinic exposes appointments over MCP."),
		x("fnd_1111111111111111", 4, 4, "a span the finding does not cite"),
		x("fnd_2222222222222222", 3, 3, "an excerpt of a finding that did not confirm"),
		x("fnd_3333333333333333", 3, 3, "an excerpt of a finding that does not exist"),
		{FindingID: "fnd_1111111111111111", Path: "docs/api.md", LineStart: 3, LineEnd: 3, Text: "another file"},
		x("fnd_1111111111111111", 3, 3, "token = "+rdyCanary),
	}
	got := v.ConfirmedExcerpts(out.Verified, in)
	if len(got) != 2 || got[0].Text != in[0].Text {
		t.Fatalf("surviving excerpts: %+v", got)
	}
	if strings.Contains(got[1].Text, rdyCanary) {
		t.Fatal("an excerpt was passed on unredacted")
	}
}
