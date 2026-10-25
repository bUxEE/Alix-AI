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

	mcptransport "github.com/buxee/alix-ai/adapters/mcp"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/integrationaudit/probes"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// The provisioning half of the MCP transport (transport_mcp.go): the bridge that lets
// the integration audit's runtime probes (master plan 4.5 checks 2, 6, 8, 9 and 10,
// internal/integrationaudit/probes) reach an MCP application's disposable tenants.
//
// # Why a bridge
//
// The probes create their tenants through a probes.Provisioner. The adapter the audit
// is given is the adapters.Wrap wrapper around the MCP adapter and exposes only the
// contract (Read, Validate, Prepare, Commit, Status, Describe, Health, Inspect), and
// the contract operations call only the tools a capability maps and the three
// management tools: a tool the adapter configuration approves (tools.allow) and no
// capability maps stays uncallable by design, which is how the test tools of a package
// are kept away from a model. Their calls do not have the shape of a capability call
// either (top-level arguments, not {identity, fields}; a reply that is not a
// DeterministicResult), so they cannot ride the contract operations. They go through
// the one call the MCP adapter exports for approved-but-unmapped tools
// (Adapter.CallApprovedTool), on the very adapter the audit holds, so the bridge is
// held to exactly what the adapter holds its own calls to and has no client, session,
// connection or listing of its own. It registers itself with probes.SetProvisionerSource
// from an init function, as the probes' documentation asks.
//
// # What it runs
//
// The package's fixtures.provisioning names three tools of one MCP endpoint:
//
//	create_tenant   {tenant_id, workspaces: [{workspace_id}],
//	                 principals: [{principal_id, workspace_id, roles, service?}]}
//	seed            {tenant_id, records: [{ref, entity_type, workspace_id,
//	                 principal_id?, ...members of the seed data file}]}
//	cleanup         {tenant_id}
//
// and each answers with structuredContent {status: "ok", tenant_id, ...}: seed adds
// records [{ref, entity_id, ...}], cleanup adds existed (bool) and remaining
// ({kind: count}), which must be stated for a cleanup to count. A refusal is
// {status: "rejected", errors: [{code, ...}]}; its code is kept when it is one of the
// platform's error codes, and nothing else the server says is ever put in an error, a
// log or a report. Text is never read. docs/integration/application-package.md section
// 12 describes the contract for application authors.
//
// The seed data file (fixtures.provisioning.seed_data, below tests/ in the installed
// package) is a JSON object with a records array. The entry whose ref a seeded record
// has contributes every member the application defines (names, references, ...) except
// the logical ones the audit owns: workspace is replaced by the workspace identifier the
// audit generated, and principal (the logical reference of a fixture principal) by the
// identifier generated for it, which is found among the principals the tenant was
// created with by their reference (probes.Principal.Ref), never by how the identifier
// is spelled. A principal that cannot be found, a ref the file lists twice and an
// entity type that differs from the suite's fail the seed before anything is sent.
//
// # Three optional abilities
//
// The package format declares no tool for any of them, so they are conventions of this
// bridge, offered only when the application cooperates: the tool is on the adapter
// configuration's tools.allow, no capability maps it, and the server lists it. The
// provisioner then also implements probes.RecordChanger, probes.FaultInjector or
// probes.StepUpCompleter, and implements none of them when the tool is not there (a
// provisioner that claimed an ability it lacks would make the probes skip what they
// could not do without saying so).
//
//	test_change_record   {tenant_id, entity_type, entity_id}
//	                     -> {status: "ok", tenant_id, entity_type, entity_id, changed: true}
//	    changes one record of a disposable tenant so that its data version moves, and
//	    nothing else: the audit uses it to show that Validate notices a change and that a
//	    preparation made before it is refused as stale.
//	test_inject_fault    {operation, capability_id, kind, tenant_ids: [...]}
//	                     -> {status: "ok", armed: true}
//	    arms one failure of the next request of the operation: kind dependency_down
//	    (answer unavailable, apply nothing) or reply_lost (apply a commit, lose its
//	    reply). tenant_ids are the disposable tenants this audit created and has not
//	    removed; the application confines the failure to requests for them.
//	test_approve_step_up {tenant_id, workspace_id, principal_id, capability_id,
//	                      idempotency_key, values: {field: text, ...}}
//	                     -> {status: "ok", tenant_id, approved: true}
//	    completes the step-up of a level 3 preparation in a disposable tenant, as the user
//	    the action was prepared for does in the application's own authenticated interface
//	    (the audit holds no credential and learns none). The tool is test support: it
//	    applies every rule that interface applies and exists only while the application
//	    runs for an audit. values are the text and identifier facts the application's own
//	    Prepare reply stated, from which the application takes what it told the user to
//	    quote (a reference); the audit does not know what the application calls it. Both
//	    designs are accepted: the interface applies the effect itself, or it records an
//	    approval that the commit which follows applies once.
//
// All three act only on tenants this provisioner created, like every method of it.
//
// # Held to the adapter's own checks
//
//   - Only audits of the MCP adapter. The source answers nil, nil (no provisioner, which
//     leaves the probes exactly as they were) unless the adapter the audit holds is the
//     one this transport built, the package declares the provisioning calls and the
//     server lists all three tools now. An error from a source stops the probes for
//     every transport, so an error is only for what is this transport's own.
//   - Egress. Every connection is the adapter's, made with the egress.Dialer
//     mcpTransportConfig builds from the package: the same allowlist, no proxy,
//     certificates verified, every redirect checked.
//   - Approval. A provisioning tool must be on the adapter configuration's tools.allow
//     and mapped by no capability, and the server lists it exactly once.
//   - Pins. Before every call the adapter lists the server's tools again and holds them
//     to what it pinned (the hash of each mapped tool's name and schemas), to the
//     manifest's contract_sha256 and to the hash each approved tool had when it was
//     first seen. A tool that changed is refused as quarantined (tool_schema_drift) and
//     nothing is sent; an adapter that has found drift (ReauditRequired) provisions
//     nothing.
//   - Bounds. A reply is cut off at the adapter's response limit, a session is the
//     adapter's, nothing is retried once a request may have been delivered and every
//     error is typed.
//
// # Only the audit's own tenants
//
// A provisioner acts on tenants whose creation it asked for in this run. Seed needs a
// tenant it created (the application said ok), Cleanup a tenant it attempted to create
// (a lost reply may still have created one), and any other tenant is refused before
// anything is sent (permission_denied): an application that does not guard its own
// tenants is never given the chance to be asked. A create the application rejected (the
// tenant exists) is not adopted, so cleaning up after it can never remove somebody
// else's tenant. Every method can be called again: a second Cleanup of a removed tenant
// reports Existed false with nothing remaining, a second create of a tenant that exists
// fails with a typed conflict, and nothing is held open between calls.
//
// A server without the provisioning tools (the application is not in test mode) is left
// as it was: no provisioner, so the probes record what they could not reach as
// unverified, critical for every consequential capability.
//
// # The install directory
//
// Only the seed data file needs the install directory, and the audit's input carries the
// install record, whose path is relative to the data directory, and not the directory.
// The transport remembers where it opened each package (a wrapper around the MCP
// factory that records TransportRequest.Dir by application and manifest hash), so a
// harness that runs the audit with a configuration registry of its own finds the package
// where its own data directory put it. The configuration's data directory
// (mcpProvisionDataDir, config.Core()) joined with the record's path is only the
// fallback, for an adapter this transport did not open through the process's registry.
// There is no credential service yet (milestone X), so, like the adapter, the client
// connects without a credential.
func init() {
	probes.SetProvisionerSource(mcpProvisionSource)
	mcpProvisionRecordInstallDirs(transportsDefault)
}

const (
	// mcpProvisionMaxSeedFileBytes bounds a seed data file.
	mcpProvisionMaxSeedFileBytes = 1 << 20
	// mcpProvisionMaxRecords bounds the records of one seed.
	mcpProvisionMaxRecords = 1024

	// mcpProvisionMaxStepUpFacts and mcpProvisionMaxStepUpFactBytes bound what a step-up sends back.
	mcpProvisionMaxStepUpFacts     = 64
	mcpProvisionMaxStepUpFactBytes = 2048

	// The names of the three optional tools. The package format declares none, so the
	// name is the convention.
	mcpProvisionChangeTool  = "test_change_record"
	mcpProvisionFaultTool   = "test_inject_fault"
	mcpProvisionApproveTool = "test_approve_step_up"

	// The reasons put in Details["reason"] of the errors raised here. The first four
	// are the adapter's own.
	mcpProvisionReasonNotOffered = mcptransport.ReasonOperationNotOffered
	mcpProvisionReasonDrift      = mcptransport.ReasonToolSchemaDrift
	mcpProvisionReasonToolError  = mcptransport.ReasonToolError
	mcpProvisionReasonTextOnly   = mcptransport.ReasonTextOnlyResult
	mcpProvisionReasonTooLarge   = adapters.ReasonResponseTooLarge
	mcpProvisionReasonRejected   = "rejected"
	mcpProvisionReasonNotOurs    = "tenant_not_created_by_this_audit"
)

var (
	mcpProvisionToolNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	// mcpProvisionIDRE is what an identifier handed to the application may look like.
	mcpProvisionIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
	// mcpProvisionEntityIDRE is what the identifier of a seeded record may look like:
	// the application chose it.
	mcpProvisionEntityIDRE = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,256}$`)

	// mcpProvisionDataDir returns the data directory the install records are relative
	// to, for an adapter whose install directory the transport did not record. The
	// audit's input carries the record and not the directory; the process's
	// configuration is where the transport registry takes it from. Tests replace it.
	mcpProvisionDataDir = func() string { return config.Core().DataDir.String() }

	// mcpProvisionReserved are the members of a seed data entry the audit owns.
	mcpProvisionReserved = map[string]bool{
		"ref": true, "entity_type": true, "workspace": true, "workspace_id": true,
		"principal": true, "principal_id": true, "tenant": true, "tenant_id": true,
	}

	// mcpProvisionInstallDirs maps a package (mcpProvisionPackageKey) to the directory the
	// transport opened it from.
	mcpProvisionInstallDirs sync.Map
)

// mcpProvisionPackageKey names an installed package.
type mcpProvisionPackageKey struct{ application, manifestHash string }

// mcpProvisionRecordInstallDirs wraps the MCP factory of a transport registry so that the
// directory every package is opened from is remembered. It must run after the MCP
// transport registered itself (transport_mcp.go sorts before this file, so its init runs
// first); a registry without the factory is a programming error found at start, not an
// audit that quietly cannot read a seed file.
func mcpProvisionRecordInstallDirs(reg *transportsRegistry) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	open, ok := reg.byKind[TransportKindMCP]
	if !ok {
		panic("cli: the MCP provisioning bridge is initialised before the MCP transport registered itself")
	}
	reg.byKind[TransportKindMCP] = func(ctx context.Context, req TransportRequest) (adapters.AppAdapter, error) {
		ad, err := open(ctx, req)
		if err == nil && req.Dir != "" {
			if dir, aerr := filepath.Abs(req.Dir); aerr == nil {
				mcpProvisionInstallDirs.Store(mcpProvisionPackageKey{req.Record.ApplicationID, req.Record.ManifestHash}, dir)
			}
		}
		return ad, err
	}
}

// mcpProvisionInstallDir returns the absolute install directory of the package of an
// install record, or "" when it is not known.
func mcpProvisionInstallDir(rec apppkg.Record) string {
	if dir, ok := mcpProvisionInstallDirs.Load(mcpProvisionPackageKey{rec.ApplicationID, rec.ManifestHash}); ok {
		// a package that was opened from a directory that is gone (a data directory that moved) is not there to read
		if fi, err := os.Stat(dir.(string)); err == nil && fi.IsDir() {
			return dir.(string)
		}
	}
	dataDir := mcpProvisionDataDir()
	if dataDir == "" || rec.Path == "" {
		return ""
	}
	dir, err := filepath.Abs(filepath.Join(dataDir, filepath.FromSlash(rec.Path)))
	if err != nil {
		return ""
	}
	return dir
}

// mcpProvisionTools are the three tools the package names, on one endpoint.
type mcpProvisionTools struct {
	endpoint string
	create   string
	seed     string
	cleanup  string
}

func (t mcpProvisionTools) all() []string { return []string{t.create, t.seed, t.cleanup} }

func mcpProvisionError(code contracts.ErrCode, msg, reason string) *contracts.Error {
	e := contracts.New(code, msg).WithDetails("transport", mcptransport.Transport)
	if reason != "" {
		e.WithDetails("reason", reason)
	}
	return e
}

func mcpProvisionReasonOf(err error) string {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil {
		r, _ := ce.Details["reason"].(string)
		return r
	}
	return ""
}

// mcpProvisionSource is the probes.ProvisionerSource of the MCP transport.
func mcpProvisionSource(ctx context.Context, in *integrationaudit.Input) (probes.Provisioner, error) {
	if in == nil || in.Package == nil || in.Adapter == nil {
		return nil, nil
	}
	closing, ok := in.Adapter.(transportsClosing)
	if !ok {
		return nil, nil
	}
	audited, ok := closing.inner.(*mcptransport.Adapter)
	if !ok {
		return nil, nil
	}
	plan, err := transportsPlanFor(in.Package.Manifest)
	if err != nil || plan.Kind != TransportKindMCP {
		return nil, nil
	}
	tools, found, err := mcpProvisionToolsOf(in.Package)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	if audited.ReauditRequired() {
		return nil, mcpProvisionDrift("")
	}
	for _, t := range tools.all() {
		if !audited.ApprovedTool(tools.endpoint, t) {
			return nil, mcpProvisionError(contracts.ErrValidationFailed,
				"a provisioning tool is not an mcp tool of the package's endpoint that the adapter configuration's tools.allow approves and no capability maps", "")
		}
	}
	// The optional tools count only when the package approves them too.
	want := tools.all()
	for _, t := range []string{mcpProvisionChangeTool, mcpProvisionFaultTool, mcpProvisionApproveTool} {
		if audited.ApprovedTool(tools.endpoint, t) {
			want = append(want, t)
		}
	}
	offered, err := audited.ApprovedToolsOffered(ctx, tools.endpoint, want...)
	if err != nil {
		return nil, err
	}
	for _, t := range tools.all() {
		if !offered[t] {
			return nil, nil
		}
	}
	p := &mcpProvisionProvisioner{
		c:       &mcpProvisionClient{audited: audited, tools: tools},
		dir:     mcpProvisionInstallDir(in.Record),
		tenants: map[string]*mcpProvisionTenant{},
	}
	return mcpProvisionWithAbilities(p, offered[mcpProvisionChangeTool], offered[mcpProvisionFaultTool], offered[mcpProvisionApproveTool]), nil
}

// mcpProvisionToolsOf reads the provisioning calls of the package's test suites. All
// suites must name the same three tools of one endpoint (a Provisioner is not told which
// suite it serves); a package without a suite names none.
func mcpProvisionToolsOf(pkg *apppkg.Package) (mcpProvisionTools, bool, error) {
	var out mcpProvisionTools
	found := false
	bad := mcpProvisionError(contracts.ErrValidationFailed, "the provisioning calls of the package's test suites are not three mcp tools of one endpoint", "")
	for _, suite := range pkg.Tests {
		pv := suite.Fixtures.Provisioning
		got := mcpProvisionTools{endpoint: string(pv.CreateTenant.Endpoint)}
		for i, call := range []struct {
			endpoint string
			tool     *string
		}{
			{string(pv.CreateTenant.Endpoint), (*string)(pv.CreateTenant.MCPTool)},
			{string(pv.Seed.Endpoint), (*string)(pv.Seed.MCPTool)},
			{string(pv.Cleanup.Endpoint), (*string)(pv.Cleanup.MCPTool)},
		} {
			if call.tool == nil || call.endpoint != got.endpoint || !mcpProvisionToolNameRE.MatchString(*call.tool) {
				return mcpProvisionTools{}, false, bad
			}
			switch i {
			case 0:
				got.create = *call.tool
			case 1:
				got.seed = *call.tool
			default:
				got.cleanup = *call.tool
			}
		}
		if !found {
			out, found = got, true
		} else if got != out {
			return mcpProvisionTools{}, false, bad
		}
	}
	return out, found, nil
}

func mcpProvisionDrift(tool string) *contracts.Error {
	e := mcpProvisionError(contracts.ErrQuarantined,
		"a tool of the application changed since it was approved, so nothing is provisioned until the application is audited again", mcpProvisionReasonDrift)
	if tool != "" {
		e.WithDetails("tool", tool)
	}
	return e
}

// ---- the client -------------------------------------------------------------

// mcpProvisionClient calls the test tools of one MCP endpoint through the audited adapter:
// every call is the adapter's approved-tool call, with its approval, egress, pin, drift and
// size checks.
type mcpProvisionClient struct {
	audited *mcptransport.Adapter
	tools   mcpProvisionTools
}

// mcpProvisionReply is what a test tool answers: the members this file reads. A member
// the server adds is ignored.
type mcpProvisionReply struct {
	Status string `json:"status"`
	Errors []struct {
		Code string `json:"code"`
	} `json:"errors"`
	TenantID string `json:"tenant_id"`
	Records  []struct {
		Ref      string `json:"ref"`
		EntityID string `json:"entity_id"`
	} `json:"records"`
	Existed   *bool            `json:"existed"`
	Remaining map[string]int64 `json:"remaining"`
	// the answer of a change of a record
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Changed    *bool  `json:"changed"`
	// the answer of an armed fault
	Armed *bool `json:"armed"`
	// the answer of a completed step-up
	Approved *bool `json:"approved"`
}

// call sends one tool call and returns its reply. sent is true once the request may
// have reached the server, so that an error with sent set can be an operation that was
// applied and whose reply was lost.
func (c *mcpProvisionClient) call(ctx context.Context, tool string, args map[string]any) (reply mcpProvisionReply, sent bool, err error) {
	rep, err := c.audited.CallApprovedTool(ctx, c.tools.endpoint, tool, args)
	if err != nil {
		return mcpProvisionReply{}, rep.Sent, err
	}
	if err := json.Unmarshal(rep.Structured, &reply); err != nil {
		return mcpProvisionReply{}, true, mcpProvisionError(contracts.ErrSchemaInvalid, "the reply does not have the shape of a provisioning reply", "")
	}
	return reply, true, nil
}

// check turns the reply's status into an error: rejected keeps the application's error
// code when it is one the platform knows, anything but ok is refused.
func (r mcpProvisionReply) check() error {
	switch r.Status {
	case "ok":
		return nil
	case "rejected":
		code := contracts.ErrValidationFailed
		if len(r.Errors) > 0 && contracts.ErrCode(r.Errors[0].Code).Valid() {
			code = contracts.ErrCode(r.Errors[0].Code)
		}
		return mcpProvisionError(code, "the application refused the provisioning request", mcpProvisionReasonRejected)
	}
	return mcpProvisionError(contracts.ErrSchemaInvalid, "the reply states no usable status", "")
}

// ---- the provisioner --------------------------------------------------------

// mcpProvisionTenant is what the provisioner knows of a tenant it asked for.
type mcpProvisionTenant struct {
	// created is true once the application said ok; false for a tenant whose creation
	// is only possible (a reply was lost).
	created    bool
	workspaces map[string]bool
	principals []probes.Principal
}

// mcpProvisionProvisioner implements probes.Provisioner over the provisioning tools.
type mcpProvisionProvisioner struct {
	c   *mcpProvisionClient
	dir string

	mu      sync.Mutex
	tenants map[string]*mcpProvisionTenant
}

var _ probes.Provisioner = (*mcpProvisionProvisioner)(nil)

// Each optional ability is a type of its own, so that a provisioner implements exactly the
// optional interfaces the application offers.
type (
	mcpProvisionChangeAbility struct{ p *mcpProvisionProvisioner }
	mcpProvisionFaultAbility  struct{ p *mcpProvisionProvisioner }
	mcpProvisionStepUpAbility struct{ p *mcpProvisionProvisioner }
)

// The provisioner a source returns is the base or the base with one combination of the
// abilities: one of eight types.
type (
	mcpProvisionWithChange struct {
		*mcpProvisionProvisioner
		mcpProvisionChangeAbility
	}
	mcpProvisionWithFault struct {
		*mcpProvisionProvisioner
		mcpProvisionFaultAbility
	}
	mcpProvisionWithStepUp struct {
		*mcpProvisionProvisioner
		mcpProvisionStepUpAbility
	}
	mcpProvisionWithBoth struct { // change and fault
		*mcpProvisionProvisioner
		mcpProvisionChangeAbility
		mcpProvisionFaultAbility
	}
	mcpProvisionWithChangeStepUp struct {
		*mcpProvisionProvisioner
		mcpProvisionChangeAbility
		mcpProvisionStepUpAbility
	}
	mcpProvisionWithFaultStepUp struct {
		*mcpProvisionProvisioner
		mcpProvisionFaultAbility
		mcpProvisionStepUpAbility
	}
	mcpProvisionWithAll struct {
		*mcpProvisionProvisioner
		mcpProvisionChangeAbility
		mcpProvisionFaultAbility
		mcpProvisionStepUpAbility
	}
)

var (
	_ probes.RecordChanger   = (*mcpProvisionWithChange)(nil)
	_ probes.FaultInjector   = (*mcpProvisionWithFault)(nil)
	_ probes.StepUpCompleter = (*mcpProvisionWithStepUp)(nil)
	_ probes.RecordChanger   = (*mcpProvisionWithBoth)(nil)
	_ probes.FaultInjector   = (*mcpProvisionWithBoth)(nil)
	_ probes.RecordChanger   = (*mcpProvisionWithAll)(nil)
	_ probes.FaultInjector   = (*mcpProvisionWithAll)(nil)
	_ probes.StepUpCompleter = (*mcpProvisionWithAll)(nil)
)

// mcpProvisionWithAbilities returns the provisioner with exactly the abilities the
// application offers.
func mcpProvisionWithAbilities(p *mcpProvisionProvisioner, change, fault, stepUp bool) probes.Provisioner {
	c, f, s := mcpProvisionChangeAbility{p}, mcpProvisionFaultAbility{p}, mcpProvisionStepUpAbility{p}
	switch {
	case change && fault && stepUp:
		return &mcpProvisionWithAll{p, c, f, s}
	case change && fault:
		return &mcpProvisionWithBoth{p, c, f}
	case change && stepUp:
		return &mcpProvisionWithChangeStepUp{p, c, s}
	case fault && stepUp:
		return &mcpProvisionWithFaultStepUp{p, f, s}
	case change:
		return &mcpProvisionWithChange{p, c}
	case fault:
		return &mcpProvisionWithFault{p, f}
	case stepUp:
		return &mcpProvisionWithStepUp{p, s}
	}
	return p
}

// ChangeRecord implements probes.RecordChanger.
func (a mcpProvisionChangeAbility) ChangeRecord(ctx context.Context, tenantID, entityType, entityID string) error {
	return a.p.changeRecord(ctx, tenantID, entityType, entityID)
}

// InjectFault implements probes.FaultInjector.
func (a mcpProvisionFaultAbility) InjectFault(ctx context.Context, f probes.Fault) error {
	return a.p.injectFault(ctx, f)
}

// CompleteStepUp implements probes.StepUpCompleter.
func (a mcpProvisionStepUpAbility) CompleteStepUp(ctx context.Context, s probes.StepUp) error {
	return a.p.completeStepUp(ctx, s)
}

func mcpProvisionInvalid(msg string) error {
	return mcpProvisionError(contracts.ErrValidationFailed, msg, "")
}

// mcpProvisionCheckTenant refuses a tenant whose identifiers could not be a generated
// one before anything is sent.
func mcpProvisionCheckTenant(t probes.Tenant) error {
	if !mcpProvisionIDRE.MatchString(t.ID) {
		return mcpProvisionInvalid("create_tenant: the tenant identifier is not usable")
	}
	if n := len(t.Workspaces); n == 0 || n > 64 {
		return mcpProvisionInvalid("create_tenant: a tenant needs between 1 and 64 workspaces")
	}
	workspaces := map[string]bool{}
	for _, w := range t.Workspaces {
		if !mcpProvisionIDRE.MatchString(w) || workspaces[w] {
			return mcpProvisionInvalid("create_tenant: a workspace identifier is not usable or is repeated")
		}
		workspaces[w] = true
	}
	if len(t.Principals) > 256 {
		return mcpProvisionInvalid("create_tenant: too many principals")
	}
	seen, refs := map[string]bool{}, map[string]bool{}
	for _, p := range t.Principals {
		if !mcpProvisionIDRE.MatchString(p.ID) || seen[p.ID] || !workspaces[p.WorkspaceID] || len(p.Roles) > 16 {
			return mcpProvisionInvalid("create_tenant: a principal identifier is not usable, is repeated or names a workspace the tenant does not have")
		}
		seen[p.ID] = true
		// A reference names one principal: a repeated one would make a seed ambiguous.
		if p.Ref != "" && (!mcpProvisionIDRE.MatchString(p.Ref) || refs[p.Ref]) {
			return mcpProvisionInvalid("create_tenant: a principal reference is not usable or is repeated")
		}
		refs[p.Ref] = true
		for _, r := range p.Roles {
			if !mcpProvisionIDRE.MatchString(r) {
				return mcpProvisionInvalid("create_tenant: a role is not usable")
			}
		}
	}
	return nil
}

// CreateTenant implements probes.Provisioner.
func (p *mcpProvisionProvisioner) CreateTenant(ctx context.Context, t probes.Tenant) error {
	if err := mcpProvisionCheckTenant(t); err != nil {
		return err
	}
	p.mu.Lock()
	known := p.tenants[t.ID]
	p.mu.Unlock()
	if known != nil && known.created {
		return mcpProvisionError(contracts.ErrConflict, "a disposable tenant with this identifier already exists", "")
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
	reply, sent, err := p.c.call(ctx, p.c.tools.create, map[string]any{"tenant_id": t.ID, "workspaces": workspaces, "principals": principals})
	if err == nil {
		err = reply.check()
	}
	if err == nil && reply.TenantID != t.ID {
		err = mcpProvisionError(contracts.ErrSchemaInvalid, "the reply is for another tenant than the one asked for", "")
	}
	st := &mcpProvisionTenant{created: err == nil, workspaces: ws, principals: slices.Clone(t.Principals)}
	switch {
	case err == nil:
		p.remember(t.ID, st)
	case sent && mcpProvisionReasonOf(err) != mcpProvisionReasonRejected:
		// The request may have been applied: the tenant is ours to clean up, and not
		// yet ours to seed.
		p.remember(t.ID, st)
	}
	return err
}

func (p *mcpProvisionProvisioner) remember(id string, st *mcpProvisionTenant) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tenants[id] = st
}

// owned returns the tenant if this provisioner asked for it. needCreated also requires
// that the application said it was created.
func (p *mcpProvisionProvisioner) owned(id string, needCreated bool) (*mcpProvisionTenant, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.tenants[id]
	if st == nil || (needCreated && !st.created) {
		return nil, mcpProvisionError(contracts.ErrPermissionDenied,
			"provisioning only acts on the disposable tenants this audit created", mcpProvisionReasonNotOurs)
	}
	return st, nil
}

// createdTenants lists the tenants this provisioner created and has not removed.
func (p *mcpProvisionProvisioner) createdTenants() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for id, st := range p.tenants {
		if st.created {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// Seed implements probes.Provisioner.
func (p *mcpProvisionProvisioner) Seed(ctx context.Context, s probes.Seed) (probes.SeedResult, error) {
	st, err := p.owned(s.TenantID, true)
	if err != nil {
		return probes.SeedResult{}, err
	}
	records, err := p.seedRecords(s, st)
	if err != nil {
		return probes.SeedResult{}, err
	}
	reply, _, err := p.c.call(ctx, p.c.tools.seed, map[string]any{"tenant_id": s.TenantID, "records": records})
	if err == nil {
		err = reply.check()
	}
	if err != nil {
		return probes.SeedResult{}, err
	}
	if reply.TenantID != s.TenantID {
		return probes.SeedResult{}, mcpProvisionError(contracts.ErrSchemaInvalid, "the reply is for another tenant than the one asked for", "")
	}
	asked := map[string]bool{}
	for _, r := range s.Records {
		asked[r.Ref] = true
	}
	res := probes.SeedResult{Entities: map[string]string{}}
	for _, r := range reply.Records {
		if !asked[r.Ref] || r.EntityID == "" || len(r.EntityID) > 256 {
			return probes.SeedResult{}, mcpProvisionError(contracts.ErrSchemaInvalid, "the reply holds a record that was not asked for, or one without an identifier", "")
		}
		res.Entities[r.Ref] = r.EntityID
	}
	return res, nil
}

// seedRecords builds the records of a seed from the audit's records and the seed data
// file. The audit's own identifiers always win over the file.
func (p *mcpProvisionProvisioner) seedRecords(s probes.Seed, st *mcpProvisionTenant) ([]map[string]any, error) {
	if n := len(s.Records); n == 0 || n > mcpProvisionMaxRecords {
		return nil, mcpProvisionInvalid("seed: a seed needs between 1 and 1024 records")
	}
	overlay, err := mcpProvisionReadSeedFile(p.dir, s.DataFile)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(s.Records))
	seen := map[string]bool{}
	for _, r := range s.Records {
		if !mcpProvisionIDRE.MatchString(r.Ref) || seen[r.Ref] || r.EntityType == "" || len(r.EntityType) > 64 || !st.workspaces[r.WorkspaceID] {
			return nil, mcpProvisionInvalid("seed: a record has no usable reference or entity type, is repeated, or names a workspace the tenant does not have")
		}
		seen[r.Ref] = true
		rec := map[string]any{"ref": r.Ref, "entity_type": r.EntityType, "workspace_id": r.WorkspaceID}
		if entry, ok := overlay[r.Ref]; ok {
			if raw, has := entry["entity_type"]; has {
				var et string
				if json.Unmarshal(raw, &et) != nil || et != r.EntityType {
					return nil, mcpProvisionInvalid("seed: the seed data file gives a record another entity type than the suite does")
				}
			}
			for k, v := range entry {
				if !mcpProvisionReserved[k] {
					rec[k] = v
				}
			}
			if raw, has := entry["principal"]; has {
				var ref string
				if json.Unmarshal(raw, &ref) != nil {
					return nil, mcpProvisionInvalid("seed: the seed data file names a principal that is not a reference")
				}
				id, found := mcpProvisionPrincipalID(st, ref)
				if !found {
					return nil, mcpProvisionInvalid("seed: the seed data file names a principal the tenant was not created with")
				}
				rec["principal_id"] = id
			}
		}
		out = append(out, rec)
	}
	return out, nil
}

// mcpProvisionPrincipalID finds the identifier the audit generated for a fixture
// principal by the reference the suite gives it (probes.Principal.Ref). How the identifier
// is spelled is the audit's own business and is never relied on here.
func mcpProvisionPrincipalID(st *mcpProvisionTenant, ref string) (string, bool) {
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

// mcpProvisionReadSeedFile reads the seed data file of the installed package: a JSON
// object whose records array holds one object per record, by ref. The path is the
// manifest's, a path below tests/ with no way out of the install directory; a file that
// is not a regular file, or is larger than the bound, is refused.
func mcpProvisionReadSeedFile(dir, rel string) (map[string]map[string]json.RawMessage, error) {
	if rel == "" {
		return nil, nil
	}
	if dir == "" {
		return nil, mcpProvisionError(contracts.ErrUnavailable, "the install directory of the package is not known, so its seed data file cannot be read", "")
	}
	if path.Clean(rel) != rel || !strings.HasPrefix(rel, "tests/") || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return nil, mcpProvisionInvalid("seed: the seed data path is not a path below tests/ of the package")
	}
	full := filepath.Join(dir, filepath.FromSlash(rel))
	fi, err := os.Lstat(full)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, mcpProvisionInvalid("seed: the seed data file cannot be read")
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, mcpProvisionInvalid("seed: the seed data file cannot be read")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, mcpProvisionMaxSeedFileBytes+1))
	if err != nil || len(raw) > mcpProvisionMaxSeedFileBytes {
		return nil, mcpProvisionInvalid("seed: the seed data file cannot be read or is too large")
	}
	var doc struct {
		Records []map[string]json.RawMessage `json:"records"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&doc); err != nil {
		return nil, mcpProvisionInvalid("seed: the seed data file is not a JSON object with a records array")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, mcpProvisionInvalid("seed: the seed data file holds more than one document")
	}
	out := make(map[string]map[string]json.RawMessage, len(doc.Records))
	for _, e := range doc.Records {
		var ref string
		if json.Unmarshal(e["ref"], &ref) != nil || !mcpProvisionIDRE.MatchString(ref) {
			return nil, mcpProvisionInvalid("seed: a record of the seed data file has no usable ref")
		}
		if _, dup := out[ref]; dup {
			return nil, mcpProvisionInvalid("seed: the seed data file lists a ref twice")
		}
		out[ref] = e
	}
	return out, nil
}

// Cleanup implements probes.Provisioner.
func (p *mcpProvisionProvisioner) Cleanup(ctx context.Context, tenantID string) (probes.CleanupReport, error) {
	st, err := p.owned(tenantID, false)
	if err != nil {
		return probes.CleanupReport{}, err
	}
	reply, _, err := p.c.call(ctx, p.c.tools.cleanup, map[string]any{"tenant_id": tenantID})
	if err == nil {
		err = reply.check()
	}
	if err != nil {
		return probes.CleanupReport{}, err
	}
	// A cleanup that does not say whether the tenant existed and what is left proves
	// nothing, so it does not count.
	if reply.TenantID != tenantID || reply.Existed == nil || reply.Remaining == nil {
		return probes.CleanupReport{}, mcpProvisionError(contracts.ErrSchemaInvalid, "the cleanup reply does not state whether the tenant existed and what remains", "")
	}
	rep := probes.CleanupReport{Existed: *reply.Existed, Remaining: make(map[string]int, len(reply.Remaining))}
	for kind, n := range reply.Remaining {
		if n < 0 {
			return probes.CleanupReport{}, mcpProvisionError(contracts.ErrSchemaInvalid, "the cleanup reply reports a negative count", "")
		}
		rep.Remaining[kind] = int(min(n, 1<<31))
	}
	if rep.Left() == 0 {
		p.mu.Lock()
		st.created = false // gone: nothing more is seeded into it
		p.mu.Unlock()
	}
	return rep, nil
}

// changeRecord asks the application to change one record of a disposable tenant this
// provisioner created, so that the record's data version moves.
func (p *mcpProvisionProvisioner) changeRecord(ctx context.Context, tenantID, entityType, entityID string) error {
	if _, err := p.owned(tenantID, true); err != nil {
		return err
	}
	if !mcpProvisionIDRE.MatchString(entityType) || !mcpProvisionEntityIDRE.MatchString(entityID) {
		return mcpProvisionInvalid("change_record: the entity type or the identifier is not usable")
	}
	reply, _, err := p.c.call(ctx, mcpProvisionChangeTool, map[string]any{"tenant_id": tenantID, "entity_type": entityType, "entity_id": entityID})
	if err == nil {
		err = reply.check()
	}
	if err != nil {
		return err
	}
	// An application that does not say it changed the record asked for, in the tenant
	// asked for, has shown nothing.
	if reply.TenantID != tenantID || reply.EntityType != entityType || reply.EntityID != entityID || reply.Changed == nil || !*reply.Changed {
		return mcpProvisionError(contracts.ErrSchemaInvalid, "the reply does not say that the record asked for was changed", "")
	}
	return nil
}

// mcpProvisionFaultKinds are the failures the audit may ask for.
var mcpProvisionFaultKinds = []probes.FaultKind{probes.FaultDependencyDown, probes.FaultReplyLost}

// injectFault asks the application to arm one failure, confined to the disposable
// tenants this provisioner created and has not removed.
func (p *mcpProvisionProvisioner) injectFault(ctx context.Context, f probes.Fault) error {
	tenants := p.createdTenants()
	if len(tenants) == 0 {
		return mcpProvisionError(contracts.ErrPermissionDenied,
			"a fault is armed only for the disposable tenants this audit created, and there is none", mcpProvisionReasonNotOurs)
	}
	if !slices.Contains(mcpProvisionFaultKinds, f.Kind) || f.Operation == "" || len(f.Operation) > 32 ||
		!mcpProvisionIDRE.MatchString(string(f.Operation)) || !mcpProvisionIDRE.MatchString(f.CapabilityID) {
		return mcpProvisionInvalid("inject_fault: the operation, the capability or the kind is not usable")
	}
	reply, _, err := p.c.call(ctx, mcpProvisionFaultTool, map[string]any{
		"operation": string(f.Operation), "capability_id": f.CapabilityID, "kind": string(f.Kind), "tenant_ids": tenants,
	})
	if err == nil {
		err = reply.check()
	}
	if err != nil {
		return err
	}
	if reply.Armed == nil || !*reply.Armed {
		return mcpProvisionError(contracts.ErrSchemaInvalid, "the reply does not say that the failure was armed", "")
	}
	return nil
}

// mcpProvisionStepUpFacts are the text and identifier facts of the application's own Prepare
// reply, by field: what the application stated and where it finds the reference it told the user
// to quote. Only those two kinds, and only so many, so that what is sent back is bounded.
func mcpProvisionStepUpFacts(res act.DeterministicResult) map[string]any {
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
		if len(out) >= mcpProvisionMaxStepUpFacts || len(text) > mcpProvisionMaxStepUpFactBytes {
			continue
		}
		out[string(f.Field)] = text
	}
	return out
}

// completeStepUp asks the application to complete the step-up of a preparation of a disposable
// tenant this provisioner created, for a principal of that tenant, as that principal does in the
// application's own interface.
func (p *mcpProvisionProvisioner) completeStepUp(ctx context.Context, s probes.StepUp) error {
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
		return mcpProvisionInvalid("approve_step_up: the preparation does not need a step-up")
	case principal == nil || principal.WorkspaceID != s.Scope.WorkspaceID || !st.workspaces[s.Scope.WorkspaceID]:
		return mcpProvisionInvalid("approve_step_up: the principal is not one of the tenant's, or not in that workspace")
	case !mcpProvisionIDRE.MatchString(s.CapabilityID) || !mcpProvisionEntityIDRE.MatchString(string(s.Prepared.IdempotencyKey)):
		return mcpProvisionInvalid("approve_step_up: the capability or the idempotency key is not usable")
	}
	reply, _, err := p.c.call(ctx, mcpProvisionApproveTool, map[string]any{
		"tenant_id": s.Scope.TenantID, "workspace_id": s.Scope.WorkspaceID, "principal_id": s.Scope.PrincipalID,
		"capability_id": s.CapabilityID, "idempotency_key": string(s.Prepared.IdempotencyKey), "values": mcpProvisionStepUpFacts(s.Result),
	})
	if err == nil {
		err = reply.check()
	}
	if err != nil {
		return err
	}
	// An application that does not say it completed the step-up asked for, in the tenant asked
	// for, has shown nothing.
	if reply.TenantID != s.Scope.TenantID || reply.Approved == nil || !*reply.Approved {
		return mcpProvisionError(contracts.ErrSchemaInvalid, "the reply does not say that the step-up was completed", "")
	}
	return nil
}
