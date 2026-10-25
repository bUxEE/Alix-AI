package mcp

import (
	"testing"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
)

func TestSmokeHTTP(t *testing.T) {
	a, h, app := newHTTPAdapter(t)
	app.Seed("ten_a", "wks_a", "item_1", "Widget")
	ctx := testContext(t)
	d, err := a.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("description: %+v", d)
	res, err := a.Read(ctx, testScope(), adapters.ReadRequest{CapabilityID: fake.ReadCapability, Fields: map[string]any{"item_id": "item_1"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("read: %+v", res.Status)
	for _, e := range h.Log() {
		t.Logf("server saw %s %s", e.Method, e.Tool)
	}
}
