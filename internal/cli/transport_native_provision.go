package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	nativetransport "github.com/buxee/alix-ai/adapters/native"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/integrationaudit/probes"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// The provisioning half of the native SDK transport (transport_native.go): the bridge that
// lets the integration audit's runtime probes (master plan 4.5 checks 2, 6, 8, 9 and 10,
// internal/integrationaudit/probes) reach the disposable tenants of an application that is
// reached through a native bridge. It is the counterpart of the MCP and OpenAPI bridges
// (transport_mcp_provision.go, transport_openapi_provision.go) and holds the same line: the
// audit creates, seeds and removes its own tenants and nobody else's.
//
// # Why a bridge, and why it is small
//
// The probes create their tenants through a probes.Provisioner. The adapter the audit holds
// is the adapters.Wrap wrapper around the native adapter and exposes only the adapter
// contract. The three operations of fixtures.provisioning (create_tenant, seed, cleanup) are
// approved by the signed adapter configuration (management.provisioning), declared by the
// application's bridge with HandleManagement and mapped by no capability, so a model can
// never reach them. The raw adapter has one method for them, Adapter.Manage, and this file is
// the composition root's half of it: it unwraps the raw adapter the transport built (the
// audit's own, never a second one), maps the audit's tenants to Manage calls and reads the
// replies.
//
// Manage already does everything a client needs: the connection is the adapter's (the egress
// dialer, the loopback address or the owner-only socket of the endpoint), both ends prove they
// hold the application key before anything is sent, every frame carries the session MAC, a
// reply is cut off at the adapter's response limit, nothing is retried and every error is
// typed and carries no text the application chose. No identity travels: a provisioning
// operation acts on the tenants its arguments name. None of that is repeated here.
//
// # The wire contract
//
// The package's fixtures.provisioning names three native methods of one native endpoint
// (native_method), every suite the same three, and the endpoint's adapter configuration
// approves all of them. The arguments of a call are a JSON object (numbers arrive at the
// bridge as float64):
//
//	create_tenant   tenant_id, workspaces: [{workspace_id}],
//	                principals: [{principal_id, workspace_id, roles, service?}]
//	seed            tenant_id, records: [{ref, entity_type, workspace_id,
//	                principal_id?, ...members of the seed data file}]
//	cleanup         tenant_id
//
// and each answers with {status: "ok", tenant_id, ...}: seed adds records
// [{ref, entity_id, ...}], cleanup adds existed (bool) and remaining ({kind: count}), which
// must be stated for a cleanup to count. A refusal is {status: "rejected", errors: [{code,
// ...}]}; its code is kept when it is one of the platform's error codes, and nothing else the
// application says is ever put in an error, a log or a report. docs/integration/
// application-package.md section 12 describes the contract for application authors. A reply
// that names another tenant than the one asked about, a seed that omits, adds or repeats a
// record, and a cleanup that does not say whether the tenant existed and what remains are
// refused.
//
// The seed data file (fixtures.provisioning.seed_data, below tests/ in the installed package)
// is read exactly as the other bridges read it: the entry whose ref a seeded record has
// contributes every member the application defines except the logical ones the audit owns.
// workspace is replaced by the workspace identifier the audit generated, and principal (the
// logical reference of a fixture principal) by the identifier generated for it, found among
// the principals the tenant was created with by their reference (probes.Principal.Ref), never
// by how the identifier is spelled. A principal that cannot be found, a ref the file lists
// twice and an entity type that differs from the suite's fail the seed before anything is
// sent. The records go to the application in the order the audit lists them, so a record can
// link to an earlier one of the same call. The reader is this file's own copy: the bridges do
// not share code that either may change on its own.
//
// # Only the audit's own tenants
//
// A provisioner acts on tenants whose creation it asked for in this run. Seed needs a tenant it
// created (the application said ok), Cleanup a tenant it attempted to create (a lost reply may
// still have created one), and any other tenant is refused before anything is sent
// (permission_denied): an application that does not guard its own tenants is never given the
// chance to be asked. A create the application refused (the tenant exists) is not adopted, so
// cleaning up after it can never remove somebody else's tenant; a create that failed without
// proof that nothing was sent (no reply, an unusable reply, a host error) is adopted for
// cleanup and never seeded. Every method can be called again: a second Cleanup of a removed
// tenant reaches the application, which answers existed false with nothing remaining, a second
// create of a tenant that exists fails with a typed conflict, and nothing is held open between
// calls.
//
// # When there is no bridge
//
// The source answers nil, nil for every audit that is not of this transport (an input without a
// package or an adapter, an adapter this transport did not build, a package of another
// transport kind) and for a package whose test suites name no native provisioning methods,
// which leaves the probes exactly as they were (every capability unverified, critical for a
// consequential one). For an audit of this transport whose suites do name them, a declaration
// that cannot be run is an error that is this transport's own, so the probes record why they
// could not provision (and still probe nothing): methods that are not three distinct native
// methods of one native endpoint, methods the adapter configuration does not approve in
// management.provisioning, or an install directory that cannot be found. An error from a source
// stops the probes for every transport, so no other kind of audit ever gets one from here.
//
// # The install directory
//
// The adapter configuration and the seed data file are read from the install directory of the
// package. The audit's input carries the install record, whose path is relative to the data
// directory, and not the directory. The transport remembers where it opened each package (a
// wrapper around the native factory that records TransportRequest.Dir by application and
// manifest hash), so a harness that runs the audit with a configuration registry of its own
// finds the package where its own data directory put it. The configuration's data directory
// (nativeProvisionDataDir, config.Core()) joined with the record's path is only the fallback,
// for an adapter this transport did not open through the process's registry.
//
// # The step-up completer
//
// The optional probes.StepUpCompleter is implemented when the application offers a way: a fourth
// management method of the endpoint with the conventional name approve_step_up, that the adapter
// configuration approves in management.provisioning like the three, that the application's bridge
// declares with HandleManagement and that no capability maps. It is test support, the audit's way
// to complete the step-up of a level 3 preparation in a disposable tenant as the user the action
// was prepared for does in the application's own authenticated interface (the audit holds no
// credential and learns none), and the bridge declares it only while the application runs for an
// audit. Its arguments are tenant_id, workspace_id, principal_id, capability_id, idempotency_key
// and values, the text and identifier facts of the application's own Prepare reply by field, from which the
// application takes what it told the user to quote (a reference). It answers {status: "ok",
// tenant_id, approved: true}; a refusal is the same {status: "rejected", errors} the other methods
// answer with. Only a preparation of a disposable tenant this provisioner created, for one of its
// principals, is ever sent. Without the method in management.provisioning the provisioner does not
// implement the interface and the probes record the completion as unverified.
//
// # Limits
//
// The optional probes.RecordChanger and probes.FaultInjector are not implemented: the package
// format declares no operation for either, so the probes that need them stay unverified with
// their own reasons.
func init() {
	probes.SetProvisionerSource(nativeProvisionSource)
	nativeProvisionRecordInstallDirs(transportsDefault)
}

const (
	// nativeProvisionMaxSeedFileBytes bounds a seed data file.
	nativeProvisionMaxSeedFileBytes = 1 << 20
	// nativeProvisionMaxRecords bounds the records of one seed.
	nativeProvisionMaxRecords = 1024
	// nativeProvisionMaxConfigBytes bounds an adapter configuration file.
	nativeProvisionMaxConfigBytes = 1 << 20
	// nativeProvisionMaxStepUpFacts and nativeProvisionMaxStepUpFactBytes bound what a step-up sends back.
	nativeProvisionMaxStepUpFacts     = 64
	nativeProvisionMaxStepUpFactBytes = 2048
	// nativeProvisionApproveMethod is the management method of the step-up completion. The package
	// format declares no method for it, so the name is the convention.
	nativeProvisionApproveMethod = "approve_step_up"

	// The reasons put in Details["reason"] of the errors raised here.
	nativeProvisionReasonRejected = "rejected"
	nativeProvisionReasonNotOurs  = "tenant_not_created_by_this_audit"
)

var (
	// nativeProvisionMethodRE is what a native method name may look like
	// (adapters/native: ^[A-Za-z_][A-Za-z0-9_.-]{0,127}$).
	nativeProvisionMethodRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
	// nativeProvisionIDRE is what an identifier handed to the application may look like.
	nativeProvisionIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
	// nativeProvisionEntityIDRE is what the identifier of a seeded record may look like: the
	// application chose it.
	nativeProvisionEntityIDRE = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,256}$`)

	// nativeProvisionDataDir returns the data directory the install records are relative to,
	// for an adapter whose install directory the transport did not record. The audit's input
	// carries the record and not the directory; the process's configuration is where the
	// transport registry takes it from. Tests replace it.
	nativeProvisionDataDir = func() string { return config.Core().DataDir.String() }

	// nativeProvisionInstallDirs maps a package (nativeProvisionPackageKey) to the directory
	// the transport opened it from.
	nativeProvisionInstallDirs sync.Map

	// nativeProvisionReserved are the members of a seed data entry the audit owns.
	nativeProvisionReserved = map[string]bool{
		"ref": true, "entity_type": true, "workspace": true, "workspace_id": true,
		"principal": true, "principal_id": true, "tenant": true, "tenant_id": true,
	}

	// nativeProvisionNotSent are the reasons of an adapter error that prove no byte of the
	// request left: the call was refused before a connection was used, the peer did not
	// authenticate, or the bridge answered before any handler ran. Any other failure may
	// have been delivered, and guessing "sent" only costs a cleanup that finds nothing.
	nativeProvisionNotSent = []string{
		nativetransport.ReasonOperationNotMapped, nativetransport.ReasonOperationNotOffered,
		nativetransport.ReasonPeerAuthFailed, nativetransport.ReasonPeerRefused, nativetransport.ReasonSocketPermissions,
		nativetransport.ReasonRequestTooLarge, nativetransport.ReasonAdapterClosed,
	}
)

// nativeProvisionPackageKey names an installed package.
type nativeProvisionPackageKey struct{ application, manifestHash string }

// nativeProvisionRecordInstallDirs wraps the native factory of a transport registry so that
// the directory every package is opened from is remembered. It must run after the native
// transport registered itself (transport_native.go sorts before this file, so its init runs
// first); a registry without the factory is a programming error found at start, not an audit
// that quietly cannot read an adapter configuration.
func nativeProvisionRecordInstallDirs(reg *transportsRegistry) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	open, ok := reg.byKind[TransportKindNative]
	if !ok {
		panic("cli: the native provisioning bridge is initialised before the native transport registered itself")
	}
	reg.byKind[TransportKindNative] = func(ctx context.Context, req TransportRequest) (adapters.AppAdapter, error) {
		ad, err := open(ctx, req)
		if err == nil && req.Dir != "" {
			if dir, aerr := filepath.Abs(req.Dir); aerr == nil {
				nativeProvisionInstallDirs.Store(nativeProvisionPackageKey{req.Record.ApplicationID, req.Record.ManifestHash}, dir)
			}
		}
		return ad, err
	}
}

// nativeProvisionInstallDir returns the absolute install directory of the package of an
// install record, or "" when it is not known.
func nativeProvisionInstallDir(rec apppkg.Record) string {
	if dir, ok := nativeProvisionInstallDirs.Load(nativeProvisionPackageKey{rec.ApplicationID, rec.ManifestHash}); ok {
		// a package that was opened from a directory that is gone (a data directory that moved) is not there to read
		if fi, err := os.Stat(dir.(string)); err == nil && fi.IsDir() {
			return dir.(string)
		}
	}
	dataDir := nativeProvisionDataDir()
	if dataDir == "" || rec.Path == "" {
		return ""
	}
	dir, err := filepath.Abs(filepath.Join(dataDir, filepath.FromSlash(rec.Path)))
	if err != nil {
		return ""
	}
	return dir
}

// nativeProvisionMethods are the three native methods the package names, on one endpoint.
type nativeProvisionMethods struct {
	endpoint string
	create   string
	seed     string
	cleanup  string
}

func (m nativeProvisionMethods) all() []string { return []string{m.create, m.seed, m.cleanup} }

func nativeProvisionError(code contracts.ErrCode, msg, reason string) *contracts.Error {
	e := contracts.New(code, msg).WithDetails("transport", nativetransport.Transport)
	if reason != "" {
		e.WithDetails("reason", reason)
	}
	return e
}

func nativeProvisionInvalid(msg string) error {
	return nativeProvisionError(contracts.ErrValidationFailed, msg, "")
}

func nativeProvisionReasonOf(err error) string {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil {
		r, _ := ce.Details["reason"].(string)
		return r
	}
	return ""
}

// nativeProvisionSource is the probes.ProvisionerSource of the native transport.
func nativeProvisionSource(_ context.Context, in *integrationaudit.Input) (probes.Provisioner, error) {
	if in == nil || in.Package == nil || in.Adapter == nil {
		return nil, nil
	}
	closing, ok := in.Adapter.(transportsClosing)
	if !ok {
		return nil, nil
	}
	audited, ok := closing.inner.(*nativetransport.Adapter)
	if !ok {
		return nil, nil
	}
	plan, err := transportsPlanFor(in.Package.Manifest)
	if err != nil || plan.Kind != TransportKindNative {
		return nil, nil
	}
	methods, found, err := nativeProvisionMethodsOf(in.Package)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	dir := nativeProvisionInstallDir(in.Record)
	if dir == "" {
		return nil, nativeProvisionError(contracts.ErrUnavailable, "the install directory of the package is not known, so its adapter configuration cannot be read", "")
	}
	stepUp, err := nativeProvisionCheckApproved(plan, dir, methods)
	if err != nil {
		return nil, err
	}
	p := &nativeProvisionProvisioner{ad: audited, methods: methods, dir: dir, tenants: map[string]*nativeProvisionTenant{}}
	if stepUp {
		return &nativeProvisionWithStepUp{p}, nil
	}
	return p, nil
}

// nativeProvisionCheckApproved reads the adapter configuration of the endpoint the provisioning
// calls name and requires that it approves all three methods in management.provisioning. The
// adapter checks the same on every call; reading it here makes the refusal one typed error with
// nothing sent, instead of a failure of the first call. It also says whether the configuration
// approves the completion of a step-up (nativeProvisionApproveMethod), which is none of the
// three methods of the suite.
func nativeProvisionCheckApproved(plan transportsPlan, dir string, m nativeProvisionMethods) (stepUp bool, err error) {
	var decl *ap.ManifestNativeEndpoint
	for _, te := range plan.Endpoints {
		if v, ok := te.Declaration.Value.(ap.ManifestNativeEndpoint); ok && te.ID == m.endpoint {
			decl = &v
		}
	}
	if decl == nil {
		return false, nativeProvisionInvalid("the provisioning calls name an endpoint that is not a native endpoint of the package")
	}
	raw, err := nativeTransportReadFile(dir, "adapter_config", string(decl.AdapterConfig), nativeProvisionMaxConfigBytes)
	if err != nil {
		return false, nativeProvisionError(contracts.ErrUnavailable, "the adapter configuration of the package cannot be read", "")
	}
	settings, err := nativetransport.ParseAdapterConfig(raw, m.endpoint)
	if err != nil {
		return false, nativeProvisionError(contracts.ErrUnavailable, "the adapter configuration of the package cannot be read", "")
	}
	for _, name := range m.all() {
		if !slices.Contains(settings.Management.Provisioning, name) {
			return false, nativeProvisionInvalid("a provisioning method is not approved by the adapter configuration's management.provisioning")
		}
	}
	return slices.Contains(settings.Management.Provisioning, nativeProvisionApproveMethod) && !slices.Contains(m.all(), nativeProvisionApproveMethod), nil
}

// nativeProvisionMethodsOf reads the provisioning calls of the package's test suites. All
// suites must name the same three distinct native methods of one endpoint (a Provisioner is
// not told which suite it serves); a package without a suite, or whose suites name no native
// method, names none.
func nativeProvisionMethodsOf(pkg *apppkg.Package) (nativeProvisionMethods, bool, error) {
	var out nativeProvisionMethods
	found, unnamed := false, false
	bad := nativeProvisionInvalid("the provisioning calls of the package's test suites are not three distinct native methods of one endpoint")
	for _, suite := range pkg.Tests {
		pv := suite.Fixtures.Provisioning
		calls := []struct {
			endpoint string
			method   *string
		}{
			{string(pv.CreateTenant.Endpoint), (*string)(pv.CreateTenant.NativeMethod)},
			{string(pv.Seed.Endpoint), (*string)(pv.Seed.NativeMethod)},
			{string(pv.Cleanup.Endpoint), (*string)(pv.Cleanup.NativeMethod)},
		}
		named := 0
		for _, c := range calls {
			if c.method != nil {
				named++
			}
		}
		if named == 0 {
			unnamed = true // no native provisioning in this suite
			continue
		}
		got := nativeProvisionMethods{endpoint: calls[0].endpoint}
		for i, c := range calls {
			if c.method == nil || c.endpoint != got.endpoint || !nativeProvisionMethodRE.MatchString(*c.method) {
				return nativeProvisionMethods{}, false, bad
			}
			switch i {
			case 0:
				got.create = *c.method
			case 1:
				got.seed = *c.method
			default:
				got.cleanup = *c.method
			}
		}
		if got.create == got.seed || got.create == got.cleanup || got.seed == got.cleanup {
			return nativeProvisionMethods{}, false, bad
		}
		if !found {
			out, found = got, true
		} else if got != out {
			return nativeProvisionMethods{}, false, bad
		}
	}
	if found && unnamed {
		return nativeProvisionMethods{}, false, bad // suites that disagree about whether there is any
	}
	return out, found, nil
}

// ---- replies ----------------------------------------------------------------

// nativeProvisionReply is what a provisioning method answers: the members this file reads. A
// member the application adds is ignored.
type nativeProvisionReply struct {
	Status string `json:"status"`
	Errors []struct {
		Code string `json:"code"`
	} `json:"errors"`
	TenantID *string `json:"tenant_id"`
	Records  []struct {
		Ref      string `json:"ref"`
		EntityID string `json:"entity_id"`
	} `json:"records"`
	Existed   *bool            `json:"existed"`
	Remaining map[string]int64 `json:"remaining"`
	// the answer of a completed step-up
	Approved *bool `json:"approved"`
}

// nativeProvisionDecode reads the object Manage returned. A member of the wrong type (a
// count that is not a whole number, a status that is not text) makes the reply unusable.
func nativeProvisionDecode(obj map[string]any) (nativeProvisionReply, error) {
	var r nativeProvisionReply
	b, err := json.Marshal(obj)
	if err != nil || json.Unmarshal(b, &r) != nil {
		return nativeProvisionReply{}, nativeProvisionError(contracts.ErrSchemaInvalid, "the reply does not have the shape of a provisioning reply", "")
	}
	return r, nil
}

// check turns the reply's status into an error: rejected keeps the application's error code
// when it is one the platform knows, anything but ok is refused.
func (r nativeProvisionReply) check() error {
	switch r.Status {
	case "ok":
		return nil
	case "rejected":
		code := contracts.ErrValidationFailed
		if len(r.Errors) > 0 && contracts.ErrCode(r.Errors[0].Code).Valid() {
			code = contracts.ErrCode(r.Errors[0].Code)
		}
		return nativeProvisionError(code, "the application refused the provisioning request", nativeProvisionReasonRejected)
	}
	return nativeProvisionError(contracts.ErrSchemaInvalid, "the reply states no usable status", "")
}

// namesOther reports whether the reply fails to name the tenant asked about.
func (r nativeProvisionReply) namesOther(tenantID string) bool {
	return r.TenantID == nil || *r.TenantID != tenantID
}

// ---- the provisioner --------------------------------------------------------

// nativeProvisionTenant is what the provisioner knows of a tenant it asked for.
type nativeProvisionTenant struct {
	// created is true once the application said ok; false for a tenant whose creation is
	// only possible (a reply was lost) and for one that is gone.
	created    bool
	workspaces map[string]bool
	principals []probes.Principal
}

// nativeProvisionProvisioner implements probes.Provisioner over Adapter.Manage.
type nativeProvisionProvisioner struct {
	ad      *nativetransport.Adapter
	methods nativeProvisionMethods
	dir     string

	mu      sync.Mutex
	tenants map[string]*nativeProvisionTenant
}

var _ probes.Provisioner = (*nativeProvisionProvisioner)(nil)

// call sends one provisioning method through the audit's own adapter. sent is true unless the
// error proves that no byte of the request left, so that an error with sent set can be an
// operation that was applied and whose reply was lost.
func (p *nativeProvisionProvisioner) call(ctx context.Context, method string, args map[string]any) (reply nativeProvisionReply, sent bool, err error) {
	obj, err := p.ad.Manage(ctx, p.methods.endpoint, method, args)
	if err != nil {
		return nativeProvisionReply{}, !nativeProvisionNothingSent(err), err
	}
	reply, err = nativeProvisionDecode(obj)
	return reply, true, err
}

func nativeProvisionNothingSent(err error) bool {
	if egress.IsDenied(err) {
		return true
	}
	return slices.Contains(nativeProvisionNotSent, nativeProvisionReasonOf(err))
}

// nativeProvisionCheckTenant refuses a tenant whose identifiers could not be a generated one
// before anything is sent.
func nativeProvisionCheckTenant(t probes.Tenant) error {
	if !nativeProvisionIDRE.MatchString(t.ID) {
		return nativeProvisionInvalid("create_tenant: the tenant identifier is not usable")
	}
	if n := len(t.Workspaces); n == 0 || n > 64 {
		return nativeProvisionInvalid("create_tenant: a tenant needs between 1 and 64 workspaces")
	}
	workspaces := map[string]bool{}
	for _, w := range t.Workspaces {
		if !nativeProvisionIDRE.MatchString(w) || workspaces[w] {
			return nativeProvisionInvalid("create_tenant: a workspace identifier is not usable or is repeated")
		}
		workspaces[w] = true
	}
	if len(t.Principals) > 256 {
		return nativeProvisionInvalid("create_tenant: too many principals")
	}
	seen, refs := map[string]bool{}, map[string]bool{}
	for _, p := range t.Principals {
		if !nativeProvisionIDRE.MatchString(p.ID) || seen[p.ID] || !workspaces[p.WorkspaceID] || len(p.Roles) > 16 {
			return nativeProvisionInvalid("create_tenant: a principal identifier is not usable, is repeated or names a workspace the tenant does not have")
		}
		seen[p.ID] = true
		// A reference names one principal: a repeated one would make a seed ambiguous.
		if p.Ref != "" && (!nativeProvisionIDRE.MatchString(p.Ref) || refs[p.Ref]) {
			return nativeProvisionInvalid("create_tenant: a principal reference is not usable or is repeated")
		}
		refs[p.Ref] = true
		for _, r := range p.Roles {
			if !nativeProvisionIDRE.MatchString(r) {
				return nativeProvisionInvalid("create_tenant: a role is not usable")
			}
		}
	}
	return nil
}

// CreateTenant implements probes.Provisioner.
func (p *nativeProvisionProvisioner) CreateTenant(ctx context.Context, t probes.Tenant) error {
	if err := nativeProvisionCheckTenant(t); err != nil {
		return err
	}
	p.mu.Lock()
	known := p.tenants[t.ID]
	p.mu.Unlock()
	if known != nil && known.created {
		return nativeProvisionError(contracts.ErrConflict, "a disposable tenant with this identifier already exists", "")
	}

	workspaces := make([]map[string]any, 0, len(t.Workspaces))
	ws := map[string]bool{}
	for _, w := range t.Workspaces {
		workspaces = append(workspaces, map[string]any{"workspace_id": w})
		ws[w] = true
	}
	principals := make([]map[string]any, 0, len(t.Principals))
	for _, pr := range t.Principals {
		// the reference is the audit's own name for the principal and never goes to the application
		m := map[string]any{"principal_id": pr.ID, "workspace_id": pr.WorkspaceID, "roles": append([]string{}, pr.Roles...)}
		if pr.Service {
			m["service"] = true
		}
		principals = append(principals, m)
	}
	reply, sent, err := p.call(ctx, p.methods.create, map[string]any{"tenant_id": t.ID, "workspaces": workspaces, "principals": principals})
	if err == nil {
		err = reply.check()
	}
	if err == nil && reply.namesOther(t.ID) {
		err = nativeProvisionError(contracts.ErrSchemaInvalid, "the reply is not for the tenant asked for", "")
	}
	st := &nativeProvisionTenant{created: err == nil, workspaces: ws, principals: slices.Clone(t.Principals)}
	switch {
	case err == nil:
		p.remember(t.ID, st)
	case sent && nativeProvisionReasonOf(err) != nativeProvisionReasonRejected:
		// The request may have been applied: the tenant is ours to clean up, and not yet
		// ours to seed.
		p.remember(t.ID, st)
	}
	return err
}

func (p *nativeProvisionProvisioner) remember(id string, st *nativeProvisionTenant) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tenants[id] = st
}

// owned returns the tenant if this provisioner asked for it. needCreated also requires that
// the application said it was created.
func (p *nativeProvisionProvisioner) owned(id string, needCreated bool) (*nativeProvisionTenant, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.tenants[id]
	if st == nil || (needCreated && !st.created) {
		return nil, nativeProvisionError(contracts.ErrPermissionDenied,
			"provisioning only acts on the disposable tenants this audit created", nativeProvisionReasonNotOurs)
	}
	return st, nil
}

// Seed implements probes.Provisioner.
func (p *nativeProvisionProvisioner) Seed(ctx context.Context, s probes.Seed) (probes.SeedResult, error) {
	st, err := p.owned(s.TenantID, true)
	if err != nil {
		return probes.SeedResult{}, err
	}
	records, err := p.seedRecords(s, st)
	if err != nil {
		return probes.SeedResult{}, err
	}
	reply, _, err := p.call(ctx, p.methods.seed, map[string]any{"tenant_id": s.TenantID, "records": records})
	if err == nil {
		err = reply.check()
	}
	if err != nil {
		return probes.SeedResult{}, err
	}
	if reply.namesOther(s.TenantID) {
		return probes.SeedResult{}, nativeProvisionError(contracts.ErrSchemaInvalid, "the reply is not for the tenant asked for", "")
	}
	asked := map[string]bool{}
	for _, r := range s.Records {
		asked[r.Ref] = true
	}
	res := probes.SeedResult{Entities: map[string]string{}}
	for _, r := range reply.Records {
		if _, dup := res.Entities[r.Ref]; dup || !asked[r.Ref] || !nativeProvisionEntityIDRE.MatchString(r.EntityID) {
			return probes.SeedResult{}, nativeProvisionError(contracts.ErrSchemaInvalid,
				"the reply holds a record that was not asked for or is repeated, or one without an identifier", "")
		}
		res.Entities[r.Ref] = r.EntityID
	}
	if len(res.Entities) != len(asked) {
		return probes.SeedResult{}, nativeProvisionError(contracts.ErrSchemaInvalid, "the reply gives no identifier for a record that was asked for", "")
	}
	return res, nil
}

// seedRecords builds the records of a seed from the audit's records and the seed data file, in
// the audit's order. The audit's own identifiers always win over the file.
func (p *nativeProvisionProvisioner) seedRecords(s probes.Seed, st *nativeProvisionTenant) ([]map[string]any, error) {
	if n := len(s.Records); n == 0 || n > nativeProvisionMaxRecords {
		return nil, nativeProvisionInvalid("seed: a seed needs between 1 and 1024 records")
	}
	overlay, err := nativeProvisionReadSeedFile(p.dir, s.DataFile)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(s.Records))
	seen := map[string]bool{}
	for _, r := range s.Records {
		if !nativeProvisionIDRE.MatchString(r.Ref) || seen[r.Ref] || r.EntityType == "" || len(r.EntityType) > 64 || !st.workspaces[r.WorkspaceID] {
			return nil, nativeProvisionInvalid("seed: a record has no usable reference or entity type, is repeated, or names a workspace the tenant does not have")
		}
		seen[r.Ref] = true
		rec := map[string]any{"ref": r.Ref, "entity_type": r.EntityType, "workspace_id": r.WorkspaceID}
		if entry, ok := overlay[r.Ref]; ok {
			if raw, has := entry["entity_type"]; has {
				var et string
				if json.Unmarshal(raw, &et) != nil || et != r.EntityType {
					return nil, nativeProvisionInvalid("seed: the seed data file gives a record another entity type than the suite does")
				}
			}
			for k, v := range entry {
				if !nativeProvisionReserved[k] {
					rec[k] = v
				}
			}
			if raw, has := entry["principal"]; has {
				var ref string
				if json.Unmarshal(raw, &ref) != nil {
					return nil, nativeProvisionInvalid("seed: the seed data file names a principal that is not a reference")
				}
				id, found := nativeProvisionPrincipalID(st, ref)
				if !found {
					return nil, nativeProvisionInvalid("seed: the seed data file names a principal the tenant was not created with")
				}
				rec["principal_id"] = id
			}
		}
		out = append(out, rec)
	}
	return out, nil
}

// nativeProvisionPrincipalID finds the identifier the audit generated for a fixture principal
// by the reference the suite gives it (probes.Principal.Ref). How the identifier is spelled is
// the audit's own business and is never relied on here.
func nativeProvisionPrincipalID(st *nativeProvisionTenant, ref string) (string, bool) {
	if ref == "" {
		return "", false
	}
	for _, pr := range st.principals {
		if pr.Ref == ref {
			return pr.ID, true
		}
	}
	return "", false
}

// nativeProvisionReadSeedFile reads the seed data file of the installed package: a JSON object
// whose records array holds one object per record, by ref. The path is the manifest's, a path
// below tests/ with no way out of the install directory; a file that is not a regular file, or
// is larger than the bound, is refused. A number keeps the text the file gives it.
func nativeProvisionReadSeedFile(dir, rel string) (map[string]map[string]json.RawMessage, error) {
	if rel == "" {
		return nil, nil
	}
	if path.Clean(rel) != rel || !strings.HasPrefix(rel, "tests/") || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return nil, nativeProvisionInvalid("seed: the seed data path is not a path below tests/ of the package")
	}
	full := filepath.Join(dir, filepath.FromSlash(rel))
	fi, err := os.Lstat(full)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, nativeProvisionInvalid("seed: the seed data file cannot be read")
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, nativeProvisionInvalid("seed: the seed data file cannot be read")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, nativeProvisionMaxSeedFileBytes+1))
	if err != nil || len(raw) > nativeProvisionMaxSeedFileBytes {
		return nil, nativeProvisionInvalid("seed: the seed data file cannot be read or is too large")
	}
	var doc struct {
		Records []map[string]json.RawMessage `json:"records"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&doc); err != nil {
		return nil, nativeProvisionInvalid("seed: the seed data file is not a JSON object with a records array")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nativeProvisionInvalid("seed: the seed data file holds more than one document")
	}
	out := make(map[string]map[string]json.RawMessage, len(doc.Records))
	for _, e := range doc.Records {
		var ref string
		if json.Unmarshal(e["ref"], &ref) != nil || !nativeProvisionIDRE.MatchString(ref) {
			return nil, nativeProvisionInvalid("seed: a record of the seed data file has no usable ref")
		}
		if _, dup := out[ref]; dup {
			return nil, nativeProvisionInvalid("seed: the seed data file lists a ref twice")
		}
		out[ref] = e
	}
	return out, nil
}

// Cleanup implements probes.Provisioner.
func (p *nativeProvisionProvisioner) Cleanup(ctx context.Context, tenantID string) (probes.CleanupReport, error) {
	st, err := p.owned(tenantID, false)
	if err != nil {
		return probes.CleanupReport{}, err
	}
	reply, _, err := p.call(ctx, p.methods.cleanup, map[string]any{"tenant_id": tenantID})
	if err == nil {
		err = reply.check()
	}
	if err != nil {
		return probes.CleanupReport{}, err
	}
	// A cleanup that does not say whether the tenant existed and what is left proves nothing,
	// so it does not count. A tenant that is already gone is the application's answer
	// existed false with nothing remaining, and is reported as that.
	if reply.namesOther(tenantID) || reply.Existed == nil || reply.Remaining == nil {
		return probes.CleanupReport{}, nativeProvisionError(contracts.ErrSchemaInvalid, "the cleanup reply does not state whether the tenant existed and what remains", "")
	}
	rep := probes.CleanupReport{Existed: *reply.Existed, Remaining: make(map[string]int, len(reply.Remaining))}
	for kind, n := range reply.Remaining {
		if n < 0 {
			return probes.CleanupReport{}, nativeProvisionError(contracts.ErrSchemaInvalid, "the cleanup reply reports a negative count", "")
		}
		rep.Remaining[kind] = int(min(n, 1<<31))
	}
	if rep.Left() == 0 {
		// Nothing more is seeded into it. The tenant stays known, so that a repeated
		// cleanup is still ours to send and the same identifiers can be created again.
		p.mu.Lock()
		st.created = false
		p.mu.Unlock()
	}
	return rep, nil
}

// ---- the step-up completer --------------------------------------------------

// nativeProvisionWithStepUp is the provisioner of an application whose adapter configuration
// approves the completion of a step-up (nativeProvisionApproveMethod).
type nativeProvisionWithStepUp struct{ *nativeProvisionProvisioner }

var _ probes.StepUpCompleter = (*nativeProvisionWithStepUp)(nil)

// CompleteStepUp implements probes.StepUpCompleter.
func (p *nativeProvisionWithStepUp) CompleteStepUp(ctx context.Context, s probes.StepUp) error {
	return p.completeStepUp(ctx, s)
}

// nativeProvisionStepUpFacts are the text and identifier facts of the application's own Prepare
// reply, by field: what the application stated and where it finds the reference it told the user
// to quote. Only those two kinds, and only so many, so that what is sent back is bounded.
func nativeProvisionStepUpFacts(res act.DeterministicResult) map[string]any {
	out := map[string]any{}
	for _, f := range res.Values {
		var text string
		switch v := f.Value.Value.(type) {
		case act.TextValue:
			text = v.Value
		case act.IdentifierValue:
			text = string(v.Value)
		default:
			continue
		}
		if len(out) >= nativeProvisionMaxStepUpFacts || len(text) > nativeProvisionMaxStepUpFactBytes {
			continue
		}
		out[string(f.Field)] = text
	}
	return out
}

// completeStepUp asks the application to complete the step-up of a preparation of a disposable
// tenant this provisioner created, for a principal of that tenant, as that principal does in the
// application's own interface.
func (p *nativeProvisionProvisioner) completeStepUp(ctx context.Context, s probes.StepUp) error {
	st, err := p.owned(s.Scope.TenantID, true)
	if err != nil {
		return err
	}
	var principal *probes.Principal
	for i := range st.principals {
		if st.principals[i].ID == s.Scope.PrincipalID {
			principal = &st.principals[i]
		}
	}
	switch {
	case s.Prepared.Confirmation.Mode != act.ConfirmationRequirementModeStepUp:
		return nativeProvisionInvalid("approve_step_up: the preparation does not need a step-up")
	case principal == nil || principal.WorkspaceID != s.Scope.WorkspaceID || !st.workspaces[s.Scope.WorkspaceID]:
		return nativeProvisionInvalid("approve_step_up: the principal is not one of the tenant's, or not in that workspace")
	case !nativeProvisionIDRE.MatchString(s.CapabilityID) || !nativeProvisionEntityIDRE.MatchString(string(s.Prepared.IdempotencyKey)):
		return nativeProvisionInvalid("approve_step_up: the capability or the idempotency key is not usable")
	}
	reply, _, err := p.call(ctx, nativeProvisionApproveMethod, map[string]any{
		"tenant_id": s.Scope.TenantID, "workspace_id": s.Scope.WorkspaceID, "principal_id": s.Scope.PrincipalID,
		"capability_id": s.CapabilityID, "idempotency_key": string(s.Prepared.IdempotencyKey), "values": nativeProvisionStepUpFacts(s.Result),
	})
	if err == nil {
		err = reply.check()
	}
	if err != nil {
		return err
	}
	// An application that does not say it completed the step-up asked for, in the tenant asked
	// for, has shown nothing.
	if reply.namesOther(s.Scope.TenantID) || reply.Approved == nil || !*reply.Approved {
		return nativeProvisionError(contracts.ErrSchemaInvalid, "the reply does not say that the step-up was completed", "")
	}
	return nil
}
