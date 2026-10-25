package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	openapitransport "github.com/buxee/alix-ai/adapters/openapi"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/integrationaudit/probes"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// The provisioning half of the OpenAPI/HTTP transport (transport_openapi.go): the
// bridge that lets the integration audit's runtime probes (master plan 4.5 checks 2,
// 6, 8, 9 and 10, internal/integrationaudit/probes) reach the disposable tenants of an
// application that is described by an OpenAPI document. It is the counterpart of the
// MCP bridge (transport_mcp_provision.go) and holds the same line: the audit creates,
// seeds and removes its own tenants and nobody else's.
//
// # Why a bridge, and why it is small
//
// The probes create their tenants through a probes.Provisioner. The adapter the audit
// holds is the adapters.Wrap wrapper around the OpenAPI adapter and exposes only the
// adapter contract, and the three operations of fixtures.provisioning are approved by
// the signed adapter configuration (management.provisioning) and mapped by no
// capability, so a model can never reach them. The raw adapter has one method for
// them, Adapter.Manage, and this file is the composition root's half of it: it
// unwraps the raw adapter the transport built (the audit's own, never a second one),
// maps the audit's tenants to Manage calls and reads the replies.
//
// Manage already does what the MCP bridge needed a client of its own for. It goes
// through the adapter's egress dialer (the endpoint's own host, port and scheme and
// nothing else, no proxy, every redirect refused), its credential, its size bounds
// and its deadlines; it checks the request and the reply against the OpenAPI document
// of the package, which is signed with it and hash-pinned when contract_sha256 is
// declared; it sends no identity; it never retries; and every error it returns is
// typed and carries no text the application chose. None of that is repeated here, so
// a provisioning call is held to exactly what any call of the adapter is held to.
//
// # The wire contract
//
// The package's fixtures.provisioning names three operations of one openapi endpoint
// (openapi_operation_id), every suite the same three, and the endpoint's adapter
// configuration approves all of them in management.provisioning. The members of a
// call are the arguments of Manage: those the operation declares as parameters (a
// path parameter tenant_id in DELETE /tenants/{tenant_id}, say) travel there and the
// rest are members of the JSON request body.
//
//	create_tenant   tenant_id, workspaces: [{workspace_id}],
//	                principals: [{principal_id, workspace_id, roles, service?}]
//	seed            tenant_id, records: [{ref, entity_type, workspace_id,
//	                principal_id?, ...members of the seed data file}]
//	cleanup         tenant_id
//
// What a reply means is carried by its HTTP status, as REST does, and not by a
// member that says it again:
//
//   - 2xx is success. A reply that carries a status member must say "ok"; one that
//     says "rejected" is a refusal (below); any other value is refused as unusable.
//     A reply that names a tenant_id must name the one asked about; a reply that
//     names none is accepted (a DELETE answers for the tenant of its path).
//   - create_tenant: 2xx means the tenant exists now. 409 is a conflict: the tenant
//     exists and is not adopted.
//   - seed: 2xx carries records [{ref, entity_id}], one per record asked for, each
//     once, with a non-empty identifier. A reply that omits a record, adds one or
//     repeats one is refused.
//   - cleanup: 2xx carries existed (a boolean) and remaining (an object of
//     non-negative counts by kind of record, {} or all zeros when nothing is left);
//     both must be present or the cleanup does not count. 404 means the tenant is
//     not there: the cleanup reports Existed false with nothing remaining, whatever
//     the body says, unless the body says the tenant existed or something remains,
//     which contradicts the status and is refused.
//   - Any other status is a refusal: the application's error code (errors[0].code,
//     kept when it is one of the platform's error codes), else the code of the status
//     (400, 404 and 422 validation_failed, 401 and 403 permission_denied, 409
//     conflict, 429 rate_limited, 5xx unavailable). Nothing else the application
//     says is ever put in an error, a log or a report.
//
// The document must describe every status the application can answer with for the
// operation, with a JSON body, or Manage refuses the reply as it would any call's.
// docs/integration/application-package.md section 12 describes the contract for
// application authors.
//
// The seed data file (fixtures.provisioning.seed_data, below tests/ in the installed
// package) is read exactly as the MCP bridge reads it: the entry whose ref a seeded
// record has contributes every member the application defines except the logical ones
// the audit owns. workspace is replaced by the workspace identifier the audit
// generated, and principal (the logical reference of a fixture principal) by the
// identifier generated for it, found among the principals the tenant was created with
// by their reference (probes.Principal.Ref), never by how the identifier is spelled. A
// principal that cannot be found, a ref the file lists twice and an entity type that
// differs from the suite's fail the seed before anything is sent. The reader is this
// file's own copy: the two bridges do not share code that either may change on its own.
//
// # Only the audit's own tenants
//
// A provisioner acts on tenants whose creation it asked for in this run. Seed needs a
// tenant it created (the application said 2xx), Cleanup a tenant it attempted to
// create (a lost reply may still have created one), and any other tenant is refused
// before anything is sent (permission_denied): an application that does not guard its
// own tenants is never given the chance to be asked. A create the application refused
// (409: the tenant exists) is not adopted, so cleaning up after it can never remove
// somebody else's tenant; a create that failed without proof that nothing was sent
// (no reply, an unusable reply, a server error) is adopted for cleanup and never
// seeded. Every method can be called again: a second Cleanup of a removed tenant
// reaches the application, gets its 404 and reports Existed false with nothing
// remaining, a second create of a tenant that exists fails with a typed conflict, and
// nothing is held open between calls.
//
// # When there is no bridge
//
// The source answers nil, nil for every audit that is not of this transport (an input
// without a package or an adapter, an adapter this transport did not build, a package of
// another transport kind) and for a package whose test suites name no provisioning calls,
// which leaves the probes exactly as they were (every capability unverified, critical for
// a consequential one). For an audit of this transport whose suites do name them, a
// declaration that cannot be run is an error that is this transport's own, so the probes
// record why they could not provision (and still probe nothing): calls that are not three
// distinct openapi operations of one openapi endpoint, operations the adapter
// configuration does not approve in management.provisioning, or an install directory
// that cannot be found. An error from a source stops the probes for every transport, so
// no other kind of audit ever gets one from here.
//
// # The install directory
//
// The adapter configuration and the seed data file are read from the install directory
// of the package. The audit's input carries the install record, whose path is relative
// to the data directory, and not the directory. The transport remembers where it opened
// each package (a wrapper around the OpenAPI factory that records TransportRequest.Dir
// by application and manifest hash), so a harness that runs the audit with a
// configuration registry of its own finds the package where its own data directory put
// it. The configuration's data directory (openapiProvisionDataDir, config.Core()) joined
// with the record's path is only the fallback, for an adapter this transport did not
// open through the process's registry.
//
// # The step-up completer
//
// The optional probes.StepUpCompleter is implemented when the application offers a way: a
// fourth operation of the endpoint with the conventional operationId approveStepUp, that
// the adapter configuration approves in management.provisioning like the three and that no
// capability maps. It is test support, the audit's way to complete the step-up of a level 3
// preparation in a disposable tenant as the user the action was prepared for does in the
// application's own authenticated interface (the audit holds no credential and learns none),
// and exists only while the application runs for an audit. Its members are the arguments of
// Manage: tenant_id (a path parameter, or a member of the body), workspace_id, principal_id,
// capability_id, idempotency_key, and values, the text and identifier facts of the application's own Prepare
// reply by field, from which the application takes what it told the user to quote (a
// reference). A 2xx reply states approved true (and status ok, and, when it names one, the
// tenant asked about); anything else is the refusal the other operations answer with. Only a
// preparation of a disposable tenant this provisioner created, for one of its principals, is
// ever sent. Without the operation in management.provisioning the provisioner does not
// implement the interface and the probes record the completion as unverified.
//
// # Limits
//
// The optional probes.RecordChanger and probes.FaultInjector are not implemented: the
// package format declares no operation for either, so the probes that need them stay
// unverified with their own reasons. There is no credential service yet (milestone
// X): the adapter sends the credential its provider resolves, and none without one.
func init() {
	probes.SetProvisionerSource(openapiProvisionSource)
	openapiProvisionRecordInstallDirs(transportsDefault)
}

const (
	// openapiProvisionMaxSeedFileBytes bounds a seed data file.
	openapiProvisionMaxSeedFileBytes = 1 << 20
	// openapiProvisionMaxRecords bounds the records of one seed.
	openapiProvisionMaxRecords = 1024
	// openapiProvisionMaxStepUpFacts and openapiProvisionMaxStepUpFactBytes bound what a step-up sends back.
	openapiProvisionMaxStepUpFacts     = 64
	openapiProvisionMaxStepUpFactBytes = 2048
	// openapiProvisionApproveOp is the operationId of the step-up completion. The package format
	// declares no operation for it, so the name is the convention.
	openapiProvisionApproveOp = "approveStepUp"

	// The reasons put in Details["reason"] of the errors raised here.
	openapiProvisionReasonRejected    = "rejected"
	openapiProvisionReasonServerError = "server_error"
	openapiProvisionReasonNotOurs     = "tenant_not_created_by_this_audit"
)

var (
	// openapiProvisionOperationRE is what an operationId of the adapter configuration
	// may look like (adapters/openapi: ^[A-Za-z_][A-Za-z0-9_.-]{0,127}$).
	openapiProvisionOperationRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
	// openapiProvisionIDRE is what an identifier handed to the application may look like.
	openapiProvisionIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
	// openapiProvisionEntityIDRE is what the identifier of a seeded record may look like:
	// the application chose it.
	openapiProvisionEntityIDRE = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,256}$`)

	// openapiProvisionDataDir returns the data directory the install records are
	// relative to, for an adapter whose install directory the transport did not record.
	// The audit's input carries the record and not the directory; the process's
	// configuration is where the transport registry takes it from. Tests replace it.
	openapiProvisionDataDir = func() string { return config.Core().DataDir.String() }

	// openapiProvisionInstallDirs maps a package (openapiProvisionPackageKey) to the
	// directory the transport opened it from.
	openapiProvisionInstallDirs sync.Map

	// openapiProvisionReserved are the members of a seed data entry the audit owns.
	openapiProvisionReserved = map[string]bool{
		"ref": true, "entity_type": true, "workspace": true, "workspace_id": true,
		"principal": true, "principal_id": true, "tenant": true, "tenant_id": true,
	}

	// openapiProvisionNotSent are the reasons of an adapter error that prove no byte of
	// the request left: the request was refused before it was built, or the credential
	// could not be read.
	openapiProvisionNotSent = []string{
		openapitransport.ReasonOperationNotMapped, openapitransport.ReasonRequestViolatesSpec, openapitransport.ReasonFieldNotAccepted,
		openapitransport.ReasonValueNotEncodable, openapitransport.ReasonRequestTooLarge,
		openapitransport.ReasonCredentialUnavailable, openapitransport.ReasonCredentialMalformed,
	}
)

// openapiProvisionPackageKey names an installed package.
type openapiProvisionPackageKey struct{ application, manifestHash string }

// openapiProvisionRecordInstallDirs wraps the OpenAPI factory of a transport registry so
// that the directory every package is opened from is remembered. It must run after the
// OpenAPI transport registered itself (transport_openapi.go sorts before this file, so its
// init runs first); a registry without the factory is a programming error found at start,
// not an audit that quietly cannot read an adapter configuration.
func openapiProvisionRecordInstallDirs(reg *transportsRegistry) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	open, ok := reg.byKind[TransportKindOpenAPI]
	if !ok {
		panic("cli: the OpenAPI provisioning bridge is initialised before the OpenAPI transport registered itself")
	}
	reg.byKind[TransportKindOpenAPI] = func(ctx context.Context, req TransportRequest) (adapters.AppAdapter, error) {
		ad, err := open(ctx, req)
		if err == nil && req.Dir != "" {
			if dir, aerr := filepath.Abs(req.Dir); aerr == nil {
				openapiProvisionInstallDirs.Store(openapiProvisionPackageKey{req.Record.ApplicationID, req.Record.ManifestHash}, dir)
			}
		}
		return ad, err
	}
}

// openapiProvisionInstallDir returns the absolute install directory of the package of an
// install record, or "" when it is not known.
func openapiProvisionInstallDir(rec apppkg.Record) string {
	if dir, ok := openapiProvisionInstallDirs.Load(openapiProvisionPackageKey{rec.ApplicationID, rec.ManifestHash}); ok {
		// a package that was opened from a directory that is gone (a data directory that moved) is not there to read
		if fi, err := os.Stat(dir.(string)); err == nil && fi.IsDir() {
			return dir.(string)
		}
	}
	dataDir := openapiProvisionDataDir()
	if dataDir == "" || rec.Path == "" {
		return ""
	}
	dir, err := filepath.Abs(filepath.Join(dataDir, filepath.FromSlash(rec.Path)))
	if err != nil {
		return ""
	}
	return dir
}

// openapiProvisionOps are the three operations the package names, on one endpoint.
type openapiProvisionOps struct {
	endpoint string
	create   string
	seed     string
	cleanup  string
}

func (o openapiProvisionOps) all() []string { return []string{o.create, o.seed, o.cleanup} }

func openapiProvisionError(code contracts.ErrCode, msg, reason string) *contracts.Error {
	e := contracts.New(code, msg).WithDetails("transport", openapitransport.Transport)
	if reason != "" {
		e.WithDetails("reason", reason)
	}
	return e
}

func openapiProvisionInvalid(msg string) error {
	return openapiProvisionError(contracts.ErrValidationFailed, msg, "")
}

func openapiProvisionReasonOf(err error) string {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil {
		r, _ := ce.Details["reason"].(string)
		return r
	}
	return ""
}

// openapiProvisionSource is the probes.ProvisionerSource of the OpenAPI transport.
func openapiProvisionSource(ctx context.Context, in *integrationaudit.Input) (probes.Provisioner, error) {
	if in == nil || in.Package == nil || in.Adapter == nil {
		return nil, nil
	}
	closing, ok := in.Adapter.(transportsClosing)
	if !ok {
		return nil, nil
	}
	audited, ok := closing.inner.(*openapitransport.Adapter)
	if !ok {
		return nil, nil
	}
	plan, err := transportsPlanFor(in.Package.Manifest)
	if err != nil || plan.Kind != TransportKindOpenAPI {
		return nil, nil
	}
	ops, found, err := openapiProvisionOpsOf(in.Package)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	dir := openapiProvisionInstallDir(in.Record)
	if dir == "" {
		return nil, openapiProvisionError(contracts.ErrUnavailable, "the install directory of the package is not known, so its adapter configuration cannot be read", "")
	}
	appID := in.Record.ApplicationID
	if appID == "" {
		appID = string(in.Package.Manifest.Application.ID)
	}
	cfg, err := openapiTransportConfig(ctx, TransportRequest{
		ApplicationID: appID, Kind: TransportKindOpenAPI, Endpoints: plan.Endpoints, Package: in.Package, Record: in.Record, Dir: dir,
	}, openapiTransportLogger())
	if err != nil {
		return nil, openapiProvisionError(contracts.ErrUnavailable, "the adapter configuration of the package cannot be read", "")
	}
	var ep *openapitransport.Endpoint
	for i := range cfg.Endpoints {
		if cfg.Endpoints[i].ID == ops.endpoint {
			ep = &cfg.Endpoints[i]
		}
	}
	if ep == nil {
		return nil, openapiProvisionInvalid("the provisioning calls name an endpoint that is not an openapi endpoint of the package")
	}
	for _, op := range ops.all() {
		if !slices.Contains(ep.Settings.Management.Provisioning, op) {
			return nil, openapiProvisionInvalid("a provisioning operation is not approved by the adapter configuration's management.provisioning")
		}
	}
	p := &openapiProvisionProvisioner{ad: audited, ops: ops, dir: dir, tenants: map[string]*openapiProvisionTenant{}}
	// The completion of a step-up counts only when the configuration approves it too, and it
	// is none of the three operations of the suite.
	if slices.Contains(ep.Settings.Management.Provisioning, openapiProvisionApproveOp) && !slices.Contains(ops.all(), openapiProvisionApproveOp) {
		return &openapiProvisionWithStepUp{p}, nil
	}
	return p, nil
}

// openapiProvisionOpsOf reads the provisioning calls of the package's test suites. All
// suites must name the same three distinct operations of one endpoint (a Provisioner is
// not told which suite it serves); a package without a suite names none.
func openapiProvisionOpsOf(pkg *apppkg.Package) (openapiProvisionOps, bool, error) {
	var out openapiProvisionOps
	found := false
	bad := openapiProvisionInvalid("the provisioning calls of the package's test suites are not three distinct openapi operations of one endpoint")
	for _, suite := range pkg.Tests {
		pv := suite.Fixtures.Provisioning
		got := openapiProvisionOps{endpoint: string(pv.CreateTenant.Endpoint)}
		for i, call := range []struct {
			endpoint string
			op       *string
		}{
			{string(pv.CreateTenant.Endpoint), (*string)(pv.CreateTenant.OpenAPIOperationID)},
			{string(pv.Seed.Endpoint), (*string)(pv.Seed.OpenAPIOperationID)},
			{string(pv.Cleanup.Endpoint), (*string)(pv.Cleanup.OpenAPIOperationID)},
		} {
			if call.op == nil || call.endpoint != got.endpoint || !openapiProvisionOperationRE.MatchString(*call.op) {
				return openapiProvisionOps{}, false, bad
			}
			switch i {
			case 0:
				got.create = *call.op
			case 1:
				got.seed = *call.op
			default:
				got.cleanup = *call.op
			}
		}
		if got.create == got.seed || got.create == got.cleanup || got.seed == got.cleanup {
			return openapiProvisionOps{}, false, bad
		}
		if !found {
			out, found = got, true
		} else if got != out {
			return openapiProvisionOps{}, false, bad
		}
	}
	return out, found, nil
}

// ---- replies ----------------------------------------------------------------

// openapiProvisionReply is what a provisioning operation answers: the status and the
// members this file reads. A member the application adds is ignored.
type openapiProvisionReply struct {
	status int
	body   struct {
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
	// malformed is true when a member this file reads has the wrong type.
	malformed bool
}

func openapiProvisionDecode(mr openapitransport.ManageReply) openapiProvisionReply {
	r := openapiProvisionReply{status: mr.Status}
	b, err := json.Marshal(mr.Body)
	if err != nil || json.Unmarshal(b, &r.body) != nil {
		r.malformed = true
	}
	return r
}

func (r openapiProvisionReply) success() bool { return r.status >= 200 && r.status < 300 }

// check turns a reply into an error: a 2xx whose members are usable is nil, anything
// else is refused.
func (r openapiProvisionReply) check() error {
	if r.success() {
		switch {
		case r.malformed:
			return openapiProvisionError(contracts.ErrSchemaInvalid, "the reply does not have the shape of a provisioning reply", "")
		case r.body.Status == "" || r.body.Status == "ok":
			return nil
		case r.body.Status != "rejected":
			return openapiProvisionError(contracts.ErrSchemaInvalid, "the reply states no usable status", "")
		}
	}
	return r.refusal()
}

// refusal is the error of a reply that is not a success: the application's error code
// when it is one the platform knows, else the code of the HTTP status.
func (r openapiProvisionReply) refusal() error {
	code := contracts.ErrSchemaInvalid
	switch {
	case r.status == http.StatusConflict:
		code = contracts.ErrConflict
	case r.status == http.StatusUnauthorized || r.status == http.StatusForbidden:
		code = contracts.ErrPermissionDenied
	case r.status == http.StatusBadRequest || r.status == http.StatusNotFound || r.status == http.StatusUnprocessableEntity:
		code = contracts.ErrValidationFailed
	case r.status == http.StatusTooManyRequests:
		code = contracts.ErrRateLimited
	case r.status >= 500:
		code = contracts.ErrUnavailable
	}
	if len(r.body.Errors) > 0 && contracts.ErrCode(r.body.Errors[0].Code).Valid() {
		code = contracts.ErrCode(r.body.Errors[0].Code)
	}
	reason := openapiProvisionReasonRejected
	if r.status >= 500 {
		// the application may have applied the request before it failed
		reason = openapiProvisionReasonServerError
	}
	return openapiProvisionError(code, "the application refused the provisioning request", reason).WithDetails("http_status", r.status)
}

// namesAnother reports whether the reply names a tenant other than the one asked about.
func (r openapiProvisionReply) namesAnother(tenantID string) bool {
	return r.body.TenantID != nil && *r.body.TenantID != tenantID
}

// ---- the provisioner --------------------------------------------------------

// openapiProvisionTenant is what the provisioner knows of a tenant it asked for.
type openapiProvisionTenant struct {
	// created is true once the application said 2xx; false for a tenant whose creation
	// is only possible (a reply was lost) and for one that is gone.
	created    bool
	workspaces map[string]bool
	principals []probes.Principal
}

// openapiProvisionProvisioner implements probes.Provisioner over Adapter.Manage.
type openapiProvisionProvisioner struct {
	ad  *openapitransport.Adapter
	ops openapiProvisionOps
	dir string

	mu      sync.Mutex
	tenants map[string]*openapiProvisionTenant
}

var _ probes.Provisioner = (*openapiProvisionProvisioner)(nil)

// call sends one provisioning operation through the audit's own adapter. sent is true
// unless the error proves that no byte of the request left, so that an error with sent
// set can be an operation that was applied and whose reply was lost.
func (p *openapiProvisionProvisioner) call(ctx context.Context, op string, args map[string]any) (reply openapiProvisionReply, sent bool, err error) {
	mr, err := p.ad.Manage(ctx, p.ops.endpoint, op, args)
	if err != nil {
		return openapiProvisionReply{}, !openapiProvisionNothingSent(err), err
	}
	return openapiProvisionDecode(mr), true, nil
}

func openapiProvisionNothingSent(err error) bool {
	if egress.IsDenied(err) {
		return true
	}
	return slices.Contains(openapiProvisionNotSent, openapiProvisionReasonOf(err))
}

// openapiProvisionCheckTenant refuses a tenant whose identifiers could not be a
// generated one before anything is sent.
func openapiProvisionCheckTenant(t probes.Tenant) error {
	if !openapiProvisionIDRE.MatchString(t.ID) {
		return openapiProvisionInvalid("create_tenant: the tenant identifier is not usable")
	}
	if n := len(t.Workspaces); n == 0 || n > 64 {
		return openapiProvisionInvalid("create_tenant: a tenant needs between 1 and 64 workspaces")
	}
	workspaces := map[string]bool{}
	for _, w := range t.Workspaces {
		if !openapiProvisionIDRE.MatchString(w) || workspaces[w] {
			return openapiProvisionInvalid("create_tenant: a workspace identifier is not usable or is repeated")
		}
		workspaces[w] = true
	}
	if len(t.Principals) > 256 {
		return openapiProvisionInvalid("create_tenant: too many principals")
	}
	seen, refs := map[string]bool{}, map[string]bool{}
	for _, p := range t.Principals {
		if !openapiProvisionIDRE.MatchString(p.ID) || seen[p.ID] || !workspaces[p.WorkspaceID] || len(p.Roles) > 16 {
			return openapiProvisionInvalid("create_tenant: a principal identifier is not usable, is repeated or names a workspace the tenant does not have")
		}
		seen[p.ID] = true
		// A reference names one principal: a repeated one would make a seed ambiguous.
		if p.Ref != "" && (!openapiProvisionIDRE.MatchString(p.Ref) || refs[p.Ref]) {
			return openapiProvisionInvalid("create_tenant: a principal reference is not usable or is repeated")
		}
		refs[p.Ref] = true
		for _, r := range p.Roles {
			if !openapiProvisionIDRE.MatchString(r) {
				return openapiProvisionInvalid("create_tenant: a role is not usable")
			}
		}
	}
	return nil
}

// CreateTenant implements probes.Provisioner.
func (p *openapiProvisionProvisioner) CreateTenant(ctx context.Context, t probes.Tenant) error {
	if err := openapiProvisionCheckTenant(t); err != nil {
		return err
	}
	p.mu.Lock()
	known := p.tenants[t.ID]
	p.mu.Unlock()
	if known != nil && known.created {
		return openapiProvisionError(contracts.ErrConflict, "a disposable tenant with this identifier already exists", "")
	}

	workspaces := make([]map[string]any, 0, len(t.Workspaces))
	ws := map[string]bool{}
	for _, w := range t.Workspaces {
		workspaces = append(workspaces, map[string]any{"workspace_id": w})
		ws[w] = true
	}
	principals := make([]map[string]any, 0, len(t.Principals))
	for _, pr := range t.Principals {
		m := map[string]any{"principal_id": pr.ID, "workspace_id": pr.WorkspaceID, "roles": append([]string{}, pr.Roles...)}
		if pr.Service {
			m["service"] = true
		}
		principals = append(principals, m)
	}
	reply, sent, err := p.call(ctx, p.ops.create, map[string]any{"tenant_id": t.ID, "workspaces": workspaces, "principals": principals})
	if err == nil {
		err = reply.check()
	}
	if err == nil && reply.namesAnother(t.ID) {
		err = openapiProvisionError(contracts.ErrSchemaInvalid, "the reply is for another tenant than the one asked for", "")
	}
	st := &openapiProvisionTenant{created: err == nil, workspaces: ws, principals: slices.Clone(t.Principals)}
	switch {
	case err == nil:
		p.remember(t.ID, st)
	case sent && openapiProvisionReasonOf(err) != openapiProvisionReasonRejected:
		// The request may have been applied: the tenant is ours to clean up, and not
		// yet ours to seed.
		p.remember(t.ID, st)
	}
	return err
}

func (p *openapiProvisionProvisioner) remember(id string, st *openapiProvisionTenant) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tenants[id] = st
}

// owned returns the tenant if this provisioner asked for it. needCreated also requires
// that the application said it was created.
func (p *openapiProvisionProvisioner) owned(id string, needCreated bool) (*openapiProvisionTenant, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.tenants[id]
	if st == nil || (needCreated && !st.created) {
		return nil, openapiProvisionError(contracts.ErrPermissionDenied,
			"provisioning only acts on the disposable tenants this audit created", openapiProvisionReasonNotOurs)
	}
	return st, nil
}

// Seed implements probes.Provisioner.
func (p *openapiProvisionProvisioner) Seed(ctx context.Context, s probes.Seed) (probes.SeedResult, error) {
	st, err := p.owned(s.TenantID, true)
	if err != nil {
		return probes.SeedResult{}, err
	}
	records, err := p.seedRecords(s, st)
	if err != nil {
		return probes.SeedResult{}, err
	}
	reply, _, err := p.call(ctx, p.ops.seed, map[string]any{"tenant_id": s.TenantID, "records": records})
	if err == nil {
		err = reply.check()
	}
	if err != nil {
		return probes.SeedResult{}, err
	}
	if reply.namesAnother(s.TenantID) {
		return probes.SeedResult{}, openapiProvisionError(contracts.ErrSchemaInvalid, "the reply is for another tenant than the one asked for", "")
	}
	asked := map[string]bool{}
	for _, r := range s.Records {
		asked[r.Ref] = true
	}
	res := probes.SeedResult{Entities: map[string]string{}}
	for _, r := range reply.body.Records {
		if _, dup := res.Entities[r.Ref]; dup || !asked[r.Ref] || !openapiProvisionEntityIDRE.MatchString(r.EntityID) {
			return probes.SeedResult{}, openapiProvisionError(contracts.ErrSchemaInvalid,
				"the reply holds a record that was not asked for or is repeated, or one without an identifier", "")
		}
		res.Entities[r.Ref] = r.EntityID
	}
	if len(res.Entities) != len(asked) {
		return probes.SeedResult{}, openapiProvisionError(contracts.ErrSchemaInvalid, "the reply gives no identifier for a record that was asked for", "")
	}
	return res, nil
}

// seedRecords builds the records of a seed from the audit's records and the seed data
// file. The audit's own identifiers always win over the file.
func (p *openapiProvisionProvisioner) seedRecords(s probes.Seed, st *openapiProvisionTenant) ([]map[string]any, error) {
	if n := len(s.Records); n == 0 || n > openapiProvisionMaxRecords {
		return nil, openapiProvisionInvalid("seed: a seed needs between 1 and 1024 records")
	}
	overlay, err := openapiProvisionReadSeedFile(p.dir, s.DataFile)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(s.Records))
	seen := map[string]bool{}
	for _, r := range s.Records {
		if !openapiProvisionIDRE.MatchString(r.Ref) || seen[r.Ref] || r.EntityType == "" || len(r.EntityType) > 64 || !st.workspaces[r.WorkspaceID] {
			return nil, openapiProvisionInvalid("seed: a record has no usable reference or entity type, is repeated, or names a workspace the tenant does not have")
		}
		seen[r.Ref] = true
		rec := map[string]any{"ref": r.Ref, "entity_type": r.EntityType, "workspace_id": r.WorkspaceID}
		if entry, ok := overlay[r.Ref]; ok {
			if raw, has := entry["entity_type"]; has {
				var et string
				if json.Unmarshal(raw, &et) != nil || et != r.EntityType {
					return nil, openapiProvisionInvalid("seed: the seed data file gives a record another entity type than the suite does")
				}
			}
			for k, v := range entry {
				if !openapiProvisionReserved[k] {
					rec[k] = v
				}
			}
			if raw, has := entry["principal"]; has {
				var ref string
				if json.Unmarshal(raw, &ref) != nil {
					return nil, openapiProvisionInvalid("seed: the seed data file names a principal that is not a reference")
				}
				id, found := openapiProvisionPrincipalID(st, ref)
				if !found {
					return nil, openapiProvisionInvalid("seed: the seed data file names a principal the tenant was not created with")
				}
				rec["principal_id"] = id
			}
		}
		out = append(out, rec)
	}
	return out, nil
}

// openapiProvisionPrincipalID finds the identifier the audit generated for a fixture
// principal by the reference the suite gives it (probes.Principal.Ref). How the identifier
// is spelled is the audit's own business and is never relied on here.
func openapiProvisionPrincipalID(st *openapiProvisionTenant, ref string) (string, bool) {
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

// openapiProvisionReadSeedFile reads the seed data file of the installed package: a JSON
// object whose records array holds one object per record, by ref. The path is the
// manifest's, a path below tests/ with no way out of the install directory; a file that
// is not a regular file, or is larger than the bound, is refused.
func openapiProvisionReadSeedFile(dir, rel string) (map[string]map[string]json.RawMessage, error) {
	if rel == "" {
		return nil, nil
	}
	if path.Clean(rel) != rel || !strings.HasPrefix(rel, "tests/") || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return nil, openapiProvisionInvalid("seed: the seed data path is not a path below tests/ of the package")
	}
	full := filepath.Join(dir, filepath.FromSlash(rel))
	fi, err := os.Lstat(full)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, openapiProvisionInvalid("seed: the seed data file cannot be read")
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, openapiProvisionInvalid("seed: the seed data file cannot be read")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, openapiProvisionMaxSeedFileBytes+1))
	if err != nil || len(raw) > openapiProvisionMaxSeedFileBytes {
		return nil, openapiProvisionInvalid("seed: the seed data file cannot be read or is too large")
	}
	var doc struct {
		Records []map[string]json.RawMessage `json:"records"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&doc); err != nil {
		return nil, openapiProvisionInvalid("seed: the seed data file is not a JSON object with a records array")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, openapiProvisionInvalid("seed: the seed data file holds more than one document")
	}
	out := make(map[string]map[string]json.RawMessage, len(doc.Records))
	for _, e := range doc.Records {
		var ref string
		if json.Unmarshal(e["ref"], &ref) != nil || !openapiProvisionIDRE.MatchString(ref) {
			return nil, openapiProvisionInvalid("seed: a record of the seed data file has no usable ref")
		}
		if _, dup := out[ref]; dup {
			return nil, openapiProvisionInvalid("seed: the seed data file lists a ref twice")
		}
		out[ref] = e
	}
	return out, nil
}

// Cleanup implements probes.Provisioner.
func (p *openapiProvisionProvisioner) Cleanup(ctx context.Context, tenantID string) (probes.CleanupReport, error) {
	st, err := p.owned(tenantID, false)
	if err != nil {
		return probes.CleanupReport{}, err
	}
	reply, _, err := p.call(ctx, p.ops.cleanup, map[string]any{"tenant_id": tenantID})
	if err != nil {
		return probes.CleanupReport{}, err
	}
	if reply.status == http.StatusNotFound {
		// There is no such tenant: nothing exists and nothing remains, so the cleanup can
		// be repeated. A body that says otherwise contradicts the status and is refused.
		if (reply.body.Existed != nil && *reply.body.Existed) || openapiProvisionLeft(reply.body.Remaining) > 0 {
			return probes.CleanupReport{}, openapiProvisionError(contracts.ErrSchemaInvalid,
				"the cleanup reply says the tenant is not there and also that it existed or that something remains", "")
		}
		p.gone(st)
		return probes.CleanupReport{Existed: false, Remaining: map[string]int{}}, nil
	}
	if err := reply.check(); err != nil {
		return probes.CleanupReport{}, err
	}
	// A cleanup that does not say whether the tenant existed and what is left proves
	// nothing, so it does not count.
	if reply.namesAnother(tenantID) || reply.body.Existed == nil || reply.body.Remaining == nil {
		return probes.CleanupReport{}, openapiProvisionError(contracts.ErrSchemaInvalid,
			"the cleanup reply does not state whether the tenant existed and what remains", "")
	}
	rep := probes.CleanupReport{Existed: *reply.body.Existed, Remaining: make(map[string]int, len(reply.body.Remaining))}
	for kind, n := range reply.body.Remaining {
		if n < 0 {
			return probes.CleanupReport{}, openapiProvisionError(contracts.ErrSchemaInvalid, "the cleanup reply reports a negative count", "")
		}
		rep.Remaining[kind] = int(min(n, 1<<31))
	}
	if rep.Left() == 0 {
		p.gone(st)
	}
	return rep, nil
}

// gone records that nothing of the tenant is left: nothing more is seeded into it. The
// tenant stays known, so that a repeated cleanup is still ours to send.
func (p *openapiProvisionProvisioner) gone(st *openapiProvisionTenant) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st.created = false
}

func openapiProvisionLeft(remaining map[string]int64) int64 {
	var n int64
	for _, c := range remaining {
		if c > 0 {
			n += c
		}
	}
	return n
}

// ---- the step-up completer --------------------------------------------------

// openapiProvisionWithStepUp is the provisioner of an application whose adapter configuration
// approves the completion of a step-up (openapiProvisionApproveOp).
type openapiProvisionWithStepUp struct{ *openapiProvisionProvisioner }

var _ probes.StepUpCompleter = (*openapiProvisionWithStepUp)(nil)

// CompleteStepUp implements probes.StepUpCompleter.
func (p *openapiProvisionWithStepUp) CompleteStepUp(ctx context.Context, s probes.StepUp) error {
	return p.completeStepUp(ctx, s)
}

// openapiProvisionStepUpFacts are the text and identifier facts of the application's own Prepare
// reply, by field: what the application stated and where it finds the reference it told the user
// to quote. Only those two kinds, and only so many, so that what is sent back is bounded.
func openapiProvisionStepUpFacts(res act.DeterministicResult) map[string]any {
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
		if len(out) >= openapiProvisionMaxStepUpFacts || len(text) > openapiProvisionMaxStepUpFactBytes {
			continue
		}
		out[string(f.Field)] = text
	}
	return out
}

// completeStepUp asks the application to complete the step-up of a preparation of a disposable
// tenant this provisioner created, for a principal of that tenant, as that principal does in the
// application's own interface.
func (p *openapiProvisionProvisioner) completeStepUp(ctx context.Context, s probes.StepUp) error {
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
		return openapiProvisionInvalid("approveStepUp: the preparation does not need a step-up")
	case principal == nil || principal.WorkspaceID != s.Scope.WorkspaceID || !st.workspaces[s.Scope.WorkspaceID]:
		return openapiProvisionInvalid("approveStepUp: the principal is not one of the tenant's, or not in that workspace")
	case !openapiProvisionIDRE.MatchString(s.CapabilityID) || !openapiProvisionEntityIDRE.MatchString(string(s.Prepared.IdempotencyKey)):
		return openapiProvisionInvalid("approveStepUp: the capability or the idempotency key is not usable")
	}
	reply, _, err := p.call(ctx, openapiProvisionApproveOp, map[string]any{
		"tenant_id": s.Scope.TenantID, "workspace_id": s.Scope.WorkspaceID, "principal_id": s.Scope.PrincipalID,
		"capability_id": s.CapabilityID, "idempotency_key": string(s.Prepared.IdempotencyKey), "values": openapiProvisionStepUpFacts(s.Result),
	})
	if err == nil {
		err = reply.check()
	}
	if err != nil {
		return err
	}
	// An application that does not say it completed the step-up asked for, in the tenant asked
	// for, has shown nothing.
	if reply.namesAnother(s.Scope.TenantID) || reply.body.Approved == nil || !*reply.body.Approved {
		return openapiProvisionError(contracts.ErrSchemaInvalid, "the reply does not say that the step-up was completed", "")
	}
	return nil
}
