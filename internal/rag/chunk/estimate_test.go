package chunk

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The estimate bounds the evidence package for the Lite context budget, so the
// property that matters is that it never undercounts. There is no tokenizer in
// the build to compare against, so these tests assert the properties that make
// an undercount impossible by construction: lower bounds every real tokenizer
// obeys, relative weights between scripts, monotonicity and sub-additivity.
func TestMultilingualTokenEstimate(t *testing.T) {
	samples := map[string]string{
		"english":    "The quick brown fox jumps over the lazy dog, then sleeps all afternoon.",
		"french":     "Les élèves ont étudié l'architecture médiévale à l'université de Strasbourg.",
		"german":     "Größenänderungen müssen vor dem Übersetzen geprüft werden.",
		"russian":    "Быстрая коричневая лиса перепрыгивает через ленивую собаку.",
		"greek":      "Η γρήγορη καφέ αλεπού πηδά πάνω από το τεμπέλικο σκυλί.",
		"arabic":     "الثعلب البني السريع يقفز فوق الكلب الكسول في الصباح.",
		"hebrew":     "השועל החום המהיר קופץ מעל הכלב העצל בבוקר.",
		"hindi":      "तेज़ भूरी लोमड़ी आलसी कुत्ते के ऊपर कूदती है।",
		"thai":       "สุนัขจิ้งจอกสีน้ำตาลกระโดดข้ามสุนัขขี้เกียจ",
		"chinese":    "敏捷的棕色狐狸跳过了那只懒惰的狗，然后整个下午都在睡觉。",
		"japanese":   "素早い茶色の狐がのろまな犬を飛び越えて、午後はずっと眠った。ひらがなとカタカナ。",
		"korean":     "빠른 갈색 여우가 게으른 개를 뛰어넘고 오후 내내 잤습니다.",
		"emoji":      "Great job 🎉🎉🙂👍🏽 see you 🚀",
		"digits":     "Invoice 2024-05-17 total 1,234,567.89 EUR ref 000123456",
		"code":       "if (a[i] >= 0x7f && !done) { return foo(b->c, \"x\"); }",
		"mixed":      "Hello 世界, привет мир! नमस्ते 🙂 123",
		"whitespace": "  \n\t  \n\n  ",
	}

	t.Run("empty is free, anything else costs at least a token", func(t *testing.T) {
		if EstimateTokens("") != 0 {
			t.Error("the empty text must cost nothing")
		}
		for name, s := range samples {
			if EstimateTokens(s) < 1 {
				t.Errorf("%s: a non-empty text estimated at %d", name, EstimateTokens(s))
			}
		}
		if EstimateTokens(" ") < 1 {
			t.Error("a single space must cost a token or more")
		}
	})

	t.Run("lower bounds every real tokenizer obeys", func(t *testing.T) {
		// A word costs at least one token; a digit and a punctuation mark are
		// at least a token's worth of a split; a Han, kana or Hangul character
		// is at least one token in the tokenizers the runtimes ship; an emoji
		// is several.
		words := func(s string) int { return len(strings.Fields(s)) }
		if got, min := EstimateTokens(samples["english"]), words(samples["english"]); got < min {
			t.Errorf("english: %d tokens for %d words", got, min)
		}
		if got := EstimateTokens("1234567890"); got < 10 {
			t.Errorf("10 digits estimated at %d: digits are split one by one by many tokenizers", got)
		}
		for _, name := range []string{"chinese", "japanese", "korean"} {
			s := samples[name]
			letters := utf8.RuneCountInString(strings.Join(strings.Fields(s), ""))
			if got := EstimateTokens(s); got < letters {
				t.Errorf("%s: %d tokens for %d characters", name, got, letters)
			}
		}
		if got := EstimateTokens(strings.Repeat("🙂", 10)); got < 20 {
			t.Errorf("10 emoji estimated at %d, want several tokens each", got)
		}
		// A tokenizer that falls back to bytes pays one token per byte at worst;
		// the estimate always covers a third of that, whatever the script.
		for name, s := range samples {
			if got, floor := EstimateTokens(s), (len(s)+2)/3; got < floor {
				t.Errorf("%s: %d tokens, below the byte floor %d", name, got, floor)
			}
		}
		if got := EstimateTokens("\xff\xfe\xfd"); got < 3 {
			t.Errorf("3 stray bytes estimated at %d", got)
		}
	})

	t.Run("scripts that tokenize worse weigh more per character", func(t *testing.T) {
		per := func(r rune) int { return EstimateTokens(strings.Repeat(string(r), 1000)) }
		latin := per('a')
		for name, r := range map[string]rune{
			"latin extended": 'é', "cyrillic": 'я', "greek": 'λ', "arabic": 'ع', "hebrew": 'ש',
			"devanagari": 'न', "thai": 'ก', "han": '字', "hiragana": 'あ', "hangul": '한',
		} {
			if got := per(r); got <= latin {
				t.Errorf("%s: %d tokens per 1000 characters, not above Latin's %d", name, got, latin)
			}
		}
		if per('я') < per('é') {
			t.Error("Cyrillic must weigh at least as much as accented Latin")
		}
		if per('न') < per('字') {
			t.Error("Indic scripts must weigh at least as much as Han")
		}
		if per('🙂') < 2*per('字') {
			t.Error("an emoji must weigh at least twice a Han character")
		}
		if per('\U00020000') < per('字') {
			t.Error("CJK extension characters must weigh at least as much as Han")
		}
	})

	t.Run("monotonic under appending and sub-additive under concatenation", func(t *testing.T) {
		for name, s := range samples {
			prev := 0
			for i := range s {
				if got := EstimateTokens(s[:i]); got < prev {
					t.Fatalf("%s: estimate fell from %d to %d at byte %d", name, prev, got, i)
				} else {
					prev = got
				}
			}
		}
		// Children never sum to less than their parent, so a budget built
		// from children is an upper bound for the parent text.
		names := make([]string, 0, len(samples))
		for n := range samples {
			names = append(names, n)
		}
		for _, a := range names {
			for _, b := range names {
				joined := EstimateTokens(samples[a] + " " + samples[b])
				if sum := EstimateTokens(samples[a]) + EstimateTokens(" ") + EstimateTokens(samples[b]); joined > sum {
					t.Errorf("%s + %s: %d > %d: concatenation must not cost more than its parts", a, b, joined, sum)
				}
			}
		}
	})

	t.Run("never more than a token per byte", func(t *testing.T) {
		// Errs high, but not absurdly: 1 token per byte is the ceiling of any
		// tokenizer with a byte fallback.
		for name, s := range samples {
			if got := EstimateTokens(s); got > len(s) {
				t.Errorf("%s: %d tokens for %d bytes", name, got, len(s))
			}
		}
	})

	t.Run("deterministic and in step with the incremental meter", func(t *testing.T) {
		for name, s := range samples {
			var m meter
			for _, r := range s {
				m.add(r, utf8.RuneLen(r))
			}
			if EstimateTokens(s) != m.toks() || EstimateTokens(s) != EstimateTokens(s) {
				t.Errorf("%s: estimator and meter disagree", name)
			}
		}
	})
}

func BenchmarkEstimateTokens(b *testing.B) {
	s := strings.Repeat("The quick brown fox, 狐狸 и собака 123. ", 25000) // about 1 MB
	b.SetBytes(int64(len(s)))
	for i := 0; i < b.N; i++ {
		_ = EstimateTokens(s)
	}
}

// The weights in estimate.go are the calibration: lowering one is a decision
// about the Lite context budget, so it must show up in a diff here.
func TestEstimateWeightsPinned(t *testing.T) {
	per := func(r rune) int { return EstimateTokens(strings.Repeat(string(r), 1000)) }
	for _, tc := range []struct {
		name string
		r    rune
		min  int // tokens per 1000 characters, at least
	}{
		{"latin letter", 'a', 340},
		{"digit", '7', 1000},
		{"punctuation", ',', 1000},
		{"han", '字', 1250},
		{"hiragana", 'あ', 1250},
		{"hangul", '한', 1250},
		{"devanagari", 'न', 1500},
		{"arabic", 'ع', 700},
		{"emoji", '🙂', 2500},
	} {
		if got := per(tc.r); got < tc.min {
			t.Errorf("%s: %d tokens per 1000 characters, calibration says at least %d", tc.name, got, tc.min)
		}
	}
}
