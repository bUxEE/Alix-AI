package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"sync"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// extractiveGenerator is the no-model answer generator (master plan rule 16:
// the deterministic method first). It does what the grounded-generation prompt
// asks of a model, without one: it reads the question and the evidence
// passages out of the prompt, picks the sentences that say the most about the
// question, and returns them as claims that cite the passage they came from.
// Everything it returns is verbatim from a passage, so the claim check
// (internal/rag/answer) passes it by construction; what is left to measure is
// whether retrieval brought the right passage and whether the sentence that
// shares the most words with the question is the one that answers it.
//
// It is label-blind: it sees what a model sees, the prompt, and nothing of the
// corpus manifest, the labels or the expected outcomes. If it read them, the
// citation precision and the unsupported-answer rate would be perfect by
// construction and tell nothing.
//
// It declines (an empty claims list) when no sentence matches enough of the
// question's content words. That floor is what stops it answering from a
// passage that merely sits near the question, and it is the only thing between
// a retrieval miss and a wrong answer in this mode; a real model has its own,
// weaker, version of it. A run with this generator therefore measures the
// retrieval pipeline and the verification around a model, never a model.
type extractiveGenerator struct {
	// minRatio is the weighted share of the question's terms (its content
	// words and its exact tokens: numbers, codes) a sentence must hold.
	minRatio float64

	mu       sync.Mutex
	calls    int
	frameSHA string
}

// newExtractiveGenerator returns the generator with the default floor.
func newExtractiveGenerator() *extractiveGenerator { return &extractiveGenerator{minRatio: 0.5} }

var (
	evidenceTag = regexp.MustCompile(`(?s)<evidence id="(e[0-9]+)">\n(.*?)\n</evidence>`)
	questionTag = regexp.MustCompile(`(?s)<question>\n(.*?)\n</question>`)
)

type wireClaim struct {
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

// maxExtractClaims bounds the claims one answer carries.
const maxExtractClaims = 2

// maxClaimRunes keeps a claim well under the claim limit of the answer package.
const maxClaimRunes = 400

type candidate struct {
	handle string
	text   string
	score  float64
	order  int
}

func (g *extractiveGenerator) Chat(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatResult, error) {
	if err := scope.Validate(); err != nil {
		return providers.ChatResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return providers.ChatResult{}, providers.ContextError(err)
	}
	var system, user string
	for _, m := range req.Messages {
		switch m.Role {
		case providers.MsgSystem:
			system += m.Content
		case providers.MsgUser:
			user = m.Content
		}
	}
	g.mu.Lock()
	g.calls++
	if g.frameSHA == "" && system != "" {
		sum := sha256.Sum256([]byte(system))
		g.frameSHA = hex.EncodeToString(sum[:])
	}
	g.mu.Unlock()

	claims := g.claims(user)
	body, err := json.Marshal(struct {
		Claims []wireClaim `json:"claims"`
	}{Claims: claims})
	if err != nil {
		return providers.ChatResult{}, contracts.Wrap(contracts.ErrInternal, err, "rageval: encoding the claims")
	}
	return providers.ChatResult{
		Text:         string(body),
		JSON:         body,
		FinishReason: providers.FinishStop,
		Usage:        providers.Usage{InputTokens: providers.EstimateTokens(system + user), OutputTokens: providers.EstimateTokens(string(body))},
		Model:        "rageval-extractive",
	}, nil
}

// ChatStream is not served: the answer package never streams.
func (g *extractiveGenerator) ChatStream(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, contracts.New(contracts.ErrUnavailable, "rageval: the extractive generator does not stream")
}

// Name and Health make it an AIRuntimeProvider.
func (g *extractiveGenerator) Name() string { return "rageval-extractive" }

func (g *extractiveGenerator) Health(context.Context) (providers.HealthReport, error) {
	return providers.HealthReport{Status: providers.HealthOK, Version: "extractive-1"}, nil
}

// FrameSHA256 is the digest of the system message of the first prompt it saw
// ("" before any): the identity of the grounded-generation prompt of the run.
func (g *extractiveGenerator) FrameSHA256() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.frameSHA
}

// Calls is the number of generations asked of it.
func (g *extractiveGenerator) Calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// claims picks the claims for the prompt's question from its passages.
func (g *extractiveGenerator) claims(user string) []wireClaim {
	qm := questionTag.FindStringSubmatch(user)
	if qm == nil {
		return []wireClaim{}
	}
	q := tokenize(qm[1])
	words := contentWords(q)
	if len(words)+len(q.exact) == 0 {
		return []wireClaim{}
	}

	var cands []candidate
	order := 0
	for _, m := range evidenceTag.FindAllStringSubmatch(user, -1) {
		for _, u := range splitUnits(m[2]) {
			order++
			if sc := g.score(q, words, u.context+" "+u.text); sc >= g.minRatio {
				cands = append(cands, candidate{handle: m[1], text: u.text, score: sc, order: order})
			}
		}
	}
	if len(cands) == 0 {
		return []wireClaim{}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].order < cands[j].order
	})
	best := cands[0].score
	out := []wireClaim{}
	seen := map[string]bool{}
	for _, c := range cands {
		if len(out) == maxExtractClaims || c.score < 0.8*best {
			break
		}
		key := foldText(c.text)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, wireClaim{Text: clipRunes(c.text, maxClaimRunes), EvidenceIDs: []string{c.handle}})
	}
	return out
}

// exactWeight is how much an exact token (a number, a code, a date) of the
// question counts next to a plain word: naming a code is the strongest thing a
// question does, and the row of a table that holds it often holds none of the
// question's other words (the header of the table is not in the passage).
const exactWeight = 3

// score is the weighted share of the question's terms the unit holds, with the
// header of its table when it is a row: its content words (matched by
// wordMatch, weight 1) and its exact tokens (matched whole, weight exactWeight),
// each counted once. It is 0 when an exact token of the question is missing: a
// question that names a code, a number or a date is answered by a sentence that
// has it or by none.
func (g *extractiveGenerator) score(q tokenSet, words []string, sentence string) float64 {
	st := tokenize(sentence)
	total := len(words) + exactWeight*len(q.exact)
	if total == 0 {
		return 0
	}
	hit := 0
	for x := range q.exact {
		if !st.exact[x] {
			return 0
		}
		hit += exactWeight
	}
	for _, w := range words {
		for sw := range st.words {
			if wordMatch(w, sw) {
				hit++
				break
			}
		}
	}
	return float64(hit) / float64(total)
}
