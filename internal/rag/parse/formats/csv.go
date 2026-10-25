package formats

import (
	"encoding/csv"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// parseCSV reads delimited text (comma, semicolon, tab or pipe, sniffed) as one
// table: a row per record, cells joined by " | ", the first row a header when it
// looks like one. Records are streamed into the output, so memory is the text
// plus the row spans, not a copy of the table as cells.
func parseCSV(data []byte, lim wire.Limits) (*Output, error) {
	var w warnings
	text := cleanText(decodeText(data, &w))
	if int64(len(text)) > lim.MaxOutputBytes {
		return nil, fail(wire.ReasonOutputTooLarge, "extracted text is %d bytes, over the limit of %d", len(text), lim.MaxOutputBytes)
	}
	b := newBuilder(lim)
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = sniffDelimiter(text)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.ReuseRecord = true
	tw := b.NewTable()
	header := false
	for n := 0; ; n++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, malformed("csv: %v", err)
		}
		if n == 0 {
			header = looksLikeHeader(rec)
		}
		cells := make([]string, len(rec))
		copy(cells, rec)
		if err := tw.AddStrings(cells); err != nil {
			return nil, err
		}
	}
	if err := tw.End(header); err != nil {
		return nil, err
	}
	return b.finish(&w), nil
}

// sniffDelimiter picks the delimiter that occurs most often, outside quotes, in
// the first lines of the text. Comma wins ties and the empty case.
func sniffDelimiter(text string) rune {
	if len(text) > 8192 {
		text = text[:8192]
	}
	counts := map[rune]int{}
	inQuote := false
	lines := 0
	for _, c := range text {
		switch {
		case c == '"':
			inQuote = !inQuote
		case c == '\n' && !inQuote:
			if lines++; lines >= 10 {
				goto done
			}
		case !inQuote && (c == ',' || c == ';' || c == '\t' || c == '|'):
			counts[c]++
		}
	}
done:
	best, bestN := ',', 0
	for _, d := range []rune{',', ';', '\t', '|'} {
		if counts[d] > bestN {
			best, bestN = d, counts[d]
		}
	}
	return best
}

// looksLikeHeader says whether the first record reads as column names: more
// than one cell, none empty, none a number.
func looksLikeHeader(rec []string) bool {
	if len(rec) < 2 {
		return false
	}
	for _, c := range rec {
		c = strings.TrimSpace(c)
		if c == "" {
			return false
		}
		if _, err := strconv.ParseFloat(strings.ReplaceAll(strings.Trim(c, "$%€£ "), ",", ""), 64); err == nil {
			return false
		}
	}
	return true
}
