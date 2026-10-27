package stream

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

func TestMapEventAbsentCapsIsFrozenV1(t *testing.T) {
	schema := frozenTurnEventSchema(t)
	turnID := actions.TurnID(streamTurnID(9001))
	for i, ev := range emitCorpus(turnID) {
		mapped := MapEvent(ev, ClientCapabilities{})
		raw, err := json.Marshal(mapped)
		if err != nil {
			t.Fatalf("corpus[%d] marshal: %v", i, err)
		}
		inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("corpus[%d] parse: %v\n%s", i, err, raw)
		}
		if err := schema.Validate(inst); err != nil {
			t.Fatalf("corpus[%d] not frozen v1: %v\n%s", i, err, raw)
		}
		assertNo15Members(t, raw, i)
	}
}

func TestMapEventFullCapsPassThrough(t *testing.T) {
	turnID := actions.TurnID(streamTurnID(9002))
	caps := CapsV15()
	for i, ev := range emitCorpus(turnID) {
		mapped := MapEvent(ev, caps)
		before, _ := json.Marshal(ev)
		after, _ := json.Marshal(mapped)
		if string(before) != string(after) {
			t.Fatalf("corpus[%d] changed under full caps\nbefore=%s\nafter=%s", i, before, after)
		}
	}
}

func TestMapEventPartialAnnouncement(t *testing.T) {
	turnID := actions.TurnID(streamTurnID(9003))
	kpi := true
	caps := ClientCapabilities{
		Blocks:         []analyst.ClientCapabilitiesBlocksItem{analyst.ClientCapabilitiesBlocksItemKpi},
		ProgressStages: []analyst.ClientCapabilitiesProgressStagesItem{analyst.ClientCapabilitiesProgressStagesItemAnalystRead},
		ChartMarks:     []analyst.ClientCapabilitiesChartMarksItem{analyst.ClientCapabilitiesChartMarksItemScatter},
		SourceLabels:   &kpi,
		BlockFields:    []analyst.ClientCapabilitiesBlockFieldsItem{analyst.ClientCapabilitiesBlockFieldsItemSource},
	}
	// kpi passes; provenance maps to markdown; analyst_compute maps; scatter stays; heatmap→table.
	ev := conversation.TurnEvent{Value: conversation.BlockEvent{
		TurnID: turnID, Seq: 0,
		Block: conversation.RichBlock{Value: conversation.KpiBlock{Label: "Revenue", Value: 12}},
	}}
	out := MapEvent(ev, caps)
	if _, ok := out.Value.(conversation.BlockEvent).Block.Value.(conversation.KpiBlock); !ok {
		t.Fatalf("kpi should pass through, got %T", out.Value.(conversation.BlockEvent).Block.Value)
	}

	ev = conversation.TurnEvent{Value: conversation.BlockEvent{
		TurnID: turnID, Seq: 1,
		Block: conversation.RichBlock{Value: conversation.ProvenanceBlock{
			Steps: []conversation.ProvenanceStep{{Kind: conversation.ProvenanceStepKindRead, Summary: "read orders"}},
		}},
	}}
	out = MapEvent(ev, caps)
	if _, ok := out.Value.(conversation.BlockEvent).Block.Value.(conversation.MarkdownBlock); !ok {
		t.Fatalf("provenance should fall back to markdown, got %T", out.Value.(conversation.BlockEvent).Block.Value)
	}

	ev = conversation.TurnEvent{Value: conversation.ProgressEvent{
		TurnID: turnID, Seq: 2,
		Stage: conversation.ProgressEventStageAnalystRead,
		State: conversation.ProgressEventStateStarted,
	}}
	out = MapEvent(ev, caps)
	if out.Value.(conversation.ProgressEvent).Stage != conversation.ProgressEventStageAnalystRead {
		t.Fatalf("announced analyst_read mapped away")
	}
	ev = conversation.TurnEvent{Value: conversation.ProgressEvent{
		TurnID: turnID, Seq: 3,
		Stage: conversation.ProgressEventStageAnalystCompute,
		State: conversation.ProgressEventStateStarted,
	}}
	out = MapEvent(ev, caps)
	if out.Value.(conversation.ProgressEvent).Stage != conversation.ProgressEventStageGenerating {
		t.Fatalf("unannounced analyst_compute = %s, want generating", out.Value.(conversation.ProgressEvent).Stage)
	}

	scatter := conversation.ChartBlock{
		Description: "scatter",
		Mark:        conversation.ChartBlockMarkScatter,
		Data:        []conversation.ChartRecord{{"x": 1, "y": 2}},
		Encoding: conversation.ChartEncoding{
			X: &conversation.ChartChannel{Field: common.FieldName("x"), Type: conversation.ChartChannelTypeQuantitative},
			Y: &conversation.ChartChannel{Field: common.FieldName("y"), Type: conversation.ChartChannelTypeQuantitative},
		},
	}
	out = MapEvent(conversation.TurnEvent{Value: conversation.BlockEvent{
		TurnID: turnID, Seq: 4, Block: conversation.RichBlock{Value: scatter},
	}}, caps)
	if got := out.Value.(conversation.BlockEvent).Block.Value.(conversation.ChartBlock).Mark; got != conversation.ChartBlockMarkScatter {
		t.Fatalf("announced scatter became %s", got)
	}

	heat := scatter
	heat.Mark = conversation.ChartBlockMarkHeatmap
	heat.Description = "heat"
	out = MapEvent(conversation.TurnEvent{Value: conversation.BlockEvent{
		TurnID: turnID, Seq: 5, Block: conversation.RichBlock{Value: heat},
	}}, caps)
	if _, ok := out.Value.(conversation.BlockEvent).Block.Value.(conversation.TableBlock); !ok {
		t.Fatalf("unannounced heatmap should become table, got %T", out.Value.(conversation.BlockEvent).Block.Value)
	}
}

func TestPublishMapsBeforeEncode(t *testing.T) {
	hub := NewHub(Config{})
	turn, err := hub.Begin(streamScope("t1", "p1"), streamTurnID(9100))
	if err != nil {
		t.Fatal(err)
	}
	// No caps set → 1.4 mapping.
	ev := conversation.TurnEvent{Value: conversation.ProgressEvent{
		Type: "progress", TurnID: actions.TurnID(turn.ID()), Seq: 0,
		Stage: conversation.ProgressEventStageAnalystRead,
		State: conversation.ProgressEventStateStarted,
	}}
	if err := turn.Publish(ev); err != nil {
		t.Fatal(err)
	}
	sub, err := turn.Subscribe(FromStart)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	frame, err := sub.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(frame.Data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["stage"] != "retrieving" {
		t.Fatalf("published stage = %v, want retrieving", doc["stage"])
	}
	schema := frozenTurnEventSchema(t)
	inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader(frame.Data))
	if err := schema.Validate(inst); err != nil {
		t.Fatalf("published frame not frozen: %v\n%s", err, frame.Data)
	}
}

func assertNo15Members(t *testing.T, raw []byte, i int) {
	t.Helper()
	s := string(raw)
	for _, bad := range []string{
		`"analyst_read"`, `"analyst_compute"`, `"analyst_search"`, `"analyst_fetch"`, `"analyst_answer"`,
		`"kpi"`, `"timeline"`, `"record_card"`, `"provenance"`, `"draft"`,
		`"scatter"`, `"heatmap"`, `"combo_bar_line"`,
		`"bar_layout"`, `"pie_style"`, `"dataset_id"`,
		`"y2"`, `"\"size\":`,
	} {
		if strings.Contains(s, bad) {
			// "size" as encoding channel is `"size":`; allow unrelated words.
			if bad == `"\"size\":` {
				continue
			}
			if bad == `"\"size\":` {
				continue
			}
			t.Fatalf("corpus[%d] still has 1.5 member %s in %s", i, bad, s)
		}
	}
	if strings.Contains(s, `"size":`) && strings.Contains(s, `"encoding"`) {
		// encoding.size must not appear in frozen chart
		var doc map[string]any
		_ = json.Unmarshal(raw, &doc)
		if blk, ok := doc["block"].(map[string]any); ok {
			if enc, ok := blk["encoding"].(map[string]any); ok {
				if _, ok := enc["size"]; ok {
					t.Fatalf("corpus[%d] encoding.size present", i)
				}
				if _, ok := enc["y2"]; ok {
					t.Fatalf("corpus[%d] encoding.y2 present", i)
				}
			}
			if _, ok := blk["source"]; ok {
				t.Fatalf("corpus[%d] block.source present", i)
			}
		}
	}
}

func emitCorpus(turnID actions.TurnID) []conversation.TurnEvent {
	pt := func(s string) *conversation.PlainText { p := conversation.PlainText(s); return &p }
	src := analyst.SourceLabelApp
	ds := analyst.DatasetID("ds_01J00000000000000000000001")
	barLayout := conversation.ChartBlockBarLayoutGrouped
	pieStyle := conversation.ChartBlockPieStyleDonut
	seq := conversation.Seq(0)
	next := func() conversation.Seq { s := seq; seq++; return s }

	chart := func(mark conversation.ChartBlockMark) conversation.ChartBlock {
		return conversation.ChartBlock{
			Description: "chart of sales",
			Mark:        mark,
			Data:        []conversation.ChartRecord{{"x": "a", "y": 1.0, "c": "s"}},
			Encoding: conversation.ChartEncoding{
				X:     &conversation.ChartChannel{Field: common.FieldName("x"), Type: conversation.ChartChannelTypeNominal},
				Y:     &conversation.ChartChannel{Field: common.FieldName("y"), Type: conversation.ChartChannelTypeQuantitative},
				Color: &conversation.ChartChannel{Field: common.FieldName("c"), Type: conversation.ChartChannelTypeNominal},
			},
			DatasetID: &ds,
			Source:    &src,
		}
	}
	pie := conversation.ChartBlock{
		Description: "share",
		Mark:        conversation.ChartBlockMarkPie,
		PieStyle:    &pieStyle,
		Data:        []conversation.ChartRecord{{"t": 1.0, "c": "a"}},
		Encoding: conversation.ChartEncoding{
			Theta: &conversation.ChartChannel{Field: common.FieldName("t"), Type: conversation.ChartChannelTypeQuantitative},
			Color: &conversation.ChartChannel{Field: common.FieldName("c"), Type: conversation.ChartChannelTypeNominal},
		},
	}
	bar := chart(conversation.ChartBlockMarkBar)
	bar.BarLayout = &barLayout
	scatter := chart(conversation.ChartBlockMarkScatter)
	scatter.Encoding.Size = &conversation.ChartChannel{Field: common.FieldName("y"), Type: conversation.ChartChannelTypeQuantitative}
	combo := chart(conversation.ChartBlockMarkComboBarLine)
	combo.Encoding.Y2 = &conversation.ChartChannel{Field: common.FieldName("y"), Type: conversation.ChartChannelTypeQuantitative}
	heat := chart(conversation.ChartBlockMarkHeatmap)

	stages := []conversation.ProgressEventStage{
		conversation.ProgressEventStageAnalystRead,
		conversation.ProgressEventStageAnalystCompute,
		conversation.ProgressEventStageAnalystSearch,
		conversation.ProgressEventStageAnalystFetch,
		conversation.ProgressEventStageAnalystAnswer,
		conversation.ProgressEventStageRetrieving,
	}
	var out []conversation.TurnEvent
	for _, st := range stages {
		out = append(out, conversation.TurnEvent{Value: conversation.ProgressEvent{
			TurnID: turnID, Seq: next(), Stage: st, State: conversation.ProgressEventStateStarted,
		}})
	}
	blocks := []conversation.RichBlockValue{
		conversation.KpiBlock{Label: "Revenue", Value: 42.5, Delta: 1.2, Period: pt("Q1"), Unit: strPtr("EUR"), Source: &src, DatasetID: &ds},
		conversation.TimelineBlock{Title: pt("Milestones"), Events: []conversation.TimelineEvent{
			{At: "2026-01-01", Label: "Start", Detail: pt("kickoff")},
		}, Source: &src, DatasetID: &ds},
		conversation.RecordCardBlock{Title: "Acme", Fields: []conversation.RecordCardField{
			{Key: common.FieldName("name"), Label: "Name", Value: "Acme"},
		}, DatasetID: &ds},
		conversation.ProvenanceBlock{Title: pt("How this was calculated"), Steps: []conversation.ProvenanceStep{
			{Kind: conversation.ProvenanceStepKindRead, Summary: "read orders", DatasetID: &ds},
			{Kind: conversation.ProvenanceStepKindCompute, Summary: "sum amount", Op: strPtr("aggregate")},
			{Kind: conversation.ProvenanceStepKindForecast, Summary: "linear trend"},
		}},
		conversation.DraftBlock{Title: "Email", Format: conversation.DraftBlockFormatMarkdown, Body: "Hello", Source: &src},
		bar, pie, scatter, combo, heat,
		chart(conversation.ChartBlockMarkLine),
		conversation.TableBlock{
			Caption:   strPtr("Sales"),
			Columns:   []conversation.TableColumn{{Key: common.FieldName("amt"), Label: "Amount"}},
			Rows:      [][]conversation.TableCell{{10}},
			DatasetID: &ds, Source: &src,
		},
		conversation.MarkdownBlock{Text: "hello"},
	}
	for _, b := range blocks {
		out = append(out, conversation.TurnEvent{Value: conversation.BlockEvent{
			TurnID: turnID, Seq: next(), Block: conversation.RichBlock{Value: b},
		}})
	}
	return out
}

func strPtr(s string) *string { return &s }

var (
	frozenOnce   sync.Once
	frozenSchema *jsonschema.Schema
	frozenErr    error
)

func frozenTurnEventSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	frozenOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		root := filepath.Join("..", "..", "schemas", "_frozen", "v1")
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".frozen.json") {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				return err
			}
			id, _ := doc.(map[string]any)["$id"].(string)
			if id == "" {
				return nil
			}
			return c.AddResource(id, doc)
		})
		if err != nil {
			frozenErr = err
			return
		}
		frozenSchema, frozenErr = c.Compile("urn:alixai:schema:conversation:turn-event:v1")
	})
	if frozenErr != nil {
		t.Fatal(frozenErr)
	}
	return frozenSchema
}
