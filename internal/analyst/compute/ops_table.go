package compute

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/analyst/dataset"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

func runFilter(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset) (Result, error) {
	in := inputs[0]
	preds, err := filterPreds(op.Params)
	if err != nil {
		return Result{}, err
	}
	var rows []analyst.DatasetRow
	for _, row := range in.Rows {
		ok, err := matchAll(row, preds)
		if err != nil {
			return Result{}, err
		}
		if ok {
			rows = append(rows, dataset.CloneRow(row))
		}
	}
	if len(rows) == 0 {
		return Result{}, refuse(contracts.ErrInsufficientData, "filter produced no rows")
	}
	return Result{Dataset: outDataset(e, op, inputs, dataset.CloneFields(in.Fields), rows)}, nil
}

func filterPreds(p *analyst.ComputeOpParams) ([]analyst.ComputeOpPredicate, error) {
	if p == nil {
		return nil, refuse(contracts.ErrSchemaInvalid, "filter requires predicate(s)")
	}
	var out []analyst.ComputeOpPredicate
	if p.Predicate != nil {
		out = append(out, *p.Predicate)
	}
	out = append(out, p.Predicates...)
	if len(out) == 0 {
		return nil, refuse(contracts.ErrSchemaInvalid, "filter requires predicate(s)")
	}
	return out, nil
}

func matchAll(row analyst.DatasetRow, preds []analyst.ComputeOpPredicate) (bool, error) {
	for _, pr := range preds {
		ok, err := matchOne(row, pr)
		if err != nil || !ok {
			return ok, err
		}
	}
	return true, nil
}

func matchOne(row analyst.DatasetRow, pr analyst.ComputeOpPredicate) (bool, error) {
	if !pr.Op.Valid() {
		return false, refuse(contracts.ErrSchemaInvalid, "bad predicate op")
	}
	v := dataset.Cell(row, string(pr.Field))
	switch pr.Op {
	case analyst.ComputeOpPredicateOpIsNull:
		return dataset.IsNull(v), nil
	case analyst.ComputeOpPredicateOpNotNull:
		return !dataset.IsNull(v), nil
	}
	switch pr.Op {
	case analyst.ComputeOpPredicateOpEq, analyst.ComputeOpPredicateOpNeq:
		eq := cellsEqual(v, pr.Value)
		if pr.Op == analyst.ComputeOpPredicateOpEq {
			return eq, nil
		}
		return !eq, nil
	case analyst.ComputeOpPredicateOpContains:
		vs, _ := dataset.AsString(v)
		want, _ := dataset.AsString(pr.Value)
		return strings.Contains(vs, want), nil
	case analyst.ComputeOpPredicateOpIn:
		arr, ok := pr.Value.([]any)
		if !ok {
			return false, refuse(contracts.ErrSchemaInvalid, "in predicate needs array value")
		}
		for _, item := range arr {
			if cellsEqual(v, item) {
				return true, nil
			}
		}
		return false, nil
	case analyst.ComputeOpPredicateOpGt, analyst.ComputeOpPredicateOpGte, analyst.ComputeOpPredicateOpLt, analyst.ComputeOpPredicateOpLte:
		a, anull, aok := dataset.AsRat(v)
		b, bnull, bok := dataset.AsRat(pr.Value)
		if !aok || !bok || anull || bnull {
			// fall back to string compare for dates/strings
			as, _ := dataset.AsString(v)
			bs, _ := dataset.AsString(pr.Value)
			cmp := strings.Compare(as, bs)
			return cmpPred(cmp, pr.Op), nil
		}
		cmp := a.Cmp(b)
		return cmpPred(cmp, pr.Op), nil
	}
	return false, refuse(contracts.ErrSchemaInvalid, "unsupported predicate")
}

func cmpPred(cmp int, op analyst.ComputeOpPredicateOp) bool {
	switch op {
	case analyst.ComputeOpPredicateOpGt:
		return cmp > 0
	case analyst.ComputeOpPredicateOpGte:
		return cmp >= 0
	case analyst.ComputeOpPredicateOpLt:
		return cmp < 0
	case analyst.ComputeOpPredicateOpLte:
		return cmp <= 0
	}
	return false
}

func cellsEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	ra, an, aok := dataset.AsRat(a)
	rb, bn, bok := dataset.AsRat(b)
	if aok && bok && !an && !bn {
		return ra.Cmp(rb) == 0
	}
	as, _ := dataset.AsString(a)
	bs, _ := dataset.AsString(b)
	return as == bs
}

func runSort(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset) (Result, error) {
	in := inputs[0]
	field, err := paramField(op.Params)
	if err != nil {
		return Result{}, err
	}
	if _, err := requireField(in, field); err != nil {
		return Result{}, err
	}
	dir := analyst.ComputeOpParamsDirectionAsc
	if op.Params != nil && op.Params.Direction != nil {
		dir = *op.Params.Direction
	}
	keepNull := op.Params != nil && op.Params.Nulls != nil && *op.Params.Nulls == analyst.ComputeOpParamsNullsKeep
	rows := make([]analyst.DatasetRow, len(in.Rows))
	for i, r := range in.Rows {
		rows[i] = dataset.CloneRow(r)
	}
	name := string(field)
	sort.SliceStable(rows, func(i, j int) bool {
		vi := dataset.Cell(rows[i], name)
		vj := dataset.Cell(rows[j], name)
		ni, nj := dataset.IsNull(vi), dataset.IsNull(vj)
		if ni || nj {
			if ni && nj {
				return false
			}
			if !keepNull {
				// nulls last for asc, first for desc when skipping is not about sort — put nulls last
				if dir == analyst.ComputeOpParamsDirectionAsc {
					return !ni && nj
				}
				return ni && !nj
			}
			if dir == analyst.ComputeOpParamsDirectionAsc {
				return ni && !nj
			}
			return !ni && nj
		}
		cmp := compareCells(vi, vj)
		if dir == analyst.ComputeOpParamsDirectionDesc {
			return cmp > 0
		}
		return cmp < 0
	})
	return Result{Dataset: outDataset(e, op, inputs, dataset.CloneFields(in.Fields), rows)}, nil
}

func compareCells(a, b any) int {
	ra, an, aok := dataset.AsRat(a)
	rb, bn, bok := dataset.AsRat(b)
	if aok && bok && !an && !bn {
		return ra.Cmp(rb)
	}
	as, _ := dataset.AsString(a)
	bs, _ := dataset.AsString(b)
	return strings.Compare(as, bs)
}

func runRank(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset) (Result, error) {
	in := inputs[0]
	field, err := paramField(op.Params)
	if err != nil {
		return Result{}, err
	}
	if _, err := requireField(in, field); err != nil {
		return Result{}, err
	}
	topN := 10
	if op.Params != nil && op.Params.TopN != nil {
		topN = int(*op.Params.TopN)
	}
	dir := analyst.ComputeOpParamsDirectionDesc
	if op.Params != nil && op.Params.Direction != nil {
		dir = *op.Params.Direction
	}
	rows := sortRowsCopy(in.Rows, string(field), dir)
	if topN < len(rows) {
		rows = rows[:topN]
	}
	fields := dataset.CloneFields(in.Fields)
	rankName := common.FieldName("rank")
	fields = append(fields, analyst.DatasetDatasetField{
		Name: rankName, Type: analyst.FieldTypeNumber, DataClass: analyst.DataClassPublic,
	})
	out := make([]analyst.DatasetRow, len(rows))
	for i, r := range rows {
		nr := dataset.CloneRow(r)
		nr[string(rankName)] = float64(i + 1)
		out[i] = nr
	}
	return Result{Dataset: outDataset(e, op, inputs, fields, out)}, nil
}

func runJoin(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset) (Result, error) {
	left, right := inputs[0], inputs[1]
	if op.Params == nil || op.Params.LeftOn == nil || op.Params.RightOn == nil {
		return Result{}, refuse(contracts.ErrSchemaInvalid, "join requires left_on and right_on")
	}
	how := analyst.ComputeOpParamsHowInner
	if op.Params.How != nil {
		how = *op.Params.How
	}
	leftOn, rightOn := string(*op.Params.LeftOn), string(*op.Params.RightOn)
	if _, err := requireField(left, *op.Params.LeftOn); err != nil {
		return Result{}, err
	}
	if _, err := requireField(right, *op.Params.RightOn); err != nil {
		return Result{}, err
	}
	// Build output fields: left fields + right fields with collision rename right_<name>
	fields := dataset.CloneFields(left.Fields)
	leftNames := map[string]bool{}
	for _, f := range left.Fields {
		leftNames[string(f.Name)] = true
	}
	rightRename := map[string]string{}
	for _, f := range right.Fields {
		name := string(f.Name)
		outName := name
		if leftNames[name] {
			outName = "right_" + name
		}
		rightRename[name] = outName
		nf := f
		nf.Name = common.FieldName(outName)
		fields = append(fields, nf)
	}
	index := map[string][]analyst.DatasetRow{}
	for _, r := range right.Rows {
		k, _ := dataset.AsString(dataset.Cell(r, rightOn))
		index[k] = append(index[k], r)
	}
	var rows []analyst.DatasetRow
	for _, lr := range left.Rows {
		k, _ := dataset.AsString(dataset.Cell(lr, leftOn))
		matches := index[k]
		if len(matches) == 0 {
			if how == analyst.ComputeOpParamsHowLeft {
				row := dataset.CloneRow(lr)
				for _, f := range right.Fields {
					row[rightRename[string(f.Name)]] = nil
				}
				rows = append(rows, row)
			}
			continue
		}
		for _, rr := range matches {
			row := dataset.CloneRow(lr)
			for _, f := range right.Fields {
				src := string(f.Name)
				row[rightRename[src]] = dataset.Cell(rr, src)
			}
			rows = append(rows, row)
		}
	}
	return Result{Dataset: outDataset(e, op, inputs, fields, rows)}, nil
}

func runAggregate(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset) (Result, error) {
	in := inputs[0]
	if op.Params == nil || op.Params.Agg == nil {
		return Result{}, refuse(contracts.ErrSchemaInvalid, "aggregate requires params.agg")
	}
	agg := analyst.ComputeOpMeasureAgg(*op.Params.Agg)
	field, err := paramField(op.Params)
	if err != nil && agg != analyst.ComputeOpMeasureAggCount {
		return Result{}, err
	}
	dec := defaultDecimals(op.Params)
	val, unit, cur, class, err := aggregateValues(in, field, agg, op.Params.Percentile, skipNulls(op.Params), dec)
	if err != nil {
		return Result{}, err
	}
	asName := common.FieldName(string(agg))
	if op.Params.Field != nil {
		asName = common.FieldName(string(agg) + "_" + string(*op.Params.Field))
	}
	fields := []analyst.DatasetDatasetField{{
		Name: asName, Type: analyst.FieldTypeNumber, DataClass: class, Unit: unit, Currency: cur,
	}}
	row := analyst.DatasetRow{string(asName): val}
	label := string(asName)
	disp := formatDisplay(val, dec)
	fact := derivedFact(e, op, inputs, label, val, disp, unit, cur, class, nil, dec)
	return Result{
		Dataset: outDataset(e, op, inputs, fields, []analyst.DatasetRow{row}),
		Facts:   []analyst.DerivedFact{fact},
	}, nil
}

func aggregateValues(in *analyst.Dataset, field common.FieldName, agg analyst.ComputeOpMeasureAgg, pct *float64, skip bool, dec int) (any, *string, *string, analyst.DataClass, error) {
	class := analyst.DataClassPublic
	var unit, cur *string
	if agg != analyst.ComputeOpMeasureAggCount {
		f, err := requireField(in, field)
		if err != nil {
			return nil, nil, nil, class, err
		}
		class = f.DataClass
		if u := dataset.UnitOf(f); u != "" {
			unit = &u
		}
		if c := dataset.CurrencyOf(f); c != "" {
			cur = &c
		}
	} else if field != "" {
		if f, ok := dataset.FieldByName(in, field); ok {
			class = f.DataClass
		}
	}
	name := string(field)
	if agg == analyst.ComputeOpMeasureAggCount {
		var nCount int64
		if field == "" {
			nCount = int64(len(in.Rows))
		} else {
			for _, row := range in.Rows {
				v := dataset.Cell(row, name)
				if dataset.IsNull(v) && skip {
					continue
				}
				nCount++
			}
		}
		return float64(nCount), nil, nil, class, nil
	}
	var nums []*big.Rat
	for _, row := range in.Rows {
		v := dataset.Cell(row, name)
		r, null, ok := dataset.AsRat(v)
		if !ok {
			return nil, nil, nil, class, refuse(contracts.ErrSchemaInvalid, "non-numeric value in %q", name)
		}
		if null {
			if skip {
				continue
			}
			return nil, nil, nil, class, refuse(contracts.ErrInsufficientData, "null in aggregate with nulls=keep")
		}
		nums = append(nums, r)
	}
	if len(nums) == 0 {
		return nil, nil, nil, class, refuse(contracts.ErrInsufficientData, "no values to aggregate")
	}
	var out *big.Rat
	switch agg {
	case analyst.ComputeOpMeasureAggSum:
		out = sumRats(nums)
	case analyst.ComputeOpMeasureAggAvg:
		out = avgRat(nums)
	case analyst.ComputeOpMeasureAggMin:
		out = minRat(nums)
	case analyst.ComputeOpMeasureAggMax:
		out = maxRat(nums)
	case analyst.ComputeOpMeasureAggMedian:
		out = medianRat(nums)
	case analyst.ComputeOpMeasureAggPercentile:
		if pct == nil {
			return nil, nil, nil, class, refuse(contracts.ErrSchemaInvalid, "percentile requires params.percentile")
		}
		out = percentileLinear(nums, *pct)
	default:
		return nil, nil, nil, class, refuse(contracts.ErrSchemaInvalid, "unknown agg")
	}
	return roundRat(out, dec, roundingMode()), unit, cur, class, nil
}

func formatDisplay(v any, dec int) string {
	switch x := v.(type) {
	case float64:
		r, err := ratFromFloat64(x)
		if err != nil {
			return fmt.Sprintf("%v", x)
		}
		return r.FloatString(dec)
	case int64:
		return fmt.Sprintf("%d", x)
	default:
		return fmt.Sprint(v)
	}
}

func runGroupBy(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset) (Result, error) {
	in := inputs[0]
	if op.Params == nil || len(op.Params.Fields) == 0 || len(op.Params.Measures) == 0 {
		return Result{}, refuse(contracts.ErrSchemaInvalid, "group_by requires fields and measures")
	}
	keys := op.Params.Fields
	for _, k := range keys {
		if _, err := requireField(in, k); err != nil {
			return Result{}, err
		}
	}
	type bucket struct {
		keyVals []any
		rows    []analyst.DatasetRow
	}
	order := []string{}
	groups := map[string]*bucket{}
	for _, row := range in.Rows {
		parts := make([]string, len(keys))
		vals := make([]any, len(keys))
		for i, k := range keys {
			v := dataset.Cell(row, string(k))
			vals[i] = v
			s, _ := dataset.AsString(v)
			parts[i] = s
		}
		gk := strings.Join(parts, "\x1e")
		b, ok := groups[gk]
		if !ok {
			b = &bucket{keyVals: vals}
			groups[gk] = b
			order = append(order, gk)
		}
		b.rows = append(b.rows, row)
	}
	fields := make([]analyst.DatasetDatasetField, 0, len(keys)+len(op.Params.Measures))
	for _, k := range keys {
		f, _ := dataset.FieldByName(in, k)
		fields = append(fields, f)
	}
	dec := defaultDecimals(op.Params)
	for _, m := range op.Params.Measures {
		mf, err := requireField(in, m.Field)
		if err != nil && m.Agg != analyst.ComputeOpMeasureAggCount {
			return Result{}, err
		}
		class := analyst.DataClassPublic
		var unit, cur *string
		if m.Agg != analyst.ComputeOpMeasureAggCount {
			class = mf.DataClass
			if u := dataset.UnitOf(mf); u != "" {
				unit = &u
			}
			if c := dataset.CurrencyOf(mf); c != "" {
				cur = &c
			}
		}
		fields = append(fields, analyst.DatasetDatasetField{
			Name: m.As, Type: analyst.FieldTypeNumber, DataClass: class, Unit: unit, Currency: cur,
		})
	}
	var outRows []analyst.DatasetRow
	for _, gk := range order {
		b := groups[gk]
		sub := &analyst.Dataset{Fields: in.Fields, Rows: b.rows}
		row := analyst.DatasetRow{}
		for i, k := range keys {
			row[string(k)] = b.keyVals[i]
		}
		for _, m := range op.Params.Measures {
			val, _, _, _, err := aggregateValues(sub, m.Field, m.Agg, m.Percentile, skipNulls(op.Params), dec)
			if err != nil {
				return Result{}, err
			}
			row[string(m.As)] = val
		}
		outRows = append(outRows, row)
	}
	return Result{Dataset: outDataset(e, op, inputs, fields, outRows)}, nil
}

func runRatio(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset) (Result, error) {
	return runBinary(e, op, inputs, "ratio", true)
}

func runPercentChange(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset) (Result, error) {
	return runBinary(e, op, inputs, "percent_change", true)
}

func runDifference(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset) (Result, error) {
	return runBinary(e, op, inputs, "difference", false)
}

func runBinary(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset, kind string, ratioLike bool) (Result, error) {
	in := inputs[0]
	p := op.Params
	if p == nil {
		return Result{}, refuse(contracts.ErrSchemaInvalid, "%s requires params", kind)
	}
	var aName, bName common.FieldName
	var outName string
	switch kind {
	case "ratio":
		if p.Numerator == nil || p.Denominator == nil {
			return Result{}, refuse(contracts.ErrSchemaInvalid, "ratio requires numerator and denominator")
		}
		aName, bName = *p.Numerator, *p.Denominator
		outName = "ratio"
	case "percent_change", "difference":
		if p.BaselineField == nil || p.CurrentField == nil {
			return Result{}, refuse(contracts.ErrSchemaInvalid, "%s requires baseline_field and current_field", kind)
		}
		aName, bName = *p.CurrentField, *p.BaselineField
		outName = kind
	}
	fa, err := requireField(in, aName)
	if err != nil {
		return Result{}, err
	}
	fb, err := requireField(in, bName)
	if err != nil {
		return Result{}, err
	}
	if kind == "difference" {
		if !dataset.SameMeasure(fa, fb) {
			return Result{}, refuse(contracts.ErrUnitMismatch, "difference unit/currency mismatch")
		}
	} else {
		// commensurate units: same unit string (both empty ok)
		if dataset.UnitOf(fa) != dataset.UnitOf(fb) {
			return Result{}, refuse(contracts.ErrUnitMismatch, "%s unit mismatch", kind)
		}
		if dataset.CurrencyOf(fa) != dataset.CurrencyOf(fb) && kind != "ratio" {
			// ratio of money/money → dimensionless; currency may match or both money
			if dataset.CurrencyOf(fa) == "" || dataset.CurrencyOf(fb) == "" {
				if dataset.CurrencyOf(fa) != dataset.CurrencyOf(fb) {
					return Result{}, refuse(contracts.ErrUnitMismatch, "%s currency mismatch", kind)
				}
			}
		}
	}
	dec := defaultDecimals(p)
	class := dataset.MaxClass(fa.DataClass, fb.DataClass)
	var unit *string
	var cur *string
	switch kind {
	case "ratio":
		u := "1"
		unit = &u
	case "percent_change":
		u := "percent"
		unit = &u
	default:
		if dataset.UnitOf(fa) != "" {
			u := dataset.UnitOf(fa)
			unit = &u
		}
		if dataset.CurrencyOf(fa) != "" {
			c := dataset.CurrencyOf(fa)
			cur = &c
		}
	}
	rows := make([]analyst.DatasetRow, 0, len(in.Rows))
	for _, row := range in.Rows {
		ra, an, aok := dataset.AsRat(dataset.Cell(row, string(aName)))
		rb, bn, bok := dataset.AsRat(dataset.Cell(row, string(bName)))
		nr := dataset.CloneRow(row)
		if !aok || !bok || an || bn {
			nr[outName] = nil
			rows = append(rows, nr)
			continue
		}
		var out *big.Rat
		switch kind {
		case "ratio":
			if rb.Sign() == 0 {
				return Result{}, refuse(contracts.ErrInsufficientData, "divide by zero")
			}
			out = new(big.Rat).Quo(ra, rb)
		case "percent_change":
			if rb.Sign() == 0 {
				return Result{}, refuse(contracts.ErrInsufficientData, "divide by zero")
			}
			out = new(big.Rat).Quo(new(big.Rat).Sub(ra, rb), rb)
			out.Mul(out, big.NewRat(100, 1))
		case "difference":
			out = new(big.Rat).Sub(ra, rb)
		}
		nr[outName] = roundRat(out, dec, roundingMode())
		rows = append(rows, nr)
		_ = ratioLike
	}
	fields := dataset.CloneFields(in.Fields)
	fields = append(fields, analyst.DatasetDatasetField{
		Name: common.FieldName(outName), Type: analyst.FieldTypeNumber, DataClass: class, Unit: unit, Currency: cur,
	})
	return Result{Dataset: outDataset(e, op, inputs, fields, rows)}, nil
}

func runMovingAverage(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset) (Result, error) {
	return runWindow(e, op, inputs, true)
}

func runCumulative(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset) (Result, error) {
	return runWindow(e, op, inputs, false)
}

func runWindow(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset, moving bool) (Result, error) {
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
		if _, err := requireField(in, orderBy); err != nil {
			return Result{}, err
		}
	}
	window := 2
	if moving {
		if op.Params == nil || op.Params.Window == nil {
			return Result{}, refuse(contracts.ErrSchemaInvalid, "moving_average requires window")
		}
		window = int(*op.Params.Window)
	}
	dec := defaultDecimals(op.Params)
	rows := sortRowsCopy(in.Rows, string(orderBy), analyst.ComputeOpParamsDirectionAsc)
	outName := "moving_average"
	if !moving {
		outName = "cumulative"
	}
	vals := make([]*big.Rat, len(rows))
	for i, row := range rows {
		r, null, ok := dataset.AsRat(dataset.Cell(row, string(field)))
		if !ok {
			return Result{}, refuse(contracts.ErrSchemaInvalid, "non-numeric")
		}
		if null {
			vals[i] = nil
		} else {
			vals[i] = r
		}
	}
	outRows := make([]analyst.DatasetRow, len(rows))
	var cum *big.Rat
	for i := range rows {
		nr := dataset.CloneRow(rows[i])
		if moving {
			if i+1 < window {
				nr[outName] = nil
			} else {
				var xs []*big.Rat
				for j := i - window + 1; j <= i; j++ {
					if vals[j] == nil {
						xs = nil
						break
					}
					xs = append(xs, vals[j])
				}
				if xs == nil {
					nr[outName] = nil
				} else {
					nr[outName] = roundRat(avgRat(xs), dec, roundingMode())
				}
			}
		} else {
			if vals[i] == nil {
				nr[outName] = nil
			} else {
				if cum == nil {
					cum = new(big.Rat)
				}
				cum.Add(cum, vals[i])
				nr[outName] = roundRat(new(big.Rat).Set(cum), dec, roundingMode())
			}
		}
		outRows[i] = nr
	}
	fields := dataset.CloneFields(in.Fields)
	fields = append(fields, analyst.DatasetDatasetField{
		Name: common.FieldName(outName), Type: analyst.FieldTypeNumber, DataClass: f.DataClass,
		Unit: f.Unit, Currency: f.Currency,
	})
	return Result{Dataset: outDataset(e, op, inputs, fields, outRows)}, nil
}

func runDateBucket(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset) (Result, error) {
	in := inputs[0]
	if op.Params == nil || op.Params.OrderBy == nil || op.Params.Bucket == nil {
		return Result{}, refuse(contracts.ErrSchemaInvalid, "date_bucket requires order_by and bucket")
	}
	ob := *op.Params.OrderBy
	f, err := requireField(in, ob)
	if err != nil {
		return Result{}, err
	}
	grain := *op.Params.Bucket
	outField := common.FieldName("bucket")
	fields := []analyst.DatasetDatasetField{
		{Name: outField, Type: analyst.FieldTypeString, DataClass: f.DataClass},
	}
	// optional count
	countName := common.FieldName("count")
	fields = append(fields, analyst.DatasetDatasetField{Name: countName, Type: analyst.FieldTypeNumber, DataClass: analyst.DataClassPublic})

	counts := map[string]int64{}
	var order []string
	for _, row := range in.Rows {
		v := dataset.Cell(row, string(ob))
		if dataset.IsNull(v) {
			continue
		}
		s, _ := dataset.AsString(v)
		key, err := bucketKey(s, grain)
		if err != nil {
			return Result{}, err
		}
		if _, ok := counts[key]; !ok {
			order = append(order, key)
		}
		counts[key]++
	}
	sort.Strings(order)
	rows := make([]analyst.DatasetRow, len(order))
	for i, k := range order {
		rows[i] = analyst.DatasetRow{string(outField): k, string(countName): float64(counts[k])}
	}
	return Result{Dataset: outDataset(e, op, inputs, fields, rows)}, nil
}

func bucketKey(s string, grain analyst.ComputeOpParamsBucket) (string, error) {
	t, err := parseTimeUTC(s)
	if err != nil {
		return "", refuse(contracts.ErrSchemaInvalid, "bad date %q", s)
	}
	t = t.UTC()
	switch grain {
	case analyst.ComputeOpParamsBucketDay:
		return t.Format("2006-01-02"), nil
	case analyst.ComputeOpParamsBucketWeek:
		y, w := t.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w), nil
	case analyst.ComputeOpParamsBucketMonth:
		return t.Format("2006-01"), nil
	case analyst.ComputeOpParamsBucketQuarter:
		q := (int(t.Month())-1)/3 + 1
		return fmt.Sprintf("%04d-Q%d", t.Year(), q), nil
	case analyst.ComputeOpParamsBucketYear:
		return t.Format("2006"), nil
	}
	return "", refuse(contracts.ErrSchemaInvalid, "bad bucket")
}

func parseTimeUTC(s string) (time.Time, error) {
	layouts := []string{
		time.RFC3339Nano, time.RFC3339, "2006-01-02", "2006-01-02T15:04:05", "2006-01-02 15:04:05",
	}
	var last error
	for _, l := range layouts {
		t, err := time.Parse(l, s)
		if err == nil {
			return t.UTC(), nil
		}
		last = err
	}
	return time.Time{}, last
}
