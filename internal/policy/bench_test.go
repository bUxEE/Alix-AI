package policy

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/clock"
)

// benchPolicy builds a policy of n rules over 20 capabilities of the kind the
// reference applications need: role conditions, workspace conditions and field
// thresholds.
func benchPackage(tb testing.TB, nRules int) (*Package, []string) {
	tb.Helper()
	var caps []CapabilityDecl
	var capIDs []string
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("area.op_%d", i)
		capIDs = append(capIDs, id)
		caps = append(caps, CapabilityDecl{
			ID: id, Audience: AudienceUser, EffectClass: EffectExternal, RiskLevel: 2, Confirmation: ConfirmExplicit,
			Fields: []FieldDecl{{Name: "amount_minor", Required: true}, {Name: "currency", Required: true}, {Name: "note", Sensitive: true}},
		})
	}
	var rules []string
	for i := 0; i < nRules; i++ {
		c := capIDs[i%len(capIDs)]
		switch i % 4 {
		case 0:
			rules = append(rules, fmt.Sprintf(`{"id":"r%d","capabilities":["%s"],"when":{"fields":{"amount_minor":{"gte":%d}}},"then":{"risk_min":3},"reason":"large"}`, i, c, 1000000+i))
		case 1:
			rules = append(rules, fmt.Sprintf(`{"id":"r%d","capabilities":["%s"],"when":{"workspaces":{"in":["ws-closed-%d"]}},"then":{"deny":true},"reason":"closed"}`, i, c, i))
		case 2:
			rules = append(rules, fmt.Sprintf(`{"id":"r%d","capabilities":["%s"],"when":{"roles":{"any":["viewer"]}},"then":{"hide_fields":["note"]},"reason":"hide"}`, i, c))
		default:
			rules = append(rules, fmt.Sprintf(`{"id":"r%d","capabilities":["%s"],"when":{"fields":{"currency":{"present":true,"not_in":["EUR","USD"]}}},"then":{"deny":true},"reason":"currency"}`, i, c))
		}
	}
	quoted := make([]string, len(capIDs))
	for i, c := range capIDs {
		quoted[i] = `"` + c + `"`
	}
	doc := fmt.Sprintf(`{"policy_id":"bench","role_grants":[{"role":"accountant","capabilities":[%s]},{"role":"viewer","capabilities":[%s]}],"risk_overrides":[],"field_projection":[],"rules":[%s]}`,
		strings.Join(quoted, ","), strings.Join(quoted, ","), strings.Join(rules, ","))
	pkg, err := NewPackage(PackageSpec{
		ApplicationID: appID, Version: "1", Status: StatusActive, Roles: []string{"accountant", "viewer"},
		Capabilities: caps, Policies: []*HostPolicy{mustParsePolicy(tb, doc)},
	})
	if err != nil {
		tb.Fatal(err)
	}
	return pkg, capIDs
}

// BenchmarkDecide measures one prepare-phase decision against a package whose
// policy holds 100 rules (ADR-0017 measurement 2).
func BenchmarkDecide(b *testing.B) {
	pkg, capIDs := benchPackage(b, 100)
	g := NewGateway(clock.NewFake(t0))
	s := scopeWith(capIDs, withRoles("accountant"))
	req := Request{
		Scope: s, Package: pkg, CapabilityID: capIDs[4], Binding: BindingOf(s), Phase: PhasePrepare,
		Fields: map[string]any{"amount_minor": 120000, "currency": "EUR"},
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if d := g.Decide(ctx, req); !d.Allowed {
			b.Fatalf("denied: %+v", d.Reasons)
		}
	}
}

// BenchmarkExpose measures building the visible tool set for 20 capabilities.
func BenchmarkExpose(b *testing.B) {
	pkg, capIDs := benchPackage(b, 100)
	g := NewGateway(clock.NewFake(t0))
	s := scopeWith(capIDs, withRoles("accountant"))
	req := ExposeRequest{Scope: s, Package: pkg, Binding: BindingOf(s)}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if e := g.Expose(ctx, req); len(e.Visible) != 20 {
			b.Fatalf("visible %d", len(e.Visible))
		}
	}
}

// BenchmarkParseHostPolicy measures loading a 100-rule document.
func BenchmarkParseHostPolicy(b *testing.B) {
	_, _ = benchPackage(b, 100)
	var rules []string
	for i := 0; i < 100; i++ {
		rules = append(rules, fmt.Sprintf(`{"id":"r%d","capabilities":["area.op_%d"],"when":{"fields":{"amount_minor":{"gte":%d}}},"then":{"risk_min":3},"reason":"large"}`, i, i%20, 1000000+i))
	}
	doc := []byte(fmt.Sprintf(`{"policy_id":"bench","role_grants":[{"role":"accountant","capabilities":["area.op_0"]}],"risk_overrides":[],"field_projection":[],"rules":[%s]}`, strings.Join(rules, ",")))
	b.ReportAllocs()
	b.SetBytes(int64(len(doc)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ParseHostPolicy(doc); err != nil {
			b.Fatal(err)
		}
	}
}
