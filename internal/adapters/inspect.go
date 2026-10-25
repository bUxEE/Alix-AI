package adapters

import (
	"encoding/json"
	"slices"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// Aspect is one thing the runtime and system audit wants to know about an
// application (master plan 4.5, "Audit targets"). The application decides what
// it exposes; AlixAI never scans for the rest.
type Aspect string

const (
	AspectVersions     Aspect = "versions"     // service and component versions
	AspectModules      Aspect = "modules"      // enabled modules
	AspectDependencies Aspect = "dependencies" // declared dependencies
	AspectHealth       Aspect = "health"       // health and readiness
	AspectScopes       Aspect = "scopes"       // configured scopes
	AspectCredentials  Aspect = "credentials"  // credential references (never values)
	AspectLimits       Aspect = "limits"       // resource limits
	AspectStorage      Aspect = "storage"      // storage paths
	AspectNetwork      Aspect = "network"      // network permissions
)

// AllAspects lists every aspect in the canonical report order.
func AllAspects() []Aspect {
	return []Aspect{
		AspectVersions, AspectModules, AspectDependencies, AspectHealth, AspectScopes,
		AspectCredentials, AspectLimits, AspectStorage, AspectNetwork,
	}
}

// Valid reports whether a is one of the nine aspects.
func (a Aspect) Valid() bool { return slices.Contains(AllAspects(), a) }

// AspectState says whether the application answered for an aspect.
type AspectState string

const (
	// AspectReported: the application exposed the aspect; Facts holds what it
	// said.
	AspectReported AspectState = "reported"
	// AspectUnsupported: the application does not expose the aspect. It is
	// unverified and must never be shown as fully supported.
	AspectUnsupported AspectState = "unsupported"
)

// maxFactsPerAspect bounds one aspect's facts.
const maxFactsPerAspect = 256

// InspectRequest names the aspects wanted; empty means all of them.
type InspectRequest struct {
	Aspects []Aspect
}

// wanted returns the aspects the request asks for, in canonical order.
func (r InspectRequest) wanted() []Aspect {
	if len(r.Aspects) == 0 {
		return AllAspects()
	}
	var out []Aspect
	for _, a := range AllAspects() {
		if slices.Contains(r.Aspects, a) {
			out = append(out, a)
		}
	}
	return out
}

// Validate refuses unknown and duplicate aspects.
func (r InspectRequest) Validate() error {
	seen := map[Aspect]bool{}
	for _, a := range r.Aspects {
		if !a.Valid() || seen[a] {
			return contracts.New(contracts.ErrValidationFailed, "inspect: aspect is unknown or repeated")
		}
		seen[a] = true
	}
	return nil
}

// AspectReport is the answer for one aspect.
type AspectReport struct {
	Aspect Aspect         `json:"aspect"`
	State  AspectState    `json:"state"`
	Facts  []actions.Fact `json:"facts"`
}

// Inspection is what Inspect returns: exactly one report per requested aspect,
// in canonical order.
type Inspection struct {
	Reports []AspectReport `json:"reports"`
}

// Report returns the report for an aspect.
func (i Inspection) Report(a Aspect) (AspectReport, bool) {
	for _, r := range i.Reports {
		if r.Aspect == a {
			return r, true
		}
	}
	return AspectReport{}, false
}

// Unverified lists the aspects the application did not report. The integration
// audit records each as unverified.
func (i Inspection) Unverified() []Aspect {
	var out []Aspect
	for _, r := range i.Reports {
		if r.State != AspectReported {
			out = append(out, r.Aspect)
		}
	}
	return out
}

// NormaliseInspection turns a host's raw inspection reply into an Inspection.
// The reply must be one JSON object of at most maxBytes (DefaultMaxResponseBytes
// when zero) with no unknown members. It may report only aspects that were
// asked for, each at most once; an asked-for aspect it leaves out is recorded as
// unsupported, because silence is not evidence. Every fact must satisfy the
// shared fact schema, an unsupported aspect carries no facts and a reported one
// carries at least one. Anything else is schema_invalid.
func NormaliseInspection(raw []byte, req InspectRequest, maxBytes int) (Inspection, error) {
	if err := req.Validate(); err != nil {
		return Inspection{}, err
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxResponseBytes
	}
	if len(raw) > maxBytes {
		return Inspection{}, responseTooLarge(int64(maxBytes))
	}
	var got Inspection
	if err := decodeStrict(raw, &got); err != nil {
		return Inspection{}, schemaInvalid("inspection is not a JSON object of the expected shape")
	}
	if got.Reports == nil {
		return Inspection{}, schemaInvalid("inspection has no reports member")
	}
	wanted := req.wanted()
	byAspect := map[Aspect]AspectReport{}
	_, factSchema, err := compiledSchemas()
	if err != nil {
		return Inspection{}, contracts.Wrap(contracts.ErrInternal, err, "adapters: fact schema unavailable")
	}
	for _, r := range got.Reports {
		if !r.Aspect.Valid() || !slices.Contains(wanted, r.Aspect) {
			return Inspection{}, schemaInvalid("inspection reports an aspect that is unknown or was not asked for")
		}
		if _, dup := byAspect[r.Aspect]; dup {
			return Inspection{}, schemaInvalid("inspection reports an aspect twice")
		}
		switch r.State {
		case AspectReported:
			if len(r.Facts) == 0 || len(r.Facts) > maxFactsPerAspect {
				return Inspection{}, schemaInvalidf("inspection: a reported aspect needs between 1 and %d facts", maxFactsPerAspect)
			}
		case AspectUnsupported:
			if len(r.Facts) != 0 {
				return Inspection{}, schemaInvalid("inspection: an unsupported aspect carries facts")
			}
		default:
			return Inspection{}, schemaInvalid("inspection: an aspect state is not reported or unsupported")
		}
		for _, f := range r.Facts {
			inst, err := toInstance(f)
			if err != nil {
				return Inspection{}, schemaInvalid("inspection: a fact cannot be encoded")
			}
			if err := factSchema.Validate(inst); err != nil {
				return Inspection{}, schemaInvalid("inspection: a fact is not a Fact: " + violations(err))
			}
		}
		if r.Facts == nil {
			r.Facts = []actions.Fact{}
		}
		byAspect[r.Aspect] = r
	}
	out := Inspection{Reports: make([]AspectReport, 0, len(wanted))}
	for _, a := range wanted {
		r, ok := byAspect[a]
		if !ok {
			r = AspectReport{Aspect: a, State: AspectUnsupported, Facts: []actions.Fact{}}
		}
		out.Reports = append(out.Reports, r)
	}
	return out, nil
}

// CheckInspection re-validates an Inspection that is already a typed value
// against the request, with the same rules as NormaliseInspection.
func CheckInspection(i Inspection, req InspectRequest) (Inspection, error) {
	b, err := json.Marshal(i)
	if err != nil {
		return Inspection{}, schemaInvalid("inspection cannot be encoded")
	}
	return NormaliseInspection(b, req, 0)
}
