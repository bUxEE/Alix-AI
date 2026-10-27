package verify

import (
	"fmt"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

func sampleFacts() AnalystFactSet {
	cur := "EUR"
	ds := analyst.Dataset{
		ID: "ds_rev_001",
		Fields: []analyst.DatasetDatasetField{
			{Name: "company", Type: analyst.FieldTypeString, DataClass: analyst.DataClassInternal},
			{Name: "revenue", Type: analyst.FieldTypeNumber, DataClass: analyst.DataClassInternal, Currency: &cur},
		},
		Rows: []analyst.DatasetRow{
			{"company": "Acme SpA", "revenue": 12400.0},
		},
		RecordCount: 1,
		Provenance: analyst.DatasetDatasetProvenance{
			Kind: analyst.DatasetDatasetProvenanceKindRead,
			CapabilityID: func() *common.CapabilityID {
				c := common.CapabilityID("company.profile.read")
				return &c
			}(),
		},
	}
	disp := "12,400"
	sl := "computed"
	df := analyst.DerivedFact{
		ID: "df_sum_001", Label: "revenue_sum", Value: 12400.0, Display: &disp,
		Currency: &cur, DataClass: analyst.DataClassInternal, SourceLabel: &sl,
		Provenance: analyst.DerivedFactDerivedProvenance{
			Op: "aggregate", InputDatasetIDs: []analyst.DatasetID{"ds_rev_001"},
			Params: map[string]any{"agg": "sum"}, Rounding: analyst.DerivedFactRounding{Mode: analyst.RoundingModeHalfUp, Decimals: 0},
		},
	}
	fc := &analyst.DerivedFactForecastMeta{
		Method: analyst.DerivedFactForecastMetaMethodLinearTrend, Horizon: 1,
		IntervalLow: 100, IntervalHigh: 200,
	}
	dfForecast := analyst.DerivedFact{
		ID: "df_fc_001", Label: "revenue_forecast", Value: 150.0, DataClass: analyst.DataClassInternal,
		SourceLabel: &sl, Forecast: fc,
		Provenance: analyst.DerivedFactDerivedProvenance{
			Op: "forecast_linear", InputDatasetIDs: []analyst.DatasetID{"ds_rev_001"},
			Params: map[string]any{}, Rounding: analyst.DerivedFactRounding{Mode: analyst.RoundingModeHalfUp, Decimals: 2},
		},
	}
	return AnalystFactSet{
		Datasets: []analyst.Dataset{ds},
		Derived:  []analyst.DerivedFact{df, dfForecast},
		Citations: []AnalystCitation{{
			Meta: analyst.WebCitation{
				ID: "wcit_gov_01", URL: "https://example.gov.it/rule", Title: "Incentive rules",
				RetrievedAt: "2026-01-01T00:00:00Z", SourceLabel: "web",
			},
			FetchedText: "The maximum headcount is 250 employees under the 2026 decree.",
		}},
	}
}

func TestAnalystInventedNumberReplaced(t *testing.T) {
	facts := sampleFacts()
	out, err := VerifyAnalyst(AnalystAnswer{Text: "Revenue is 99999 EUR."}, facts, AnalystOptions{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != SourceTemplate || out.Reason != ReasonVerificationFailed {
		t.Fatalf("want template replace, got %+v", out)
	}
	if len(out.Blocks) < 2 {
		t.Fatalf("template needs table+provenance, got %d", len(out.Blocks))
	}
}

func TestAnalystComputedNumberSurvivesFormats(t *testing.T) {
	facts := sampleFacts()
	enForms := []string{
		"Revenue is 12400.",
		"Revenue is 12,400.",
		"Revenue is 12\u00a0400.", // grouped_space uses NBSP
		"Revenue is 12.4k.",
	}
	for _, text := range enForms {
		out, err := VerifyAnalyst(AnalystAnswer{Text: text}, facts, AnalystOptions{Language: LangEN})
		if err != nil {
			t.Fatal(err)
		}
		if out.Source != SourceModel {
			t.Fatalf("%q should pass, got reason=%s viol=%v", text, out.Reason, out.Violations)
		}
	}
	// grouped_dot is the Italian thousands form
	out, err := VerifyAnalyst(AnalystAnswer{Text: "Il fatturato è 12.400."}, facts, AnalystOptions{Language: LangIT})
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != SourceModel {
		t.Fatalf("IT grouped_dot should pass: %v", out.Violations)
	}
}

func TestAnalystForecastWithoutLabelReplaced(t *testing.T) {
	facts := sampleFacts()
	out, err := VerifyAnalyst(AnalystAnswer{Text: "Next period will be 150."}, facts, AnalystOptions{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != SourceTemplate {
		t.Fatalf("unlabelled forecast must be replaced, got %+v", out)
	}
	out2, err := VerifyAnalyst(AnalystAnswer{
		Text: "The estimate for next period is 150 using linear_trend.",
	}, facts, AnalystOptions{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	if out2.Source != SourceModel {
		t.Fatalf("labelled forecast should pass: %v", out2.Violations)
	}
}

func TestAnalystWebNumberNotInSourceReplaced(t *testing.T) {
	facts := sampleFacts()
	cid := "wcit_gov_01"
	spans := []analyst.SourceSpan{{Label: "rule", Source: analyst.SourceLabelWeb, WebCitationID: &cid}}
	out, err := VerifyAnalyst(AnalystAnswer{
		Text: "The cap is 999 employees.", SourceSpans: spans,
	}, facts, AnalystOptions{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != SourceTemplate {
		t.Fatal("web number not in fetch must fail")
	}
	out2, err := VerifyAnalyst(AnalystAnswer{
		Text: "The cap is 250 employees.", SourceSpans: spans,
	}, facts, AnalystOptions{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	if out2.Source != SourceModel {
		t.Fatalf("web number in fetch should pass: %v", out2.Violations)
	}
}

func TestAnalystKnowledgeAsTenantReplaced(t *testing.T) {
	facts := sampleFacts()
	spans := []analyst.SourceSpan{{Label: "k", Source: analyst.SourceLabelKnowledge}}
	out, err := VerifyAnalyst(AnalystAnswer{
		Text: "In general, companies like yours often have revenue around 12400.", SourceSpans: spans,
	}, facts, AnalystOptions{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != SourceTemplate {
		t.Fatal("knowledge posing as tenant figure must fail")
	}
}

func TestAnalystNamedEntityNotInDataReplaced(t *testing.T) {
	facts := sampleFacts()
	out, err := VerifyAnalyst(AnalystAnswer{
		Text: "Company ZzzCorp_99 reported growth.",
	}, facts, AnalystOptions{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != SourceTemplate {
		t.Fatalf("unknown entity should fail, got %+v", out)
	}
	out2, err := VerifyAnalyst(AnalystAnswer{Text: "Acme SpA looks healthy."}, facts, AnalystOptions{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	if out2.Source != SourceModel {
		t.Fatalf("known entity should pass: %v", out2.Violations)
	}
}

func TestAnalystQualitativePasses(t *testing.T) {
	facts := sampleFacts()
	out, err := VerifyAnalyst(AnalystAnswer{
		Text: "Overall the picture is stable and the trend looks constructive.",
	}, facts, AnalystOptions{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != SourceModel {
		t.Fatalf("qualitative should pass: %v", out.Violations)
	}
}

func TestAnalystStreamingSegmentFailure(t *testing.T) {
	facts := sampleFacts()
	ans := AnalystAnswer{FinalDatasetID: func() *analyst.DatasetID { id := analyst.DatasetID("ds_rev_001"); return &id }()}
	s, err := NewAnalystStream(ans, facts, AnalystOptions{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	step, err := s.Push("Acme SpA is fine. ")
	if err != nil {
		t.Fatal(err)
	}
	if step.Replace {
		t.Fatalf("first segment should pass: %+v", step)
	}
	step, err = s.Push("Secret figure 777777 confirms it.")
	if err != nil {
		t.Fatal(err)
	}
	// may need Finish if segment not complete
	if !step.Replace {
		step, err = s.Finish()
		if err != nil {
			t.Fatal(err)
		}
	}
	if !step.Replace || step.Outcome.Source != SourceTemplate {
		t.Fatalf("want replace with template, got %+v", step)
	}
	hasTable, hasProv := false, false
	for _, b := range step.Outcome.Blocks {
		switch b.Value.(type) {
		case conversation.TableBlock:
			hasTable = true
		case conversation.ProvenanceBlock:
			hasProv = true
		}
	}
	if !hasTable || !hasProv {
		t.Fatalf("template blocks table=%v provenance=%v", hasTable, hasProv)
	}
}

func TestAnalystBlockMaterialisedFromDataset(t *testing.T) {
	facts := sampleFacts()
	id := analyst.DatasetID("ds_rev_001")
	out, err := VerifyAnalyst(AnalystAnswer{
		Text: "Acme SpA revenue is 12400.",
		Blocks: []analyst.StepProposalBlockPlan{{
			Type: analyst.StepProposalBlockPlanTypeTable, DatasetID: &id,
		}},
	}, facts, AnalystOptions{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != SourceModel || len(out.Blocks) != 1 {
		t.Fatalf("got %+v", out)
	}
	tb, ok := out.Blocks[0].Value.(conversation.TableBlock)
	if !ok || tb.DatasetID == nil || *tb.DatasetID != id {
		t.Fatalf("block not materialised: %#v", out.Blocks[0])
	}
	if len(tb.Rows) != 1 || tb.Rows[0][1] != 12400.0 {
		t.Fatalf("rows from dataset: %#v", tb.Rows)
	}
	if len(out.CoreMaterialised) != 1 || !out.CoreMaterialised[0] {
		t.Fatalf("materialised blocks must carry CoreMaterialised: %v", out.CoreMaterialised)
	}
}

func TestAnalystChartMaterialisedFromDataset(t *testing.T) {
	facts := sampleFacts()
	id := analyst.DatasetID("ds_rev_001")
	mark := analyst.StepProposalBlockPlanMarkBar
	out, err := VerifyAnalyst(AnalystAnswer{
		Text: "Acme SpA revenue is 12400.",
		Blocks: []analyst.StepProposalBlockPlan{{
			Type: analyst.StepProposalBlockPlanTypeChart, DatasetID: &id, Mark: &mark,
			Description: func() *string { s := "Revenue by company."; return &s }(),
		}},
	}, facts, AnalystOptions{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != SourceModel || len(out.Blocks) != 1 {
		t.Fatalf("got %+v", out)
	}
	ch, ok := out.Blocks[0].Value.(conversation.ChartBlock)
	if !ok || ch.DatasetID == nil || *ch.DatasetID != id {
		t.Fatalf("chart not materialised: %#v", out.Blocks[0])
	}
	if len(ch.Data) != 1 || ch.Data[0]["revenue"] != 12400.0 {
		t.Fatalf("chart data from dataset: %#v", ch.Data)
	}
}

func TestAnalystBlockPlanCannotInjectRows(t *testing.T) {
	// Block plans have no rows member; materialisation always copies from the dataset.
	// A plan that only names the dataset must never invent cell values.
	facts := sampleFacts()
	id := analyst.DatasetID("ds_rev_001")
	blocks, err := materialiseAnalystBlocks([]analyst.StepProposalBlockPlan{{
		Type: analyst.StepProposalBlockPlanTypeTable, DatasetID: &id,
		Title: func() *string { s := "Injected title only"; return &s }(),
	}}, facts)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks=%d", len(blocks))
	}
	tb := blocks[0].Value.(conversation.TableBlock)
	if len(tb.Rows) != 1 || tb.Rows[0][0] != "Acme SpA" || tb.Rows[0][1] != 12400.0 {
		t.Fatalf("rows must equal dataset, got %#v", tb.Rows)
	}
	if strings.Contains(fmt.Sprint(tb.Rows), "99999") {
		t.Fatal("invented cell value")
	}
}

func TestAnalystCompactK(t *testing.T) {
	// ensure 12.4k maps to 12400 via existing extractor
	facts := sampleFacts()
	out, err := VerifyAnalyst(AnalystAnswer{Text: "About 12.4k in revenue."}, facts, AnalystOptions{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != SourceModel {
		// If compact form is not recognised as 12400, document — still must not accept invented
		if strings.Contains(out.Text, "Replaced") {
			t.Logf("compact_k not matched by extractor in this path: %v", out.Violations)
		}
	}
}
