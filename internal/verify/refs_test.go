package verify

import (
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

func refFacts() AnalystFactSet {
	cur := "EUR"
	return AnalystFactSet{
		Datasets: []analyst.Dataset{{
			ID: "ds_att_001",
			Fields: []analyst.DatasetDatasetField{
				{Name: "attendee_name", Type: analyst.FieldTypeString, DataClass: analyst.DataClassConfidential},
				{Name: "seat", Type: analyst.FieldTypeNumber, DataClass: analyst.DataClassInternal},
				{Name: "fee", Type: analyst.FieldTypeNumber, DataClass: analyst.DataClassInternal, Currency: &cur},
				{Name: "joined_on", Type: analyst.FieldTypeDate, DataClass: analyst.DataClassInternal},
			},
			Rows: []analyst.DatasetRow{
				{"attendee_name": "Maria Rossi", "seat": 12.0, "fee": 1500.5, "joined_on": "2026-03-14"},
				{"attendee_name": "Luca Bianchi", "seat": 3.0, "fee": 200.0, "joined_on": "2026-04-01"},
			},
			RecordCount: 2,
		}},
	}
}

func TestParseRefTokens(t *testing.T) {
	toks := ParseRefTokens("Hello {{ref:ds_att_001:0:attendee_name}} and {{ref:ds_att_001:1:seat}}.")
	if len(toks) != 2 {
		t.Fatalf("got %d tokens", len(toks))
	}
	if toks[0].DatasetID != "ds_att_001" || toks[0].Row != 0 || toks[0].Field != "attendee_name" {
		t.Fatalf("tok0 = %+v", toks[0])
	}
	if toks[1].Row != 1 || toks[1].Field != "seat" {
		t.Fatalf("tok1 = %+v", toks[1])
	}
}

func TestCheckAnalystRefsInvalid(t *testing.T) {
	facts := refFacts()
	cases := []string{
		"{{ref:ds_missing:0:attendee_name}}",
		"{{ref:ds_att_001:9:attendee_name}}",
		"{{ref:ds_att_001:0:no_such}}",
		"{{ref:not_a_ds:0:attendee_name}}",
		"{{ref:ds_att_001:attendee_name}}",
		"{{ref:ds_att_001:0:attendee_name",
	}
	for _, text := range cases {
		res := CheckAnalystRefs(text, facts)
		if res.OK {
			t.Fatalf("%q should fail", text)
		}
	}
}

func TestResolveAnalystRefsFormatting(t *testing.T) {
	facts := refFacts()
	en, err := ResolveAnalystRefs(
		"Attendee {{ref:ds_att_001:0:attendee_name}} seat {{ref:ds_att_001:0:seat}} fee {{ref:ds_att_001:0:fee}} on {{ref:ds_att_001:0:joined_on}}.",
		facts, LangEN)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(en, "Maria Rossi") || !strings.Contains(en, "12") || !strings.Contains(en, "1500.5 EUR") {
		t.Fatalf("en resolve = %q", en)
	}
	if !strings.Contains(en, "14 March 2026") {
		t.Fatalf("en date = %q", en)
	}
	it, err := ResolveAnalystRefs(
		"Quota {{ref:ds_att_001:0:fee}} il {{ref:ds_att_001:0:joined_on}}.",
		facts, LangIT)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(it, "1500,5 EUR") {
		t.Fatalf("it money = %q", it)
	}
	if !strings.Contains(it, "14 marzo 2026") {
		t.Fatalf("it date = %q", it)
	}
}

func TestVerifyAnalystRefTokenPassesAndKeepsModelText(t *testing.T) {
	facts := refFacts()
	plan := "The assembly was attended by {{ref:ds_att_001:0:attendee_name}}."
	out, err := VerifyAnalyst(AnalystAnswer{Text: plan}, facts, AnalystOptions{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != SourceModel || out.ModelText != plan {
		t.Fatalf("want model text with tokens, got source=%s model=%q text=%q", out.Source, out.ModelText, out.Text)
	}
	resolved, err := ResolveAnalystRefs(out.ModelText, facts, LangEN)
	if err != nil || !strings.Contains(resolved, "Maria Rossi") || strings.Contains(resolved, "{{ref:") {
		t.Fatalf("resolve = %q err=%v", resolved, err)
	}
}

func TestVerifyAnalystBadRefReplaced(t *testing.T) {
	facts := refFacts()
	out, err := VerifyAnalyst(AnalystAnswer{Text: "Hi {{ref:ds_att_001:99:attendee_name}}."}, facts, AnalystOptions{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != SourceTemplate {
		t.Fatalf("want template, got %+v", out)
	}
}

func TestVerifyAnalystRefInTitleReplaced(t *testing.T) {
	facts := refFacts()
	title := "{{ref:ds_att_001:0:attendee_name}}"
	id := analyst.DatasetID("ds_att_001")
	out, err := VerifyAnalyst(AnalystAnswer{
		Text: "Ok.",
		Blocks: []analyst.StepProposalBlockPlan{{
			Type: analyst.StepProposalBlockPlanTypeTable, DatasetID: &id, Title: &title,
		}},
	}, facts, AnalystOptions{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != SourceTemplate {
		t.Fatalf("title ref must fail, got %+v", out)
	}
}

func TestAnalystStreamRefSplitAcrossSegments(t *testing.T) {
	facts := refFacts()
	s, err := NewAnalystStream(AnalystAnswer{}, facts, AnalystOptions{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	// Split the token across pushes so a half-token must not be emitted.
	step, err := s.Push("Attendee {{ref:ds_att_001:0:attend")
	if err != nil {
		t.Fatal(err)
	}
	if len(step.Segments) != 0 {
		t.Fatalf("incomplete token must not emit: %v", step.Segments)
	}
	step, err = s.Push("ee_name}}.")
	if err != nil {
		t.Fatal(err)
	}
	fin, err := s.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if fin.Replace || fin.Outcome.Source != SourceModel {
		t.Fatalf("finish = %+v", fin)
	}
	all := strings.Join(append(step.Segments, fin.Segments...), "")
	if fin.Outcome.Text != "" && !strings.Contains(fin.Outcome.Text, "Maria Rossi") {
		// Prefer outcome text (resolved); also check emitted segments.
		all = fin.Outcome.Text
	}
	if !strings.Contains(all, "Maria Rossi") {
		t.Fatalf("resolved stream missing name: segments=%v outcome=%q", append(step.Segments, fin.Segments...), fin.Outcome.Text)
	}
	if strings.Contains(all, "{{ref:") {
		t.Fatalf("client must not see token: %q", all)
	}
	if fin.Outcome.ModelText == "" || !strings.Contains(fin.Outcome.ModelText, "{{ref:ds_att_001:0:attendee_name}}") {
		t.Fatalf("model text must keep token: %q", fin.Outcome.ModelText)
	}
	if strings.Contains(fin.Outcome.ModelText, "Maria Rossi") {
		t.Fatalf("model text must not contain resolved name: %q", fin.Outcome.ModelText)
	}
}

func TestHasIncompleteRefToken(t *testing.T) {
	if !HasIncompleteRefToken("x {{ref:ds_a:0:n") {
		t.Fatal("expected incomplete")
	}
	if HasIncompleteRefToken("x {{ref:ds_att_001:0:attendee_name}} y") {
		t.Fatal("complete should be fine")
	}
}

func TestMaskRefTokens(t *testing.T) {
	got := MaskRefTokens("A {{ref:ds_att_001:0:attendee_name}} B")
	if strings.Contains(got, "{{ref:") || !strings.Contains(got, refPlaceholder) {
		t.Fatalf("mask = %q", got)
	}
}
