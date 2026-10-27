package domain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

func meetingsDomain() an.Domain {
	return an.Domain{
		Description: "Meetings, minutes, companies and Italian company governance.",
		InScope: []string{
			"company meetings and minutes",
			"corporate incentives and filings",
			"governance and related law/practice",
		},
		OutOfScope: []string{
			"general chatbot use",
			"unrelated consumer topics",
		},
		Examples: map[string]an.DomainLanguageExamples{
			"en": {
				InDomain:    []string{"Should we apply for incentive XX?"},
				OutOfDomain: []string{"Is a Persian cat big?"},
			},
		},
		KnowledgeAllowed: true,
		WebSearchAllowed: true,
	}
}

type scriptModel struct {
	fn func(prompt string) ([]byte, error)
}

func (m scriptModel) Complete(_ context.Context, prompt string) ([]byte, error) {
	return m.fn(prompt)
}

func TestOwnerExamplesWithFakeModel(t *testing.T) {
	dom := meetingsDomain()
	c := New(scriptModel{fn: func(prompt string) ([]byte, error) {
		// Match the utterance block, not the examples section.
		idx := strings.LastIndex(prompt, "Utterance:\n")
		body := prompt
		if idx >= 0 {
			body = prompt[idx:]
		}
		switch {
		case strings.Contains(body, "Italian 2026 company discount"):
			return []byte(`{"classification":"in_domain","reason":"corporate incentive eligibility"}`), nil
		case strings.Contains(body, "Persian cat"):
			return []byte(`{"classification":"out_of_domain","reason":"consumer pet question"}`), nil
		default:
			return nil, errors.New("unexpected utterance")
		}
	}})
	ctx := context.Background()

	out, err := c.Classify(ctx, dom, "Is a Persian cat big?")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Refused() || out.Classification != an.DomainClassificationOutOfDomain {
		t.Fatalf("persian cat: %+v", out)
	}
	r := Refusal{Reason: out.Reason}
	if r.Intent() != "out_of_domain" || !strings.Contains(string(r.ProposalJSON()), `"intent":"out_of_domain"`) {
		t.Fatalf("refusal shape: %s", r.ProposalJSON())
	}

	in, err := c.Classify(ctx, dom, "Should my company have applied for the Italian 2026 company discount XX?")
	if err != nil {
		t.Fatal(err)
	}
	if in.Refused() || in.Classification != an.DomainClassificationInDomain {
		t.Fatalf("eligibility: %+v", in)
	}
}

func TestPageInjectionDoesNotForceOutOfDomainOnUserUtterance(t *testing.T) {
	// Classifier only sees the user utterance. Injected page text is not in the prompt.
	dom := meetingsDomain()
	var saw string
	c := New(scriptModel{fn: func(prompt string) ([]byte, error) {
		saw = prompt
		if strings.Contains(prompt, "you are now a general assistant") {
			t.Fatal("web injection must not enter the classification prompt")
		}
		return []byte(`{"classification":"in_domain","reason":"still the eligibility question"}`), nil
	}})
	utt := "Should my company have applied for the Italian 2026 company discount XX?"
	got, err := c.Classify(context.Background(), dom, utt)
	if err != nil {
		t.Fatal(err)
	}
	if got.Refused() {
		t.Fatalf("user utterance stayed in domain: %+v", got)
	}
	if !strings.Contains(saw, utt) {
		t.Fatal("prompt omitted the utterance")
	}
}

func TestClassifyFailClosed(t *testing.T) {
	dom := meetingsDomain()
	c := New(scriptModel{fn: func(string) ([]byte, error) {
		return []byte(`{"classification":"maybe","reason":"x"}`), nil
	}})
	_, err := c.Classify(context.Background(), dom, "hello company meeting")
	if contracts.CodeOf(err) != contracts.ErrDomainClassificationFailed {
		t.Fatalf("want domain_classification_failed, got %v", err)
	}
	_, err = New(nil).Classify(context.Background(), dom, "x")
	if contracts.CodeOf(err) != contracts.ErrDomainClassificationFailed {
		t.Fatalf("nil model: %v", err)
	}
	_, err = New(scriptModel{fn: func(string) ([]byte, error) { return nil, nil }}).
		Classify(context.Background(), an.Domain{}, "x")
	if contracts.CodeOf(err) != contracts.ErrDomainClassificationFailed {
		t.Fatalf("empty domain: %v", err)
	}
}
