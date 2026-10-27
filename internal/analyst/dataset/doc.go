// Package dataset holds shared helpers for analyst datasets and derived facts
// (ADR 0047): field lookup, data-class ranking, row/cell coercion and
// display forms the verifier accepts.
//
// The integrator and the compute engine share these helpers; neither invents
// rows or field classes — those come from adapter reads or compute ops.
package dataset
