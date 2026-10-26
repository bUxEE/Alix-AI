package blocks

import (
	"fmt"

	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// checkTable holds a table to what the schema cannot say: it is bounded in
// rows, columns and total cells (the schema alone allows 1000 rows of 32
// cells of 1024 characters, a block of tens of megabytes), every row has one
// cell per column, keys are unique and every cell is a clean scalar. A table is never truncated to fit: a table that states
// application data and silently shows part of it would be a wrong answer.
// When Context.Facts is set, every finite number in a cell must be one of those
// facts (the same rule as a chart): a number the application did not state is refused.
func (v *Validator) checkTable(t conversation.TableBlock, ctx Context, col *collector) {
	lim := v.limits
	if len(t.Columns) > lim.MaxTableColumns || len(t.Rows) > lim.MaxTableRows ||
		len(t.Columns)*len(t.Rows) > lim.MaxTableCells {
		col.add(RuleTableBounds, "/rows")
		return
	}
	if t.Caption != nil {
		if rule := textRule(*t.Caption, kindLine); rule != "" {
			col.add(rule, "/caption")
		}
	}
	seen := make(map[string]bool, len(t.Columns))
	for i, c := range t.Columns {
		path := fmt.Sprintf("/columns/%d", i)
		if seen[string(c.Key)] {
			col.add(RuleTableKeys, path+"/key")
		}
		seen[string(c.Key)] = true
		if rule := textRule(c.Label, kindLine); rule != "" {
			col.add(rule, path+"/label")
		}
	}
	for i, row := range t.Rows {
		if len(row) != len(t.Columns) {
			col.add(RuleTableShape, fmt.Sprintf("/rows/%d", i))
			continue
		}
		for j, cell := range row {
			path := fmt.Sprintf("/rows/%d/%d", i, j)
			if rule := checkCell(cell, ctx); rule != "" {
				col.add(rule, path)
			}
		}
		if col.failed() && len(col.vs) >= maxReportedViolations {
			return
		}
	}
}

// checkCell checks one cell. The column kind is a formatting hint for the
// renderer (the published valid examples put a number in a boolean column), so
// it does not decide what a cell may hold; what is checked is that the cell is
// a plain scalar, that a string is clean text, that a number is finite, and —
// when facts are supplied — that every number is one the application stated.
func checkCell(cell any, ctx Context) Rule {
	switch x := cell.(type) {
	case nil, bool:
		return ""
	case string:
		if rule := textRule(x, kindLine); rule != "" {
			return rule
		}
		if r, ok := numericLiteral(x); ok && !ctx.Facts.has(r) {
			return RuleChartNumber
		}
		return ""
	default:
		r, isNum, finite := asNumber(cell)
		switch {
		case !isNum:
			return RuleTableKind
		case !finite:
			return RuleChartNumeric
		case !ctx.Facts.has(r):
			return RuleChartNumber
		}
		return ""
	}
}
