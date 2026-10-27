package compute

import (
	"encoding/json"
	"fmt"

	"github.com/buxee/alix-ai/internal/analyst/dataset"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// Options configure an Engine.
type Options struct {
	// MaxRows caps output rows (default 500). Exceeding → analyst_cap_exceeded.
	MaxRows int
	// Forecasts enables forecast_* ops; otherwise ability_disabled.
	Forecasts bool
	// MintDatasetID returns the next dataset id. Nil → ds_compute_<n>.
	MintDatasetID func() analyst.DatasetID
	// MintFactID returns the next derived fact id. Nil → df_compute_<n>.
	MintFactID func() string
}

// Engine runs catalog compute ops.
type Engine struct {
	opts    Options
	dsSeq   int
	factSeq int
}

// New builds an Engine.
func New(opts Options) *Engine {
	if opts.MaxRows <= 0 {
		opts.MaxRows = 500
	}
	if opts.MaxRows > 5000 {
		opts.MaxRows = 5000
	}
	return &Engine{opts: opts}
}

// Result is one successful compute step.
type Result struct {
	Dataset analyst.Dataset
	Facts   []analyst.DerivedFact
}

type runner func(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset) (Result, error)

// Run executes op over inputs (order must match op.Inputs). Inputs are not mutated.
func (e *Engine) Run(op analyst.ComputeOp, inputs []*analyst.Dataset) (Result, error) {
	if e == nil {
		return Result{}, contracts.New(contracts.ErrInternal, "compute engine is nil")
	}
	if !op.Op.Valid() {
		return Result{}, refuse(contracts.ErrSchemaInvalid, "unknown op %q", string(op.Op))
	}
	if err := checkInputs(op, inputs); err != nil {
		return Result{}, err
	}
	if err := validateParams(op); err != nil {
		return Result{}, err
	}
	var fn runner
	switch op.Op {
	case analyst.ComputeOpOpFilter:
		fn = runFilter
	case analyst.ComputeOpOpSort:
		fn = runSort
	case analyst.ComputeOpOpGroupBy:
		fn = runGroupBy
	case analyst.ComputeOpOpAggregate:
		fn = runAggregate
	case analyst.ComputeOpOpRatio:
		fn = runRatio
	case analyst.ComputeOpOpPercentChange:
		fn = runPercentChange
	case analyst.ComputeOpOpDifference:
		fn = runDifference
	case analyst.ComputeOpOpRank:
		fn = runRank
	case analyst.ComputeOpOpJoin:
		fn = runJoin
	case analyst.ComputeOpOpMovingAverage:
		fn = runMovingAverage
	case analyst.ComputeOpOpCumulative:
		fn = runCumulative
	case analyst.ComputeOpOpDateBucket:
		fn = runDateBucket
	case analyst.ComputeOpOpForecastLinear, analyst.ComputeOpOpForecastHolt, analyst.ComputeOpOpForecastSeasonalNaive:
		if !e.opts.Forecasts {
			return Result{}, refuse(contracts.ErrAbilityDisabled, "forecasts ability is off")
		}
		fn = runForecast
	default:
		return Result{}, refuse(contracts.ErrSchemaInvalid, "unknown op %q", string(op.Op))
	}
	res, err := fn(e, op, inputs)
	if err != nil {
		return Result{}, err
	}
	if len(res.Dataset.Rows) > e.opts.MaxRows {
		return Result{}, refuse(contracts.ErrAnalystCapExceeded, "output exceeds max_records_per_dataset (%d)", e.opts.MaxRows)
	}
	res.Dataset.RecordCount = int64(len(res.Dataset.Rows))
	return res, nil
}

func checkInputs(op analyst.ComputeOp, inputs []*analyst.Dataset) error {
	if len(op.Inputs) < 1 || len(op.Inputs) > 2 {
		return refuse(contracts.ErrSchemaInvalid, "inputs must have 1 or 2 dataset ids")
	}
	if op.Op == analyst.ComputeOpOpJoin && len(op.Inputs) != 2 {
		return refuse(contracts.ErrSchemaInvalid, "join requires exactly two inputs")
	}
	if len(inputs) != len(op.Inputs) {
		return refuse(contracts.ErrSchemaInvalid, "resolved inputs (%d) != proposal inputs (%d)", len(inputs), len(op.Inputs))
	}
	for i, ds := range inputs {
		if ds == nil {
			return refuse(contracts.ErrSchemaInvalid, "input %d is nil", i)
		}
		if ds.ID != op.Inputs[i] {
			return refuse(contracts.ErrSchemaInvalid, "input %d id %q != proposal %q", i, ds.ID, op.Inputs[i])
		}
	}
	return nil
}

func refuse(code contracts.ErrCode, format string, args ...any) error {
	return contracts.Newf(code, format, args...)
}

func (e *Engine) mintDS() analyst.DatasetID {
	if e.opts.MintDatasetID != nil {
		return e.opts.MintDatasetID()
	}
	e.dsSeq++
	return analyst.DatasetID(fmt.Sprintf("ds_compute_%03d", e.dsSeq))
}

func (e *Engine) mintFact() string {
	if e.opts.MintFactID != nil {
		return e.opts.MintFactID()
	}
	e.factSeq++
	return fmt.Sprintf("df_compute_%03d", e.factSeq)
}

func paramsMap(p *analyst.ComputeOpParams) map[string]any {
	if p == nil {
		return map[string]any{}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

func computeProvenance(op analyst.ComputeOp, inputs []*analyst.Dataset) analyst.DatasetDatasetProvenance {
	ids := make([]analyst.DatasetID, len(inputs))
	for i, ds := range inputs {
		ids[i] = ds.ID
	}
	name := string(op.Op)
	return analyst.DatasetDatasetProvenance{
		Kind:            analyst.DatasetDatasetProvenanceKindCompute,
		Op:              &name,
		Params:          paramsMap(op.Params),
		InputDatasetIDs: ids,
	}
}

func outDataset(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset, fields []analyst.DatasetDatasetField, rows []analyst.DatasetRow) analyst.Dataset {
	if rows == nil {
		rows = []analyst.DatasetRow{}
	}
	if fields == nil {
		fields = []analyst.DatasetDatasetField{}
	}
	return analyst.Dataset{
		ID:          e.mintDS(),
		Fields:      fields,
		Rows:        rows,
		RecordCount: int64(len(rows)),
		Provenance:  computeProvenance(op, inputs),
	}
}

func derivedFact(e *Engine, op analyst.ComputeOp, inputs []*analyst.Dataset, label string, value any, display string, unit, currency *string, class analyst.DataClass, fc *analyst.DerivedFactForecastMeta, dec int) analyst.DerivedFact {
	ids := make([]analyst.DatasetID, len(inputs))
	for i, ds := range inputs {
		ids[i] = ds.ID
	}
	sl := "computed"
	var disp *string
	if display != "" {
		disp = &display
	}
	return analyst.DerivedFact{
		ID:          e.mintFact(),
		Label:       label,
		Value:       value,
		Display:     disp,
		Unit:        unit,
		Currency:    currency,
		DataClass:   class,
		SourceLabel: &sl,
		Provenance: analyst.DerivedFactDerivedProvenance{
			Op:              string(op.Op),
			InputDatasetIDs: ids,
			Params:          paramsMap(op.Params),
			Rounding: analyst.DerivedFactRounding{
				Mode:     roundingMode(),
				Decimals: int64(dec),
			},
		},
		Forecast: fc,
	}
}

func requireField(ds *analyst.Dataset, name common.FieldName) (analyst.DatasetDatasetField, error) {
	f, ok := dataset.FieldByName(ds, name)
	if !ok {
		return analyst.DatasetDatasetField{}, refuse(contracts.ErrSchemaInvalid, "unknown field %q", string(name))
	}
	return f, nil
}

func paramField(p *analyst.ComputeOpParams) (common.FieldName, error) {
	if p == nil || p.Field == nil {
		return "", refuse(contracts.ErrSchemaInvalid, "params.field is required")
	}
	return *p.Field, nil
}

func skipNulls(p *analyst.ComputeOpParams) bool {
	if p != nil && p.Nulls != nil && *p.Nulls == analyst.ComputeOpParamsNullsKeep {
		return false
	}
	return true
}
