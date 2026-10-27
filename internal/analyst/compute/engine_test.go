package compute

import (
	"bytes"
	"encoding/json"
	"math"
	"math/big"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }
func fn(s string) *common.FieldName {
	n := common.FieldName(s)
	return &n
}
func str(s string) *string { return &s }

func moneyDS(id string, amounts ...any) *analyst.Dataset {
	cur := "EUR"
	rows := make([]analyst.DatasetRow, len(amounts))
	for i, a := range amounts {
		rows[i] = analyst.DatasetRow{"amount": a}
	}
	return &analyst.Dataset{
		ID: analyst.DatasetID(id),
		Fields: []analyst.DatasetDatasetField{{
			Name: "amount", Type: analyst.FieldTypeNumber, DataClass: analyst.DataClassInternal, Currency: &cur,
		}},
		Rows: rows, RecordCount: int64(len(rows)),
		Provenance: analyst.DatasetDatasetProvenance{Kind: analyst.DatasetDatasetProvenanceKindRead},
	}
}

func numDS(id, field string, class analyst.DataClass, vals ...any) *analyst.Dataset {
	rows := make([]analyst.DatasetRow, len(vals))
	for i, v := range vals {
		rows[i] = analyst.DatasetRow{field: v}
	}
	return &analyst.Dataset{
		ID: analyst.DatasetID(id),
		Fields: []analyst.DatasetDatasetField{{
			Name: common.FieldName(field), Type: analyst.FieldTypeNumber, DataClass: class,
		}},
		Rows: rows, RecordCount: int64(len(rows)),
		Provenance: analyst.DatasetDatasetProvenance{Kind: analyst.DatasetDatasetProvenanceKindRead},
	}
}

func eng() *Engine {
	n, f := 0, 0
	return New(Options{
		MaxRows: 500, Forecasts: true,
		MintDatasetID: func() analyst.DatasetID {
			n++
			return analyst.DatasetID("ds_out_" + itoa(n))
		},
		MintFactID: func() string {
			f++
			return "df_out_" + itoa(f)
		},
	})
}

func TestAggregateSumMoneyExact(t *testing.T) {
	e := eng()
	ds := moneyDS("ds_in_001", 0.1, 0.2)
	agg := analyst.ComputeOpParamsAggSum
	res, err := e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpAggregate, Inputs: []analyst.DatasetID{"ds_in_001"},
		Params: &analyst.ComputeOpParams{Field: fn("amount"), Agg: &agg, Decimals: i64(2)},
	}, []*analyst.Dataset{ds})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Dataset.Rows[0]["sum_amount"].(float64)
	if got != 0.3 {
		t.Fatalf("0.1+0.2 = %v, want 0.3", got)
	}
	if res.Facts[0].Provenance.Op != "aggregate" {
		t.Fatal("provenance missing")
	}
}

func TestAggregateNullSkipAndEmpty(t *testing.T) {
	e := eng()
	ds := numDS("ds_in_002", "x", analyst.DataClassPublic, 1.0, nil, 3.0)
	agg := analyst.ComputeOpParamsAggSum
	skip := analyst.ComputeOpParamsNullsSkip
	res, err := e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpAggregate, Inputs: []analyst.DatasetID{"ds_in_002"},
		Params: &analyst.ComputeOpParams{Field: fn("x"), Agg: &agg, Nulls: &skip},
	}, []*analyst.Dataset{ds})
	if err != nil {
		t.Fatal(err)
	}
	if res.Dataset.Rows[0]["sum_x"].(float64) != 4 {
		t.Fatalf("got %v", res.Dataset.Rows[0]["sum_x"])
	}
	empty := numDS("ds_in_003", "x", analyst.DataClassPublic, nil, nil)
	_, err = e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpAggregate, Inputs: []analyst.DatasetID{"ds_in_003"},
		Params: &analyst.ComputeOpParams{Field: fn("x"), Agg: &agg, Nulls: &skip},
	}, []*analyst.Dataset{empty})
	if !contracts.HasCode(err, contracts.ErrInsufficientData) {
		t.Fatalf("want insufficient_data, got %v", err)
	}
}

func TestUnitCurrencyMismatch(t *testing.T) {
	e := eng()
	u1, u2 := "kg", "lb"
	ds := &analyst.Dataset{
		ID: "ds_in_004",
		Fields: []analyst.DatasetDatasetField{
			{Name: "a", Type: analyst.FieldTypeNumber, DataClass: analyst.DataClassPublic, Unit: &u1},
			{Name: "b", Type: analyst.FieldTypeNumber, DataClass: analyst.DataClassPublic, Unit: &u2},
		},
		Rows:        []analyst.DatasetRow{{"a": 1.0, "b": 2.0}},
		RecordCount: 1,
		Provenance:  analyst.DatasetDatasetProvenance{Kind: analyst.DatasetDatasetProvenanceKindRead},
	}
	_, err := e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpDifference, Inputs: []analyst.DatasetID{"ds_in_004"},
		Params: &analyst.ComputeOpParams{CurrentField: fn("a"), BaselineField: fn("b")},
	}, []*analyst.Dataset{ds})
	if !contracts.HasCode(err, contracts.ErrUnitMismatch) {
		t.Fatalf("want unit_mismatch, got %v", err)
	}
	c1, c2 := "EUR", "USD"
	ds2 := &analyst.Dataset{
		ID: "ds_in_005",
		Fields: []analyst.DatasetDatasetField{
			{Name: "a", Type: analyst.FieldTypeNumber, DataClass: analyst.DataClassPublic, Currency: &c1},
			{Name: "b", Type: analyst.FieldTypeNumber, DataClass: analyst.DataClassPublic, Currency: &c2},
		},
		Rows:        []analyst.DatasetRow{{"a": 1.0, "b": 2.0}},
		RecordCount: 1,
		Provenance:  analyst.DatasetDatasetProvenance{Kind: analyst.DatasetDatasetProvenanceKindRead},
	}
	_, err = e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpDifference, Inputs: []analyst.DatasetID{"ds_in_005"},
		Params: &analyst.ComputeOpParams{CurrentField: fn("a"), BaselineField: fn("b")},
	}, []*analyst.Dataset{ds2})
	if !contracts.HasCode(err, contracts.ErrUnitMismatch) {
		t.Fatalf("want unit_mismatch currency, got %v", err)
	}
}

func TestPercentileAndMedian(t *testing.T) {
	e := eng()
	ds := numDS("ds_in_006", "x", analyst.DataClassPublic, 1.0, 2.0, 3.0, 4.0)
	agg := analyst.ComputeOpParamsAggPercentile
	res, err := e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpAggregate, Inputs: []analyst.DatasetID{"ds_in_006"},
		Params: &analyst.ComputeOpParams{Field: fn("x"), Agg: &agg, Percentile: f64(50), Decimals: i64(2)},
	}, []*analyst.Dataset{ds})
	if err != nil {
		t.Fatal(err)
	}
	// linear: rank=1.5 → 2.5
	got := res.Dataset.Rows[0]["percentile_x"].(float64)
	if math.Abs(got-2.5) > 1e-9 {
		t.Fatalf("p50=%v want 2.5", got)
	}
}

func TestJoinDuplicates(t *testing.T) {
	e := eng()
	left := &analyst.Dataset{
		ID: "ds_l_001",
		Fields: []analyst.DatasetDatasetField{
			{Name: "id", Type: analyst.FieldTypeString, DataClass: analyst.DataClassPublic},
			{Name: "v", Type: analyst.FieldTypeNumber, DataClass: analyst.DataClassPublic},
		},
		Rows:        []analyst.DatasetRow{{"id": "a", "v": 1.0}, {"id": "a", "v": 2.0}},
		RecordCount: 2, Provenance: analyst.DatasetDatasetProvenance{Kind: analyst.DatasetDatasetProvenanceKindRead},
	}
	right := &analyst.Dataset{
		ID: "ds_r_001",
		Fields: []analyst.DatasetDatasetField{
			{Name: "id", Type: analyst.FieldTypeString, DataClass: analyst.DataClassPublic},
			{Name: "w", Type: analyst.FieldTypeNumber, DataClass: analyst.DataClassPublic},
		},
		Rows:        []analyst.DatasetRow{{"id": "a", "w": 10.0}, {"id": "a", "w": 20.0}},
		RecordCount: 2, Provenance: analyst.DatasetDatasetProvenance{Kind: analyst.DatasetDatasetProvenanceKindRead},
	}
	how := analyst.ComputeOpParamsHowInner
	res, err := e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpJoin, Inputs: []analyst.DatasetID{"ds_l_001", "ds_r_001"},
		Params: &analyst.ComputeOpParams{LeftOn: fn("id"), RightOn: fn("id"), How: &how},
	}, []*analyst.Dataset{left, right})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Dataset.Rows) != 4 {
		t.Fatalf("want 4 joined rows, got %d", len(res.Dataset.Rows))
	}
}

func TestDateBucketDSTAndMonthEnd(t *testing.T) {
	e := eng()
	ds := &analyst.Dataset{
		ID: "ds_d_001",
		Fields: []analyst.DatasetDatasetField{
			{Name: "ts", Type: analyst.FieldTypeDatetime, DataClass: analyst.DataClassPublic},
		},
		Rows: []analyst.DatasetRow{
			{"ts": "2026-03-29T01:30:00Z"}, // near EU DST
			{"ts": "2026-03-29T02:30:00Z"},
			{"ts": "2026-01-31T23:00:00Z"},
			{"ts": "2026-02-01T00:00:00Z"},
		},
		RecordCount: 4, Provenance: analyst.DatasetDatasetProvenance{Kind: analyst.DatasetDatasetProvenanceKindRead},
	}
	day := analyst.ComputeOpParamsBucketDay
	res, err := e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpDateBucket, Inputs: []analyst.DatasetID{"ds_d_001"},
		Params: &analyst.ComputeOpParams{OrderBy: fn("ts"), Bucket: &day},
	}, []*analyst.Dataset{ds})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Dataset.Rows) != 3 {
		t.Fatalf("want 3 day buckets (UTC), got %d %#v", len(res.Dataset.Rows), res.Dataset.Rows)
	}
	month := analyst.ComputeOpParamsBucketMonth
	res2, err := e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpDateBucket, Inputs: []analyst.DatasetID{"ds_d_001"},
		Params: &analyst.ComputeOpParams{OrderBy: fn("ts"), Bucket: &month},
	}, []*analyst.Dataset{ds})
	if err != nil {
		t.Fatal(err)
	}
	// Jan 31, Feb 1, Mar 29 → three UTC months
	if len(res2.Dataset.Rows) != 3 {
		t.Fatalf("want 3 months, got %d", len(res2.Dataset.Rows))
	}
}

func TestForecastKnownAnswers(t *testing.T) {
	// Series 2,4,6,8 → linear a=2,b=2; next = 10,12 (OLS x=0..3)
	// Independently: slope = (nΣxy-ΣxΣy)/(nΣx²-(Σx)²) with x=0..3,y=2,4,6,8 → b=2,a=2.
	e := eng()
	ds := numDS("ds_f_001", "y", analyst.DataClassPublic, 2.0, 4.0, 6.0, 8.0)
	res, err := e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpForecastLinear, Inputs: []analyst.DatasetID{"ds_f_001"},
		Params: &analyst.ComputeOpParams{Field: fn("y"), Horizon: i64(2), Decimals: i64(2)},
	}, []*analyst.Dataset{ds})
	if err != nil {
		t.Fatal(err)
	}
	if res.Dataset.Rows[0]["estimate"].(float64) != 10 || res.Dataset.Rows[1]["estimate"].(float64) != 12 {
		t.Fatalf("linear got %#v", res.Dataset.Rows)
	}
	if res.Facts[0].Forecast == nil || res.Facts[0].Forecast.Method != analyst.DerivedFactForecastMetaMethodLinearTrend {
		t.Fatal("forecast meta missing")
	}

	// Seasonal naive m=2 on [1,2,3,4] → next two: 3,4
	ds2 := numDS("ds_f_002", "y", analyst.DataClassPublic, 1.0, 2.0, 3.0, 4.0)
	res2, err := e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpForecastSeasonalNaive, Inputs: []analyst.DatasetID{"ds_f_002"},
		Params: &analyst.ComputeOpParams{Field: fn("y"), Horizon: i64(2), SeasonLength: i64(2), Decimals: i64(2)},
	}, []*analyst.Dataset{ds2})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Dataset.Rows[0]["estimate"].(float64) != 3 || res2.Dataset.Rows[1]["estimate"].(float64) != 4 {
		t.Fatalf("seasonal got %#v", res2.Dataset.Rows)
	}

	// Holt insufficient
	ds3 := numDS("ds_f_003", "y", analyst.DataClassPublic, 1.0, 2.0, 3.0)
	_, err = e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpForecastHolt, Inputs: []analyst.DatasetID{"ds_f_003"},
		Params: &analyst.ComputeOpParams{Field: fn("y"), Horizon: i64(1)},
	}, []*analyst.Dataset{ds3})
	if !contracts.HasCode(err, contracts.ErrInsufficientData) {
		t.Fatalf("want insufficient_data, got %v", err)
	}

	// Known Holt vector: y=[10,12,14,16], α=0.3, β=0.1 → hand-stepped level/trend → h1
	ds4 := numDS("ds_f_004", "y", analyst.DataClassPublic, 10.0, 12.0, 14.0, 16.0)
	res4, err := e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpForecastHolt, Inputs: []analyst.DatasetID{"ds_f_004"},
		Params: &analyst.ComputeOpParams{Field: fn("y"), Horizon: i64(1), Alpha: f64(0.3), Beta: f64(0.1), Decimals: i64(4)},
	}, []*analyst.Dataset{ds4})
	if err != nil {
		t.Fatal(err)
	}
	want := holtH1([]float64{10, 12, 14, 16}, 0.3, 0.1)
	got := res4.Dataset.Rows[0]["estimate"].(float64)
	if math.Abs(got-want) > 1e-3 {
		t.Fatalf("holt got %v want %v", got, want)
	}
}

func holtH1(ys []float64, alpha, beta float64) float64 {
	l, b := ys[0], ys[1]-ys[0]
	for t := 1; t < len(ys); t++ {
		lNew := alpha*ys[t] + (1-alpha)*(l+b)
		b = beta*(lNew-l) + (1-beta)*b
		l = lNew
	}
	return l + b
}

func TestFilterEmptyRefuse(t *testing.T) {
	e := eng()
	ds := numDS("ds_in_010", "x", analyst.DataClassPublic, 1.0, 2.0)
	_, err := e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpFilter, Inputs: []analyst.DatasetID{"ds_in_010"},
		Params: &analyst.ComputeOpParams{Predicate: &analyst.ComputeOpPredicate{Field: "x", Op: analyst.ComputeOpPredicateOpGt, Value: 9.0}},
	}, []*analyst.Dataset{ds})
	if !contracts.HasCode(err, contracts.ErrInsufficientData) {
		t.Fatalf("got %v", err)
	}
}

func TestDataClassPropagation(t *testing.T) {
	e := eng()
	ds := numDS("ds_in_011", "x", analyst.DataClassRestricted, 1.0, 2.0)
	agg := analyst.ComputeOpParamsAggSum
	res, err := e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpAggregate, Inputs: []analyst.DatasetID{"ds_in_011"},
		Params: &analyst.ComputeOpParams{Field: fn("x"), Agg: &agg},
	}, []*analyst.Dataset{ds})
	if err != nil {
		t.Fatal(err)
	}
	if res.Dataset.Fields[0].DataClass != analyst.DataClassRestricted {
		t.Fatal(res.Dataset.Fields[0].DataClass)
	}
	if res.Facts[0].DataClass != analyst.DataClassRestricted {
		t.Fatal(res.Facts[0].DataClass)
	}
}

func TestDeterminism(t *testing.T) {
	mk := func() *Engine {
		n, f := 0, 0
		return New(Options{Forecasts: true,
			MintDatasetID: func() analyst.DatasetID { n++; return analyst.DatasetID("ds_out_" + itoa(n)) },
			MintFactID:    func() string { f++; return "df_out_" + itoa(f) },
		})
	}
	ds := moneyDS("ds_in_012", 0.1, 0.2, 0.3)
	agg := analyst.ComputeOpParamsAggSum
	op := analyst.ComputeOp{
		Op: analyst.ComputeOpOpAggregate, Inputs: []analyst.DatasetID{"ds_in_012"},
		Params: &analyst.ComputeOpParams{Field: fn("amount"), Agg: &agg, Decimals: i64(2)},
	}
	a, err := mk().Run(op, []*analyst.Dataset{ds})
	if err != nil {
		t.Fatal(err)
	}
	b, err := mk().Run(op, []*analyst.Dataset{ds})
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if !bytes.Equal(ja, jb) {
		t.Fatalf("not deterministic:\n%s\n%s", ja, jb)
	}
}

func TestOpsSmoke(t *testing.T) {
	e := eng()
	ds := &analyst.Dataset{
		ID: "ds_in_020",
		Fields: []analyst.DatasetDatasetField{
			{Name: "name", Type: analyst.FieldTypeString, DataClass: analyst.DataClassInternal},
			{Name: "amt", Type: analyst.FieldTypeNumber, DataClass: analyst.DataClassInternal, Currency: str("EUR")},
			{Name: "base", Type: analyst.FieldTypeNumber, DataClass: analyst.DataClassInternal, Currency: str("EUR")},
			{Name: "ts", Type: analyst.FieldTypeDate, DataClass: analyst.DataClassPublic},
		},
		Rows: []analyst.DatasetRow{
			{"name": "a", "amt": 10.0, "base": 5.0, "ts": "2026-01-01"},
			{"name": "b", "amt": 20.0, "base": 10.0, "ts": "2026-01-02"},
			{"name": "a", "amt": 30.0, "base": 15.0, "ts": "2026-01-03"},
		},
		RecordCount: 3, Provenance: analyst.DatasetDatasetProvenance{Kind: analyst.DatasetDatasetProvenanceKindRead},
	}
	dir := analyst.ComputeOpParamsDirectionAsc
	cases := []analyst.ComputeOp{
		{Op: analyst.ComputeOpOpSort, Inputs: []analyst.DatasetID{"ds_in_020"}, Params: &analyst.ComputeOpParams{Field: fn("amt"), Direction: &dir}},
		{Op: analyst.ComputeOpOpFilter, Inputs: []analyst.DatasetID{"ds_in_020"}, Params: &analyst.ComputeOpParams{Predicate: &analyst.ComputeOpPredicate{Field: "amt", Op: analyst.ComputeOpPredicateOpGte, Value: 10.0}}},
		{Op: analyst.ComputeOpOpGroupBy, Inputs: []analyst.DatasetID{"ds_in_020"}, Params: &analyst.ComputeOpParams{
			Fields:   []common.FieldName{"name"},
			Measures: []analyst.ComputeOpMeasure{{Field: "amt", Agg: analyst.ComputeOpMeasureAggSum, As: "total"}},
		}},
		{Op: analyst.ComputeOpOpRatio, Inputs: []analyst.DatasetID{"ds_in_020"}, Params: &analyst.ComputeOpParams{Numerator: fn("amt"), Denominator: fn("base")}},
		{Op: analyst.ComputeOpOpPercentChange, Inputs: []analyst.DatasetID{"ds_in_020"}, Params: &analyst.ComputeOpParams{CurrentField: fn("amt"), BaselineField: fn("base")}},
		{Op: analyst.ComputeOpOpDifference, Inputs: []analyst.DatasetID{"ds_in_020"}, Params: &analyst.ComputeOpParams{CurrentField: fn("amt"), BaselineField: fn("base")}},
		{Op: analyst.ComputeOpOpRank, Inputs: []analyst.DatasetID{"ds_in_020"}, Params: &analyst.ComputeOpParams{Field: fn("amt"), TopN: i64(2)}},
		{Op: analyst.ComputeOpOpMovingAverage, Inputs: []analyst.DatasetID{"ds_in_020"}, Params: &analyst.ComputeOpParams{Field: fn("amt"), OrderBy: fn("ts"), Window: i64(2)}},
		{Op: analyst.ComputeOpOpCumulative, Inputs: []analyst.DatasetID{"ds_in_020"}, Params: &analyst.ComputeOpParams{Field: fn("amt"), OrderBy: fn("ts")}},
	}
	for _, op := range cases {
		if _, err := e.Run(op, []*analyst.Dataset{ds}); err != nil {
			t.Fatalf("%s: %v", op.Op, err)
		}
	}
}

func TestParamBounds(t *testing.T) {
	e := eng()
	ds := numDS("ds_in_030", "y", analyst.DataClassPublic, 1.0, 2.0, 3.0, 4.0)
	cases := []struct {
		name string
		op   analyst.ComputeOp
	}{
		{"alpha0", analyst.ComputeOp{Op: analyst.ComputeOpOpForecastHolt, Inputs: []analyst.DatasetID{"ds_in_030"},
			Params: &analyst.ComputeOpParams{Field: fn("y"), Horizon: i64(1), Alpha: f64(0)}}},
		{"alphaNeg", analyst.ComputeOp{Op: analyst.ComputeOpOpForecastHolt, Inputs: []analyst.DatasetID{"ds_in_030"},
			Params: &analyst.ComputeOpParams{Field: fn("y"), Horizon: i64(1), Alpha: f64(-0.1)}}},
		{"alphaGT1", analyst.ComputeOp{Op: analyst.ComputeOpOpForecastHolt, Inputs: []analyst.DatasetID{"ds_in_030"},
			Params: &analyst.ComputeOpParams{Field: fn("y"), Horizon: i64(1), Alpha: f64(1.1)}}},
		{"beta0", analyst.ComputeOp{Op: analyst.ComputeOpOpForecastHolt, Inputs: []analyst.DatasetID{"ds_in_030"},
			Params: &analyst.ComputeOpParams{Field: fn("y"), Horizon: i64(1), Beta: f64(0)}}},
		{"confLow", analyst.ComputeOp{Op: analyst.ComputeOpOpForecastLinear, Inputs: []analyst.DatasetID{"ds_in_030"},
			Params: &analyst.ComputeOpParams{Field: fn("y"), Horizon: i64(1), Confidence: f64(0.4)}}},
		{"confHigh", analyst.ComputeOp{Op: analyst.ComputeOpOpForecastLinear, Inputs: []analyst.DatasetID{"ds_in_030"},
			Params: &analyst.ComputeOpParams{Field: fn("y"), Horizon: i64(1), Confidence: f64(1.0)}}},
		{"horizon0", analyst.ComputeOp{Op: analyst.ComputeOpOpForecastLinear, Inputs: []analyst.DatasetID{"ds_in_030"},
			Params: &analyst.ComputeOpParams{Field: fn("y"), Horizon: i64(0)}}},
		{"horizon37", analyst.ComputeOp{Op: analyst.ComputeOpOpForecastLinear, Inputs: []analyst.DatasetID{"ds_in_030"},
			Params: &analyst.ComputeOpParams{Field: fn("y"), Horizon: i64(37)}}},
		{"window1", analyst.ComputeOp{Op: analyst.ComputeOpOpMovingAverage, Inputs: []analyst.DatasetID{"ds_in_030"},
			Params: &analyst.ComputeOpParams{Field: fn("y"), Window: i64(1)}}},
		{"season1", analyst.ComputeOp{Op: analyst.ComputeOpOpForecastSeasonalNaive, Inputs: []analyst.DatasetID{"ds_in_030"},
			Params: &analyst.ComputeOpParams{Field: fn("y"), Horizon: i64(1), SeasonLength: i64(1)}}},
		{"pctNeg", analyst.ComputeOp{Op: analyst.ComputeOpOpAggregate, Inputs: []analyst.DatasetID{"ds_in_030"},
			Params: &analyst.ComputeOpParams{Field: fn("y"), Agg: func() *analyst.ComputeOpParamsAgg {
				a := analyst.ComputeOpParamsAggPercentile
				return &a
			}(), Percentile: f64(-1)}}},
		{"pct101", analyst.ComputeOp{Op: analyst.ComputeOpOpAggregate, Inputs: []analyst.DatasetID{"ds_in_030"},
			Params: &analyst.ComputeOpParams{Field: fn("y"), Agg: func() *analyst.ComputeOpParamsAgg {
				a := analyst.ComputeOpParamsAggPercentile
				return &a
			}(), Percentile: f64(101)}}},
	}
	for _, tc := range cases {
		_, err := e.Run(tc.op, []*analyst.Dataset{ds})
		if !contracts.HasCode(err, contracts.ErrSchemaInvalid) {
			t.Fatalf("%s: want schema_invalid, got %v", tc.name, err)
		}
	}
}

func TestForecastOverflowRefuse(t *testing.T) {
	e := eng()
	huge := 1.7e308
	// Same-sign extremes: OLS sums / products overflow float64.
	ds := numDS("ds_in_031", "y", analyst.DataClassPublic, huge, huge, huge, huge)
	_, err := e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpForecastLinear, Inputs: []analyst.DatasetID{"ds_in_031"},
		Params: &analyst.ComputeOpParams{Field: fn("y"), Horizon: i64(1)},
	}, []*analyst.Dataset{ds})
	if err == nil {
		t.Fatal("expected overflow refusal")
	}
	if !contracts.HasCode(err, contracts.ErrForecastUnavailable) {
		t.Fatalf("want forecast_unavailable, got %v", err)
	}

	// Holt with extreme swing can diverge in level/trend floats
	ds2 := numDS("ds_in_032", "y", analyst.DataClassPublic, 1e300, -1e300, 1e300, -1e300, 1e300)
	_, err = e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpForecastHolt, Inputs: []analyst.DatasetID{"ds_in_032"},
		Params: &analyst.ComputeOpParams{Field: fn("y"), Horizon: i64(2), Alpha: f64(1), Beta: f64(1)},
	}, []*analyst.Dataset{ds2})
	if err == nil {
		t.Fatal("expected holt overflow/diverge refusal")
	}
	if !contracts.HasCode(err, contracts.ErrForecastUnavailable) {
		t.Fatalf("want forecast_unavailable, got %v", err)
	}

	// Non-finite cell refused as non-numeric (AsRat ok=false)
	ds3 := numDS("ds_in_033", "y", analyst.DataClassPublic, math.Inf(1), 2.0, 3.0, 4.0)
	_, err = e.Run(analyst.ComputeOp{
		Op: analyst.ComputeOpOpForecastLinear, Inputs: []analyst.DatasetID{"ds_in_033"},
		Params: &analyst.ComputeOpParams{Field: fn("y"), Horizon: i64(1)},
	}, []*analyst.Dataset{ds3})
	if !contracts.HasCode(err, contracts.ErrSchemaInvalid) {
		t.Fatalf("Inf cell: want schema_invalid, got %v", err)
	}
}

func TestHoltLocalGuard(t *testing.T) {
	_, _, err := forecastHolt([]*big.Rat{big.NewRat(1, 1)}, 1, 0.3, 0.1)
	if !contracts.HasCode(err, contracts.ErrInsufficientData) {
		t.Fatalf("want insufficient_data for <2 points, got %v", err)
	}
}

func TestRatFromFloat64Guard(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := ratFromFloat64(v); !contracts.HasCode(err, contracts.ErrForecastUnavailable) {
			t.Fatalf("%v: want forecast_unavailable, got %v", v, err)
		}
	}
	r, err := ratFromFloat64(1.5)
	if err != nil || r.Cmp(big.NewRat(3, 2)) != 0 {
		t.Fatalf("1.5: got %v %v", r, err)
	}
}

func FuzzComputeParams(f *testing.F) {
	f.Add("filter", "amount", int64(2), 50.0, 0.3, 0.1, int64(1))
	f.Add("aggregate", "amount", int64(0), 90.0, 0.0, 0.0, int64(0))
	f.Add("rank", "amount", int64(5), 0.0, 1.5, -1.0, int64(40))
	f.Add("forecast", "amount", int64(2), 50.0, 0.3, 0.1, int64(1))
	f.Add("forecast", "amount", int64(2), math.Inf(1), math.NaN(), 2.0, int64(-1))
	f.Fuzz(func(t *testing.T, opName, field string, top int64, pct, alpha, beta float64, horizon int64) {
		e := New(Options{MaxRows: 50, Forecasts: true})
		ds := moneyDS("ds_fuzz_01", 0.1, 0.2, nil, 0.3, 1.7e308, -1.7e308)
		var op analyst.ComputeOp
		op.Inputs = []analyst.DatasetID{"ds_fuzz_01"}
		p := &analyst.ComputeOpParams{Field: fn("amount"), Decimals: i64(2)}
		switch opName {
		case "filter":
			op.Op = analyst.ComputeOpOpFilter
			p.Predicate = &analyst.ComputeOpPredicate{Field: "amount", Op: analyst.ComputeOpPredicateOpGt, Value: 0.0}
		case "aggregate":
			op.Op = analyst.ComputeOpOpAggregate
			agg := analyst.ComputeOpParamsAggSum
			p.Agg = &agg
			p.Percentile = &pct
			if pct >= 0 && pct <= 100 && !math.IsNaN(pct) && !math.IsInf(pct, 0) {
				agg2 := analyst.ComputeOpParamsAggPercentile
				p.Agg = &agg2
			}
		case "rank":
			op.Op = analyst.ComputeOpOpRank
			p.TopN = &top
		case "forecast":
			op.Op = analyst.ComputeOpOpForecastHolt
			p.Horizon = &horizon
			p.Alpha = &alpha
			p.Beta = &beta
		default:
			op.Op = analyst.ComputeOpOpSort
		}
		_ = field
		op.Params = p
		_, _ = e.Run(op, []*analyst.Dataset{ds}) // must not panic
	})
}
