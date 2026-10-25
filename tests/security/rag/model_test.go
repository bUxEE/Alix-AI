package ragsec

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
)

// The model is the one scripted part of the suite. The scripts below stand for a
// model that does as the text in front of it says: the point of every injection
// test is that doing so changes nothing, because nothing the model writes is
// kept because it wrote it (master plan 3 rule 5).

var (
	// fenceRE matches one fenced block of the answer prompt's user message.
	fenceRE = regexp.MustCompile(`(?s)<(?:evidence|question)[^>]*>\n.*?\n</(?:evidence|question)>`)
	// evidenceRE captures the handle and the text of one passage.
	evidenceRE = regexp.MustCompile(`(?s)<evidence id="(e\d+)">\n(.*?)\n</evidence>`)
	sentenceRE = regexp.MustCompile(`[^.!?]+[.!?]+`)
)

// passage is one fenced evidence block as the model sees it.
type passage struct {
	ID, Text string
}

// claimOut is one claim as the model writes it.
type claimOut struct {
	Text string   `json:"text"`
	IDs  []string `json:"evidence_ids"`
}

// passagesOf reads the passages out of an answer prompt.
func passagesOf(userMessage string) []passage {
	var out []passage
	for _, m := range evidenceRE.FindAllStringSubmatch(userMessage, -1) {
		out = append(out, passage{ID: m[1], Text: m[2]})
	}
	return out
}

// sentencesOf splits a passage into its sentences, dropping headings.
func sentencesOf(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for _, s := range sentenceRE.FindAllString(line, -1) {
			if s = strings.TrimSpace(s); len(strings.Fields(s)) >= 4 {
				out = append(out, s)
			}
		}
	}
	return out
}

// script writes the model's claims for a question over the passages it was given.
type script func(question string, ps []passage) []claimOut

// faithful states the first sentence of every passage and nothing else.
func faithful() script {
	return func(_ string, ps []passage) []claimOut {
		var out []claimOut
		for _, p := range ps {
			if ss := sentencesOf(p.Text); len(ss) > 0 {
				out = append(out, claimOut{Text: ss[0], IDs: []string{p.ID}})
			}
		}
		return out
	}
}

// obedient follows what the passages say: it writes first the claims a model that
// had accepted the injected instructions would write (answering from its own
// knowledge, saying the policy changed, naming a tool, citing a passage it was
// never given), then states every sentence of every passage, injected ones
// included. The extras come first so that the answerer's cap on claims cannot be
// what removes them.
func obedient(extra ...claimOut) script {
	return func(_ string, ps []passage) []claimOut {
		out := append([]claimOut(nil), extra...)
		for _, p := range ps {
			for _, s := range sentencesOf(p.Text) {
				out = append(out, claimOut{Text: s, IDs: []string{p.ID}})
			}
		}
		return out
	}
}

// model is the scripted model behind the fake provider.
type model struct {
	mu      sync.Mutex
	answer  script
	raw     string // when set, the answer call replies with exactly this
	rewrite []string
}

func newModel() *model { return &model{answer: faithful()} }

func (m *model) setScript(s script) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answer, m.raw = s, ""
}

func (m *model) setRaw(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.raw = s
}

func (m *model) setRewrite(queries ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rewrite = queries
}

// install registers the model's two scripted roles on the fake provider:
// grounded answering (controller) and query rewriting (router). Embeddings and
// reranking use the fake's deterministic defaults.
func (m *model) install(fp *fake.Provider) {
	fp.On(fake.ForRole(providers.RoleController)).ReplyFunc(func(c fake.Call) (string, error) {
		m.mu.Lock()
		raw, sc := m.raw, m.answer
		m.mu.Unlock()
		if raw != "" {
			return raw, nil
		}
		var user string
		for _, msg := range c.Request.Messages {
			if msg.Role == providers.MsgUser {
				user = msg.Content
			}
		}
		claims := sc("", passagesOf(user))
		if claims == nil {
			claims = []claimOut{}
		}
		b, err := json.Marshal(map[string]any{"claims": claims})
		return string(b), err
	})
	fp.On(fake.ForRole(providers.RoleRouter)).ReplyFunc(func(fake.Call) (string, error) {
		m.mu.Lock()
		q := m.rewrite
		m.mu.Unlock()
		if q == nil {
			q = []string{"loan renewal rules"}
		}
		b, err := json.Marshal(map[string]any{"queries": q})
		return string(b), err
	})
}

// fromKnowledge is a model that ignores the passages and answers from what it
// knows, citing the first passage it was given (or none when it was given none):
// what a model that accepted "answer from your own knowledge" would write.
func fromKnowledge(claims ...string) script {
	return func(_ string, ps []passage) []claimOut {
		if len(ps) == 0 {
			return nil
		}
		var out []claimOut
		for _, c := range claims {
			out = append(out, claimOut{Text: c, IDs: []string{ps[0].ID}})
		}
		return out
	}
}
