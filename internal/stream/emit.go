package stream

import (
	"fmt"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// ClientCapabilities is what a turn's client announced it can parse of the 1.5
// analyst / rich-block surface (ADR 0047). Absent / zero value means 1.4: every
// event is mapped to a form that validates against schemas/_frozen/v1 before it
// is encoded onto SSE, WebSocket or AG-UI.
//
// The dialogue loop (or the gateway, when it has read the turn body /
// forwardedProps) must call Turn.SetClientCapabilities before the first
// Publish of the turn. This package owns the mapping; persistence of the
// announcement on the turn options belongs to the integrator that opens the
// hub turn.
type ClientCapabilities = analyst.ClientCapabilities

// CapsV15 is the full 1.5 announcement: every block, mark, layout, progress
// stage and block field this release's SDKs and kits parse. SDKs copy this
// (or a subset) onto turn requests / AG-UI forwardedProps.
func CapsV15() ClientCapabilities {
	c := analyst.ClientCapabilitiesContract15
	src := true
	return ClientCapabilities{
		Contract: &c,
		Blocks: []analyst.ClientCapabilitiesBlocksItem{
			analyst.ClientCapabilitiesBlocksItemMarkdown,
			analyst.ClientCapabilitiesBlocksItemCode,
			analyst.ClientCapabilitiesBlocksItemTable,
			analyst.ClientCapabilitiesBlocksItemChart,
			analyst.ClientCapabilitiesBlocksItemDiagram,
			analyst.ClientCapabilitiesBlocksItemCitationList,
			analyst.ClientCapabilitiesBlocksItemActionCard,
			analyst.ClientCapabilitiesBlocksItemClarificationChoices,
			analyst.ClientCapabilitiesBlocksItemConfirmationPanel,
			analyst.ClientCapabilitiesBlocksItemKpi,
			analyst.ClientCapabilitiesBlocksItemTimeline,
			analyst.ClientCapabilitiesBlocksItemRecordCard,
			analyst.ClientCapabilitiesBlocksItemProvenance,
			analyst.ClientCapabilitiesBlocksItemDraft,
		},
		ChartMarks: []analyst.ClientCapabilitiesChartMarksItem{
			analyst.ClientCapabilitiesChartMarksItemBar,
			analyst.ClientCapabilitiesChartMarksItemLine,
			analyst.ClientCapabilitiesChartMarksItemArea,
			analyst.ClientCapabilitiesChartMarksItemPoint,
			analyst.ClientCapabilitiesChartMarksItemPie,
			analyst.ClientCapabilitiesChartMarksItemScatter,
			analyst.ClientCapabilitiesChartMarksItemHeatmap,
			analyst.ClientCapabilitiesChartMarksItemComboBarLine,
		},
		ChartLayouts: []analyst.ClientCapabilitiesChartLayoutsItem{
			analyst.ClientCapabilitiesChartLayoutsItemGrouped,
			analyst.ClientCapabilitiesChartLayoutsItemStacked,
			analyst.ClientCapabilitiesChartLayoutsItemHorizontal,
			analyst.ClientCapabilitiesChartLayoutsItemDonut,
		},
		ProgressStages: []analyst.ClientCapabilitiesProgressStagesItem{
			analyst.ClientCapabilitiesProgressStagesItemInterpreting,
			analyst.ClientCapabilitiesProgressStagesItemRetrieving,
			analyst.ClientCapabilitiesProgressStagesItemSources,
			analyst.ClientCapabilitiesProgressStagesItemValidating,
			analyst.ClientCapabilitiesProgressStagesItemPreparing,
			analyst.ClientCapabilitiesProgressStagesItemCommitting,
			analyst.ClientCapabilitiesProgressStagesItemToolCall,
			analyst.ClientCapabilitiesProgressStagesItemQueued,
			analyst.ClientCapabilitiesProgressStagesItemGenerating,
			analyst.ClientCapabilitiesProgressStagesItemVerifying,
			analyst.ClientCapabilitiesProgressStagesItemAnalystRead,
			analyst.ClientCapabilitiesProgressStagesItemAnalystCompute,
			analyst.ClientCapabilitiesProgressStagesItemAnalystSearch,
			analyst.ClientCapabilitiesProgressStagesItemAnalystFetch,
			analyst.ClientCapabilitiesProgressStagesItemAnalystAnswer,
		},
		SourceLabels: &src,
		BlockFields: []analyst.ClientCapabilitiesBlockFieldsItem{
			analyst.ClientCapabilitiesBlockFieldsItemDatasetID,
			analyst.ClientCapabilitiesBlockFieldsItemBarLayout,
			analyst.ClientCapabilitiesBlockFieldsItemPieStyle,
			analyst.ClientCapabilitiesBlockFieldsItemSource,
		},
	}
}

// MapEvent rewrites a validated TurnEvent so it only carries members the client
// announced. Zero / absent capabilities map everything to the frozen 1.4 form.
// Announced items pass through; unannounced 1.5 stages, blocks, marks, layouts
// and members are mapped or stripped per ADR 0047.
func MapEvent(ev conversation.TurnEvent, caps ClientCapabilities) conversation.TurnEvent {
	view := newCapView(caps)
	switch v := ev.Value.(type) {
	case conversation.ProgressEvent:
		v.Stage = mapStage(v.Stage, view)
		ev.Value = v
	case conversation.BlockEvent:
		v.Block = mapBlock(v.Block, view)
		ev.Value = v
	}
	return ev
}

type capView struct {
	blocks       map[string]bool
	marks        map[string]bool
	layouts      map[string]bool
	stages       map[string]bool
	sourceLabels bool
	fields       map[string]bool
}

func newCapView(c ClientCapabilities) capView {
	v := capView{
		blocks:  map[string]bool{},
		marks:   map[string]bool{},
		layouts: map[string]bool{},
		stages:  map[string]bool{},
		fields:  map[string]bool{},
	}
	for _, b := range c.Blocks {
		v.blocks[string(b)] = true
	}
	for _, m := range c.ChartMarks {
		v.marks[string(m)] = true
	}
	for _, l := range c.ChartLayouts {
		v.layouts[string(l)] = true
	}
	for _, s := range c.ProgressStages {
		v.stages[string(s)] = true
	}
	for _, f := range c.BlockFields {
		v.fields[string(f)] = true
	}
	if c.SourceLabels != nil {
		v.sourceLabels = *c.SourceLabels
	}
	return v
}

func (v capView) allowsBlock(typ string) bool {
	if isV14Block(typ) {
		return true
	}
	return v.blocks[typ]
}

func isV14Block(typ string) bool {
	switch typ {
	case "markdown", "code", "table", "chart", "diagram", "citation_list", "action_card", "clarification_choices", "confirmation_panel":
		return true
	}
	return false
}

func (v capView) allowsMark(mark string) bool {
	if isV14Mark(mark) {
		return true
	}
	return v.marks[mark]
}

func isV14Mark(mark string) bool {
	switch mark {
	case "bar", "line", "area", "point", "pie":
		return true
	}
	return false
}

func (v capView) allowsStage(stage string) bool {
	if isV14Stage(stage) {
		return true
	}
	return v.stages[stage]
}

func isV14Stage(stage string) bool {
	switch stage {
	case "interpreting", "retrieving", "sources", "validating", "preparing", "committing", "tool_call", "queued", "generating", "verifying":
		return true
	}
	return false
}

func (v capView) allowsField(name string) bool { return v.fields[name] }

func mapStage(stage conversation.ProgressEventStage, v capView) conversation.ProgressEventStage {
	s := string(stage)
	if v.allowsStage(s) {
		return stage
	}
	switch stage {
	case conversation.ProgressEventStageAnalystRead,
		conversation.ProgressEventStageAnalystSearch,
		conversation.ProgressEventStageAnalystFetch:
		return conversation.ProgressEventStageRetrieving
	case conversation.ProgressEventStageAnalystCompute:
		return conversation.ProgressEventStageGenerating
	case conversation.ProgressEventStageAnalystAnswer:
		return conversation.ProgressEventStageVerifying
	}
	return conversation.ProgressEventStageGenerating
}

func mapBlock(b conversation.RichBlock, v capView) conversation.RichBlock {
	switch blk := b.Value.(type) {
	case conversation.KpiBlock:
		if v.allowsBlock("kpi") {
			b.Value = stripKpi(blk, v)
			return b
		}
		b.Value = kpiAsTable(blk)
	case conversation.TimelineBlock:
		if v.allowsBlock("timeline") {
			b.Value = stripTimeline(blk, v)
			return b
		}
		b.Value = timelineAsTable(blk)
	case conversation.RecordCardBlock:
		if v.allowsBlock("record_card") {
			b.Value = stripRecordCard(blk, v)
			return b
		}
		b.Value = recordCardAsTable(blk)
	case conversation.ProvenanceBlock:
		if v.allowsBlock("provenance") {
			b.Value = stripProvenance(blk, v)
			return b
		}
		b.Value = provenanceAsMarkdown(blk)
	case conversation.DraftBlock:
		if v.allowsBlock("draft") {
			b.Value = stripDraft(blk, v)
			return b
		}
		b.Value = draftAsMarkdown(blk)
	case conversation.ChartBlock:
		b.Value = mapChart(blk, v)
	case conversation.TableBlock:
		b.Value = stripTable(blk, v)
	case conversation.MarkdownBlock, conversation.CodeBlock, conversation.DiagramBlock,
		conversation.CitationListBlock, conversation.ActionCardBlock,
		conversation.ClarificationChoicesBlock, conversation.ConfirmationPanelBlock:
		// 1.4 types: unchanged.
	default:
		// Unknown variant: leave as-is (validator already refused unknowns).
	}
	return b
}

func mapChart(c conversation.ChartBlock, v capView) conversation.RichBlockValue {
	mark := string(c.Mark)
	if mark == "heatmap" && !v.allowsMark("heatmap") {
		return chartAsTable(c)
	}
	if !v.allowsMark(mark) {
		switch c.Mark {
		case conversation.ChartBlockMarkScatter:
			c.Mark = conversation.ChartBlockMarkPoint
		case conversation.ChartBlockMarkComboBarLine:
			c.Mark = conversation.ChartBlockMarkLine
		case conversation.ChartBlockMarkHeatmap:
			return chartAsTable(c)
		default:
			c.Mark = conversation.ChartBlockMarkBar
		}
	}
	if c.BarLayout != nil {
		layout := string(*c.BarLayout)
		if !v.allowsField("bar_layout") || !v.layouts[layout] {
			c.BarLayout = nil
		}
	}
	if c.PieStyle != nil {
		style := string(*c.PieStyle)
		if !v.allowsField("pie_style") || !v.layouts[style] {
			c.PieStyle = nil
		}
	}
	if !v.allowsField("dataset_id") {
		c.DatasetID = nil
	}
	if !v.allowsField("source") || !v.sourceLabels {
		c.Source = nil
	}
	// y2 / size are 1.5 encoding channels; omit unless the client announced a mark that needs them.
	if !v.allowsMark("scatter") && !v.allowsMark("combo_bar_line") && !v.allowsMark("heatmap") {
		c.Encoding.Y2 = nil
		c.Encoding.Size = nil
	} else {
		if !v.allowsMark("combo_bar_line") {
			c.Encoding.Y2 = nil
		}
		if !v.allowsMark("scatter") {
			c.Encoding.Size = nil
		}
	}
	return c
}

func stripTable(t conversation.TableBlock, v capView) conversation.TableBlock {
	if !v.allowsField("dataset_id") {
		t.DatasetID = nil
	}
	if !v.allowsField("source") || !v.sourceLabels {
		t.Source = nil
	}
	return t
}

func stripKpi(k conversation.KpiBlock, v capView) conversation.KpiBlock {
	if !v.allowsField("dataset_id") {
		k.DatasetID = nil
	}
	if !v.allowsField("source") || !v.sourceLabels {
		k.Source = nil
	}
	return k
}

func stripTimeline(t conversation.TimelineBlock, v capView) conversation.TimelineBlock {
	if !v.allowsField("dataset_id") {
		t.DatasetID = nil
	}
	if !v.allowsField("source") || !v.sourceLabels {
		t.Source = nil
	}
	return t
}

func stripRecordCard(r conversation.RecordCardBlock, v capView) conversation.RecordCardBlock {
	if !v.allowsField("dataset_id") {
		r.DatasetID = nil
	}
	if !v.allowsField("source") || !v.sourceLabels {
		r.Source = nil
	}
	return r
}

func stripProvenance(p conversation.ProvenanceBlock, v capView) conversation.ProvenanceBlock {
	steps := make([]conversation.ProvenanceStep, len(p.Steps))
	for i, s := range p.Steps {
		if !v.allowsField("dataset_id") {
			s.DatasetID = nil
		}
		if !v.sourceLabels {
			s.WebCitationID = nil
		}
		steps[i] = s
	}
	p.Steps = steps
	return p
}

func stripDraft(d conversation.DraftBlock, v capView) conversation.DraftBlock {
	if !v.allowsField("source") || !v.sourceLabels {
		d.Source = nil
	}
	return d
}

func kpiAsTable(k conversation.KpiBlock) conversation.TableBlock {
	cap := string(k.Label)
	cols := []conversation.TableColumn{
		{Key: common.FieldName("label"), Label: "Label"},
		{Key: common.FieldName("value"), Label: "Value"},
	}
	row := []conversation.TableCell{string(k.Label), fmt.Sprint(k.Value)}
	if k.Delta != nil {
		cols = append(cols, conversation.TableColumn{Key: common.FieldName("delta"), Label: "Delta"})
		row = append(row, fmt.Sprint(k.Delta))
	}
	if k.Unit != nil {
		cols = append(cols, conversation.TableColumn{Key: common.FieldName("unit"), Label: "Unit"})
		row = append(row, *k.Unit)
	}
	if k.Period != nil {
		cols = append(cols, conversation.TableColumn{Key: common.FieldName("period"), Label: "Period"})
		row = append(row, string(*k.Period))
	}
	return conversation.TableBlock{Caption: &cap, Columns: cols, Rows: [][]conversation.TableCell{row}}
}

func timelineAsTable(t conversation.TimelineBlock) conversation.TableBlock {
	var cap *string
	if t.Title != nil {
		s := string(*t.Title)
		cap = &s
	}
	cols := []conversation.TableColumn{
		{Key: common.FieldName("at"), Label: "When"},
		{Key: common.FieldName("label"), Label: "Event"},
		{Key: common.FieldName("detail"), Label: "Detail"},
	}
	rows := make([][]conversation.TableCell, 0, len(t.Events))
	for _, e := range t.Events {
		detail := ""
		if e.Detail != nil {
			detail = string(*e.Detail)
		}
		rows = append(rows, []conversation.TableCell{e.At, string(e.Label), detail})
	}
	return conversation.TableBlock{Caption: cap, Columns: cols, Rows: rows}
}

func recordCardAsTable(r conversation.RecordCardBlock) conversation.TableBlock {
	cap := r.Title
	cols := []conversation.TableColumn{
		{Key: common.FieldName("key"), Label: "Key"},
		{Key: common.FieldName("label"), Label: "Label"},
		{Key: common.FieldName("value"), Label: "Value"},
	}
	rows := make([][]conversation.TableCell, 0, len(r.Fields))
	for _, f := range r.Fields {
		rows = append(rows, []conversation.TableCell{string(f.Key), f.Label, f.Value})
	}
	return conversation.TableBlock{Caption: &cap, Columns: cols, Rows: rows}
}

func provenanceAsMarkdown(p conversation.ProvenanceBlock) conversation.MarkdownBlock {
	var b strings.Builder
	title := "How this was calculated"
	if p.Title != nil && *p.Title != "" {
		title = string(*p.Title)
	}
	b.WriteString("## ")
	b.WriteString(title)
	b.WriteByte('\n')
	for _, s := range p.Steps {
		fmt.Fprintf(&b, "- **%s**: %s\n", s.Kind, s.Summary)
	}
	return conversation.MarkdownBlock{Text: b.String()}
}

func draftAsMarkdown(d conversation.DraftBlock) conversation.MarkdownBlock {
	var b strings.Builder
	if d.Title != "" {
		b.WriteString("# ")
		b.WriteString(string(d.Title))
		b.WriteByte('\n')
		b.WriteByte('\n')
	}
	b.WriteString(d.Body)
	return conversation.MarkdownBlock{Text: b.String()}
}

func chartAsTable(c conversation.ChartBlock) conversation.TableBlock {
	var cap *string
	if c.Title != nil {
		s := string(*c.Title)
		cap = &s
	}
	keys := chartFieldKeys(c)
	cols := make([]conversation.TableColumn, 0, len(keys))
	for _, k := range keys {
		cols = append(cols, conversation.TableColumn{Key: common.FieldName(k), Label: k})
	}
	if len(cols) == 0 {
		cols = []conversation.TableColumn{{Key: common.FieldName("value"), Label: "Value"}}
	}
	rows := make([][]conversation.TableCell, 0, len(c.Data))
	for _, rec := range c.Data {
		row := make([]conversation.TableCell, len(cols))
		for i, col := range cols {
			if val, ok := rec[string(col.Key)]; ok {
				row[i] = val
			}
		}
		rows = append(rows, row)
	}
	return conversation.TableBlock{Caption: cap, Columns: cols, Rows: rows}
}

func chartFieldKeys(c conversation.ChartBlock) []string {
	seen := map[string]bool{}
	var keys []string
	add := func(ch *conversation.ChartChannel) {
		if ch == nil {
			return
		}
		k := string(ch.Field)
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		keys = append(keys, k)
	}
	add(c.Encoding.X)
	add(c.Encoding.Y)
	add(c.Encoding.Y2)
	add(c.Encoding.Color)
	add(c.Encoding.Size)
	add(c.Encoding.Theta)
	if len(keys) == 0 {
		for _, rec := range c.Data {
			for k := range rec {
				if !seen[k] {
					seen[k] = true
					keys = append(keys, k)
				}
			}
			break
		}
	}
	return keys
}
