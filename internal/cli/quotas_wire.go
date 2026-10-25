package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/agui"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/quotas"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Per-tenant quotas on the real request paths (M5-28, ADR 0018).
//
// internal/quotas does the accounting; this file is the composition root that puts it in front of
// the paths a tenant can reach. Nothing in internal/gateway, internal/sessions or internal/files
// imports it: the quota check is a handler wrapper that runs after identity.Middleware (so only a
// verified Scope ever reaches a bucket) and before the route's own handler.
//
//	requests_per_minute   every /v1 request except the commit path, one token each
//	concurrent_sessions   one lease per session: taken when a session is opened (POST /sessions, the first
//	                      turn or AG-UI run of a session), renewed by every later request of the session,
//	                      released by DELETE /sessions/{id}, by a failed open, or by the lease's own expiry
//	storage_bytes         charged by the files service in the transaction that stores the file (ChargeTx, through
//	                      the files.Charger hook quotaTxCharger fills): a refusal stores nothing and charges
//	                      nothing. At the boundary POST /files is only CHECKED early, against the declared length,
//	                      before a byte is read, and once the handler has answered the gauge is set to what the
//	                      files service measures, so a delete or an expiry cannot leave it high
//	ingest_bytes_per_day  charged in the same transaction for a file stored as a rag_source, and by every source sync
//	                      in the transaction that writes the versions it creates (the ingest.Charger hook the same
//	                      adapter fills); a tenant over its day is refused with resets_at
//
// A files service that does not charge in its own transaction (it has no Charger: a stand-in, a build without
// the hook) is still charged here at the boundary, the way M5-28 did it: Admit a reservation, settle it against
// the measured gauge, charge ingestion once the file exists and withdraw the file when the day is over.
//
// The commit path is never refused for quota: POST /actions/{id}/confirm and /reject, the reads of an
// action's status and receipt (a client that is unsure of an outcome must be able to ask, master plan 18),
// and an AG-UI run that is a resume (the user has already confirmed). Those requests still spend a token
// when one is left, so a tenant's traffic is counted, but a drained bucket never stops them.
//
// What stays at the boundary, and says so: the early check, the reconciliation of the storage gauge after a
// delete or an expiry, and the legacy reservation for a service that cannot charge in its transaction. The
// authority is the charge inside the data's transaction. See docs/operations/production.md.

// quotaWireSectionName is the configuration section of internal/quotas.
const quotaWireSectionName = "quotas"

// quotaWireSections holds the "quotas" section copy registered on each registry other than
// config.Default, where internal/quotas registers its own in init.
var quotaWireSections sync.Map // *config.Registry -> *quotas.Config

// quotaWireRegisterSections registers the quotas section on reg, for a registry other than
// config.Default (tests, tools). The type and the name are the package's, so a block that
// config.Default accepts is accepted here and refused for the same reasons.
func quotaWireRegisterSections(reg *config.Registry) {
	c := quotas.DefaultConfig()
	reg.Register(quotaWireSectionName, &c)
	quotaWireSections.Store(reg, &c)
}

// quotaWireSectionOf returns the section as the registry serve ran on loaded it.
func quotaWireSectionOf(reg *config.Registry) quotas.Config {
	if reg != nil && reg != config.Default {
		if v, ok := quotaWireSections.Load(reg); ok {
			return *v.(*quotas.Config)
		}
	}
	return quotas.FromConfig()
}

// quotaWireConfig resolves the configuration the quotas are built from. The quotas profile is the
// serve profile unless the section names another one: the section's own default is lite, so "lite"
// and "" both mean "follow the deployment profile". A profile with no quota table is a refused start,
// never a guessed one.
func quotaWireConfig(c quotas.Config, serveProfile string) (quotas.Config, error) {
	if c.Profile == "" || c.Profile == quotas.ProfileLite {
		c.Profile = serveProfile
	}
	if c.Profile == "" {
		c.Profile = quotas.ProfileLite
	}
	c = c.WithDefaults()
	if _, ok := quotas.ProfileLimits(c.Profile); !ok {
		return c, contracts.Newf(contracts.ErrValidationFailed, "the deployment profile %q has no quota table", c.Profile)
	}
	return c, nil
}

// quotaWireInput is what quotaWireNew builds from.
type quotaWireInput struct {
	DB     *store.DB
	Clock  clock.Clock
	Log    *slog.Logger
	Config quotas.Config
	// Profile is the deployment profile name; it picks the quota table when the section names none.
	Profile string
	// Files finds the files service when a request needs it. It is a lookup, not a value, because the
	// files provider runs after this wiring is built.
	Files func() (gateway.FilesService, bool)
}

const (
	// quotaWireUploadCeiling is what an upload with no declared length reserves: the gateway's own
	// ceiling for a request body.
	quotaWireUploadCeiling = 16 << 20
	// quotaWirePeekLimit bounds how much of an AG-UI body is read to tell a resume from a new run.
	quotaWirePeekLimit = 1 << 20
	// quotaWireSweepAt is how many session entries the wiring keeps before it looks for stale ones.
	quotaWireSweepAt = 1024
)

// quotaWire is the quota check in front of the /v1 routes.
type quotaWire struct {
	q     *quotas.Quotas
	clk   clock.Clock
	log   *slog.Logger
	ttl   time.Duration
	files func() (gateway.FilesService, bool)

	mu     sync.Mutex
	leases map[quotaWireSession]*quotaWireHeld
}

// quotaWireSession identifies one session of one principal: the verified scope, nothing from the request.
type quotaWireSession struct{ app, tenant, workspace, principal, session string }

type quotaWireHeld struct {
	lease *quotas.Lease
	seen  time.Time
}

func quotaWireSessionOf(s contracts.Scope) quotaWireSession {
	return quotaWireSession{s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID}
}

// quotaWireNew builds the quotas from the profile defaults and the section.
func quotaWireNew(in quotaWireInput) (*quotaWire, error) {
	cfg, err := quotaWireConfig(in.Config, in.Profile)
	if err != nil {
		return nil, err
	}
	clk := in.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	q, err := quotas.New(in.DB, quotas.Options{Config: cfg, Clock: clk})
	if err != nil {
		return nil, err
	}
	log := in.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	files := in.Files
	if files == nil {
		files = func() (gateway.FilesService, bool) { return nil, false }
	}
	quotaTxBind(in.DB, q)
	log.Info("tenant quotas", slog.String("profile", cfg.Profile), slog.Int("overrides", len(cfg.Overrides)),
		slog.Duration("lease_ttl", cfg.LeaseTTL))
	return &quotaWire{q: q, clk: clk, log: log, ttl: cfg.LeaseTTL, files: files, leases: map[quotaWireSession]*quotaWireHeld{}}, nil
}

// prune drops the daily counters older than two days (the dispatcher-side maintenance of ADR 0018).
func (w *quotaWire) prune(ctx context.Context) error {
	_, err := w.q.Prune(ctx, 2)
	return err
}

// quotaWireRoute is what a request is, as far as quotas are concerned.
type quotaWireRoute int

const (
	quotaWirePlain       quotaWireRoute = iota
	quotaWireCommit                     // the commit path: confirm, reject, status, receipt
	quotaWireAguiRun                    // POST /agui: a new run, or a resume
	quotaWireOpen                       // POST /sessions
	quotaWireTurn                       // POST /sessions/{id}/turns
	quotaWireEnd                        // DELETE /sessions/{id}
	quotaWireSessionRead                // any other route of a session
	quotaWireUpload                     // POST /files
	quotaWireFileDelete                 // DELETE /files/{id}
)

// quotaWireClassify reads the matched pattern of the route ("POST /v1/sessions"), which the mux set
// before the identity middleware ran. A request with no pattern is plain and so is charged.
func quotaWireClassify(r *http.Request) quotaWireRoute {
	method, path, _ := strings.Cut(r.Pattern, " ")
	switch {
	case method == http.MethodPost && path == "/v1/sessions":
		return quotaWireOpen
	case method == http.MethodPost && path == "/v1/sessions/{session_id}/turns":
		return quotaWireTurn
	case method == http.MethodDelete && path == "/v1/sessions/{session_id}":
		return quotaWireEnd
	case strings.HasPrefix(path, "/v1/sessions/"):
		return quotaWireSessionRead
	case method == http.MethodPost && path == "/v1/agui":
		return quotaWireAguiRun
	case strings.HasPrefix(path, "/v1/agui"):
		return quotaWireSessionRead
	case method == http.MethodPost && path == "/v1/files":
		return quotaWireUpload
	case method == http.MethodDelete && strings.HasPrefix(path, "/v1/files/"):
		return quotaWireFileDelete
	case strings.HasPrefix(path, "/v1/actions/"):
		return quotaWireCommit
	}
	return quotaWirePlain
}

// quotaWireAfter puts the quota check behind the identity middleware: auth(quotas(next)).
func quotaWireAfter(auth func(http.Handler) http.Handler, w *quotaWire) func(http.Handler) http.Handler {
	if auth == nil || w == nil {
		return auth
	}
	return func(next http.Handler) http.Handler { return auth(w.Wrap(next)) }
}

// Wrap is the quota check for one /v1 route.
func (w *quotaWire) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		scope, ok := identity.ScopeFromContext(r.Context())
		if !ok {
			// identity.Middleware always sets one; without it nothing is admitted.
			gateway.WriteError(rw, r, contracts.New(contracts.ErrScopeMissing, "request has no verified scope"))
			return
		}
		ctx := r.Context()
		route := quotaWireClassify(r)

		// The commit path is never refused. A run that is a resume has already been confirmed by the user.
		if route == quotaWireCommit || (route == quotaWireAguiRun && quotaWireIsResume(r)) {
			_, _ = w.q.Admit(ctx, scope, quotas.DimRequestsPerMinute, 1) // counted when a token is left, never a reason to refuse
			if route == quotaWireAguiRun {
				w.adopt(ctx, scope)
			}
			next.ServeHTTP(rw, r)
			return
		}

		if _, err := w.q.Admit(ctx, scope, quotas.DimRequestsPerMinute, 1); err != nil {
			quotaWireRefuse(rw, r, err, 0)
			return
		}
		switch route {
		case quotaWireOpen, quotaWireTurn, quotaWireAguiRun:
			w.serveOpening(rw, r, next, scope)
		case quotaWireEnd:
			sw := &quotaWireStatus{ResponseWriter: rw}
			next.ServeHTTP(sw, r)
			if sw.ok() {
				w.drop(scope)
			}
		case quotaWireSessionRead:
			w.adopt(ctx, scope)
			next.ServeHTTP(rw, r)
		case quotaWireUpload:
			w.serveUpload(rw, r, next, scope)
		case quotaWireFileDelete:
			w.serveFileDelete(rw, r, next, scope)
		default:
			next.ServeHTTP(rw, r)
		}
	})
}

// ---- sessions ---------------------------------------------------------------------------

// ensure makes sure the session of scope holds a lease: it renews the one it has, or takes a slot.
// taken says a new slot was taken now. A refused slot is the error; nothing is held then.
func (w *quotaWire) ensure(ctx context.Context, scope contracts.Scope) (taken bool, err error) {
	k := quotaWireSessionOf(scope)
	now := w.clk.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if h := w.leases[k]; h != nil {
		if h.lease.Renew() == nil {
			h.seen = now
			return false, nil
		}
		delete(w.leases, k) // expired: another session may hold its slot now
	}
	lease, err := w.q.Admit(ctx, scope, quotas.DimConcurrentSessions, 1)
	if err != nil {
		return false, err
	}
	w.leases[k] = &quotaWireHeld{lease: lease, seen: now}
	if len(w.leases) >= quotaWireSweepAt {
		w.sweepLocked(now)
	}
	return true, nil
}

// sweepLocked forgets entries whose lease must have expired by now: unseen for two lease lifetimes.
func (w *quotaWire) sweepLocked(now time.Time) {
	for k, h := range w.leases {
		if now.Sub(h.seen) > 2*w.ttl {
			h.lease.Release()
			delete(w.leases, k)
		}
	}
}

// adopt gives a session that is already running a slot when there is one. It never refuses: a session
// that exists is not turned away by a read, only opening one is.
func (w *quotaWire) adopt(ctx context.Context, scope contracts.Scope) {
	_, _ = w.ensure(ctx, scope)
}

// drop releases the session's slot.
func (w *quotaWire) drop(scope contracts.Scope) {
	k := quotaWireSessionOf(scope)
	w.mu.Lock()
	h := w.leases[k]
	delete(w.leases, k)
	w.mu.Unlock()
	if h != nil {
		h.lease.Release()
	}
}

// serveOpening runs a route that opens a session or starts work in one. A tenant with all its slots in use
// is refused before the handler runs; a slot taken for a request the handler then fails is given back.
func (w *quotaWire) serveOpening(rw http.ResponseWriter, r *http.Request, next http.Handler, scope contracts.Scope) {
	taken, err := w.ensure(r.Context(), scope)
	if err != nil {
		quotaWireRefuse(rw, r, err, 0)
		return
	}
	sw := &quotaWireStatus{ResponseWriter: rw}
	next.ServeHTTP(sw, r)
	if taken && !sw.ok() {
		w.drop(scope)
	}
}

// ---- files ------------------------------------------------------------------------------

// quotaWireUsage is the part of *files.Service that measures what a tenant holds.
type quotaWireUsage interface {
	Usage(ctx context.Context, scope contracts.Scope) (files.Usage, error)
}

// reconcile sets the storage gauge to what the files service measures. It reports whether it could.
func (w *quotaWire) reconcile(ctx context.Context, scope contracts.Scope, svc gateway.FilesService) bool {
	u, ok := svc.(quotaWireUsage)
	if !ok {
		return false
	}
	use, err := u.Usage(ctx, scope)
	if err != nil {
		w.log.Warn("quotas: the storage gauge could not be measured", slog.String("code", string(contracts.CodeOf(err))))
		return false
	}
	if err := w.q.SetStorage(ctx, scope, use.Bytes); err != nil {
		w.log.Warn("quotas: the storage gauge could not be set", slog.String("code", string(contracts.CodeOf(err))))
		return false
	}
	return true
}

// settle replaces a reservation of est bytes with what was stored: the measured gauge, or, with no way
// to measure, the difference.
func (w *quotaWire) settle(ctx context.Context, scope contracts.Scope, svc gateway.FilesService, est, actual int64) {
	if svc != nil && w.reconcile(ctx, scope, svc) {
		return
	}
	if give := est - actual; give > 0 {
		if err := w.q.Refund(ctx, scope, quotas.DimStorageBytes, give); err != nil {
			w.log.Warn("quotas: a reservation could not be given back", slog.String("code", string(contracts.CodeOf(err))))
		}
	}
}

func (w *quotaWire) serveUpload(rw http.ResponseWriter, r *http.Request, next http.Handler, scope contracts.Scope) {
	ctx := r.Context()
	svc, _ := w.files()
	if svc != nil {
		w.reconcile(ctx, scope, svc) // a gauge that expiry or a delete left high must not lock the tenant out
	}
	est := r.ContentLength
	if est <= 0 || est > quotaWireUploadCeiling {
		est = quotaWireUploadCeiling
	}
	if quotaTxCharges(svc) {
		w.serveUploadTx(rw, r, next, scope, svc, est)
		return
	}
	if _, err := w.q.Admit(ctx, scope, quotas.DimStorageBytes, est); err != nil {
		quotaWireRefuse(rw, r, err, http.StatusInsufficientStorage)
		return
	}
	buf := newQuotaWireBuffer(rw)
	next.ServeHTTP(buf, r)
	if !buf.ok() {
		w.settle(ctx, scope, svc, est, 0)
		buf.replay()
		return
	}
	var ref struct {
		FileID    string `json:"file_id"`
		SizeBytes int64  `json:"size_bytes"`
		Treatment string `json:"treatment"`
	}
	if err := json.Unmarshal(buf.body.Bytes(), &ref); err != nil {
		ref.SizeBytes = est // the answer cannot be read: keep the reservation until the next measure
		w.log.Warn("quotas: an upload answer could not be read")
	}
	w.settle(ctx, scope, svc, est, ref.SizeBytes)
	if ref.Treatment == string(files.TreatmentRAGSource) {
		if _, err := w.q.Admit(ctx, scope, quotas.DimIngestBytesPerDay, max(ref.SizeBytes, 1)); err != nil {
			if svc != nil && ref.FileID != "" {
				if derr := svc.Delete(ctx, scope, ref.FileID); derr == nil {
					w.settle(ctx, scope, svc, 0, 0)
					quotaWireRefuse(rw, r, err, 0)
					return
				}
			}
			// The file could not be withdrawn: it stays, and so does the answer that says so.
			w.log.Error("quotas: a file over the ingestion quota could not be withdrawn")
		}
	}
	buf.replay()
}

// serveUploadTx is the upload of a files service that charges in its own transaction. The boundary only turns
// away early what the service would refuse anyway (a tenant that cannot take the declared length), takes nothing,
// and afterwards sets the gauge to what the service measures. The answer is the handler's own: a refusal by the
// in-transaction charge is already its typed 507, with nothing stored and nothing charged, and a file the RAG
// ingestion refused has already been withdrawn and its charge given back, so there is no file to withdraw here.
func (w *quotaWire) serveUploadTx(rw http.ResponseWriter, r *http.Request, next http.Handler, scope contracts.Scope, svc gateway.FilesService, declared int64) {
	ctx := r.Context()
	if r.ContentLength <= 0 {
		declared = 1 // an undeclared length proves nothing about the size: only a tenant with no room at all is turned away early
	}
	if err := w.q.Check(ctx, scope, quotas.DimStorageBytes, declared); err != nil {
		quotaWireRefuse(rw, r, err, http.StatusInsufficientStorage)
		return
	}
	next.ServeHTTP(rw, r)
	w.reconcile(ctx, scope, svc)
}

func (w *quotaWire) serveFileDelete(rw http.ResponseWriter, r *http.Request, next http.Handler, scope contracts.Scope) {
	ctx := r.Context()
	svc, _ := w.files()
	var size int64
	if _, measurable := svc.(quotaWireUsage); svc != nil && !measurable {
		if ref, err := svc.Stat(ctx, scope, r.PathValue("file_id")); err == nil {
			size = ref.SizeBytes
		}
	}
	sw := &quotaWireStatus{ResponseWriter: rw}
	next.ServeHTTP(sw, r)
	if !sw.ok() || svc == nil {
		return
	}
	if !w.reconcile(ctx, scope, svc) && size > 0 {
		if err := w.q.Refund(ctx, scope, quotas.DimStorageBytes, size); err != nil {
			w.log.Warn("quotas: a deleted file's bytes could not be given back", slog.String("code", string(contracts.CodeOf(err))))
		}
	}
}

// ---- AG-UI ------------------------------------------------------------------------------

// quotaWireIsResume reports whether an AG-UI run answers an interrupt: a resume entry, or a last message that
// is a tool result which is not a UI tool's (agui.Intake). The body is read up to a bound and put back.
func quotaWireIsResume(r *http.Request) bool {
	if r.Body == nil || r.Body == http.NoBody {
		return false
	}
	head, err := io.ReadAll(io.LimitReader(r.Body, quotaWirePeekLimit+1))
	r.Body = quotaWireBody{Reader: io.MultiReader(bytes.NewReader(head), r.Body), Closer: r.Body}
	if err != nil || len(head) > quotaWirePeekLimit {
		return false
	}
	var in struct {
		Resume   []json.RawMessage `json:"resume"`
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"toolCallId"`
		} `json:"messages"`
	}
	if json.Unmarshal(head, &in) != nil {
		return false
	}
	if len(in.Resume) > 0 {
		return true
	}
	if n := len(in.Messages); n > 0 {
		last := in.Messages[n-1]
		return last.Role == "tool" && !strings.HasPrefix(last.ToolCallID, agui.UICallPrefix)
	}
	return false
}

type quotaWireBody struct {
	io.Reader
	io.Closer
}

// ---- refusals and response wrappers -------------------------------------------------------

// quotaWireRefuse writes a quota refusal: rate_limited with retry_after_ms (and a Retry-After header that
// says the same, in whole seconds), or resource_exhausted with dimension, limit and, for the daily counter,
// resets_at. status 0 is the gateway's own status for the code.
func quotaWireRefuse(rw http.ResponseWriter, r *http.Request, err error, status int) {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil && ce.Code == contracts.ErrRateLimited {
		if ms, ok := ce.Details["retry_after_ms"].(int64); ok {
			rw = quotaWireRetryAfter{ResponseWriter: rw, seconds: max(1, (ms+999)/1000)}
		}
	}
	if status != 0 {
		gateway.WriteErrorStatus(rw, r, status, err)
		return
	}
	gateway.WriteError(rw, r, err)
}

// quotaWireRetryAfter replaces the gateway's fixed Retry-After with the bucket's own wait.
type quotaWireRetryAfter struct {
	http.ResponseWriter
	seconds int64
}

func (w quotaWireRetryAfter) WriteHeader(code int) {
	w.Header().Set("Retry-After", strconv.FormatInt(w.seconds, 10))
	w.ResponseWriter.WriteHeader(code)
}

func (w quotaWireRetryAfter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// quotaWireStatus remembers the status a handler answered with.
type quotaWireStatus struct {
	http.ResponseWriter
	code int
}

func (w *quotaWireStatus) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *quotaWireStatus) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *quotaWireStatus) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *quotaWireStatus) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ok is a 2xx answer. A handler that wrote nothing answered 200.
func (w *quotaWireStatus) ok() bool {
	return w.code == 0 || (w.code >= 200 && w.code < 300)
}

// quotaWireBuffer holds an upload's answer (a small JSON document) until the quota has settled, so the
// answer can still be replaced by a refusal.
type quotaWireBuffer struct {
	real   http.ResponseWriter
	header http.Header
	code   int
	body   bytes.Buffer
}

func newQuotaWireBuffer(rw http.ResponseWriter) *quotaWireBuffer {
	return &quotaWireBuffer{real: rw, header: http.Header{}}
}

func (b *quotaWireBuffer) Header() http.Header { return b.header }

func (b *quotaWireBuffer) WriteHeader(code int) {
	if b.code == 0 {
		b.code = code
	}
}

func (b *quotaWireBuffer) Write(p []byte) (int, error) {
	if b.code == 0 {
		b.code = http.StatusOK
	}
	return b.body.Write(p)
}

// Unwrap lets the handler extend the real connection's write deadline.
func (b *quotaWireBuffer) Unwrap() http.ResponseWriter { return b.real }

func (b *quotaWireBuffer) ok() bool {
	return b.code == 0 || (b.code >= 200 && b.code < 300)
}

// replay sends the held answer.
func (b *quotaWireBuffer) replay() {
	h := b.real.Header()
	for k, v := range b.header {
		h[k] = v
	}
	if b.code == 0 {
		b.code = http.StatusOK
	}
	b.real.WriteHeader(b.code)
	_, _ = b.real.Write(b.body.Bytes())
}

// ---- charging in the data's own transaction (M9-01) -------------------------------------------

// quotaTxCharger fills the two hooks the data modules define, files.Charger and ingest.Charger, from
// internal/quotas. Neither module imports internal/quotas (the module map allows it only base and store): this
// composition root is the one place that knows both. ChargeTx is called with the transaction that writes the
// data, so the quota refusal aborts the write and a later failure of the write takes the charge back.
type quotaTxCharger struct{ q *quotas.Quotas }

var (
	_ files.Charger  = quotaTxCharger{}
	_ ingest.Charger = quotaTxCharger{}
)

func quotaTxDimension(kind files.ChargeKind) (quotas.Dimension, bool) {
	switch kind {
	case files.ChargeStorage:
		return quotas.DimStorageBytes, true
	case files.ChargeIngest:
		return quotas.DimIngestBytesPerDay, true
	}
	return "", false
}

// ChargeTx implements files.Charger. An unknown kind is refused: a charge that names no dimension is never a
// charge that is skipped.
func (c quotaTxCharger) ChargeTx(ctx context.Context, tx *store.Tx, scope contracts.Scope, kind files.ChargeKind, bytes int64) error {
	dim, ok := quotaTxDimension(kind)
	if !ok {
		return contracts.Newf(contracts.ErrValidationFailed, "quotas: %q is not a volume that can be charged", string(kind))
	}
	return c.q.ChargeTx(ctx, tx, scope, dim, bytes)
}

// Refund implements files.Charger.
func (c quotaTxCharger) Refund(ctx context.Context, scope contracts.Scope, kind files.ChargeKind, bytes int64) error {
	dim, ok := quotaTxDimension(kind)
	if !ok {
		return contracts.Newf(contracts.ErrValidationFailed, "quotas: %q is not a volume that can be refunded", string(kind))
	}
	return c.q.Refund(ctx, scope, dim, bytes)
}

// ChargeIngestTx implements ingest.Charger.
func (c quotaTxCharger) ChargeIngestTx(ctx context.Context, tx *store.Tx, scope contracts.Scope, bytes int64) error {
	return c.q.ChargeTx(ctx, tx, scope, quotas.DimIngestBytesPerDay, bytes)
}

// quotaTxChargers maps a store to the quotas built over it. The serve composition builds the quotas (this file)
// before the files provider and in a different function, so the provider finds them here by the store it was
// given instead of through a field of a file this task does not own.
var quotaTxChargers sync.Map // *store.DB -> quotaTxCharger

// quotaTxBind makes the quotas the charger of every data module built over db: the files provider asks
// quotaTxChargerFor, and the ingestion finds it through ingest.BindCharger when a sync is about to write.
func quotaTxBind(db *store.DB, q *quotas.Quotas) {
	if db == nil || q == nil {
		return
	}
	c := quotaTxCharger{q: q}
	quotaTxChargers.Store(db, c)
	ingest.BindCharger(db, c)
}

// quotaTxChargerFor is files.Options.Charge for the store: the quotas bound to it, or nil (an interface that
// really is nil, so the service knows it has no charger) when none were.
func quotaTxChargerFor(db *store.DB) files.Charger {
	if v, ok := quotaTxChargers.Load(db); ok {
		return v.(quotaTxCharger)
	}
	return nil
}

// quotaTxService is a files service that says whether it charges in its own transaction.
type quotaTxService interface{ ChargesInTransaction() bool }

func quotaTxCharges(svc gateway.FilesService) bool {
	t, ok := svc.(quotaTxService)
	return ok && t.ChargesInTransaction()
}
