package compute

import (
	"math"
	"math/big"
	"sort"

	"github.com/buxee/alix-ai/internal/analyst/dataset"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

func runForecast(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset) (Result, error) {
	in := inputs[0]
	field, err := paramField(op.Params)
	if err != nil {
		return Result{}, err
	}
	f, err := requireField(in, field)
	if err != nil {
		return Result{}, err
	}
	orderBy := field
	if op.Params != nil && op.Params.OrderBy != nil {
		orderBy = *op.Params.OrderBy
	}
	horizon := int64(1)
	if op.Params != nil && op.Params.Horizon != nil {
		horizon = *op.Params.Horizon
	}
	conf := 0.8
	if op.Params != nil && op.Params.Confidence != nil {
		conf = *op.Params.Confidence
	}
	dec := defaultDecimals(op.Params)

	rows := sortRowsCopy(in.Rows, string(orderBy), analyst.ComputeOpParamsDirectionAsc)
	var ys []*big.Rat
	for _, row := range rows {
		r, null, ok := dataset.AsRat(dataset.Cell(row, string(field)))
		if !ok {
			return Result{}, refuse(contracts.ErrSchemaInvalid, "non-numeric series")
		}
		if null {
			continue
		}
		ys = append(ys, r)
	}

	var method analyst.DerivedFactForecastMetaMethod
	var forecasts []*big.Rat
	var se float64
	var ferr error
	switch op.Op {
	case analyst.ComputeOpOpForecastLinear:
		method = analyst.DerivedFactForecastMetaMethodLinearTrend
		if len(ys) < 3 {
			return Result{}, refuse(contracts.ErrInsufficientData, "linear forecast needs ≥3 points")
		}
		forecasts, se, ferr = forecastLinear(ys, int(horizon))
	case analyst.ComputeOpOpForecastHolt:
		method = analyst.DerivedFactForecastMetaMethodHolt
		if len(ys) < 4 {
			return Result{}, refuse(contracts.ErrInsufficientData, "holt forecast needs ≥4 points")
		}
		alpha, beta := 0.3, 0.1
		if op.Params != nil && op.Params.Alpha != nil {
			alpha = *op.Params.Alpha
		}
		if op.Params != nil && op.Params.Beta != nil {
			beta = *op.Params.Beta
		}
		forecasts, se, ferr = forecastHolt(ys, int(horizon), alpha, beta)
	case analyst.ComputeOpOpForecastSeasonalNaive:
		method = analyst.DerivedFactForecastMetaMethodSeasonalNaive
		if op.Params == nil || op.Params.SeasonLength == nil {
			return Result{}, refuse(contracts.ErrSchemaInvalid, "season_length required")
		}
		m := int(*op.Params.SeasonLength)
		if len(ys) < 2*m {
			return Result{}, refuse(contracts.ErrForecastUnavailable, "seasonal_naive needs 2 full seasons")
		}
		forecasts, se, ferr = forecastSeasonalNaive(ys, int(horizon), m)
	default:
		return Result{}, refuse(contracts.ErrSchemaInvalid, "not a forecast op")
	}
	if ferr != nil {
		return Result{}, ferr
	}
	if math.IsNaN(se) || math.IsInf(se, 0) {
		return Result{}, refuse(contracts.ErrForecastUnavailable, "forecast residual overflow")
	}

	z := normQuantile((1 + conf) / 2)
	outName := common.FieldName("estimate")
	lowName := common.FieldName("interval_low")
	highName := common.FieldName("interval_high")
	stepName := common.FieldName("horizon_step")
	methodName := common.FieldName("method")
	fields := []analyst.DatasetDatasetField{
		{Name: stepName, Type: analyst.FieldTypeNumber, DataClass: analyst.DataClassPublic},
		{Name: outName, Type: analyst.FieldTypeNumber, DataClass: f.DataClass, Unit: f.Unit, Currency: f.Currency},
		{Name: lowName, Type: analyst.FieldTypeNumber, DataClass: f.DataClass, Unit: f.Unit, Currency: f.Currency},
		{Name: highName, Type: analyst.FieldTypeNumber, DataClass: f.DataClass, Unit: f.Unit, Currency: f.Currency},
		{Name: methodName, Type: analyst.FieldTypeString, DataClass: analyst.DataClassPublic},
	}
	outRows := make([]analyst.DatasetRow, len(forecasts))
	var facts []analyst.DerivedFact
	for i, fr := range forecasts {
		est := roundRat(fr, dec, roundingMode())
		if math.IsNaN(est) || math.IsInf(est, 0) {
			return Result{}, refuse(contracts.ErrForecastUnavailable, "forecast estimate overflow")
		}
		half := z * se * math.Sqrt(float64(i+1))
		loRat, err := ratFromFloat64(est - half)
		if err != nil {
			return Result{}, err
		}
		hiRat, err := ratFromFloat64(est + half)
		if err != nil {
			return Result{}, err
		}
		lo := roundRat(loRat, dec, roundingMode())
		hi := roundRat(hiRat, dec, roundingMode())
		outRows[i] = analyst.DatasetRow{
			string(stepName):   float64(i + 1),
			string(outName):    est,
			string(lowName):    lo,
			string(highName):   hi,
			string(methodName): string(method),
		}
		c := conf
		fc := &analyst.DerivedFactForecastMeta{
			Method:       method,
			Horizon:      int64(i + 1),
			IntervalLow:  lo,
			IntervalHigh: hi,
			Confidence:   &c,
		}
		label := "forecast_h" + itoa(i+1)
		facts = append(facts, derivedFact(e, op, inputs, label, est, formatDisplay(est, dec), f.Unit, f.Currency, f.DataClass, fc, dec))
	}
	return Result{Dataset: outDataset(e, op, inputs, fields, outRows), Facts: facts}, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// forecastLinear: OLS y = a + b*x with x=0..n-1; forecast steps n..n+h-1.
// Method: ordinary least squares linear trend.
func forecastLinear(ys []*big.Rat, h int) ([]*big.Rat, float64, error) {
	n := len(ys)
	var sumX, sumY, sumXX, sumXY float64
	for i, y := range ys {
		yf, err := floatFromRat(y)
		if err != nil {
			return nil, 0, err
		}
		x := float64(i)
		sumX += x
		sumY += yf
		sumXX += x * x
		sumXY += x * yf
	}
	nf := float64(n)
	den := nf*sumXX - sumX*sumX
	var a, b float64
	if den == 0 {
		a = sumY / nf
		b = 0
	} else {
		b = (nf*sumXY - sumX*sumY) / den
		a = (sumY - b*sumX) / nf
	}
	if !finite(a) || !finite(b) {
		return nil, 0, refuse(contracts.ErrForecastUnavailable, "linear fit overflow")
	}
	var sse float64
	for i, y := range ys {
		yf, err := floatFromRat(y)
		if err != nil {
			return nil, 0, err
		}
		pred := a + b*float64(i)
		d := yf - pred
		sse += d * d
	}
	se := 0.0
	if n > 2 {
		se = math.Sqrt(sse / float64(n-2))
	}
	if !finite(se) {
		return nil, 0, refuse(contracts.ErrForecastUnavailable, "linear residual overflow")
	}
	out := make([]*big.Rat, h)
	for i := 0; i < h; i++ {
		v := a + b*float64(n+i)
		r, err := ratFromFloat64(v)
		if err != nil {
			return nil, 0, err
		}
		out[i] = r
	}
	return out, se, nil
}

// forecastHolt: Holt's linear trend method (additive). Defaults α=0.3, β=0.1.
// Level/trend init: l0=y0, b0=y1-y0. Requires len(ys) ≥ 2 (callers enforce ≥4).
func forecastHolt(ys []*big.Rat, h int, alpha, beta float64) ([]*big.Rat, float64, error) {
	n := len(ys)
	if n < 2 {
		return nil, 0, refuse(contracts.ErrInsufficientData, "holt needs ≥2 points")
	}
	y0, err := floatFromRat(ys[0])
	if err != nil {
		return nil, 0, err
	}
	y1, err := floatFromRat(ys[1])
	if err != nil {
		return nil, 0, err
	}
	l, b := y0, y1-y0
	var sse float64
	var fitted int
	for t := 0; t < n; t++ {
		yt, err := floatFromRat(ys[t])
		if err != nil {
			return nil, 0, err
		}
		if t > 0 {
			pred := l + b
			d := yt - pred
			sse += d * d
			fitted++
			lNew := alpha*yt + (1-alpha)*(l+b)
			b = beta*(lNew-l) + (1-beta)*b
			l = lNew
			if !finite(l) || !finite(b) {
				return nil, 0, refuse(contracts.ErrForecastUnavailable, "holt diverged")
			}
		}
	}
	se := 0.0
	if fitted > 1 {
		se = math.Sqrt(sse / float64(fitted))
	}
	if !finite(se) {
		return nil, 0, refuse(contracts.ErrForecastUnavailable, "holt residual overflow")
	}
	out := make([]*big.Rat, h)
	for i := 0; i < h; i++ {
		r, err := ratFromFloat64(l + b*float64(i+1))
		if err != nil {
			return nil, 0, err
		}
		out[i] = r
	}
	return out, se, nil
}

// forecastSeasonalNaive: ŷ_{n+h} = y_{n+h-m}.
func forecastSeasonalNaive(ys []*big.Rat, h, m int) ([]*big.Rat, float64, error) {
	n := len(ys)
	var sse float64
	var k int
	for t := m; t < n; t++ {
		a, err := floatFromRat(ys[t])
		if err != nil {
			return nil, 0, err
		}
		b, err := floatFromRat(ys[t-m])
		if err != nil {
			return nil, 0, err
		}
		d := a - b
		sse += d * d
		k++
	}
	se := 0.0
	if k > 0 {
		se = math.Sqrt(sse / float64(k))
	}
	if !finite(se) {
		return nil, 0, refuse(contracts.ErrForecastUnavailable, "seasonal residual overflow")
	}
	out := make([]*big.Rat, h)
	for i := 0; i < h; i++ {
		idx := n - m + (i % m)
		out[i] = new(big.Rat).Set(ys[idx])
	}
	return out, se, nil
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// normQuantile approximates the standard normal quantile for p in (0,1).
func normQuantile(p float64) float64 {
	// Beasley-Springer-Moro approximation for common confidence levels.
	switch {
	case math.Abs(p-0.9) < 1e-9:
		return 1.2815515655446004
	case math.Abs(p-0.95) < 1e-9:
		return 1.6448536269514722
	case math.Abs(p-0.975) < 1e-9:
		return 1.959963984540054
	case math.Abs(p-0.99) < 1e-9:
		return 2.3263478740408408
	}
	// rational approximation
	a := []float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02, 1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := []float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02, 6.680131188771972e+01, -1.328068155288572e+01}
	c := []float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00, -2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := []float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00, 3.754408661907416e+00}
	plow, phigh := 0.02425, 1-0.02425
	if p < plow {
		q := math.Sqrt(-2 * math.Log(p))
		return (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
	if p > phigh {
		q := math.Sqrt(-2 * math.Log(1-p))
		return -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
	q := p - 0.5
	r := q * q
	return (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q /
		(((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
}

func sortRowsCopy(rows []analyst.DatasetRow, field string, dir analyst.ComputeOpParamsDirection) []analyst.DatasetRow {
	out := make([]analyst.DatasetRow, len(rows))
	for i, r := range rows {
		out[i] = dataset.CloneRow(r)
	}
	sortRowsInPlace(out, field, dir)
	return out
}

func sortRowsInPlace(rows []analyst.DatasetRow, field string, dir analyst.ComputeOpParamsDirection) {
	sort.SliceStable(rows, func(i, j int) bool {
		cmp := compareCells(dataset.Cell(rows[i], field), dataset.Cell(rows[j], field))
		if dir == analyst.ComputeOpParamsDirectionDesc {
			return cmp > 0
		}
		return cmp < 0
	})
}
