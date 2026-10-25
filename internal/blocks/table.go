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
func (v *Validator) checkTable(t conversation.TableBlock, col *collector) {
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
			if rule := checkCell(cell); rule != "" {
				col.add(rule, fmt.Sprintf("/rows/%d/%d", i, j))
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
// a plain scalar, that a string is clean text and that a number is finite.
func checkCell(cell any) Rule {
	switch x := cell.(type) {
	case nil, bool:
		return ""
	case string:
		return textRule(x, kindLine)
	default:
		_, isNum, finite := asNumber(cell)
		switch {
		case !isNum:
			return RuleTableKind
		case !finite:
			return RuleChartNumeric
		}
		return ""
	}
}
