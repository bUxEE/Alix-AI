package wire

import (
	"fmt"
	"unicode/utf8"
)

// Validate checks a result against its text, the way the chunker will, so a
// malformed result is refused at the boundary instead of becoming a citation
// that points nowhere. Both sides call it: the worker as a self-check, the host
// because everything the worker sends is untrusted. maxBlocks <= 0 means no
// cap.
func (r *Result) Validate(text []byte, maxBlocks int) error {
	if int64(len(text)) != r.TextSize {
		return fmt.Errorf("text is %d bytes, result declares %d", len(text), r.TextSize)
	}
	if !utf8.Valid(text) {
		return fmt.Errorf("text is not valid UTF-8")
	}
	onBoundary := func(i int) bool { return i == 0 || i == len(text) || utf8.RuneStart(text[i]) }
	if len(r.PageStarts) > 1<<20 {
		return fmt.Errorf("%d pages", len(r.PageStarts))
	}
	for i, p := range r.PageStarts {
		switch {
		case p < 0 || p > len(text):
			return fmt.Errorf("page start %d (page %d) is outside the text", p, i+1)
		case !onBoundary(p):
			return fmt.Errorf("page start %d (page %d) is inside a code point", p, i+1)
		case i > 0 && p <= r.PageStarts[i-1]:
			return fmt.Errorf("page starts are not strictly ascending at page %d", i+1)
		}
	}
	structural := len(r.Blocks)
	prevEnd := 0
	for i, b := range r.Blocks {
		switch {
		case b.Start < prevEnd:
			return fmt.Errorf("block %d starts at %d, before the previous block ends at %d", i, b.Start, prevEnd)
		case b.Start >= b.End:
			return fmt.Errorf("block %d has an empty or reversed span [%d,%d)", i, b.Start, b.End)
		case b.End > len(text):
			return fmt.Errorf("block %d ends at %d, past the %d-byte text", i, b.End, len(text))
		case !onBoundary(b.Start) || !onBoundary(b.End):
			return fmt.Errorf("block %d span [%d,%d) is inside a code point", i, b.Start, b.End)
		}
		switch b.Kind {
		case KindHeading:
			if b.Level < 1 || b.Level > 6 {
				return fmt.Errorf("block %d is a heading of level %d", i, b.Level)
			}
		case KindParagraph, KindCode, KindTable:
			if b.Level != 0 || b.Title != "" {
				return fmt.Errorf("block %d (%s) carries a heading level or title", i, b.Kind)
			}
		default:
			return fmt.Errorf("block %d has unknown kind %q", i, b.Kind)
		}
		if b.Hidden != (b.HiddenReason != "") {
			return fmt.Errorf("block %d hidden flag and reason disagree", i)
		}
		if b.Kind != KindTable {
			if len(b.Rows) != 0 || b.HasHeader {
				return fmt.Errorf("block %d (%s) carries table rows", i, b.Kind)
			}
		} else {
			if len(b.Rows) == 0 {
				return fmt.Errorf("block %d is a table without rows", i)
			}
			structural += len(b.Rows)
			prev := b.Start
			for j, row := range b.Rows {
				switch {
				case row.Start < prev || row.End > b.End:
					return fmt.Errorf("block %d row %d is outside the table or overlaps the previous row", i, j)
				case row.Start >= row.End:
					return fmt.Errorf("block %d row %d has an empty or reversed span", i, j)
				case !onBoundary(row.Start) || !onBoundary(row.End):
					return fmt.Errorf("block %d row %d is inside a code point", i, j)
				}
				prev = row.End
			}
		}
		if maxBlocks > 0 && structural > maxBlocks {
			return fmt.Errorf("more than %d blocks and rows", maxBlocks)
		}
		prevEnd = b.End
	}
	return nil
}
