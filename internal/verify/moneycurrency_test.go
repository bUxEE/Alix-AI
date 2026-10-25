package verify

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// M3-40. The ledger's confirmation of a payment carries the application's own English summary
// ("Record a payment of 25.000 KWD on invoice INV-0001, ...") beside the typed money facts. The
// Italian template stated the summary and then failed its own verification with unknown_amount: the
// allowed set read the summary without knowing KWD (a code the lexicon does not list, carried only by
// the directive's money facts), so "25.000" was the bare number 25000 there, while the response check
// knew KWD and read the same characters as the AMOUNT 25000 KWD, which nothing allowed. English
// passed by accident (25.000 is 25 in English). These tests hold the reading of host prose to the
// reading of the response, in both languages and in a currency with no minor digits (JPY) and one
// with three (KWD), and hold the other half: an amount nothing carries is still refused.

type moneyCurrencyCase struct {
	name string
	lang Language
	cur  string
	// minor is the amount in minor units and exp the currency's minor digits.
	minor int64
	exp   int
}

// decimal is the canonical decimal text of the amount, as an application writes a money fact.
func (c moneyCurrencyCase) decimal(minor int64) string {
	if c.exp == 0 {
		return fmt.Sprintf("%d", minor)
	}
	unit := int64(1)
	for i := 0; i < c.exp; i++ {
		unit *= 10
	}
	return fmt.Sprintf("%d.%0*d", minor/unit, c.exp, minor%unit)
}

// stated is the amount as the response of the case's language writes it.
func (c moneyCurrencyCase) stated(minor int64, cur string) string {
	return localizeDecimal(c.decimal(minor), c.lang) + " " + cur
}

func moneyCurrencyCases() []moneyCurrencyCase {
	var out []moneyCurrencyCase
	for _, l := range []Language{LangEN, LangIT} {
		out = append(out,
			moneyCurrencyCase{name: string(l) + " JPY", lang: l, cur: "JPY", minor: 25000, exp: 0},
			moneyCurrencyCase{name: string(l) + " KWD", lang: l, cur: "KWD", minor: 25000, exp: 3},
		)
	}
	return out
}

// moneyConfirmation is the confirmation of a payment as the ledger builds it: the application's
// summary, the request's own fields and the typed money facts. textFirst puts the summary before
// the money facts (the ledger's order) or after them.
func moneyConfirmation(c moneyCurrencyCase, summary string, textFirst bool, must ...string) *conversation.DialogueDirective {
	text := []actions.Fact{textFact("summary", summary)}
	rest := []actions.Fact{
		intFact("amount_minor", c.minor), textFact("currency", c.cur),
		moneyFact("payment_amount", c.decimal(c.minor), c.cur),
		moneyFact("balance_after", c.decimal(c.minor*40+7), c.cur),
	}
	facts := append(slices.Clone(text), rest...)
	if !textFirst {
		facts = append(slices.Clone(rest), text...)
	}
	d := directive(conversation.DialogueDirectiveActRequestConfirmation, facts, must...)
	prep := actions.PreparationID(prepID)
	d.PreparationID = &prep
	d.Confirmation = actions.ConfirmationRequirement{Mode: actions.ConfirmationRequirementModeExplicit}
	d.PermittedNextActions = []actions.PermittedNextAction{
		{Action: actions.PermittedNextActionActionConfirm},
		{Action: actions.PermittedNextActionActionReject},
	}
	return d
}

// proseOf writes the application's summary of the case in the number convention given: the
// application is English-speaking whatever the language of the response (the ledger's is), or
// Italian-speaking whatever it is.
func proseOf(c moneyCurrencyCase, convention Language) string {
	grouped := func(minor int64) string {
		s := localizeDecimal(c.decimal(minor), convention)
		return s + " " + c.cur
	}
	return "Record a payment of " + grouped(c.minor) + " on invoice INV-0001, leaving " + grouped(c.minor*40+7) + " to pay."
}

// The template for the confirmation verifies whatever language the host prose is written in and
// wherever the prose stands among the facts, and states the prose and the typed amount.
func TestConfirmationTemplateVerifiesWithHostProseAmount(t *testing.T) {
	for _, c := range moneyCurrencyCases() {
		for _, convention := range []Language{LangEN, LangIT} {
			for _, textFirst := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s prose %s textFirst=%v", c.name, convention, textFirst), func(t *testing.T) {
					prose := proseOf(c, convention)
					d := moneyConfirmation(c, prose, textFirst)
					out, err := Degraded(d, nil, Options{Language: c.lang})
					if err != nil {
						t.Fatalf("the %s template of a %s payment with the prose %q: %v", c.lang, c.cur, prose, err)
					}
					if out.Source != SourceTemplate {
						t.Errorf("source = %q, want the template", out.Source)
					}
					for _, want := range []string{prose, c.stated(c.minor, c.cur)} {
						if !strings.Contains(out.Text, want) {
							t.Errorf("the template does not state %q:\n%s", want, out.Text)
						}
					}
					// the order of the facts is not a fact: the verdict on the same text is the same
					swapped := moneyConfirmation(c, prose, !textFirst)
					if res := mustVerify(t, out.Text, swapped, nil, Options{Language: c.lang}); !res.OK {
						t.Errorf("the template, checked against the same facts in the other order: %v", res.Violations)
					}
				})
			}
		}
	}
}

// What the model may say about the amount is what the facts and the host's prose carry, and nothing
// else: the right amount passes, and the amount made ten times larger, ten times smaller or one
// minor unit different is refused as an unknown amount, and the right digits in another currency as
// a currency mismatch. A verifier that let anything through (or one that refused the right amount)
// fails here.
func TestMoneyOfAConfirmationIsStillCheckedAgainstTheFacts(t *testing.T) {
	for _, c := range moneyCurrencyCases() {
		for _, convention := range []Language{LangEN, LangIT} {
			t.Run(fmt.Sprintf("%s prose %s", c.name, convention), func(t *testing.T) {
				opts := Options{Language: c.lang}
				d := moneyConfirmation(c, proseOf(c, convention), true, "payment_amount")
				say := func(amount string) string { return "Please confirm a payment of " + amount + " on invoice INV-0001." }

				if res := mustVerify(t, say(c.stated(c.minor, c.cur)), d, nil, opts); !res.OK {
					t.Errorf("the right amount was refused: %v", res.Violations)
				}
				for name, minor := range map[string]int64{
					"ten times larger":  c.minor * 10,
					"ten times smaller": c.minor / 10,
					"one minor unit up": c.minor + 1,
				} {
					res := mustVerify(t, say(c.stated(minor, c.cur)), d, nil, opts)
					if res.OK || !slices.Contains(kinds(res), string(ViolationUnknownAmount)) {
						t.Errorf("%s (%s): kinds %v, want unknown_amount", name, c.stated(minor, c.cur), kinds(res))
					}
				}
				other := "EUR"
				res := mustVerify(t, say(c.stated(c.minor, other)), d, nil, opts)
				if res.OK || !slices.Contains(kinds(res), string(ViolationCurrencyMismatch)) {
					t.Errorf("the right digits in %s: kinds %v, want currency_mismatch", other, kinds(res))
				}
				// the template is the answer to a refusal, so Realise swaps the invented text for it
				out, err := Realise(say(c.stated(c.minor*10, c.cur)), d, nil, opts)
				if err != nil || out.Source != SourceTemplate || out.Reason != ReasonVerificationFailed {
					t.Errorf("Realise of an invented amount = %+v, %v, want the template", out, err)
				}
			})
		}
	}
}

// The host's prose is what makes an amount stateable beyond the facts, and only the amounts it
// carries: with no prose, the amount read the other way round (the same characters in the other
// language's convention: "25.000" is 25 in English and 25000 in Italian) is refused in both
// languages, so that the fix that reads prose like the response does not let a thousandfold
// misreading in.
func TestMisreadMoneyIsRefusedWhenNothingCarriesIt(t *testing.T) {
	for _, c := range moneyCurrencyCases() {
		if c.exp == 0 {
			continue // no fraction digits: no separator means two things
		}
		t.Run(c.name, func(t *testing.T) {
			opts := Options{Language: c.lang}
			facts := []actions.Fact{moneyFact("payment_amount", c.decimal(c.minor), c.cur)}
			d := confirmationOf(facts...)
			if res := mustVerify(t, "Payment amount: "+c.stated(c.minor, c.cur), d, nil, opts); !res.OK {
				t.Fatalf("the amount as the language writes it was refused: %v", res.Violations)
			}
			// the same characters read in the other convention
			other := LangEN
			if c.lang == LangEN {
				other = LangIT
			}
			misread := localizeDecimal(c.decimal(c.minor), other) + " " + c.cur
			if misread == c.stated(c.minor, c.cur) {
				t.Fatal("the test needs the two conventions to spell the amount differently")
			}
			res := mustVerify(t, "Payment amount: "+misread, d, nil, opts)
			if res.OK || !slices.Contains(kinds(res), string(ViolationUnknownAmount)) {
				t.Errorf("%q in a %s response: kinds %v, want unknown_amount", misread, c.lang, kinds(res))
			}
		})
	}
}

// An amount in a currency the lexicon lists was already read, in host prose, as the response reads
// it; the code a directive carries only in its money facts (KWD) now is too. The two cannot differ:
// the Italian reading of English prose admits the same thing for EUR as for KWD.
func TestHostProseMoneyIsReadLikeTheResponseWhateverTheCurrency(t *testing.T) {
	for _, cur := range []string{"EUR", "KWD"} {
		t.Run(cur, func(t *testing.T) {
			opts := Options{Language: LangIT}
			d := confirmationOf(
				textFact("summary", "Record a payment of 25.000 "+cur+"."),
				moneyFact("payment_amount", "25.000", cur),
			)
			// "25.000" is twenty-five thousand in Italian, in the prose and in the response alike
			if res := mustVerify(t, "- Summary: Record a payment of 25.000 "+cur+".\n- Payment amount: 25,000 "+cur, d, nil, opts); !res.OK {
				t.Errorf("the host's prose, quoted verbatim: %v", res.Violations)
			}
			if res := mustVerify(t, "Pagamento di 26.000 "+cur, d, nil, opts); res.OK {
				t.Error("an amount the prose and the facts do not carry was accepted")
			}
		})
	}
}
