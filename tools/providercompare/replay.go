package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
)

// CorpusRun is one provider's replay of one shared corpus.
type CorpusRun struct {
	Corpus string `json:"corpus"`
	// Cases is the number of tasks in the corpus, Sent how many reached the stub, NotSent how
	// many are decided locally by design (RAG declines).
	Cases   int `json:"cases"`
	Sent    int `json:"sent"`
	NotSent int `json:"not_sent"`
	// Decoded is how many sent tasks came back exactly as the stub played them: the adapter
	// carried the reply through without loss. It is a wire-fidelity count, not model quality.
	Decoded int `json:"decoded"`
	// Errors counts sent tasks the adapter refused.
	Errors int `json:"errors"`
	// ExcerptKept counts RAG tasks whose request body held the excerpt (RAG only).
	ExcerptKept int `json:"excerpt_kept"`
	// ExcerptUntrusted counts RAG tasks whose excerpt travelled outside the system instruction.
	ExcerptUntrusted int     `json:"excerpt_untrusted"`
	ToksIn           int     `json:"toks_in"`
	ToksOut          int     `json:"toks_out"`
	CostMicros       int64   `json:"cost_micros"`
	Currency         string  `json:"currency"`
	RequestBytesMean float64 `json:"request_bytes_mean"`
	// OverheadMS is the per-call time with no injected latency: the adapter, the egress dialer
	// and a loopback round trip.
	OverheadP50MS float64 `json:"overhead_p50_ms"`
	OverheadP95MS float64 `json:"overhead_p95_ms"`
	Label         string  `json:"label"`
}

// pctl is the nearest-rank percentile, the definition tools/bench uses.
func pctl(xs []float64, pct int) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	rank := (pct*len(s) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(s) {
		rank = len(s)
	}
	return s[rank-1]
}

func roundTo(x float64, places int) float64 {
	p := math.Pow10(places)
	return math.Round(x*p) / p
}

// requestFor builds the request of a task.
func requestFor(t task) providers.RichRequest {
	r := providers.RichRequest{
		Messages: []providers.RichMessage{textMessage(providers.MsgSystem, true, compareSystemPrompt)},
		MaxToks:  256,
	}
	if t.Excerpt != "" {
		// Retrieved text is data: it travels in an untrusted user-role part, never in the system instruction.
		r.Messages = append(r.Messages, textMessage(providers.MsgUser, false, t.Excerpt))
	}
	r.Messages = append(r.Messages, textMessage(providers.MsgUser, false, t.Prompt))
	return r
}

// sameJSON reports whether two JSON documents are the same value.
func sameJSON(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	ab, _ := json.Marshal(x)
	bb, _ := json.Marshal(y)
	return bytes.Equal(ab, bb)
}

// runCorpus replays a corpus through one adapter and the stub.
func runCorpus(ctx context.Context, s *subject, corpus string, tasks []task) CorpusRun {
	run := CorpusRun{Corpus: corpus, Cases: len(tasks), Label: LabelEmulated}
	var times []float64
	var bytesSum int
	for _, t := range tasks {
		if t.NotSent {
			run.NotSent++
			continue
		}
		in := providers.EstimateTokens(compareSystemPrompt + t.Excerpt + t.Prompt)
		out := providers.EstimateTokens(t.Reply)
		s.stub.script(scenario{Kind: kText, Text: t.Reply, ToksIn: in, ToksOut: out})
		started := time.Now()
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		res, err := s.gen.Generate(cctx, compareScope("ses_"+t.ID), requestFor(t))
		cancel()
		elapsed := time.Since(started)
		run.Sent++
		reqs := s.stub.requests()
		if len(reqs) > 0 {
			bytesSum += reqs[0].Bytes
			if t.Excerpt != "" {
				if strings.Contains(bodyStrings(reqs[0].Body), t.Excerpt) {
					run.ExcerptKept++
					run.ExcerptUntrusted++ // the excerpt is a user-role part by construction (requestFor)
				}
			}
		}
		if err != nil {
			run.Errors++
			continue
		}
		times = append(times, float64(elapsed.Microseconds())/1000)
		run.ToksIn += res.Usage.ToksIn
		run.ToksOut += res.Usage.ToksOut
		run.CostMicros += res.Usage.CostMicros
		if res.Usage.Currency != "" {
			run.Currency = res.Usage.Currency
		}
		got := res.Text()
		switch {
		case t.JSONReply && sameJSON(got, t.Reply):
			run.Decoded++
		case !t.JSONReply && got == t.Reply && containsAll(got, t.Facts):
			run.Decoded++
		}
	}
	if run.Sent > 0 {
		run.RequestBytesMean = roundTo(float64(bytesSum)/float64(run.Sent), 1)
	}
	run.OverheadP50MS = roundTo(pctl(times, 50), 3)
	run.OverheadP95MS = roundTo(pctl(times, 95), 3)
	return run
}

func containsAll(s string, parts []string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// bodyStrings returns every string value of a JSON request body, newline-joined, so a check
// does not depend on how an adapter escapes characters.
func bodyStrings(body string) string {
	var v any
	if json.Unmarshal([]byte(body), &v) != nil {
		return body
	}
	var sb strings.Builder
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			sb.WriteString(t)
			sb.WriteByte('\n')
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return sb.String()
}
