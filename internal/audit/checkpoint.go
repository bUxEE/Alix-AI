package audit

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Checkpoint format and ID prefix.
const (
	CheckpointFormat   = "alixai.audit-checkpoint/v1"
	CheckpointIDPrefix = "achk"
)

// CheckpointBody is the document a checkpoint signature covers
// (trust.CanonicalJSON of it). It states that the chain of (application,
// tenant) had HeadSeq events and that the hash of the last one was HeadHash.
// CreatedAt and ExpiresAt repeat the signature record's signed_at and
// expires_at inside the signed bytes: the signature record's own timestamps
// are not covered by the signature, so verification requires them to agree.
type CheckpointBody struct {
	Format        string `json:"format"`
	ID            string `json:"id"`
	ApplicationID string `json:"application_id"`
	TenantID      string `json:"tenant_id"`
	HeadSeq       int64  `json:"head_seq"`
	HeadHash      string `json:"head_hash"`
	CreatedAt     string `json:"created_at"`
	ExpiresAt     string `json:"expires_at"`
}

// Chain returns the chain the checkpoint is for.
func (b CheckpointBody) Chain() ChainKey {
	return ChainKey{ApplicationID: b.ApplicationID, TenantID: b.TenantID}
}

// Checkpoint is a signed chain head. It is the unit that is held outside the
// database (in an integration-audit report, in a backup manifest): its JSON
// form is self-contained.
type Checkpoint struct {
	Checkpoint CheckpointBody  `json:"checkpoint"`
	Signature  trust.Signature `json:"signature"`
}

func checkpointTime(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

// Checkpoint signs the current head of a chain and stores the checkpoint. If
// the newest checkpoint already covers the same head and has not expired it is
// returned unchanged. It fails with unavailable when the Log has no signing
// key, and with validation_failed for a chain without events.
func (l *Log) Checkpoint(ctx context.Context, key ChainKey) (Checkpoint, error) {
	if l.signing == nil {
		return Checkpoint{}, contracts.New(contracts.ErrUnavailable, "audit: no checkpoint signing key is configured")
	}
	if err := key.validate(); err != nil {
		return Checkpoint{}, err
	}
	var out Checkpoint
	err := l.db.Tx(ctx, func(tx *store.Tx) error {
		seq, hash, err := chainHead(ctx, tx, key)
		if err != nil {
			return err
		}
		if seq == 0 {
			return contracts.New(contracts.ErrValidationFailed, "audit: the chain has no events to checkpoint")
		}
		now := l.clk.Now().UTC()
		if prev, ok, err := latestCheckpoint(ctx, tx, key); err != nil {
			return err
		} else if ok && prev.cp.Checkpoint.HeadSeq == seq && prev.cp.Checkpoint.HeadHash == hash {
			if exp, perr := time.Parse(time.RFC3339, prev.cp.Checkpoint.ExpiresAt); perr == nil && exp.After(now) {
				out = prev.cp
				return nil
			}
		}
		signedAt := now.Truncate(time.Second)
		body := CheckpointBody{
			Format:        CheckpointFormat,
			ID:            l.ids.New(CheckpointIDPrefix),
			ApplicationID: key.ApplicationID,
			TenantID:      key.TenantID,
			HeadSeq:       seq,
			HeadHash:      hash,
			CreatedAt:     checkpointTime(signedAt),
			ExpiresAt:     checkpointTime(signedAt.Add(l.validity)),
		}
		sig, err := trust.SignPayload(body, l.signing.KeyID, l.signing.Key, signedAt, signedAt.Add(l.validity))
		if err != nil {
			return err
		}
		if sig.SignedAt != body.CreatedAt || sig.ExpiresAt != body.ExpiresAt {
			return contracts.New(contracts.ErrInternal, "audit: the signature record disagrees with the checkpoint body about time")
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
			`INSERT INTO audit_checkpoints (id, application_id, tenant_id, workspace_id, head_seq, body, signature, created_at)
			 VALUES (?, ?, ?, '', ?, ?, ?, ?)`,
			body.ID, key.ApplicationID, key.TenantID, seq, string(canon), string(sigJSON), now.UnixMilli()); err != nil {
			return err
		}
		out = Checkpoint{Checkpoint: body, Signature: sig}
		return nil
	})
	if err != nil {
		return Checkpoint{}, wrapDB(err)
	}
	return out, nil
}

// MaybeCheckpoint signs a checkpoint when ev is the Nth event of its chain
// (Options.CheckpointEvery). Append and AppendAt call it after their
// transaction commits; a caller that used AppendTx calls it after its own
// commit. A failure never fails the append that triggered it, which is already
// durable: it is logged, and the next checkpoint (or CheckpointAll) covers it.
func (l *Log) MaybeCheckpoint(ctx context.Context, ev Event) {
	if l.every <= 0 || l.signing == nil || ev.ChainSeq%l.every != 0 {
		return
	}
	if _, err := l.Checkpoint(ctx, ev.Chain()); err != nil {
		l.logger.Warn("audit checkpoint failed", "chain", ev.Chain().String(), "head_seq", ev.ChainSeq, "code", string(contracts.CodeOf(err)))
	}
}

// CheckpointAll signs a checkpoint for every chain whose head moved since its
// newest checkpoint. It is the periodic job's body. It continues past a failing
// chain and returns the first error after trying all of them.
func (l *Log) CheckpointAll(ctx context.Context) (signed int, err error) {
	chains, err := l.Chains(ctx)
	if err != nil {
		return 0, err
	}
	for _, k := range chains {
		before, ok, perr := l.latestStored(ctx, k)
		if perr != nil {
			err = firstErr(err, perr)
			continue
		}
		cp, cerr := l.Checkpoint(ctx, k)
		if cerr != nil {
			err = firstErr(err, cerr)
			continue
		}
		if !ok || before.Checkpoint.ID != cp.Checkpoint.ID {
			signed++
		}
	}
	return signed, err
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// ExportCheckpoint returns the newest stored checkpoint of a chain, for
// holding outside the database. It does not verify it: whoever relies on it
// passes it to VerifyChain (VerifyOptions.Expected) or VerifyCheckpoint.
func (l *Log) ExportCheckpoint(ctx context.Context, key ChainKey) (Checkpoint, error) {
	if err := key.validate(); err != nil {
		return Checkpoint{}, err
	}
	cp, ok, err := l.latestStored(ctx, key)
	if err != nil {
		return Checkpoint{}, err
	}
	if !ok {
		return Checkpoint{}, contracts.New(contracts.ErrValidationFailed, "audit: the chain has no checkpoint")
	}
	return cp, nil
}

func (l *Log) latestStored(ctx context.Context, key ChainKey) (Checkpoint, bool, error) {
	var (
		out Checkpoint
		ok  bool
	)
	err := l.db.View(ctx, func(tx *store.Tx) error {
		s, found, err := latestCheckpoint(ctx, tx, key)
		out, ok = s.cp, found
		return err
	})
	if err != nil {
		return Checkpoint{}, false, wrapDB(err)
	}
	return out, ok, nil
}

// storedCheckpoint is a checkpoint row. column is the head_seq the row
// declares; VerifyChain checks it against the signed body.
type storedCheckpoint struct {
	cp     Checkpoint
	column int64
}

// latestCheckpoint reads the newest checkpoint of a chain: highest head, then
// the most recently stored.
func latestCheckpoint(ctx context.Context, q store.Reader, key ChainKey) (storedCheckpoint, bool, error) {
	var (
		s    storedCheckpoint
		body string
		sig  string
	)
	err := q.QueryRowContext(ctx,
		`SELECT head_seq, body, signature FROM audit_checkpoints
		 WHERE application_id = ? AND tenant_id = ? AND workspace_id = ''
		 ORDER BY head_seq DESC, seq DESC LIMIT 1`,
		key.ApplicationID, key.TenantID).Scan(&s.column, &body, &sig)
	if errors.Is(err, sql.ErrNoRows) {
		return storedCheckpoint{}, false, nil
	}
	if err != nil {
		return storedCheckpoint{}, false, err
	}
	if err := decodeStrict([]byte(body), &s.cp.Checkpoint); err != nil {
		return storedCheckpoint{}, false, contracts.New(contracts.ErrInternal, "audit: a stored checkpoint body does not parse")
	}
	if err := decodeStrict([]byte(sig), &s.cp.Signature); err != nil {
		return storedCheckpoint{}, false, contracts.New(contracts.ErrInternal, "audit: a stored checkpoint signature does not parse")
	}
	return s, true, nil
}

func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// VerifyCheckpoint checks a checkpoint on its own: its format, that it names
// key, that its signed timestamps agree with the signature record, and that
// the signature verifies under a platform key from the trust store registered
// for trust.PurposeAuditCheckpoint (no other platform key, such as a release
// key, can make a checkpoint) and has not expired. It says nothing about whether any chain matches it.
//
// The error is unauthenticated for a signature that does not verify or a key
// that is unknown or revoked, expired when expiry is the only reason,
// scope_violation for a checkpoint of another chain, and unavailable when the
// Log has no verifier.
func (l *Log) VerifyCheckpoint(ctx context.Context, cp Checkpoint, key ChainKey) error {
	if l.verifier == nil {
		return contracts.New(contracts.ErrUnavailable, "audit: no trust verifier is configured, checkpoints cannot be verified")
	}
	b := cp.Checkpoint
	if b.Format != CheckpointFormat {
		return contracts.New(contracts.ErrSchemaInvalid, "audit: unknown checkpoint format")
	}
	if b.Chain() != key {
		return contracts.New(contracts.ErrScopeViolation, "audit: the checkpoint is for another chain")
	}
	if b.HeadSeq < 1 || !ValidHash(b.HeadHash) {
		return contracts.New(contracts.ErrSchemaInvalid, "audit: the checkpoint head is malformed")
	}
	if b.CreatedAt != cp.Signature.SignedAt || b.ExpiresAt != cp.Signature.ExpiresAt {
		return contracts.New(contracts.ErrUnauthenticated, "audit: the checkpoint's signed timestamps disagree with its signature record")
	}
	if _, err := l.verifier.VerifyPayload(ctx, b, []trust.Signature{cp.Signature},
		trust.PayloadOptions{ApplicationID: trust.PlatformApplicationID, Purpose: trust.PurposeAuditCheckpoint}); err != nil {
		return err
	}
	return nil
}
