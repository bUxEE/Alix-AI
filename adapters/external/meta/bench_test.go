package meta

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/buxee/alix-ai/internal/providers"
)

// TestAdapterOverheadLoopback measures the adapter's own cost of one
// typed-proposal call (request build, schema conversion, loopback HTTP round
// trip to a fake that answers from memory, response decode, pricing) and logs it
// (run with -v). It says nothing about a provider's latency; it only fails if the
// call fails.
func TestAdapterOverheadLoopback(t *testing.T) {
	body := toolResponse("toolu_01Fixture", "book", `{"patient":"Luca Ferri","slot":"2026-10-14T10:30"}`, 900, 40)
	r := newRig(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}, nil)
	req := userReq("Book Luca Ferri for Wednesday.")
	req.Tools = []providers.ToolSpec{{Name: "book", Parameters: json.RawMessage(bookSchema)}}
	ctx := context.Background()
	var failed error
	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := r.client.Generate(ctx, testScope(), req); err != nil {
				failed = err
				b.Fatal(err)
			}
		}
	})
	if failed != nil {
		t.Fatal(failed)
	}
	t.Logf("one typed-proposal call over loopback: %d ns/op, %d B/op, %d allocs/op (n=%d)", res.NsPerOp(), res.AllocedBytesPerOp(), res.AllocsPerOp(), res.N)
}
