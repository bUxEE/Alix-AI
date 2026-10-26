//go:build !noexternal

package cli

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/fsperm"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/oauth"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// externalOAuthConfig is external_providers.oauth (1.1.0 popup catalog).
type externalOAuthConfig struct {
	PopupOrigins []string                         `yaml:"popup_origins"`
	Providers    map[string]externalOAuthProvider `yaml:"providers"`
}

// externalOAuthProvider is one catalog entry under external_providers.oauth.providers.<id>.
type externalOAuthProvider struct {
	Enabled         bool     `yaml:"enabled"`
	ClientID        string   `yaml:"client_id"`
	ClientSecretRef string   `yaml:"client_secret_ref"`
	RedirectURIs    []string `yaml:"redirect_uris"`
	Loopback        *bool    `yaml:"loopback"`
	Disabled        bool     `yaml:"disabled"`
}

// externalCredRefConfig is one external_providers.credentials[] entry (env or file reference).
// Application and Tenant are required for serve-time binding (credentials are always scoped).
type externalCredRefConfig struct {
	Provider    string `yaml:"provider"`
	Label       string `yaml:"label"`
	Env         string `yaml:"env"`
	File        string `yaml:"file"`
	Application string `yaml:"application"`
	Tenant      string `yaml:"tenant"`
}

func (c externalOAuthConfig) Validate() error {
	for _, o := range c.PopupOrigins {
		if o == "*" || !oauth.OriginAllowed(o, []string{o}) {
			return contracts.New(contracts.ErrValidationFailed, "external_providers.oauth.popup_origins: exact origins only (never *)")
		}
	}
	for id, p := range c.Providers {
		if _, ok := oauth.Preset(pv.ProviderID(id)); !ok {
			return contracts.New(contracts.ErrValidationFailed, "external_providers.oauth.providers: unknown provider "+id)
		}
		if p.Enabled && id != "openai" && id != "openrouter" && p.ClientID == "" {
			return contracts.New(contracts.ErrValidationFailed, "external_providers.oauth.providers."+id+": client_id is required when enabled")
		}
		for _, r := range p.RedirectURIs {
			u, err := url.Parse(r)
			if err != nil || u.Scheme != "https" {
				return contracts.New(contracts.ErrValidationFailed, "external_providers.oauth.providers."+id+": redirect_uris must be https")
			}
		}
	}
	return nil
}

func (c externalCredRefConfig) Validate() error {
	if c.Provider == "" {
		return contracts.New(contracts.ErrValidationFailed, "external_providers.credentials: provider is required")
	}
	if (c.Env == "") == (c.File == "") {
		return contracts.New(contracts.ErrValidationFailed, "external_providers.credentials: name exactly one of env or file")
	}
	if c.Env != "" {
		if !strings.HasPrefix(c.Env, credentials.DefaultEnvPrefix) {
			return contracts.New(contracts.ErrValidationFailed, "external_providers.credentials: env must use prefix "+credentials.DefaultEnvPrefix)
		}
		if looksLikeLiteralKey(c.Env) {
			return contracts.New(contracts.ErrValidationFailed, "external_providers.credentials: env names a variable, not a literal key; use env: ALIXAI_SECRET_…")
		}
	}
	if c.File != "" && (strings.ContainsAny(c.File, `/\`) || looksLikeLiteralKey(c.File)) {
		return contracts.New(contracts.ErrValidationFailed, "external_providers.credentials: file is a name in the secret directory, not a path or literal key")
	}
	if looksLikeLiteralKey(c.Label) {
		return contracts.New(contracts.ErrValidationFailed, "external_providers.credentials: label must not look like a literal key")
	}
	return nil
}

func looksLikeLiteralKey(s string) bool {
	if len(s) < 12 {
		return false
	}
	for _, p := range []string{"sk-", "sk-or-", "sk-ant-", "xai-", "AIza"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func buildOAuthProviders(sec externalConfig) ([]oauth.ProviderConfig, error) {
	var out []oauth.ProviderConfig
	for id, entry := range sec.OAuth.Providers {
		if !entry.Enabled || entry.Disabled {
			continue
		}
		preset, ok := oauth.Preset(pv.ProviderID(id))
		if !ok {
			return nil, contracts.New(contracts.ErrValidationFailed, "unknown oauth provider "+id)
		}
		if entry.ClientID != "" {
			preset.ClientID = entry.ClientID
		}
		if preset.DynamicClient && preset.ClientID == "" {
			preset.ClientID = preset.InitialClientID
		}
		preset.ClientSecretRef = entry.ClientSecretRef
		if entry.ClientSecretRef != "" {
			preset.ClientAuth = oauth.ClientAuthPost
		}
		if len(entry.RedirectURIs) > 0 {
			preset.RedirectURIs = append([]string{}, entry.RedirectURIs...)
		}
		if entry.Loopback != nil {
			preset.Loopback = *entry.Loopback
		}
		if preset.LoopbackOnly {
			preset.Loopback = true
		}
		n, err := preset.Normalize()
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func allowOAuthEgress(eg *egress.Registry, app string, providers []oauth.ProviderConfig) error {
	if eg == nil || app == "" {
		return nil
	}
	var dests []egress.Destination
	for _, p := range providers {
		for _, raw := range []string{p.AuthorizationEndpoint, p.TokenEndpoint, p.RevocationEndpoint, p.KeyExchangeEndpoint} {
			if raw == "" {
				continue
			}
			d, err := egress.DestinationFromURL(raw)
			if err != nil {
				continue
			}
			dests = append(dests, d)
		}
	}
	if len(dests) == 0 {
		return nil
	}
	al, err := egress.NewAllowlist(dests...)
	if err != nil {
		return err
	}
	return eg.Set(app, al)
}

func (a *externalAdmin) ensureOAuthEgress(app string, id pv.ProviderID) error {
	if a == nil || a.oauth == nil {
		return nil
	}
	preset, ok := oauth.Preset(id)
	if !ok {
		return nil
	}
	// Merge the requested preset with every engine-configured provider so Set does not drop siblings.
	providers := []oauth.ProviderConfig{preset}
	if built, err := buildOAuthProviders(externalSection); err == nil {
		providers = append(providers, built...)
	}
	return allowOAuthEgress(a.oauthEgress(), app, providers)
}

// oauthEgress returns the engine's egress registry (same pointer New was given).
func (a *externalAdmin) oauthEgress() *egress.Registry {
	if a != nil && a.oauthEg != nil {
		return a.oauthEg
	}
	return extFinalOAuthRegistry()
}

// OAuthFlowStatus implements gateway.ProvidersOAuthFlowService.
func (a *externalAdmin) OAuthFlowStatus(ctx context.Context, scope contracts.Scope, flowID string) (gateway.ProvidersOAuthFlowStatus, error) {
	v, err := a.oauth.FlowGet(ctx, scope, flowID)
	if err != nil {
		return gateway.ProvidersOAuthFlowStatus{}, err
	}
	out := gateway.ProvidersOAuthFlowStatus{
		FlowID: v.FlowID, Provider: v.Provider, Status: string(v.Status), UpdatedAt: v.UpdatedAt,
	}
	if v.Reason != "" {
		out.Reason = string(v.Reason)
	}
	if v.Status == oauth.FlowCompleted && v.Connection != nil {
		prof, err := a.profileFor(ctx, scope, v.Connection.Ref.Binding.ProviderID)
		if err == nil {
			if st, err := a.status(ctx, scope, prof); err == nil {
				out.Connection = &st
			}
		}
	}
	return out, nil
}

// OAuthFlowCancel implements gateway.ProvidersOAuthFlowService.
func (a *externalAdmin) OAuthFlowCancel(ctx context.Context, scope contracts.Scope, flowID string) error {
	return a.oauth.FlowCancel(ctx, scope, flowID)
}

var _ gateway.ProvidersOAuthFlowService = (*externalAdmin)(nil)

func (a *externalAdmin) oauthStartWithOptions(ctx context.Context, scope contracts.Scope, req gateway.ProvidersOAuthStart) (gateway.ProvidersOAuthStarted, error) {
	if _, ok := oauth.Preset(req.Provider); ok {
		_ = a.ensureOAuthEgress(scope.ApplicationID, req.Provider)
	}
	pr := oauth.PrepareRequest{ProviderID: req.Provider, Capabilities: req.Capabilities, Reauthorize: req.Reauthorize, SwitchAccount: req.SwitchAccount}
	consent, err := a.oauth.Prepare(ctx, scope, pr)
	if err != nil {
		return gateway.ProvidersOAuthStarted{}, externalOAuthError(err)
	}
	presentation := string(req.Presentation)
	if presentation == "" {
		presentation = oauth.PresentationRedirect
	}
	callback := string(req.Callback)
	if callback == "" {
		callback = oauth.CallbackHost
	}
	redirect := req.RedirectURI
	var lb *oauth.LoopbackServer
	if callback == oauth.CallbackLoopback {
		if redirect != gateway.ProvidersOAuthLoopbackPlaceholder && redirect != oauth.LoopbackPlaceholder {
			return gateway.ProvidersOAuthStarted{}, contracts.New(contracts.ErrValidationFailed,
				"loopback callback needs redirect_uri "+gateway.ProvidersOAuthLoopbackPlaceholder)
		}
		path := "/oauth/callback"
		if preset, ok := oauth.Preset(req.Provider); ok && preset.LoopbackPath != "" {
			path = preset.LoopbackPath
		}
		lb, redirect, err = a.oauth.BindLoopback(path)
		if err != nil {
			return gateway.ProvidersOAuthStarted{}, err
		}
	}
	hostID, err := a.ensureAgentHostID()
	if err != nil {
		if lb != nil {
			_ = lb.Close()
		}
		return gateway.ProvidersOAuthStarted{}, err
	}
	au, err := a.oauth.Begin(ctx, scope, oauth.BeginRequest{
		PrepareRequest: pr, ConsentDigest: consent.Digest, RedirectURI: redirect, Label: req.Label,
		Presentation: presentation, CallbackMode: callback, OpenerOrigin: req.OpenerOrigin,
		ExtAgentHostID: hostID,
	})
	if err != nil {
		if lb != nil {
			_ = lb.Close()
		}
		return gateway.ProvidersOAuthStarted{}, externalOAuthError(err)
	}
	if lb != nil && au.FlowID != "" {
		lb.Engine, lb.Scope, lb.FlowID = a.oauth, scope, au.FlowID
		lb.Opener = req.OpenerOrigin
		a.oauth.AttachLoopback(au.FlowID, lb)
		go lb.Serve()
	}
	out := gateway.ProvidersOAuthStarted{
		AuthorizationURL: au.URL, RedirectURI: au.RedirectURI, ExpiresAt: au.ExpiresAt,
		FlowID: au.FlowID, PollIntervalMS: au.PollIntervalMS,
		Consent: gateway.ProvidersOAuthConsent{
			ProviderName: au.Consent.ProviderName, Account: au.Consent.Account, DataAccess: au.Consent.DataAccess,
			Reauthorization: au.Consent.Reauthorization, SwitchAccount: au.Consent.SwitchAccount,
		},
	}
	for _, s := range au.Consent.Scopes {
		out.Consent.Scopes = append(out.Consent.Scopes, gateway.ProvidersOAuthScope{Name: s.Name, Description: s.Description, DataAccess: s.DataAccess})
	}
	return out, nil
}

func (a *externalAdmin) ensureAgentHostID() (string, error) {
	path := a.hostIDPath
	if path == "" {
		return newOpaqueHostID()
	}
	if b, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", contracts.New(contracts.ErrUnavailable, "oauth: could not read the SIWC host id file")
	}
	id, err := newOpaqueHostID()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", contracts.New(contracts.ErrUnavailable, "oauth: could not create the SIWC host id directory")
	}
	if err := fsperm.Restrict(filepath.Dir(path), true); err != nil && runtime.GOOS != "windows" {
		return "", contracts.New(contracts.ErrUnavailable, "oauth: could not restrict the SIWC host id directory")
	}
	if err := writeHostIDFile(path, id); err != nil {
		return "", contracts.New(contracts.ErrUnavailable, "oauth: could not write the SIWC host id file")
	}
	return id, nil
}

func writeHostIDFile(path, id string) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := fsperm.Restrict(tmp, false); err != nil && runtime.GOOS != "windows" {
		_ = f.Close()
		return err
	}
	if _, err := f.WriteString(id + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return fsperm.Restrict(path, false)
}

func newOpaqueHostID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", contracts.New(contracts.ErrUnavailable, "oauth: could not mint a stable host id")
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func init() {
	Register(Command{Name: "providers login", Summary: "connect a provider with browser OAuth (loopback)", Run: providersLoginRun})
	Register(Command{Name: "providers credential add-ref", Summary: "register an env or file credential reference", Run: providersCredAddRefRun})
}

func providersLoginRun(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("providers login")
	cfgPath := env.ConfigFlag(fs)
	operator := fs.String("operator", "", "operator principal id")
	application := fs.String("application", "", "application id")
	tenant := fs.String("tenant", "operator-cli", "tenant id")
	label := fs.String("label", "", "connection label")
	forWhom := fs.String("for", "", "openai SIWC: application/principal whose ChatGPT plan is used")
	noBrowser := fs.Bool("no-browser", false, "print the URL only")
	if err := env.ParseFlags(fs, "alixai providers login <provider> [-config f] -operator ID -application APP [-tenant T] [-label L] [-for APP/PRINCIPAL] [-no-browser]", args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return Usagef("providers login needs exactly one provider id")
	}
	provider := pv.ProviderID(fs.Arg(0))
	switch provider {
	case "openai", "gemini", "google-stt", "openrouter":
	default:
		fmt.Fprintln(env.withDefaults().Stderr, "this provider has no browser login; use `alixai providers credential add-ref` or the settings API")
		return Usagef("this provider has no browser login")
	}
	if *operator == "" || *application == "" {
		return Usagef("-operator and -application are required")
	}
	var forPrincipal string
	if provider == "openai" {
		app, principal, err := parseLoginFor(*forWhom)
		if err != nil {
			return err
		}
		if app != *application {
			return Usagef("--for application must match -application")
		}
		forPrincipal = principal
	}
	s, closeFn, err := extWireOpen(ctx, env, *cfgPath, *operator, *application, *tenant)
	if err != nil {
		return err
	}
	defer closeFn()
	if forPrincipal != "" {
		s.scope.PrincipalID = forPrincipal
	}

	pr := oauth.PrepareRequest{ProviderID: provider, Capabilities: []string{"inference"}}
	consent, err := s.admin.oauth.Prepare(ctx, s.scope, pr)
	if err != nil {
		if oauth.IsManualFallback(err) {
			fmt.Fprintln(env.withDefaults().Stderr, "this provider has no browser login; use `alixai providers credential add-ref` or the settings API")
			return Usagef("this provider has no browser login")
		}
		return err
	}
	path := "/oauth/callback"
	if preset, ok := oauth.Preset(provider); ok && preset.LoopbackPath != "" {
		path = preset.LoopbackPath
	}
	lb, redirect, err := s.admin.oauth.BindLoopback(path)
	if err != nil {
		return err
	}
	hostID, err := s.admin.ensureAgentHostID()
	if err != nil {
		_ = lb.Close()
		return err
	}
	au, err := s.admin.oauth.Begin(ctx, s.scope, oauth.BeginRequest{
		PrepareRequest: pr, ConsentDigest: consent.Digest, RedirectURI: redirect, Label: *label,
		Presentation: oauth.PresentationPopup, CallbackMode: oauth.CallbackLoopback,
		SkipOpenerCheck: true, ExtAgentHostID: hostID,
	})
	if err != nil {
		_ = lb.Close()
		return err
	}
	lb.Engine, lb.Scope, lb.FlowID, lb.CLIClose = s.admin.oauth, s.scope, au.FlowID, true
	s.admin.oauth.AttachLoopback(au.FlowID, lb)
	go lb.Serve()

	fmt.Fprintln(env.withDefaults().Stdout, au.URL)
	if !*noBrowser {
		if err := openSystemBrowser(au.URL); err != nil {
			fmt.Fprintln(env.withDefaults().Stderr, "could not open a browser; open the URL printed above")
		}
	}
	deadline := time.Now().Add(oauth.DefaultStateTTL)
	for time.Now().Before(deadline) {
		v, err := s.admin.oauth.FlowGet(ctx, s.scope, au.FlowID)
		if err != nil {
			return err
		}
		switch v.Status {
		case oauth.FlowCompleted:
			if v.Connection != nil {
				if prof, err := s.admin.profileFor(ctx, s.scope, provider); err == nil {
					s.admin.bindIfFree(ctx, s.scope, prof, v.Connection.Ref.RefID)
				}
			}
			st, err := s.admin.status(ctx, s.scope, mustOrEmptyProfile(ctx, s, provider))
			if err != nil {
				return nil
			}
			enc, _ := json.Marshal(st)
			fmt.Fprintln(env.withDefaults().Stdout, string(enc))
			return nil
		case oauth.FlowFailed, oauth.FlowExpired, oauth.FlowCancelled:
			return errors.New("oauth login did not complete")
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("oauth login timed out")
}

func mustOrEmptyProfile(ctx context.Context, s *extWireSession, provider pv.ProviderID) pv.ProviderProfile {
	p, err := s.admin.profileFor(ctx, s.scope, provider)
	if err != nil {
		return pv.ProviderProfile{ProviderID: provider}
	}
	return p
}

func providersCredAddRefRun(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("providers credential add-ref")
	cfgPath := env.ConfigFlag(fs)
	operator := fs.String("operator", "", "")
	application := fs.String("application", "", "")
	tenant := fs.String("tenant", "operator-cli", "")
	provider := fs.String("provider", "", "")
	label := fs.String("label", "", "")
	envName := fs.String("env", "", "")
	fileName := fs.String("file", "", "")
	if err := env.ParseFlags(fs, "alixai providers credential add-ref [-config f] -operator ID -application APP [-tenant T] -provider ID -label L (-env NAME | -file NAME)", args); err != nil {
		return err
	}
	if *operator == "" || *application == "" || *provider == "" || *label == "" {
		return Usagef("-operator, -application, -provider and -label are required")
	}
	ref := externalCredRefConfig{Provider: *provider, Label: *label, Env: *envName, File: *fileName}
	if err := ref.Validate(); err != nil {
		return err
	}
	s, closeFn, err := extWireOpen(ctx, env, *cfgPath, *operator, *application, *tenant)
	if err != nil {
		return err
	}
	defer closeFn()
	backend := credentials.BackendEnv
	locator := *envName
	if *fileName != "" {
		backend = credentials.BackendFile
		locator = *fileName
	}
	got, err := s.admin.creds.AddExternalRef(ctx, s.scope, credentials.ExternalRefRequest{
		ProviderID: pv.ProviderID(*provider), Label: *label, Backend: backend, Locator: locator,
	})
	if err != nil {
		return err
	}
	if prof, err := s.admin.profileFor(ctx, s.scope, pv.ProviderID(*provider)); err == nil {
		s.admin.bindIfFree(ctx, s.scope, prof, got.RefID)
	}
	enc, _ := json.Marshal(got)
	fmt.Fprintln(env.withDefaults().Stdout, string(enc))
	return nil
}

// parseLoginFor parses --for "<application id>/<principal id>" for openai SIWC.
func parseLoginFor(raw string) (app, principal string, err error) {
	if raw == "" {
		return "", "", Usagef("--for <application>/<principal> is required for openai (the person whose ChatGPT plan is used)")
	}
	i := strings.IndexByte(raw, '/')
	if i <= 0 || i == len(raw)-1 || strings.Contains(raw[i+1:], "/") {
		return "", "", Usagef("--for must be <application id>/<principal id>")
	}
	app, principal = raw[:i], raw[i+1:]
	if !externalIDRE.MatchString(app) || !externalIDRE.MatchString(principal) {
		return "", "", Usagef("--for application and principal must be identifiers")
	}
	return app, principal, nil
}

// openSystemBrowser opens rawURL in the OS browser without a shell string. Tests replace it.
var openSystemBrowser = openSystemBrowserOS

func openSystemBrowserOS(rawURL string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL).Start()
	case "darwin":
		return exec.Command("open", rawURL).Start()
	default:
		return exec.Command("xdg-open", rawURL).Start()
	}
}

// bindCredentialRefsAtStartup registers external_providers.credentials entries.
func bindCredentialRefsAtStartup(ctx context.Context, a *externalAdmin, scope contracts.Scope, refs []externalCredRefConfig) error {
	for _, r := range refs {
		if err := r.Validate(); err != nil {
			return err
		}
		backend := credentials.BackendEnv
		locator := r.Env
		if r.File != "" {
			backend, locator = credentials.BackendFile, r.File
		}
		got, err := a.creds.AddExternalRef(ctx, scope, credentials.ExternalRefRequest{
			ProviderID: pv.ProviderID(r.Provider), Label: r.Label, Backend: backend, Locator: locator,
		})
		if err != nil {
			return fmt.Errorf("external_providers.credentials %s: %w", r.Provider, err)
		}
		if prof, err := a.profileFor(ctx, scope, pv.ProviderID(r.Provider)); err == nil {
			a.bindIfFree(ctx, scope, prof, got.RefID)
		}
	}
	return nil
}
