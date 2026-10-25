package oskeyring

import (
	"context"
	"errors"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/buxee/alix-ai/internal/credentials"
)

// The real OS store is not part of verify: go-keyring's in-memory mock stands in for it.
func TestOSStoreOverKeyringMock(t *testing.T) {
	keyring.MockInit()
	s := New()
	if _, err := s.Get("svc", "u"); !errors.Is(err, credentials.ErrOSStoreNotFound) {
		t.Fatalf("absent entry: %v", err)
	}
	if err := s.Set("svc", "u", "TESTONLY-value"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get("svc", "u"); err != nil || v != "TESTONLY-value" {
		t.Fatalf("Get = %q, %v", v, err)
	}
	if err := s.Delete("svc", "u"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("svc", "u"); !errors.Is(err, credentials.ErrOSStoreNotFound) {
		t.Fatalf("second delete: %v", err)
	}

	// and as the credentials backend
	b := credentials.NewOSBackend(s)
	slot := credentials.Slot{Ref: "cred_X"}
	if err := b.Put(context.Background(), slot, "TESTONLY-v2"); err != nil {
		t.Fatal(err)
	}
	if v, err := b.Get(context.Background(), slot); err != nil || v.Reveal() != "TESTONLY-v2" {
		t.Fatalf("backend Get: %v", err)
	}
}

func TestKeyringFailureTextIsNotPassedOn(t *testing.T) {
	keyring.MockInitWithError(errors.New("TESTONLY-canary leaked by the OS"))
	_, err := New().Get("svc", "u")
	if err == nil || errors.Is(err, credentials.ErrOSStoreNotFound) {
		t.Fatalf("want a plain failure, got %v", err)
	}
	if got := err.Error(); got != errUnavailable.Error() {
		t.Fatalf("the OS error text was passed on: %q", got)
	}
}
