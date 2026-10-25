package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// ragSourcesFake is a RAGSources that records what it is asked and answers from
// a script, including answers a correct implementation never gives.
type ragSourcesFake struct {
	mu     sync.Mutex
	calls  []string
	scopes []contracts.Scope

	// answer is what Register and Status return.
	answer []RAGSourcesRecord
	err    error
}

func (f *ragSourcesFake) note(call string, scope contracts.Scope) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	f.scopes = append(f.scopes, scope)
}

func (f *ragSourcesFake) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *ragSourcesFake) Register(_ context.Context, scope contracts.Scope, _ RAGSourcesRegistration) (RAGSourcesRecord, error) {
	f.note("register", scope)
	if f.err != nil || len(f.answer) == 0 {
		return RAGSourcesRecord{}, f.err
	}
	return f.answer[0], nil
}

func (f *ragSourcesFake) Reaudit(_ context.Context, scope contracts.Scope) (RAGSourcesReaudit, error) {
	f.note("reaudit", scope)
	return RAGSourcesReaudit{Checked: 1, Rebound: 1}, f.err
}

func (f *ragSourcesFake) Remove(_ context.Context, scope contracts.Scope, _ string) error {
	f.note("remove", scope)
	return f.err
}

func (f *ragSourcesFake) Status(_ context.Context, scope contracts.Scope, _ string) ([]RAGSourcesRecord, error) {
	f.note("status", scope)
	return f.answer, f.err
}

func ragSourcesScope(tenant, workspace string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_test", ApplicationID: "demo-library", TenantID: tenant, WorkspaceID: workspace,
		PrincipalID: "usr-1", Roles: []string{"member"}, CapabilityScopes: []string{"rag.read"}, SessionID: "ses_test",
	}
}

func ragSourcesRecord(scope contracts.Scope, id string) RAGSourcesRecord {
	return RAGSourcesRecord{
		ApplicationID: scope.ApplicationID, TenantID: scope.TenantID, WorkspaceID: scope.WorkspaceID,
		PackageSourceID: "handbook", PackageVersion: "1.0.0", Serving: true,
		Status: rag.SourceStatus{SourceID: actions.SourceID(id), DisplayName: "Handbook", State: rag.SourceStatusStateReady},
	}
}

func ragSourcesCode(t *testing.T, name string, err error, want contracts.ErrCode) {
	t.Helper()
	if got := contracts.CodeOf(err); err == nil || got != want {
		t.Errorf("%s: error = %v (code %q), want code %q", name, err, got, want)
	}
}

// Every method of the interface is scope checked in the gateway: before the
// implementation sees a call (an incomplete or malformed scope, a malformed
// name or id), and after it answers (a record of another application, tenant or
// workspace never reaches the caller), and the implementation is given exactly
// the scope the middleware verified.
func TestRAGSourcesInterfaceIsScopeChecked(t *testing.T) {
	ctx := context.Background()
	mine := ragSourcesScope("t1", "w1")
	srcMine, srcOther := ids.New(ragSourcesIDPrefix), ids.New(ragSourcesIDPrefix)
	register := RAGSourcesRegistration{PackageSourceID: "handbook"}

	t.Run("a call with a bad scope never reaches the implementation", func(t *testing.T) {
		fake := &ragSourcesFake{answer: []RAGSourcesRecord{ragSourcesRecord(mine, srcMine)}}
		svc := RAGSourcesScoped(fake)
		noTenant := mine
		noTenant.TenantID = ""
		wildcard := mine
		wildcard.WorkspaceID = "*"
		noPrincipal := mine
		noPrincipal.PrincipalID = ""
		for name, scope := range map[string]contracts.Scope{
			"no tenant": noTenant, "a wildcard workspace": wildcard, "no principal": noPrincipal, "a zero scope": {},
		} {
			_, err := svc.Register(ctx, scope, register)
			ragSourcesCode(t, name+": Register", err, contracts.CodeOf(scope.Validate()))
			_, err = svc.Reaudit(ctx, scope)
			ragSourcesCode(t, name+": Reaudit", err, contracts.CodeOf(scope.Validate()))
			ragSourcesCode(t, name+": Remove", svc.Remove(ctx, scope, srcMine), contracts.CodeOf(scope.Validate()))
			_, err = svc.Status(ctx, scope, "")
			ragSourcesCode(t, name+": Status", err, contracts.CodeOf(scope.Validate()))
		}
		if got := fake.called(); len(got) != 0 {
			t.Fatalf("the implementation was called with a bad scope: %v", got)
		}
	})

	t.Run("a malformed name or id never reaches the implementation", func(t *testing.T) {
		fake := &ragSourcesFake{}
		svc := RAGSourcesScoped(fake)
		for _, name := range []string{"", " handbook", "handbook ", "hand\x00book", "hand\nbook", strings.Repeat("x", ragSourcesMaxName+1), "bad\xffutf8"} {
			_, err := svc.Register(ctx, mine, RAGSourcesRegistration{PackageSourceID: name})
			ragSourcesCode(t, "Register "+name, err, contracts.ErrValidationFailed)
		}
		for _, id := range []string{"", "docs", "src_", "SRC_01ARZ3NDEKTSV4RRFFQ69G5FAV", "src_01ARZ3NDEKTSV4RRFFQ69G5FAV' OR 1=1", "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV"} {
			ragSourcesCode(t, "Remove "+id, svc.Remove(ctx, mine, id), contracts.ErrValidationFailed)
		}
		for _, id := range []string{"docs", "src_", "ses_01ARZ3NDEKTSV4RRFFQ69G5FAV"} {
			_, err := svc.Status(ctx, mine, id)
			ragSourcesCode(t, "Status "+id, err, contracts.ErrValidationFailed)
		}
		if got := fake.called(); len(got) != 0 {
			t.Fatalf("the implementation was called with a malformed argument: %v", got)
		}
	})

	t.Run("the implementation is given the verified scope itself", func(t *testing.T) {
		fake := &ragSourcesFake{answer: []RAGSourcesRecord{ragSourcesRecord(mine, srcMine)}}
		svc := RAGSourcesScoped(fake)
		if _, err := svc.Register(ctx, mine, register); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Reaudit(ctx, mine); err != nil {
			t.Fatal(err)
		}
		if err := svc.Remove(ctx, mine, srcMine); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Status(ctx, mine, ""); err != nil {
			t.Fatal(err)
		}
		if got := fake.called(); strings.Join(got, ",") != "register,reaudit,remove,status" {
			t.Fatalf("calls = %v", got)
		}
		for i, got := range fake.scopes {
			if got.TenantID != mine.TenantID || got.WorkspaceID != mine.WorkspaceID || got.ApplicationID != mine.ApplicationID ||
				got.PrincipalID != mine.PrincipalID || got.SessionID != mine.SessionID || got.DeploymentID != mine.DeploymentID {
				t.Errorf("call %d got scope %+v, not the verified one", i, got)
			}
		}
	})

	t.Run("a record of another scope is never passed on", func(t *testing.T) {
		foreign := map[string]RAGSourcesRecord{
			"another tenant":    ragSourcesRecord(ragSourcesScope("t2", "w1"), srcOther),
			"another workspace": ragSourcesRecord(ragSourcesScope("t1", "w2"), srcOther),
			"another application": func() RAGSourcesRecord {
				r := ragSourcesRecord(mine, srcOther)
				r.ApplicationID = "another-app"
				return r
			}(),
			"no scope at all": func() RAGSourcesRecord {
				r := ragSourcesRecord(mine, srcOther)
				r.ApplicationID, r.TenantID, r.WorkspaceID = "", "", ""
				return r
			}(),
		}
		for name, rec := range foreign {
			// A registration that answers with one is a fault, not a source.
			fake := &ragSourcesFake{answer: []RAGSourcesRecord{rec}}
			got, err := RAGSourcesScoped(fake).Register(ctx, mine, register)
			ragSourcesCode(t, name+": Register", err, contracts.ErrInternal)
			if got.Status.SourceID != "" || got.PackageSourceID != "" {
				t.Errorf("%s: Register leaked %+v with its error", name, got)
			}

			// A single source of another scope is the answer for one that does not exist.
			fake = &ragSourcesFake{answer: []RAGSourcesRecord{rec}}
			_, err = RAGSourcesScoped(fake).Status(ctx, mine, srcOther)
			ragSourcesCode(t, name+": Status of its id", err, contracts.ErrValidationFailed)
			var ce *contracts.Error
			if !errors.As(err, &ce) || ce.Details["reason"] != "not_found" {
				t.Errorf("%s: Status = %v, want the not_found answer", name, err)
			}

			// A listing drops it and keeps the caller's own.
			fake = &ragSourcesFake{answer: []RAGSourcesRecord{rec, ragSourcesRecord(mine, srcMine)}}
			list, err := RAGSourcesScoped(fake).Status(ctx, mine, "")
			if err != nil || len(list) != 1 || string(list[0].Status.SourceID) != srcMine {
				t.Errorf("%s: Status of the scope = %+v, %v; want only its own source", name, list, err)
			}
		}
	})

	t.Run("a source the scope holds is answered, one it does not is not_found", func(t *testing.T) {
		fake := &ragSourcesFake{answer: []RAGSourcesRecord{ragSourcesRecord(mine, srcMine)}}
		svc := RAGSourcesScoped(fake)
		got, err := svc.Status(ctx, mine, srcMine)
		if err != nil || len(got) != 1 || !got[0].Serving {
			t.Fatalf("Status of a held source = %+v, %v", got, err)
		}
		// An implementation that answers for a different id than the one asked is
		// not trusted to have answered.
		if _, err := svc.Status(ctx, mine, srcOther); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("Status of an id the answer does not hold = %v", err)
		}
		// An empty implementation answer for a single source is not_found as well.
		empty := RAGSourcesScoped(&ragSourcesFake{})
		if _, err := empty.Status(ctx, mine, srcMine); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("Status of an unknown id = %v", err)
		}
		if list, err := empty.Status(ctx, mine, ""); err != nil || len(list) != 0 {
			t.Errorf("an empty listing = %+v, %v", list, err)
		}
	})

	t.Run("the implementation's own errors keep their code", func(t *testing.T) {
		for _, code := range []contracts.ErrCode{contracts.ErrPermissionDenied, contracts.ErrQuarantined, contracts.ErrConflict, contracts.ErrUnavailable} {
			svc := RAGSourcesScoped(&ragSourcesFake{err: contracts.New(code, "no")})
			_, err := svc.Register(ctx, mine, register)
			ragSourcesCode(t, "Register "+string(code), err, code)
			_, err = svc.Reaudit(ctx, mine)
			ragSourcesCode(t, "Reaudit "+string(code), err, code)
			ragSourcesCode(t, "Remove "+string(code), svc.Remove(ctx, mine, srcMine), code)
			_, err = svc.Status(ctx, mine, "")
			ragSourcesCode(t, "Status "+string(code), err, code)
		}
	})

	t.Run("a missing implementation is refused by the registry, not wrapped", func(t *testing.T) {
		if RAGSourcesScoped(nil) != nil {
			t.Fatal("RAGSourcesScoped(nil) returned a service")
		}
		if err := Provide(NewServices(), RAGSourcesScoped(nil)); err == nil {
			t.Error("a nil RAGSources was provided")
		}
		svc := NewServices()
		if err := Provide[RAGSources](svc, RAGSourcesScoped(&ragSourcesFake{})); err != nil {
			t.Fatal(err)
		}
		if _, ok := Lookup[RAGSources](svc); !ok {
			t.Error("the provided RAGSources is not found by its type")
		}
	})
}
