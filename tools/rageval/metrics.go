package main

import (
	"math"
	"sort"
)

// The metrics of master plan 22.4, defined once, here, and re-derived by
// `rageval check` from the raw per-question records of a result, so that a
// result edited by hand, or written by a tool with another idea of a metric,
// does not pass.
//
// # Retrieval (answerable questions only)
//
// The unit of relevance is the evidence entry of the question's label
// (corpus.Evidence): a document of a source and the anchor strings that must be
// in the passage. The ranked list is the evidence package, in the rank the
// package gives its items (the rank the model sees), cut to k.
//
//   - An item HITS an entry when it comes from that document of that source and
//     its text holds every anchor.
//   - An item is NOVEL when it hits at least one entry no earlier item hit. An
//     item that hits only entries already hit (a duplicate, or the parent
//     section of a child already in the list) and an item that hits none are
//     not novel: they add nothing.
//   - recall@k = entries hit by the first k items / entries.
//   - MRR = 1 / rank of the first novel item, 0 when there is none.
//   - nDCG@k is binary: DCG@k = sum over novel items at rank i <= k of
//     1/log2(i+1); the ideal list puts one entry at every rank from 1 to
//     min(k, entries), so IDCG@k = sum for i = 1..min(k, entries) of 1/log2(i+1).
//
// Questions that must be declined are not in any of the three: there is nothing
// to retrieve for them, and they count against the unsupported-answer rate
// instead.
//
// # Answers
//
//   - answer rate: answerable questions that got a grounded answer.
//   - fact accuracy: of those answers, the ones that state every labelled fact
//     and no forbidden string.
//   - citation precision: of the citations of those answers, the share that point at
//     labelled evidence (a citation is correct when the passage it cites hits an
//     entry).
//   - citation label accuracy: of the questions that label a citation (a
//     version, an availability, a staleness), the ones whose citations carry
//     the label.
//   - unsupported-answer rate: of the questions that must be declined, the
//     share that got an answer. Lower is better; 0 is the target.
//   - decline correctness: of the same questions, the share that were declined
//     with nothing forbidden anywhere.
//   - leaks: questions in which a passage of a forbidden source reached the
//     evidence package or a citation, or a forbidden string reached an answer
//     or a passage. The target is 0, always.
//   - correct: the question's own pass. An answerable question passes when it is
//     answered, states every fact, leaks nothing, cites labelled evidence and
//     meets its citation label; a question to decline passes when it is
//     declined and leaks nothing. An error is a failure of everything it
//     touches (fail closed), never skipped.

// CitationRecord is one citation of an answer, judged.
type CitationRecord struct {
	Source       string `json:"source"`
	Version      string `json:"version"`
	Availability string `json:"availability"`
	Staleness    string `json:"staleness"`
	// Correct is true when the passage the citation points at hits an entry of
	// the question's label.
	Correct bool `json:"correct"`
}

// QuestionRecord is the raw outcome of one question: what the pipeline did and
// every flag the judge derived from it. The metrics are a pure function of a
// slice of these (computeMetrics).
type QuestionRecord struct {
	ID       string `json:"id"`
	Language string `json:"language"`
	Kind     string `json:"kind"`
	// Outcome is what a correct system does (OutcomeAnswer, OutcomeDecline).
	Outcome string `json:"outcome"`
	// Status is ok when the pipeline answered (with an answer or a decline) and
	// error when it failed; ErrorCode says why.
	Status    string  `json:"status"`
	ErrorCode *string `json:"error_code"`

	// Expected is the number of evidence entries of the label.
	Expected int `json:"expected"`
	// Ranked is, for each item of the evidence package in rank order, the
	// indexes of the label's entries it hits (ascending).
	Ranked [][]int `json:"ranked"`
	// Mode is how the query was served (hybrid, fts_only) and Degraded why a
	// stage did not contribute.
	Mode     string   `json:"mode"`
	Degraded []string `json:"degraded"`

	// Declined is true when the result was no_supported_evidence, and Reason
	// its reason. Claims is the number of claims of an answer.
	Declined bool   `json:"declined"`
	Reason   string `json:"reason"`
	Claims   int    `json:"claims"`

	FactsHit   int `json:"facts_hit"`
	FactsTotal int `json:"facts_total"`
	// ForbiddenHits counts forbidden strings found in the answer or in the
	// passages of the package; ForbiddenSourceHits counts package items and
	// citations of a forbidden source.
	ForbiddenHits       int  `json:"forbidden_hits"`
	ForbiddenSourceHits int  `json:"forbidden_source_hits"`
	Leak                bool `json:"leak"`

	Citations []CitationRecord `json:"citations"`
	// LabelOK is nil when the question labels no citation.
	LabelOK *bool `json:"label_ok"`

	Correct bool `json:"correct"`

	// Measurements. Latencies are in milliseconds (nil on an error).
	LatencyMS          *float64 `json:"latency_ms"`
	RetrievalLatencyMS *float64 `json:"retrieval_latency_ms"`
	AnswerLatencyMS    *float64 `json:"answer_latency_ms"`
	// StagesMS is the retrieval pipeline's own timing of the question, per stage
	// (fts, embed, vector, hydrate, expand, rerank, recheck; a stage that did not
	// run is 0), empty on an error. embed is the query-embedding latency.
	StagesMS       map[string]float64 `json:"stages_ms"`
	PackageToksEst int                `json:"package_toks_est"`
	ToksIn         int                `json:"toks_in"`
	ToksOut        int                `json:"toks_out"`
}

// novelHits returns, for each rank (position i is rank i+1), how many entries
// of the label were first hit there.
func novelHits(ranked [][]int) []int {
	seen := map[int]bool{}
	out := make([]int, len(ranked))
	for i, hits := range ranked {
		for _, e := range hits {
			if !seen[e] {
				seen[e] = true
				out[i]++
			}
		}
	}
	return out
}

// recallAt is the share of the nExpect entries hit by the first k items.
func recallAt(novel []int, nExpect, k int) float64 {
	if nExpect <= 0 {
		return 0
	}
	hit := 0
	for i := 0; i < len(novel) && i < k; i++ {
		hit += novel[i]
	}
	return float64(hit) / float64(nExpect)
}

// reciprocalRank is 1 over the rank of the first novel item, 0 when none is.
func reciprocalRank(novel []int) float64 {
	for i, n := range novel {
		if n > 0 {
			return 1 / float64(i+1)
		}
	}
	return 0
}

// ndcgAt is the binary nDCG@k of the ranking (see the package comment).
func ndcgAt(novel []int, nExpect, k int) float64 {
	if nExpect <= 0 || k <= 0 {
		return 0
	}
	dcg := 0.0
	for i := 0; i < len(novel) && i < k; i++ {
		if novel[i] > 0 {
			dcg += 1 / math.Log2(float64(i+2))
		}
	}
	idcg := 0.0
	for i := 0; i < min(k, nExpect); i++ {
		idcg += 1 / math.Log2(float64(i+2))
	}
	return dcg / idcg
}

// RetrievalMetrics are the means of the retrieval metrics over the answerable
// questions.
type RetrievalMetrics struct {
	// Questions is the number of answerable questions with at least one entry.
	Questions int   `json:"questions"`
	K         []int `json:"k"`
	// RecallAtK and NDCGAtK are parallel to K. MRR is over the whole list.
	RecallAtK []*float64 `json:"recall_at_k"`
	MRR       *float64   `json:"mrr"`
	NDCGAtK   []*float64 `json:"ndcg_at_k"`
}

// AnswerMetrics are the answer-stage metrics.
type AnswerMetrics struct {
	Answerable            Ratio `json:"answerable_answered"`
	AnswerCorrect         Ratio `json:"answerable_correct"`
	FactAccuracy          Ratio `json:"fact_accuracy"`
	CitationPrecision     Ratio `json:"citation_precision"`
	CitationLabelAccuracy Ratio `json:"citation_label_accuracy"`
	DeclineCorrect        Ratio `json:"decline_correct"`
	UnsupportedAnswerRate Ratio `json:"unsupported_answer_rate"`
}

// Metrics is every metric over some set of questions.
type Metrics struct {
	Questions int              `json:"questions"`
	Errors    int              `json:"errors"`
	Correct   Ratio            `json:"correct"`
	Leaks     int              `json:"leaks"`
	Retrieval RetrievalMetrics `json:"retrieval"`
	Answers   AnswerMetrics    `json:"answers"`
}

func meanOf(xs []float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return round6(s / float64(len(xs)))
}

// computeMetrics derives every metric from the records. ks are the cut-offs of
// recall@k and nDCG@k.
func computeMetrics(recs []QuestionRecord, ks []int) Metrics {
	m := Metrics{Questions: len(recs)}
	m.Retrieval.K = append([]int{}, ks...)
	recall := make([][]float64, len(ks))
	ndcg := make([][]float64, len(ks))
	var mrr []float64

	var (
		correct                                     int
		answerable, answered, answeredCorrect       int
		factOK, nAnswered                           int
		cites, citesOK                              int
		labelled, labelOK                           int
		mustDecline, declinedClean, wronglyAnswered int
	)
	for _, r := range recs {
		if r.Status != StatusOK {
			m.Errors++
		}
		if r.Correct {
			correct++
		}
		if r.Leak {
			m.Leaks++
		}
		switch r.Outcome {
		case OutcomeAnswer:
			answerable++
			if r.Expected > 0 {
				m.Retrieval.Questions++
				novel := novelHits(r.Ranked)
				for i, k := range ks {
					recall[i] = append(recall[i], recallAt(novel, r.Expected, k))
					ndcg[i] = append(ndcg[i], ndcgAt(novel, r.Expected, k))
				}
				mrr = append(mrr, reciprocalRank(novel))
			}
			if r.Status == StatusOK && !r.Declined {
				answered++
				nAnswered++
				if r.FactsHit == r.FactsTotal && r.ForbiddenHits == 0 {
					factOK++
				}
				for _, c := range r.Citations {
					cites++
					if c.Correct {
						citesOK++
					}
				}
				if r.Correct {
					answeredCorrect++
				}
			}
			if r.LabelOK != nil {
				labelled++
				if *r.LabelOK {
					labelOK++
				}
			}
		case OutcomeDecline:
			mustDecline++
			if r.Status == StatusOK && !r.Declined {
				wronglyAnswered++
			}
			if r.Status == StatusOK && r.Declined && !r.Leak {
				declinedClean++
			}
		}
	}
	m.Correct = newRatio(correct, len(recs))
	for i := range ks {
		m.Retrieval.RecallAtK = append(m.Retrieval.RecallAtK, meanOf(recall[i]))
		m.Retrieval.NDCGAtK = append(m.Retrieval.NDCGAtK, meanOf(ndcg[i]))
	}
	m.Retrieval.MRR = meanOf(mrr)
	m.Answers = AnswerMetrics{
		Answerable:            newRatio(answered, answerable),
		AnswerCorrect:         newRatio(answeredCorrect, answerable),
		FactAccuracy:          newRatio(factOK, nAnswered),
		CitationPrecision:     newRatio(citesOK, cites),
		CitationLabelAccuracy: newRatio(labelOK, labelled),
		DeclineCorrect:        newRatio(declinedClean, mustDecline),
		UnsupportedAnswerRate: newRatio(wronglyAnswered, mustDecline),
	}
	return m
}

// groupBy splits the records by a key and computes the metrics of each group.
func groupBy(recs []QuestionRecord, ks []int, key func(QuestionRecord) string) map[string]Metrics {
	groups := map[string][]QuestionRecord{}
	for _, r := range recs {
		k := key(r)
		groups[k] = append(groups[k], r)
	}
	out := make(map[string]Metrics, len(groups))
	for k, g := range groups {
		out[k] = computeMetrics(g, ks)
	}
	return out
}

// sortedKeys returns the keys of m in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Question statuses.
const (
	StatusOK    = "ok"
	StatusError = "error"
)
