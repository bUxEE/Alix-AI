package m1

import (
	"fmt"
	"os"
	"sync"
	"testing"
)

// tb is the part of testing.TB the fixtures use. *testing.T satisfies it, and so does the
// lifecycle that owns the deployment the conversation tests share: building a deployment takes
// half a minute (an integration audit against the real application, twice), so it is built once
// per test binary and must outlive the test that happened to ask for it first.
type tb interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
	Logf(format string, args ...any)
	TempDir() string
	Cleanup(func())
	Failed() bool
}

var _ tb = (*testing.T)(nil)

// setupFailure is what a fixture's Fatal raises on the lifecycle: caught by the one place that
// builds the shared deployment and handed to every test that needed it.
type setupFailure struct{ msg string }

func (f setupFailure) Error() string { return f.msg }

// lifecycle is a tb that is not a test: its temporary directories and cleanups live until
// TestMain ends the run.
type lifecycle struct {
	mu       sync.Mutex
	dirs     []string
	cleanups []func()
}

func (l *lifecycle) Helper()      {}
func (l *lifecycle) Failed() bool { return false }
func (l *lifecycle) Logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "m1 fixture: "+format+"\n", args...)
}
func (l *lifecycle) Errorf(format string, args ...any) {
	panic(setupFailure{fmt.Sprintf(format, args...)})
}
func (l *lifecycle) Fatalf(format string, args ...any) {
	panic(setupFailure{fmt.Sprintf(format, args...)})
}
func (l *lifecycle) Fatal(args ...any) { panic(setupFailure{fmt.Sprint(args...)}) }

func (l *lifecycle) TempDir() string {
	dir, err := os.MkdirTemp("", "alixai-m1-gate-")
	if err != nil {
		panic(setupFailure{fmt.Sprintf("create a temporary directory: %v", err)})
	}
	l.mu.Lock()
	l.dirs = append(l.dirs, dir)
	l.mu.Unlock()
	return dir
}

func (l *lifecycle) Cleanup(f func()) {
	l.mu.Lock()
	l.cleanups = append(l.cleanups, f)
	l.mu.Unlock()
}

// close runs the cleanups newest first, then removes the directories. A cleanup that fails is
// reported and does not stop the others: it runs after every test has finished, and a late
// complaint must not turn a passing run into a crash.
func (l *lifecycle) close() {
	l.mu.Lock()
	cleanups, dirs := l.cleanups, l.dirs
	l.cleanups, l.dirs = nil, nil
	l.mu.Unlock()
	for i := len(cleanups) - 1; i >= 0; i-- {
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "m1 fixture: a cleanup failed: %v\n", r)
				}
			}()
			cleanups[i]()
		}()
	}
	for _, d := range dirs {
		_ = os.RemoveAll(d)
	}
}

// shared is the lifecycle of the deployment the conversation tests share.
var shared = &lifecycle{}

func TestMain(m *testing.M) {
	code := m.Run()
	shared.close()
	os.Exit(code)
}

// guard runs build and turns a fixture's Fatal into an error.
func guard[T any](build func() T) (v T, err error) {
	defer func() {
		if r := recover(); r != nil {
			if f, ok := r.(setupFailure); ok {
				err = f
				return
			}
			panic(r)
		}
	}()
	return build(), nil
}
