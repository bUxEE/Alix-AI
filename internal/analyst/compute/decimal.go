package compute

import (
	"math"
	"math/big"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

// ratFromFloat64 converts f to a Rat. NaN/±Inf → typed refusal (never nil deref).
func ratFromFloat64(f float64) (*big.Rat, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, refuse(contracts.ErrForecastUnavailable, "non-finite numeric value")
	}
	r := new(big.Rat).SetFloat64(f)
	if r == nil {
		return nil, refuse(contracts.ErrForecastUnavailable, "non-finite numeric value")
	}
	return r, nil
}

// floatFromRat converts r to float64, refusing non-finite results.
func floatFromRat(r *big.Rat) (float64, error) {
	if r == nil {
		return 0, refuse(contracts.ErrForecastUnavailable, "nil rational")
	}
	f, _ := r.Float64()
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, refuse(contracts.ErrForecastUnavailable, "numeric overflow")
	}
	return f, nil
}

// roundRat rounds r to decimals places with mode and returns a float64 at that scale.
func roundRat(r *big.Rat, decimals int, mode analyst.RoundingMode) float64 {
	if r == nil {
		return 0
	}
	if decimals < 0 {
		decimals = 0
	}
	if decimals > 8 {
		decimals = 8
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	scaled := new(big.Rat).Mul(new(big.Rat).Set(r), new(big.Rat).SetInt(scale))
	neg := scaled.Sign() < 0
	abs := new(big.Rat).Abs(scaled)
	// Split into integer and fractional parts of abs.
	ip := new(big.Int).Quo(abs.Num(), abs.Denom())
	frac := new(big.Rat).Sub(abs, new(big.Rat).SetInt(ip))
	// Compare frac to 1/2.
	half := big.NewRat(1, 2)
	cmp := frac.Cmp(half)
	bump := false
	switch mode {
	case analyst.RoundingModeFloor:
		if neg && frac.Sign() != 0 {
			bump = true
		}
	case analyst.RoundingModeCeil:
		if !neg && frac.Sign() != 0 {
			bump = true
		}
	case analyst.RoundingModeHalfEven:
		if cmp > 0 {
			bump = true
		} else if cmp == 0 && ip.Bit(0) == 1 {
			bump = true
		}
	default: // half_up
		if cmp >= 0 {
			bump = true
		}
	}
	if bump {
		ip.Add(ip, big.NewInt(1))
	}
	if neg {
		ip.Neg(ip)
	}
	out := new(big.Rat).SetFrac(ip, scale)
	f, _ := out.Float64()
	return f
}

func defaultDecimals(p *analyst.ComputeOpParams) int {
	if p != nil && p.Decimals != nil {
		return int(*p.Decimals)
	}
	return 2
}

func roundingMode() analyst.RoundingMode { return analyst.RoundingModeHalfUp }

func sumRats(xs []*big.Rat) *big.Rat {
	s := new(big.Rat)
	for _, x := range xs {
		if x != nil {
			s.Add(s, x)
		}
	}
	return s
}

func avgRat(xs []*big.Rat) *big.Rat {
	if len(xs) == 0 {
		return nil
	}
	s := sumRats(xs)
	return new(big.Rat).Quo(s, new(big.Rat).SetInt64(int64(len(xs))))
}

func minRat(xs []*big.Rat) *big.Rat {
	if len(xs) == 0 {
		return nil
	}
	m := new(big.Rat).Set(xs[0])
	for _, x := range xs[1:] {
		if x.Cmp(m) < 0 {
			m.Set(x)
		}
	}
	return m
}

func maxRat(xs []*big.Rat) *big.Rat {
	if len(xs) == 0 {
		return nil
	}
	m := new(big.Rat).Set(xs[0])
	for _, x := range xs[1:] {
		if x.Cmp(m) > 0 {
			m.Set(x)
		}
	}
	return m
}

// percentileLinear is NumPy-style linear interpolation for percentile p in [0,100].
func percentileLinear(xs []*big.Rat, p float64) *big.Rat {
	n := len(xs)
	if n == 0 {
		return nil
	}
	sorted := make([]*big.Rat, n)
	for i, x := range xs {
		sorted[i] = new(big.Rat).Set(x)
	}
	for i := 1; i < n; i++ {
		j := i
		for j > 0 && sorted[j].Cmp(sorted[j-1]) < 0 {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
			j--
		}
	}
	if p <= 0 {
		return new(big.Rat).Set(sorted[0])
	}
	if p >= 100 {
		return new(big.Rat).Set(sorted[n-1])
	}
	if n == 1 {
		return new(big.Rat).Set(sorted[0])
	}
	rankF := p / 100 * float64(n-1)
	lo := int(rankF)
	if lo >= n-1 {
		return new(big.Rat).Set(sorted[n-1])
	}
	frac := rankF - float64(lo)
	fr, err := ratFromFloat64(frac)
	if err != nil {
		return new(big.Rat).Set(sorted[lo]) // frac in [0,1) — unreachable unless overflow
	}
	diff := new(big.Rat).Sub(sorted[lo+1], sorted[lo])
	diff.Mul(diff, fr)
	return new(big.Rat).Add(sorted[lo], diff)
}

func medianRat(xs []*big.Rat) *big.Rat { return percentileLinear(xs, 50) }
