package rag

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	ragc "github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// extractive is the audit's stand-in for the model that proposes the claims of a grounded
// answer. The audit asks no model anything (master plan 4.5: the audit is deterministic and
// does not ask an LLM to certify another system), but the part of the answer pipeline that
// decides (the claim check by code, the final recheck of the sources, the citations and
// their references) only runs when something proposes claims. This stand-in proposes one,
// copied verbatim from the first passage of the evidence package it was handed.
//
// It is handed the package out of band (set), and never reads the prompt. It has no
// opinion about what a question expects: what it proposes depends on the passages alone,
// so a path that expects no evidence meets the same stand-in as one that expects an
// answer. It is not a model and is never offered as one.
type extractive struct {
	mu  sync.Mutex
	pkg ragc.EvidencePackage
}

var _ providers.TextGenerator = (*extractive)(nil)

// set hands the stand-in the package the next Chat call is about.
func (e *extractive) set(pkg ragc.EvidencePackage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pkg = pkg
}

// maxClaimRunes keeps the proposed claim well under the answer package's own cap.
const maxClaimRunes = 300

// Chat implements providers.TextGenerator: one claim, the first sentence of the first
// passage that is not a heading, citing that passage.
func (e *extractive) Chat(ctx context.Context, _ contracts.Scope, _ providers.ChatRequest) (providers.ChatResult, error) {
	if err := ctx.Err(); err != nil {
		return providers.ChatResult{}, err
	}
	e.mu.Lock()
	pkg := e.pkg
	e.mu.Unlock()

	type claim struct {
		Text        string   `json:"text"`
		EvidenceIDs []string `json:"evidence_ids"`
	}
	claims := []claim{}
	for _, it := range pkg.Items {
		if it.Availability == conversation.CitationAvailabilityRemoved {
			continue
		}
		if s, ok := firstSentence(it.Text); ok {
			claims = append(claims, claim{Text: s, EvidenceIDs: []string{string(it.EvidenceID)}})
			break
		}
	}
	raw, err := json.Marshal(map[string]any{"claims": claims})
	if err != nil {
		return providers.ChatResult{}, contracts.Wrap(contracts.ErrInternal, err, "rag audit: the stand-in could not encode its claims")
	}
	return providers.ChatResult{Text: string(raw), JSON: raw, FinishReason: providers.FinishStop}, nil
}

// ChatStream implements providers.TextGenerator. The answer step never streams.
func (e *extractive) ChatStream(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, contracts.New(contracts.ErrUnavailable, "rag audit: the stand-in does not stream")
}

// firstSentence returns the first sentence of the first line of text that is not blank and
// not a Markdown heading, cut to maxClaimRunes. A sentence ends at a full stop followed by
// white space or the end of the line.
func firstSentence(text string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		end := len(line)
		for i := 0; i < len(line); i++ {
			if line[i] == '.' && (i+1 == len(line) || line[i+1] == ' ') {
				end = i + 1
				break
			}
		}
		s := strings.TrimSpace(line[:end])
		if utf8.RuneCountInString(s) > maxClaimRunes {
			s = string([]rune(s)[:maxClaimRunes])
		}
		if s != "" {
			return s, true
		}
	}
	return "", false
}
