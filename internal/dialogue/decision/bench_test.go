package decision

import (
	"testing"

	"github.com/buxee/alix-ai/internal/dialogue"
)

// The benchmarks run the layer against the clinic world on the dev box with an instant fake
// scorer and an in-memory audit appender: they measure this layer's own cost (the policy
// gateway calls of the resolver dominate), never a model. See docs/efficiency/D-04.md.

func benchRequest(utterance string) dialogue.DecisionRequest {
	return dialogue.DecisionRequest{
		Scope: recep(), Utterance: utterance, Language: "en",
		Candidates: []dialogue.DecisionCandidate{{CapabilityID: "appointments.book"}, {CapabilityID: "appointments.cancel"}, {CapabilityID: "availability.list"}},
	}
}

func BenchmarkExactRule(b *testing.B) {
	cs := []Named{
		{ID: "appointments.book", Names: []string{"appointments.book", "Book an appointment"}},
		{ID: "appointments.cancel", Names: []string{"appointments.cancel", "Cancel an appointment"}},
		{ID: "availability.list", Names: []string{"availability.list", "List availability"}},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ExactCapability("Book an appointment", cs)
	}
}

func BenchmarkStageDisabled(b *testing.B) {
	w := newWorld(b, withDisabledLayer())
	st, req := w.layer.Stage(), benchRequest(sayBook)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st.Decide(ctxb(), req)
	}
}

func BenchmarkStageExact(b *testing.B) {
	w := newWorld(b)
	st, req := w.layer.Stage(), benchRequest(sayExact)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r := st.Decide(ctxb(), req); r.Selected == nil {
			b.Fatalf("no selection: %+v", r)
		}
	}
}

func BenchmarkStageScorerSelected(b *testing.B) {
	w := newWorld(b, withScript(pick("availability.list", 0.97)))
	st, req := w.layer.Stage(), benchRequest(sayBook)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r := st.Decide(ctxb(), req); r.Selected == nil {
			b.Fatalf("no selection: %+v", r)
		}
	}
}
