package extmode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/blocks"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// A provider reply that restates host numbers in a table and a bar chart is accepted; a single
// altered number in the chart is refused and the deterministic template is used instead.
func TestExternalRichBlocksVerifiedAgainstDirectiveFacts(t *testing.T) {
	facts := []actions.Fact{
		{Field: "slot_1_clinic", Value: actions.CanonicalValue{Value: actions.TextValue{Value: "North"}}},
		{Field: "slot_1_n", Value: actions.CanonicalValue{Value: actions.IntegerValue{Value: 12}}},
		{Field: "slot_2_clinic", Value: actions.CanonicalValue{Value: actions.TextValue{Value: "South"}}},
		{Field: "slot_2_n", Value: actions.CanonicalValue{Value: actions.IntegerValue{Value: 18}}},
	}
	d := &conversation.DialogueDirective{
		TurnID: "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", Act: conversation.DialogueDirectiveActPresentResult,
		Facts: facts, MustState: []common.FieldName{"slot_1_clinic", "slot_1_n", "slot_2_clinic", "slot_2_n"},
		MissingFields: []common.FieldName{}, InvalidFields: []actions.Issue{}, AllowedSuggestions: []actions.Choice{},
		Confirmation:         actions.ConfirmationRequirement{Mode: actions.ConfirmationRequirementModeNone},
		PermittedNextActions: []actions.PermittedNextAction{}, Sources: []conversation.Citation{},
	}
	text := "North has 12 bookings; South has 18."
	tableJSON := `{"type":"table","columns":[{"key":"clinic","label":"Clinic"},{"key":"n","label":"N"}],"rows":[["North",12],["South",18]]}`
	goodChart := `{"type":"chart","description":"Bookings.","mark":"bar","data":[{"clinic":"North","n":12},{"clinic":"South","n":18}],"encoding":{"x":{"field":"clinic","type":"nominal"},"y":{"field":"n","type":"quantitative"}}}`
	badChart := `{"type":"chart","description":"Bookings.","mark":"bar","data":[{"clinic":"North","n":12},{"clinic":"South","n":99}],"encoding":{"x":{"field":"clinic","type":"nominal"},"y":{"field":"n","type":"quantitative"}}}`

	t.Run("matching table and chart are accepted", func(t *testing.T) {
		res := providers.RichResult{Parts: []providers.ContentPart{
			providers.TextPart(text), providers.TextPart(tableJSON), providers.TextPart(goodChart),
		}}
		got, refused := RichBlocks(blocks.Default(), blocks.Context{Facts: blocks.NewFactSet(facts)}, res)
		if len(refused) != 0 || len(got) < 2 {
			t.Fatalf("blocks=%d refused=%+v", len(got), refused)
		}
		var data []conversation.RichBlock
		for _, b := range got {
			switch b.Value.(type) {
			case conversation.TableBlock, conversation.ChartBlock:
				data = append(data, b)
			}
		}
		if len(data) != 2 {
			t.Fatalf("data blocks=%d total=%d", len(data), len(got))
		}
		out, err := verify.RealiseRich(text, data, d, nil, verify.Options{Language: verify.LangEN})
		if err != nil || out.Source != verify.SourceModel || len(out.Blocks) != 2 {
			t.Fatalf("out=%+v err=%v", out, err)
		}
	})
	t.Run("an altered chart number is the template", func(t *testing.T) {
		res := providers.RichResult{Parts: []providers.ContentPart{
			providers.TextPart(text), providers.TextPart(tableJSON), providers.TextPart(badChart),
		}}
		got, refused := RichBlocks(blocks.Default(), blocks.Context{Facts: blocks.NewFactSet(facts)}, res)
		if len(refused) == 0 {
			t.Fatalf("expected the chart to be refused, got blocks=%d", len(got))
		}
		var bad conversation.RichBlock
		if err := json.Unmarshal([]byte(badChart), &bad); err != nil {
			t.Fatal(err)
		}
		out, err := verify.RealiseRich(text, []conversation.RichBlock{bad}, d, nil, verify.Options{Language: verify.LangEN})
		if err != nil || out.Source != verify.SourceTemplate {
			t.Fatalf("source=%s err=%v, want template", out.Source, err)
		}
	})
	t.Run("dataset_id does not bypass the fact check", func(t *testing.T) {
		// A model table with dataset_id + an invented number must not verify.
		bypass := `{"type":"table","dataset_id":"ds_anything","columns":[{"key":"clinic","label":"Clinic"},{"key":"n","label":"N"}],"rows":[["North",99],["South",18]]}`
		res := providers.RichResult{Parts: []providers.ContentPart{
			providers.TextPart(text), providers.TextPart(bypass),
		}}
		got, refused := RichBlocks(blocks.Default(), blocks.Context{Facts: blocks.NewFactSet(facts)}, res)
		if len(refused) == 0 {
			t.Fatalf("model table with dataset_id + invented number must be refused, got blocks=%d", len(got))
		}
		var bad conversation.RichBlock
		if err := json.Unmarshal([]byte(bypass), &bad); err != nil {
			t.Fatal(err)
		}
		out, err := verify.RealiseRich(text, []conversation.RichBlock{bad}, d, nil, verify.Options{Language: verify.LangEN})
		if err != nil || out.Source != verify.SourceTemplate {
			t.Fatalf("RealiseRich source=%s err=%v, want template (not invented 99)", out.Source, err)
		}
		if strings.Contains(out.Text, "99") {
			t.Fatalf("template must not carry the invented number: %q", out.Text)
		}
	})
}
