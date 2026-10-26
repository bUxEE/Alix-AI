package verify

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/internal/blocks"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// indexedFactRE matches host facts shaped as "<prefix>_<n>_<field>" (clinic slots:
// slot_1_starts_at). The numeric index is 1-based; fields of one index form one row.
var indexedFactRE = regexp.MustCompile(`^([a-z][a-z0-9]*)_([1-9][0-9]{0,2})_([a-z][a-z0-9_]*)$`)

// BuildRecordTable turns a capability read result into a table when the host returned records
// (identifiers) or indexed fact rows. Columns are the distinct field suffixes (or entity fields),
// labelled from FieldLabels when present. Nil when there is nothing tabular to show.
func BuildRecordTable(ids []actions.RecordRef, facts []actions.Fact, labels map[string]string) *RecordTable {
	rows, cols := indexedRows(facts)
	if len(rows) == 0 && len(ids) > 0 {
		rows, cols = identifierRows(ids, facts)
	}
	if len(rows) == 0 {
		return nil
	}
	out := &RecordTable{Rows: rows}
	for _, c := range cols {
		label := c
		if labels != nil {
			if l := strings.TrimSpace(labels[c]); l != "" {
				label = l
			} else if l := strings.TrimSpace(labels[strings.ReplaceAll(c, "_", " ")]); l != "" {
				label = l
			}
		}
		if label == c {
			label = strings.ReplaceAll(c, "_", " ")
		}
		out.Columns = append(out.Columns, conversation.TableColumn{Key: common.FieldName(c), Label: label})
	}
	return out
}

func indexedRows(facts []actions.Fact) ([][]any, []string) {
	type rowMap map[string]any
	byIdx := map[int]rowMap{}
	colSet := map[string]bool{}
	for _, f := range facts {
		m := indexedFactRE.FindStringSubmatch(string(f.Field))
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		field := m[3]
		colSet[field] = true
		if byIdx[n] == nil {
			byIdx[n] = rowMap{}
		}
		byIdx[n][field] = cellValue(f)
	}
	if len(byIdx) == 0 {
		return nil, nil
	}
	cols := sortedKeys(colSet)
	idxs := make([]int, 0, len(byIdx))
	for i := range byIdx {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	rows := make([][]any, 0, len(idxs))
	for _, i := range idxs {
		rm := byIdx[i]
		row := make([]any, len(cols))
		for j, c := range cols {
			if v, ok := rm[c]; ok {
				row[j] = v
			}
		}
		rows = append(rows, row)
	}
	return rows, cols
}

func identifierRows(ids []actions.RecordRef, facts []actions.Fact) ([][]any, []string) {
	labels := []string{}
	for _, f := range facts {
		if string(f.Field) == "label" {
			labels = append(labels, fmt.Sprint(cellValue(f)))
		}
	}
	cols := []string{"entity_type", "entity_id"}
	if len(labels) == len(ids) {
		cols = append(cols, "label")
	}
	rows := make([][]any, 0, len(ids))
	for i, id := range ids {
		row := []any{string(id.EntityType), string(id.EntityID)}
		if len(labels) == len(ids) {
			row = append(row, labels[i])
		}
		rows = append(rows, row)
	}
	return rows, cols
}

func cellValue(f actions.Fact) any {
	if f.Display != nil && strings.TrimSpace(*f.Display) != "" {
		return strings.TrimSpace(*f.Display)
	}
	switch v := f.Value.Value.(type) {
	case actions.TextValue:
		return v.Value
	case *actions.TextValue:
		if v != nil {
			return v.Value
		}
	case actions.IntegerValue:
		return v.Value
	case *actions.IntegerValue:
		if v != nil {
			return v.Value
		}
	case actions.DecimalValue:
		return string(v.Value)
	case *actions.DecimalValue:
		if v != nil {
			return string(v.Value)
		}
	case actions.BooleanValue:
		return v.Value
	case *actions.BooleanValue:
		if v != nil {
			return v.Value
		}
	case actions.DateValue:
		return string(v.Value)
	case *actions.DateValue:
		if v != nil {
			return string(v.Value)
		}
	case actions.TimeValue:
		return string(v.Value)
	case *actions.TimeValue:
		if v != nil {
			return string(v.Value)
		}
	case actions.DatetimeValue:
		return string(v.Value)
	case *actions.DatetimeValue:
		if v != nil {
			return string(v.Value)
		}
	case actions.IdentifierValue:
		return string(v.Value)
	case *actions.IdentifierValue:
		if v != nil {
			return string(v.Value)
		}
	case actions.MoneyValue:
		return v.Value + " " + v.Currency
	case *actions.MoneyValue:
		if v != nil {
			return v.Value + " " + v.Currency
		}
	}
	return nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// presentBlocks builds validated table/chart blocks for a present_result template from opts.Table
// and opts.Presentation. Empty when there is nothing to show or the act is not present_result.
// A block that fails validation is omitted: the text template still answers (host labels can carry
// characters a table cell refuses; that must not fail the turn).
func presentBlocks(d *conversation.DialogueDirective, opts Options) []conversation.RichBlock {
	if d == nil || d.Act != conversation.DialogueDirectiveActPresentResult || opts.Table == nil || len(opts.Table.Rows) == 0 {
		return nil
	}
	facts := blocks.NewFactSet(d.Facts)
	var out []conversation.RichBlock
	table := conversation.RichBlock{Value: conversation.TableBlock{
		Type: "table", Columns: opts.Table.Columns, Rows: opts.Table.Rows,
	}}
	if clean, err := blocks.Default().Block(table, blocks.Context{Facts: facts}); err == nil {
		out = append(out, clean)
	}
	if opts.Presentation != nil && opts.Presentation.Chart != nil {
		if ch, ok := chartFromTable(opts.Table, opts.Presentation.Chart); ok {
			cb := conversation.RichBlock{Value: ch}
			if clean, err := blocks.Default().Block(cb, blocks.Context{Facts: facts}); err == nil {
				out = append(out, clean)
			}
		}
	}
	return out
}

func chartFromTable(t *RecordTable, hint *ChartHint) (conversation.ChartBlock, bool) {
	if t == nil || hint == nil || len(t.Rows) == 0 {
		return conversation.ChartBlock{}, false
	}
	mark := conversation.ChartBlockMark(hint.Type)
	if !mark.Valid() {
		return conversation.ChartBlock{}, false
	}
	xi, yi := -1, -1
	for i, c := range t.Columns {
		if string(c.Key) == hint.X {
			xi = i
		}
		if string(c.Key) == hint.Y {
			yi = i
		}
	}
	if xi < 0 || yi < 0 {
		return conversation.ChartBlock{}, false
	}
	data := make([]conversation.ChartRecord, 0, len(t.Rows))
	for _, row := range t.Rows {
		if xi >= len(row) || yi >= len(row) {
			continue
		}
		data = append(data, conversation.ChartRecord{hint.X: row[xi], hint.Y: row[yi]})
	}
	if len(data) == 0 {
		return conversation.ChartBlock{}, false
	}
	desc := conversation.PlainText(fmt.Sprintf("%s of %s by %s.", hint.Type, hint.Y, hint.X))
	return conversation.ChartBlock{
		Type: "chart", Description: desc, Mark: mark, Data: data,
		Encoding: conversation.ChartEncoding{
			X: &conversation.ChartChannel{Field: common.FieldName(hint.X), Type: conversation.ChartChannelTypeNominal},
			Y: &conversation.ChartChannel{Field: common.FieldName(hint.Y), Type: conversation.ChartChannelTypeQuantitative},
		},
	}, true
}
