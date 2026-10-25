package verify

import (
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// The benchmarks measure what a turn pays for verification: no model, no I/O, one directive and
// one response text. Run: go test -bench . -benchmem ./internal/verify/

func BenchmarkVerifyConfirmationEN(b *testing.B) {
	d := confirmDirective()
	b.ReportAllocs()
	b.SetBytes(int64(len(goodConfirmEN)))
	for i := 0; i < b.N; i++ {
		if res, err := Verify(goodConfirmEN, d, nil, en); err != nil || !res.OK {
			b.Fatal(res, err)
		}
	}
}

func BenchmarkVerifyConfirmationIT(b *testing.B) {
	d := confirmDirective()
	text := "Conferma: Dr. Rossi mercoledì 14 ottobre 2026 alle 10:30, costo 50,00 EUR. Procedo?"
	b.ReportAllocs()
	b.SetBytes(int64(len(text)))
	for i := 0; i < b.N; i++ {
		if res, err := Verify(text, d, nil, it); err != nil || !res.OK {
			b.Fatal(res, err)
		}
	}
}

func BenchmarkVerifyOutcomeWithReceipt(b *testing.B) {
	facts := []actions.Fact{idFact("appointment_id", "apt_20991"), dateFact("slot_date", "2026-10-14"), timeFact("slot_start", "10:30")}
	d := outcomeDirective(facts, "appointment_id", "slot_date", "slot_start")
	r := receipt(actions.ActionReceiptStatusCommitted, facts...)
	text := "Done: your appointment apt_20991 is booked for 14 October 2026 at 10:30."
	b.ReportAllocs()
	b.SetBytes(int64(len(text)))
	for i := 0; i < b.N; i++ {
		if res, err := Verify(text, d, r, en); err != nil || !res.OK {
			b.Fatal(res, err)
		}
	}
}

// A long answer, about 2 KB: a grounded answer with a few facts and citations.
func BenchmarkVerifyLongAnswer(b *testing.B) {
	d := confirmDirective()
	d.Sources = nil
	text := goodConfirmEN + " " + strings.Repeat("The clinic is open on weekdays and the front desk will help you with anything else you need. ", 20)
	b.ReportAllocs()
	b.SetBytes(int64(len(text)))
	for i := 0; i < b.N; i++ {
		if res, err := Verify(text, d, nil, en); err != nil || !res.OK {
			b.Fatal(res, err)
		}
	}
}

func BenchmarkRenderEN(b *testing.B) {
	d := confirmDirective()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Render(d, nil, en); err != nil {
			b.Fatal(err)
		}
	}
}

// The fallback path: verification fails, the template is rendered and verified in turn.
func BenchmarkRealiseFallback(b *testing.B) {
	d := confirmDirective()
	bad := "Booked! Dr. Rossi on 14 October 2026 at 10:30 for 60.00 EUR."
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if out, err := Realise(bad, d, nil, en); err != nil || out.Source != SourceTemplate {
			b.Fatal(out, err)
		}
	}
}
