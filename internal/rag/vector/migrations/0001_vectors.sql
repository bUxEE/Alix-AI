-- ragvector: the embedded vector index (ADR 0011, option A: an exact flat scan
-- over int8 codes). Every table carries the three scope columns and every query
-- filters on them (master plan 10.2).
--
-- vector_generations: one row per index generation of a source in a scope.
-- A generation is fixed to the source, embedding model and dimension of its
-- first batch. vectors is a running count (kept inside the writing
-- transaction) used for the ceiling; rev changes on every write to the
-- generation, so a resident cache can tell, in the same read snapshot as its
-- scan, whether what it holds is still current. state is 'live', or 'dropping'
-- while the generation is being deleted: its rows go in bounded batches so one
-- delete cannot hold the store's single writer for seconds (100,000 vectors
-- take about two seconds in one transaction), and a dropping generation is
-- invisible at once to search, stats, reuse and upserts.
CREATE TABLE vector_generations (
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    generation_id  TEXT    NOT NULL,
    source_id      TEXT    NOT NULL,
    model_id       TEXT    NOT NULL,
    dim            INTEGER NOT NULL CHECK (dim > 0),
    vectors        INTEGER NOT NULL DEFAULT 0 CHECK (vectors >= 0),
    rev            INTEGER NOT NULL DEFAULT 0,
    state          TEXT    NOT NULL DEFAULT 'live' CHECK (state IN ('live', 'dropping')),
    created_ms     INTEGER NOT NULL,
    PRIMARY KEY (application_id, tenant_id, workspace_id, generation_id)
) WITHOUT ROWID;

CREATE INDEX vector_generations_source
    ON vector_generations (application_id, tenant_id, workspace_id, source_id);

-- vector_rows: one row per chunk vector. The primary key starts with the scope
-- and the generation, so one generation of one scope is a single contiguous
-- key range: a scan reads exactly that range, never another scope's rows, and
-- WITHOUT ROWID keeps the vector in the clustered key order (a rowid table with
-- a secondary index measured 5x slower when scopes interleave).
--
-- code is dim signed bytes (int8). inv_norm is the reciprocal of the code's
-- length, so a score is dot(unit query, code) * inv_norm. input_key is the
-- SHA-256 reuse key of the exact embedding input, scope and model (SEC-023).
-- Reuse finds a vector by the SAME chunk id in another live generation of the
-- same scope and compares input_key, so it needs no index of its own: an index
-- on a hash is random-access, and measured it made a 100,000-row delete about
-- four times slower and the database 25 percent larger (docs/efficiency/M2-07.md).
-- The key dies with the row, so a deleted chunk leaves no reusable embedding
-- behind.
CREATE TABLE vector_rows (
    application_id TEXT NOT NULL,
    tenant_id      TEXT NOT NULL,
    workspace_id   TEXT NOT NULL,
    generation_id  TEXT NOT NULL,
    chunk_id       TEXT NOT NULL,
    input_key      BLOB NOT NULL CHECK (length(input_key) = 32),
    inv_norm       REAL NOT NULL,
    code           BLOB NOT NULL,
    PRIMARY KEY (application_id, tenant_id, workspace_id, generation_id, chunk_id),
    FOREIGN KEY (application_id, tenant_id, workspace_id, generation_id)
        REFERENCES vector_generations (application_id, tenant_id, workspace_id, generation_id)
        ON DELETE CASCADE
) WITHOUT ROWID;
