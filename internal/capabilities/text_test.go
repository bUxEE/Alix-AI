package capabilities

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestTokenize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		max  int
		want []string
	}{
		{"Dov'è il mio ordine?", 0, []string{"dov", "e", "il", "mio", "ordine"}},
		{"l’appuntamento", 0, []string{"l", "appuntamento"}}, // typographic apostrophe
		{"Ci sono novità sul mio ticket?", 0, []string{"ci", "sono", "novita", "sul", "mio", "ticket"}},
		{"CAFÉ Crème Brûlée", 0, []string{"cafe", "creme", "brulee"}},
		{"cafe\u0301 resume\u0301", 0, []string{"cafe", "resume"}}, // decomposed accents
		{"Straße", 0, []string{"strasse"}},
		{"SKU WH-1182, 48,50 euro", 0, []string{"sku", "wh", "1182", "48", "50", "euro"}},
		{"予約したい", 0, []string{"予", "約", "し", "た", "い"}}, // unspaced scripts: one token per character
		{"  \t\n ", 0, nil},
		{"one two three four", 2, []string{"one", "two"}},
		{"alpha", 1, []string{"alpha"}},
		{"", 0, nil},
	} {
		if got := tokenize(tc.in, tc.max); !cmp.Equal(got, tc.want) {
			t.Errorf("tokenize(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

func TestTokensMatch(t *testing.T) {
	match := [][2]string{
		{"book", "book"},
		{"book", "booking"},
		{"book", "bookings"},
		{"cancel", "cancelled"},
		{"cancel", "cancellation"},
		{"reservation", "reservations"},
		{"reserve", "reservation"},
		{"prenotare", "prenotazione"},
		{"prenota", "prenotare"},
		{"annulla", "annullare"},
		{"annullare", "annullamento"},
		{"ordine", "ordini"},
		{"camera", "camere"},
		{"ticket", "tickets"},
		{"open", "opening"},
		{"dermatologica", "dermatologia"},
		{"x", "x"},
	}
	for _, p := range match {
		if !tokensMatch(p[0], p[1]) || !tokensMatch(p[1], p[0]) {
			t.Errorf("%q and %q should match", p[0], p[1])
		}
	}
	differ := [][2]string{
		{"state", "status"},
		{"order", "ordine"},   // 3 shared runes: below the stem floor
		{"day", "days"},       // under four runes only match themselves
		{"the", "then"},       //
		{"table", "tablet"},   // would match under the stem rule; see below
		{"book", "bookshelf"}, // a prefix more than three runes shorter
		{"cat", "category"},
		{"apri", "aperto"},
		{"close", "closing"}, // a miss the author covers with a synonym
		{"stock", "stop"},
		{"paga", "pagina"},
	}
	// table/tablet is a known false friend: the rule is generous by design, and
	// the package author's synonyms are the correction. Pin it so a change is seen.
	for _, p := range differ {
		got := tokensMatch(p[0], p[1])
		want := p == [2]string{"table", "tablet"}
		if got != want {
			t.Errorf("tokensMatch(%q, %q) = %v, want %v", p[0], p[1], got, want)
		}
	}
}

func TestPhraseMatching(t *testing.T) {
	utt := tokenize("How many days off do I have left?", 0)
	for _, tc := range []struct {
		label string
		want  bool
	}{
		{"days off", true},
		{"day off", false}, // "day" is under four runes: it only matches itself
		{"days", true},
		{"off days", false},  // order matters
		{"days left", false}, // consecutive tokens
		{"left", true},
		{"", false},
		{"days off do I have left today", false}, // longer than what remains
	} {
		p, ok := newPhrase(tc.label, "en")
		if !ok {
			if tc.want {
				t.Errorf("%q: no phrase", tc.label)
			}
			continue
		}
		if got := p.matches(utt); got != tc.want {
			t.Errorf("phrase %q matches = %v, want %v", tc.label, got, tc.want)
		}
	}
	jp, _ := newPhrase("予約", "ja")
	if !jp.matches(tokenize("明日の予約をしたい", 0)) {
		t.Error("a Japanese label should match as a run of characters")
	}
	if jp.matches(tokenize("予測と約束", 0)) {
		t.Error("the characters must be consecutive")
	}
}

func TestKeywordTokens(t *testing.T) {
	got := keywordTokens("Find a title for the Member")
	if want := []string{"find", "title", "member"}; !cmp.Equal(got, want) {
		t.Errorf("keywordTokens = %q, want %q", got, want)
	}
	if got := keywordTokens("Prenota un tavolo per il cliente"); !cmp.Equal(got, []string{"prenota", "tavolo", "cliente"}) {
		t.Errorf("Italian stopwords: %q", got)
	}
	if got := keywordTokens("予約"); got != nil {
		t.Errorf("unspaced script gives keywords %q", got)
	}
}

func FuzzTokenizeAndMatch(f *testing.F) {
	for _, s := range []string{"Dov'è il mio ordine?", "予約", "a\u0301", "", "    ", "x-y_z", "\xff\xfe"} {
		f.Add(s, s)
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		ta, tb := tokenize(a, 64), tokenize(b, 64)
		if len(ta) > 64 || len(tb) > 64 {
			t.Fatal("tokenize exceeded its bound")
		}
		for _, x := range ta {
			if x == "" {
				t.Fatalf("empty token from %q", a)
			}
			for _, y := range tb {
				if tokensMatch(x, y) != tokensMatch(y, x) {
					t.Fatalf("tokensMatch(%q,%q) is not symmetric", x, y)
				}
			}
		}
		if p, ok := newPhrase(a, "en"); ok {
			p.matches(tb)
		}
	})
}

func TestTruncateRunes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"abcdef", 3, "abc"},
		{"abc", 3, "abc"},
		{"abc", 10, "abc"},
		{"àèìòù", 2, "àè"}, // two runes, four bytes
		{"予約したい", 2, "予約"},
		{"", 5, ""},
	} {
		if got := truncateRunes(tc.in, tc.n); got != tc.want {
			t.Errorf("truncateRunes(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestResolveLanguages(t *testing.T) {
	ix, err := compile(demoPackage(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tag  string
		want []string
	}{
		{"it", []string{"it"}},
		{"IT", []string{"it"}},
		{"it-CH", []string{"it"}},
		{"en_GB", []string{"en"}},
		{"", []string{"en", "it"}},
		{"fr", []string{"en", "it"}}, // unsupported: every language
		{"x", []string{"en", "it"}},
		{string(make([]byte, 500)), []string{"en", "it"}},
	} {
		if got := ix.resolveLanguages(tc.tag); !cmp.Equal(got, tc.want) {
			t.Errorf("resolveLanguages(%q) = %v, want %v", tc.tag, got, tc.want)
		}
	}
}
