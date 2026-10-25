package fsperm

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/fsperm/fspermtest"
)

func TestCreatedThingsAreOwnerOnly(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a", "keys")
	created, err := MkdirOwnerOnly(dir)
	if err != nil || !created {
		t.Fatalf("MkdirOwnerOnly = %v, %v", created, err)
	}
	if err := Check(dir, true); err != nil {
		t.Fatalf("a directory it made is not owner only: %v", err)
	}
	if again, err := MkdirOwnerOnly(dir); err != nil || again {
		t.Fatalf("an existing directory: created %v, %v", again, err)
	}
	f, err := CreateFile(filepath.Join(dir, "x.key"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("secret")
	_ = f.Close()
	if err := Check(f.Name(), false); err != nil {
		t.Fatalf("a file it made is not owner only: %v", err)
	}
	if _, err := CreateFile(f.Name()); !errors.Is(err, os.ErrExist) {
		t.Fatalf("CreateFile over an existing file: %v", err)
	}
	if err := Check(f.Name(), true); !errors.Is(err, ErrNotOwnerOnly) {
		t.Fatalf("a file checked as a directory: %v", err)
	}
}

// A file other users can read is not owner only: on POSIX by its mode, on
// Windows by an entry for Authenticated Users.
func TestLooseThingsAreRefused(t *testing.T) {
	root := t.TempDir()
	loose := filepath.Join(root, "loose.txt")
	if err := os.WriteFile(loose, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fspermtest.Loosen(t, loose)
	if err := Check(loose, false); !errors.Is(err, ErrNotOwnerOnly) {
		t.Fatalf("a loose file passed: %v", err)
	}
	if err := Restrict(loose, false); err != nil {
		t.Fatal(err)
	}
	if err := Check(loose, false); err != nil {
		t.Fatalf("after Restrict: %v", err)
	}
	if err := Check(filepath.Join(root, "missing"), false); err == nil || errors.Is(err, ErrNotOwnerOnly) {
		t.Fatalf("a missing path: %v", err)
	}
}

func TestLockExcludes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.lock")
	unlock, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	order := []string{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		u2, err := Lock(path)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		order = append(order, "second")
		mu.Unlock()
		_ = u2()
	}()
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	order = append(order, "first")
	mu.Unlock()
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	<-done
	if len(order) != 2 || order[0] != "first" {
		t.Fatalf("the second Lock did not wait for the first: %v", order)
	}
}

// What is created the ordinary way inside an owner-only directory (a key file
// copied back, a restored database moved in) inherits only the owner and is
// owner only; inside a directory open to others it is not.
func TestInheritedPermissionsCount(t *testing.T) {
	root := t.TempDir()
	tight := filepath.Join(root, "tight")
	if _, err := MkdirOwnerOnly(tight); err != nil {
		t.Fatal(err)
	}
	inTight := filepath.Join(tight, "copied.key")
	if err := os.WriteFile(inTight, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Check(inTight, false); err != nil {
		t.Errorf("a file written into an owner-only directory: %v", err)
	}
	loose := filepath.Join(root, "loose")
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatal(err)
	}
	fspermtest.Loosen(t, loose)
	inLoose := filepath.Join(loose, "copied.key")
	if err := os.WriteFile(inLoose, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// POSIX modes do not inherit; on Windows the entry for others does.
		if err := Check(inLoose, false); !errors.Is(err, ErrNotOwnerOnly) {
			t.Errorf("a file that inherits an entry for Authenticated Users: %v", err)
		}
	}
	if err := Check(loose, true); !errors.Is(err, ErrNotOwnerOnly) {
		t.Errorf("a directory open to others: %v", err)
	}
}
