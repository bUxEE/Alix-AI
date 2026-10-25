package answer

import (
	"strings"
	"testing"
)

// The cost of claim verification, which is the whole of the core's work after the model
// call: a package of 8 passages of about 380 tokens each (the Lite budget is 3000) and
// 12 claims, each citing two passages. It is the efficiency evidence for the no-model
// choice (master plan rule 16).
func BenchmarkVerifyClaims(b *testing.B) {
	cfg := DefaultConfig()
	var texts []string
	for i := 0; i < 8; i++ {
		var sb strings.Builder
		for j := 0; j < 12; j++ {
			sb.WriteString("Section ")
			sb.WriteString(itoa(i))
			sb.WriteString(" clause ")
			sb.WriteString(itoa(j))
			sb.WriteString(": customers may return unopened products within ")
			sb.WriteString(itoa(10 + j))
			sb.WriteString(" days of delivery, and refunds are processed within ")
			sb.WriteString(itoa(2 + i))
			sb.WriteString(" business days after the product is received by the warehouse.\n")
		}
		texts = append(texts, sb.String())
	}
	handles := map[string]int{}
	for i := range texts {
		handles["e"+itoa(i+1)] = i
	}
	var proposed []modelClaim
	for i := 0; i < 12; i++ {
		proposed = append(proposed, modelClaim{
			Text:        "Customers may return unopened products within " + itoa(10+i) + " days of delivery.",
			EvidenceIDs: []string{"e" + itoa(i%8+1), "e" + itoa((i+3)%8+1)},
		})
	}
	a := &Answerer{cfg: cfg}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		var d Diagnostics
		if kept := a.verifyClaims(proposed, texts, handles, &d); len(kept) == 0 {
			b.Fatal("nothing survived")
		}
	}
}
