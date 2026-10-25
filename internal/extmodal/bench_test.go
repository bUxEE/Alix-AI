package extmodal

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/providers"
)

func benchService(b *testing.B) (*Service, *MemoryPolicy) {
	b.Helper()
	pol := NewMemoryPolicy()
	pol.SetProvider(provID, Enable(true, All()...))
	pol.SetTenant(scopeA, Enable(true, All()...))
	svc, err := New(Deps{Policy: pol, Egress: &recEgress{}})
	if err != nil {
		b.Fatal(err)
	}
	return svc, pol
}

// BenchmarkAdmit is the cost of the modality gate itself (policy read, profile and capability checks,
// disclosure). The efficiency record quotes it as the overhead the gate adds to a call.
func BenchmarkAdmit(b *testing.B) {
	svc, _ := benchService(b)
	t := fullTarget()
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := svc.Admit(ctx, scopeA, t, ModVision); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIngestText is the cost of sanitising a typical 1 KiB provider answer into a markdown block.
func BenchmarkIngestText(b *testing.B) {
	svc, _ := benchService(b)
	t := fullTarget()
	ctx := context.Background()
	text := ""
	for len(text) < 1024 {
		text += "The **visit** is on Tuesday at 10:00 in room 4, see [the map](https://example.org/map). "
	}
	res := providers.RichResult{Parts: []providers.ContentPart{providers.TextPart(text)}}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := svc.Ingest(ctx, scopeA, t, res, IngestOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}
