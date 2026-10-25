package actions

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// ---- inputs and outputs ----

// ReadInput asks for a level 0 read.
type ReadInput struct {
	// Session is the scope the conversation session is bound to, from stored
	// session state. The policy decision is bound to its application, tenant
	// and workspace (not copied from the presented scope), and the presented
	// scope must be the same authenticated context (identity.CheckContinuity).
	Session contracts.Scope
	// CapabilityID is the capability to read through.
	CapabilityID string
	// Fields are the candidate field values. Only fields the policy lets the
	// model see or propose reach the host; the rest are dropped.
	Fields map[string]any
	// TaskFields are the sensitive fields the current task needs, decided by
	// deterministic code (never by the model).
	TaskFields []string
}

// ReadOutcome is the result of a level 0 read.
type ReadOutcome struct {
	// Result is the host's typed result.
	Result contracts.DeterministicResult
	// Risk is the effective risk level the policy decided (always 0 here).
	Risk contracts.RiskLevel
	// VisibleFields are the fields the model may see for this capability.
	VisibleFields []string
	// DroppedFields are the proposed fields that were not forwarded.
	DroppedFields []string
}

// PrepareInput asks for a preparation. Everything in it is a candidate:
// nothing the model wrote can reach an authoritative member of the resulting
// PreparedAction.
type PrepareInput struct {
	// Session is the scope the conversation session is bound to; see ReadInput.
	Session contracts.Scope
	// CapabilityID is the capability to prepare.
	CapabilityID string
	// Fields are the candidate field values, typically a TaskProposal's fields.
	Fields map[string]any
	// TaskFields are the sensitive fields the current task needs, decided by
	// deterministic code (never by the model).
	TaskFields []string
}

// PrepareInputFromProposal builds a PrepareInput from a model's TaskProposal.
// It reads the capability ID and the candidate fields and nothing else: the
// intent must be a task or a correction, and entity references, uncertainty and
// the trace ID are the dialogue controller's business. An authoritative value
// the model tried to smuggle in has nowhere to go.
func PrepareInputFromProposal(session contracts.Scope, tp contracts.TaskProposal, taskFields []string) (PrepareInput, error) {
	if tp.Intent != conversation.TaskProposalIntentTask && tp.Intent != conversation.TaskProposalIntentCorrection {
		return PrepareInput{}, contracts.New(contracts.ErrValidationFailed, "actions: only a task or a correction can be prepared")
	}
	if tp.CapabilityID == nil || *tp.CapabilityID == "" {
		return PrepareInput{}, contracts.New(contracts.ErrUnknownCapability, "actions: the proposal names no capability")
	}
	fields := make(map[string]any, len(tp.Fields))
	for k, v := range tp.Fields {
		fields[k] = v
	}
	return PrepareInput{Session: session, CapabilityID: string(*tp.CapabilityID), Fields: fields, TaskFields: taskFields}, nil
}

// Prepared is the result of Prepare.
type Prepared struct {
	// Action is the PreparedAction, in state prepared. It is what the user is
	// shown and what the model may verbalise. Its authoritative members come
	// from the core and from the host's Prepare result only.
	Action contracts.PreparedAction
	// Confirmation is the single-use secret a client sends back with Confirm.
	// It is empty unless Action.Confirmation.Mode is explicit. It travels from
	// the coordinator to the client UI and nowhere else: never into a prompt,
	// the model's context, a log or an audit event.
	Confirmation ConfirmationToken
	// Result is the host's Prepare result. It is set whenever the host
	// answered, including when the host refused (then Action is zero and the
	// error carries the host's first issue code), so the dialogue can build
	// its directive from the typed issues.
	Result contracts.DeterministicResult
	// DroppedFields are the proposed fields that were not forwarded to the
	// host because the policy does not let the model propose them.
	DroppedFields []string
}

// ConfirmInput presents a confirmation.
type ConfirmInput struct {
	PreparationID string
	// Token is the secret Prepare returned.
	Token ConfirmationToken
}

// ---- confirmation token ----

var tokenRE = regexp.MustCompile(`^cfm_[A-Za-z0-9_-]{43}$`)

// ConfirmationToken is the single-use secret that proves a confirmation was
// issued for one preparation to one principal in one session. It prints and
// marshals as "[redacted]"; the one place its value is read is Reveal, called
// by the layer that hands it to the client.
type ConfirmationToken struct{ secret string }

// ParseConfirmationToken wraps a token a client presented. It checks the shape
// only; Confirm decides whether it is the right one.
func ParseConfirmationToken(s string) (ConfirmationToken, error) {
	if !tokenRE.MatchString(s) {
		return ConfirmationToken{}, contracts.New(contracts.ErrValidationFailed, "actions: the confirmation token is malformed")
	}
	return ConfirmationToken{secret: s}, nil
}

func newConfirmationToken() (ConfirmationToken, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ConfirmationToken{}, contracts.Wrap(contracts.ErrInternal, err, "actions: no entropy for a confirmation token")
	}
	return ConfirmationToken{secret: "cfm_" + base64.RawURLEncoding.EncodeToString(b[:])}, nil
}

// Reveal returns the secret. Only the layer that hands it to a client calls it.
func (t ConfirmationToken) Reveal() string { return t.secret }

// IsZero reports whether there is no token.
func (t ConfirmationToken) IsZero() bool { return t.secret == "" }

// String returns "[redacted]".
func (ConfirmationToken) String() string { return "[redacted]" }

// GoString returns "[redacted]".
func (ConfirmationToken) GoString() string { return "[redacted]" }

// MarshalJSON encodes "[redacted]": a token never leaves through JSON by accident.
func (ConfirmationToken) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// LogValue keeps the secret out of structured logs.
func (ConfirmationToken) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// confirmMAC binds a token to the four things a confirmation is bound to. It
// is HMAC-SHA256 keyed by the token, so the stored value verifies only the
// token it was made from, and only against this preparation, principal,
// session and set of data versions.
func confirmMAC(t ConfirmationToken, preparationID, principalID, sessionID, digest string) string {
	m := hmac.New(sha256.New, []byte(t.secret))
	for _, part := range []string{"alixai.confirmation/v1", preparationID, principalID, sessionID, digest} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		m.Write(n[:])
		m.Write([]byte(part))
	}
	return hex.EncodeToString(m.Sum(nil))
}

// dataDigest is a stable digest of a preparation's data versions.
func dataDigest(dvs []act.DataVersion) string {
	cp := append([]act.DataVersion(nil), dvs...)
	sort.Slice(cp, func(i, j int) bool {
		a, b := cp[i], cp[j]
		if a.EntityType != b.EntityType {
			return a.EntityType < b.EntityType
		}
		if a.EntityID != b.EntityID {
			return a.EntityID < b.EntityID
		}
		return a.Version < b.Version
	})
	b, _ := json.Marshal(cp)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ---- the stored preparation ----

// row is one stored preparation.
type row struct {
	id             string
	scope          contracts.Scope
	binding        policy.Binding
	capabilityID   string
	state          act.PreparedActionState
	risk           contracts.RiskLevel
	mode           act.ConfirmationRequirementMode
	mac            string
	key            string
	fields         map[string]any
	prepared       contracts.PreparedAction
	snapshot       string
	digest         string
	preparedAt     time.Time
	expiresAt      time.Time
	confirmedAt    time.Time
	committingAt   time.Time
	committingFrom act.PreparedActionState
	attempts       int
	receiptID      string
	updatedAt      time.Time
}

const prepCols = `id, application_id, tenant_id, workspace_id, principal_id, session_id, scope_json, capability_id, state,
	risk_level, confirmation_mode, confirmation_mac, idempotency_key, fields_json, prepared_json, snapshot_version, data_digest,
	prepared_at, expires_at, confirmed_at, committing_at, committing_from, commit_attempts, receipt_id, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func corrupt(what string) error {
	return contracts.New(contracts.ErrInternal, "actions: stored preparation is inconsistent: "+what)
}

func scanRow(s rowScanner) (row, error) {
	var (
		r                                                                  row
		app, tenant, ws, principal, session, scopeJSON, capID, state, mode string
		fieldsJSON, preparedJSON                                           string
		risk, preparedAt, expiresAt, updatedAt                             int64
		confirmedAt, committingAt                                          sql.NullInt64
		committingFrom, receiptID                                          sql.NullString
		attempts                                                           int
	)
	if err := s.Scan(&r.id, &app, &tenant, &ws, &principal, &session, &scopeJSON, &capID, &state,
		&risk, &mode, &r.mac, &r.key, &fieldsJSON, &preparedJSON, &r.snapshot, &r.digest,
		&preparedAt, &expiresAt, &confirmedAt, &committingAt, &committingFrom, &attempts, &receiptID, &updatedAt); err != nil {
		return row{}, err
	}
	if err := json.Unmarshal([]byte(scopeJSON), &r.scope); err != nil {
		return row{}, corrupt("scope")
	}
	if r.scope.ApplicationID != app || r.scope.TenantID != tenant || r.scope.WorkspaceID != ws ||
		r.scope.PrincipalID != principal || r.scope.SessionID != session {
		return row{}, corrupt("scope columns differ from the stored scope")
	}
	if err := r.scope.Validate(); err != nil {
		return row{}, corrupt("scope")
	}
	r.binding = policy.Binding{ApplicationID: app, TenantID: tenant, WorkspaceID: ws}
	r.capabilityID = capID
	r.state = act.PreparedActionState(state)
	if !r.state.Valid() {
		return row{}, corrupt("state")
	}
	lvl, err := contracts.ParseRiskLevel(int(risk))
	if err != nil {
		return row{}, corrupt("risk level")
	}
	r.risk = lvl
	r.mode = act.ConfirmationRequirementMode(mode)
	if !r.mode.Valid() {
		return row{}, corrupt("confirmation mode")
	}
	if r.fields, err = decodeFields(fieldsJSON); err != nil {
		return row{}, corrupt("fields")
	}
	if err := json.Unmarshal([]byte(preparedJSON), &r.prepared); err != nil {
		return row{}, corrupt("prepared action")
	}
	if string(r.prepared.PreparationID) != r.id || string(r.prepared.CapabilityID) != capID ||
		string(r.prepared.IdempotencyKey) != r.key {
		return row{}, corrupt("prepared action differs from its columns")
	}
	r.prepared.State = r.state
	r.preparedAt, r.expiresAt, r.updatedAt = fromMS(preparedAt), fromMS(expiresAt), fromMS(updatedAt)
	if confirmedAt.Valid {
		r.confirmedAt = fromMS(confirmedAt.Int64)
	}
	if committingAt.Valid {
		r.committingAt = fromMS(committingAt.Int64)
	}
	if committingFrom.Valid {
		r.committingFrom = act.PreparedActionState(committingFrom.String)
	}
	r.attempts = attempts
	r.receiptID = receiptID.String
	return r, nil
}

func decodeFields(s string) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// ---- time ----

func toMS(t time.Time) int64    { return t.UnixMilli() }
func fromMS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// trunc brings a clock reading to the precision the store keeps.
func trunc(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

// stamp formats an instant as a schema timestamp (RFC 3339, UTC, literal Z).
func stamp(t time.Time) common.Timestamp {
	return common.Timestamp(t.UTC().Format("2006-01-02T15:04:05.000Z"))
}

// ---- errors ----

// errChanged reports that a conditional update found the row in another state:
// something else moved the preparation first.
var errChanged = errors.New("actions: the preparation changed state")

func errExpired() *contracts.Error {
	return contracts.New(contracts.ErrExpired, "the preparation has expired; prepare the action again")
}

func errStale() *contracts.Error {
	return contracts.New(contracts.ErrStale, "the data the preparation relied on changed; prepare the action again")
}

func errNoPreparation() *contracts.Error {
	// A preparation that does not exist and one in another scope are
	// indistinguishable, like jobs. The reason tells both apart from a
	// continuity refusal: a session that still names a preparation retention
	// removed has nothing left to void.
	return contracts.New(contracts.ErrScopeViolation, "actions: no such preparation in this scope").WithDetails("reason", "not_found")
}

// hostIssueCode maps a host issue code to an ErrCode.
func hostIssueCode(c common.ErrorCode) contracts.ErrCode {
	code := contracts.ErrCode(c)
	if !code.Valid() {
		return contracts.ErrInternal
	}
	return code
}

// issueSummary lists the codes and fields of a result's issues for an error's
// details: never the host's prose.
func issueSummary(issues []act.Issue) []map[string]string {
	out := make([]map[string]string, 0, len(issues))
	for _, is := range issues {
		m := map[string]string{"code": string(is.Code)}
		if is.Field != nil {
			m["field"] = string(*is.Field)
		}
		out = append(out, m)
	}
	return out
}

// hostError turns a host result that is not a success into a typed error whose
// code is the host's first issue code.
func hostError(res contracts.DeterministicResult, what string) *contracts.Error {
	code := contracts.ErrInternal
	switch {
	case len(res.Errors) > 0:
		code = hostIssueCode(res.Errors[0].Code)
	case res.Status == act.DeterministicResultStatusUnavailable:
		code = contracts.ErrUnavailable
	}
	e := contracts.New(code, "the application did not accept the "+what)
	if len(res.Errors) > 0 {
		e = e.WithDetails("issues", issueSummary(res.Errors))
	}
	return e.WithDetails("host_status", string(res.Status))
}
