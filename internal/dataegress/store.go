package dataegress

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// policyKey addresses one policy. It is built only from a verified scope or an
// operator (keyFor), so no request value ever reaches it. The columns follow the
// store's scope rule: the coarser levels leave the finer columns empty (the
// table's CHECK enforces it) and every statement names all of them.
type policyKey struct {
	level                                                providers.EgressLevel
	deployment, application, tenant, workspace, sourceID string
}

// keyFor builds the key of the level a scope addresses. There is no way to name
// another deployment, application, tenant, workspace or principal's rows.
func keyFor(level providers.EgressLevel, scope contracts.Scope, sourceID string) (policyKey, error) {
	if err := scope.Validate(); err != nil {
		return policyKey{}, err
	}
	k := policyKey{level: level, deployment: scope.DeploymentID}
	switch level {
	case providers.EgressLevelDeployment:
	case providers.EgressLevelApplication:
		k.application = scope.ApplicationID
	case providers.EgressLevelTenant:
		k.application, k.tenant = scope.ApplicationID, scope.TenantID
	case providers.EgressLevelWorkspace:
		k.application, k.tenant, k.workspace = scope.ApplicationID, scope.TenantID, scope.WorkspaceID
	case providers.EgressLevelSource:
		if !sourceIDRE.MatchString(sourceID) {
			return policyKey{}, contracts.New(contracts.ErrValidationFailed, "dataegress: a source policy needs a valid source id").WithDetails("field", "source_id")
		}
		k.application, k.tenant, k.workspace, k.sourceID = scope.ApplicationID, scope.TenantID, scope.WorkspaceID, sourceID
	default:
		return policyKey{}, contracts.New(contracts.ErrValidationFailed, "dataegress: unknown policy level")
	}
	return k, nil
}

const keyWhere = "deployment_id = ? AND application_id = ? AND tenant_id = ? AND workspace_id = ? AND level = ? AND source_id = ?"

func (k policyKey) args() []any {
	return []any{k.deployment, k.application, k.tenant, k.workspace, string(k.level), k.sourceID}
}

// target is the policy target the key names.
func (k policyKey) target() providers.EgressPolicyTarget {
	ident := func(s string) *common.Identifier {
		if s == "" {
			return nil
		}
		v := common.Identifier(s)
		return &v
	}
	t := providers.EgressPolicyTarget{DeploymentID: common.Identifier(k.deployment), ApplicationID: ident(k.application), TenantID: ident(k.tenant), WorkspaceID: ident(k.workspace)}
	if k.sourceID != "" {
		id := providers.SourceID(k.sourceID)
		t.SourceID = &id
	}
	return t
}

func targetEqual(a, b providers.EgressPolicyTarget) bool {
	str := func(p *common.Identifier) string {
		if p == nil {
			return ""
		}
		return string(*p)
	}
	src := func(p *providers.SourceID) string {
		if p == nil {
			return ""
		}
		return string(*p)
	}
	return a.DeploymentID == b.DeploymentID && str(a.ApplicationID) == str(b.ApplicationID) && str(a.TenantID) == str(b.TenantID) &&
		str(a.WorkspaceID) == str(b.WorkspaceID) && src(a.SourceID) == src(b.SourceID)
}

// storedPolicy is one row.
type storedPolicy struct {
	policy   providers.EgressPolicy
	document []byte
}

// readPolicy reads the policy at k, or found=false. A row that does not decode
// strictly, or whose document disagrees with its key, is an internal fault: it is
// never read as something it is not (the caller fails closed).
func readPolicy(ctx context.Context, q store.Reader, k policyKey) (sp storedPolicy, found bool, err error) {
	var id, doc string
	var rev int64
	err = q.QueryRowContext(ctx, "SELECT policy_id, revision, document FROM dataegress_policies WHERE "+keyWhere, k.args()...).Scan(&id, &rev, &doc)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return storedPolicy{}, false, nil
	case err != nil:
		return storedPolicy{}, false, err
	}
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.DisallowUnknownFields()
	var p providers.EgressPolicy
	if err := dec.Decode(&p); err != nil {
		return storedPolicy{}, false, contracts.New(contracts.ErrInternal, "dataegress: a stored policy does not decode")
	}
	if string(p.PolicyID) != id || p.Revision != rev || p.Level != k.level || !targetEqual(p.Target, k.target()) {
		return storedPolicy{}, false, contracts.New(contracts.ErrInternal, "dataegress: a stored policy disagrees with its key")
	}
	return storedPolicy{policy: p, document: []byte(doc)}, true, nil
}

// chain is the policy found at each of the four request levels. A nil member is
// a level with no policy, which means disabled.
type chain struct {
	deployment, application, tenant, workspace *providers.EgressPolicy
}

func (c chain) all() [4]*providers.EgressPolicy {
	return [4]*providers.EgressPolicy{c.deployment, c.application, c.tenant, c.workspace}
}

var levelNames = [4]providers.EgressDecisionLevelResultLevel{
	providers.EgressDecisionLevelResultLevelDeployment,
	providers.EgressDecisionLevelResultLevelApplication,
	providers.EgressDecisionLevelResultLevelTenant,
	providers.EgressDecisionLevelResultLevelWorkspace,
}

// loadChain reads the four level policies of a scope in one snapshot.
func (s *Service) loadChain(ctx context.Context, scope contracts.Scope) (chain, error) {
	var out chain
	levels := [4]providers.EgressLevel{providers.EgressLevelDeployment, providers.EgressLevelApplication, providers.EgressLevelTenant, providers.EgressLevelWorkspace}
	dst := [4]**providers.EgressPolicy{&out.deployment, &out.application, &out.tenant, &out.workspace}
	err := s.db.View(ctx, func(tx *store.Tx) error {
		for i, lv := range levels {
			k, err := keyFor(lv, scope, "")
			if err != nil {
				return err
			}
			sp, found, err := readPolicy(ctx, tx, k)
			if err != nil {
				return err
			}
			if found {
				p := sp.policy
				*dst[i] = &p
			}
		}
		return nil
	})
	return out, err
}

// loadSource reads the source-level policy of one RAG source of the scope's
// workspace, or nil. A source of another workspace, tenant, application or
// deployment is never visible: the key is built from the scope.
func (s *Service) loadSource(ctx context.Context, scope contracts.Scope, id providers.SourceID) (*providers.EgressPolicy, error) {
	k, err := keyFor(providers.EgressLevelSource, scope, string(id))
	if err != nil {
		return nil, err
	}
	sp, found, err := readPolicy(ctx, s.db, k)
	if err != nil || !found {
		return nil, err
	}
	p := sp.policy
	return &p, nil
}

// ---- operators ----

// Operator identifies an operator of the deployment: the person or service that
// runs the installation, as authenticated by a composition root (the CLI, the local
// admin surface, the operator API after its own authorisation). It is NOT built from
// a context token or a request body. The deployment-level policy and the global kill
// switch are written only through an Operator, so a host application's token, however
// it was minted, cannot reach them.
type Operator struct {
	DeploymentID string
	PrincipalID  string
}

func (o Operator) validate() error {
	for _, c := range []struct {
		level contracts.ScopeLevel
		v     string
	}{{contracts.LevelDeployment, o.DeploymentID}, {contracts.LevelPrincipal, o.PrincipalID}} {
		if strings.TrimSpace(c.v) == "" {
			return contracts.Newf(contracts.ErrScopeMissing, "dataegress: operator %s is missing", c.level)
		}
		bad := !utf8.ValidString(c.v) || utf8.RuneCountInString(c.v) > identifierMax
		for _, r := range c.v {
			if r == '*' || unicode.IsSpace(r) || unicode.IsControl(r) {
				bad = true
			}
		}
		if bad {
			return contracts.Newf(contracts.ErrScopeViolation, "dataegress: operator %s is malformed", c.level)
		}
	}
	return nil
}

// ---- kill switch persistence ----

// KillSwitchState is the state of the emergency switch of one deployment.
type KillSwitchState struct {
	Engaged   bool
	Reason    string
	ChangedBy string
	ChangedAt time.Time
}

func readKillSwitch(ctx context.Context, q store.Reader, deployment string) (KillSwitchState, error) {
	var st KillSwitchState
	var engaged, at int64
	err := q.QueryRowContext(ctx,
		"SELECT engaged, reason, changed_by, changed_at FROM dataegress_kill_switch WHERE deployment_id = ? AND application_id = '' AND tenant_id = '' AND workspace_id = ''",
		deployment).Scan(&engaged, &st.Reason, &st.ChangedBy, &at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return KillSwitchState{}, nil
	case err != nil:
		return KillSwitchState{}, err
	}
	st.Engaged, st.ChangedAt = engaged == 1, time.UnixMilli(at).UTC()
	return st, nil
}

func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
