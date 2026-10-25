package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/providers"
)

func generate(t *testing.T, g *extractiveGenerator, question string, passages ...string) []wireClaim {
	t.Helper()
	var sb strings.Builder
	for i, p := range passages {
		sb.WriteString("<evidence id=\"e" + string(rune('1'+i)) + "\">\n" + p + "\n</evidence>\n")
	}
	sb.WriteString("<question>\n" + question + "\n</question>")
	c := loadTestCorpus(t)
	tn, _ := c.Tenant("acme")
	res, err := g.Chat(context.Background(), scopeFor(c, tn, "usr_alice"), providers.ChatRequest{
		Role: providers.RoleController,
		Messages: []providers.Message{
			{Role: providers.MsgSystem, Content: "frame", Trusted: true},
			{Role: providers.MsgUser, Content: sb.String()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Claims []wireClaim `json:"claims"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || out.Claims == nil {
		t.Fatalf("the output %q is not a claims object: %v", res.Text, err)
	}
	return out.Claims
}

func TestExtractiveGenerator(t *testing.T) {
	refund := "A customer may request a refund within 14 days of delivery. The refund is paid to the original payment method within 5 business days after the returned item is received."
	table := "| Invoice | Customer | Amount | Status |\n| --- | --- | --- | --- |\n| INV-2026-00418 | Fabrikam Logistics | 8,910.00 EUR | open |"

	t.Run("claims are sentences of the passage that states them, citing it", func(t *testing.T) {
		g := newExtractiveGenerator()
		claims := generate(t, g, "Within how many days of delivery can a customer request a refund?", "Unrelated text about shipping to Italy.", refund)
		if len(claims) == 0 || claims[0].EvidenceIDs[0] != "e2" || !strings.Contains(claims[0].Text, "14 days") || !strings.Contains(refund, claims[0].Text) {
			t.Fatalf("claims = %+v", claims)
		}
		if g.Calls() != 1 || g.FrameSHA256() == "" {
			t.Errorf("calls %d, frame digest %q", g.Calls(), g.FrameSHA256())
		}
	})
	t.Run("a table row is a claim without its pipes", func(t *testing.T) {
		claims := generate(t, newExtractiveGenerator(), "What is the amount of invoice INV-2026-00418?", table)
		if len(claims) != 1 || claims[0].Text != "INV-2026-00418 | Fabrikam Logistics | 8,910.00 EUR | open" {
			t.Fatalf("claims = %+v", claims)
		}
	})
	t.Run("the header of a table is context for its rows and never a claim", func(t *testing.T) {
		warranty := "The table lists the warranty for each product line.\n\n" +
			"| Product line | SKU | Warranty | Extended option |\n" +
			"| --- | --- | --- | --- |\n" +
			"| Atlas router | AX-9000-B | 24 months | EW-36 |\n" +
			"| Atlas Mini | AX-9000-M | 12 months | EW-24 |"
		claims := generate(t, newExtractiveGenerator(), "Which extended warranty option exists for the Atlas Mini?", warranty)
		if len(claims) == 0 || claims[0].Text != "Atlas Mini | AX-9000-M | 12 months | EW-24" {
			t.Fatalf("claims = %+v", claims)
		}
		for _, c := range claims {
			if strings.HasPrefix(c.Text, "Product line") {
				t.Errorf("the header row is a claim: %+v", c)
			}
		}
		units := splitUnits(warranty)
		if len(units) != 3 || units[1].context != "Product line | SKU | Warranty | Extended option" || units[0].context != "" {
			t.Errorf("units = %+v", units)
		}
	})
	t.Run("a question that names a code is answered by a sentence that has it, or by none", func(t *testing.T) {
		if claims := generate(t, newExtractiveGenerator(), "What is the amount of invoice INV-2026-99999?", table); len(claims) != 0 {
			t.Fatalf("answered about a code the passages do not have: %+v", claims)
		}
	})
	t.Run("it declines when no sentence says enough about the question", func(t *testing.T) {
		for _, q := range []string{"What is the capital of Australia?", "Who is the chief executive officer of Acme?"} {
			if claims := generate(t, newExtractiveGenerator(), q, refund, table); len(claims) != 0 {
				t.Errorf("%q: answered %+v", q, claims)
			}
		}
	})
	t.Run("it never writes more claims than its cap, and never one twice", func(t *testing.T) {
		claims := generate(t, newExtractiveGenerator(), "refund within days of delivery", refund, refund, refund)
		if len(claims) > maxExtractClaims {
			t.Errorf("%d claims", len(claims))
		}
		seen := map[string]bool{}
		for _, c := range claims {
			if seen[c.Text] {
				t.Errorf("claim %q twice", c.Text)
			}
			seen[c.Text] = true
		}
	})
	t.Run("no question in the prompt, no claim", func(t *testing.T) {
		g := newExtractiveGenerator()
		if claims := g.claims("<evidence id=\"e1\">\nsomething\n</evidence>"); len(claims) != 0 {
			t.Errorf("claims = %+v", claims)
		}
	})
}

func TestWordMatch(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"warranty", "warranties", true},
		{"fattura", "fatture", true},
		{"work", "working", true},
		{"work", "worm", false},
		{"cat", "car", false}, // under four letters: equal or nothing
		{"refund", "refunds", true},
		{"invoice", "invalid", false},
	} {
		if got := wordMatch(tc.a, tc.b); got != tc.want {
			t.Errorf("wordMatch(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
