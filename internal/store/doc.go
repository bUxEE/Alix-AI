// Package store is AlixAI's embedded store: one SQLite database in WAL mode
// (master plan §5.1, §6) behind a small, driver-free API. It is the only
// package that imports modernc.org/sqlite (ADR 0009); every other package sees
// DB, Tx, the scoped query helpers and the Is* error predicates.
//
// # Connections
//
// Open starts exactly one writer connection and a bounded pool of read-only
// connections (Config.ReaderConns). WAL lets readers run while the writer
// holds an open transaction, and a single writer connection serialises writers
// inside the process, so a busy database is the exception (another process
// holding the write lock) rather than the rule. Every connection gets the same
// hardening pragmas: busy_timeout, foreign_keys, synchronous=NORMAL, a bounded
// page cache, mmap off, trusted_schema off, double-quoted-string literals off
// and SQLite's defensive mode. Open fails closed when WAL cannot be enabled.
//
// The page cache is per connection, so the real memory ceiling is
// Config.PageCacheBudgetKiB: the writer cache plus ReaderConns reader caches.
//
// # Transactions
//
// DB.Tx runs fn in a write transaction (BEGIN IMMEDIATE) and DB.View runs fn in
// a read-only snapshot. Both retry the whole attempt, with bounded, jittered,
// context-aware backoff, when SQLite reports SQLITE_BUSY, and give up with
// contracts.ErrUnavailable. fn may therefore run more than once and must not
// have side effects outside the transaction. fn receives only the Tx; using
// the DB's own methods inside fn for writes would wait for the writer
// connection that fn is already holding.
//
// # Migrations
//
// Each module embeds its migrations and registers them:
//
//	//go:embed migrations/*.sql
//	var migrations embed.FS
//
//	func init() { store.RegisterMigrations("rag", migrations) }
//
// Files live under migrations/ and are named NNNN_name.sql (four digits, the
// version, contiguous from 0001). Open applies modules in registration order
// and versions in order, each migration and its schema_migrations row in one
// transaction. A recorded checksum that differs from the embedded file, or a
// recorded version the binary no longer embeds, aborts Open with
// contracts.ErrInternal before anything is applied: an applied migration is
// never edited, a new one is added.
//
// # Scope
//
// Every table carries application_id, tenant_id and workspace_id (NOT NULL,
// TEXT) and every query filters on them (master plan §10.2). ScopedWhere,
// ScopedStmt, QueryScoped, ExecScoped and InsertScoped build those predicates
// from a validated contracts.Scope and refuse to run without one. The
// storetest subpackage has the lint helper a module's tests call to assert its
// tables comply.
//
// # Times and IDs
//
// Persist times from the injected clock.Clock as UTC: either unix
// milliseconds in an INTEGER column or RFC3339Nano UTC in a TEXT column. The
// connections are set to UTC so a stray time.Time argument is at least
// sortable. IDs come from internal/ids and are stored as TEXT.
package store
