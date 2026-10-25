//go:build !noexternal

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/buxee/alix-ai/adapters/external/anthropic"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// The operator commands that manage a provider profile (milestone X, X-26). They work offline against the data
// directory, like `alixai keys init`: no HTTP route, no OpenAPI or SDK change, and no running server needed.
//
//	alixai providers register   -operator ID -application APP (-template anthropic | -profile FILE.json)
//	alixai providers list       -operator ID -application APP
//	alixai providers set-pricing -operator ID -application APP [-tenant T] -profile ID -pricing-file FILE.json
//	alixai providers set-billing-notice -operator ID -application APP [-tenant T] -profile ID -billing-notice-file FILE.json
//	alixai providers ack-billing -operator ID -application APP [-tenant T] -profile ID -notice-version V -confirm-not-api-access -confirm-separate-billing
//	alixai providers review     -operator ID -application APP [-tenant T] -profile ID -terms-hash sha256:... [-reviewed-on YYYY-MM-DD]
//	alixai providers conformance -operator ID -application APP [-tenant T] -profile ID -report FILE.json
//	alixai providers qualify    -operator ID -application APP [-tenant T] -profile ID
//	alixai providers release    -operator ID -application APP [-tenant T] -profile ID [-reviewed-on D] [-region R]...
//	alixai providers hold       -operator ID -application APP [-tenant T] -profile ID
//	alixai providers retire     -operator ID -application APP [-tenant T] -profile ID
//
// Who may run them: "<-application>/<-operator>" must be in external_providers.deployment_operators of the
// configuration, and the section must be enabled. The data directory is the operator's own, so this is the same
// authority the server's routes read, not a new one; every change lands in the registry's event log under that
// principal. With external mode off (the default) every command refuses and nothing is created.
//
// The pricing table and the billing notice of a profile are set at registration (-pricing-file,
// -billing-notice-file, or the template's own) and changed afterwards with set-pricing and set-billing-notice. A
// price change keeps the profile (its id, credential, acknowledgement, conformance report and lifecycle); a later
// call is priced from the new table and a call already reserved keeps the estimate it was admitted with. A changed
// billing notice drops the recorded acknowledgement and returns a qualified profile to candidate: acknowledge the
// new notice (ack-billing) and qualify again. Neither changes a retired profile, and an unchanged notice is a no-op.
//
// The credential is not handled here. A key is submitted through PUT /v1/providers/{provider}/credential (or
// OAuth) once the profile exists, and is bound to the profile when it has none.

func init() {
	for _, c := range extWireCommands() {
		Register(c)
	}
}

// extWireSession is what a command works with: the registry over the data directory and the operator's scope.
type extWireSession struct {
	env   *Env
	out   io.Writer
	admin *externalAdmin
	reg   *providerreg.Registry
	scope contracts.Scope
	now   func() time.Time
}

type extWireSpec struct {
	name, summary, usage string
	// flags registers the command's own flags and returns the check to run after parsing.
	flags func(fs *flag.FlagSet) func() error
	run   func(ctx context.Context, s *extWireSession) error
}

// extWireList is a repeatable string flag.
type extWireList []string

func (l *extWireList) String() string     { return strings.Join(*l, ",") }
func (l *extWireList) Set(v string) error { *l = append(*l, v); return nil }

func extWireCommands() []Command {
	profileFlag := func(dst *string) func(fs *flag.FlagSet) func() error {
		return func(fs *flag.FlagSet) func() error {
			fs.StringVar(dst, "profile", "", "id of the provider profile (pprf_...)")
			return func() error {
				if *dst == "" {
					return Usagef("-profile is required")
				}
				return nil
			}
		}
	}
	var (
		profile                   string
		template, file            string
		reviewedOn, termsHash     string
		pricingFile, noticeFile   string
		permitTenants, regions    extWireList
		reportFile, noticeVersion string
		notAPI, separateBilling   bool
	)
	specs := []extWireSpec{
		{
			name: "providers register", summary: "register a candidate provider profile from the shipped template or a profile file",
			usage: "alixai providers register [-config f] -operator ID -application APP (-template anthropic | -profile-file FILE) [-permit-tenant T]... [-reviewed-on D] [-terms-hash H] [-pricing-file F] [-billing-notice-file F]",
			flags: func(fs *flag.FlagSet) func() error {
				fs.StringVar(&template, "template", "", "start from the profile an adapter ships (anthropic)")
				fs.StringVar(&file, "profile-file", "", "start from this provider profile JSON (the id, credential, lifecycle and timestamps are ignored)")
				fs.StringVar(&reviewedOn, "reviewed-on", "", "date of the terms review, YYYY-MM-DD (default: the profile's own)")
				fs.StringVar(&termsHash, "terms-hash", "", "hash of the terms text that was read (default: the profile's own)")
				fs.StringVar(&pricingFile, "pricing-file", "", "price table JSON (default: the profile's own)")
				fs.StringVar(&noticeFile, "billing-notice-file", "", "billing notice JSON (default: the profile's own)")
				permitTenants = nil // the lists are not reset by fs.Var, and these variables outlive a run
				fs.Var(&permitTenants, "permit-tenant", "narrow use to this tenant (repeatable); default: the whole application")
				return func() error {
					if (template == "") == (file == "") {
						return Usagef("name exactly one of -template and -profile-file")
					}
					return nil
				}
			},
			run: func(ctx context.Context, s *extWireSession) error {
				draft, err := extWireDraft(template, file, s.scope.ApplicationID, permitTenants)
				if err != nil {
					return err
				}
				if reviewedOn != "" {
					draft.LastPolicyReview, draft.Terms.RecordedOn = reviewedOn, reviewedOn
				}
				if termsHash != "" {
					draft.Terms.TermsHash = pv.SHA256(termsHash)
				}
				if pricingFile != "" {
					if err := extWireReadJSON(pricingFile, &draft.Pricing); err != nil {
						return err
					}
				}
				if noticeFile != "" {
					if err := extWireReadJSON(noticeFile, &draft.BillingNotice); err != nil {
						return err
					}
				}
				p, err := s.reg.Register(ctx, s.scope, draft)
				if err != nil {
					return err
				}
				return extWirePrint(s.out, p)
			},
		},
		{
			name: "providers list", summary: "list the provider profiles of an application",
			usage: "alixai providers list [-config f] -operator ID -application APP",
			run: func(ctx context.Context, s *extWireSession) error {
				all, err := s.reg.List(ctx, s.scope)
				if err != nil {
					return err
				}
				if all == nil {
					all = []pv.ProviderProfile{}
				}
				return extWirePrint(s.out, all)
			},
		},
		{
			name: "providers ack-billing", summary: "acknowledge the billing notice of a profile (a subscription is not API access)",
			usage: "alixai providers ack-billing [-config f] -operator ID -application APP [-tenant T] -profile ID -notice-version V -confirm-not-api-access -confirm-separate-billing",
			flags: func(fs *flag.FlagSet) func() error {
				check := profileFlag(&profile)(fs)
				fs.StringVar(&noticeVersion, "notice-version", "", "version of the notice you were shown (see `providers list`)")
				fs.BoolVar(&notAPI, "confirm-not-api-access", false, "I understand a consumer subscription is not API access")
				fs.BoolVar(&separateBilling, "confirm-separate-billing", false, "I understand API usage is billed separately")
				return func() error {
					if err := check(); err != nil {
						return err
					}
					if noticeVersion == "" {
						return Usagef("-notice-version is required")
					}
					return nil
				}
			},
			run: func(ctx context.Context, s *extWireSession) error {
				p, err := s.reg.AcknowledgeBillingNotice(ctx, s.scope, pv.ProfileID(profile), providerreg.Acknowledgement{
					NoticeVersion: noticeVersion, ConsumerSubscriptionIsNotAPIAccess: notAPI, SeparateAPIBillingUnderstood: separateBilling,
				})
				if err != nil {
					return err
				}
				return extWirePrint(s.out, p)
			},
		},
		{
			name: "providers set-pricing", summary: "replace the price table of a profile (the id, credential, acknowledgement and lifecycle are kept)",
			usage: "alixai providers set-pricing [-config f] -operator ID -application APP [-tenant T] -profile ID -pricing-file FILE.json",
			flags: func(fs *flag.FlagSet) func() error {
				check := profileFlag(&profile)(fs)
				fs.StringVar(&pricingFile, "pricing-file", "", "price table JSON (currency, effective_on, prices in integer micro-units)")
				return func() error {
					if err := check(); err != nil {
						return err
					}
					if pricingFile == "" {
						return Usagef("-pricing-file is required")
					}
					return nil
				}
			},
			run: func(ctx context.Context, s *extWireSession) error {
				var table pv.PriceTable
				if err := extWireReadJSON(pricingFile, &table); err != nil {
					return err
				}
				p, err := s.reg.SetPricing(ctx, s.scope, pv.ProfileID(profile), table)
				if err != nil {
					return err
				}
				return extWirePrint(s.out, p)
			},
		},
		{
			name: "providers set-billing-notice", summary: "replace the billing notice of a profile (a changed notice drops the acknowledgement and returns a qualified profile to candidate)",
			usage: "alixai providers set-billing-notice [-config f] -operator ID -application APP [-tenant T] -profile ID -billing-notice-file FILE.json",
			flags: func(fs *flag.FlagSet) func() error {
				check := profileFlag(&profile)(fs)
				fs.StringVar(&noticeFile, "billing-notice-file", "", "billing notice JSON (version, text, optional https url)")
				return func() error {
					if err := check(); err != nil {
						return err
					}
					if noticeFile == "" {
						return Usagef("-billing-notice-file is required")
					}
					return nil
				}
			},
			run: func(ctx context.Context, s *extWireSession) error {
				var notice pv.ProviderProfileBillingNotice
				if err := extWireReadJSON(noticeFile, &notice); err != nil {
					return err
				}
				p, err := s.reg.SetBillingNotice(ctx, s.scope, pv.ProfileID(profile), notice)
				if err != nil {
					return err
				}
				return extWirePrint(s.out, p)
			},
		},
		{
			name: "providers review", summary: "record an operator review of the provider's terms (ends a stale_review, terms_changed or operator_hold quarantine)",
			usage: "alixai providers review [-config f] -operator ID -application APP [-tenant T] -profile ID -terms-hash sha256:... [-reviewed-on YYYY-MM-DD]",
			flags: func(fs *flag.FlagSet) func() error {
				check := profileFlag(&profile)(fs)
				fs.StringVar(&termsHash, "terms-hash", "", "hash of the terms text that was read")
				fs.StringVar(&reviewedOn, "reviewed-on", "", "date of the review, YYYY-MM-DD (default: today, UTC)")
				return func() error {
					if err := check(); err != nil {
						return err
					}
					if termsHash == "" {
						return Usagef("-terms-hash is required")
					}
					return nil
				}
			},
			run: func(ctx context.Context, s *extWireSession) error {
				p, err := s.reg.Review(ctx, s.scope, pv.ProfileID(profile), providerreg.ReviewInput{
					TermsHash: pv.SHA256(termsHash), ReviewedOn: extWireDay(reviewedOn, s.now),
				})
				if err != nil {
					return err
				}
				return extWirePrint(s.out, p)
			},
		},
		{
			name: "providers conformance", summary: "record a conformance report (from `go run ./tools/providerconf`) on a profile",
			usage: "alixai providers conformance [-config f] -operator ID -application APP [-tenant T] -profile ID -report FILE.json",
			flags: func(fs *flag.FlagSet) func() error {
				check := profileFlag(&profile)(fs)
				fs.StringVar(&reportFile, "report", "", "conformance report JSON")
				return func() error {
					if err := check(); err != nil {
						return err
					}
					if reportFile == "" {
						return Usagef("-report is required")
					}
					return nil
				}
			},
			run: func(ctx context.Context, s *extWireSession) error {
				var rep pv.ConformanceReport
				if err := extWireReadJSON(reportFile, &rep); err != nil {
					return err
				}
				p, err := s.reg.RecordConformance(ctx, s.scope, pv.ProfileID(profile), rep)
				if err != nil {
					return err
				}
				return extWirePrint(s.out, p)
			},
		},
		{
			name: "providers qualify", summary: "make a candidate profile production-qualified (needs credential, billing ack, passing report, fresh review)",
			usage: "alixai providers qualify [-config f] -operator ID -application APP [-tenant T] -profile ID",
			flags: profileFlag(&profile),
			run: func(ctx context.Context, s *extWireSession) error {
				p, err := s.reg.Qualify(ctx, s.scope, pv.ProfileID(profile))
				if err != nil {
					return err
				}
				return extWirePrint(s.out, p)
			},
		},
		{
			name: "providers release", summary: "end a region_changed or excessive_permissions quarantine",
			usage: "alixai providers release [-config f] -operator ID -application APP [-tenant T] -profile ID [-reviewed-on D] [-region R]...",
			flags: func(fs *flag.FlagSet) func() error {
				check := profileFlag(&profile)(fs)
				fs.StringVar(&reviewedOn, "reviewed-on", "", "date of the review that justifies the release (default: today, UTC)")
				regions = nil
				fs.Var(&regions, "region", "a processing region the operator accepted (repeatable; region_changed only)")
				return check
			},
			run: func(ctx context.Context, s *extWireSession) error {
				in := providerreg.ReleaseInput{ReviewedOn: extWireDay(reviewedOn, s.now)}
				for _, r := range regions {
					in.Regions = append(in.Regions, pv.RegionCode(r))
				}
				p, err := s.reg.Release(ctx, s.scope, pv.ProfileID(profile), in)
				if err != nil {
					return err
				}
				return extWirePrint(s.out, p)
			},
		},
		{
			name: "providers hold", summary: "quarantine a profile by hand (operator_hold); a review ends it",
			usage: "alixai providers hold [-config f] -operator ID -application APP [-tenant T] -profile ID",
			flags: profileFlag(&profile),
			run: func(ctx context.Context, s *extWireSession) error {
				p, err := s.reg.Hold(ctx, s.scope, pv.ProfileID(profile))
				if err != nil {
					return err
				}
				return extWirePrint(s.out, p)
			},
		},
		{
			name: "providers retire", summary: "retire a profile for good",
			usage: "alixai providers retire [-config f] -operator ID -application APP [-tenant T] -profile ID",
			flags: profileFlag(&profile),
			run: func(ctx context.Context, s *extWireSession) error {
				p, err := s.reg.Retire(ctx, s.scope, pv.ProfileID(profile))
				if err != nil {
					return err
				}
				return extWirePrint(s.out, p)
			},
		},
	}
	out := make([]Command, 0, len(specs))
	for _, sp := range specs {
		out = append(out, Command{Name: sp.name, Summary: sp.summary, Run: extWireRunner(sp)})
	}
	return out
}

// extWireRunner turns a spec into a command. The state of the flags is local to each run.
func extWireRunner(sp extWireSpec) func(context.Context, *Env, []string) error {
	return func(ctx context.Context, env *Env, args []string) error {
		fs := env.FlagSet(sp.name)
		cfgPath := env.ConfigFlag(fs)
		operator := fs.String("operator", "", `principal id of the operator ("<application>/<operator>" is listed in external_providers.deployment_operators)`)
		application := fs.String("application", "", "application the profiles belong to")
		tenant := fs.String("tenant", "operator-cli", "tenant whose credential the profile is bound to (qualify and release check it in this tenant)")
		var check func() error
		if sp.flags != nil {
			check = sp.flags(fs)
		}
		if err := env.ParseFlags(fs, sp.usage, args); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return Usagef("unexpected argument %q", fs.Arg(0))
		}
		if *operator == "" || *application == "" {
			return Usagef("-operator and -application are required")
		}
		if check != nil {
			if err := check(); err != nil {
				return err
			}
		}
		s, closeFn, err := extWireOpen(ctx, env, *cfgPath, *operator, *application, *tenant)
		if err != nil {
			return err
		}
		defer closeFn()
		return sp.run(ctx, s)
	}
}

// extWireOpen loads the configuration, refuses unless external mode is enabled and the operator is listed, and
// opens the services over <data_dir>/external.
func extWireOpen(ctx context.Context, env *Env, cfgPath, operator, application, tenant string) (*extWireSession, func(), error) {
	db, core, err := operatorStore(ctx, env, cfgPath)
	if err != nil {
		return nil, nil, err
	}
	closeDB := func() { _ = db.Close() }
	sec := externalSection
	switch {
	case !sec.Enabled:
		closeDB()
		return nil, nil, errors.New("providers: external mode is off (external_providers.enabled); nothing was changed")
	case !sec.isDeploymentOperator(application, operator):
		closeDB()
		return nil, nil, contracts.New(contracts.ErrPermissionDenied, `providers: "<application>/<operator>" is not listed in external_providers.deployment_operators`)
	}
	deployment := identity.CurrentConfig().DeploymentID
	if deployment == "" {
		closeDB()
		return nil, nil, errors.New("providers: identity.deployment_id is not configured")
	}
	clk := clock.Real{}
	log := slog.New(slog.DiscardHandler)
	alg, err := audit.New(db, audit.Options{Clock: clk, Registry: audit.DefaultRegistry, Logger: log})
	if err != nil {
		closeDB()
		return nil, nil, err
	}
	admin, err := newExternalAdmin(externalAdminDeps{
		DB: db, Audit: alg, Clock: clk, Log: log, DeploymentID: deployment, Dir: filepath.Join(core.DataDir.String(), "external"), Section: sec,
	})
	if err != nil {
		closeDB()
		return nil, nil, err
	}
	scope := contracts.Scope{
		DeploymentID: deployment, ApplicationID: application, TenantID: tenant, WorkspaceID: "operator-cli",
		PrincipalID: operator, Roles: []string{"operator"}, CapabilityScopes: []string{"providers.manage"}, SessionID: "ses_operator_cli",
	}
	if err := scope.Validate(); err != nil {
		closeDB()
		return nil, nil, Usagef("-operator, -application or -tenant is not an identifier: %v", err)
	}
	return &extWireSession{env: env, out: env.Stdout, admin: admin, reg: admin.reg, scope: scope, now: clk.Now}, closeDB, nil
}

// extWireDraft builds the registration draft from the shipped template or a file, clears what the registry owns
// and sets the grant to the application (or to the tenants named).
func extWireDraft(template, file, application string, tenants []string) (pv.ProviderProfile, error) {
	var raw []byte
	switch {
	case template == ExternalProviderAnthropic:
		raw = anthropic.ProfileJSON()
	case template != "":
		return pv.ProviderProfile{}, Usagef("-template %q: only anthropic ships a profile; use -profile-file", template)
	default:
		b, err := os.ReadFile(file)
		if err != nil {
			return pv.ProviderProfile{}, fmt.Errorf("providers: %w", err)
		}
		raw = b
	}
	var p pv.ProviderProfile
	if err := json.Unmarshal(raw, &p); err != nil {
		return pv.ProviderProfile{}, fmt.Errorf("providers: the profile is not valid: %w", err)
	}
	p.ProfileID, p.CredRef, p.BillingNoticeAck, p.LastConformance = "", nil, nil, nil
	p.Lifecycle, p.QuarantineReason, p.CreatedAt, p.UpdatedAt = "", nil, "", ""
	app := common.Identifier(application)
	p.PermittedScopes = []pv.ProviderProfileScopeGrant{{ApplicationID: app}}
	if len(tenants) > 0 {
		p.PermittedScopes = nil
		for _, t := range tenants {
			id := common.Identifier(t)
			p.PermittedScopes = append(p.PermittedScopes, pv.ProviderProfileScopeGrant{ApplicationID: app, TenantID: &id})
		}
	}
	return p, nil
}

func extWireReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("providers: %w", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("providers: %s is not valid: %w", filepath.Base(path), err)
	}
	return nil
}

func extWireDay(v string, now func() time.Time) string {
	if v != "" {
		return v
	}
	return now().UTC().Format("2006-01-02")
}

func extWirePrint(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
