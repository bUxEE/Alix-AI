package providerreg

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Every statement of the module carries this predicate: a registry row is application-level, so
// the tenant and workspace columns are the empty string and the application comes from the
// verified scope. There is no statement without it.
const appWhere = "application_id = ? AND tenant_id = '' AND workspace_id = ''"

// Event kinds recorded in providerreg_events.
const (
	EventRegistered          = "registered"
	EventCredentialBound     = "credential_bound"
	EventBillingAcknowledged = "billing_acknowledged"
	EventConformanceRecorded = "conformance_recorded"
	EventQualified           = "qualified"
	EventQuarantined         = "quarantined"
	EventReleased            = "released"
	EventReviewed            = "reviewed"
	EventModelVersionPinned  = "model_version_pinned"
	EventRetired             = "retired"
	EventPermissionsSet      = "permissions_set"
	EventFallbackSet         = "fallback_set"
	EventFallbackUsed        = "fallback_used"
)

// Event is one recorded change of a profile: who did it, when, and the fixed reason code.
type Event struct {
	ProfileID     providers.ProfileID
	Kind          string
	FromLifecycle providers.ProfileLifecycle
	ToLifecycle   providers.ProfileLifecycle
	Reason        string
	// RelatedID is the other profile of a fallback hop, or the report or acknowledgement id.
	RelatedID string
	Actor     string
	At        time.Time
}

// decodeProfile reads a stored document strictly: an unknown member or value is a corrupted
// row, never silently dropped.
func decodeProfile(doc string) (providers.ProviderProfile, error) {
	var p providers.ProviderProfile
	dec := json.NewDecoder(bytes.NewReader([]byte(doc)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return providers.ProviderProfile{}, contracts.Wrap(contracts.ErrInternal, err, "providerreg: a stored profile cannot be decoded")
	}
	return p, nil
}

// loadProfile reads one profile of the application, or found=false.
func loadProfile(ctx context.Context, q store.Reader, app string, id providers.ProfileID) (p providers.ProviderProfile, found bool, err error) {
	var doc string
	err = q.QueryRowContext(ctx,
		"SELECT document FROM providerreg_profiles WHERE "+appWhere+" AND profile_id = ?", app, string(id)).Scan(&doc)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return providers.ProviderProfile{}, false, nil
	case err != nil:
		return providers.ProviderProfile{}, false, wrapStore(err, "providerreg: reading a profile failed")
	}
	p, err = decodeProfile(doc)
	if err != nil {
		return providers.ProviderProfile{}, false, err
	}
	if p.ProfileID != id {
		return providers.ProviderProfile{}, false, contracts.New(contracts.ErrInternal, "providerreg: a stored profile does not match its row")
	}
	return p, true, nil
}

// quarantineColumn is the stored quarantine_reason: NULL unless quarantined.
func quarantineColumn(p providers.ProviderProfile) any {
	if p.QuarantineReason == nil {
		return nil
	}
	return string(*p.QuarantineReason)
}

func insertProfile(ctx context.Context, q store.Querier, app string, p providers.ProviderProfile, now time.Time) error {
	doc, err := json.Marshal(p)
	if err != nil {
		return contracts.Wrap(contracts.ErrInternal, err, "providerreg: a profile cannot be encoded")
	}
	_, err = q.ExecContext(ctx,
		`INSERT INTO providerreg_profiles
		   (application_id, tenant_id, workspace_id, profile_id, provider_id, model_id, model_version,
		    lifecycle, quarantine_reason, document, created_at, updated_at)
		 VALUES (?, '', '', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		app, string(p.ProfileID), string(p.ProviderID), string(p.ModelID), string(p.ModelVersion),
		string(p.Lifecycle), quarantineColumn(p), string(doc), now.UnixMilli(), now.UnixMilli())
	return wrapStore(err, "providerreg: storing a profile failed")
}

func updateProfile(ctx context.Context, q store.Querier, app string, p providers.ProviderProfile, now time.Time) error {
	doc, err := json.Marshal(p)
	if err != nil {
		return contracts.Wrap(contracts.ErrInternal, err, "providerreg: a profile cannot be encoded")
	}
	res, err := q.ExecContext(ctx,
		`UPDATE providerreg_profiles
		    SET provider_id = ?, model_id = ?, model_version = ?, lifecycle = ?, quarantine_reason = ?,
		        document = ?, updated_at = ?
		  WHERE `+appWhere+` AND profile_id = ?`,
		string(p.ProviderID), string(p.ModelID), string(p.ModelVersion), string(p.Lifecycle), quarantineColumn(p),
		string(doc), now.UnixMilli(), app, string(p.ProfileID))
	if err != nil {
		return wrapStore(err, "providerreg: updating a profile failed")
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return contracts.New(contracts.ErrInternal, "providerreg: a profile update touched an unexpected number of rows")
	}
	return nil
}

func insertEvent(ctx context.Context, q store.Querier, app string, ev Event) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO providerreg_events
		   (application_id, tenant_id, workspace_id, profile_id, kind, from_lifecycle, to_lifecycle,
		    reason, related_id, actor, at)
		 VALUES (?, '', '', ?, ?, ?, ?, ?, ?, ?, ?)`,
		app, string(ev.ProfileID), ev.Kind, string(ev.FromLifecycle), string(ev.ToLifecycle),
		ev.Reason, ev.RelatedID, ev.Actor, ev.At.UnixMilli())
	return wrapStore(err, "providerreg: recording an event failed")
}

// edit is one read-modify-write of a profile inside a single write transaction. The mutator
// changes p and notes events; the registry saves both together, or neither.
type edit struct {
	// q reads other profiles of the same application inside the transaction.
	q      store.Reader
	app    string
	p      providers.ProviderProfile
	before providers.ProfileLifecycle
	now    time.Time
	actor  string
	events []Event
	// keep commits the changes even though the mutator returns an error: a stale profile is
	// quarantined and the caller is still refused.
	keep bool
}

// note records an event on the profile as it is now.
func (e *edit) note(kind string, reason Reason, related string) {
	e.events = append(e.events, Event{
		ProfileID: e.p.ProfileID, Kind: kind, FromLifecycle: e.before, ToLifecycle: e.p.Lifecycle,
		Reason: string(reason), RelatedID: related, Actor: e.actor, At: e.now,
	})
	e.before = e.p.Lifecycle
}

// quarantine holds the profile for review and keeps that change even when the caller is refused.
func (e *edit) quarantine(reason providers.ProviderProfileQuarantineReason) {
	e.p.Lifecycle = providers.ProfileLifecycleQuarantined
	r := reason
	e.p.QuarantineReason = &r
	e.keep = true
	e.note(EventQuarantined, Reason(reason), "")
}

// setLifecycle moves the profile and clears any quarantine reason.
func (e *edit) setLifecycle(l providers.ProfileLifecycle) {
	e.p.Lifecycle = l
	e.p.QuarantineReason = nil
}

// mutate runs fn against the stored profile in one write transaction. An error from fn rolls
// everything back unless fn set keep. The saved profile is returned with fn's error (nil when
// it succeeded). A profile of another application is "not found".
func (r *Registry) mutate(ctx context.Context, scope contracts.Scope, id providers.ProfileID, actor string, fn func(e *edit) error) (providers.ProviderProfile, error) {
	app, err := appOf(scope)
	if err != nil {
		return providers.ProviderProfile{}, err
	}
	var out providers.ProviderProfile
	var fnErr error
	err = r.db.Tx(ctx, func(tx *store.Tx) error {
		out, fnErr = providers.ProviderProfile{}, nil // a busy retry runs the closure again
		p, found, err := loadProfile(ctx, tx, app, id)
		if err != nil {
			return err
		}
		if !found {
			return notFound()
		}
		now := r.clk.Now()
		e := &edit{q: tx, app: app, p: p, before: p.Lifecycle, now: now, actor: actor}
		beforeDoc, err := json.Marshal(p)
		if err != nil {
			return contracts.Wrap(contracts.ErrInternal, err, "providerreg: a profile cannot be encoded")
		}
		ferr := fn(e)
		if ferr != nil && !e.keep {
			return ferr
		}
		fnErr = ferr
		afterDoc, err := json.Marshal(e.p)
		if err != nil {
			return contracts.Wrap(contracts.ErrInternal, err, "providerreg: a profile cannot be encoded")
		}
		if !bytes.Equal(beforeDoc, afterDoc) {
			e.p.UpdatedAt = common.Timestamp(timestamp(now))
			if err := updateProfile(ctx, tx, app, e.p, now); err != nil {
				return err
			}
		}
		for _, ev := range e.events {
			if err := insertEvent(ctx, tx, app, ev); err != nil {
				return err
			}
		}
		out = e.p
		return nil
	})
	if err != nil {
		return providers.ProviderProfile{}, err
	}
	return out, fnErr
}

// Get returns one profile of the caller's application.
func (r *Registry) Get(ctx context.Context, scope contracts.Scope, id providers.ProfileID) (providers.ProviderProfile, error) {
	app, err := appOf(scope)
	if err != nil {
		return providers.ProviderProfile{}, err
	}
	p, found, err := loadProfile(ctx, r.db, app, id)
	if err != nil {
		return providers.ProviderProfile{}, err
	}
	if !found {
		return providers.ProviderProfile{}, notFound()
	}
	return p, nil
}

// List returns every profile of the caller's application, oldest first.
func (r *Registry) List(ctx context.Context, scope contracts.Scope) ([]providers.ProviderProfile, error) {
	app, err := appOf(scope)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, "SELECT document FROM providerreg_profiles WHERE "+appWhere+" ORDER BY seq", app)
	if err != nil {
		return nil, wrapStore(err, "providerreg: listing profiles failed")
	}
	defer rows.Close()
	var out []providers.ProviderProfile
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, wrapStore(err, "providerreg: listing profiles failed")
		}
		p, err := decodeProfile(doc)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStore(err, "providerreg: listing profiles failed")
	}
	return out, nil
}

// Events returns the recorded changes of one profile of the caller's application, oldest first.
func (r *Registry) Events(ctx context.Context, scope contracts.Scope, id providers.ProfileID) ([]Event, error) {
	app, err := appOf(scope)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT kind, from_lifecycle, to_lifecycle, reason, related_id, actor, at
		   FROM providerreg_events WHERE `+appWhere+` AND profile_id = ? ORDER BY seq`, app, string(id))
	if err != nil {
		return nil, wrapStore(err, "providerreg: reading events failed")
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		ev := Event{ProfileID: id}
		var from, to string
		var at int64
		if err := rows.Scan(&ev.Kind, &from, &to, &ev.Reason, &ev.RelatedID, &ev.Actor, &at); err != nil {
			return nil, wrapStore(err, "providerreg: reading events failed")
		}
		ev.FromLifecycle, ev.ToLifecycle, ev.At = providers.ProfileLifecycle(from), providers.ProfileLifecycle(to), time.UnixMilli(at).UTC()
		out = append(out, ev)
	}
	return out, wrapStore(rows.Err(), "providerreg: reading events failed")
}
