// Package fake is an in-memory host application and an in-memory adapter for
// tests (the counterpart of internal/providers/fake). It has two parts:
//
//   - App is the host: a small deterministic application with two capabilities
//     (a read, and a reversible create with prepare, commit, status and cancel),
//     idempotent commits, tenant-scoped records, a record of every request it
//     received (the identity context the host saw) and scriptable misbehaviour.
//     It speaks raw JSON, like a real host, so a transport fixture can put it
//     behind any wire protocol: decode the request, call App.Handle, send
//     Reply.Body, and act on Reply.Action.
//   - Adapter is an adapters.AppAdapter over an App with no wire at all. It
//     behaves like a real transport where it matters (the identity it sends,
//     the size-limited read, Normalise, the error classification), so the shared
//     conformance suite (tests/conformance/adapters) exercises the same code
//     paths real transports use.
//
// # The application
//
// ReadCapability ("items.get") reads one item by item_id. ActionCapability
// ("items.create") creates an item named name (1 to 64 characters):
//
//	validate   checks the name, changes nothing
//	prepare    returns the exact effect and the version of the collection it saw
//	commit     applies the effect once per idempotency key, answers a repeat with
//	           the original reply, refuses a stale preparation and a key reused
//	           for a different action
//	status     reports what was recorded for a key, or rejected when nothing was
//	cancel     rejected: an applied effect is not reversed here
//
// Items belong to a tenant and workspace; a read in another tenant does not see
// them. A host refusal is a result with status rejected and typed errors, as
// the contract says.
//
// # Misbehaviour
//
// Inject arms a one-shot Fault for the next request of an operation: garbled,
// partial, prose or oversize replies, a reply for the wrong capability or key,
// a hung host, a reply or request lost on the wire, a denial at the host's
// reauthorisation, a failed dependency, a newer Describe. Faults that only
// damage the reply apply after the operation ran, so a damaged Commit reply
// leaves the effect applied: the case recovery has to handle.
package fake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// The capabilities and entity types of the fake application.
const (
	// ReadCapability reads one item.
	ReadCapability = "items.get"
	// ActionCapability creates one item.
	ActionCapability = "items.create"
	// ItemEntity is the entity type of an item.
	ItemEntity = "item"
	// CollectionEntity is the entity type of the version snapshot a
	// preparation records.
	CollectionEntity = "item_collection"
)

// Fault is one injectable misbehaviour. A fault is consumed by the next
// request of the operation it was injected for.
type Fault string

const (
	// FaultMalformed truncates the reply in the middle of the JSON document.
	FaultMalformed Fault = "malformed"
	// FaultPartial answers with a JSON object that lacks the required members.
	FaultPartial Fault = "partial"
	// FaultTextOnly answers with prose instead of JSON.
	FaultTextOnly Fault = "text_only"
	// FaultGeneric answers with a JSON object that is only prose and a status.
	FaultGeneric Fault = "generic"
	// FaultWrongCapability makes the reply name another capability.
	FaultWrongCapability Fault = "wrong_capability"
	// FaultWrongKey makes the reply echo another idempotency key.
	FaultWrongKey Fault = "wrong_key"
	// FaultOversize pads the reply beyond any sane size limit.
	FaultOversize Fault = "oversize"
	// FaultHang makes the host never answer; the request is not processed.
	FaultHang Fault = "hang"
	// FaultDropReply processes the request, then loses the reply on the wire.
	FaultDropReply Fault = "drop_reply"
	// FaultDropRequest loses the request on the wire: it is not processed.
	FaultDropRequest Fault = "drop_request"
	// FaultDeny makes the host's reauthorisation refuse the request: a rejected
	// result with permission_denied, nothing executed.
	FaultDeny Fault = "deny"
	// FaultDependencyDown makes the host report that a dependency is down: a
	// result with status unavailable, nothing executed.
	FaultDependencyDown Fault = "dependency_down"
	// FaultNewerDescribe makes Describe carry members and versions from a newer
	// contract revision.
	FaultNewerDescribe Fault = "newer_describe"
)

// AllFaults lists every fault.
func AllFaults() []Fault {
	return []Fault{
		FaultMalformed, FaultPartial, FaultTextOnly, FaultGeneric, FaultWrongCapability, FaultWrongKey,
		FaultOversize, FaultHang, FaultDropReply, FaultDropRequest, FaultDeny, FaultDependencyDown, FaultNewerDescribe,
	}
}

// Action is what a wire layer must do with a Reply.
type Action int

const (
	// Deliver sends Reply.Body back.
	Deliver Action = iota
	// Hang sends nothing and holds the request open until the client gives up.
	Hang
	// Drop closes the connection without a reply.
	Drop
	// Refuse behaves as if nothing were listening (connection refused).
	Refuse
)

// Request is what a transport carries to the host: the identity context, the
// operation and its parameters.
type Request struct {
	Operation      adapters.Operation
	Identity       adapters.Identity
	CapabilityID   string
	Fields         map[string]any
	Prepared       *actions.PreparedAction
	IdempotencyKey string
	Aspects        []adapters.Aspect
}

// Reply is what the host sends back. Body is the JSON the host's wire carries:
// for the six result operations, the host's members of a DeterministicResult
// (no operation or capability_id, which belong to the core); for the others the
// description, health or inspection document.
type Reply struct {
	Action Action
	Body   []byte
	// EchoedKey is the idempotency key the reply carries when the protocol
	// echoes one ("" otherwise).
	EchoedKey string
}

// Call is one request the host received.
type Call struct {
	Seq            int
	Operation      adapters.Operation
	Identity       adapters.Identity
	CapabilityID   string
	IdempotencyKey string
}

// AppOption configures NewApp.
type AppOption func(*App)

// WithClock sets the clock behind freshness timestamps.
func WithClock(c clock.Clock) AppOption { return func(a *App) { a.clk = c } }

// WithOversizeBytes sets how large a FaultOversize reply is. It must exceed the
// response limit of the adapter under test.
func WithOversizeBytes(n int) AppOption { return func(a *App) { a.oversize = n } }

// WithNonIdempotentCommit makes a broken host: a commit applies its effect
// every time it is received, whatever the key, and never reports a stale
// preparation. It exists so tests of the safety checks have a host that fails
// them.
func WithNonIdempotentCommit() AppOption { return func(a *App) { a.idempotent = false } }

// WithApplicationVersion sets the version label the host reports.
func WithApplicationVersion(v string) AppOption { return func(a *App) { a.appVersion = v } }

// WithUnmapped sets the host operations that no capability maps, which Describe
// reports for the audit's control-surface check.
func WithUnmapped(names ...string) AppOption {
	return func(a *App) { a.unmapped = append([]string(nil), names...) }
}

type scopeKey struct{ tenant, workspace string }

func (k scopeKey) String() string { return k.tenant + ":" + k.workspace }

type collection struct {
	version int
	items   map[string]string
}

type commitKey struct {
	scope scopeKey
	key   string
}

type commitRecord struct {
	fingerprint string
	body        []byte
}

// App is the fake host application. It is safe for concurrent use.
type App struct {
	mu         sync.Mutex
	clk        clock.Clock
	oversize   int
	idempotent bool
	appVersion string
	unmapped   []string
	versions   []int
	available  bool
	faults     map[adapters.Operation][]Fault
	calls      []Call
	effects    int
	nextID     int
	colls      map[scopeKey]*collection
	commits    map[commitKey]commitRecord
}

// NewApp returns a host with no records, speaking contract version 1.
func NewApp(opts ...AppOption) *App {
	a := &App{
		clk:        clock.NewFake(time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)),
		oversize:   8 << 20,
		idempotent: true,
		appVersion: "1.0.0",
		versions:   []int{adapters.ContractVersion},
		available:  true,
		faults:     map[adapters.Operation][]Fault{},
		colls:      map[scopeKey]*collection{},
		commits:    map[commitKey]commitRecord{},
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Inject arms a one-shot fault for the next request of op. Faults for one
// operation are consumed in the order they were injected.
func (a *App) Inject(op adapters.Operation, f Fault) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.faults[op] = append(a.faults[op], f)
}

// ClearFaults drops every fault that has not fired.
func (a *App) ClearFaults() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.faults = map[adapters.Operation][]Fault{}
}

// SetAvailable brings the host down (every request is refused before it is
// received) or back up.
func (a *App) SetAvailable(up bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.available = up
}

// SetContractVersions sets the contract versions the host reports speaking.
func (a *App) SetContractVersions(v ...int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.versions = append([]int(nil), v...)
}

// OversizeBytes is the size of a FaultOversize reply.
func (a *App) OversizeBytes() int { return a.oversize }

// Calls returns every request the host received, oldest first. A request the
// host never received (it was down, or the adapter refused it first) is absent.
func (a *App) Calls() []Call {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Call(nil), a.calls...)
}

// Effects is how many times the host applied the create effect.
func (a *App) Effects() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.effects
}

// Items returns the item names of one tenant and workspace by item ID.
func (a *App) Items(tenant, workspace string) map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]string{}
	if c := a.colls[scopeKey{tenant, workspace}]; c != nil {
		for k, v := range c.items {
			out[k] = v
		}
	}
	return out
}

// Seed adds an item to a tenant and workspace without a commit.
func (a *App) Seed(tenant, workspace, itemID, name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.collection(scopeKey{tenant, workspace})
	c.items[itemID] = name
	c.version++
}

// ApplyOutside records an effect under an idempotency key the way the host's
// own authenticated UI would after a level-3 step-up (ADR 0048). Status for
// that key then reports the effect; AlixAI's Commit path is never used.
func (a *App) ApplyOutside(tenant, workspace, key, name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sk := scopeKey{tenant, workspace}
	c := a.collection(sk)
	a.nextID++
	id := fmt.Sprintf("item_%d", a.nextID)
	c.items[id] = name
	c.version++
	a.effects++
	b := a.ok(c)
	b["identifiers"] = []any{record(ItemEntity, id)}
	b["values"] = []any{fact("name", "text", name)}
	b["allowed_next_actions"] = []any{map[string]any{"action": "invoke_capability", "capability_id": ReadCapability, "label": "View the item"}}
	a.commits[commitKey{sk, key}] = commitRecord{fingerprint: name, body: marshal(b)}
}

func (a *App) collection(k scopeKey) *collection {
	c := a.colls[k]
	if c == nil {
		c = &collection{items: map[string]string{}}
		a.colls[k] = c
	}
	return c
}

func (a *App) nextFault(op adapters.Operation) Fault {
	q := a.faults[op]
	if len(q) == 0 {
		return ""
	}
	a.faults[op] = q[1:]
	return q[0]
}

// Handle processes one request and returns what the wire should do. It never
// blocks: a hung host is Reply{Action: Hang} and the wire layer holds the
// connection.
func (a *App) Handle(_ context.Context, req Request) Reply {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.available {
		return Reply{Action: Refuse}
	}
	a.calls = append(a.calls, Call{
		Seq: len(a.calls) + 1, Operation: req.Operation, Identity: req.Identity,
		CapabilityID: req.CapabilityID, IdempotencyKey: req.IdempotencyKey,
	})
	fault := a.nextFault(req.Operation)
	switch fault {
	case FaultHang:
		return Reply{Action: Hang}
	case FaultDropRequest:
		return Reply{Action: Drop}
	}

	var body map[string]any
	switch fault {
	case FaultDeny:
		body = rejected("permission_denied", "", "the application does not allow this request")
	case FaultDependencyDown:
		body = dependencyDown()
	default:
		body = a.execute(req, fault)
	}
	raw := marshal(body)
	reply := Reply{Action: Deliver, Body: raw}
	if _, isResult := req.Operation.Result(); isResult && req.IdempotencyKey != "" {
		reply.EchoedKey = req.IdempotencyKey
	}

	switch fault {
	case FaultDropReply:
		return Reply{Action: Drop}
	case FaultMalformed:
		reply.Body = raw[:len(raw)/2]
	case FaultPartial:
		reply.Body = []byte(`{"status":"ok"}`)
		if _, isResult := req.Operation.Result(); !isResult {
			reply.Body = []byte(`{}`)
		}
	case FaultTextOnly:
		reply.Body = []byte("Done! Everything went fine and your request was completed.")
	case FaultGeneric:
		reply.Body = []byte(`{"status":"ok","message":"Done! Everything went fine."}`)
	case FaultWrongCapability:
		body["capability_id"] = "items.other"
		reply.Body = marshal(body)
	case FaultWrongKey:
		reply.EchoedKey = "idem_wrong_wrong_wrong"
	case FaultOversize:
		pad := a.oversize - len(raw)
		if pad < 0 {
			pad = 0
		}
		reply.Body = append(append([]byte(nil), raw...), bytes.Repeat([]byte(" "), pad)...)
	}
	return reply
}

// execute runs one operation against the application state.
func (a *App) execute(req Request, fault Fault) map[string]any {
	switch req.Operation {
	case adapters.OpDescribe:
		return a.describe(fault)
	case adapters.OpHealth:
		return map[string]any{"status": "ok", "application_version": a.appVersion}
	case adapters.OpInspect:
		return a.inspect(req)
	}
	sk := scopeKey{req.Identity.TenantID, req.Identity.WorkspaceID}
	c := a.collection(sk)
	switch {
	case req.CapabilityID == ReadCapability && req.Operation == adapters.OpRead:
		return a.read(sk, c, req)
	case req.CapabilityID == ActionCapability:
		switch req.Operation {
		case adapters.OpValidate:
			return a.validate(c, req)
		case adapters.OpPrepare:
			return a.prepare(sk, c, req)
		case adapters.OpCommit:
			return a.commit(sk, c, req)
		case adapters.OpStatus:
			return a.status(sk, req)
		case adapters.OpCancel:
			return a.cancel(sk, req)
		}
	}
	return rejected("unknown_capability", "", "the application does not offer this operation for this capability")
}

func (a *App) describe(fault Fault) map[string]any {
	versions := append([]int(nil), a.versions...)
	d := map[string]any{
		"contract_versions":   versions,
		"application_version": a.appVersion,
		"capabilities": []any{
			map[string]any{"capability_id": ReadCapability, "operations": []any{"read"}},
			map[string]any{"capability_id": ActionCapability, "operations": []any{"validate", "prepare", "commit", "status", "cancel"}},
		},
	}
	if len(a.unmapped) > 0 {
		d["unmapped"] = append([]string(nil), a.unmapped...)
	}
	if fault == FaultNewerDescribe {
		d["contract_versions"] = append(versions, adapters.ContractVersion+1)
		d["x_future_member"] = map[string]any{"nested": []any{1, 2, 3}}
		d["capabilities"] = []any{
			map[string]any{"capability_id": ReadCapability, "operations": []any{"read"}, "x_cost_hint": 3},
			map[string]any{"capability_id": ActionCapability, "operations": []any{"validate", "prepare", "commit", "status", "cancel"}},
		}
	}
	return d
}

func (a *App) inspect(req Request) map[string]any {
	wanted := adapters.InspectRequest{Aspects: req.Aspects}
	reports := []any{}
	for _, asp := range adapters.AllAspects() {
		if len(req.Aspects) > 0 && !contains(wanted.Aspects, asp) {
			continue
		}
		switch asp {
		case adapters.AspectVersions:
			reports = append(reports, map[string]any{"aspect": asp, "state": "reported", "facts": []any{
				fact("application_version", "text", a.appVersion),
				fact("contract_version", "integer", adapters.ContractVersion),
			}})
		case adapters.AspectHealth:
			reports = append(reports, map[string]any{"aspect": asp, "state": "reported", "facts": []any{
				fact("status", "text", "ok"),
			}})
		case adapters.AspectLimits:
			reports = append(reports, map[string]any{"aspect": asp, "state": "reported", "facts": []any{
				fact("max_name_length", "integer", 64),
			}})
		default:
			reports = append(reports, map[string]any{"aspect": asp, "state": "unsupported", "facts": []any{}})
		}
	}
	return map[string]any{"reports": reports}
}

func contains(s []adapters.Aspect, a adapters.Aspect) bool {
	for _, x := range s {
		if x == a {
			return true
		}
	}
	return false
}

func (a *App) read(sk scopeKey, c *collection, req Request) map[string]any {
	id, ok := req.Fields["item_id"].(string)
	if !ok || id == "" {
		return rejected("missing_fields", "item_id", "item_id is required")
	}
	name, ok := c.items[id]
	if !ok {
		return rejected("validation_failed", "item_id", "no such item")
	}
	b := a.ok(c)
	b["identifiers"] = []any{record(ItemEntity, id)}
	b["values"] = []any{fact("name", "text", name)}
	b["allowed_next_actions"] = []any{map[string]any{"action": "invoke_capability", "capability_id": ActionCapability, "label": "Create another item"}}
	return b
}

func validName(fields map[string]any) (string, map[string]any) {
	v, present := fields["name"]
	if !present {
		return "", rejected("missing_fields", "name", "name is required")
	}
	name, ok := v.(string)
	if !ok || name == "" || utf8.RuneCountInString(name) > 64 {
		return "", rejected("validation_failed", "name", "name must be 1 to 64 characters")
	}
	return name, nil
}

func (a *App) validate(c *collection, req Request) map[string]any {
	name, bad := validName(req.Fields)
	if bad != nil {
		return bad
	}
	b := a.ok(c)
	b["values"] = []any{fact("name", "text", name)}
	return b
}

func (a *App) prepare(sk scopeKey, c *collection, req Request) map[string]any {
	name, bad := validName(req.Fields)
	if bad != nil {
		return bad
	}
	b := a.ok(c)
	b["values"] = []any{fact("name", "text", name)}
	b["allowed_next_actions"] = []any{map[string]any{"action": "confirm"}, map[string]any{"action": "cancel"}}
	b["preparation"] = map[string]any{
		"summary": fmt.Sprintf("Create an item named %q", name),
		"effects": []any{map[string]any{
			"operation":   "create",
			"entity_type": ItemEntity,
			"description": fmt.Sprintf("Create an item named %q.", name),
			"changes":     []any{fact("name", "text", name)},
			"external":    false,
		}},
		"data_versions":       []any{map[string]any{"entity_type": CollectionEntity, "entity_id": sk.String(), "version": fmt.Sprintf("v%d", c.version)}},
		"declared_risk_level": 1,
		"valid_for_seconds":   300,
	}
	return b
}

// preparedName extracts the item name and the collection version from the
// prepared action the core sent.
func preparedName(p *actions.PreparedAction) (name, version string, ok bool) {
	if p == nil {
		return "", "", false
	}
	for _, e := range p.Effects {
		if e.Operation != actions.EffectOperationCreate || string(e.EntityType) != ItemEntity {
			continue
		}
		for _, f := range e.Changes {
			if f.Field != "name" {
				continue
			}
			if tv, isText := f.Value.Value.(actions.TextValue); isText {
				name = tv.Value
			}
		}
	}
	for _, dv := range p.DataVersions {
		if string(dv.EntityType) == CollectionEntity {
			version = string(dv.Version)
		}
	}
	return name, version, name != "" && version != ""
}

func (a *App) commit(sk scopeKey, c *collection, req Request) map[string]any {
	name, version, ok := preparedName(req.Prepared)
	if !ok {
		return rejected("validation_failed", "", "the prepared action is incomplete")
	}
	ck := commitKey{sk, req.IdempotencyKey}
	if a.idempotent {
		if rec, seen := a.commits[ck]; seen {
			if rec.fingerprint != name {
				return rejected("conflict", "idempotency_key", "the key was used for a different action")
			}
			return unmarshal(rec.body) // the original reply, byte for byte
		}
		if version != fmt.Sprintf("v%d", c.version) {
			return rejected("stale", "", "the data changed since the action was prepared")
		}
	}
	a.nextID++
	id := fmt.Sprintf("item_%d", a.nextID)
	c.items[id] = name
	c.version++
	a.effects++

	b := a.ok(c)
	b["identifiers"] = []any{record(ItemEntity, id)}
	b["values"] = []any{fact("name", "text", name)}
	b["allowed_next_actions"] = []any{map[string]any{"action": "invoke_capability", "capability_id": ReadCapability, "label": "View the item"}}
	a.commits[ck] = commitRecord{fingerprint: name, body: marshal(b)}
	return b
}

func (a *App) status(sk scopeKey, req Request) map[string]any {
	rec, ok := a.commits[commitKey{sk, req.IdempotencyKey}]
	if !ok {
		return rejected("validation_failed", "idempotency_key", "the application has no record of this key")
	}
	return unmarshal(rec.body)
}

func (a *App) cancel(sk scopeKey, req Request) map[string]any {
	if _, ok := a.commits[commitKey{sk, req.IdempotencyKey}]; ok {
		return rejected("conflict", "", "the action was already applied; cancel does not reverse it")
	}
	return rejected("validation_failed", "idempotency_key", "the application has nothing pending for this key")
}

// ok is the common start of a successful result.
func (a *App) ok(c *collection) map[string]any {
	return map[string]any{
		"status":               "ok",
		"identifiers":          []any{},
		"values":               []any{},
		"errors":               []any{},
		"allowed_next_actions": []any{},
		"freshness": map[string]any{
			"as_of":   a.clk.Now().UTC().Format("2006-01-02T15:04:05Z"),
			"version": fmt.Sprintf("v%d", c.version),
		},
	}
}

func rejected(code, field, message string) map[string]any {
	issue := map[string]any{"code": code, "message": message}
	if field != "" {
		issue["field"] = field
	}
	return map[string]any{
		"status":               "rejected",
		"identifiers":          []any{},
		"values":               []any{},
		"errors":               []any{issue},
		"allowed_next_actions": []any{},
	}
}

func dependencyDown() map[string]any {
	return map[string]any{
		"status":               "unavailable",
		"identifiers":          []any{},
		"values":               []any{},
		"errors":               []any{map[string]any{"code": "unavailable", "message": "a service the application depends on is not reachable"}},
		"allowed_next_actions": []any{map[string]any{"action": "retry"}},
	}
}

func fact(field, kind string, value any) map[string]any {
	return map[string]any{"field": field, "value": map[string]any{"kind": kind, "value": value}}
}

func record(entityType, id string) map[string]any {
	return map[string]any{"entity_type": entityType, "entity_id": id}
}

func marshal(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		panic("fake: cannot encode a reply: " + err.Error())
	}
	return b
}

func unmarshal(b []byte) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		panic("fake: cannot decode a stored reply: " + err.Error())
	}
	return m
}
