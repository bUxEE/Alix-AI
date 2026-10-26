package exports

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// Dataset is one tabular view of a verified table or chart block.
type Dataset struct {
	ID      string
	Name    string // sheet / caption
	Headers []string
	Rows    [][]string
	Kinds   []cellKind // per column; empty means all text
}

type cellKind int

const (
	kindText cellKind = iota
	kindNumber
	kindBool
	kindDate
)

// datasetsFromBlocks builds one dataset per table and chart in blocks, in order.
func datasetsFromBlocks(blocks []IdentifiedBlock) []Dataset {
	var out []Dataset
	for _, b := range blocks {
		switch v := b.Block.Value.(type) {
		case conversation.TableBlock:
			out = append(out, datasetFromTable(b.ID, v))
		case conversation.ChartBlock:
			out = append(out, datasetFromChart(b.ID, v))
		}
	}
	return out
}

func datasetFromTable(id string, t conversation.TableBlock) Dataset {
	headers := make([]string, len(t.Columns))
	kinds := make([]cellKind, len(t.Columns))
	for i, c := range t.Columns {
		headers[i] = c.Label
		kinds[i] = kindText
		if c.Kind != nil {
			switch *c.Kind {
			case conversation.TableColumnKindNumber:
				kinds[i] = kindNumber
			case conversation.TableColumnKindBoolean:
				kinds[i] = kindBool
			case conversation.TableColumnKindDate:
				kinds[i] = kindDate
			}
		}
	}
	rows := make([][]string, len(t.Rows))
	for i, row := range t.Rows {
		cells := make([]string, len(row))
		for j, cell := range row {
			cells[j] = cellString(cell)
		}
		rows[i] = cells
	}
	name := "table"
	if t.Caption != nil && strings.TrimSpace(*t.Caption) != "" {
		name = strings.TrimSpace(*t.Caption)
	}
	return Dataset{ID: id, Name: name, Headers: headers, Rows: rows, Kinds: kinds}
}

func datasetFromChart(id string, c conversation.ChartBlock) Dataset {
	fields := chartFieldOrder(c)
	headers := make([]string, len(fields))
	kinds := make([]cellKind, len(fields))
	for i, f := range fields {
		headers[i] = f.label
		if f.quantitative {
			kinds[i] = kindNumber
		} else if f.temporal {
			kinds[i] = kindDate
		}
	}
	rows := make([][]string, 0, len(c.Data))
	for _, rec := range c.Data {
		cells := make([]string, len(fields))
		for i, f := range fields {
			cells[i] = cellString(rec[f.key])
		}
		rows = append(rows, cells)
	}
	name := "chart"
	if c.Title != nil && strings.TrimSpace(string(*c.Title)) != "" {
		name = strings.TrimSpace(string(*c.Title))
	} else if strings.TrimSpace(string(c.Description)) != "" {
		name = strings.TrimSpace(string(c.Description))
	}
	return Dataset{ID: id, Name: name, Headers: headers, Rows: rows, Kinds: kinds}
}

type chartField struct {
	key          string
	label        string
	quantitative bool
	temporal     bool
}

func chartFieldOrder(c conversation.ChartBlock) []chartField {
	type entry struct {
		order int
		f     chartField
	}
	var list []entry
	add := func(order int, ch *conversation.ChartChannel) {
		if ch == nil {
			return
		}
		label := string(ch.Field)
		if ch.Title != nil && strings.TrimSpace(string(*ch.Title)) != "" {
			label = strings.TrimSpace(string(*ch.Title))
		}
		list = append(list, entry{order, chartField{
			key: string(ch.Field), label: label,
			quantitative: ch.Type == conversation.ChartChannelTypeQuantitative,
			temporal:     ch.Type == conversation.ChartChannelTypeTemporal,
		}})
	}
	add(0, c.Encoding.X)
	add(1, c.Encoding.Y)
	add(2, c.Encoding.Color)
	add(3, c.Encoding.Theta)
	sort.SliceStable(list, func(i, j int) bool { return list[i].order < list[j].order })
	// Deduplicate fields that feed more than one channel.
	seen := map[string]bool{}
	out := make([]chartField, 0, len(list))
	for _, e := range list {
		if seen[e.f.key] {
			continue
		}
		seen[e.f.key] = true
		out = append(out, e.f)
	}
	if len(out) == 0 && len(c.Data) > 0 {
		keys := make([]string, 0, len(c.Data[0]))
		for k := range c.Data[0] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, chartField{key: k, label: k})
		}
	}
	return out
}

func cellString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case json.Number:
		return x.String()
	default:
		return fmt.Sprint(v)
	}
}
