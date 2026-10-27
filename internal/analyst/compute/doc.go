// Package compute is the deterministic analyst compute engine (ADR 0047, task A).
//
// The model proposes a ComputeOp; the core validates parameters, refuses unit or
// currency mismatches and insufficient series with typed ErrCodes, and emits a
// dataset and optional derived facts. There is no code execution.
//
// # Integrator API
//
//	eng := compute.New(compute.Options{MaxRows: 500, Forecasts: true})
//	res, err := eng.Run(op, []*analyst.Dataset{in})
//
// Run returns contracts.Error with codes schema_invalid, unit_mismatch,
// insufficient_data, forecast_unavailable, analyst_cap_exceeded, or
// ability_disabled. On success, Result.Dataset has provenance kind=compute with
// op, params and input ids; Result.Facts carry rounding and forecast metadata
// when applicable. Output field data_class is the most restrictive class of
// contributing inputs. Same inputs and Options with fixed Mint* yield the same
// JSON bytes for dataset rows/fields/provenance and facts.
//
// Caps: Options.MaxRows (default 500) truncates with analyst_cap_exceeded when
// an op would emit more rows. Forecast ops require Options.Forecasts.
package compute
