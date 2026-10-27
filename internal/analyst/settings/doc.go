// Package settings stores tenant analyst ability ceilings and resolves the
// effective ability set (ADR 0047).
//
// Levels: deployment config ∧ application package assistant.abilities ∧ tenant
// store — most restrictive wins. A tenant admin may only narrow. The resolved
// controller's analyst_capable flag gates every ability. AbilityResolver returns
// the effective set and typed disabled_reasons. The gateway surface is
// GET|PUT /v1/ops/tenants/{tenant_id}/analyst and GET /v1/me/analyst
// (gateway.AnalystService).
package settings
