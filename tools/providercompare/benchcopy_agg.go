package main

import "time"

// Copied unchanged from tools/bench/scoring.go (Aggregate, filterCases, aggregateBy) and
// tools/bench/runner.go (durMS, summarizeLatency, summarizeErrors). See benchcopy_result.go.

const (
	CatRouting    = "routing"
	CatExtraction = "extraction"
	CatRefusal    = "refusal"
	CatInjection  = "injection"
)

// Aggregate computes the metrics over outcomes. It reads only the flags the
// scorer set, so the same function re-derives the aggregates of a stored
// result from its cases.
func Aggregate(cases []CaseOutcome) MetricSet {
	var m MetricSet
	var (
		valid, intent, catalogue, pass           int
		capOK, capTotal                          int
		fieldOK, fieldTotal, valueHits, valueN   int
		refusalOK, refusalTotal, resOK, resTotal int
	)
	for _, c := range cases {
		m.Cases++
		if c.SchemaValid {
			valid++
		}
		if c.IntentOK {
			intent++
		}
		if c.CatalogueOK {
			catalogue++
		}
		if c.Pass {
			pass++
		}
		if c.CapabilityOK != nil {
			capTotal++
			if *c.CapabilityOK {
				capOK++
			}
		}
		if c.FieldsExact != nil {
			fieldTotal++
			if *c.FieldsExact {
				fieldOK++
			}
			valueN += c.FieldTotal
			valueHits += c.FieldHits
		}
		if c.RefusalOK != nil {
			refusalTotal++
			if *c.RefusalOK {
				refusalOK++
			}
		}
		if c.Resisted != nil {
			resTotal++
			if *c.Resisted {
				resOK++
			}
		}
	}
	m.SchemaValid = newRatio(valid, m.Cases)
	m.Intent = newRatio(intent, m.Cases)
	m.CatalogueConformity = newRatio(catalogue, m.Cases)
	m.Pass = newRatio(pass, m.Cases)
	m.CapabilityRouting = newRatio(capOK, capTotal)
	m.FieldExactMatch = newRatio(fieldOK, fieldTotal)
	m.FieldValueAccuracy = newRatio(valueHits, valueN)
	m.RefusalCorrectness = newRatio(refusalOK, refusalTotal)
	m.InjectionResistance = newRatio(resOK, resTotal)
	return m
}

// filterCases returns the outcomes for which keep is true.
func filterCases(cases []CaseOutcome, keep func(CaseOutcome) bool) []CaseOutcome {
	var out []CaseOutcome
	for _, c := range cases {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

// aggregateBy builds the per-category and per-language blocks.
func aggregateBy(cases []CaseOutcome) (ByCategory, ByLanguage) {
	cat := func(name string) MetricSet {
		return Aggregate(filterCases(cases, func(c CaseOutcome) bool { return c.Category == name }))
	}
	lang := func(name string) MetricSet {
		return Aggregate(filterCases(cases, func(c CaseOutcome) bool { return c.Language == name }))
	}
	return ByCategory{
			Routing: cat(CatRouting), Extraction: cat(CatExtraction), Refusal: cat(CatRefusal), Injection: cat(CatInjection),
		},
		ByLanguage{EN: lang("en"), IT: lang("it")}
}

// durMS converts a duration to milliseconds with microsecond precision.
func durMS(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// summarizeLatency derives the latency block from the outcomes alone, which is
// how `bench check` verifies it.
func summarizeLatency(cases []CaseOutcome, warmupFirst *float64) LatencyInfo {
	var totals, ttfts, prompts []float64
	var toksIn, toksOut, decodeToks int
	var totalMS, decodeMS float64
	for _, c := range cases {
		if c.Status != StatusOK || c.LatencyMS == nil {
			continue
		}
		totals = append(totals, *c.LatencyMS)
		prompts = append(prompts, float64(c.PromptToksEst))
		totalMS += *c.LatencyMS
		out := 0
		if c.ToksOut != nil {
			out = *c.ToksOut
			toksOut += out
		}
		if c.ToksIn != nil {
			toksIn += *c.ToksIn
		}
		if c.TTFTMS != nil {
			ttfts = append(ttfts, *c.TTFTMS)
			if gen := *c.LatencyMS - *c.TTFTMS; gen > 0 {
				decodeMS += gen
				decodeToks += out
			}
		}
	}
	info := LatencyInfo{
		TotalMS: Summarize(totals), TTFTMS: Summarize(ttfts), PromptToksEst: Summarize(prompts),
		ToksIn: toksIn, ToksOut: toksOut, WarmupFirstRequestMS: warmupFirst,
	}
	if totalMS > 0 {
		info.ToksPerSecond = round3(float64(toksOut) / (totalMS / 1000))
	}
	if decodeMS > 0 {
		info.DecodeToksPerSecond = round3(float64(decodeToks) / (decodeMS / 1000))
	}
	return info
}

// summarizeErrors counts the cases that ended in an error by code.
func summarizeErrors(cases []CaseOutcome) ErrorsInfo {
	e := ErrorsInfo{ByCode: map[string]int{}}
	for _, c := range cases {
		if c.Status == StatusError {
			e.Count++
			if c.ErrorCode != nil {
				e.ByCode[*c.ErrorCode]++
			}
		}
	}
	return e
}
