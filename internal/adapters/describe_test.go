package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// TestDescribeV1GoldenKeepsDecoding: the version 1 Description document is
// frozen. This file is a copy of what a v1 adapter emitted; it must keep
// decoding strictly into the current type and must validate. A change that
// breaks it breaks every adapter already deployed, and needs a contract
// version bump instead.
func TestDescribeV1GoldenKeepsDecoding(t *testing.T) {
	raw, err := os.ReadFile("testdata/describe_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var d Description
	if err := decodeStrict(raw, &d); err != nil {
		t.Fatalf("the frozen v1 document no longer decodes: %v", err)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("the frozen v1 document no longer validates: %v", err)
	}
	want := Description{
		Transport:          "mcp",
		ContractVersions:   []int{1},
		ApplicationVersion: "2.7.1",
		Capabilities: []CapabilityOps{
			{CapabilityID: "appointments.book", Operations: []Operation{OpValidate, OpPrepare, OpCommit, OpStatus}},
			{CapabilityID: "appointments.list_availability", Operations: []Operation{OpRead}},
		},
		Unmapped: []string{"admin_reset"},
	}
	if diff := cmp.Diff(want, d); diff != "" {
		t.Fatalf("v1 document decoded differently (-want +got):\n%s", diff)
	}
	// Re-encoding keeps every v1 member under its v1 name.
	out, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(out, &members); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"transport", "contract_versions", "application_version", "capabilities", "unmapped"} {
		if _, ok := members[k]; !ok {
			t.Errorf("encoded Description lost member %q", k)
		}
	}

	// And through the host-facing parser, with the transport set by the adapter.
	p, err := ParseDescription("openapi", raw)
	if err != nil || p.Transport != "openapi" {
		t.Fatalf("ParseDescription = %+v, %v; the transport is the adapter's, not the document's", p, err)
	}
}

func TestParseDescriptionIgnoresUnknownMembersButValidatesTheRest(t *testing.T) {
	// A newer host adds members and a newer contract version: still describable.
	newer := `{"contract_versions":[1,2],"capabilities":[{"capability_id":"a.b","operations":["read"],"x_cost":3}],"x_future":{"k":[1,2,3]},"application_version":"9.0"}`
	d, err := ParseDescription("native", []byte(newer))
	if err != nil {
		t.Fatalf("a newer description was refused: %v", err)
	}
	if v, err := SelectVersion(SupportedContractVersions, d.ContractVersions); err != nil || v != 1 {
		t.Fatalf("negotiated %d, %v", v, err)
	}

	bad := map[string]string{
		"not json":              `nope`,
		"two documents":         `{"contract_versions":[1],"capabilities":[]}{"contract_versions":[1]}`,
		"array":                 `[]`,
		"no versions":           `{"capabilities":[]}`,
		"empty versions":        `{"contract_versions":[],"capabilities":[]}`,
		"version zero":          `{"contract_versions":[0],"capabilities":[]}`,
		"negative version":      `{"contract_versions":[-1],"capabilities":[]}`,
		"duplicate version":     `{"contract_versions":[1,1],"capabilities":[]}`,
		"version as string":     `{"contract_versions":["1"],"capabilities":[]}`,
		"too many versions":     `{"contract_versions":[1,2,3,4,5,6,7,8,9],"capabilities":[]}`,
		"bad capability id":     `{"contract_versions":[1],"capabilities":[{"capability_id":"a b","operations":["read"]}]}`,
		"wildcard capability":   `{"contract_versions":[1],"capabilities":[{"capability_id":"*","operations":["read"]}]}`,
		"duplicate capability":  `{"contract_versions":[1],"capabilities":[{"capability_id":"a","operations":["read"]},{"capability_id":"a","operations":["read"]}]}`,
		"no operations":         `{"contract_versions":[1],"capabilities":[{"capability_id":"a","operations":[]}]}`,
		"unknown operation":     `{"contract_versions":[1],"capabilities":[{"capability_id":"a","operations":["delete"]}]}`,
		"non-result operation":  `{"contract_versions":[1],"capabilities":[{"capability_id":"a","operations":["health"]}]}`,
		"duplicate operation":   `{"contract_versions":[1],"capabilities":[{"capability_id":"a","operations":["read","read"]}]}`,
		"control in version":    `{"contract_versions":[1],"capabilities":[],"application_version":"1.0\u0000"}`,
		"empty unmapped name":   `{"contract_versions":[1],"capabilities":[],"unmapped":[""]}`,
		"unmapped with newline": `{"contract_versions":[1],"capabilities":[],"unmapped":["a\nb"]}`,
	}
	for name, raw := range bad {
		if _, err := ParseDescription("mcp", []byte(raw)); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Errorf("%s: got %v, want schema_invalid", name, err)
		}
	}
	if _, err := ParseDescription("Not A Transport", []byte(newer)); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
		t.Errorf("malformed transport name accepted: %v", err)
	}
	huge := Description{Transport: "mcp", ContractVersions: []int{1}}
	for i := 0; i <= maxDescribeCapabilities; i++ {
		huge.Capabilities = append(huge.Capabilities, CapabilityOps{CapabilityID: fmt.Sprintf("cap_%d", i), Operations: []Operation{OpRead}})
	}
	if err := huge.Validate(); err == nil {
		t.Error("an oversized description was accepted")
	}
}

type describeStub struct {
	AppAdapter
	d   Description
	err error
}

func (s describeStub) Describe(context.Context) (Description, error) { return s.d, s.err }

// TestVersionNegotiation: the highest shared contract version is chosen, and a
// host that speaks only versions the core does not is refused.
func TestVersionNegotiation(t *testing.T) {
	mk := func(vs ...int) describeStub {
		return describeStub{d: Description{Transport: "fake", ContractVersions: vs}}
	}
	cases := []struct {
		name string
		core []int
		host []int
		want int
	}{
		{"same", []int{1}, []int{1}, 1},
		{"host knows more", []int{1}, []int{1, 2, 3}, 1},
		{"core knows more", []int{1, 2}, []int{1}, 1},
		{"highest shared wins", []int{1, 2, 3}, []int{3, 1, 5}, 3},
		{"order is irrelevant", []int{1, 2}, []int{2, 1}, 2},
		{"only a higher major", []int{1}, []int{2}, 0},
		{"only higher majors", []int{1}, []int{2, 3, 7}, 0},
		{"nothing shared", []int{2}, []int{1, 3}, 0},
		{"empty host list", []int{1}, nil, 0},
	}
	for _, tc := range cases {
		got, err := SelectVersion(tc.core, tc.host)
		if tc.want == 0 {
			if err == nil {
				t.Errorf("%s: got version %d, want a refusal", tc.name, got)
				continue
			}
			if !contracts.HasCode(err, contracts.ErrValidationFailed) || reasonOf(err) != "version_incompatible" {
				t.Errorf("%s: %v, want validation_failed with reason version_incompatible", tc.name, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: got %d, %v, want %d", tc.name, got, err, tc.want)
		}
	}

	n, err := Negotiate(context.Background(), mk(1, 2))
	if err != nil || n.Version != 1 || n.Description.Transport != "fake" {
		t.Fatalf("Negotiate = %+v, %v", n, err)
	}
	if _, err := Negotiate(context.Background(), mk(2)); !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Fatalf("a host that only speaks v2: %v", err)
	}
	boom := contracts.New(contracts.ErrUnavailable, "down")
	if _, err := Negotiate(context.Background(), describeStub{err: boom}); !errors.Is(err, boom) {
		t.Fatalf("a Describe failure was not passed on: %v", err)
	}
	if _, err := Negotiate(context.Background(), describeStub{d: Description{Transport: "fake"}}); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
		t.Fatalf("an invalid description was negotiated: %v", err)
	}
}

func TestDescriptionSupports(t *testing.T) {
	d := Description{Capabilities: []CapabilityOps{{CapabilityID: "a.b", Operations: []Operation{OpRead}}}}
	if !d.Supports("a.b", OpRead) || d.Supports("a.b", OpCommit) || d.Supports("a.c", OpRead) {
		t.Fatal("Supports")
	}
}

func TestHealthReportValidation(t *testing.T) {
	ok := []HealthReport{
		{Status: HealthOK},
		{Status: HealthOK, ApplicationVersion: "1.2"},
		{Status: HealthDegraded, Reason: "queue is backed up"},
		{Status: HealthUnavailable, Reason: "database down"},
	}
	for _, h := range ok {
		if err := h.Validate(); err != nil {
			t.Errorf("%+v: %v", h, err)
		}
	}
	bad := []HealthReport{
		{},
		{Status: "fine"},
		{Status: HealthDegraded},
		{Status: HealthUnavailable, Reason: "  "},
		{Status: HealthOK, Reason: strings.Repeat("x", maxHealthReason+1)},
		{Status: HealthOK, Reason: "line\nbreak"},
	}
	for _, h := range bad {
		if err := h.Validate(); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Errorf("%+v: got %v, want schema_invalid", h, err)
		}
	}
	if h, err := ParseHealth([]byte(`{"status":"ok","x_new":1}`)); err != nil || h.Status != HealthOK {
		t.Errorf("unknown member: %+v, %v", h, err)
	}
	for _, raw := range []string{``, `{}`, `[]`, `{"status":"ok"} {}`, `{"status":"meh"}`, `"ok"`} {
		if _, err := ParseHealth([]byte(raw)); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Errorf("%q: got %v", raw, err)
		}
	}
}
