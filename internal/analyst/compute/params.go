package compute

import (
	"math"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

// validateParams enforces schema bounds on numeric params (schema_invalid).
func validateParams(op analyst.ComputeOp) error {
	p := op.Params
	if p == nil {
		return nil
	}
	if p.Percentile != nil {
		v := *p.Percentile
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100 {
			return refuse(contracts.ErrSchemaInvalid, "percentile must be in [0,100]")
		}
	}
	if p.Alpha != nil {
		v := *p.Alpha
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 1 {
			return refuse(contracts.ErrSchemaInvalid, "alpha must be in (0,1]")
		}
	}
	if p.Beta != nil {
		v := *p.Beta
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 1 {
			return refuse(contracts.ErrSchemaInvalid, "beta must be in (0,1]")
		}
	}
	if p.Confidence != nil {
		v := *p.Confidence
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0.5 || v > 0.99 {
			return refuse(contracts.ErrSchemaInvalid, "confidence must be in [0.5,0.99]")
		}
	}
	if p.Horizon != nil {
		h := *p.Horizon
		if h < 1 || h > 36 {
			return refuse(contracts.ErrSchemaInvalid, "horizon must be in [1,36]")
		}
	}
	if p.Window != nil {
		w := *p.Window
		if w < 2 || w > 365 {
			return refuse(contracts.ErrSchemaInvalid, "window must be in [2,365]")
		}
	}
	if p.SeasonLength != nil {
		s := *p.SeasonLength
		if s < 2 || s > 366 {
			return refuse(contracts.ErrSchemaInvalid, "season_length must be in [2,366]")
		}
	}
	if p.TopN != nil {
		n := *p.TopN
		if n < 1 || n > 1000 {
			return refuse(contracts.ErrSchemaInvalid, "top_n must be in [1,1000]")
		}
	}
	if p.Decimals != nil {
		d := *p.Decimals
		if d < 0 || d > 8 {
			return refuse(contracts.ErrSchemaInvalid, "decimals must be in [0,8]")
		}
	}
	for _, m := range p.Measures {
		if m.Percentile != nil {
			v := *m.Percentile
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100 {
				return refuse(contracts.ErrSchemaInvalid, "measure percentile must be in [0,100]")
			}
		}
	}
	return nil
}
