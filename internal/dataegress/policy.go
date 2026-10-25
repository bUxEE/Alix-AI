package dataegress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// PolicyInput is what a writer states about a policy: the operator-supplied
// ("host") members of providers.EgressPolicy. The target, ID, revision, approval
// and timestamps are minted by the engine from the verified scope or operator and
// the clock; a writer cannot name another tenant, set an approval or forge an author.
type PolicyInput struct {
	// Level is application, tenant, workspace or source. The deployment level is
	// written with PutDeploymentPolicy.
	Level providers.EgressLevel
	// SourceID names the RAG source of a source-level policy (a source of the
	// writer's workspace) and is empty at every other level.
	SourceID providers.SourceID
	// ExpectedRevision is the revision the writer read (0 to create). A mismatch is
	// a conflict, so two writers cannot silently overwrite each other.
	ExpectedRevision int64

	Enabled          bool
	SourceMode       providers.EgressMode
	SourceClass      *providers.DataClassification
	AllowedProviders []providers.ProviderID
	AllowedModels    []providers.ModelRef
	AllowedClasses   []providers.EgressEligibleClass
	AllowedRegions   []providers.RegionCode
	// Files is the files setting: whether excerpts of uploaded files may leave, and up to
	// which class. Nil keeps them local (the default). Not at source level.
	Files *providers.EgressPolicyContentEgress
	// Audio is the audio setting: whether raw audio may leave (to an external speech
	// recogniser), and up to which class. Nil keeps it local (the default). Not at source level.
	Audio *providers.EgressPolicyContentEgress
}

// PutPolicy creates or replaces the policy of one level of the caller's own
// scope: the application's, its tenant's, its workspace's or one source of its
// workspace. The caller needs the write capability of the level (WriteCapability);
// a policy that lets internal or confidential data leave also needs
// CapabilityApprove, and the approval it records is the caller's own, from the
// verified scope. The change and its audit event are one transaction.
func (s *Service) PutPolicy(ctx context.Context, scope contracts.Scope, in PolicyInput) (providers.EgressPolicy, error) {
	if in.Level == providers.EgressLevelDeployment {
		return providers.EgressPolicy{}, contracts.New(contracts.ErrPermissionDenied, "dataegress: the deployment policy is written by an operator, not through a request scope")
	}
	capability, ok := WriteCapability(in.Level)
	if !ok {
		return providers.EgressPolicy{}, contracts.New(contracts.ErrValidationFailed, "dataegress: unknown policy level")
	}
	if err := scope.Validate(); err != nil {
		return providers.EgressPolicy{}, err
	}
	if !scope.Permits(capability) {
		return providers.EgressPolicy{}, contracts.New(contracts.ErrPermissionDenied, "dataegress: the context token does not permit writing this policy level").WithDetails("capability", capability)
	}
	k, err := keyFor(in.Level, scope, string(in.SourceID))
	if err != nil {
		return providers.EgressPolicy{}, err
	}
	loc, err := audit.ForScope(scope)
	if err != nil {
		return providers.EgressPolicy{}, err
	}
	return s.put(ctx, k, scope.PrincipalID, scope.Permits(CapabilityApprove), loc, in)
}

// PutDeploymentPolicy creates or replaces the deployment-level policy of the
// operator's deployment. It is an operator path (see Operator); the operator's
// approval is recorded where the policy lets internal data leave.
func (s *Service) PutDeploymentPolicy(ctx context.Context, op Operator, in PolicyInput) (providers.EgressPolicy, error) {
	if err := op.validate(); err != nil {
		return providers.EgressPolicy{}, err
	}
	if in.Level != providers.EgressLevelDeployment {
		return providers.EgressPolicy{}, contracts.New(contracts.ErrValidationFailed, "dataegress: PutDeploymentPolicy writes the deployment level only")
	}
	k := policyKey{level: providers.EgressLevelDeployment, deployment: op.DeploymentID}
	return s.put(ctx, k, op.PrincipalID, true, audit.Platform(), in)
}

// GetPolicy returns the policy of one level of the caller's own scope, or
// found=false (a level with no policy is disabled). The deployment level is
// readable too: it is the deployment's, not another tenant's.
func (s *Service) GetPolicy(ctx context.Context, scope contracts.Scope, level providers.EgressLevel, sourceID providers.SourceID) (providers.EgressPolicy, bool, error) {
	if err := scope.Validate(); err != nil {
		return providers.EgressPolicy{}, false, err
	}
	if !scope.Permits(CapabilityRead) {
		return providers.EgressPolicy{}, false, contracts.New(contracts.ErrPermissionDenied, "dataegress: the context token does not permit reading egress policy").WithDetails("capability", CapabilityRead)
	}
	k, err := keyFor(level, scope, string(sourceID))
	if err != nil {
		return providers.EgressPolicy{}, false, err
	}
	sp, found, err := readPolicy(ctx, s.db, k)
	if err != nil {
		return providers.EgressPolicy{}, false, wrapStore(err, "dataegress: the policy could not be read")
	}
	return sp.policy, found, nil
}

func (s *Service) put(ctx context.Context, k policyKey, by string, canApprove bool, loc audit.Locator, in PolicyInput) (providers.EgressPolicy, error) {
	// Refuse an invalid policy before anything else, whatever revision the writer holds.
	if _, err := buildPolicy(k, in, providers.EgressPolicyID(s.ids.New(prefixPolicy)), 1, by, canApprove, s.clk.Now().UTC()); err != nil {
		return providers.EgressPolicy{}, err
	}
	var out providers.EgressPolicy
	err := s.db.Tx(ctx, func(tx *store.Tx) error {
		cur, found, err := readPolicy(ctx, tx, k)
		if err != nil {
			return err
		}
		current := int64(0)
		if found {
			current = cur.policy.Revision
		}
		if in.ExpectedRevision != current {
			return contracts.New(contracts.ErrConflict, "dataegress: the policy changed since it was read; read it again and retry").WithDetails("current_revision", current)
		}
		now := s.clk.Now().UTC()
		id := providers.EgressPolicyID(s.ids.New(prefixPolicy))
		if found {
			id = cur.policy.PolicyID
		}
		p, err := buildPolicy(k, in, id, current+1, by, canApprove, now)
		if err != nil {
			return err
		}
		doc, err := canonicalJSON(p)
		if err != nil {
			return contracts.Wrap(contracts.ErrInternal, err, "dataegress: the policy cannot be encoded")
		}
		if found {
			res, err := tx.ExecContext(ctx,
				"UPDATE dataegress_policies SET revision = ?, document = ?, updated_by = ?, updated_at = ? WHERE "+keyWhere+" AND revision = ?",
				append([]any{p.Revision, string(doc), by, now.UnixMilli()}, append(k.args(), current)...)...)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return contracts.New(contracts.ErrConflict, "dataegress: the policy changed since it was read; read it again and retry").WithDetails("current_revision", current)
			}
		} else {
			_, err := tx.ExecContext(ctx,
				"INSERT INTO dataegress_policies (policy_id, deployment_id, application_id, tenant_id, workspace_id, source_id, level, revision, document, updated_by, created_at, updated_at)"+
					" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				string(id), k.deployment, k.application, k.tenant, k.workspace, k.sourceID, string(k.level), p.Revision, string(doc), by, now.UnixMilli(), now.UnixMilli())
			if store.IsUniqueViolation(err) {
				return contracts.New(contracts.ErrConflict, "dataegress: the policy changed since it was read; read it again and retry")
			}
			if err != nil {
				return err
			}
		}
		sum := sha256.Sum256(doc)
		if _, err := EventPolicyChanged.AppendTx(ctx, s.audit, tx, loc, PolicyChanged{
			PolicyID: string(id), Level: string(k.level), Revision: p.Revision, PreviousRevision: current,
			Enabled: p.Enabled, SourceMode: string(p.SourceMode), SHA256: hex.EncodeToString(sum[:]), ChangedBy: by,
		}); err != nil {
			// Fail closed: a policy change that cannot be recorded does not happen.
			return contracts.Wrap(contracts.ErrUnavailable, err, "the policy change could not be recorded")
		}
		out = p
		return nil
	})
	if err != nil {
		return providers.EgressPolicy{}, wrapStore(err, "dataegress: the policy could not be written")
	}
	return out, nil
}

// buildPolicy assembles and checks one policy. It implements the cross-field rules
// of schemas/providers/egress-policy that JSON Schema cannot leave to the type, so a
// policy that would be invalid cannot be stored: restricted data is never
// egress-eligible, a restricted source is local_only or deny, confidential is a
// per-source decision above which it cannot be listed, the files and audio settings are
// not a source's (uploads and raw audio have no source) and never name restricted, and any
// allowance of internal or confidential data (files and audio included) carries the approval
// of an authorised operator.
func buildPolicy(k policyKey, in PolicyInput, id providers.EgressPolicyID, revision int64, by string, canApprove bool, now time.Time) (providers.EgressPolicy, error) {
	bad := func(field, msg string) error {
		return contracts.New(contracts.ErrValidationFailed, "dataegress: "+msg).WithDetails("field", field)
	}
	if !in.SourceMode.Valid() {
		return providers.EgressPolicy{}, bad("source_mode", "the source mode is not a known mode")
	}
	if in.Level != k.level || (in.Level == providers.EgressLevelSource) != (k.sourceID != "") || (in.SourceID != "" && string(in.SourceID) != k.sourceID) {
		return providers.EgressPolicy{}, bad("source_id", "the source id and the level disagree")
	}
	if len(in.AllowedProviders) > 32 || len(in.AllowedModels) > 128 || len(in.AllowedClasses) > 3 || len(in.AllowedRegions) > 32 {
		return providers.EgressPolicy{}, bad("allowed", "an allowlist is longer than the schema allows")
	}
	provs := make([]providers.ProviderID, 0, len(in.AllowedProviders))
	for _, p := range in.AllowedProviders {
		if !providerIDRE.MatchString(string(p)) {
			return providers.EgressPolicy{}, bad("allowed_providers", "a provider id is malformed (wildcards are not allowed)")
		}
		provs = append(provs, p)
	}
	models := make([]providers.ModelRef, 0, len(in.AllowedModels))
	for _, m := range in.AllowedModels {
		if !providerIDRE.MatchString(string(m.ProviderID)) || !modelIDRE.MatchString(string(m.ModelID)) {
			return providers.EgressPolicy{}, bad("allowed_models", "a model reference is malformed (wildcards are not allowed)")
		}
		models = append(models, m)
	}
	classes := make([]providers.EgressEligibleClass, 0, len(in.AllowedClasses))
	for _, c := range in.AllowedClasses {
		if !c.Valid() {
			return providers.EgressPolicy{}, bad("allowed_classes", "a class is not egress-eligible (restricted can never be allowed)")
		}
		if c == providers.EgressEligibleClassConfidential && in.Level != providers.EgressLevelSource {
			return providers.EgressPolicy{}, bad("allowed_classes", "confidential can be allowed only by a source policy")
		}
		classes = append(classes, c)
	}
	regions := make([]providers.RegionCode, 0, len(in.AllowedRegions))
	for _, r := range in.AllowedRegions {
		if !regionCodeRE.MatchString(string(r)) {
			return providers.EgressPolicy{}, bad("allowed_regions", "a region code is malformed (unknown is not a region)")
		}
		regions = append(regions, r)
	}
	provs, models, classes, regions = dedupeSorted(provs), dedupeModels(models), dedupeClasses(classes), dedupeSorted(regions)

	if in.Level == providers.EgressLevelSource {
		if in.SourceClass == nil || !in.SourceClass.Valid() {
			return providers.EgressPolicy{}, bad("source_class", "a source policy states the classification of the source")
		}
		if *in.SourceClass == providers.DataClassificationRestricted && in.SourceMode != providers.EgressModeLocalOnly && in.SourceMode != providers.EgressModeDeny {
			return providers.EgressPolicy{}, bad("source_mode", "a restricted source can only be local_only or deny")
		}
	} else if in.SourceClass != nil {
		return providers.EgressPolicy{}, bad("source_class", "only a source policy has a source class")
	}
	// The files and audio settings govern content no RAG source covers, so a source policy has neither.
	content := func(field string, c *providers.EgressPolicyContentEgress) (*providers.EgressPolicyContentEgress, error) {
		switch {
		case c == nil:
			return nil, nil
		case in.Level == providers.EgressLevelSource:
			return nil, bad(field, "uploaded files and raw audio have no source, so a source policy has no "+field+" setting")
		case !c.MaxClass.Valid():
			return nil, bad(field, "the "+field+" class is not egress-eligible (restricted can never be allowed)")
		}
		v := *c
		return &v, nil
	}
	files, err := content("files", in.Files)
	if err != nil {
		return providers.EgressPolicy{}, err
	}
	audio, err := content("audio", in.Audio)
	if err != nil {
		return providers.EgressPolicy{}, err
	}
	allowsAbovePublic := func(c *providers.EgressPolicyContentEgress) bool {
		return c != nil && c.Enabled && c.MaxClass != providers.EgressEligibleClassPublic
	}

	needsApproval := allowsAbovePublic(files) || allowsAbovePublic(audio) ||
		containsClass(classes, providers.EgressEligibleClassInternal) ||
		(in.Level == providers.EgressLevelSource && containsClass(classes, providers.EgressEligibleClassConfidential)) ||
		(in.SourceClass != nil && (*in.SourceClass == providers.DataClassificationInternal || *in.SourceClass == providers.DataClassificationConfidential) &&
			(in.SourceMode == providers.EgressModeAllowExcerpt || in.SourceMode == providers.EgressModeAllowMetadata))
	var approval *providers.EgressPolicyApproval
	if needsApproval {
		if !canApprove {
			return providers.EgressPolicy{}, contracts.New(contracts.ErrPermissionDenied,
				"dataegress: letting internal or confidential data leave needs the approval of an authorised operator").WithDetails("capability", CapabilityApprove)
		}
		approval = &providers.EgressPolicyApproval{ApprovedBy: common.Identifier(by), ApprovedAt: timestamp(now)}
	}
	return providers.EgressPolicy{
		PolicyID: id, Revision: revision, Level: in.Level, Target: k.target(), Enabled: in.Enabled, SourceMode: in.SourceMode,
		SourceClass: in.SourceClass, AllowedProviders: provs, AllowedModels: models, AllowedClasses: classes, AllowedRegions: regions,
		Files: files, Audio: audio, Approval: approval, UpdatedAt: timestamp(now), UpdatedBy: common.Identifier(by),
	}, nil
}

func timestamp(t time.Time) common.Timestamp {
	return common.Timestamp(t.UTC().Format(time.RFC3339Nano))
}

func containsClass(list []providers.EgressEligibleClass, c providers.EgressEligibleClass) bool {
	for _, x := range list {
		if x == c {
			return true
		}
	}
	return false
}

func dedupeSorted[T ~string](in []T) []T {
	out := append([]T{}, in...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	n := 0
	for i, v := range out {
		if i == 0 || v != out[n-1] {
			out[n] = v
			n++
		}
	}
	return out[:n]
}

func dedupeModels(in []providers.ModelRef) []providers.ModelRef {
	out := append([]providers.ModelRef{}, in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProviderID != out[j].ProviderID {
			return out[i].ProviderID < out[j].ProviderID
		}
		return out[i].ModelID < out[j].ModelID
	})
	n := 0
	for i, v := range out {
		if i == 0 || v != out[n-1] {
			out[n] = v
			n++
		}
	}
	return out[:n]
}

func dedupeClasses(in []providers.EgressEligibleClass) []providers.EgressEligibleClass {
	out := make([]providers.EgressEligibleClass, 0, 3)
	for _, c := range []providers.EgressEligibleClass{providers.EgressEligibleClassPublic, providers.EgressEligibleClassInternal, providers.EgressEligibleClassConfidential} {
		if containsClass(in, c) {
			out = append(out, c)
		}
	}
	return out
}
