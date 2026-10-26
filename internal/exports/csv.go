package exports

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"
)

// renderCSV writes datasets as RFC 4180 CSV. Multiple datasets are separated by
// a blank line and a "# <name>" comment line (Excel ignores lines starting with #).
func renderCSV(sets []Dataset, excelBOM bool) ([]byte, error) {
	if len(sets) == 0 {
		return nil, fmt.Errorf("no tabular data")
	}
	var buf bytes.Buffer
	if excelBOM {
		buf.Write([]byte{0xEF, 0xBB, 0xBF})
	}
	w := csv.NewWriter(&buf)
	w.UseCRLF = true
	for i, set := range sets {
		if i > 0 {
			w.Flush()
			buf.WriteString("\r\n")
		}
		if len(sets) > 1 {
			_ = w.Write([]string{"# " + sanitizeSheetName(set.Name)})
		}
		headers := make([]string, len(set.Headers))
		for j, h := range set.Headers {
			headers[j] = guardFormula(h)
		}
		if err := w.Write(headers); err != nil {
			return nil, err
		}
		for _, row := range set.Rows {
			out := make([]string, len(row))
			for j, cell := range row {
				out[j] = guardFormula(cell)
			}
			if err := w.Write(out); err != nil {
				return nil, err
			}
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sanitizeSheetName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Sheet"
	}
	repl := strings.NewReplacer(":", " ", "\\", " ", "/", " ", "?", " ", "*", " ", "[", " ", "]", " ")
	s = repl.Replace(s)
	if len(s) > 31 {
		s = s[:31]
	}
	return s
}
