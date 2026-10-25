package report

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/readiness/catalog"
	"github.com/buxee/alix-ai/internal/readiness/scan"
	"github.com/buxee/alix-ai/internal/readiness/verify"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

// RdyReportIDPrefix is the typed-ID prefix of a readiness report (2 to 8 lowercase letters).
const RdyReportIDPrefix = "rrpt"

// RdyNewReportID mints a report id from the injected generator.
func RdyNewReportID(g *ids.Generator) common.TypedID {
	return common.TypedID(g.New(RdyReportIDPrefix))
}

// RdyModel describes the model that proposed findings. A model counts as qualified only when the
// caller says the registry shows it so; the zero value is unqualified.
type RdyModel struct {
	ModelID   string
	Runtime   readiness.ReportModelRuntime
	Qualified bool
}

// RdyModelLeg is what the assist leg reports about itself (ADR 0037). Status must be set. The
// counts are the assist engine's own; the builder adds the proposals the verifier refused to
// Dropped, so the report holds one total.
type RdyModelLeg struct {
	Status readiness.ReportModelLegStatus
	// PromptTemplateSHA256 is the digest of the fixed prompt template and output schema. It is
	// required when the leg ran and may be empty when no prompt was built.
	PromptTemplateSHA256 string
	Calls                int
	// Received counts the proposals the model made, one per requirement it named.
	Received int
	// Dropped counts the proposals the engine refused before the verifier saw them.
	Dropped int
	// AttemptedOverrides is the part of Dropped that tried to do more than add a finding.
	AttemptedOverrides int
	BudgetExhausted    bool
}

// RdyBuildInput is one report to build from one scan.
type RdyBuildInput struct {
	// Root is the directory that was scanned.
	Root string
	// ScannedRoot is how the operator wrote it, for the report; Root is used when empty.
	ScannedRoot string
	Scan        *scan.ScanResult
	// Proposals are the model's findings, each with the text it quotes. They need Model.
	Proposals []verify.RdyProposal
	// Model is nil when no model ran: the report then says it is deterministic only.
	Model *RdyModel
	// ModelLeg, when set, is recorded in the report together with the model label. It must agree
	// with Model: a leg that ran has a model and may have proposals, any other leg has neither.
	ModelLeg *RdyModelLeg

	ReportID         common.TypedID
	GeneratedAt      time.Time
	PreviousReportID common.TypedID
	// Catalog is the catalog the scan used; nil loads the embedded one.
	Catalog *catalog.Catalog
}

// RdyReport is a built report: the schema document that is stored and exchanged, the verifier's
// drops (not part of the document), and the decision data the document is derived from.
type RdyReport struct {
	Doc readiness.Report
	// Dropped are model proposals the verifier did not turn into findings, by reason.
	Dropped []verify.RdyDropped
}

// RdyBuild verifies the scan's findings (and any model proposals), evaluates the checklist and
// returns the report. It re-reads every cited file itself: there is no way to build a report from
// findings that have not been through the verifier.
func RdyBuild(in RdyBuildInput) (*RdyReport, error) {
	if in.Scan == nil {
		return nil, errors.New("readiness report: no scan result")
	}
	cat := in.Catalog
	if cat == nil {
		var err error
		if cat, err = catalog.CatalogLoad(); err != nil {
			return nil, fmt.Errorf("readiness report: %w", err)
		}
	}
	if in.Scan.CatalogSHA256 != cat.SHA256() {
		return nil, errors.New("readiness report: the scan was made with a different catalog")
	}
	if in.ReportID == "" || in.GeneratedAt.IsZero() {
		return nil, errors.New("readiness report: a report id and a time are required")
	}
	if len(in.Proposals) > 0 && in.Model == nil {
		return nil, errors.New("readiness report: model proposals were given without the model that made them")
	}
	if in.Model != nil && (in.Model.ModelID == "" || len(in.Model.ModelID) > 128 || !in.Model.Runtime.Valid() ||
		strings.ContainsAny(in.Model.ModelID, " \t\r\n*") || !utf8.ValidString(in.Model.ModelID)) {
		return nil, errors.New("readiness report: the model is not described by a valid id and runtime")
	}
	if err := rdyCheckLeg(in); err != nil {
		return nil, err
	}
	ev, err := RdyNewEvaluator(cat)
	if err != nil {
		return nil, err
	}
	v, err := verify.RdyNew(in.Root, in.Scan, cat)
	if err != nil {
		return nil, err
	}
	defer v.Close()
	main, err := v.Verify(in.Scan.Findings, in.Proposals)
	if err != nil {
		return nil, err
	}
	conf, err := v.Verify(RdyConflicts(in.Scan), nil)
	if err != nil {
		return nil, err
	}
	var evalIn RdyEvalInput
	for _, f := range main.Verified {
		if f.FromModel() {
			evalIn.Model = append(evalIn.Model, f)
		} else {
			evalIn.Rule = append(evalIn.Rule, f)
		}
	}
	evalIn.Conflicts = conf.Verified
	res, err := ev.Evaluate(evalIn)
	if err != nil {
		return nil, err
	}
	if len(res.Findings) > 5000 {
		return nil, errors.New("readiness report: too many findings for one report")
	}

	doc := readiness.Report{
		SchemaVersion:  1,
		ReportID:       in.ReportID,
		Advisory:       true,
		GeneratedAt:    common.Timestamp(in.GeneratedAt.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")),
		ScannedRoot:    rdyDisplayRoot(firstNonEmpty(in.ScannedRoot, in.Root)),
		InputsSHA256:   readiness.SHA256(rdyInputsHash(in.Scan, cat)),
		CatalogVersion: int64(in.Scan.CatalogVersion),
		CatalogSHA256:  readiness.SHA256(cat.SHA256()),
		Mode:           readiness.ReportModeDeterministicOnly,
		Scan:           rdyScanStats(in.Scan),
		Checklist:      res.Checklist,
		Summary:        res.Summary,
		Findings:       res.Findings,
	}
	rules := int64(RdyEvaluatorVersion)
	doc.EvaluatorVersion = &rules
	if in.PreviousReportID != "" {
		p := in.PreviousReportID
		doc.PreviousReportID = &p
	}
	if in.Model != nil {
		q := readiness.ReportModelQualificationUnqualified
		if in.Model.Qualified {
			q = readiness.ReportModelQualificationQualified
		}
		doc.Mode = readiness.ReportModeDeterministicAndModel
		doc.Model = &readiness.ReportModel{ModelID: common.Identifier(in.Model.ModelID), Runtime: in.Model.Runtime, Qualification: q}
		if in.ModelLeg != nil {
			l := RdyModelLabel(q)
			doc.Model.Label = &l
		}
	}
	if in.ModelLeg != nil {
		leg := &readiness.ReportModelLeg{
			Status:             in.ModelLeg.Status,
			Calls:              int64(in.ModelLeg.Calls),
			ProposalsReceived:  int64(in.ModelLeg.Received),
			DroppedProposals:   int64(in.ModelLeg.Dropped + len(main.Dropped)),
			AttemptedOverrides: int64(in.ModelLeg.AttemptedOverrides),
		}
		if in.ModelLeg.PromptTemplateSHA256 != "" {
			d := readiness.SHA256(in.ModelLeg.PromptTemplateSHA256)
			leg.PromptTemplateSHA256 = &d
		}
		if in.ModelLeg.BudgetExhausted {
			t := true
			leg.BudgetExhausted = &t
		}
		doc.ModelLeg = leg
	}
	return &RdyReport{Doc: doc, Dropped: main.Dropped}, nil
}

// RdyModelLabel is the label a model carries in the report: unqualified unless the registry
// showed a qualified status, and advisory either way.
func RdyModelLabel(q readiness.ReportModelQualification) readiness.ReportModelLabel {
	if q == readiness.ReportModelQualificationQualified {
		return readiness.ReportModelLabelQualifiedAdvisory
	}
	return readiness.ReportModelLabelUnqualifiedAdvisory
}

var rdyHexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// rdyCheckLeg refuses a model leg that contradicts the model it comes with: a leg that did not
// run cannot carry a model or proposals, and a leg that ran needs the model and the template
// digest.
func rdyCheckLeg(in RdyBuildInput) error {
	l := in.ModelLeg
	if l == nil {
		return nil
	}
	switch {
	case !l.Status.Valid():
		return errors.New("readiness report: the model leg has no valid status")
	case l.Calls < 0 || l.Calls > 64 || l.Received < 0 || l.Dropped < 0 || l.AttemptedOverrides < 0 || l.AttemptedOverrides > l.Dropped:
		return errors.New("readiness report: the model leg counts are out of range")
	case l.PromptTemplateSHA256 != "" && !rdyHexDigest.MatchString(l.PromptTemplateSHA256):
		return errors.New("readiness report: the prompt template digest is not 64 hex digits")
	case l.Status == readiness.ReportModelLegStatusRan && (in.Model == nil || l.PromptTemplateSHA256 == ""):
		return errors.New("readiness report: a model leg that ran needs its model and its template digest")
	case l.Status != readiness.ReportModelLegStatusRan && (in.Model != nil || len(in.Proposals) > 0):
		return errors.New("readiness report: a model leg that did not run cannot carry a model or proposals")
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// rdyDisplayRoot keeps a hostile directory name out of the report: control characters become
// question marks and the length is bounded.
func rdyDisplayRoot(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			r = '?'
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) > 1024 {
		out = out[:1024]
		for !utf8.ValidString(out) {
			out = out[:len(out)-1]
		}
	}
	if out == "" {
		out = "?"
	}
	return out
}

// RdyEvaluatorVersion is the version of the readiness rules: the scan's detectors and deciders, the
// verifier and the checklist evaluator. Bump it whenever a release changes how a requirement is
// evaluated, so that a stored report made by the older rules is no longer current. A report that
// does not record it was made by version 1.
const RdyEvaluatorVersion = 2

// RdyReportEvaluatorVersion is the rules version a report was made by: 1 when it does not say.
func RdyReportEvaluatorVersion(doc readiness.Report) int64 {
	if doc.EvaluatorVersion == nil {
		return 1
	}
	return *doc.EvaluatorVersion
}

// rdyInputsHash is the hash the report records for its inputs: the scanned files (their paths
// and hashes), the catalog, the scan options and the rules version, so an unchanged project,
// catalog, option set and rules give the same value and a change in any of them gives another.
func rdyInputsHash(res *scan.ScanResult, cat *catalog.Catalog) string {
	lim, _ := json.Marshal(res.Limits)
	h := sha256.New()
	fmt.Fprintf(h, "files:%s\ncatalog:%d:%s\nlimits:%s\nrules:%d\n", res.InputsSHA256, res.CatalogVersion, cat.SHA256(), lim, RdyEvaluatorVersion)
	return hex.EncodeToString(h.Sum(nil))
}

func rdyScanStats(res *scan.ScanResult) readiness.ReportScanStats {
	var st readiness.ReportScanStats
	for _, f := range res.Files {
		if f.SHA256 != "" {
			st.FilesScanned++
			st.BytesScanned += f.Size
		}
	}
	skipped := map[string]bool{}
	for _, s := range res.Skipped {
		skipped[s.Path] = true
	}
	st.FilesSkipped = int64(len(skipped))
	st.SecretFilesRecorded = int64(len(res.SecretFiles))
	for _, e := range res.Excerpts {
		st.ValuesRedacted += int64(strings.Count(e.Text, scan.ScanRedacted))
	}
	st.Truncated = res.Truncated
	return st
}
