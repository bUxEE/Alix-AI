package audit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// ErrChainBroken is wrapped by the error VerifyChain returns when it finds
// anything wrong: errors.Is(err, ErrChainBroken). The VerifyReport next to it
// says what and where. Any other error means the check could not be made.
var ErrChainBroken = errors.New("audit: chain failed verification")

// ProblemKind classifies one finding of VerifyChain.
type ProblemKind string

// Problem kinds.
const (
	// ProblemHashMismatch: an event's stored hash is not the hash of its
	// stored content: it was modified (or its position, workspace, type, time
	// or data was).
	ProblemHashMismatch ProblemKind = "hash_mismatch"
	// ProblemLinkBroken: an event's prev_hash is not the hash of the event
	// before it (or, for the first, the genesis hash of the chain): an event
	// was removed, inserted, reordered or spliced in from another chain.
	ProblemLinkBroken ProblemKind = "link_broken"
	// ProblemGap: positions are not contiguous from 1.
	ProblemGap ProblemKind = "gap"
	// ProblemOrder: rows were not inserted in chain order.
	ProblemOrder ProblemKind = "order"
	// ProblemMalformed: a stored value is not well-formed (id, hash, data).
	ProblemMalformed ProblemKind = "malformed"
	// ProblemTruncated: the chain is shorter than a signed checkpoint says it
	// was: the newest events were removed (SEC-086).
	ProblemTruncated ProblemKind = "truncated"
	// ProblemRewritten: the chain is long enough but its hash at a
	// checkpoint's position differs from the signed one: events before that
	// point were rewritten, with the hashes recomputed (SEC-086).
	ProblemRewritten ProblemKind = "rewritten"
	// ProblemCheckpointInvalid: a checkpoint does not verify (signature, key,
	// chain, format, or the row disagrees with the signed body).
	ProblemCheckpointInvalid ProblemKind = "checkpoint_invalid"
	// ProblemCheckpointExpired: the checkpoint's signature has expired.
	ProblemCheckpointExpired ProblemKind = "checkpoint_expired"
	// ProblemCheckpointUnverifiable: checkpoints exist but the Log has no
	// verifier, so they cannot be believed.
	ProblemCheckpointUnverifiable ProblemKind = "checkpoint_unverifiable"
	// ProblemUnanchored: VerifyOptions.RequireAnchor was set and no verified
	// checkpoint matched the chain.
	ProblemUnanchored ProblemKind = "unanchored"

	// ProblemTombstoneMismatch: an erased event's row differs from what its
	// signed tombstone states (its envelope, its hash or its erased marker), or
	// the row carries the erased marker without a tombstone's backing.
	ProblemTombstoneMismatch ProblemKind = "tombstone_mismatch"
	// ProblemTombstoneInvalid: a tombstone does not verify (signature, key,
	// chain, format) or does not parse.
	ProblemTombstoneInvalid ProblemKind = "tombstone_invalid"
	// ProblemTombstoneExpired: the tombstone's signature has expired.
	ProblemTombstoneExpired ProblemKind = "tombstone_expired"
	// ProblemTombstoneUnverifiable: erased events exist but the Log has no
	// verifier, so their tombstones cannot be believed.
	ProblemTombstoneUnverifiable ProblemKind = "tombstone_unverifiable"
	// ProblemTombstoneUnrecorded: the audit.erased event a tombstone names as
	// its record is not on the chain.
	ProblemTombstoneUnrecorded ProblemKind = "tombstone_unrecorded"
)

// maxProblems bounds a report; a chain with a broken tail does not produce a
// million findings.
const maxProblems = 64

// Problem is one finding. ChainSeq is the position it concerns (0 when it is
// about the chain as a whole). Detail never contains event data.
type Problem struct {
	Kind     ProblemKind `json:"kind"`
	ChainSeq int64       `json:"chain_seq,omitempty"`
	Detail   string      `json:"detail"`
}

// VerifyOptions tunes VerifyChain.
type VerifyOptions struct {
	// Expected is a checkpoint held outside the database (ExportCheckpoint at
	// an earlier time, or taken from a report or backup manifest). The chain
	// must extend it: be at least as long and carry the same hash at its
	// position. This is the defence against someone who can delete checkpoint
	// rows as well as events.
	Expected *Checkpoint
	// RequireAnchor makes a chain with events but no verified, matching
	// checkpoint a problem. Without it such a chain is verified as far as an
	// unkeyed hash chain can be, and the report says Anchored is false.
	RequireAnchor bool
}

// VerifyReport is the outcome of VerifyChain.
type VerifyReport struct {
	Chain ChainKey `json:"chain"`
	// Events is the number of rows found; HeadSeq and HeadHash are those of
	// the newest one.
	Events   int64  `json:"events"`
	HeadSeq  int64  `json:"head_seq"`
	HeadHash string `json:"head_hash"`
	// Erased is how many of the events had their payload replaced by a signed
	// tombstone. Their hashes are checked through the tombstone's signature, not
	// recomputed (ADR-0036).
	Erased int64 `json:"erased"`
	// Anchored is true when a verified checkpoint matched the chain; AnchorSeq
	// is the newest position that did, and EventsAfterAnchor how many events
	// lie beyond it, unprotected by any checkpoint.
	Anchored          bool  `json:"anchored"`
	AnchorSeq         int64 `json:"anchor_seq"`
	EventsAfterAnchor int64 `json:"events_after_anchor"`
	// Problems lists what was found, in the order found; MoreProblems is set
	// when the list was cut at its limit.
	Problems     []Problem `json:"problems"`
	MoreProblems bool      `json:"more_problems,omitempty"`
}

// OK reports whether no problem was found.
func (r VerifyReport) OK() bool { return len(r.Problems) == 0 && !r.MoreProblems }

func (r *VerifyReport) add(kind ProblemKind, seq int64, format string, args ...any) {
	r.addProblem(Problem{Kind: kind, ChainSeq: seq, Detail: fmt.Sprintf(format, args...)})
}

func (r *VerifyReport) addProblem(p Problem) {
	if len(r.Problems) >= maxProblems {
		r.MoreProblems = true
		return
	}
	r.Problems = append(r.Problems, p)
}

// anchor is a checkpoint that VerifyChain will hold the chain to.
type anchor struct {
	label string
	cp    Checkpoint
}

// VerifyChain checks one chain: every event's hash against its stored
// content, that positions are contiguous from 1, that every prev_hash is the
// previous hash (the first the genesis hash of the chain), that rows were
// inserted in chain order, and that the chain matches the newest stored
// checkpoint and, when given, VerifyOptions.Expected (see the package comment
// for what a checkpoint does and does not cover).
//
// It reads one consistent snapshot and holds no write lock. It always returns
// the report; the error is nil only when the report has no problem. A broken
// chain is an error wrapping ErrChainBroken (code internal: fail closed),
// anything else means the check could not be made.
func (l *Log) VerifyChain(ctx context.Context, key ChainKey, opts VerifyOptions) (VerifyReport, error) {
	if err := key.validate(); err != nil {
		return VerifyReport{Chain: key}, err
	}
	rep := VerifyReport{Chain: key}

	// Checkpoints are loaded and their signatures checked before the snapshot
	// is taken: the trust store has its own reads, and a verification must
	// never wait on a pooled connection while holding another.
	anchors, err := l.loadAnchors(ctx, key, opts, &rep)
	if err != nil {
		return rep, err
	}

	wanted := make([]int64, 0, len(anchors))
	for _, a := range anchors {
		wanted = append(wanted, a.cp.Checkpoint.HeadSeq)
	}
	var (
		walked VerifyReport
		hashAt map[int64]string
		erased erasedEvents
	)
	err = l.db.View(ctx, func(tx *store.Tx) error {
		// The view may be retried: every attempt starts from nothing.
		walked = VerifyReport{Chain: key}
		hashAt = make(map[int64]string, len(wanted))
		for _, s := range wanted {
			hashAt[s] = ""
		}
		erased = erasedEvents{recorders: map[string]bool{}}
		return walkChain(ctx, tx, key, &walked, hashAt, &erased)
	})
	if err != nil {
		return rep, wrapDB(err)
	}
	rep.Events, rep.HeadSeq, rep.HeadHash, rep.Erased = walked.Events, walked.HeadSeq, walked.HeadHash, walked.Erased
	for _, p := range walked.Problems {
		rep.addProblem(p)
	}
	rep.MoreProblems = rep.MoreProblems || walked.MoreProblems
	// Signatures are checked after the snapshot is closed, like the checkpoints'.
	l.verifyTombstones(ctx, key, erased, &rep)

	for _, a := range anchors {
		b := a.cp.Checkpoint
		switch {
		case b.HeadSeq > rep.HeadSeq:
			rep.add(ProblemTruncated, b.HeadSeq,
				"the chain ends at position %d but the %s checkpoint signed at %s covers position %d: the newest events were removed",
				rep.HeadSeq, a.label, b.CreatedAt, b.HeadSeq)
		case hashAt[b.HeadSeq] == "":
			rep.add(ProblemRewritten, b.HeadSeq, "no event at position %d, which the %s checkpoint covers", b.HeadSeq, a.label)
		case hashAt[b.HeadSeq] != b.HeadHash:
			rep.add(ProblemRewritten, b.HeadSeq,
				"the hash at position %d differs from the one the %s checkpoint signed at %s: the chain was rewritten",
				b.HeadSeq, a.label, b.CreatedAt)
		default:
			rep.Anchored = true
			rep.AnchorSeq = max(rep.AnchorSeq, b.HeadSeq)
		}
	}
	if rep.Anchored {
		rep.EventsAfterAnchor = rep.HeadSeq - rep.AnchorSeq
	}
	if opts.RequireAnchor && !rep.Anchored && rep.Events > 0 {
		rep.add(ProblemUnanchored, 0, "no verified checkpoint matches the chain")
	}

	if !rep.OK() {
		return rep, brokenError(rep)
	}
	return rep, nil
}

// loadAnchors returns the checkpoints the chain is held to: the newest stored
// one and the expected one, each only if it verifies. A checkpoint that does
// not verify is a problem in rep, not an anchor.
func (l *Log) loadAnchors(ctx context.Context, key ChainKey, opts VerifyOptions, rep *VerifyReport) ([]anchor, error) {
	var (
		anchors []anchor
		stored  storedCheckpoint
		has     bool
	)
	err := l.db.View(ctx, func(tx *store.Tx) error {
		var err error
		stored, has, err = latestCheckpoint(ctx, tx, key)
		return err
	})
	if err != nil {
		var ce *contracts.Error
		if !errors.As(err, &ce) {
			return nil, wrapDB(err)
		}
		// A stored checkpoint that does not even parse is itself tampering.
		rep.add(ProblemCheckpointInvalid, 0, "the newest stored checkpoint is unreadable: %s", ce.Message)
		has = false
	}
	try := func(label string, cp Checkpoint) {
		if verr := l.VerifyCheckpoint(ctx, cp, key); verr != nil {
			kind := ProblemCheckpointInvalid
			switch contracts.CodeOf(verr) {
			case contracts.ErrExpired:
				kind = ProblemCheckpointExpired
			case contracts.ErrUnavailable:
				kind = ProblemCheckpointUnverifiable
			}
			rep.add(kind, cp.Checkpoint.HeadSeq, "the %s checkpoint does not verify: %v", label, verr)
			return
		}
		anchors = append(anchors, anchor{label: label, cp: cp})
	}
	if has {
		if stored.column != stored.cp.Checkpoint.HeadSeq {
			rep.add(ProblemCheckpointInvalid, stored.column, "the stored checkpoint row disagrees with its signed body about the head position")
		} else {
			try("stored", stored.cp)
		}
	}
	if opts.Expected != nil {
		try("expected", *opts.Expected)
	}
	return anchors, nil
}

// erasedEvents is what the walk found out about erased events: each erased
// event with its tombstone, and the ids of the audit.erased records the chain
// holds.
type erasedEvents struct {
	events    []erasedEvent
	recorders map[string]bool
}

type erasedEvent struct {
	seq  int64
	tomb Tombstone
}

// tombstoneAgrees reports whether a stored erased event is exactly the one its
// tombstone signed: envelope, links and hash.
func tombstoneAgrees(e Event, b TombstoneBody) bool {
	return b.Chain() == e.Chain() && b.EventID == e.ID && b.ChainSeq == e.ChainSeq && b.EventType == string(e.Type) &&
		b.OccurredAtMs == e.OccurredAt.UnixMilli() && b.WorkspaceID == e.WorkspaceID && b.PrincipalID == e.PrincipalID &&
		b.SessionID == e.SessionID && b.PrevHash == e.PrevHash && b.EventHash == e.Hash
}

// verifyTombstones checks the signature of every tombstone the walk met, and
// that the audit.erased record each names is on the chain.
func (l *Log) verifyTombstones(ctx context.Context, key ChainKey, erased erasedEvents, rep *VerifyReport) {
	for _, ev := range erased.events {
		if verr := l.VerifyTombstone(ctx, ev.tomb, key); verr != nil {
			kind := ProblemTombstoneInvalid
			switch contracts.CodeOf(verr) {
			case contracts.ErrExpired:
				kind = ProblemTombstoneExpired
			case contracts.ErrUnavailable:
				kind = ProblemTombstoneUnverifiable
			}
			rep.add(kind, ev.seq, "the tombstone of this event does not verify: %v", verr)
			continue
		}
		if !erased.recorders[ev.tomb.Tombstone.RecordedBy] {
			rep.add(ProblemTombstoneUnrecorded, ev.seq, "the audit.erased event that records this erasure is not on the chain")
		}
	}
}

// walkChain reads a chain in position order and checks it. hashAt receives the
// stored hash at every position that is a key of it on entry.
func walkChain(ctx context.Context, tx *store.Tx, key ChainKey, rep *VerifyReport, hashAt map[int64]string, erased *erasedEvents) error {
	tombs, err := loadTombstones(ctx, tx, key)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT seq, id, workspace_id, chain_seq, event_type, occurred_at, principal_id, session_id, data, prev_hash, hash
		   FROM audit_events WHERE application_id = ? AND tenant_id = ? ORDER BY chain_seq`,
		key.ApplicationID, key.TenantID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var (
		expect   = int64(1)
		prevHash = GenesisHash(key)
		lastRow  int64
	)
	for rows.Next() {
		var (
			row, ms         int64
			data, typ, wsID string
			e               Event
		)
		if err := rows.Scan(&row, &e.ID, &wsID, &e.ChainSeq, &typ, &ms, &e.PrincipalID, &e.SessionID, &data, &e.PrevHash, &e.Hash); err != nil {
			return err
		}
		e.ApplicationID, e.TenantID, e.WorkspaceID = key.ApplicationID, key.TenantID, wsID
		e.Type = EventType(typ)
		e.OccurredAt = time.UnixMilli(ms).UTC()
		e.Data = []byte(data)

		rep.Events++
		if e.ChainSeq != expect {
			rep.add(ProblemGap, e.ChainSeq, "expected position %d, found %d: %d event(s) are missing before it", expect, e.ChainSeq, e.ChainSeq-expect)
		}
		if e.PrevHash != prevHash {
			rep.add(ProblemLinkBroken, e.ChainSeq, "prev_hash is not the hash of the preceding event")
		}
		if !ids.Valid(e.ID, IDPrefix) || !ValidHash(e.Hash) || !ValidHash(e.PrevHash) {
			rep.add(ProblemMalformed, e.ChainSeq, "id or hash is malformed")
		}
		if e.Type == EventAuditErased {
			erased.recorders[e.ID] = true
		}
		if tomb, isErased := tombs[e.ID]; isErased {
			// The payload is gone, so the hash cannot be recomputed: the signed
			// tombstone vouches for the envelope and the hash instead, and the
			// links around the event are checked above like any other.
			rep.Erased++
			switch {
			case tomb == nil:
				rep.add(ProblemTombstoneInvalid, e.ChainSeq, "the tombstone of this event does not parse")
			case string(e.Data) != ErasedMarker || !tombstoneAgrees(e, tomb.Tombstone):
				rep.add(ProblemTombstoneMismatch, e.ChainSeq, "the erased event differs from what its tombstone signed")
			default:
				erased.events = append(erased.events, erasedEvent{seq: e.ChainSeq, tomb: *tomb})
			}
		} else if want, herr := e.ComputeHash(); herr != nil {
			rep.add(ProblemMalformed, e.ChainSeq, "the event cannot be hashed: %s", contracts.CodeOf(herr))
		} else if want != e.Hash {
			rep.add(ProblemHashMismatch, e.ChainSeq, "the stored hash is not the hash of the stored content")
		}
		if row <= lastRow {
			rep.add(ProblemOrder, e.ChainSeq, "the row was inserted before the row of an earlier position")
		}
		lastRow = row
		if _, want := hashAt[e.ChainSeq]; want {
			hashAt[e.ChainSeq] = e.Hash
		}
		expect = e.ChainSeq + 1
		prevHash = e.Hash
		rep.HeadSeq, rep.HeadHash = e.ChainSeq, e.Hash
	}
	return rows.Err()
}

// brokenError wraps ErrChainBroken with a short, data-free summary.
func brokenError(rep VerifyReport) error {
	kinds := map[ProblemKind]int{}
	var order []ProblemKind
	for _, p := range rep.Problems {
		if kinds[p.Kind] == 0 {
			order = append(order, p.Kind)
		}
		kinds[p.Kind]++
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		parts = append(parts, fmt.Sprintf("%s x%d", k, kinds[k]))
	}
	first := rep.Problems[0]
	msg := fmt.Sprintf("audit: chain %s failed verification (%s); first: %s at position %d",
		rep.Chain, strings.Join(parts, ", "), first.Kind, first.ChainSeq)
	return contracts.Wrap(contracts.ErrInternal, ErrChainBroken, msg)
}
