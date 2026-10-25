package fake

import (
	"strings"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Call is what a Matcher sees: the verified scope and the chat request, as
// sent.
type Call struct {
	Scope   contracts.Scope
	Request providers.ChatRequest
	// Rich is the request as sent when the call came through
	// MultimodalGenerator (Request is then its text view); nil otherwise.
	Rich *providers.RichRequest
}

// Matcher decides whether a scripted rule applies to a call. Matchers run
// while the provider holds its lock: they must be pure and must not call back
// into the provider.
type Matcher func(Call) bool

// Any matches every call.
func Any() Matcher { return func(Call) bool { return true } }

// Contains matches when any message's content contains sub.
func Contains(sub string) Matcher {
	return func(c Call) bool {
		for _, m := range c.Request.Messages {
			if strings.Contains(m.Content, sub) {
				return true
			}
		}
		return false
	}
}

// LastUserContains matches when the last user message contains sub.
func LastUserContains(sub string) Matcher {
	return func(c Call) bool {
		for i := len(c.Request.Messages) - 1; i >= 0; i-- {
			if m := c.Request.Messages[i]; m.Role == providers.MsgUser {
				return strings.Contains(m.Content, sub)
			}
		}
		return false
	}
}

// SystemContains matches when any system message contains sub.
func SystemContains(sub string) Matcher {
	return func(c Call) bool {
		for _, m := range c.Request.Messages {
			if m.Role == providers.MsgSystem && strings.Contains(m.Content, sub) {
				return true
			}
		}
		return false
	}
}

// ForRole matches the model role the request addresses (controller by
// default).
func ForRole(role providers.Role) Matcher {
	return func(c Call) bool { return c.Request.EffectiveRole() == role }
}

// ForTenant matches calls made under the given tenant.
func ForTenant(tenantID string) Matcher {
	return func(c Call) bool { return c.Scope.TenantID == tenantID }
}

// Constrained matches requests carrying the given output constraint kind.
// Constrained(providers.ConstraintNone) matches unconstrained requests.
func Constrained(kind providers.ConstraintKind) Matcher {
	return func(c Call) bool { return c.Request.Output.Kind() == kind }
}

// All matches when every matcher matches (and for no matchers).
func All(ms ...Matcher) Matcher {
	return func(c Call) bool {
		for _, m := range ms {
			if !m(c) {
				return false
			}
		}
		return true
	}
}

// OneOf matches when at least one matcher matches.
func OneOf(ms ...Matcher) Matcher {
	return func(c Call) bool {
		for _, m := range ms {
			if m(c) {
				return true
			}
		}
		return false
	}
}

// Not inverts a matcher.
func Not(m Matcher) Matcher { return func(c Call) bool { return !m(c) } }

// HasPart matches a rich call that carries a part of the given kind.
func HasPart(kind providers.Modality) Matcher {
	return func(c Call) bool {
		if c.Rich == nil {
			return false
		}
		for _, m := range c.Rich.Messages {
			for _, p := range m.Parts {
				if p.Kind == kind {
					return true
				}
			}
		}
		return false
	}
}

// OffersTool matches a rich call whose allowlist offers the named tool.
func OffersTool(name string) Matcher {
	return func(c Call) bool {
		if c.Rich == nil {
			return false
		}
		for _, t := range c.Rich.Tools {
			if t.Name == name {
				return true
			}
		}
		return false
	}
}
