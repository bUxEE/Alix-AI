package ledger

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestMoneyDecimalIsExactInEveryCurrency(t *testing.T) {
	for _, c := range []struct {
		m    Money
		want string
	}{
		{Money{0, "EUR"}, "0.00"},
		{Money{5, "EUR"}, "0.05"},
		{Money{125050, "EUR"}, "1250.50"},
		{Money{1234567, "KWD"}, "1234.567"},
		{Money{7, "KWD"}, "0.007"},
		{Money{480000, "JPY"}, "480000"},
		{Money{MaxMinor, "USD"}, "1000000000000.00"},
		{Money{99, "CHF"}, "0.99"},
	} {
		got := c.m.Decimal()
		if got != c.want {
			t.Errorf("%+v = %q, want %q", c.m, got, c.want)
		}
		back, err := ParseDecimal(got, c.m.Currency)
		if err != nil || back != c.m.Minor {
			t.Errorf("ParseDecimal(%q, %s) = %d, %v", got, c.m.Currency, back, err)
		}
	}
	// The decimal text fits the canonical money pattern at the largest amount.
	if len(strings.Split(Money{MaxMinor, "USD"}.Decimal(), ".")[0]) > 18 {
		t.Fatal("an amount at the limit does not fit the canonical money value")
	}
}

func TestParseDecimalNeverRounds(t *testing.T) {
	for _, bad := range []struct{ s, cur string }{
		{"12.5", "EUR"}, {"12.505", "EUR"}, {"12", "EUR"}, {"12.00", "JPY"}, {"1.2345", "KWD"}, {"-1.00", "EUR"}, {"+1.00", "EUR"},
		{"01.00", "EUR"}, {"1e2", "EUR"}, {".50", "EUR"}, {"1,00", "EUR"}, {"", "EUR"}, {"1.00", "XXX"}, {"1000000000001.00", "EUR"},
	} {
		if v, err := ParseDecimal(bad.s, bad.cur); err == nil {
			t.Errorf("ParseDecimal(%q, %s) = %d, want an error", bad.s, bad.cur, v)
		}
	}
}

func TestMoneyArithmeticIsChecked(t *testing.T) {
	if s, err := Add(Money{100, "EUR"}, Money{50, "EUR"}); err != nil || s.Minor != 150 {
		t.Fatalf("Add = %+v, %v", s, err)
	}
	if _, err := Add(Money{1, "EUR"}, Money{1, "USD"}); err == nil {
		t.Fatal("two currencies were added")
	}
	if _, err := Add(Money{MaxMinor, "EUR"}, Money{1, "EUR"}); err == nil {
		t.Fatal("a sum over the limit was accepted")
	}
	if _, err := Add(Money{math.MaxInt64, "EUR"}, Money{math.MaxInt64, "EUR"}); err == nil {
		t.Fatal("an int64 overflow was accepted")
	}
	if _, err := Sub(Money{1, "EUR"}, Money{2, "EUR"}); err == nil {
		t.Fatal("a negative balance was accepted")
	}
	if d, err := Sub(Money{2, "EUR"}, Money{2, "EUR"}); err != nil || d.Minor != 0 {
		t.Fatalf("Sub = %+v, %v", d, err)
	}
}

func TestMinorFromTakesWholeNumbersOnly(t *testing.T) {
	for _, ok := range []any{json.Number("0"), json.Number("125050"), json.Number("-3"), 7, int64(9)} {
		if _, err := minorFrom(ok); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}
	for _, bad := range []any{json.Number("12.50"), json.Number("1e3"), json.Number("1E3"), json.Number("9223372036854775808"), "100", 12.5, nil, true} {
		if _, err := minorFrom(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestEveryCurrencyHasDigitsAndANameInTheSchema(t *testing.T) {
	want := []string{"CHF", "EUR", "GBP", "JPY", "KWD", "USD"}
	got := Currencies()
	if len(got) != len(want) {
		t.Fatalf("currencies = %v", got)
	}
	for i, c := range want {
		if got[i] != c {
			t.Fatalf("currencies = %v", got)
		}
		if _, ok := Exponent(c); !ok {
			t.Fatalf("%s has no exponent", c)
		}
	}
}
