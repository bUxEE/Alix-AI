// Package ledger is a synthetic multi-tenant accounting application: the third
// neutral AlixAI reference application. It is a test fixture with real business
// logic, not a product, and every name and number in it is made up.
//
// It is unlike the other two on purpose. It is written in Go and embeds the host
// bridge of pkg/bridge, so AlixAI reaches it through the native adapter over local
// IPC (not MCP or OpenAPI); a tenant is an organisation and a workspace is one of
// its teams; money is an integer count of minor units with an ISO currency; one
// capability is level 3 and can only be finished in the application's own UI;
// and its contracts are served by a small document API that AlixAI's RAG source
// provider reads. Application events (an invoice was paid, an invoice was
// voided) are signed with the application's key and invalidate AlixAI state;
// they never trigger a commit.
//
// The package imports pkg/bridge, pkg/contracts and the standard library only: a
// host application never needs AlixAI's service code to be integrated.
package ledger

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/buxee/alix-ai/pkg/bridge"
)

// ApplicationID is the application's ID in its package and in every scope.
const ApplicationID = "ledger"

// Version is the application version reported by describe, health and inspect.
const Version = "1.0.0"

// Role is the one role a principal holds in the application's own directory.
type Role string

// The three roles.
const (
	RoleAccountant Role = "accountant"
	RoleManager    Role = "manager"
	RoleViewer     Role = "viewer"
)

// Permissions the host checks before it runs an operation.
const (
	PermInvoicesRead   = "invoices.read"
	PermQuotesDraft    = "quotes.draft"
	PermInvoicesIssue  = "invoices.issue"
	PermPaymentsRecord = "payments.record"
	PermInvoicesVoid   = "invoices.void"
)

// rolePermissions is the whole authorisation table: a viewer reads, an
// accountant also drafts, issues and records payments, and only a manager can
// void. A role not in the table holds nothing.
var rolePermissions = map[Role][]string{
	RoleViewer:     {PermInvoicesRead},
	RoleAccountant: {PermInvoicesRead, PermQuotesDraft, PermInvoicesIssue, PermPaymentsRecord},
	RoleManager:    {PermInvoicesRead, PermQuotesDraft, PermInvoicesIssue, PermPaymentsRecord, PermInvoicesVoid},
}

func (r Role) allows(permission string) bool {
	for _, p := range rolePermissions[r] {
		if p == permission {
			return true
		}
	}
	return false
}

// Invoice and quote states.
const (
	quoteDraft     = "draft"
	quoteInvoiced  = "invoiced"
	invoiceIssued  = "issued"
	invoicePartial = "partially_paid"
	invoicePaid    = "paid"
	invoiceVoid    = "void"
)

type team struct{ id, name string }

type principal struct {
	id, team, name string
	role           Role
}

type customer struct {
	id, team, name, currency string
	version                  int64
}

type quote struct {
	id, number, team, customerID, description string
	amount                                    Money
	validUntil, status                        string
	version                                   int64
}

type invoice struct {
	id, number, team, customerID, quoteID string
	total                                 Money
	paid                                  int64
	issuedOn, dueOn, status               string
	version                               int64
}

type payment struct {
	id, number, invoiceID, team, paidOn, method string
	amount                                      Money
}

type contract struct {
	id, key, team, title, body string
	version                    int64
	modified                   time.Time
}

// receipt is what a commit recorded for an idempotency key.
type receipt struct {
	capability, principal, fingerprint string
	result                             Result
	at                                 time.Time
}

type tenant struct {
	id, name   string
	disposable bool

	teams      map[string]*team
	principals map[string]*principal
	customers  map[string]*customer
	quotes     map[string]*quote
	invoices   map[string]*invoice
	payments   map[string]*payment
	contracts  map[string]*contract

	quoteSeq, invoiceSeq, paymentSeq int
	receipts                         map[string]*receipt
	// approvals are the step-up approvals the application's own authenticated UI
	// recorded: void reference -> approving principal.
	approvals map[string]string
}

func newTenant(id, name string, disposable bool) *tenant {
	return &tenant{
		id: id, name: name, disposable: disposable,
		teams: map[string]*team{}, principals: map[string]*principal{}, customers: map[string]*customer{},
		quotes: map[string]*quote{}, invoices: map[string]*invoice{}, payments: map[string]*payment{},
		contracts: map[string]*contract{}, receipts: map[string]*receipt{}, approvals: map[string]string{},
	}
}

// Config configures New.
type Config struct {
	// Clock supplies the time; nil means time.Now. Every date, expiry and
	// timestamp comes from it, so tests are deterministic.
	Clock func() time.Time
	// Events receives the application's signed events; nil keeps them in the
	// outbox only.
	Events EventSink
	// TestSupport enables the provisioning operations the integration audit uses
	// to create, seed and remove a disposable tenant. A production run leaves it
	// off and the bridge then declares none of them.
	TestSupport bool
	// DataDir is reported by inspect as the storage location; the ledger itself
	// keeps its data in memory.
	DataDir string
}

// Ledger is the application. All state is behind one mutex: the application is
// small and a single writer keeps every rule easy to see.
type Ledger struct {
	cfg Config

	mu      sync.Mutex
	tenants map[string]*tenant
	seq     int64
	outbox  []Event
	evtSeq  int64
	// flushMu serialises event deliveries so they leave in order.
	flushMu sync.Mutex
	runtime Runtime
}

// Runtime is what the running process knows about itself, for inspect.
type Runtime struct {
	// Listen is the bridge address; DocsListen the document API address.
	Listen, DocsListen string
	// KeyID is the application key id, never the key.
	KeyID string
}

// SetRuntime records the process facts inspect reports.
func (l *Ledger) SetRuntime(r Runtime) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runtime = r
}

// New builds an empty ledger.
func New(cfg Config) *Ledger {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &Ledger{cfg: cfg, tenants: map[string]*tenant{}}
}

func (l *Ledger) now() time.Time { return l.cfg.Clock().UTC() }

func (l *Ledger) today() string { return l.now().Format("2006-01-02") }

// nextID returns a process-wide unique entity ID, so an identifier never means a
// record of two tenants.
func (l *Ledger) nextID(prefix string) string {
	l.seq++
	return prefix + "_" + pad(l.seq, 4)
}

func pad(n int64, width int) string {
	s := strconv.FormatInt(n, 10)
	for len(s) < width {
		s = "0" + s
	}
	return s
}

// ---- the directory and access -----------------------------------------------

// caller is an identity the application accepted for one permission.
type caller struct {
	tenant    *tenant
	principal *principal
	identity  bridge.Identity
}

// resolveCaller reauthorises the identity AlixAI sent from the application's own
// directory, on every call. It trusts none of the asserted roles: the directory's
// role decides, and an assertion that disagrees with it is refused.
func (l *Ledger) resolveCaller(id bridge.Identity, permission string) (*caller, *refusal) {
	t := l.tenants[id.TenantID]
	if id.ApplicationID != ApplicationID || t == nil {
		return nil, refuse("scope_violation", "", "the request is outside the application's scope")
	}
	p := t.principals[id.PrincipalID]
	if p == nil {
		return nil, refuse("permission_denied", "", "the principal is not known to this organisation")
	}
	if p.team != id.WorkspaceID {
		return nil, refuse("scope_violation", "", "the request is outside the principal's team")
	}
	asserted := false
	for _, r := range id.Roles {
		if r == string(p.role) {
			asserted = true
		}
	}
	if !asserted {
		return nil, refuse("permission_denied", "", "the roles of the request do not match the directory")
	}
	if !p.role.allows(permission) {
		return nil, refuse("permission_denied", "", "the role may not use this capability")
	}
	return &caller{tenant: t, principal: p, identity: id}, nil
}

// canSeeTeam says whether the caller may reach records of a team. Records of the
// caller's own team are always reachable; a manager also reaches the other teams of
// the organisation when the capability's scope is the organisation (tenantWide).
func (c *caller) canSeeTeam(teamID string, tenantWide bool) bool {
	if teamID == c.principal.team {
		return true
	}
	if tenantWide && c.principal.role == RoleManager {
		_, ok := c.tenant.teams[teamID]
		return ok
	}
	return false
}

// ---- small helpers ----------------------------------------------------------

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Flush sends the events in the outbox that have not been delivered. A delivery
// that fails stays in the outbox for the next flush; a commit never fails
// because of one.
func (l *Ledger) Flush(ctx context.Context) error {
	return l.flushOutbox(ctx)
}
