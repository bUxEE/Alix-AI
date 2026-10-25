package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// pragmaOn returns the named pragma as seen by one pinned connection of pool.
func pragmaOn(t *testing.T, pool *sql.DB, pragma string) string {
	t.Helper()
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close()
	var v string
	if err := conn.QueryRowContext(context.Background(), "PRAGMA "+pragma).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", pragma, err)
	}
	return v
}

func TestOpenEnablesWAL(t *testing.T) {
	db := openTest(t, nil)

	// A write so the -wal file exists on disk: proves WAL is actually in use,
	// not just reported.
	insertNote(t, db, testScope("app", "t1", "w1"), "n1", "hello", "world")
	if _, err := os.Stat(db.Path() + "-wal"); err != nil {
		t.Errorf("no -wal file next to the database after a write: %v", err)
	}

	cfg := db.Config()
	type want struct{ pragma, writer, reader string }
	for _, w := range []want{
		{"journal_mode", "wal", "wal"},
		{"foreign_keys", "1", "1"},
		{"synchronous", "1", "1"}, // NORMAL
		{"busy_timeout", "5000", "5000"},
		{"cache_size", "-8000", "-2000"},
		{"mmap_size", "0", "0"},
		{"trusted_schema", "0", "0"},
		{"query_only", "0", "1"},
	} {
		if got := pragmaOn(t, db.w, w.pragma); got != w.writer {
			t.Errorf("writer PRAGMA %s = %q, want %q", w.pragma, got, w.writer)
		}
		if got := pragmaOn(t, db.r, w.pragma); got != w.reader {
			t.Errorf("reader PRAGMA %s = %q, want %q", w.pragma, got, w.reader)
		}
	}

	// Defaults are what the efficiency review assumes.
	if cfg.CacheKiB != 8000 || cfg.ReaderCacheKiB != 2000 || cfg.ReaderConns != 4 || cfg.MmapMiB != 0 {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	if got, want := cfg.PageCacheBudgetKiB(), 8000+4*2000; got != want {
		t.Errorf("PageCacheBudgetKiB = %d, want %d", got, want)
	}

	// Pools are bounded as configured.
	st := db.Stats()
	if st.Writer.MaxOpenConnections != 1 {
		t.Errorf("writer pool max = %d, want exactly 1", st.Writer.MaxOpenConnections)
	}
	if st.Readers.MaxOpenConnections != cfg.ReaderConns {
		t.Errorf("reader pool max = %d, want %d", st.Readers.MaxOpenConnections, cfg.ReaderConns)
	}

	// The reader pool cannot write; the failure is SQLite's, not a silent no-op.
	if _, err := db.r.ExecContext(context.Background(), `DELETE FROM notes`); err == nil {
		t.Error("a write on the reader pool succeeded")
	}
	// Foreign keys are enforced on the writer.
	err := db.Tx(context.Background(), func(tx *Tx) error {
		if _, err := tx.ExecContext(context.Background(), `CREATE TABLE fk_parent (id INTEGER PRIMARY KEY)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(context.Background(), `CREATE TABLE fk_child (id INTEGER PRIMARY KEY, p INTEGER NOT NULL REFERENCES fk_parent(id))`); err != nil {
			return err
		}
		_, err := tx.ExecContext(context.Background(), `INSERT INTO fk_child (p) VALUES (42)`)
		return err
	})
	if !IsConstraint(err) {
		t.Errorf("foreign key violation not reported as a constraint error: %v", err)
	}
	// Double-quoted string literals are off: a typo'd identifier is an error.
	if _, err := db.r.QueryContext(context.Background(), `SELECT "no_such_column"`); err == nil {
		t.Error(`"no_such_column" was accepted as a string literal (DQS must be off)`)
	}
}

func TestOpenReopenAndClose(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Path: filepath.Join(dir, "sub", "alixai.db"), Migrations: notesRegistry(t)}
	db, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	insertNote(t, db, testScope("app", "t1", "w1"), "n1", "persist", "me")
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// The last connection closing checkpoints and removes the WAL.
	if _, err := os.Stat(db.Path() + "-wal"); !os.IsNotExist(err) {
		t.Errorf("-wal still present after Close: %v", err)
	}

	db2, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if n := count(t, db2, `SELECT count(*) FROM notes`); n != 1 {
		t.Errorf("rows after reopen = %d, want 1", n)
	}
}

func TestOpenRejectsBadOptions(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]Options{
		"empty path":    {Path: ""},
		"memory":        {Path: ":memory:"},
		"memory in uri": {Path: "file::memory:?cache=shared"},
		"query in path": {Path: filepath.Join(dir, "a.db?mode=ro")},
		"nul":           {Path: filepath.Join(dir, "a\x00.db")},
		"negative cache": {
			Path: filepath.Join(dir, "b.db"), Config: Config{CacheKiB: -1},
		},
		"too many readers": {
			Path: filepath.Join(dir, "c.db"), Config: Config{ReaderConns: 1000},
		},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			o.Migrations = NewRegistry()
			db, err := Open(context.Background(), o)
			if err == nil {
				db.Close()
				t.Fatal("Open succeeded")
			}
			if got := contracts.CodeOf(err); got != contracts.ErrValidationFailed {
				t.Errorf("code = %s, want %s (err: %v)", got, contracts.ErrValidationFailed, err)
			}
		})
	}

	// A path that cannot be created is unavailable, and nothing is left open.
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), Options{Path: filepath.Join(blocker, "x.db"), Migrations: NewRegistry()})
	if got := contracts.CodeOf(err); got != contracts.ErrUnavailable {
		t.Errorf("uncreatable path: code = %s, want %s (err: %v)", got, contracts.ErrUnavailable, err)
	}
}

func TestConfigValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatalf("zero config (all defaults) invalid: %v", err)
	}
	for name, c := range map[string]Config{
		"cache":   {CacheKiB: -5},
		"reader":  {ReaderConns: 33},
		"mmap":    {MmapMiB: 1 << 20},
		"busy":    {BusyTimeoutMs: -1},
		"rcache":  {ReaderCacheKiB: 1 << 30},
		"too big": {CacheKiB: 1<<20 + 1},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, c)
		}
	}
}

func TestTxCommitRollbackAndPanic(t *testing.T) {
	db := openTest(t, nil)
	ctx := context.Background()
	sc := testScope("app", "t1", "w1")

	// Error: rolled back, and the caller's own error comes back unchanged.
	sentinel := contracts.New(contracts.ErrValidationFailed, "nope")
	err := db.Tx(ctx, func(tx *Tx) error {
		if _, err := InsertScoped(ctx, tx, sc, "notes", []string{"id", "title", "body"}, "a", "x", "y"); err != nil {
			return err
		}
		return sentinel
	})
	if err != error(sentinel) {
		t.Errorf("Tx returned %v, want the callback's own error", err)
	}
	if n := count(t, db, `SELECT count(*) FROM notes`); n != 0 {
		t.Errorf("rows after rolled-back Tx = %d", n)
	}

	// Panic: rolled back and propagated.
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Errorf("recover = %v, want the original panic", r)
			}
		}()
		_ = db.Tx(ctx, func(tx *Tx) error {
			_, _ = InsertScoped(ctx, tx, sc, "notes", []string{"id", "title", "body"}, "b", "x", "y")
			panic("boom")
		})
	}()
	if n := count(t, db, `SELECT count(*) FROM notes`); n != 0 {
		t.Errorf("rows after panicking Tx = %d", n)
	}
	// The writer connection was released: a later Tx still works.
	if err := db.Tx(ctx, func(tx *Tx) error {
		_, err := InsertScoped(ctx, tx, sc, "notes", []string{"id", "title", "body"}, "c", "x", "y")
		return err
	}); err != nil {
		t.Fatalf("Tx after panic: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM notes`); n != 1 {
		t.Errorf("rows after commit = %d, want 1", n)
	}

	// View is a read-only snapshot.
	err = db.View(ctx, func(tx *Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM notes`)
		return err
	})
	if err == nil {
		t.Error("write inside View succeeded")
	}

	// A unique violation is classified without importing the driver.
	_, err = InsertScoped(ctx, db, sc, "notes", []string{"id", "title", "body"}, "c", "dup", "dup")
	if !IsUniqueViolation(err) || !IsConstraint(err) {
		t.Errorf("duplicate primary key not classified: %v", err)
	}
}

// holdWriteLock opens a second, independent connection (as another process
// would) and takes the database write lock until release is called.
func holdWriteLock(t *testing.T, db *DB) (release func()) {
	t.Helper()
	pool, err := openPool(db.cfg.dsn(db.path, roleWriter), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.BeginTx(context.Background(), nil)
	if err != nil {
		pool.Close()
		t.Fatalf("blocker BEGIN IMMEDIATE: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = tx.Rollback()
			_ = pool.Close()
		})
	}
	t.Cleanup(release)
	return release
}

func TestTxRetriesBusy(t *testing.T) {
	ctx := context.Background()
	sc := testScope("app", "t1", "w1")
	insert := func(tx *Tx) error {
		_, err := InsertScoped(ctx, tx, sc, "notes", []string{"id", "title", "body"}, "n1", "retry", "me")
		return err
	}

	t.Run("succeeds once the other writer finishes", func(t *testing.T) {
		db := openTest(t, nil, func(o *Options) {
			o.Config.BusyTimeoutMs = 1 // make SQLite give up fast so the helper's loop is what waits
			o.BusyAttempts = 100
			o.BusyBackoff = 5 * time.Millisecond
		})
		release := holdWriteLock(t, db)
		go func() {
			time.Sleep(150 * time.Millisecond)
			release()
		}()
		if err := db.Tx(ctx, insert); err != nil {
			t.Fatalf("Tx did not ride out the lock: %v", err)
		}
		if got := db.Stats().BusyRetries; got == 0 {
			t.Error("BusyRetries = 0: the lock was never contended")
		}
		if n := count(t, db, `SELECT count(*) FROM notes`); n != 1 {
			t.Errorf("rows = %d, want 1 (exactly one committed attempt)", n)
		}
	})

	t.Run("gives up as unavailable", func(t *testing.T) {
		db := openTest(t, nil, func(o *Options) {
			o.Config.BusyTimeoutMs = 1
			o.BusyAttempts = 3
			o.BusyBackoff = 2 * time.Millisecond
		})
		holdWriteLock(t, db)
		err := db.Tx(ctx, insert)
		if got := contracts.CodeOf(err); got != contracts.ErrUnavailable {
			t.Fatalf("code = %s, want %s (err: %v)", got, contracts.ErrUnavailable, err)
		}
		if !IsBusy(err) {
			t.Errorf("the SQLITE_BUSY cause is not reachable from %v", err)
		}
		var ce *contracts.Error
		if errors.As(err, &ce) {
			if !ce.Retryable {
				t.Error("exhausted busy retries should be marked retryable")
			}
			if ce.Details["attempts"] != 3 {
				t.Errorf("Details[attempts] = %v, want 3", ce.Details["attempts"])
			}
		}
		if got := db.Stats().BusyRetries; got != 2 {
			t.Errorf("BusyRetries = %d, want 2 (3 attempts)", got)
		}
	})

	t.Run("a cancelled context stops the backoff", func(t *testing.T) {
		db := openTest(t, nil, func(o *Options) {
			o.Config.BusyTimeoutMs = 1
			o.BusyAttempts = 50
			o.BusyBackoff = time.Second
		})
		holdWriteLock(t, db)
		cctx, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
		defer cancel()
		start := time.Now()
		err := db.Tx(cctx, insert)
		if el := time.Since(start); el > 3*time.Second {
			t.Errorf("Tx took %v after its context expired", el)
		}
		if got := contracts.CodeOf(err); got != contracts.ErrTimeout {
			t.Errorf("code = %s, want %s (err: %v)", got, contracts.ErrTimeout, err)
		}
	})
}

func TestConcurrentReadersSingleWriter(t *testing.T) {
	const readerConns = 3
	db := openTest(t, nil, func(o *Options) { o.Config.ReaderConns = readerConns })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE pairs (id INTEGER PRIMARY KEY, batch INTEGER NOT NULL);
		CREATE TABLE counter (id INTEGER PRIMARY KEY CHECK (id = 1), n INTEGER NOT NULL);
		INSERT INTO counter (id, n) VALUES (1, 0);`); err != nil {
		t.Fatal(err)
	}

	// Phase 1: a write transaction is open and uncommitted. Readers (more of
	// them than pooled connections) must neither block on it nor see its rows,
	// and a second writer must wait for it.
	inside, release := make(chan struct{}), make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- db.Tx(ctx, func(tx *Tx) error {
			for i := 0; i < 2; i++ {
				if _, err := tx.ExecContext(ctx, `INSERT INTO pairs (batch) VALUES (1)`); err != nil {
					return err
				}
			}
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- db.Tx(ctx, func(tx *Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO pairs (batch) VALUES (2), (2)`)
			return err
		})
	}()

	var readWG sync.WaitGroup
	readErrs := make(chan error, 8)
	var peakOpen atomic.Int64
	for r := 0; r < 8; r++ {
		readWG.Add(1)
		go func() {
			defer readWG.Done()
			for i := 0; i < 20; i++ {
				var n int
				if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pairs`).Scan(&n); err != nil {
					readErrs <- err
					return
				}
				if n != 0 {
					readErrs <- errors.New("a reader saw uncommitted rows")
					return
				}
				if open := int64(db.Stats().Readers.OpenConnections); open > peakOpen.Load() {
					peakOpen.Store(open)
				}
			}
		}()
	}
	readersDone := make(chan struct{})
	go func() { readWG.Wait(); close(readersDone) }()
	select {
	case <-readersDone:
	case <-time.After(10 * time.Second):
		t.Fatal("readers blocked behind an open write transaction")
	}
	close(readErrs)
	for err := range readErrs {
		t.Errorf("reader: %v", err)
	}

	select {
	case err := <-secondDone:
		t.Fatalf("second writer finished while the first still held the write lock (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	for i, ch := range []chan error{firstDone, secondDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("writer %d: %v", i+1, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("writer %d never finished", i+1)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM pairs`); n != 4 {
		t.Fatalf("pairs = %d, want 4 after both writers committed", n)
	}
	if got := peakOpen.Load(); got > readerConns {
		t.Errorf("reader pool grew to %d connections, bound is %d", got, readerConns)
	}

	// Phase 2: many writers doing read-modify-write through Tx while readers
	// check atomicity (rows arrive in pairs) and monotonic visibility.
	const writers, perWriter = 8, 40
	var wg sync.WaitGroup
	errs := make(chan error, writers+4)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				err := db.Tx(ctx, func(tx *Tx) error {
					var n int
					if err := tx.QueryRowContext(ctx, `SELECT n FROM counter WHERE id = 1`).Scan(&n); err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, `UPDATE counter SET n = ? WHERE id = 1`, n+1); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx, `INSERT INTO pairs (batch) VALUES (?), (?)`, n, n)
					return err
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	stop := make(chan struct{})
	var rwg sync.WaitGroup
	for r := 0; r < 4; r++ {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			last := -1
			for {
				select {
				case <-stop:
					return
				default:
				}
				err := db.View(ctx, func(tx *Tx) error {
					var n, rows int
					if err := tx.QueryRowContext(ctx, `SELECT n FROM counter WHERE id = 1`).Scan(&n); err != nil {
						return err
					}
					if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pairs`).Scan(&rows); err != nil {
						return err
					}
					if rows != 4+2*n {
						return errors.New("snapshot inconsistent: counter and pairs disagree inside one View")
					}
					if n < last {
						return errors.New("a reader saw the counter go backwards")
					}
					last = n
					return nil
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	rwg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("%v", err)
	}
	if n := count(t, db, `SELECT n FROM counter WHERE id = 1`); n != writers*perWriter {
		t.Errorf("counter = %d, want %d: updates were lost or an error was swallowed", n, writers*perWriter)
	}
	if got := db.Stats().BusyRetries; got != 0 {
		t.Errorf("BusyRetries = %d: a single process must never contend with itself", got)
	}
	if got := db.Stats().Writer.MaxOpenConnections; got != 1 {
		t.Errorf("writer pool max = %d, want 1", got)
	}
}

func TestOptionsFromConfigPath(t *testing.T) {
	o := FromConfig()
	if !strings.HasSuffix(filepath.ToSlash(o.Path), "/"+DefaultFileName) {
		t.Errorf("FromConfig path = %q, want <data_dir>/%s", o.Path, DefaultFileName)
	}
	if o.Config != section {
		t.Errorf("FromConfig did not read the registered section")
	}
}
