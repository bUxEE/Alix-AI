package parse

import (
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

func TestWorkerSmoke(t *testing.T) {
	r := newTestRunner(t)
	res := mustParse(t, r, wire.FormatMD, []byte("# Hello\n\nA paragraph.\n"))
	if !strings.Contains(res.Text, "A paragraph.") {
		t.Fatalf("text = %q", res.Text)
	}
	t.Logf("sandbox=%+v stats=%+v", res.Sandbox, res.Stats)
}
