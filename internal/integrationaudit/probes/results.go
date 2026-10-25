package probes

import (
	"encoding/json"
	"strings"

	"github.com/buxee/alix-ai/internal/adapters"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// fingerprint renders a result as canonical JSON without freshness.as_of, the one
// member that legitimately differs between two answers about unchanged data. Two
// stable answers have the same fingerprint.
func fingerprint(res act.DeterministicResult) string {
	b, err := json.Marshal(res)
	if err != nil {
		return "unencodable"
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return "unencodable"
	}
	if f, ok := m["freshness"].(map[string]any); ok {
		delete(f, "as_of")
	}
	if fu, ok := m["follow_up"].(map[string]any); ok {
		delete(fu, "as_of")
	}
	out, err := json.Marshal(m) // encoding/json sorts map keys
	if err != nil {
		return "unencodable"
	}
	return string(out)
}

// resultText renders a whole result as one string, for scanning it for an
// identifier that must not be in it.
func resultText(res act.DeterministicResult) string {
	b, err := json.Marshal(res)
	if err != nil {
		return ""
	}
	return string(b)
}

// mentions returns the identifiers among ids that the result contains.
func mentions(res act.DeterministicResult, ids []string) []string {
	text := resultText(res)
	var out []string
	for _, id := range ids {
		if id != "" && strings.Contains(text, id) {
			out = append(out, id)
		}
	}
	return out
}

// firstCode returns the first error code of a result, or "none".
func firstCode(res act.DeterministicResult) string {
	if len(res.Errors) == 0 {
		return "none"
	}
	return string(res.Errors[0].Code)
}

// ok reports whether the host said it did what was asked.
func ok(res act.DeterministicResult) bool { return res.Status == act.DeterministicResultStatusOk }

// recorded reports whether the host recorded the outcome of a commit: ok, queued or
// partial.
func recorded(res act.DeterministicResult) bool {
	switch res.Status {
	case act.DeterministicResultStatusOk, act.DeterministicResultStatusQueued, act.DeterministicResultStatusPartial:
		return true
	}
	return false
}

// refused reports whether the host refused, with no effect: rejected.
func refused(res act.DeterministicResult) bool {
	return res.Status == act.DeterministicResultStatusRejected
}

// version returns freshness.version, or "" when the result carries none.
func version(res act.DeterministicResult) string {
	if res.Freshness == nil {
		return ""
	}
	return string(res.Freshness.Version)
}

// noRecord reports whether a Status result is the no-record marker.
func noRecord(res act.DeterministicResult) bool { return adapters.StatusShowsNoRecord(res) }

// disclosed reports whether a refused result carries anything about a record:
// identifiers or values.
func disclosed(res act.DeterministicResult) bool {
	return len(res.Identifiers) > 0 || len(res.Values) > 0
}

// covers reports whether a prepared action's effects may produce the record.
func covers(effects []act.Effect, ref act.RecordRef) bool {
	for _, e := range effects {
		if e.EntityType != ref.EntityType {
			continue
		}
		if e.EntityID == nil || *e.EntityID == ref.EntityID {
			return true
		}
	}
	return false
}
