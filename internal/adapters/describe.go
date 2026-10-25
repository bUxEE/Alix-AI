package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// ContractVersion is the version of the AppAdapter contract this build speaks.
// A change that old hosts or old adapters cannot ignore raises it; a change
// they can ignore (a new optional member of Description) does not.
const ContractVersion = 1

// SupportedContractVersions are the contract versions the core can speak, newest
// last. Negotiate picks the highest one the adapter shares.
var SupportedContractVersions = []int{ContractVersion}

// Limits on a Description. They bound what a host can make the core hold.
const (
	maxDescribeVersions     = 8
	maxDescribeCapabilities = 1024
	maxDescribeUnmapped     = 1024
	maxDescribeText         = 256
	maxContractVersion      = 1000
)

// CapabilityOps lists the operations an adapter can perform for one
// capability: the operations the application package mapped to a call.
type CapabilityOps struct {
	CapabilityID string      `json:"capability_id"`
	Operations   []Operation `json:"operations"`
}

// Description is what Describe returns. Its JSON form is frozen for version 1:
// members may be added as optional, never removed or retyped, and an old
// document must keep decoding (testdata/describe_v1.json pins one).
type Description struct {
	// Transport names the kind of adapter: "mcp", "openapi", "native", "fake".
	// The adapter sets it, not the host.
	Transport string `json:"transport"`
	// ContractVersions are the AppAdapter contract versions the adapter and
	// host application can speak. At least one.
	ContractVersions []int `json:"contract_versions"`
	// ApplicationVersion is the host application's own version label, opaque.
	ApplicationVersion string `json:"application_version,omitempty"`
	// Capabilities are the mapped operations, per capability.
	Capabilities []CapabilityOps `json:"capabilities"`
	// Unmapped names operations the host exposes that no capability is mapped
	// to (an MCP tool, an OpenAPI operationId, a native method). They are never
	// callable; the integration audit reports them as undocumented or
	// over-broad (check 7).
	Unmapped []string `json:"unmapped,omitempty"`
}

var transportRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// validIdentifier is the grammar of capability, entity and scope identifiers:
// 1 to 128 characters, no whitespace, no control characters and no "*". It
// mirrors schemas/common/ids.schema.json and contracts.Scope.
func validIdentifier(s string) bool {
	if s == "" || utf8.RuneCountInString(s) > contracts.MaxScopeValueLen || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r == '*' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// shortText reports whether s is printable, has no control characters and is at
// most max characters. Empty is allowed.
func shortText(s string, max int) bool {
	if utf8.RuneCountInString(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// Validate checks a Description. A violation is schema_invalid: the host sent
// something that is not a description, and none of it is used.
func (d Description) Validate() error {
	fail := func(format string, args ...any) error {
		return schemaInvalid("description: " + fmt.Sprintf(format, args...))
	}
	if !transportRE.MatchString(d.Transport) {
		return fail("transport is empty or malformed")
	}
	if n := len(d.ContractVersions); n == 0 || n > maxDescribeVersions {
		return fail("contract_versions must list between 1 and %d versions", maxDescribeVersions)
	}
	seenV := map[int]bool{}
	for _, v := range d.ContractVersions {
		if v < 1 || v > maxContractVersion || seenV[v] {
			return fail("contract_versions holds an invalid or duplicate version")
		}
		seenV[v] = true
	}
	if !shortText(d.ApplicationVersion, maxDescribeText) {
		return fail("application_version is not short printable text")
	}
	if len(d.Capabilities) > maxDescribeCapabilities {
		return fail("more than %d capabilities", maxDescribeCapabilities)
	}
	seenC := map[string]bool{}
	for _, c := range d.Capabilities {
		if !validIdentifier(c.CapabilityID) || seenC[c.CapabilityID] {
			return fail("a capability id is malformed or duplicated")
		}
		seenC[c.CapabilityID] = true
		if len(c.Operations) == 0 || len(c.Operations) > len(ResultOperations()) {
			return fail("a capability lists no operations or too many")
		}
		seenO := map[Operation]bool{}
		for _, op := range c.Operations {
			if _, ok := op.Result(); !ok || seenO[op] {
				return fail("a capability lists an unknown or duplicate operation")
			}
			seenO[op] = true
		}
	}
	if len(d.Unmapped) > maxDescribeUnmapped {
		return fail("more than %d unmapped operations", maxDescribeUnmapped)
	}
	for _, u := range d.Unmapped {
		if u == "" || !shortText(u, maxDescribeText) {
			return fail("an unmapped operation name is empty or not short printable text")
		}
	}
	return nil
}

// Supports reports whether the adapter can perform op for the capability.
func (d Description) Supports(capabilityID string, op Operation) bool {
	for _, c := range d.Capabilities {
		if c.CapabilityID == capabilityID {
			return slices.Contains(c.Operations, op)
		}
	}
	return false
}

// ParseDescription decodes the host's description document for a transport.
//
// Unknown members are ignored: a host built against a newer minor revision of
// the contract may describe more than this build understands, and that must not
// make it undescribable. Everything this build does read is validated strictly,
// and the transport name is the adapter's, whatever the document says.
func ParseDescription(transport string, raw []byte) (Description, error) {
	var d Description
	if err := decodeLoose(raw, &d); err != nil {
		return Description{}, schemaInvalid("description is not a JSON object of the expected shape")
	}
	d.Transport = transport
	if err := d.Validate(); err != nil {
		return Description{}, err
	}
	return d, nil
}

// decodeLoose decodes exactly one JSON document, ignoring unknown members.
func decodeLoose(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(v); err != nil {
		return err
	}
	return noTrailingData(dec)
}

// decodeStrict decodes exactly one JSON document and refuses unknown members.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return noTrailingData(dec)
}

// noTrailingData reports an error unless the decoder is at the end of its
// input (whitespace aside).
func noTrailingData(dec *json.Decoder) error {
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing data after the JSON document")
	}
	return nil
}

// Negotiated is the outcome of the handshake with an adapter.
type Negotiated struct {
	// Version is the contract version both sides will use.
	Version int
	// Description is what the adapter reported.
	Description Description
}

// SelectVersion returns the highest contract version present in both lists, or
// a validation_failed error naming both lists when there is none. A host that
// only speaks a higher major version than the core is refused, never guessed at.
func SelectVersion(core, host []int) (int, error) {
	best := 0
	for _, v := range host {
		if v > best && slices.Contains(core, v) {
			best = v
		}
	}
	if best == 0 {
		return 0, contracts.New(contracts.ErrValidationFailed, "adapter and core share no contract version").
			WithDetails("reason", "version_incompatible").
			WithDetails("core_versions", slices.Clone(core)).
			WithDetails("adapter_versions", slices.Clone(host))
	}
	return best, nil
}

// Negotiate asks the adapter to describe itself, validates the description and
// chooses the contract version. Call it when an application is activated and
// whenever its package or adapter changes; a failure means the adapter must not
// be used.
func Negotiate(ctx context.Context, a AppAdapter) (Negotiated, error) {
	d, err := a.Describe(ctx)
	if err != nil {
		return Negotiated{}, err
	}
	if err := d.Validate(); err != nil {
		return Negotiated{}, err
	}
	v, err := SelectVersion(SupportedContractVersions, d.ContractVersions)
	if err != nil {
		return Negotiated{}, err
	}
	return Negotiated{Version: v, Description: d}, nil
}

// Health is the operational state of an application, as the adapter sees it.
type Health string

const (
	HealthOK          Health = "ok"
	HealthDegraded    Health = "degraded"
	HealthUnavailable Health = "unavailable"
)

// Valid reports whether h is one of the three states.
func (h Health) Valid() bool {
	return h == HealthOK || h == HealthDegraded || h == HealthUnavailable
}

// HealthReport is what Health returns.
type HealthReport struct {
	Status Health `json:"status"`
	// Reason is a short, non-sensitive explanation when Status is not ok.
	Reason string `json:"reason,omitempty"`
	// ApplicationVersion is the application's version label, if it reports one.
	ApplicationVersion string `json:"application_version,omitempty"`
}

const maxHealthReason = 200

// Validate checks a HealthReport.
func (h HealthReport) Validate() error {
	switch {
	case !h.Status.Valid():
		return schemaInvalid("health: status is not ok, degraded or unavailable")
	case !shortText(h.Reason, maxHealthReason):
		return schemaInvalid("health: reason is not short printable text")
	case !shortText(h.ApplicationVersion, maxDescribeText):
		return schemaInvalid("health: application_version is not short printable text")
	case h.Status != HealthOK && strings.TrimSpace(h.Reason) == "":
		return schemaInvalid("health: a report that is not ok must say why")
	}
	return nil
}

// ParseHealth decodes the host's health document. Like ParseDescription it
// ignores unknown members and validates the ones it reads.
func ParseHealth(raw []byte) (HealthReport, error) {
	var h HealthReport
	if err := decodeLoose(raw, &h); err != nil {
		return HealthReport{}, schemaInvalid("health report is not a JSON object of the expected shape")
	}
	if err := h.Validate(); err != nil {
		return HealthReport{}, err
	}
	return h, nil
}
