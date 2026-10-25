package apps

import (
	"context"
	"embed"
	"errors"
	"fmt"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

//go:embed migrations/*.sql
var migrations embed.FS

// This package imports apppkg, so apppkg's migrations are registered before these triggers are.
func init() { store.RegisterMigrations("apps", migrations) }

// refusedAudit is the data of the refusal's audit event. It holds application ids and domain labels
// (an operator's choice, never a secret) and the package's manifest hash.
type refusedAudit struct {
	Phase                string `json:"phase"`
	SecurityDomain       string `json:"security_domain"`
	ActiveApplicationID  string `json:"active_application_id"`
	ActiveSecurityDomain string `json:"active_security_domain"`
	ManifestHash         string `json:"manifest_hash,omitempty"`
}

// EventDomainRefused is the audit event of a refusal, on the application chain of the application
// that was refused.
var EventDomainRefused = audit.MustRegister[refusedAudit](audit.TypeSpec{
	Name:        "package.domain_refused",
	Description: "an application was refused because its security domain differs from the applications active in this process (ADR 0016)",
	Levels:      audit.LevelApplication,
	Required:    []string{"phase", "active_application_id"},
})

// Guard applies the rule of this package over a store and an audit log.
type Guard struct {
	db  *store.DB
	log *audit.Log
}

// NewGuard returns a Guard. Both arguments are required: a refusal that cannot be recorded is still
// a refusal, but a guard that could never record one is a wiring mistake found at start.
func NewGuard(db *store.DB, log *audit.Log) (*Guard, error) {
	switch {
	case db == nil:
		return nil, contracts.New(contracts.ErrInternal, "apps: a guard needs the store")
	case log == nil:
		return nil, contracts.New(contracts.ErrInternal, "apps: a guard needs the audit log")
	}
	return &Guard{db: db, log: log}, nil
}

// Others lists the applications that are active in the process under an id other than
// applicationID, with their domains, ordered by application id. A store that cannot be read is an
// unavailable error, never an empty list: nothing is admitted on a guess.
func (g *Guard) Others(ctx context.Context, applicationID string) ([]Active, error) {
	var out []Active
	err := g.db.View(ctx, func(tx *store.Tx) error {
		out = out[:0]
		rows, err := tx.QueryContext(ctx,
			`SELECT application_id, security_domain FROM apppkg_packages
			  WHERE status = ? AND application_id <> ? ORDER BY application_id`,
			string(apppkg.StateActive), applicationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a Active
			if err := rows.Scan(&a.ApplicationID, &a.SecurityDomain); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "apps: cannot read the applications that are active in this process")
	}
	return out, nil
}

// Check applies the rule to c. It returns nil when c may be installed or activated and the typed
// refusal (code conflict, details reason security_domain_mismatch) when not. It records nothing.
func (g *Guard) Check(ctx context.Context, c Candidate) error {
	switch {
	case c.ApplicationID == "":
		return contracts.New(contracts.ErrValidationFailed, "apps: a candidate needs its application id")
	case !c.Phase.Valid():
		return contracts.Newf(contracts.ErrValidationFailed, "apps: %q is not a phase of the rule (want %s or %s)", c.Phase, PhaseInstall, PhaseActivation)
	}
	active, err := g.Others(ctx, c.ApplicationID)
	if err != nil {
		return err
	}
	if a, ok := Conflict(c, active); ok {
		return refusal(c, a)
	}
	return nil
}

// Admit is Check that also records a refusal: the event package.domain_refused on the candidate's
// application chain, attributed to operator ("" for the system). The refusal is returned whether or
// not the event could be written (a refusal never depends on its record); when the event could not
// be, the returned error says so as well.
func (g *Guard) Admit(ctx context.Context, c Candidate, operator string) error {
	err := g.Check(ctx, c)
	ce, ok := AsRefusal(err)
	if !ok {
		return err
	}
	loc, lerr := audit.ForApplication(c.ApplicationID, operator)
	if lerr == nil {
		_, lerr = EventDomainRefused.Append(ctx, g.log, loc, refusedAudit{
			Phase:                string(c.Phase),
			SecurityDomain:       c.SecurityDomain,
			ActiveApplicationID:  fmt.Sprint(ce.Details["active_application_id"]),
			ActiveSecurityDomain: fmt.Sprint(ce.Details["active_security_domain"]),
			ManifestHash:         c.ManifestHash,
		})
	}
	if lerr != nil {
		return errors.Join(err, fmt.Errorf("apps: the refusal could not be recorded in the audit log: %w", lerr))
	}
	return err
}
