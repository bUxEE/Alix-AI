package main

import (
	"math"
	"testing"
)

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.12f, want %.12f", name, got, want)
	}
}

func rate(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %v", name, want)
		return
	}
	if math.Abs(*got-want) > 5e-7 { // rates are rounded to six decimals
		t.Errorf("%s = %v, want %v", name, *got, want)
	}
}

func ratio(t *testing.T, name string, got Ratio, passed, total int) {
	t.Helper()
	if got.Passed != passed || got.Total != total {
		t.Errorf("%s = %d/%d, want %d/%d", name, got.Passed, got.Total, passed, total)
	}
	if total == 0 && got.Rate != nil {
		t.Errorf("%s has a rate with a zero total", name)
	}
	if total > 0 {
		rate(t, name+".rate", got.Rate, float64(passed)/float64(total))
	}
}

// TestMetricsAgainstHandComputed checks every metric against numbers worked out
// by hand, including a duplicate hit and a ranking with nothing relevant (the
// cases where a metric is easiest to get wrong).
//
// Discount of rank i is 1/log2(i+1): d(1) = 1, d(2) = 1/log2(3) = 0.630929753571,
// d(3) = 0.5, d(4) = 1/log2(5) = 0.430676558073.
func TestMetricsAgainstHandComputed(t *testing.T) {
	d1, d2, d3, d4 := 1.0, 0.6309297535714575, 0.5, 0.43067655807339306

	// A: three entries (0, 1, 2); rank 1 irrelevant, rank 2 hits entry 0, rank 3 hits
	// entry 0 again (a duplicate: not novel), rank 4 hits entries 1 and 2 at once.
	//   novel per rank   = 0, 1, 0, 2
	//   recall@1,2,3,4   = 0/3, 1/3, 1/3, 3/3
	//   MRR              = 1/2
	//   DCG@2            = d(2);            IDCG@2 = d(1) + d(2)
	//   DCG@4            = d(2) + d(4);     IDCG@4 = d(1) + d(2) + d(3)   (min(4, 3) = 3 ideal items)
	//   nDCG@2           = 0.6309297535714575 / 1.6309297535714575 = 0.386852807235
	//   nDCG@4           = 1.0616063116448506 / 2.1309297535714578 = 0.498189257466
	rankedA := [][]int{{}, {0}, {0}, {1, 2}}
	novelA := novelHits(rankedA)
	if got, want := novelA, []int{0, 1, 0, 2}; len(got) != 4 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("novelHits = %v, want %v", got, want)
	}
	for k, want := range map[int]float64{1: 0, 2: 1.0 / 3, 3: 1.0 / 3, 4: 1} {
		near(t, "A recall@"+string(rune('0'+k)), recallAt(novelA, 3, k), want)
	}
	near(t, "A MRR", reciprocalRank(novelA), 0.5)
	near(t, "A nDCG@1", ndcgAt(novelA, 3, 1), 0)
	near(t, "A nDCG@2", ndcgAt(novelA, 3, 2), d2/(d1+d2))
	near(t, "A nDCG@2 literal", ndcgAt(novelA, 3, 2), 0.386852807235)
	near(t, "A nDCG@4", ndcgAt(novelA, 3, 4), (d2+d4)/(d1+d2+d3))
	near(t, "A nDCG@4 literal", ndcgAt(novelA, 3, 4), 0.498189257466)

	// B: two entries, nothing relevant in the list: every metric is 0 (not NaN).
	novelB := novelHits([][]int{{}, {}, {}})
	near(t, "B recall@3", recallAt(novelB, 2, 3), 0)
	near(t, "B MRR", reciprocalRank(novelB), 0)
	near(t, "B nDCG@3", ndcgAt(novelB, 2, 3), 0)
	// An empty list is the same.
	near(t, "empty nDCG@5", ndcgAt(novelHits(nil), 2, 5), 0)

	// C: a perfect list of two entries. nDCG is 1 at every k, recall is 1/2 at k = 1.
	novelC := novelHits([][]int{{0}, {1}})
	near(t, "C recall@1", recallAt(novelC, 2, 1), 0.5)
	near(t, "C recall@2", recallAt(novelC, 2, 2), 1)
	near(t, "C MRR", reciprocalRank(novelC), 1)
	near(t, "C nDCG@1", ndcgAt(novelC, 2, 1), 1)
	near(t, "C nDCG@2", ndcgAt(novelC, 2, 2), 1)

	// D: one entry found at rank 3 only: recall@2 = 0, recall@3 = 1, MRR = 1/3,
	// nDCG@3 = d(3) / d(1) = 0.5.
	novelD := novelHits([][]int{{}, {}, {0}})
	near(t, "D recall@2", recallAt(novelD, 1, 2), 0)
	near(t, "D recall@3", recallAt(novelD, 1, 3), 1)
	near(t, "D MRR", reciprocalRank(novelD), 1.0/3)
	near(t, "D nDCG@3", ndcgAt(novelD, 1, 3), d3/d1)

	// Aggregates over seven hand-built records, k = 1 and 2.
	tr := true
	cit := func(correct bool) CitationRecord {
		return CitationRecord{Source: "s", Version: "v1", Availability: "available", Staleness: "current", Correct: correct}
	}
	recs := []QuestionRecord{
		// r1: answerable, list A, answered, fact ok, two citations (one correct).
		{ID: "r1", Outcome: OutcomeAnswer, Status: StatusOK, Expected: 3, Ranked: rankedA, Claims: 1,
			FactsHit: 1, FactsTotal: 1, Citations: []CitationRecord{cit(true), cit(false)}, Correct: true},
		// r2: answerable, nothing relevant retrieved, declined: a false decline.
		{ID: "r2", Outcome: OutcomeAnswer, Status: StatusOK, Expected: 2, Ranked: [][]int{{}, {}, {}}, Declined: true, FactsTotal: 1},
		// r3: answerable, perfect list, answered, two facts, one correct citation, label met.
		{ID: "r3", Outcome: OutcomeAnswer, Status: StatusOK, Expected: 2, Ranked: [][]int{{0}, {1}}, Claims: 1,
			FactsHit: 2, FactsTotal: 2, Citations: []CitationRecord{cit(true)}, LabelOK: &tr, Correct: true},
		// r4: to decline, declined, nothing leaked: correct.
		{ID: "r4", Outcome: OutcomeDecline, Status: StatusOK, Declined: true, Ranked: [][]int{}, Correct: true},
		// r5: to decline, but answered: an unsupported answer.
		{ID: "r5", Outcome: OutcomeDecline, Status: StatusOK, Ranked: [][]int{{}}, Claims: 1, Citations: []CitationRecord{cit(false)}},
		// r6: to decline, the pipeline failed: a failure of everything it touches.
		{ID: "r6", Outcome: OutcomeDecline, Status: StatusError, Ranked: [][]int{}},
		// r7: answerable, one entry never retrieved, answered with the wrong fact and a wrong citation.
		{ID: "r7", Outcome: OutcomeAnswer, Status: StatusOK, Expected: 1, Ranked: [][]int{{}}, Claims: 1,
			FactsHit: 0, FactsTotal: 1, Citations: []CitationRecord{cit(false)}},
	}
	m := computeMetrics(recs, []int{1, 2})

	if m.Questions != 7 || m.Errors != 1 || m.Leaks != 0 {
		t.Errorf("questions %d, errors %d, leaks %d; want 7, 1, 0", m.Questions, m.Errors, m.Leaks)
	}
	ratio(t, "correct", m.Correct, 3, 7) // r1, r3, r4

	// Four answerable questions with entries: r1, r2, r3, r7.
	//   recall@1 = (0 + 0 + 1/2 + 0) / 4 = 0.125
	//   recall@2 = (1/3 + 0 + 1 + 0) / 4 = 0.333333
	//   MRR      = (1/2 + 0 + 1 + 0) / 4 = 0.375
	//   nDCG@1   = (0 + 0 + 1 + 0) / 4 = 0.25
	//   nDCG@2   = (0.386852807235 + 0 + 1 + 0) / 4 = 0.346713201809
	if m.Retrieval.Questions != 4 {
		t.Errorf("retrieval questions = %d, want 4", m.Retrieval.Questions)
	}
	rate(t, "recall@1", m.Retrieval.RecallAtK[0], 0.125)
	rate(t, "recall@2", m.Retrieval.RecallAtK[1], 1.0/3)
	rate(t, "MRR", m.Retrieval.MRR, 0.375)
	rate(t, "nDCG@1", m.Retrieval.NDCGAtK[0], 0.25)
	rate(t, "nDCG@2", m.Retrieval.NDCGAtK[1], 0.346713201809)

	// Answers: r1, r3, r7 answered of 4 answerable; r1 and r3 correct; of the three
	// answered, r1 and r3 state their facts (r7 does not); citations: r1 two (one
	// right), r3 one (right), r7 one (wrong) = 2 of 4; one labelled question, met.
	// Of the three to decline: r4 declined cleanly (1/3), r5 answered (1/3), r6
	// errored (counts in neither: it is not an answer, and it is not a correct decline).
	a := m.Answers
	ratio(t, "answerable_answered", a.Answerable, 3, 4)
	ratio(t, "answerable_correct", a.AnswerCorrect, 2, 4)
	ratio(t, "fact_accuracy", a.FactAccuracy, 2, 3)
	ratio(t, "citation_precision", a.CitationPrecision, 2, 4)
	ratio(t, "citation_label_accuracy", a.CitationLabelAccuracy, 1, 1)
	ratio(t, "decline_correct", a.DeclineCorrect, 1, 3)
	ratio(t, "unsupported_answer_rate", a.UnsupportedAnswerRate, 1, 3)

	// No answerable question: the retrieval means are nil, not 0 and not NaN, and
	// every rate with a zero total has a nil rate.
	none := computeMetrics([]QuestionRecord{recs[3]}, []int{1})
	if none.Retrieval.MRR != nil || none.Retrieval.RecallAtK[0] != nil || none.Retrieval.NDCGAtK[0] != nil {
		t.Errorf("retrieval metrics of a set with no answerable question must be nil: %+v", none.Retrieval)
	}
	ratio(t, "decline_correct (one)", none.Answers.DeclineCorrect, 1, 1)
	ratio(t, "citation_precision (none)", none.Answers.CitationPrecision, 0, 0)

	// A leak is counted per question and a leaking question is never correct in
	// its own record (the judge sets that; here the count is what is checked).
	leaky := recs[3]
	leaky.Leak, leaky.ForbiddenSourceHits = true, 1
	if got := computeMetrics([]QuestionRecord{leaky}, []int{1}); got.Leaks != 1 {
		t.Errorf("leaks = %d, want 1", got.Leaks)
	}
	ratio(t, "decline_correct (leak)", computeMetrics([]QuestionRecord{leaky}, []int{1}).Answers.DeclineCorrect, 0, 1)
}

// TestPercentileNearestRank is the textbook nearest-rank example: for the
// samples 15, 20, 35, 40, 50 the 5th percentile is 15, the 30th and 40th are
// 20, the 50th is 35, the 95th and 100th are 50. (rank = ceil(p/100 * n).)
func TestPercentileNearestRank(t *testing.T) {
	xs := []float64{50, 15, 40, 20, 35} // unsorted on purpose
	for pct, want := range map[int]float64{5: 15, 30: 20, 40: 20, 50: 35, 95: 50, 100: 50} {
		got, ok := Percentile(xs, pct)
		if !ok || got != want {
			t.Errorf("p%d = %v (ok %v), want %v", pct, got, ok, want)
		}
	}
	if _, ok := Percentile(nil, 50); ok {
		t.Error("the percentile of no samples must not exist")
	}
	if _, ok := Percentile(xs, 0); ok {
		t.Error("p0 must not exist")
	}
	s := Summarize(xs)
	if s.N != 5 || *s.P50 != 35 || *s.P95 != 50 || *s.Min != 15 || *s.Max != 50 || *s.Mean != 32 {
		t.Errorf("Summarize = %+v", s)
	}
	if e := Summarize(nil); e.N != 0 || e.P50 != nil || e.Mean != nil {
		t.Errorf("Summarize(nil) = %+v, want all nil", e)
	}
}
