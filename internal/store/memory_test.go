package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The footprint probe measures what the store costs a Lite deployment: the
// resident memory of open + migrate + N inserts + FTS queries, with the real
// page-cache settings. It runs in a child process (this test binary,
// re-executed) so the numbers are the store's, not the accumulated state of the
// rest of the test run, and so the OS-level peak is meaningful.
//
// Reproduce the figures in docs/efficiency/M0-05.md with:
//
//	ALIXAI_PROBE_ROWS=10000 go test -run TestMemoryFootprint -v ./internal/store/
//
// Knobs (all optional): ALIXAI_PROBE_ROWS, ALIXAI_PROBE_CACHE_KIB,
// ALIXAI_PROBE_READER_CACHE_KIB, ALIXAI_PROBE_READERS, ALIXAI_PROBE_MMAP_MIB.

type probeResult struct {
	Rows           int `json:"rows"`
	CacheKiB       int `json:"cache_kib"`
	ReaderCacheKiB int `json:"reader_cache_kib"`
	Readers        int `json:"readers"`
	MmapMiB        int `json:"mmap_mib"`

	// Bytes, from the OS.
	BaselineRSS     uint64 `json:"baseline_rss"`
	AfterOpenRSS    uint64 `json:"after_open_rss"`
	AfterInsertRSS  uint64 `json:"after_insert_rss"`
	AfterQueriesRSS uint64 `json:"after_queries_rss"`
	AfterBackupRSS  uint64 `json:"after_backup_rss"`
	PeakRSS         uint64 `json:"peak_rss"`
	PrivateAfter    uint64 `json:"private_after"`
	GoHeapInuse     uint64 `json:"go_heap_inuse"`
	DBBytes         int64  `json:"db_bytes"`
	WALBytes        int64  `json:"wal_bytes"`
	MeasuredRSS     bool   `json:"measured_rss"`

	OpenMigrateMs  float64 `json:"open_migrate_ms"`
	InsertTxMs     float64 `json:"insert_tx_ms"`
	InsertPerSec   float64 `json:"insert_per_sec"`
	PlainPerSec    float64 `json:"plain_insert_per_sec"`
	BackupMs       float64 `json:"backup_ms"`
	Autocommit1kMs float64 `json:"autocommit_1k_ms"`
	QueryP50us     float64 `json:"fts_query_p50_us"`
	QueryP95us     float64 `json:"fts_query_p95_us"`
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func fileSize(path string) int64 {
	if st, err := os.Stat(path); err == nil {
		return st.Size()
	}
	return 0
}

// TestMemoryProbeChild is the measuring half. It does nothing unless the
// parent sets ALIXAI_PROBE_OUT, so a plain `go test` run passes it instantly.
func TestMemoryProbeChild(t *testing.T) {
	out := os.Getenv("ALIXAI_PROBE_OUT")
	if out == "" {
		return
	}
	ctx := context.Background()
	res := probeResult{
		Rows:           envInt("ALIXAI_PROBE_ROWS", 10000),
		CacheKiB:       envInt("ALIXAI_PROBE_CACHE_KIB", DefaultCacheKiB),
		ReaderCacheKiB: envInt("ALIXAI_PROBE_READER_CACHE_KIB", DefaultReaderCacheKiB),
		Readers:        envInt("ALIXAI_PROBE_READERS", DefaultReaderConns),
		MmapMiB:        envInt("ALIXAI_PROBE_MMAP_MIB", 0),
	}
	var ok bool
	res.BaselineRSS, _, _, ok = processMemory()
	res.MeasuredRSS = ok

	path := filepath.Join(t.TempDir(), "probe.db")
	start := time.Now()
	db, err := Open(ctx, Options{
		Path: path, Migrations: notesRegistry(t),
		Config: Config{CacheKiB: res.CacheKiB, ReaderCacheKiB: res.ReaderCacheKiB, ReaderConns: res.Readers, MmapMiB: res.MmapMiB},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	res.OpenMigrateMs = ms(time.Since(start))
	res.AfterOpenRSS, _, _, _ = processMemory()

	// Deterministic corpus: 30 words drawn from a 3000-word vocabulary with a
	// skewed (Zipf-like) distribution, about 200 bytes a row, over 5 tenants.
	rng := rand.New(rand.NewPCG(1, 2))
	vocab := make([]string, 3000)
	for i := range vocab {
		vocab[i] = fmt.Sprintf("w%dx", i)
	}
	zipf := rand.NewZipf(rng, 1.2, 4, uint64(len(vocab)-1))
	body := func() string {
		var b strings.Builder
		for i := 0; i < 30; i++ {
			b.WriteString(vocab[zipf.Uint64()])
			b.WriteByte(' ')
		}
		return b.String()
	}
	tenants := []string{"t0", "t1", "t2", "t3", "t4"}

	start = time.Now()
	if err := db.Tx(ctx, func(tx *Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO notes (id, application_id, tenant_id, workspace_id, title, body) VALUES (?, 'app', ?, 'w1', ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := 0; i < res.Rows; i++ {
			if _, err := stmt.ExecContext(ctx, fmt.Sprintf("n%07d", i), tenants[i%len(tenants)], "title "+vocab[zipf.Uint64()], body()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	el := time.Since(start)
	res.InsertTxMs = ms(el)
	res.InsertPerSec = float64(res.Rows) / el.Seconds()
	res.AfterInsertRSS, _, _, _ = processMemory()

	// The same volume into a plain scoped table (no FTS triggers), to separate
	// the driver's insert cost from the FTS5 indexing cost.
	if _, err := db.ExecContext(ctx, `CREATE TABLE plain (id TEXT PRIMARY KEY, application_id TEXT NOT NULL, tenant_id TEXT NOT NULL, workspace_id TEXT NOT NULL, body TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if err := db.Tx(ctx, func(tx *Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO plain (id, application_id, tenant_id, workspace_id, body) VALUES (?, 'app', ?, 'w1', ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := 0; i < res.Rows; i++ {
			if _, err := stmt.ExecContext(ctx, fmt.Sprintf("p%07d", i), tenants[i%len(tenants)], body()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res.PlainPerSec = float64(res.Rows) / time.Since(start).Seconds()

	// Durable single-row commits, the interactive-path cost (synchronous=NORMAL).
	start = time.Now()
	for i := 0; i < 1000; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO notes (id, application_id, tenant_id, workspace_id, title, body) VALUES (?, 'app', 't0', 'w1', 'auto', ?)`,
			fmt.Sprintf("a%07d", i), body()); err != nil {
			t.Fatal(err)
		}
	}
	res.Autocommit1kMs = ms(time.Since(start))

	// BM25 queries scoped to one tenant, spread over all reader connections.
	// Query terms come from the corpus distribution (common terms, so the
	// matches are real), drawn up front because the generator is not
	// goroutine-safe.
	const queries = 400
	terms := make([]string, queries)
	for i := range terms {
		terms[i] = vocab[zipf.Uint64()] + " AND " + vocab[zipf.Uint64()]
	}
	lat := make([]float64, queries)
	var wg sync.WaitGroup
	jobs := make(chan int)
	for w := 0; w < res.Readers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				q := terms[i]
				t0 := time.Now()
				rows, err := db.QueryContext(ctx,
					`SELECT n.id FROM notes_fts JOIN notes n ON n.rowid = notes_fts.rowid
					 WHERE notes_fts MATCH ? AND notes_fts.tenant_id = ? ORDER BY bm25(notes_fts) LIMIT 10`, q, tenants[i%len(tenants)])
				if err != nil {
					t.Error(err)
					return
				}
				for rows.Next() {
				}
				rows.Close()
				lat[i] = float64(time.Since(t0).Nanoseconds()) / 1000
			}
		}()
	}
	for i := 0; i < queries; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	sort.Float64s(lat)
	res.QueryP50us, res.QueryP95us = lat[queries/2], lat[queries*95/100]

	res.AfterQueriesRSS, _, _, _ = processMemory()

	// Online backup (VACUUM INTO + integrity check + sync) of the loaded database.
	start = time.Now()
	if err := db.Backup(ctx, filepath.Join(t.TempDir(), "probe-backup.db")); err != nil {
		t.Fatal(err)
	}
	res.BackupMs = ms(time.Since(start))

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	res.GoHeapInuse = memStats.HeapInuse
	res.AfterBackupRSS, res.PeakRSS, res.PrivateAfter, _ = processMemory()
	res.DBBytes, res.WALBytes = fileSize(path), fileSize(path+"-wal")

	b, _ := json.Marshal(res)
	if err := os.WriteFile(out, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func mib(b uint64) float64 { return float64(b) / (1 << 20) }

// TestMemoryFootprint runs the probe in a child process and holds the
// Lite-profile defaults to a ceiling, so a change that quietly inflates the
// page caches fails here rather than in the field.
func TestMemoryFootprint(t *testing.T) {
	if raceEnabled {
		t.Log("built with -race: its memory and speed overhead would make the footprint figures and the ceiling meaningless; measured in the normal build")
		return
	}
	out := filepath.Join(t.TempDir(), "probe.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestMemoryProbeChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ALIXAI_PROBE_OUT="+out)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("probe child failed: %v\n%s", err, o)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("probe wrote no result: %v", err)
	}
	var r probeResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	t.Logf("rows=%d cache=%dKiB reader_cache=%dKiB readers=%d mmap=%dMiB", r.Rows, r.CacheKiB, r.ReaderCacheKiB, r.Readers, r.MmapMiB)
	t.Logf("open+migrate %.1f ms; %d inserts in one tx with FTS5 triggers %.0f ms (%.0f rows/s), plain table %.0f rows/s; 1000 autocommit inserts %.0f ms (%.2f ms each)",
		r.OpenMigrateMs, r.Rows, r.InsertTxMs, r.InsertPerSec, r.PlainPerSec, r.Autocommit1kMs, r.Autocommit1kMs/1000)
	t.Logf("FTS5 BM25 top-10, tenant-scoped, %d readers: p50 %.0f us, p95 %.0f us", r.Readers, r.QueryP50us, r.QueryP95us)
	t.Logf("db %.1f MiB, wal %.1f MiB, go heap in use %.1f MiB; online backup %.0f ms", mib(uint64(r.DBBytes)), mib(uint64(r.WALBytes)), mib(r.GoHeapInuse), r.BackupMs)
	if !r.MeasuredRSS {
		t.Log("no OS memory counters on this platform; skipping the RSS ceiling")
		return
	}
	t.Logf("RSS MiB: baseline %.1f -> after open %.1f -> after inserts %.1f -> after queries %.1f -> after backup %.1f; peak (whole run, backup included) %.1f; private %.1f",
		mib(r.BaselineRSS), mib(r.AfterOpenRSS), mib(r.AfterInsertRSS), mib(r.AfterQueriesRSS), mib(r.AfterBackupRSS), mib(r.PeakRSS), mib(r.PrivateAfter))

	// Ceiling for the defaults (8 MB writer cache + 4 x 2 MB reader caches = 16
	// MB of page cache): the store may add at most this much resident memory
	// over the bare test process. Generous on purpose; the measured figure is
	// in the efficiency review.
	const ceilingMiB = 96
	if r.Rows != 10000 || r.CacheKiB != DefaultCacheKiB || r.ReaderCacheKiB != DefaultReaderCacheKiB || r.Readers != DefaultReaderConns || r.MmapMiB != 0 {
		t.Log("non-default probe settings: not applying the ceiling")
		return
	}
	if growth := mib(r.PeakRSS) - mib(r.BaselineRSS); growth > ceilingMiB {
		t.Errorf("store grew the process by %.1f MiB at peak, ceiling %d MiB", growth, ceilingMiB)
	}
}
