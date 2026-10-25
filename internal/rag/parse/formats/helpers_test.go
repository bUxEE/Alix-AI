package formats

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// testLimits are generous limits for tests that are not about limits.
func testLimits() wire.Limits {
	return wire.Limits{
		MaxInputBytes:     64 << 20,
		MaxOutputBytes:    16 << 20,
		MaxExpandedBytes:  128 << 20,
		MaxExpansionRatio: 100,
		MaxEntries:        2000,
		MaxDepth:          100,
		MaxPages:          5000,
		MaxBlocks:         400000,
	}
}

func mustParse(t *testing.T, f wire.Format, data []byte) *Output {
	t.Helper()
	out, err := Parse(f, data, testLimits())
	if err != nil {
		t.Fatalf("Parse(%s): %v", f, err)
	}
	checkOutput(t, out)
	return out
}

// checkOutput validates an output the way the host and the chunker will.
func checkOutput(t *testing.T, out *Output) {
	t.Helper()
	r := &wire.Result{TextSize: int64(len(out.Text)), PageStarts: out.PageStarts, Blocks: out.Blocks}
	if err := r.Validate(out.Text, 0); err != nil {
		t.Fatalf("output is not valid: %v\n%s", err, dump(out))
	}
}

// dump renders an output readably: one line per block.
func dump(o *Output) string {
	var sb strings.Builder
	for _, b := range o.Blocks {
		flag := ""
		if b.Hidden {
			flag = " HIDDEN(" + b.HiddenReason + ")"
		}
		switch b.Kind {
		case wire.KindHeading:
			fmt.Fprintf(&sb, "H%d %q%s\n", b.Level, string(o.Text[b.Start:b.End]), flag)
		case wire.KindTable:
			fmt.Fprintf(&sb, "TABLE header=%v rows=%d%s\n", b.HasHeader, len(b.Rows), flag)
			for _, r := range b.Rows {
				fmt.Fprintf(&sb, "  | %q\n", string(o.Text[r.Start:r.End]))
			}
		default:
			fmt.Fprintf(&sb, "%s %q%s\n", strings.ToUpper(string(b.Kind[:1])), string(o.Text[b.Start:b.End]), flag)
		}
	}
	if len(o.PageStarts) > 0 {
		fmt.Fprintf(&sb, "pages=%v\n", o.PageStarts)
	}
	if len(o.Warnings) > 0 {
		fmt.Fprintf(&sb, "warnings=%v\n", o.Warnings)
	}
	if len(o.NeedsOCR) > 0 {
		fmt.Fprintf(&sb, "needs_ocr=%v\n", o.NeedsOCR)
	}
	return sb.String()
}

// allocated is the number of bytes f allocates.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// hiddenText is the text of every hidden block, joined.
func hiddenText(o *Output) string {
	var parts []string
	for _, b := range o.Blocks {
		if b.Hidden {
			parts = append(parts, string(o.Text[b.Start:b.End]))
		}
	}
	return strings.Join(parts, "\n")
}

// visibleText is the text of every block that is not hidden, joined.
func visibleText(o *Output) string {
	var parts []string
	for _, b := range o.Blocks {
		if !b.Hidden {
			parts = append(parts, string(o.Text[b.Start:b.End]))
		}
	}
	return strings.Join(parts, "\n")
}
