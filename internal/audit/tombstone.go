package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Tombstone format, ID prefix and the fixed payload of an erased event.
const (
	TombstoneFormat   = "alixai.audit-tombstone/v1"
	TombstoneIDPrefix = "atmb"

	// ErasedMarker is the data column of an erased event. It is constant on
	// purpose: nothing about the erased payload (not its size, not a digest)
	// survives in the row. The tombstone, not this value, says an event is
	// erased.
	ErasedMarker = `{"erased":true}`

	// DefaultTombstoneValidity is how long a tombstone signature is valid:
	// a hundred years. A tombstone cannot be re-signed (it is part of an
	// immutable row), so its expiry is far beyond any retention period;
	// revocation of the key, not expiry, is the control. See ADR-0036.
	DefaultTombstoneValidity = 100 * 365 * 24 * time.Hour

	// OperatorWorkspace is the workspace the record of an erasure is
	// appended in on a tenant chain: a tenant exporting its own workspaces
	// does not receive the operator's erasure records, an operator listing the
	// chain sees them.
	OperatorWorkspace = "operator"

	// MaxEraseBatch bounds the events one erasure transaction tombstones.
	MaxEraseBatch = 10000

	// MaxReasonBytes bounds the reason an erasure states.
	MaxReasonBytes = 200
)

// ErrErasureBlocked is wrapped by the error EraseTx returns when a guard
// refused the request: errors.Is(err, ErrErasureBlocked). Nothing was written.
var ErrErasureBlocked = errors.New("audit: erasure refused because something still depends on the events")

// TombstoneBody is the document a tombstone signature covers
// (trust.CanonicalJSON of it). It states which event was erased (its full
// envelope and its hash), why, by whom, when, and which audit.erased event of
// the chain records the erasure. ErasedAt and ExpiresAt repeat the signature
// record's signed_at and expires_at inside the signed bytes, as a checkpoint
// does.
type TombstoneBody struct {
	Format        string `json:"format"`
	ID            string `json:"id"`
	ApplicationID string `json:"application_id"`
	TenantID      string `json:"tenant_id"`
	WorkspaceID   string `json:"workspace_id"`
	EventID       string `json:"event_id"`
	ChainSeq      int64  `json:"chain_seq"`
	EventType     string `json:"event_type"`
	OccurredAtMs  int64  `json:"occurred_at_ms"`
	PrincipalID   string `json:"principal_id"`
	SessionID     string `json:"session_id"`
	PrevHash      string `json:"prev_hash"`
	// EventHash is the hash the erased event had, and still has in the chain.
	EventHash string `json:"event_hash"`
	Reason    string `json:"reason"`
	// Operator is who erased it: a principal id of an operator or the id of a
	// service identity (the retention job).
	Operator string `json:"operator"`
	// RecordedBy is the id of the audit.erased event that records the erasure.
	RecordedBy string `json:"recorded_by"`
	ErasedAt   string `json:"erased_at"`
	ExpiresAt  string `json:"expires_at"`
}

// Chain returns the chain the erased event belongs to.
func (b TombstoneBody) Chain() ChainKey {
	return ChainKey{ApplicationID: b.ApplicationID, TenantID: b.TenantID}
}

// Tombstone is a signed tombstone. Its JSON form is self-contained: an export
// line of an erased event carries it, so a recipient can verify it with the
// platform public key and without the database.
type Tombstone struct {
	Tombstone TombstoneBody   `json:"tombstone"`
	Signature trust.Signature `json:"signature"`
}

// Erased reports whether the event's payload was replaced by a tombstone. It
// is true for an event read by Export that carries one.
func (e Event) Erased() bool { return e.Tombstone != nil }

// Guard is asked, once per erasure and inside the erasing transaction, which
// events of a chain must keep their payload. It returns the function that
// decides for one event: a non-empty string is the reason the event is still
// depended on (it never contains event data). The audit package knows nothing
// of what depends on an event; the package that owns the dependency (actions:
// a pending or uncertain preparation) provides the guard.
type Guard func(ctx context.Context, q store.Reader, chain ChainKey) (keep func(Event) string, err error)

// EraseRequest asks to replace the payloads of events of one chain by signed
// tombstones.
type EraseRequest struct {
	Chain ChainKey
	// EventIDs are the events to erase, 1 to MaxEraseBatch, all on Chain.
	EventIDs []string
	// Kind says what asked: "tenant", "principal" or "retention". It goes into
	// the audit.erased record; the subject (a principal, a tenant) does not.
	Kind string
	// Reason is why, at most MaxReasonBytes of printable text with no
	// personal data. It is signed into every tombstone.
	Reason string
	// Operator is who erases: an operator's principal id or a service identity.
	Operator string
	// Guard decides which events must keep their payload. It is required.
	Guard Guard
	// SkipBlocked erases the events the guard allows and leaves the others
	// (retention). Without it one blocked event refuses the whole request.
	SkipBlocked bool
}

// EraseResult says what EraseTx did.
type EraseResult struct {
	// Erased is how many events lost their payload; AlreadyErased how many
	// asked for already had a tombstone; Blocked how many a guard kept.
	Erased, AlreadyErased, Blocked int
	// RecordID is the audit.erased event that records the erasure, empty when
	// nothing was erased.
	RecordID string
	// FirstSeq and LastSeq are the chain positions of the first and last event
	// erased.
	FirstSeq, LastSeq int64
}

// EraseRecord is the data of an audit.erased event: what an auditor can learn
// about an erasure from the chain. It names no subject.
type EraseRecord struct {
	Kind     string `json:"kind"`
	Reason   string `json:"reason"`
	Operator string `json:"operator"`
	Erased   int    `json:"erased"`
	Blocked  int    `json:"blocked"`
	FirstSeq int64  `json:"first_seq"`
	LastSeq  int64  `json:"last_seq"`
}

// EraseKinds are the values of EraseRequest.Kind.
var EraseKinds = []string{"tenant", "principal", "retention", "workspace"}

func (r EraseRequest) validate() error {
	if err := r.Chain.validate(); err != nil {
		return err
	}
	if r.Chain.ApplicationID == trust.PlatformApplicationID {
		return contracts.New(contracts.ErrValidationFailed, "audit: the platform chain is not erased")
	}
	if r.Guard == nil {
		return contracts.New(contracts.ErrValidationFailed, "audit: an erasure needs a guard")
	}
	if n := len(r.EventIDs); n < 1 || n > MaxEraseBatch {
		return contracts.Newf(contracts.ErrValidationFailed, "audit: an erasure names 1 to %d events", MaxEraseBatch)
	}
	known := false
	for _, k := range EraseKinds {
		known = known || k == r.Kind
	}
	if !known {
		return contracts.New(contracts.ErrValidationFailed, "audit: the erasure kind is not tenant, principal, retention or workspace")
	}
	if err := checkReason(r.Reason); err != nil {
		return err
	}
	return checkID(contracts.LevelPrincipal, r.Operator)
}

func checkReason(s string) error {
	bad := strings.TrimSpace(s) == "" || len(s) > MaxReasonBytes || !utf8.ValidString(s)
	for _, r := range s {
		bad = bad || unicode.IsControl(r)
	}
	if bad {
		return contracts.Newf(contracts.ErrValidationFailed, "audit: the reason is required, printable and at most %d bytes", MaxReasonBytes)
	}
	return nil
}

// EraseTx replaces the payload of each requested event by a signed tombstone,
// inside tx, the caller's write transaction. It is the only way an audit
// payload is ever removed. In order, and before anything is written:
//
//   - the Log must hold the platform signing key (no key, no erasure: a
//     payload is never removed without its tombstone);
//   - every event must exist on the chain, must not be an audit.erased record
//     and must still hash to what is stored (an event that does not is
//     refused, never tombstoned: signing it would launder the tampering);
//   - the guard is asked about every event that is not already erased.
//
// Then one audit.erased event is appended to the chain and every event is
// tombstoned, all in tx: either every payload goes with its tombstone and
// the record, or none does. The hashes, prev_hash links and positions are not
// touched, so every checkpoint still matches the chain.
func (l *Log) EraseTx(ctx context.Context, tx *store.Tx, req EraseRequest) (EraseResult, error) {
	var res EraseResult
	if tx == nil {
		return res, contracts.New(contracts.ErrInternal, "audit: EraseTx needs a transaction")
	}
	if l.signing == nil {
		return res, contracts.New(contracts.ErrUnavailable, "audit: erasure needs the platform signing key: a payload is never erased without its signed tombstone")
	}
	if err := req.validate(); err != nil {
		return res, err
	}
	keep, err := req.Guard(ctx, tx, req.Chain)
	if err != nil {
		return res, err
	}
	if keep == nil {
		return res, contracts.New(contracts.ErrInternal, "audit: the guard returned no decision function")
	}

	seen := make(map[string]bool, len(req.EventIDs))
	ids := make([]string, 0, len(req.EventIDs))
	for _, id := range req.EventIDs {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	found, err := loadErasable(ctx, tx, req.Chain, ids)
	if err != nil {
		return res, err
	}
	var targets []Event
	var blocked []string
	for _, id := range ids {
		x, ok := found[id]
		if !ok {
			return res, contracts.Newf(contracts.ErrValidationFailed, "audit: event %s is not on chain %s", clipName(id), req.Chain)
		}
		e := x.event
		if e.Type == EventAuditErased {
			return res, contracts.New(contracts.ErrValidationFailed, "audit: the record of an erasure is itself never erased")
		}
		if x.erased {
			res.AlreadyErased++
			continue
		}
		if herr := e.HashValid(); herr != nil {
			return res, contracts.Wrap(contracts.ErrInternal, ErrChainBroken,
				"audit: event "+id+" no longer matches its hash: the chain must be investigated before anything is erased")
		}
		if why := keep(e); why != "" {
			blocked = append(blocked, id)
			res.Blocked++
			continue
		}
		targets = append(targets, e)
	}
	if len(blocked) > 0 && !req.SkipBlocked {
		shown := blocked
		if len(shown) > 10 {
			shown = shown[:10]
		}
		return EraseResult{}, contracts.Wrap(contracts.ErrConflict, ErrErasureBlocked,
			"audit: erasure refused: events are still depended on by a pending or uncertain action; nothing was erased").
			WithDetails("blocked", len(blocked)).WithDetails("event_ids", shown)
	}
	if len(targets) == 0 {
		return res, nil
	}
	res.Erased = len(targets)
	res.FirstSeq, res.LastSeq = targets[0].ChainSeq, targets[0].ChainSeq
	for _, e := range targets {
		res.FirstSeq, res.LastSeq = min(res.FirstSeq, e.ChainSeq), max(res.LastSeq, e.ChainSeq)
	}

	loc, err := erasureLocator(req.Chain, req.Operator)
	if err != nil {
		return EraseResult{}, err
	}
	rec, err := l.AppendTx(ctx, tx, loc, NewEvent{Type: EventAuditErased, Data: EraseRecord{
		Kind: req.Kind, Reason: req.Reason, Operator: req.Operator,
		Erased: res.Erased, Blocked: res.Blocked, FirstSeq: res.FirstSeq, LastSeq: res.LastSeq,
	}})
	if err != nil {
		return EraseResult{}, err
	}
	res.RecordID = rec.ID

	now := l.clk.Now().UTC().Truncate(time.Second)
	for _, e := range targets {
		if err := l.writeTombstone(ctx, tx, e, req, rec.ID, now); err != nil {
			return EraseResult{}, err
		}
	}
	return res, nil
}

// Erase is EraseTx in its own transaction.
func (l *Log) Erase(ctx context.Context, req EraseRequest) (EraseResult, error) {
	var res EraseResult
	err := l.db.Tx(ctx, func(tx *store.Tx) error {
		var err error
		res, err = l.EraseTx(ctx, tx, req)
		return err
	})
	if err != nil {
		return EraseResult{}, wrapDB(err)
	}
	return res, nil
}

// erasureLocator places the record of an erasure: on a tenant chain in the
// operator workspace, attributed to the operator; on an application chain
// attributed to the operator too.
func erasureLocator(chain ChainKey, operator string) (Locator, error) {
	if chain.IsApplication() {
		return ForApplication(chain.ApplicationID, operator)
	}
	return ForIDs(chain.ApplicationID, chain.TenantID, OperatorWorkspace, operator, "")
}

func (l *Log) writeTombstone(ctx context.Context, tx *store.Tx, e Event, req EraseRequest, recordedBy string, now time.Time) error {
	expires := now.Add(DefaultTombstoneValidity)
	body := TombstoneBody{
		Format:        TombstoneFormat,
		ID:            l.ids.New(TombstoneIDPrefix),
		ApplicationID: e.ApplicationID,
		TenantID:      e.TenantID,
		WorkspaceID:   e.WorkspaceID,
		EventID:       e.ID,
		ChainSeq:      e.ChainSeq,
		EventType:     string(e.Type),
		OccurredAtMs:  e.OccurredAt.UnixMilli(),
		PrincipalID:   e.PrincipalID,
		SessionID:     e.SessionID,
		PrevHash:      e.PrevHash,
		EventHash:     e.Hash,
		Reason:        req.Reason,
		Operator:      req.Operator,
		RecordedBy:    recordedBy,
		ErasedAt:      checkpointTime(now),
		ExpiresAt:     checkpointTime(expires),
	}
	sig, err := trust.SignPayload(body, l.signing.KeyID, l.signing.Key, now, expires)
	if err != nil {
		return err
	}
	if sig.SignedAt != body.ErasedAt || sig.ExpiresAt != body.ExpiresAt {
		return contracts.New(contracts.ErrInternal, "audit: the signature record disagrees with the tombstone body about time")
	}
	canon, err := trust.CanonicalJSON(body)
	if err != nil {
		return err
	}
	sigJSON, err := json.Marshal(sig)
	if err != nil {
		return contracts.New(contracts.ErrInternal, "audit: signature does not marshal")
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_tombstones (id, application_id, tenant_id, workspace_id, event_id, chain_seq, body, signature, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		body.ID, e.ApplicationID, e.TenantID, e.WorkspaceID, e.ID, e.ChainSeq, string(canon), string(sigJSON), l.clk.Now().UnixMilli()); err != nil {
		return wrapDB(err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE audit_events SET data = ? WHERE application_id = ? AND tenant_id = ? AND id = ?`,
		ErasedMarker, e.ApplicationID, e.TenantID, e.ID)
	if err != nil {
		return wrapDB(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return contracts.New(contracts.ErrInternal, "audit: the erased event was not found when its payload was replaced")
	}
	return nil
}

// eraseReadChunk bounds the ids one read of loadErasable binds.
const eraseReadChunk = 500

// erasable is an event of a chain and whether it already has a tombstone.
type erasable struct {
	event  Event
	erased bool
}

// loadErasable reads the events of a chain that ids name, by id, with whether each already has a
// tombstone: one query per eraseReadChunk ids instead of two per event. An id that is not on the
// chain is not in the map.
func loadErasable(ctx context.Context, q store.Reader, chain ChainKey, ids []string) (map[string]erasable, error) {
	out := make(map[string]erasable, len(ids))
	for len(ids) > 0 {
		n := min(len(ids), eraseReadChunk)
		args := []any{chain.ApplicationID, chain.TenantID}
		for _, id := range ids[:n] {
			args = append(args, id)
		}
		ids = ids[n:]
		rows, err := q.QueryContext(ctx,
			`SELECT id, workspace_id, chain_seq, event_type, occurred_at, principal_id, session_id, data, prev_hash, hash,
			        EXISTS (SELECT 1 FROM audit_tombstones t WHERE t.event_id = audit_events.id)
			   FROM audit_events WHERE application_id = ? AND tenant_id = ? AND id IN (?`+strings.Repeat(", ?", n-1)+`)`,
			args...)
		if err != nil {
			return nil, wrapDB(err)
		}
		for rows.Next() {
			var (
				x    erasable
				e    = &x.event
				ms   int64
				typ  string
				data string
			)
			if err := rows.Scan(&e.ID, &e.WorkspaceID, &e.ChainSeq, &typ, &ms, &e.PrincipalID, &e.SessionID, &data, &e.PrevHash, &e.Hash, &x.erased); err != nil {
				_ = rows.Close()
				return nil, wrapDB(err)
			}
			e.ApplicationID, e.TenantID = chain.ApplicationID, chain.TenantID
			e.Type, e.OccurredAt, e.Data = EventType(typ), time.UnixMilli(ms).UTC(), json.RawMessage(data)
			out[e.ID] = x
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, wrapDB(err)
		}
	}
	return out, nil
}

// EventRef names an event of a chain.
type EventRef struct {
	ID       string
	ChainSeq int64
}

// Selector picks events of one chain to erase. Exactly one of All, Principal
// and Before must be set.
type Selector struct {
	// All selects every event: the erasure of a tenant.
	All bool
	// Principal selects the events whose envelope principal_id is this id, and
	// those whose payload holds it as a JSON string value.
	Principal string
	// Before selects the events that occurred before this instant: retention.
	Before time.Time
	// Workspace, when set with Principal, further narrows to that workspace
	// (principal×workspace erasure). Empty means every workspace of the tenant.
	Workspace string
	// AfterSeq is a cursor: only chain positions above it.
	AfterSeq int64
	// Limit bounds the answer, 1 to MaxEraseBatch.
	Limit int
}

// SelectEvents lists, oldest first, the events of chain that the selector
// picks and that still have a payload: events that already have a tombstone
// and the audit.erased records are never selected.
func SelectEvents(ctx context.Context, q store.Reader, chain ChainKey, sel Selector) ([]EventRef, error) {
	if err := chain.validate(); err != nil {
		return nil, err
	}
	n := 0
	for _, set := range []bool{sel.All, sel.Principal != "", !sel.Before.IsZero()} {
		if set {
			n++
		}
	}
	if n != 1 {
		return nil, contracts.New(contracts.ErrValidationFailed, "audit: a selector sets exactly one of All, Principal and Before")
	}
	if sel.Limit < 1 || sel.Limit > MaxEraseBatch {
		return nil, contracts.Newf(contracts.ErrValidationFailed, "audit: a selector's limit is 1 to %d", MaxEraseBatch)
	}
	query := `SELECT id, chain_seq FROM audit_events
	           WHERE application_id = ? AND tenant_id = ? AND chain_seq > ? AND event_type <> ?
	             AND NOT EXISTS (SELECT 1 FROM audit_tombstones t WHERE t.event_id = audit_events.id)`
	args := []any{chain.ApplicationID, chain.TenantID, sel.AfterSeq, string(EventAuditErased)}
	switch {
	case sel.Principal != "":
		if err := checkID(contracts.LevelPrincipal, sel.Principal); err != nil {
			return nil, err
		}
		query += ` AND (principal_id = ? OR instr(data, ?) > 0)`
		args = append(args, sel.Principal, `"`+sel.Principal+`"`)
		if sel.Workspace != "" {
			if err := checkID(contracts.LevelWorkspace, sel.Workspace); err != nil {
				return nil, err
			}
			query += ` AND workspace_id = ?`
			args = append(args, sel.Workspace)
		}
	case !sel.Before.IsZero():
		query += ` AND occurred_at < ?`
		args = append(args, sel.Before.UnixMilli())
	}
	query += ` ORDER BY chain_seq LIMIT ?`
	args = append(args, sel.Limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrapDB(err)
	}
	defer rows.Close()
	var out []EventRef
	for rows.Next() {
		var r EventRef
		if err := rows.Scan(&r.ID, &r.ChainSeq); err != nil {
			return nil, wrapDB(err)
		}
		out = append(out, r)
	}
	return out, wrapDB(rows.Err())
}

// VerifyTombstone checks a tombstone on its own: its format, that it names
// key, that its signed timestamps agree with the signature record, and that
// the signature verifies under a platform key of the trust store registered
// for trust.PurposeAuditCheckpoint and has not expired. It says nothing about whether the event it names carries it.
//
// The error is unauthenticated for a signature that does not verify or a key
// that is unknown or revoked, expired when expiry is the only reason,
// scope_violation for a tombstone of another chain, and unavailable when the
// Log has no verifier.
func (l *Log) VerifyTombstone(ctx context.Context, t Tombstone, key ChainKey) error {
	if l.verifier == nil {
		return contracts.New(contracts.ErrUnavailable, "audit: no trust verifier is configured, tombstones cannot be verified")
	}
	b := t.Tombstone
	switch {
	case b.Format != TombstoneFormat:
		return contracts.New(contracts.ErrSchemaInvalid, "audit: unknown tombstone format")
	case b.Chain() != key:
		return contracts.New(contracts.ErrScopeViolation, "audit: the tombstone is for another chain")
	case b.ChainSeq < 1 || !ValidHash(b.EventHash) || !ValidHash(b.PrevHash):
		return contracts.New(contracts.ErrSchemaInvalid, "audit: the tombstone's event reference is malformed")
	case b.ErasedAt != t.Signature.SignedAt || b.ExpiresAt != t.Signature.ExpiresAt:
		return contracts.New(contracts.ErrUnauthenticated, "audit: the tombstone's signed timestamps disagree with its signature record")
	}
	if _, err := l.verifier.VerifyPayload(ctx, b, []trust.Signature{t.Signature},
		trust.PayloadOptions{ApplicationID: trust.PlatformApplicationID, Purpose: trust.PurposeAuditCheckpoint}); err != nil {
		return err
	}
	return nil
}

// tombstoneTableExists is false for a database that predates the tombstone
// migration (a backup, a copy opened with no migrations): such a database
// holds no tombstone, so none is expected.
func tombstoneTableExists(ctx context.Context, q store.Reader) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'audit_tombstones'`).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// loadTombstones reads the tombstones of a chain, by event id. A row that does
// not parse is returned as a nil entry: the event is reported, not skipped.
func loadTombstones(ctx context.Context, q store.Reader, key ChainKey) (map[string]*Tombstone, error) {
	out := map[string]*Tombstone{}
	ok, err := tombstoneTableExists(ctx, q)
	if err != nil || !ok {
		return out, err
	}
	rows, err := q.QueryContext(ctx,
		`SELECT event_id, body, signature FROM audit_tombstones WHERE application_id = ? AND tenant_id = ?`,
		key.ApplicationID, key.TenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, body, sig string
		if err := rows.Scan(&id, &body, &sig); err != nil {
			return nil, err
		}
		var t Tombstone
		if decodeStrict([]byte(body), &t.Tombstone) != nil || decodeStrict([]byte(sig), &t.Signature) != nil {
			out[id] = nil
			continue
		}
		out[id] = &t
	}
	return out, rows.Err()
}

// loadTombstoneOf reads the tombstone of one event, for Export, which has
// checked that the table exists (tombstoneTableExists).
func loadTombstoneOf(ctx context.Context, q store.Reader, eventID string) (*Tombstone, error) {
	var body, sig string
	err := q.QueryRowContext(ctx, `SELECT body, signature FROM audit_tombstones WHERE event_id = ?`, eventID).Scan(&body, &sig)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t Tombstone
	if decodeStrict([]byte(body), &t.Tombstone) != nil || decodeStrict([]byte(sig), &t.Signature) != nil {
		return nil, contracts.New(contracts.ErrInternal, "audit: a stored tombstone does not parse")
	}
	return &t, nil
}

// CanSign reports whether the Log holds the platform signing key, which erasure
// needs: without it EraseTx refuses, because a payload is never erased without
// its signed tombstone.
func (l *Log) CanSign() bool { return l.signing != nil }
