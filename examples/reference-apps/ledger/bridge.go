package ledger

import (
	"context"
	"log/slog"

	"github.com/buxee/alix-ai/pkg/bridge"
)

// EndpointID is the ID of the native endpoint in the application's package.
const EndpointID = "ledger-bridge"

// BridgeConfig configures NewBridge.
type BridgeConfig struct {
	// Keys are the application keys by key id (32 or more random bytes each),
	// shared with AlixAI out of band: the package's adapter configuration holds
	// only a reference to the key AlixAI uses.
	Keys map[string][]byte
	// PeerCheck optionally restricts the operating system account of the peer
	// (Linux Unix sockets); the key is required either way.
	PeerCheck func(bridge.PeerCredentials) error
	// Logger receives one line per refused peer or failed call; it never receives
	// a key, a proof or a request body.
	Logger *slog.Logger
}

type method struct {
	name string
	run  func(*Ledger, bridge.Call) Result
	// keyed operations echo the idempotency key so a reply for another request is
	// recognisable.
	keyed bool
}

// capabilityMethods are the native methods the package maps, by capability. They are
// the only operations the bridge declares besides the protocol's own (describe,
// health, inspect) and, in test support mode, the provisioning operations.
func capabilityMethods() []method {
	methods := []method{
		{"invoices_find", func(l *Ledger, c bridge.Call) Result { return l.findInvoices(c.Identity, c.Fields) }, false},
		{"resolve_customer", func(l *Ledger, c bridge.Call) Result {
			return l.resolve("customer", PermQuotesDraft, false, c.Identity, c.Fields)
		}, false},
		{"resolve_quote", func(l *Ledger, c bridge.Call) Result {
			return l.resolve("quote", PermInvoicesIssue, false, c.Identity, c.Fields)
		}, false},
		// An invoice is resolved for the capability that is about to use it: a payment
		// reaches the caller's team, a void (a manager's) the whole organisation.
		{"resolve_invoice", func(l *Ledger, c bridge.Call) Result {
			return l.resolve("invoice", PermPaymentsRecord, false, c.Identity, c.Fields)
		}, false},
		{"resolve_invoice_for_void", func(l *Ledger, c bridge.Call) Result {
			return l.resolve("invoice", PermInvoicesVoid, true, c.Identity, c.Fields)
		}, false},
	}
	for prefix, def := range map[string]*actionDef{
		"quotes_draft": quoteDraftDef, "invoices_issue": invoiceIssueDef,
		"payments_record": paymentRecordDef, "invoices_void": invoiceVoidDef,
	} {
		methods = append(methods,
			method{prefix + "_validate", func(l *Ledger, c bridge.Call) Result { return l.validateAction(def, c.Identity, c.Fields) }, false},
			method{prefix + "_prepare", func(l *Ledger, c bridge.Call) Result { return l.prepareAction(def, c.Identity, c.Fields) }, false},
			method{prefix + "_commit", func(l *Ledger, c bridge.Call) Result {
				return l.commitAction(def, c.Identity, c.IdempotencyKey, c.Prepared)
			}, true},
			method{prefix + "_status", func(l *Ledger, c bridge.Call) Result { return l.statusAction(def, c.Identity, c.IdempotencyKey) }, true},
		)
	}
	return methods
}

// Methods lists the capability methods the bridge declares, for the agreement test
// between the package and the application.
func Methods() []string {
	var out []string
	for _, m := range capabilityMethods() {
		out = append(out, m.name)
	}
	return out
}

// NewBridge builds the host bridge of the application: the native adapter's peer in
// the application's process. It serves exactly what the application declares, to a
// peer that proved it holds the application key. The bridge authenticates AlixAI, not
// the principal: every handler reauthorises the identity it receives.
func (l *Ledger) NewBridge(cfg BridgeConfig) (*bridge.Server, error) {
	srv, err := bridge.New(bridge.Config{
		ApplicationID: ApplicationID, Keys: cfg.Keys, EndpointID: EndpointID, ApplicationVersion: Version,
		PeerCheck: cfg.PeerCheck, Logger: cfg.Logger, Inspect: l.inspect,
		Health: func(context.Context) (bridge.Health, error) { return bridge.Health{Status: "ok"}, nil },
	})
	if err != nil {
		return nil, err
	}
	for _, m := range capabilityMethods() {
		if err := srv.Handle(m.name, func(_ context.Context, c bridge.Call) (bridge.Reply, error) {
			rep := bridge.Reply{Result: m.run(l, c)}
			if m.keyed {
				rep.IdempotencyKey = c.IdempotencyKey
			}
			return rep, nil
		}); err != nil {
			return nil, err
		}
	}
	if l.cfg.TestSupport {
		if err := l.registerManagement(srv); err != nil {
			return nil, err
		}
	}
	return srv, nil
}
