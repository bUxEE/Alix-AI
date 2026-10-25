package verify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// describe renders an atom compactly for table tests.
func describe(a atom) string {
	switch a.kind {
	case atomNumber:
		return "n:" + a.s
	case atomDate:
		return "d:" + formatDateAtom(a)
	case atomTime:
		return "t:" + formatTimeAtom(a)
	case atomMoney:
		return "m:" + a.s + a.cur
	case atomInstant:
		return "i:" + a.s
	case atomID:
		return "id:" + a.s
	case atomWeekday:
		return "wd:" + strconv.Itoa(a.n1)
	case atomWdDate:
		return "wdd:" + strconv.Itoa(a.n1) + "/" + formatDateAtom(dateAtom(a.n2, a.n3, a.n4))
	case atomCiteIdx:
		return "cite:" + strconv.Itoa(a.n1)
	case atomUnknown:
		return "?"
	}
	return "atom?"
}

// readings renders the readings of every claim in text: readings join with "|", atoms of one
// reading with "+", claims with " ".
func readings(text string, lang Language) string {
	var out []string
	for _, c := range extractClaims(normalise(text), lang, nil, nil, nil) {
		var rs []string
		for _, r := range c.readings {
			var as []string
			for _, a := range r {
				as = append(as, describe(a))
			}
			rs = append(rs, strings.Join(as, "+"))
		}
		switch {
		case c.kind == ckCompletion:
			out = append(out, "done")
		case c.kind == ckCitation && len(rs) > 0:
			out = append(out, "cite")
		default:
			out = append(out, strings.Join(rs, "|"))
		}
	}
	return strings.Join(out, " ")
}

func fact(field string, v actions.CanonicalValueValue) actions.Fact {
	return actions.Fact{Field: common.FieldName(field), Value: actions.CanonicalValue{Value: v}}
}

func withDisplay(f actions.Fact, d string) actions.Fact {
	f.Display = &d
	return f
}

func textFact(field, v string) actions.Fact { return fact(field, actions.TextValue{Value: v}) }
func intFact(field string, v int64) actions.Fact {
	return fact(field, actions.IntegerValue{Value: v})
}
func decFact(field, v string) actions.Fact { return fact(field, actions.DecimalValue{Value: v}) }
func boolFact(field string, v bool) actions.Fact {
	return fact(field, actions.BooleanValue{Value: v})
}
func dateFact(field, v string) actions.Fact { return fact(field, actions.DateValue{Value: v}) }
func timeFact(field, v string) actions.Fact { return fact(field, actions.TimeValue{Value: v}) }
func moneyFact(field, amount, cur string) actions.Fact {
	return fact(field, actions.MoneyValue{Value: amount, Currency: cur})
}
func idFact(field, v string) actions.Fact {
	return fact(field, actions.IdentifierValue{Value: common.Identifier(v)})
}
func datetimeFact(field, ts string, zone string) actions.Fact {
	v := actions.DatetimeValue{Value: common.Timestamp(ts)}
	if zone != "" {
		v.Zone = &zone
	}
	return fact(field, v)
}

const (
	turnID    = "trn_01J0000000000000000000000A"
	receiptID = "rcpt_01J0000000000000000000000A"
	prepID    = "prep_01J0000000000000000000000A"
	srcA      = "src_01J0000000000000000000000A"
	srcB      = "src_01J0000000000000000000000B"
)

// directive builds a directive with the given act and facts; mustState names the facts the
// response must state.
func directive(act conversation.DialogueDirectiveAct, facts []actions.Fact, mustState ...string) *conversation.DialogueDirective {
	d := &conversation.DialogueDirective{
		TurnID: actions.TurnID(turnID),
		Act:    act,
		Facts:  facts,
	}
	capID := common.CapabilityID("appointments.book")
	d.CapabilityID = &capID
	for _, f := range mustState {
		d.MustState = append(d.MustState, common.FieldName(f))
	}
	return d
}

func receipt(status actions.ActionReceiptStatus, facts ...actions.Fact) *actions.ActionReceipt {
	return &actions.ActionReceipt{
		ReceiptID:     actions.ReceiptID(receiptID),
		PreparationID: actions.PreparationID(prepID),
		CapabilityID:  "appointments.book",
		Status:        status,
		Facts:         facts,
		AuditRef:      "aud_01J0000000000000000000000A",
		IssuedAt:      "2026-10-09T08:03:00Z",
	}
}

func outcomeDirective(facts []actions.Fact, must ...string) *conversation.DialogueDirective {
	d := directive(conversation.DialogueDirectiveActReportOutcome, facts, must...)
	id := actions.ReceiptID(receiptID)
	d.ReceiptID = &id
	return d
}

func source(id, name string, claims ...string) conversation.Citation {
	return conversation.Citation{
		SourceID:     actions.SourceID(id),
		DisplayName:  name,
		Version:      "rev-12",
		Availability: conversation.CitationAvailabilityAvailable,
		Staleness:    conversation.CitationStalenessCurrent,
		OpenRef:      conversation.CitationOpenRef{Value: conversation.CitationOpenRefSource{Kind: "source"}},
		Claims:       claims,
	}
}

var (
	en = Options{Language: LangEN}
	it = Options{Language: LangIT}
)

// mustVerify verifies and fails the test on an error (not on a violation).
func mustVerify(t *testing.T, text string, d *conversation.DialogueDirective, r *actions.ActionReceipt, opts Options) Result {
	t.Helper()
	res, err := Verify(text, d, r, opts)
	if err != nil {
		t.Fatalf("Verify returned an error: %v", err)
	}
	return res
}

func kinds(r Result) []string {
	var ks []string
	for _, v := range r.Violations {
		ks = append(ks, string(v.Kind))
	}
	sort.Strings(ks)
	return ks
}

// exampleDirectives loads every valid DialogueDirective example of the schemas, decoding them
// with the generated fail-closed decoder.
func exampleDirectives(t *testing.T) map[string]*conversation.DialogueDirective {
	t.Helper()
	dir := filepath.Join("..", "..", "schemas", "conversation", "examples", "dialogue-directive", "valid")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no directive examples in %s (err=%v)", dir, err)
	}
	out := map[string]*conversation.DialogueDirective{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var d conversation.DialogueDirective
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out[strings.TrimSuffix(filepath.Base(f), ".json")] = &d
	}
	return out
}

func exampleReceipts(t *testing.T) map[string]*actions.ActionReceipt {
	t.Helper()
	dir := filepath.Join("..", "..", "schemas", "actions", "examples", "action-receipt", "valid")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no receipt examples in %s (err=%v)", dir, err)
	}
	out := map[string]*actions.ActionReceipt{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var r actions.ActionReceipt
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out[strings.TrimSuffix(filepath.Base(f), ".json")] = &r
	}
	return out
}
