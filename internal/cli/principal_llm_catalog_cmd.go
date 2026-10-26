//go:build !noexternal

package cli

import (
	"context"
	"flag"

	"github.com/buxee/alix-ai/internal/providerreg"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func init() {
	for _, c := range principalLLMCatalogCommands() {
		Register(c)
	}
}

func principalLLMCatalogCommands() []Command {
	var (
		profile, display, terms, notice string
		oauthAvail                      bool
	)
	specs := []extWireSpec{
		{
			name: "providers catalog-add", summary: "mark a qualified profile as a per-principal catalog entry (ADR 0041)",
			usage: "alixai providers catalog-add [-config f] -operator ID -application APP -profile ID [-display-name N] [-terms-summary T] [-billing-notice-version V] [-oauth-available]",
			flags: func(fs *flag.FlagSet) func() error {
				fs.StringVar(&profile, "profile", "", "qualified profile id (pprf_...)")
				fs.StringVar(&display, "display-name", "", "display name for the settings UI")
				fs.StringVar(&terms, "terms-summary", "", "short terms summary for the settings UI")
				fs.StringVar(&notice, "billing-notice-version", "", "billing notice version principals must acknowledge")
				fs.BoolVar(&oauthAvail, "oauth-available", false, "provider offers official third-party OAuth for /v1/me/oauth")
				return func() error {
					if profile == "" {
						return Usagef("-profile is required")
					}
					return nil
				}
			},
			run: func(ctx context.Context, s *extWireSession) error {
				entry, err := s.reg.CatalogAdd(ctx, s.scope, providerreg.CatalogEntryInput{
					ProfileID: pv.ProfileID(profile), DisplayName: display, TermsSummary: terms,
					BillingNoticeVersion: notice, OAuthAvailable: oauthAvail,
				})
				if err != nil {
					return err
				}
				return extWirePrint(s.out, entry)
			},
		},
		{
			name: "providers catalog-list", summary: "list operator-approved catalog entries principals may pick (ADR 0041)",
			usage: "alixai providers catalog-list [-config f] -operator ID -application APP",
			run: func(ctx context.Context, s *extWireSession) error {
				cat, err := s.reg.Catalog(ctx, s.scope)
				if err != nil {
					return err
				}
				return extWirePrint(s.out, cat)
			},
		},
		{
			name: "providers catalog-remove", summary: "remove a profile from the per-principal catalog (ADR 0041)",
			usage: "alixai providers catalog-remove [-config f] -operator ID -application APP -profile ID",
			flags: func(fs *flag.FlagSet) func() error {
				fs.StringVar(&profile, "profile", "", "catalog profile id (pprf_...)")
				return func() error {
					if profile == "" {
						return Usagef("-profile is required")
					}
					return nil
				}
			},
			run: func(ctx context.Context, s *extWireSession) error {
				return s.reg.CatalogRemove(ctx, s.scope, pv.ProfileID(profile))
			},
		},
	}
	out := make([]Command, 0, len(specs))
	for _, sp := range specs {
		out = append(out, Command{Name: sp.name, Summary: sp.summary, Run: extWireRunner(sp)})
	}
	return out
}
