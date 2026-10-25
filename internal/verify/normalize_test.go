package verify

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// factCase is one row of the normalisation tables: a fact the response must state, a response in
// some language, and whether it verifies.
type factCase struct {
	name string
	lang Language
	fact actions.Fact
	text string
	ok   bool
	// violation, when set, must be among the violations of a rejected response.
	violation ViolationKind
}

func runFactCases(t *testing.T, cases []factCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := directive(conversation.DialogueDirectiveActPresentResult, []actions.Fact{tc.fact}, string(tc.fact.Field))
			res := mustVerify(t, tc.text, d, nil, Options{Language: tc.lang})
			if res.OK != tc.ok {
				t.Fatalf("%s %q: OK = %v, want %v (violations %v)", tc.lang, tc.text, res.OK, tc.ok, res.Violations)
			}
			if !tc.ok && tc.violation != "" && !res.Has(tc.violation) {
				t.Errorf("%s %q: violations %v lack %s", tc.lang, tc.text, kinds(res), tc.violation)
			}
		})
	}
}

// Dates, numbers, times and amounts are compared by value after normalisation, in the
// conventions of the response's language: the same value written in different languages and
// spellings is accepted, and a value that is only right in another convention is not. Every
// "wrong" row has a "right" row for the same value, so a verifier that rejects everything fails.
func TestDateAndNumberNormalizationMultilingual(t *testing.T) {
	oct14 := dateFact("d", "2026-10-14")
	may4 := dateFact("d", "2026-05-04")
	apr5 := dateFact("d", "2026-04-05")
	runFactCases(t, []factCase{
		// Dates, English.
		{"en day month year", LangEN, oct14, "It is on 14 October 2026.", true, ""},
		{"en month day year", LangEN, oct14, "It is on October 14, 2026.", true, ""},
		{"en abbreviation", LangEN, oct14, "It is on Oct 14 2026.", true, ""},
		{"en ordinal", LangEN, oct14, "It is on 14th October 2026.", true, ""},
		{"en the 14th of", LangEN, oct14, "It is on the 14th of October 2026.", true, ""},
		{"en iso", LangEN, oct14, "It is on 2026-10-14.", true, ""},
		{"en day first slashes, day over 12", LangEN, oct14, "It is on 14/10/2026.", true, ""},
		{"en month first slashes, day over 12", LangEN, oct14, "It is on 10/14/2026.", true, ""},
		{"en without a year", LangEN, oct14, "It is on 14 October.", true, ""},
		{"en wrong day", LangEN, oct14, "It is on 15 October 2026.", false, ViolationUnknownDate},
		{"en wrong month", LangEN, oct14, "It is on 14 November 2026.", false, ViolationUnknownDate},
		{"en wrong year", LangEN, oct14, "It is on 14 October 2027.", false, ViolationUnknownDate},
		{"en wrong iso", LangEN, oct14, "It is on 2026-10-15.", false, ViolationUnknownDate},
		{"en impossible reading", LangEN, oct14, "It is on 13/10/2026.", false, ViolationUnknownDate},
		{"en ambiguous slashes are rejected even when one reading is right", LangEN, may4, "It is on 04/05/2026.", false, ViolationUnknownDate},
		{"en ambiguous slashes, the other date", LangEN, apr5, "It is on 04/05/2026.", false, ViolationUnknownDate},
		{"en unambiguous text form of the same date", LangEN, may4, "It is on 4 May 2026.", true, ""},
		{"en equal day and month are not ambiguous", LangEN, dateFact("d", "2026-05-05"), "It is on 05/05/2026.", true, ""},
		// Dates, Italian.
		{"it day month year", LangIT, oct14, "È il 14 ottobre 2026.", true, ""},
		{"it abbreviation", LangIT, oct14, "È il 14 ott 2026.", true, ""},
		{"it day first slashes", LangIT, oct14, "È il 14/10/2026.", true, ""},
		{"it dots", LangIT, oct14, "È il 14.10.2026.", true, ""},
		{"it dashes", LangIT, oct14, "È il 14-10-2026.", true, ""},
		{"it ordinal degree", LangIT, oct14, "È il 14° ottobre 2026.", true, ""},
		{"it with a weekday", LangIT, oct14, "È mercoledì 14 ottobre 2026.", true, ""},
		{"it without a year", LangIT, oct14, "È il 14 ottobre.", true, ""},
		{"it slashes are day first: 4 May", LangIT, may4, "È il 04/05/2026.", true, ""},
		{"it slashes are day first: not 5 April", LangIT, apr5, "È il 04/05/2026.", false, ViolationUnknownDate},
		{"it slashes are day first: 5 April", LangIT, apr5, "È il 05/04/2026.", true, ""},
		{"it slashes are day first: not 4 May", LangIT, may4, "È il 05/04/2026.", false, ViolationUnknownDate},
		{"it month first slashes are not a date", LangIT, oct14, "È il 10/14/2026.", false, ""},
		{"it wrong day", LangIT, oct14, "È il 15 ottobre 2026.", false, ViolationUnknownDate},
		{"it english month names also read", LangIT, oct14, "È il 14 October 2026.", true, ""},
		// Numbers.
		{"en integer", LangEN, intFact("n", 1234), "There are 1234.", true, ""},
		{"en thousands comma", LangEN, intFact("n", 1234), "There are 1,234.", true, ""},
		{"en decimal point is not thousands", LangEN, intFact("n", 1234), "There are 1.234.", false, ViolationUnknownNumber},
		{"en spaced groups are two numbers", LangEN, intFact("n", 1234), "There are 1 234.", false, ""},
		{"en words", LangEN, intFact("n", 1234), "There are twelve hundred and thirty-four.", true, ""},
		{"en words, wrong", LangEN, intFact("n", 1234), "There are twelve hundred and thirty-five.", false, ViolationUnknownNumber},
		{"it integer", LangIT, intFact("n", 1234), "Ce ne sono 1234.", true, ""},
		{"it thousands dot", LangIT, intFact("n", 1234), "Ce ne sono 1.234.", true, ""},
		{"it decimal comma is not thousands", LangIT, intFact("n", 1234), "Ce ne sono 1,234.", false, ViolationUnknownNumber},
		{"it nbsp thousands", LangIT, intFact("n", 1234), "Ce ne sono 1\u00a0234.", true, ""},
		{"it words", LangIT, intFact("n", 1234), "Ce ne sono milleduecentotrentaquattro.", true, ""},
		{"it words, wrong", LangIT, intFact("n", 1234), "Ce ne sono milleduecentotrentacinque.", false, ViolationUnknownNumber},
		{"en decimal with grouping", LangEN, decFact("r", "1234.56"), "The rate is 1,234.56.", true, ""},
		{"en decimal plain", LangEN, decFact("r", "1234.56"), "The rate is 1234.56.", true, ""},
		{"en decimal in the other convention is still unambiguous", LangEN, decFact("r", "1234.56"), "The rate is 1.234,56.", true, ""},
		{"en decimal trailing zeros", LangEN, decFact("r", "12.50"), "The rate is 12.5.", true, ""},
		{"en decimal wrong", LangEN, decFact("r", "1234.56"), "The rate is 1,234.65.", false, ViolationUnknownNumber},
		{"it decimal", LangIT, decFact("r", "1234.56"), "Il tasso è 1.234,56.", true, ""},
		{"it decimal plain", LangIT, decFact("r", "1234.56"), "Il tasso è 1234,56.", true, ""},
		{"it decimal wrong", LangIT, decFact("r", "1234.56"), "Il tasso è 1.234,65.", false, ViolationUnknownNumber},
		{"en words twenty-four", LangEN, intFact("n", 24), "That is twenty-four hours.", true, ""},
		{"en words twenty four", LangEN, intFact("n", 24), "That is twenty four hours.", true, ""},
		{"it words ventiquattro", LangIT, intFact("n", 24), "Sono ventiquattro ore.", true, ""},
		{"it words with accent ventitré", LangIT, intFact("n", 23), "Sono ventitré ore.", true, ""},
		{"fullwidth digits", LangEN, intFact("n", 24), "That is \uff12\uff14 hours.", true, ""},
		{"arabic-indic digits", LangEN, intFact("n", 24), "That is \u0662\u0664 hours.", true, ""},
		{"zero-width inside the number is read through and not shown", LangEN, intFact("n", 24), "That is 2\u200b4 hours.", false, ViolationInvisibleCharacters},
		{"negative", LangEN, intFact("n", -5), "The balance is -5.", true, ""},
		{"negative vs positive", LangEN, intFact("n", -5), "The balance is 5.", false, ViolationMissingFact},
		// Times.
		{"24h", LangEN, timeFact("t", "10:30"), "At 10:30.", true, ""},
		{"am", LangEN, timeFact("t", "10:30"), "At 10:30 am.", true, ""},
		{"AM capitals", LangEN, timeFact("t", "10:30"), "At 10:30 AM.", true, ""},
		{"a.m.", LangEN, timeFact("t", "10:30"), "At 10:30 a.m.", true, ""},
		{"italian dot", LangIT, timeFact("t", "10:30"), "Alle 10.30.", true, ""},
		{"italian ore", LangIT, timeFact("t", "10:30"), "Alle ore 10:30.", true, ""},
		{"italian h", LangIT, timeFact("t", "10:30"), "Alle 10h30.", true, ""},
		{"pm is not am", LangEN, timeFact("t", "10:30"), "At 10:30 pm.", false, ViolationUnknownTime},
		{"24h evening", LangEN, timeFact("t", "10:30"), "At 22:30.", false, ViolationUnknownTime},
		{"a minute off", LangEN, timeFact("t", "10:30"), "At 10:31.", false, ViolationUnknownTime},
		{"pm", LangEN, timeFact("t", "22:30"), "At 10:30 pm.", true, ""},
		{"dot pm", LangEN, timeFact("t", "22:30"), "At 10.30 pm.", true, ""},
		{"24h evening exact", LangEN, timeFact("t", "22:30"), "At 22:30.", true, ""},
		{"evening stated as morning", LangEN, timeFact("t", "22:30"), "At 10:30.", false, ViolationUnknownTime},
		{"noon", LangEN, timeFact("t", "12:00"), "At noon.", true, ""},
		{"noon as 12 pm", LangEN, timeFact("t", "12:00"), "At 12 pm.", true, ""},
		{"noon as 12 am is midnight", LangEN, timeFact("t", "12:00"), "At 12 am.", false, ViolationUnknownTime},
		{"italian mezzogiorno", LangIT, timeFact("t", "12:00"), "A mezzogiorno.", true, ""},
		{"midnight", LangEN, timeFact("t", "00:00"), "At midnight.", true, ""},
		{"italian mezzanotte", LangIT, timeFact("t", "00:00"), "A mezzanotte.", true, ""},
		{"seconds", LangEN, timeFact("t", "10:30:15"), "At 10:30:15.", true, ""},
		{"seconds omitted", LangEN, timeFact("t", "10:30:15"), "At 10:30.", false, ViolationMissingFact},
		{"zero seconds are the same time", LangEN, timeFact("t", "10:30"), "At 10:30:00.", true, ""},
		// Money.
		{"en symbol", LangEN, moneyFact("m", "1250.50", "EUR"), "It costs €1,250.50.", true, ""},
		{"en code after", LangEN, moneyFact("m", "1250.50", "EUR"), "It costs 1250.50 EUR.", true, ""},
		{"en word", LangEN, moneyFact("m", "1250.50", "EUR"), "It costs 1,250.50 euros.", true, ""},
		{"it symbol after", LangIT, moneyFact("m", "1250.50", "EUR"), "Costa 1.250,50 €.", true, ""},
		{"it nbsp symbol", LangIT, moneyFact("m", "1250.50", "EUR"), "Costa 1.250,50\u00a0€.", true, ""},
		{"it symbol before", LangIT, moneyFact("m", "1250.50", "EUR"), "Costa € 1.250,50.", true, ""},
		{"it word", LangIT, moneyFact("m", "1250.50", "EUR"), "Costa 1.250,50 euro.", true, ""},
		{"wrong currency", LangEN, moneyFact("m", "1250.50", "EUR"), "It costs $1,250.50.", false, ViolationCurrencyMismatch},
		{"wrong currency, it", LangIT, moneyFact("m", "1250.50", "EUR"), "Costa 1.250,50 USD.", false, ViolationCurrencyMismatch},
		{"no currency at all", LangEN, moneyFact("m", "1250.50", "EUR"), "It costs 1,250.50.", false, ViolationMissingFact},
		{"money in another currency", LangEN, moneyFact("m", "1250.50", "CHF"), "It costs CHF 1,250.50.", true, ""},
		{"money in swiss franc words", LangEN, moneyFact("m", "20", "CHF"), "It costs 20 francs.", true, ""},
		// Instants.
		{"instant z", LangEN, datetimeFact("i", "2026-10-14T08:30:00Z", ""), "At 2026-10-14T08:30:00Z.", true, ""},
		{"instant with offset", LangEN, datetimeFact("i", "2026-10-14T08:30:00Z", ""), "At 2026-10-14T10:30:00+02:00.", true, ""},
		{"instant wrong", LangEN, datetimeFact("i", "2026-10-14T08:30:00Z", ""), "At 2026-10-14T08:30:01Z.", false, ViolationUnknownDate},
		{"instant without a zone cannot be read", LangEN, datetimeFact("i", "2026-10-14T08:30:00Z", ""), "At 2026-10-14T08:30:00.", false, ViolationUnknownDate},
	})
}

// A fact of each canonical kind: the right wording verifies, the same wording with one value
// changed fails, and omitting it fails. The kinds are text, integer, decimal, boolean, date,
// time, datetime, money and identifier.
func TestEveryCanonicalKindIsChecked(t *testing.T) {
	cases := []struct {
		kind    string
		fact    actions.Fact
		right   string
		changed string
		omitted string
		lang    Language
	}{
		{"text", textFact("doctor", "Dr. Rossi"), "Your doctor is Dr Rossi.", "Your doctor is Dr. Bianchi.", "Your doctor is assigned.", LangEN},
		{"integer", intFact("hours", 24), "Notice is 24 hours.", "Notice is 25 hours.", "Notice is required.", LangEN},
		{"decimal", decFact("rate", "2.5"), "The rate is 2.5.", "The rate is 2.6.", "The rate applies.", LangEN},
		{"boolean", boolFact("refundable", true), "Yes, refundable.", "No, refundable.", "Refundable.", LangEN},
		{"date", dateFact("day", "2026-10-14"), "On 14 October 2026.", "On 14 October 2025.", "On a Wednesday.", LangEN},
		{"time", timeFact("start", "10:30"), "At 10:30.", "At 10:45.", "At the usual time.", LangEN},
		{"datetime", datetimeFact("slot", "2026-10-14T08:30:00Z", "Europe/Rome"), "On 14 October 2026 at 10:30.", "On 14 October 2026 at 09:30.", "On 14 October 2026.", LangEN},
		{"money", moneyFact("fee", "50.00", "EUR"), "Fee: 50.00 EUR.", "Fee: 50.00 USD.", "Fee: 50.00.", LangEN},
		{"identifier", idFact("ref", "apt_20991"), "Reference apt_20991.", "Reference apt_20992.", "Reference assigned.", LangEN},
		{"text italian", textFact("medico", "Dr. Rossi"), "Il medico è Dr. Rossi.", "Il medico è Dr. Verdi.", "Il medico è assegnato.", LangIT},
		{"date italian", dateFact("giorno", "2026-10-14"), "Il 14 ottobre 2026.", "Il 14 ottobre 2025.", "In un mercoledì.", LangIT},
		{"datetime italian", datetimeFact("slot", "2026-10-14T08:30:00Z", "Europe/Rome"), "Il 14 ottobre 2026 alle 10:30.", "Il 14 ottobre 2026 alle 08:30.", "Il 14 ottobre 2026.", LangIT},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			d := directive(conversation.DialogueDirectiveActPresentResult, []actions.Fact{tc.fact}, string(tc.fact.Field))
			opts := Options{Language: tc.lang}
			if res := mustVerify(t, tc.right, d, nil, opts); !res.OK {
				t.Fatalf("right wording %q: %v", tc.right, res.Violations)
			}
			if res := mustVerify(t, tc.changed, d, nil, opts); res.OK {
				t.Errorf("changed wording %q verified", tc.changed)
			}
			if res := mustVerify(t, tc.omitted, d, nil, opts); res.OK || !res.Has(ViolationMissingFact) {
				t.Errorf("omitted %q: %v", tc.omitted, res.Violations)
			}
		})
	}
}

// Text a model cannot use to hide a number, a date or an identifier from the scan.
func TestNormaliseRemovesWhatHidesATokenFromTheScan(t *testing.T) {
	tests := map[string]string{
		"2\u200b4":           "24",
		"2\u200d4":           "24",
		"\u202e24":           "24",
		"\ufeff24":           "24",
		"2\u00ad4":           "24",
		"\uff12\uff14":       "24",
		"\u0662\u0664":       "24",
		"\u09e8\u09ea":       "24",
		"\u2077":             "7",
		"a\u2013b":           "a-b",
		"1\u201210":          "1-10",
		"it\u2019s":          "it's",
		"\u00bd":             "1/2",
		"x\r\ny":             "x\ny",
		"a\u2028b":           "a b",
		"a\u3000b":           "a b",
		"1\u202f250":         "1\u00a0250",
		"apt_2\u200b0\u200c": "apt_20",
	}
	for in, want := range tests {
		if got := normalise(in); got != want {
			t.Errorf("normalise(%q) = %q, want %q", in, got, want)
		}
	}
	if got := normalise("\u216b"); got != string(unknownNumber) {
		t.Errorf("a roman numeral normalised to %q", got)
	}
}

// Anything goes in; nothing panics, and a verdict is always consistent with its violations.
func FuzzVerify(f *testing.F) {
	for _, s := range []string{
		"", "Please confirm: Dr. Rossi on Wednesday 14 October 2026 at 10:30, fee 50.00 EUR.",
		"Done: apt_20991 [1] [src_01J0000000000000000000000A] (source: x)", "1.234,56 € 10.30 \u00a0 2\u200b4 \uff12 ventitré twenty-four",
		"Source:\n- a\n- b\n", "12/13/14 31/02 0/0 99:99 -", "http://", "[", "[[1]]", "\x00\xff\xfe", strings.Repeat("9", 5000),
	} {
		f.Add(s)
	}
	d := confirmDirective()
	f.Fuzz(func(t *testing.T, text string) {
		if !utf8.ValidString(text) {
			return
		}
		for _, opts := range []Options{en, it} {
			res, err := Verify(text, d, nil, opts)
			if err != nil {
				t.Fatalf("Verify error: %v", err)
			}
			if res.OK != (len(res.Violations) == 0) {
				t.Fatalf("OK = %v with violations %v", res.OK, res.Violations)
			}
			if res.OK && strings.TrimSpace(text) == "" {
				t.Fatal("empty text verified")
			}
		}
		if n := normalise(text); normalise(n) != n {
			t.Fatalf("normalise is not idempotent on %q", text)
		}
	})
}
