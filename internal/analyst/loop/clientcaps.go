package loop

import (
	"sync"

	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

// clientCaps holds announcements for live turns so the emit layer can read them
// without depending on internal/stream. Entries are removed by [ForgetClientCapabilities].
var clientCaps sync.Map // turnID → an.ClientCapabilities

// RememberClientCapabilities stores the turn request's client_capabilities under
// turnID. An empty/zero value is still stored so absent vs empty can be distinguished
// by the caller; pass ok=false via Forget when the turn ends without an announcement.
func RememberClientCapabilities(turnID string, caps an.ClientCapabilities) {
	if turnID == "" {
		return
	}
	clientCaps.Store(turnID, caps)
}

// ClientCapabilitiesFor returns the announcement stored for turnID, if any.
// Documented accessor for the emit / AG-UI mapping sibling (ADR 0047).
func ClientCapabilitiesFor(turnID string) (an.ClientCapabilities, bool) {
	v, ok := clientCaps.Load(turnID)
	if !ok {
		return an.ClientCapabilities{}, false
	}
	c, _ := v.(an.ClientCapabilities)
	return c, true
}

// ForgetClientCapabilities drops the stored announcement when the turn completes.
func ForgetClientCapabilities(turnID string) {
	if turnID == "" {
		return
	}
	clientCaps.Delete(turnID)
}
