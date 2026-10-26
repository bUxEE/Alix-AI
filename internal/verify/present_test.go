package verify

import (
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

func TestBuildRecordTableFromIndexedFacts(t *testing.T) {
	facts := []actions.Fact{
		intFact("slot_count", 2),
		textFact("slot_1_starts_at", "09:00"),
		intFact("slot_1_n", 12),
		textFact("slot_2_starts_at", "10:30"),
		intFact("slot_2_n", 18),
	}
	tbl := BuildRecordTable(nil, facts, map[string]string{"starts_at": "Start", "n": "Bookings"})
	if tbl == nil || len(tbl.Rows) != 2 || len(tbl.Columns) != 2 {
		t.Fatalf("table = %+v", tbl)
	}
}

func TestPresentResultTemplateEmitsTableAndChart(t *testing.T) {
	facts := []actions.Fact{
		textFact("slot_1_clinic", "North"),
		intFact("slot_1_n", 12),
		textFact("slot_2_clinic", "South"),
		intFact("slot_2_n", 18),
	}
	d := directive(conversation.DialogueDirectiveActPresentResult, facts, "slot_1_clinic", "slot_1_n", "slot_2_clinic", "slot_2_n")
	opts := Options{
		Language:     LangEN,
		Table:        BuildRecordTable(nil, facts, nil),
		Presentation: &Presentation{Chart: &ChartHint{Type: "bar", X: "clinic", Y: "n"}},
	}
	out, err := Degraded(d, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != SourceTemplate || len(out.Blocks) != 2 {
		t.Fatalf("source=%s blocks=%d text=%q", out.Source, len(out.Blocks), out.Text)
	}
	var table, chart bool
	for _, b := range out.Blocks {
		switch b.Value.(type) {
		case conversation.TableBlock:
			table = true
		case conversation.ChartBlock:
			chart = true
		}
	}
	if !table || !chart {
		t.Fatalf("want table and chart, got %+v", out.Blocks)
	}
}

func TestRealiseRichRefusesAlteredChartNumber(t *testing.T) {
	facts := []actions.Fact{textFact("slot_1_clinic", "North"), intFact("slot_1_n", 12)}
	d := directive(conversation.DialogueDirectiveActPresentResult, facts, "slot_1_clinic", "slot_1_n")
	bad := conversation.RichBlock{Value: conversation.ChartBlock{
		Type: "chart", Description: "Bookings.", Mark: conversation.ChartBlockMarkBar,
		Data: []conversation.ChartRecord{{"clinic": "North", "n": int64(99)}},
		Encoding: conversation.ChartEncoding{
			X: &conversation.ChartChannel{Field: "clinic", Type: conversation.ChartChannelTypeNominal},
			Y: &conversation.ChartChannel{Field: "n", Type: conversation.ChartChannelTypeQuantitative},
		},
	}}
	out, err := RealiseRich("Clinic: North. Bookings: 12.", []conversation.RichBlock{bad}, d, nil, Options{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != SourceTemplate {
		t.Fatalf("source=%s, want template after an altered chart number", out.Source)
	}
}

func TestRealiseRichAcceptsMatchingTableAndChart(t *testing.T) {
	facts := []actions.Fact{textFact("slot_1_clinic", "North"), intFact("slot_1_n", 12)}
	d := directive(conversation.DialogueDirectiveActPresentResult, facts, "slot_1_clinic", "slot_1_n")
	n := int64(12)
	table := conversation.RichBlock{Value: conversation.TableBlock{
		Type: "table",
		Columns: []conversation.TableColumn{
			{Key: "clinic", Label: "Clinic"},
			{Key: "n", Label: "N"},
		},
		Rows: [][]any{{"North", n}},
	}}
	chart := conversation.RichBlock{Value: conversation.ChartBlock{
		Type: "chart", Description: "Bookings.", Mark: conversation.ChartBlockMarkBar,
		Data: []conversation.ChartRecord{{"clinic": "North", "n": n}},
		Encoding: conversation.ChartEncoding{
			X: &conversation.ChartChannel{Field: "clinic", Type: conversation.ChartChannelTypeNominal},
			Y: &conversation.ChartChannel{Field: "n", Type: conversation.ChartChannelTypeQuantitative},
		},
	}}
	out, err := RealiseRich("Clinic: North. Bookings: 12.", []conversation.RichBlock{table, chart}, d, nil, Options{Language: LangEN})
	if err != nil {
		t.Fatal(err)
	}
	if out.Source != SourceModel || len(out.Blocks) != 2 {
		t.Fatalf("source=%s blocks=%d", out.Source, len(out.Blocks))
	}
}
