package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// The host members of a DeterministicResult (schemas/actions/deterministic-result).
// operation and capability_id belong to AlixAI, which knows what it called, so the
// ledger never states them. Facts, effects and data versions use the generated
// public types so the canonical value kinds (money included) are the contract's.

// Issue is one typed problem of a rejected result.
type Issue struct {
	Code    string `json:"code"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// Freshness says when and from which snapshot a result was read.
type Freshness struct {
	AsOf    string `json:"as_of"`
	Version string `json:"version"`
}

// NextAction is a follow-up the host allows.
type NextAction struct {
	Action       string `json:"action"`
	CapabilityID string `json:"capability_id,omitempty"`
	Label        string `json:"label,omitempty"`
}

// Preparation is the host's description of a staged action.
type Preparation struct {
	Summary           string                `json:"summary"`
	Effects           []actions.Effect      `json:"effects"`
	DataVersions      []actions.DataVersion `json:"data_versions"`
	DeclaredRiskLevel int                   `json:"declared_risk_level"`
	ValidForSeconds   int                   `json:"valid_for_seconds"`
}

// Result is the host's answer to one call.
type Result struct {
	Status             string              `json:"status"`
	Identifiers        []actions.RecordRef `json:"identifiers"`
	Values             []actions.Fact      `json:"values"`
	Errors             []Issue             `json:"errors"`
	Freshness          *Freshness          `json:"freshness,omitempty"`
	AllowedNextActions []NextAction        `json:"allowed_next_actions"`
	Preparation        *Preparation        `json:"preparation,omitempty"`
}

// MarshalJSON writes every array, empty ones as [] and never null.
func (r Result) MarshalJSON() ([]byte, error) {
	type plain Result
	p := plain(r)
	if p.Identifiers == nil {
		p.Identifiers = []actions.RecordRef{}
	}
	if p.Values == nil {
		p.Values = []actions.Fact{}
	}
	if p.Errors == nil {
		p.Errors = []Issue{}
	}
	if p.AllowedNextActions == nil {
		p.AllowedNextActions = []NextAction{}
	}
	return json.Marshal(p)
}

// ---- canonical values -------------------------------------------------------

func canon(field string, v actions.CanonicalValueValue) actions.Fact {
	return actions.Fact{Field: common.FieldName(field), Value: actions.CanonicalValue{Value: v}}
}

func textFact(field, v string) actions.Fact { return canon(field, actions.TextValue{Value: v}) }
func intFact(field string, v int64) actions.Fact {
	return canon(field, actions.IntegerValue{Value: v})
}
func boolFact(field string, v bool) actions.Fact { return canon(field, actions.BooleanValue{Value: v}) }
func dateFact(field, v string) actions.Fact      { return canon(field, actions.DateValue{Value: v}) }
func identFact(field, v string) actions.Fact {
	return canon(field, actions.IdentifierValue{Value: common.Identifier(v)})
}

// moneyFact is the only way an amount leaves the application: the exact decimal
// text of the integer minor units, and the ISO currency, as one canonical value.
func moneyFact(field string, m Money) actions.Fact {
	return canon(field, actions.MoneyValue{Value: m.Decimal(), Currency: m.Currency})
}

func ref(entityType, id string) actions.RecordRef {
	return actions.RecordRef{EntityType: common.Identifier(entityType), EntityID: common.Identifier(id)}
}

func dataVersion(entityType, id string, version int64) actions.DataVersion {
	return actions.DataVersion{
		EntityType: common.Identifier(entityType), EntityID: common.Identifier(id),
		Version: actions.OpaqueVersion(itoa(version)),
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// snapshotVersion is the version a result reports in freshness.version: a digest
// over the version of every record the result relied on, in a fixed order.
// Validate and Prepare compute it with this one function over the same records,
// so on unchanged data they agree, and a change to any of those records (and to
// nothing else) changes it. It is not a response tag and not a tenant counter.
func snapshotVersion(versions []actions.DataVersion) string {
	lines := make([]string, 0, len(versions))
	for _, v := range versions {
		lines = append(lines, string(v.EntityType)+":"+string(v.EntityID)+":"+string(v.Version))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "rv1-" + hex.EncodeToString(sum[:])[:32]
}

func freshnessOf(now time.Time, versions []actions.DataVersion) *Freshness {
	return &Freshness{AsOf: now.UTC().Format(time.RFC3339), Version: snapshotVersion(versions)}
}

// ---- refusals ---------------------------------------------------------------

// refusal is a typed reason an operation did not run. It becomes a result with
// status rejected, never a bridge error: an error means "the reply was lost",
// and after a commit that is resolved through status.
type refusal struct {
	code    string
	field   string
	message string
	fresh   *Freshness
}

func (r *refusal) Error() string { return r.code + ": " + r.message }

func refuse(code, field, message string) *refusal {
	return &refusal{code: code, field: field, message: message}
}

func (r *refusal) result() Result {
	return Result{
		Status:    "rejected",
		Errors:    []Issue{{Code: r.code, Field: r.field, Message: r.message}},
		Freshness: r.fresh,
	}
}

func okResult(now time.Time, versions []actions.DataVersion) Result {
	return Result{Status: "ok", Freshness: freshnessOf(now, versions)}
}
