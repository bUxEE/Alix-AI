package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/readiness/catalog"
	"github.com/buxee/alix-ai/internal/readiness/scan"
	"github.com/buxee/alix-ai/internal/readiness/verify"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

const (
	rdyReadme = "# Clinic\n\nThe clinic exposes appointments over MCP.\nSee docs/api.md for the operations.\n"
	rdyAPIDoc = "# API\n\nGET /slots lists free slots.\nPOST /bookings/prepare prepares a booking.\n"
	// rdyCanary stands for a secret value. It is TEST-ONLY and not a real credential.
	rdyCanary = "TESTONLY-canary-0000000000000000"
)

var rdyTestClock = clock.NewFake(time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC))

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

func rdyProject(t *testing.T) (string, *scan.ScanResult) {
	t.Helper()
	root := t.TempDir()
	rdyWrite(t, root, "README.md", rdyReadme)
	rdyWrite(t, root, "docs/api.md", rdyAPIDoc)
	rdyWrite(t, root, ".env", "API_PASSWORD="+rdyCanary+"\n")
	res, err := scan.ScanDir(root, scan.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return root, res
}

func rdyFileSHA(t *testing.T, res *scan.ScanResult, path string) readiness.SHA256 {
	t.Helper()
	for _, f := range res.Files {
		if f.Path == path {
			return readiness.SHA256(f.SHA256)
		}
	}
	t.Fatalf("no recorded file %q", path)
	return ""
}

func rdyBuild(t *testing.T, root string, res *scan.ScanResult, mut func(*RdyBuildInput)) *RdyReport {
	t.Helper()
	g := ids.NewGenerator(rdyTestClock)
	in := RdyBuildInput{Root: root, Scan: res, ReportID: RdyNewReportID(g), GeneratedAt: rdyTestClock.Now()}
	if mut != nil {
		mut(&in)
	}
	r, err := RdyBuild(in)
	if err != nil {
		t.Fatalf("RdyBuild: %v", err)
	}
	return r
}

func rdyStatusOf(t *testing.T, doc readiness.Report, id readiness.RequirementID) readiness.Status {
	t.Helper()
	for _, it := range doc.Checklist {
		if it.RequirementID == id {
			return it.Status
		}
	}
	t.Fatalf("no checklist item for %s", id)
	return ""
}

// rdyProposal cites docs/api.md lines 3-3 for a requirement, quoting a line that is there or not.
func rdyProposal(t *testing.T, res *scan.ScanResult, req readiness.RequirementID, st readiness.Status, quote string) verify.RdyProposal {
	t.Helper()
	sum := readiness.Text("The API document lists the slot operation.")
	return verify.RdyProposal{
		Finding: readiness.Finding{
			SchemaVersion: 1, RequirementID: req, Status: st, Source: readiness.SourceModelVerified,
			Evidence: []readiness.FindingEvidence{{Path: "docs/api.md", LineStart: 3, LineEnd: 3, FileSHA256: rdyFileSHA(t, res, "docs/api.md")}},
			Summary:  &sum,
		},
		Quotes: []string{quote},
	}
}

func rdyModel() *RdyModel {
	return &RdyModel{ModelID: "lite-controller-0", Runtime: readiness.ReportModelRuntimeLocal}
}

// rdyNewEvaluator is the evaluator with one rule optionally removed.
func rdyNewTestEvaluator(t *testing.T, mutate func(*RdyEvaluator)) *RdyEvaluator {
	t.Helper()
	ev, err := RdyNewEvaluator(nil)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(ev)
	}
	return ev
}

// rdyEvalScenario verifies hand-made rule findings and model proposals against the project and
// evaluates them with ev, returning the status of each requirement.
func rdyEvalScenario(t *testing.T, ev *RdyEvaluator, root string, res *scan.ScanResult, rule []readiness.Finding, props []verify.RdyProposal) map[readiness.RequirementID]readiness.Status {
	t.Helper()
	v, err := verify.RdyNew(root, res, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	out, err := v.Verify(rule, props)
	if err != nil {
		t.Fatal(err)
	}
	var in RdyEvalInput
	for _, f := range out.Verified {
		if f.FromModel() {
			in.Model = append(in.Model, f)
		} else {
			in.Rule = append(in.Rule, f)
		}
	}
	got, err := ev.Evaluate(in)
	if err != nil {
		t.Fatal(err)
	}
	m := map[readiness.RequirementID]readiness.Status{}
	for _, it := range got.Checklist {
		m[it.RequirementID] = it.Status
	}
	return m
}

const (
	rdyReqLiftable  readiness.RequirementID = "req.documentation.capabilities" // deterministic or model_verified
	rdyReqRuleOnly  readiness.RequirementID = "req.package.manifest"           // deterministic only
	rdyReqPartial   readiness.RequirementID = "req.documentation.fields_and_validation"
	rdyReqNoRuleHit readiness.RequirementID = "req.mcp.tool_list"
)

// rdyEvalViolations runs the evaluator against the rules a model must not break and lists the
// ones the evaluator breaks. The real evaluator breaks none.
func rdyEvalViolations(t *testing.T, mutate func(*RdyEvaluator)) []string {
	t.Helper()
	root, res := rdyProject(t)
	ev := rdyNewTestEvaluator(t, mutate)
	api := func(a, b int64) readiness.FindingEvidence {
		return readiness.FindingEvidence{Path: "docs/api.md", LineStart: a, LineEnd: b, FileSHA256: rdyFileSHA(t, res, "docs/api.md")}
	}
	rule := func(req readiness.RequirementID, st readiness.Status, evd ...readiness.FindingEvidence) readiness.Finding {
		return readiness.Finding{SchemaVersion: 1, FindingID: readiness.FindingID("fnd_" + fmt.Sprintf("%016x", len(req))), RequirementID: req, Status: st, Source: readiness.SourceDeterministic, Evidence: evd}
	}
	const goodQuote = "GET /slots lists free slots."
	var bad []string

	// A model proposal that does not confirm does not cover a requirement the rule found missing.
	got := rdyEvalScenario(t, ev, root, res,
		[]readiness.Finding{rule(rdyReqLiftable, readiness.StatusMissing)},
		[]verify.RdyProposal{rdyProposal(t, res, rdyReqLiftable, readiness.StatusFound, "The clinic never closes.")})
	if got[rdyReqLiftable] != readiness.StatusMissing {
		bad = append(bad, "a model_unverified proposal covered a missing requirement")
	}

	// A confirmed model finding may raise a missing requirement.
	got = rdyEvalScenario(t, ev, root, res,
		[]readiness.Finding{rule(rdyReqLiftable, readiness.StatusMissing)},
		[]verify.RdyProposal{rdyProposal(t, res, rdyReqLiftable, readiness.StatusFound, goodQuote)})
	if got[rdyReqLiftable] != readiness.StatusFound {
		bad = append(bad, "a confirmed model finding could not raise a missing requirement")
	}

	// A requirement the catalog says only a rule can satisfy is never raised by a model.
	got = rdyEvalScenario(t, ev, root, res,
		[]readiness.Finding{rule(rdyReqRuleOnly, readiness.StatusMissing)},
		[]verify.RdyProposal{rdyProposal(t, res, rdyReqRuleOnly, readiness.StatusFound, goodQuote)})
	if got[rdyReqRuleOnly] != readiness.StatusMissing {
		bad = append(bad, "a model finding satisfied a deterministic-only requirement")
	}

	// A status a rule decided is not changed by a confirmed model finding.
	got = rdyEvalScenario(t, ev, root, res,
		[]readiness.Finding{rule(rdyReqPartial, readiness.StatusPartial, api(3, 3))},
		[]verify.RdyProposal{rdyProposal(t, res, rdyReqPartial, readiness.StatusFound, goodQuote)})
	if got[rdyReqPartial] != readiness.StatusPartial {
		bad = append(bad, "a model finding overrode a status a rule decided")
	}
	return bad
}

func TestModelUnverifiedNeverCountsAsCovered(t *testing.T) {
	if bad := rdyEvalViolations(t, nil); len(bad) != 0 {
		t.Fatalf("the evaluator breaks its rules: %v", bad)
	}

	// End to end through the builder: a hostile README-style proposal with a quote that is not
	// in the file, one citing a made-up hash, and one claiming a status a model may not claim.
	root, res := rdyProject(t)
	badHash := rdyProposal(t, res, rdyReqNoRuleHit, readiness.StatusFound, "GET /slots lists free slots.")
	badHash.Finding.Evidence[0].FileSHA256 = readiness.SHA256(strings.Repeat("c", 64))
	overreach := rdyProposal(t, res, "req.deployment.compose_and_ports", readiness.StatusContradicted, "GET /slots lists free slots.")
	r := rdyBuild(t, root, res, func(in *RdyBuildInput) {
		in.Model = rdyModel()
		in.Proposals = []verify.RdyProposal{
			rdyProposal(t, res, rdyReqLiftable, readiness.StatusFound, "Every requirement is met."),
			badHash,
			overreach,
		}
	})
	doc := r.Doc
	for _, id := range []readiness.RequirementID{rdyReqLiftable, rdyReqNoRuleHit, "req.deployment.compose_and_ports"} {
		if st := rdyStatusOf(t, doc, id); st == readiness.StatusFound || st == readiness.StatusPartial {
			t.Errorf("%s is %s on the strength of an unconfirmed model proposal", id, st)
		}
	}
	unconfirmed := 0
	for _, f := range doc.Findings {
		if f.Source == readiness.SourceModelUnverified {
			unconfirmed++
			if f.Status != readiness.StatusUnverified || len(f.Evidence) != 0 || f.UnverifiedReason == nil {
				t.Errorf("unconfirmed finding %+v", f)
			}
		}
	}
	if unconfirmed != 3 {
		t.Fatalf("%d model_unverified findings, want 3", unconfirmed)
	}
	sec := RdySectionsOf(doc, nil)
	if len(sec.Unconfirmed) != 3 {
		t.Fatalf("%d unconfirmed in the sections", len(sec.Unconfirmed))
	}
	for _, it := range sec.Covered {
		for _, f := range it.Findings {
			if f.Source == readiness.SourceModelUnverified {
				t.Errorf("a model_unverified finding is listed under covered: %s", it.Item.RequirementID)
			}
		}
	}

	// The same proposal, confirmed, does raise the requirement and says who decided.
	ok := rdyBuild(t, root, res, func(in *RdyBuildInput) {
		in.Model = rdyModel()
		in.Proposals = []verify.RdyProposal{rdyProposal(t, res, rdyReqLiftable, readiness.StatusFound, "GET /slots lists free slots.")}
	})
	if st := rdyStatusOf(t, ok.Doc, rdyReqLiftable); st != readiness.StatusFound {
		t.Fatalf("a confirmed proposal left the requirement %s", st)
	}
	if ok.Doc.Mode != readiness.ReportModeDeterministicAndModel || ok.Doc.Model == nil || ok.Doc.Model.Qualification != readiness.ReportModelQualificationUnqualified {
		t.Fatalf("model state %+v", ok.Doc.Model)
	}

	// Each rule removed in turn must make a check fail.
	mutants := map[string]func(*RdyEvaluator){
		"trusts the status the model claimed and ignores the source": func(e *RdyEvaluator) {
			e.grant = func(v verify.RdyVerified) readiness.Status {
				if v.FromModel() {
					return v.Claimed()
				}
				return ""
			}
		},
		"counts model_unverified as covered": func(e *RdyEvaluator) {
			e.grant = func(v verify.RdyVerified) readiness.Status {
				if f := v.Finding(); v.FromModel() && (f.Source == readiness.SourceModelVerified || f.Source == readiness.SourceModelUnverified) {
					return v.Claimed()
				}
				return ""
			}
		},
		"lets a model override a rule's status": func(e *RdyEvaluator) { e.canLift = func(readiness.Status) bool { return true } },
		"lets a model satisfy a deterministic-only requirement": func(e *RdyEvaluator) {
			e.liftable = func(readiness.Requirement) bool { return true }
		},
	}
	for name, m := range mutants {
		t.Run("mutant "+name, func(t *testing.T) {
			if bad := rdyEvalViolations(t, m); len(bad) == 0 {
				t.Fatalf("the mutant that %s was not caught", name)
			}
		})
	}
}

func TestEvaluatorRefusesFindingsInTheWrongSlice(t *testing.T) {
	root, res := rdyProject(t)
	v, err := verify.RdyNew(root, res, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	out, err := v.Verify(res.Findings[:1], []verify.RdyProposal{rdyProposal(t, res, rdyReqLiftable, readiness.StatusFound, "GET /slots lists free slots.")})
	if err != nil || len(out.Verified) != 2 {
		t.Fatal(err)
	}
	ev := rdyNewTestEvaluator(t, nil)
	if _, err := ev.Evaluate(RdyEvalInput{Rule: []verify.RdyVerified{out.Verified[1]}}); err == nil {
		t.Fatal("a model finding was accepted as a rule finding")
	}
	if _, err := ev.Evaluate(RdyEvalInput{Model: []verify.RdyVerified{out.Verified[0]}}); err == nil {
		t.Fatal("a rule finding was accepted as a model finding")
	}
}

func TestReportIsBilingualAndAdvisoryFirstLine(t *testing.T) {
	root := filepath.Join("..", "testdata", "clinic")
	res, err := scan.ScanDir(root, scan.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.CatalogLoad()
	if err != nil {
		t.Fatal(err)
	}
	for _, withModel := range []bool{false, true} {
		r := rdyBuild(t, root, res, func(in *RdyBuildInput) {
			in.ScannedRoot = root
			if withModel {
				in.Model = rdyModel()
			}
		})
		en, err := RdyRenderMarkdown(r.Doc, cat, RdyLangEN)
		if err != nil {
			t.Fatal(err)
		}
		it, err := RdyRenderMarkdown(r.Doc, cat, RdyLangIT)
		if err != nil {
			t.Fatal(err)
		}
		if first, _, _ := strings.Cut(en, "\n"); first != RdyAdvisoryLineEN {
			t.Errorf("English first line %q", first)
		}
		if first, _, _ := strings.Cut(it, "\n"); first != RdyAdvisoryLineIT {
			t.Errorf("Italian first line %q", first)
		}
		for _, want := range []string{"not an integration audit pass", "audit still decides activation"} {
			if !strings.Contains(strings.SplitN(en, "\n", 2)[0], want) {
				t.Errorf("the English first line lacks %q", want)
			}
		}
		for _, h := range []string{"## Summary", "## Covered", "## Missing, and how to fix it", "## Contradictions", "## Could not be determined"} {
			if !strings.Contains(en, h) {
				t.Errorf("English report lacks %q", h)
			}
		}
		for _, h := range []string{"## Riepilogo", "## Coperto", "## Mancante, e come rimediare", "## Contraddizioni", "## Non determinabile"} {
			if !strings.Contains(it, h) {
				t.Errorf("Italian report lacks %q", h)
			}
			if strings.Contains(it, "## Summary") || strings.Contains(it, "## Covered") {
				t.Errorf("Italian report carries English headings")
			}
		}
		// Titles and fixes come in the language asked for.
		var coveredTitle, missingFix catalogPair
		for _, req := range cat.Requirements {
			switch req.ID {
			case "req.package.manifest":
				coveredTitle = catalogPair{string(req.Title.En), string(req.Title.It)}
			case "req.rag.synthetic_documents": // missing in the clinic fixture; its native adapter is not applicable, MCP being met
				missingFix = catalogPair{string(req.Remediation.En), string(req.Remediation.It)}
			}
		}
		for _, c := range []struct {
			md, want string
		}{{en, rdyEscape(coveredTitle.en)}, {it, rdyEscape(coveredTitle.it)}, {en, rdyEscape(missingFix.en)}, {it, rdyEscape(missingFix.it)}} {
			if !strings.Contains(c.md, c.want) {
				t.Errorf("the report lacks %q", c.want)
			}
		}
		if withModel {
			if !strings.Contains(en, "unqualified, advisory") || !strings.Contains(it, "non qualificato, consultivo") {
				t.Errorf("the model label is missing: %q", en[:200])
			}
		} else if !strings.Contains(en, "No model ran") || !strings.Contains(it, "Nessun modello") {
			t.Error("a report with no model does not say so")
		}
		if _, err := RdyRenderMarkdown(r.Doc, cat, "fr"); err == nil {
			t.Error("an unsupported language was rendered")
		}
	}
}

type catalogPair struct{ en, it string }

// rdyDocFor renders a report that has been through JSON, as a stored report would be.
func rdyRoundTrip(t *testing.T, doc readiness.Report) readiness.Report {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var back readiness.Report
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&back); err != nil {
		t.Fatal(err)
	}
	return back
}

func TestContradictionsAndUnknownsAreListed(t *testing.T) {
	root := t.TempDir()
	// A Dockerfile and a compose file that disagree about the container port: the scanner's own contradiction.
	rdyWrite(t, root, "Dockerfile", "FROM example.invalid/node@sha256:0000000000000000000000000000000000000000000000000000000000000000\nEXPOSE 3000\n")
	rdyWrite(t, root, "docker-compose.yml", "services:\n  app:\n    build: .\n    ports:\n      - \"127.0.0.1:8787:8787\"\n")
	// Two API descriptions that disagree about one operation.
	rdyWrite(t, root, "api/openapi.yaml", "openapi: 3.0.3\ninfo:\n  title: A\n  version: 1.0.0\npaths:\n  /slots:\n    get:\n      operationId: listSlots\n      responses:\n        \"200\":\n          description: ok\n")
	rdyWrite(t, root, "api/legacy.yaml", "openapi: 3.0.3\ninfo:\n  title: B\n  version: 1.0.0\npaths:\n  /slot-list:\n    post:\n      operationId: listSlots\n      responses:\n        \"200\":\n          description: ok\n")
	rdyWrite(t, root, "README.md", rdyReadme)
	rdyWrite(t, root, ".env", "API_PASSWORD="+rdyCanary+"\n")
	res, err := scan.ScanDir(root, scan.ScanOptions{Limits: scan.ScanLimits{MaxFiles: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("the fixture was meant to hit the file cap")
	}
	// Raise the cap again for the contradictions: a second scan without the cap.
	full, err := scan.ScanDir(root, scan.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := catalog.CatalogLoad()

	conflicts := RdyConflicts(full)
	if len(conflicts) != 1 || conflicts[0].RequirementID != "req.surface.control_surface" || len(conflicts[0].Evidence) != 2 {
		t.Fatalf("conflicts %+v", conflicts)
	}

	r := rdyBuild(t, root, full, func(in *RdyBuildInput) { in.ScannedRoot = "fixture\nwith `hostile` | name" })
	doc := r.Doc
	if raw, err := json.Marshal(doc); err != nil {
		t.Fatal(err)
	} else if msg := rdyValidate(t, "urn:alixai:schema:readiness:report:v1", raw); msg != "" {
		t.Fatalf("the report with contradictions does not validate: %s", msg)
	}
	if rdyStatusOf(t, doc, "req.deployment.compose_and_ports") != readiness.StatusContradicted {
		t.Errorf("the Dockerfile and compose disagreement is %s", rdyStatusOf(t, doc, "req.deployment.compose_and_ports"))
	}
	if rdyStatusOf(t, doc, "req.surface.control_surface") != readiness.StatusContradicted {
		t.Errorf("the API disagreement is %s", rdyStatusOf(t, doc, "req.surface.control_surface"))
	}
	if doc.Summary.Contradicted != 2 {
		t.Errorf("summary %+v", doc.Summary)
	}

	en, err := RdyRenderMarkdown(rdyRoundTrip(t, doc), cat, RdyLangEN)
	if err != nil {
		t.Fatal(err)
	}
	contra := en[strings.Index(en, "## Contradictions"):strings.Index(en, "## Could not be determined")]
	for _, want := range []string{"req.deployment.compose_and_ports", "req.surface.control_surface", "disagree about the operation listSlots", "Dockerfile", "docker-compose.yml"} {
		if !strings.Contains(contra, want) {
			t.Errorf("the contradictions section lacks %q:\n%s", want, contra)
		}
	}

	// Unknowns: the rule that cannot tell, and what the scan could not read.
	unknown := en[strings.Index(en, "## Could not be determined"):strings.Index(en, "## What the scan covered")]
	for _, it := range doc.Checklist {
		if it.Status == readiness.StatusUnverified && !strings.Contains(unknown, string(it.RequirementID)) {
			t.Errorf("the unverified requirement %s is not listed as undetermined", it.RequirementID)
		}
	}
	if doc.Scan.SecretFilesRecorded != 1 || !strings.Contains(en, "never read: 1") {
		t.Errorf("the secret file is not accounted for: %+v", doc.Scan)
	}
	if strings.Contains(en, rdyCanary) {
		t.Fatal("a secret value reached the report")
	}
	// A hostile directory name is neutralised.
	for _, line := range strings.Split(en, "\n") {
		if strings.Contains(line, "Scanned directory") && (strings.Contains(line, "\n") || strings.Count(line, "`") != 2) {
			t.Errorf("hostile directory name broke out of its code span: %q", line)
		}
	}

	// The truncated scan says its absence of findings is not proof.
	tr := rdyBuild(t, root, res, nil)
	if !tr.Doc.Scan.Truncated {
		t.Error("the report lost the truncation")
	}
	md, _ := RdyRenderMarkdown(tr.Doc, cat, RdyLangEN)
	if !strings.Contains(md, "absence of a finding is not evidence of absence") {
		t.Error("a truncated scan is not flagged in the report")
	}

	// A file changed after the scan turns what rested on it into an unknown, and says why.
	rdyWrite(t, root, "api/legacy.yaml", "openapi: 3.0.3\ninfo:\n  title: B2\n  version: 1.0.0\npaths:\n  /slot-list:\n    post:\n      operationId: listSlots\n      responses:\n        \"200\":\n          description: ok\n")
	changed := rdyBuild(t, root, full, nil)
	if st := rdyStatusOf(t, changed.Doc, "req.surface.control_surface"); st == readiness.StatusContradicted || st == readiness.StatusFound {
		t.Errorf("a requirement resting on a changed file is %s", st)
	}
	md, _ = RdyRenderMarkdown(changed.Doc, cat, RdyLangEN)
	unknown = md[strings.Index(md, "## Could not be determined"):]
	if !strings.Contains(unknown, "hash it had when it was scanned") {
		t.Errorf("the changed file is not explained:\n%s", unknown)
	}
}

var (
	rdySchemaOnce sync.Once
	rdySchemaComp *jsonschema.Compiler
	rdySchemaErr  error
)

type rdyRefuseLoader struct{}

func (rdyRefuseLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("remote schema loading is refused: %s", url)
}

func rdyValidate(t *testing.T, id string, raw []byte) string {
	t.Helper()
	rdySchemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		c.UseLoader(rdyRefuseLoader{})
		for _, area := range []string{"common", "readiness"} {
			files, err := filepath.Glob(filepath.Join("..", "..", "..", "schemas", area, "*.schema.json"))
			if err != nil || len(files) == 0 {
				rdySchemaErr = fmt.Errorf("no schemas under schemas/%s", area)
				return
			}
			for _, f := range files {
				b, err := os.ReadFile(f)
				if err != nil {
					rdySchemaErr = err
					return
				}
				doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
				if err != nil {
					rdySchemaErr = err
					return
				}
				sid, _ := doc.(map[string]any)["$id"].(string)
				if err := c.AddResource(sid, doc); err != nil {
					rdySchemaErr = err
					return
				}
			}
		}
		rdySchemaComp = c
	})
	if rdySchemaErr != nil {
		t.Fatalf("schemas: %v", rdySchemaErr)
	}
	sch, err := rdySchemaComp.Compile(id)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return "not JSON: " + err.Error()
	}
	if err := sch.Validate(inst); err != nil {
		return err.Error()
	}
	return ""
}

func TestBuiltReportsValidateAgainstTheSchemaAndAreDeterministic(t *testing.T) {
	root, res := rdyProject(t)
	cat, _ := catalog.CatalogLoad()
	_ = cat
	fixedID := common.TypedID("rrpt_01J0000000000000000000000A")
	for name, mut := range map[string]func(*RdyBuildInput){
		"no model": func(in *RdyBuildInput) { in.ReportID = fixedID },
		"model": func(in *RdyBuildInput) {
			in.ReportID = fixedID
			in.Model = rdyModel()
			in.Proposals = []verify.RdyProposal{
				rdyProposal(t, res, rdyReqLiftable, readiness.StatusFound, "GET /slots lists free slots."),
				rdyProposal(t, res, rdyReqNoRuleHit, readiness.StatusFound, "not in the file at all"),
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := rdyBuild(t, root, res, mut)
			raw, err := json.Marshal(a.Doc)
			if err != nil {
				t.Fatal(err)
			}
			if msg := rdyValidate(t, "urn:alixai:schema:readiness:report:v1", raw); msg != "" {
				t.Fatalf("the report does not validate: %s", msg)
			}
			if !a.Doc.Advisory || !bytes.Contains(raw, []byte(`"advisory":true`)) {
				t.Fatal("the report is not marked advisory")
			}
			b := rdyBuild(t, root, res, mut)
			rawB, _ := json.Marshal(b.Doc)
			if !bytes.Equal(raw, rawB) {
				t.Fatal("two builds of the same inputs differ")
			}
		})
	}

	// The inputs hash moves with the files, the catalog and the options, and with nothing else.
	base := rdyBuild(t, root, res, nil).Doc.InputsSHA256
	if again := rdyBuild(t, root, res, func(in *RdyBuildInput) { in.GeneratedAt = in.GeneratedAt.Add(time.Hour) }).Doc.InputsSHA256; again != base {
		t.Error("the inputs hash depends on the time")
	}
	lim := *res
	lim.Limits.MaxFiles++
	if rdyBuild(t, root, &lim, nil).Doc.InputsSHA256 == base {
		t.Error("the inputs hash ignores the scan options")
	}
	other := *res
	other.InputsSHA256 = strings.Repeat("d", 64)
	if rdyBuild(t, root, &other, nil).Doc.InputsSHA256 == base {
		t.Error("the inputs hash ignores the files")
	}
}

func TestBuildRefusesWhatItCannotStandBehind(t *testing.T) {
	root, res := rdyProject(t)
	g := ids.NewGenerator(rdyTestClock)
	ok := RdyBuildInput{Root: root, Scan: res, ReportID: common.TypedID(g.New(RdyReportIDPrefix)), GeneratedAt: rdyTestClock.Now()}
	for name, mut := range map[string]func(*RdyBuildInput){
		"no scan":                   func(in *RdyBuildInput) { in.Scan = nil },
		"no id":                     func(in *RdyBuildInput) { in.ReportID = "" },
		"proposals without model":   func(in *RdyBuildInput) { in.Proposals = []verify.RdyProposal{{}} },
		"a model without a runtime": func(in *RdyBuildInput) { in.Model = &RdyModel{ModelID: "m"} },
		"a catalog mismatch":        func(in *RdyBuildInput) { c := *res; c.CatalogSHA256 = strings.Repeat("e", 64); in.Scan = &c },
		"a drive root":              func(in *RdyBuildInput) { in.Root = filepath.VolumeName(root) + string(filepath.Separator) },
	} {
		t.Run(name, func(t *testing.T) {
			in := ok
			mut(&in)
			if _, err := RdyBuild(in); err == nil {
				t.Fatal("RdyBuild accepted it")
			}
		})
	}
}

func TestALongOperationNameCannotBreakTheSchemaLimit(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("a", 1500)
	api := func(path, verb string) string {
		return strings.Join([]string{"openapi: 3.0.3", "info:", "  title: A", "  version: 1.0.0", "paths:", "  /" + path + ":", "    " + verb + ":", "      operationId: " + long, "      responses:", "        \"200\":", "          description: ok"}, "\n") + "\n"
	}
	rdyWrite(t, root, "api/a.yaml", api(long+"1", "get"))
	rdyWrite(t, root, "api/b.yaml", api(long+"2", "post"))
	res, err := scan.ScanDir(root, scan.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	conflicts := RdyConflicts(res)
	if len(conflicts) == 0 {
		t.Skip("the scanner did not keep the long operation ids, so there is no conflict to bound")
	}
	for _, c := range conflicts {
		if n := len([]rune(string(*c.Summary))); n > 600 {
			t.Errorf("a conflict summary is %d characters", n)
		}
	}
	r := rdyBuild(t, root, res, nil)
	raw, _ := json.Marshal(r.Doc)
	if msg := rdyValidate(t, "urn:alixai:schema:readiness:report:v1", raw); msg != "" {
		t.Fatalf("the report does not validate: %s", msg)
	}
	md, err := RdyRenderMarkdownWith(r.Doc, nil, RdyLangIT, RdyRenderOptions{DroppedProposals: 2})
	if err != nil || !strings.Contains(md, "scartate") {
		t.Fatalf("the dropped count is not rendered: %v", err)
	}
}

// ---- the model leg (M8-04) ----

func TestModelLegIsRecordedValidatedAndRendered(t *testing.T) {
	root, res := rdyProject(t)
	digest := strings.Repeat("ab", 32)
	ran := func(in *RdyBuildInput) {
		in.ReportID = "rrpt_01J0000000000000000000000A"
		in.Model = rdyModel()
		in.ModelLeg = &RdyModelLeg{Status: readiness.ReportModelLegStatusRan, PromptTemplateSHA256: digest, Calls: 2, Received: 5, Dropped: 2, AttemptedOverrides: 1, BudgetExhausted: true}
		in.Proposals = []verify.RdyProposal{
			rdyProposal(t, res, rdyReqLiftable, readiness.StatusFound, "GET /slots lists free slots."),
			rdyProposal(t, res, rdyReqLiftable, readiness.StatusFound, "GET /slots lists free slots."), // a repeat: the verifier drops it
		}
	}
	r := rdyBuild(t, root, res, ran)
	leg := r.Doc.ModelLeg
	if leg == nil || leg.Status != readiness.ReportModelLegStatusRan || leg.Calls != 2 || leg.ProposalsReceived != 5 ||
		leg.AttemptedOverrides != 1 || leg.BudgetExhausted == nil || !*leg.BudgetExhausted || string(*leg.PromptTemplateSHA256) != digest {
		t.Fatalf("leg = %+v", leg)
	}
	// The verifier's own drop is added to the engine's.
	if leg.DroppedProposals != 2+int64(len(r.Dropped)) || len(r.Dropped) != 1 {
		t.Fatalf("dropped %d, verifier dropped %d", leg.DroppedProposals, len(r.Dropped))
	}
	if r.Doc.Model.Label == nil || *r.Doc.Model.Label != readiness.ReportModelLabelUnqualifiedAdvisory {
		t.Fatalf("label = %v", r.Doc.Model.Label)
	}
	raw, _ := json.Marshal(r.Doc)
	if msg := rdyValidate(t, "urn:alixai:schema:readiness:report:v1", raw); msg != "" {
		t.Fatalf("the report does not validate: %s", msg)
	}
	cat, _ := catalog.CatalogLoad()
	en, _ := RdyRenderMarkdown(r.Doc, cat, RdyLangEN)
	it, _ := RdyRenderMarkdown(r.Doc, cat, RdyLangIT)
	for _, want := range []string{"Model leg", "model calls", "tried to change or remove", "budget ran out", "dropped without becoming findings: 3"} {
		if !strings.Contains(en, want) {
			t.Errorf("English markdown lacks %q", want)
		}
	}
	for _, want := range []string{"Fase del modello", "chiamate al modello", "budget del modello"} {
		if !strings.Contains(it, want) {
			t.Errorf("Italian markdown lacks %q", want)
		}
	}

	// A leg that did not run says why, leaves the report deterministic_only and validates.
	for _, st := range []readiness.ReportModelLegStatus{readiness.ReportModelLegStatusNotConfigured, readiness.ReportModelLegStatusUnavailable, readiness.ReportModelLegStatusFailed, readiness.ReportModelLegStatusRefused} {
		d := rdyBuild(t, root, res, func(in *RdyBuildInput) {
			in.ReportID = "rrpt_01J0000000000000000000000A"
			in.ModelLeg = &RdyModelLeg{Status: st, Calls: 1}
		})
		if d.Doc.Mode != readiness.ReportModeDeterministicOnly || d.Doc.Model != nil || d.Doc.ModelLeg.Status != st {
			t.Fatalf("%s: mode %s model %v", st, d.Doc.Mode, d.Doc.Model)
		}
		raw, _ := json.Marshal(d.Doc)
		if msg := rdyValidate(t, "urn:alixai:schema:readiness:report:v1", raw); msg != "" {
			t.Fatalf("%s does not validate: %s", st, msg)
		}
		md, _ := RdyRenderMarkdown(d.Doc, cat, RdyLangEN)
		if !strings.Contains(md, "only the deterministic scan") {
			t.Errorf("%s: the markdown does not say only the deterministic scan ran", st)
		}
	}

	// A report built without a leg is the report it always was.
	plain := rdyBuild(t, root, res, nil)
	if plain.Doc.ModelLeg != nil {
		t.Fatal("a model leg appeared on its own")
	}
	if mdPlain, _ := RdyRenderMarkdown(plain.Doc, cat, RdyLangEN); strings.Contains(mdPlain, "Model leg") {
		t.Fatal("the markdown shows a leg the report does not have")
	}
}

func TestBuildRefusesAModelLegThatContradictsTheModel(t *testing.T) {
	root, res := rdyProject(t)
	g := ids.NewGenerator(rdyTestClock)
	digest := strings.Repeat("cd", 32)
	ok := RdyBuildInput{Root: root, Scan: res, ReportID: common.TypedID(g.New(RdyReportIDPrefix)), GeneratedAt: rdyTestClock.Now()}
	for name, mut := range map[string]func(*RdyBuildInput){
		"ran without a model": func(in *RdyBuildInput) {
			in.ModelLeg = &RdyModelLeg{Status: readiness.ReportModelLegStatusRan, PromptTemplateSHA256: digest}
		},
		"ran without a digest": func(in *RdyBuildInput) {
			in.Model = rdyModel()
			in.ModelLeg = &RdyModelLeg{Status: readiness.ReportModelLegStatusRan}
		},
		"failed with a model": func(in *RdyBuildInput) {
			in.Model = rdyModel()
			in.ModelLeg = &RdyModelLeg{Status: readiness.ReportModelLegStatusFailed}
		},
		"refused with proposals": func(in *RdyBuildInput) {
			in.Model = rdyModel()
			in.Proposals = []verify.RdyProposal{{}}
			in.ModelLeg = &RdyModelLeg{Status: readiness.ReportModelLegStatusRefused}
		},
		"no status": func(in *RdyBuildInput) { in.ModelLeg = &RdyModelLeg{} },
		"more overrides than drops": func(in *RdyBuildInput) {
			in.ModelLeg = &RdyModelLeg{Status: readiness.ReportModelLegStatusFailed, AttemptedOverrides: 2, Dropped: 1}
		},
		"a negative count": func(in *RdyBuildInput) {
			in.ModelLeg = &RdyModelLeg{Status: readiness.ReportModelLegStatusFailed, Received: -1}
		},
		"a digest that is not 64 digits": func(in *RdyBuildInput) {
			in.ModelLeg = &RdyModelLeg{Status: readiness.ReportModelLegStatusFailed, PromptTemplateSHA256: "abc"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := ok
			mut(&in)
			if _, err := RdyBuild(in); err == nil {
				t.Fatal("RdyBuild accepted it")
			}
		})
	}
}

func TestAModelLegThatMadeNoCallDoesNotClaimToHaveProposed(t *testing.T) {
	root, res := rdyProject(t)
	r := rdyBuild(t, root, res, func(in *RdyBuildInput) {
		in.Model = rdyModel()
		in.ModelLeg = &RdyModelLeg{Status: readiness.ReportModelLegStatusRan, PromptTemplateSHA256: strings.Repeat("ef", 32)}
	})
	cat, _ := catalog.CatalogLoad()
	en, _ := RdyRenderMarkdown(r.Doc, cat, RdyLangEN)
	if !strings.Contains(en, "it made no call") || strings.Contains(en, "a model proposed findings and the verifier checked each one") {
		t.Fatalf("the markdown misdescribes an idle model leg:\n%s", en)
	}
}

// A missing requirement reads as a sentence in both languages: the detector's phrase already
// carries its article, so the report must never say "No an ..." or "No a ...".
func TestMissingSentencesAreGrammatical(t *testing.T) {
	root, res := rdyProject(t)
	r := rdyBuild(t, root, res, nil)
	cat, _ := catalog.CatalogLoad()
	for _, lang := range []RdyLang{RdyLangEN, RdyLangIT} {
		md, err := RdyRenderMarkdown(r.Doc, cat, lang)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(md, "\n") {
			if strings.HasPrefix(line, "- No a ") || strings.HasPrefix(line, "- No an ") {
				t.Errorf("%s: ungrammatical missing line %q", lang, line)
			}
		}
		if want := "- Not found: an MCP server configuration or entry point."; !strings.Contains(md, want) {
			t.Errorf("%s: the report lacks %q", lang, want)
		}
	}
}

// An unverified finding whose summary is its reason prints the sentence once, in both languages.
func TestAnUnverifiedReasonIsNotPrintedTwice(t *testing.T) {
	root := filepath.Join("..", "testdata", "clinic")
	res, err := scan.ScanDir(root, scan.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := rdyBuild(t, root, res, nil)
	if st := rdyStatusOf(t, r.Doc, "req.rag.live_sources"); st != readiness.StatusUnverified {
		t.Fatalf("req.rag.live_sources is %s, want unverified for this test", st)
	}
	cat, _ := catalog.CatalogLoad()
	for _, lang := range []RdyLang{RdyLangEN, RdyLangIT} {
		md, err := RdyRenderMarkdown(rdyRoundTrip(t, r.Doc), cat, lang)
		if err != nil {
			t.Fatal(err)
		}
		const reason = "Whether the declared sources are reachable is verified by the integration audit, not by readiness discovery."
		if n := strings.Count(md, reason); n != 1 {
			t.Errorf("%s: the reason appears %d times, want once", lang, n)
		}
		lines := strings.Split(md, "\n")
		for i := 1; i < len(lines); i++ {
			if strings.HasPrefix(lines[i], "- ") && lines[i] == lines[i-1] {
				t.Errorf("%s: line printed twice in a row: %q", lang, lines[i])
			}
		}
	}
}

// rdySection is the markdown section under the heading, up to the next one.
func rdySection(md, heading string) string {
	i := strings.Index(md, "\n## "+heading+"\n")
	if i < 0 {
		return ""
	}
	rest := md[i+len(heading)+5:]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// MCP, OpenAPI and a native adapter are alternative transports: when one is met, the requirements of
// the others are not applicable, never missing. When none is met, all of them stay missing.
func TestAlternativeTransportsAreNotApplicableOnceOneIsMet(t *testing.T) {
	others := []readiness.RequirementID{"req.mcp.server", "req.mcp.tool_list", "req.native.adapter"}
	cat, _ := catalog.CatalogLoad()

	root, res := rdyProject(t)
	none := rdyBuild(t, root, res, nil).Doc
	for _, id := range append(others, "req.openapi.description") {
		if st := rdyStatusOf(t, none, id); st != readiness.StatusMissing {
			t.Errorf("with no transport %s is %s, want missing", id, st)
		}
	}
	if none.Summary.NotApplicable != nil {
		t.Errorf("with no transport the summary counts %d not applicable", *none.Summary.NotApplicable)
	}
	if md, _ := RdyRenderMarkdown(none, cat, RdyLangEN); !strings.Contains(rdySection(md, "Missing, and how to fix it"), "or choose OpenAPI or a native adapter") {
		t.Error("with no transport the missing section lost the choose-one fix")
	}

	rdyWrite(t, root, "api/openapi.yaml", "openapi: 3.0.3\ninfo:\n  title: A\n  version: 1.0.0\npaths:\n  /slots:\n    get:\n      operationId: listSlots\n      responses:\n        \"200\":\n          description: ok\n")
	res, err := scan.ScanDir(root, scan.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	doc := rdyRoundTrip(t, rdyBuild(t, root, res, nil).Doc)
	if st := rdyStatusOf(t, doc, "req.openapi.description"); st != readiness.StatusFound {
		t.Fatalf("req.openapi.description is %s, want found", st)
	}
	missing := 0
	for _, it := range doc.Checklist {
		if it.Status == readiness.StatusMissing {
			missing++
		}
	}
	for _, id := range others {
		if st := rdyStatusOf(t, doc, id); st != readiness.StatusNotApplicable {
			t.Errorf("with OpenAPI met %s is %s, want not_applicable", id, st)
		}
	}
	if doc.Summary.NotApplicable == nil || *doc.Summary.NotApplicable != int64(len(others)) || doc.Summary.Missing != int64(missing) {
		t.Errorf("summary %+v, want %d not applicable and %d missing", doc.Summary, len(others), missing)
	}
	if raw, err := json.Marshal(doc); err != nil {
		t.Fatal(err)
	} else if msg := rdyValidate(t, "urn:alixai:schema:readiness:report:v1", raw); msg != "" {
		t.Fatalf("the report does not validate: %s", msg)
	}
	for _, c := range []struct {
		lang             RdyLang
		missing, notAppl string
	}{{RdyLangEN, "Missing, and how to fix it", "Not applicable"}, {RdyLangIT, "Mancante, e come rimediare", "Non applicabile"}} {
		md, err := RdyRenderMarkdown(doc, cat, c.lang)
		if err != nil {
			t.Fatal(err)
		}
		miss, na := rdySection(md, c.missing), rdySection(md, c.notAppl)
		for _, id := range others {
			if strings.Contains(miss, string(id)) {
				t.Errorf("%s: %s is listed as missing", c.lang, id)
			}
			if !strings.Contains(na, string(id)) {
				t.Errorf("%s: %s is not listed as not applicable:\n%s", c.lang, id, na)
			}
		}
		if !strings.Contains(na, "req.openapi.description") {
			t.Errorf("%s: the not-applicable section does not name the transport in use:\n%s", c.lang, na)
		}
	}
}
