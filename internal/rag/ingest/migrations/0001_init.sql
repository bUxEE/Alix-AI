-- Module "ragingest": the durable state of the ingestion pipeline (master plan
-- 12.3, 18, 19). Every table carries application_id, tenant_id and
-- workspace_id; every query filters on them (store.ScopedWhere).
--
-- Times are unix nanoseconds from the injected clock. ModTime and SeenAt of a
-- snapshot record are stored at full precision: the size-and-time shortcut of
-- the source provider compares them for equality, and a coarser copy would
-- only cost a rehash, but a finer one rounded on one side never matches.

-- One row per registered source of a scope: what the last accepted scan found
-- and why the source is, or is not, available. last_success_ns advances only
-- when the root was actually reached and the scan applied (master plan 18: a
-- source that could not be checked is never reported as checked).
CREATE TABLE ragingest_source (
    application_id   TEXT    NOT NULL,
    tenant_id        TEXT    NOT NULL,
    workspace_id     TEXT    NOT NULL,
    source_id        TEXT    NOT NULL,
    -- Digest of the part of the definition that decides what is admitted. A
    -- different digest on the next sync is a re-sync with the new rules.
    fingerprint      TEXT    NOT NULL,
    availability     TEXT    NOT NULL CHECK (availability IN ('available', 'unavailable', 'removed')),
    -- Why the source is not available: root_unreachable, mass_deletion,
    -- limit_exceeded, removed, tenant_deleted. Empty when available.
    reason           TEXT    NOT NULL DEFAULT '',
    error_code       TEXT    NOT NULL DEFAULT '',
    message          TEXT    NOT NULL DEFAULT '',
    since_ns         INTEGER NOT NULL,
    -- Bumped each time a scan's snapshot is persisted. A scan persists only if
    -- the value is still the one it read, so two concurrent scans cannot
    -- overwrite each other's snapshot.
    snapshot_seq     INTEGER NOT NULL DEFAULT 0,
    live_items       INTEGER NOT NULL DEFAULT 0,
    last_scan_ns     INTEGER NOT NULL DEFAULT 0,
    last_success_ns  INTEGER NOT NULL DEFAULT 0,
    -- The figures of the last accepted scan (source.Counts), for the status.
    scan_discovered  INTEGER NOT NULL DEFAULT 0,
    scan_skipped     INTEGER NOT NULL DEFAULT 0,
    scan_failed      INTEGER NOT NULL DEFAULT 0,
    scan_tombstoned  INTEGER NOT NULL DEFAULT 0,
    -- What the declaration says about deleting this source's derived data
    -- (purge or disable), as of the last scan, so a removal needs no registry.
    deletion_mode    TEXT    NOT NULL DEFAULT 'purge',
    purge_after_ns   INTEGER NOT NULL DEFAULT 0,
    -- The scan that was refused as a mass deletion, until it is confirmed,
    -- superseded by a scan that no longer deletes, or the source is removed.
    hold_digest      TEXT    NOT NULL DEFAULT '',
    hold_deleted     INTEGER NOT NULL DEFAULT 0,
    hold_previous    INTEGER NOT NULL DEFAULT 0,
    hold_why         TEXT    NOT NULL DEFAULT '',
    hold_ns          INTEGER NOT NULL DEFAULT 0,
    -- An operator's confirmation of exactly that deletion set. It is single
    -- use and expires.
    confirm_digest   TEXT    NOT NULL DEFAULT '',
    confirm_by       TEXT    NOT NULL DEFAULT '',
    confirm_until_ns INTEGER NOT NULL DEFAULT 0,
    created_ns       INTEGER NOT NULL,
    updated_ns       INTEGER NOT NULL,
    PRIMARY KEY (application_id, tenant_id, workspace_id, source_id)
) WITHOUT ROWID;

-- The snapshot: the live records after the last accepted scan, which is what
-- the provider's Discover takes as its previous argument.
CREATE TABLE ragingest_item (
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    source_id      TEXT    NOT NULL,
    item_key       TEXT    NOT NULL,
    size           INTEGER NOT NULL,
    mod_ns         INTEGER NOT NULL,
    hash           TEXT    NOT NULL,
    mime           TEXT    NOT NULL,
    seen_ns        INTEGER NOT NULL,
    PRIMARY KEY (application_id, tenant_id, workspace_id, source_id, item_key)
) WITHOUT ROWID;

-- The index generations this module opened, and how they are being built.
-- lane is ingest (priority class 5) or reindex (class 6: a rebuild because a
-- component version changed); seeded says every source version of the build
-- has been created, so "nothing left to do" can be trusted; vectors_dropped
-- says the embeddings of a retired generation were removed, which has to be
-- remembered because compaction later removes the generation row from the
-- index and with it the means to find out.
CREATE TABLE ragingest_generation (
    application_id  TEXT    NOT NULL,
    tenant_id       TEXT    NOT NULL,
    workspace_id    TEXT    NOT NULL,
    source_id       TEXT    NOT NULL,
    generation_id   TEXT    NOT NULL,
    lane            TEXT    NOT NULL CHECK (lane IN ('ingest', 'reindex')),
    seeded          INTEGER NOT NULL DEFAULT 0 CHECK (seeded IN (0, 1)),
    vectors_dropped INTEGER NOT NULL DEFAULT 0 CHECK (vectors_dropped IN (0, 1)),
    created_ns      INTEGER NOT NULL,
    PRIMARY KEY (application_id, tenant_id, workspace_id, generation_id)
) WITHOUT ROWID;
CREATE INDEX ragingest_generation_source
    ON ragingest_generation (application_id, tenant_id, workspace_id, source_id);

-- An immutable source version bound to the generation it is being indexed
-- into (master plan 12.3: "immutable source version and hash"). The content
-- address, size, time and sniffed type never change after the row is written;
-- only the progress columns do. A changed item gets a new row and the old one
-- is superseded. Several rows share a hash when one document is indexed into
-- two generations.
CREATE TABLE ragingest_version (
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    version_id     TEXT    NOT NULL,
    source_id      TEXT    NOT NULL,
    generation_id  TEXT    NOT NULL,
    item_key       TEXT    NOT NULL,
    document_id    TEXT    NOT NULL,
    hash           TEXT    NOT NULL,
    size           INTEGER NOT NULL,
    mod_ns         INTEGER NOT NULL,
    mime           TEXT    NOT NULL,
    format         TEXT    NOT NULL,
    -- pending (parse queued) -> parsed (staged) -> indexed; failed and skipped
    -- are final for this version; superseded is a version that was replaced,
    -- withdrawn or found stale (error_reason says which).
    state          TEXT    NOT NULL CHECK (state IN ('pending', 'parsed', 'indexed', 'failed', 'skipped', 'superseded')),
    error_code     TEXT    NOT NULL DEFAULT '',
    error_reason   TEXT    NOT NULL DEFAULT '',
    error_message  TEXT    NOT NULL DEFAULT '',
    chunks         INTEGER NOT NULL DEFAULT 0,
    -- The ids of the child chunks the index holds for this version (a JSON
    -- array): the map the vector index needs, which does not follow the
    -- full-text index's tombstones by itself.
    chunk_ids      TEXT    NOT NULL DEFAULT '[]',
    -- 1 while embeddings of this row's chunks may still sit in the vector
    -- index although the row no longer serves them (it was replaced by an
    -- indexed successor, or its item was deleted). Cleared once they are
    -- removed, except those a live successor holds under the same chunk id.
    vec_owed       INTEGER NOT NULL DEFAULT 0 CHECK (vec_owed IN (0, 1)),
    embed_state    TEXT    NOT NULL DEFAULT 'none' CHECK (embed_state IN ('none', 'pending', 'done')),
    job_id         TEXT    NOT NULL DEFAULT '',
    requeues       INTEGER NOT NULL DEFAULT 0,
    created_ns     INTEGER NOT NULL,
    updated_ns     INTEGER NOT NULL,
    PRIMARY KEY (application_id, tenant_id, workspace_id, version_id)
) WITHOUT ROWID;
-- One current version per document and generation.
CREATE UNIQUE INDEX ragingest_version_current
    ON ragingest_version (application_id, tenant_id, workspace_id, generation_id, item_key)
    WHERE state <> 'superseded';
CREATE INDEX ragingest_version_work
    ON ragingest_version (application_id, tenant_id, workspace_id, source_id, state)
    WHERE state IN ('pending', 'parsed');
CREATE INDEX ragingest_version_generation
    ON ragingest_version (application_id, tenant_id, workspace_id, generation_id, state);
CREATE INDEX ragingest_version_item
    ON ragingest_version (application_id, tenant_id, workspace_id, source_id, item_key);
CREATE INDEX ragingest_version_owed
    ON ragingest_version (application_id, tenant_id, workspace_id, source_id)
    WHERE vec_owed = 1;

-- The embedding batches of an indexed version: one row per embedding job, so
-- "every chunk of this generation has its embedding" is a count, and a job
-- that is delivered twice is counted once.
CREATE TABLE ragingest_embed (
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    version_id     TEXT    NOT NULL,
    batch_no       INTEGER NOT NULL,
    chunk_count    INTEGER NOT NULL,
    embedded       INTEGER NOT NULL DEFAULT 0,
    -- 0 while the batch is waiting, the completion time when it ran, and -1
    -- when it was given up after maxRequeues failed jobs (the chunks stay
    -- full-text only).
    done_ns        INTEGER NOT NULL DEFAULT 0,
    job_id         TEXT    NOT NULL DEFAULT '',
    requeues       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (application_id, tenant_id, workspace_id, version_id, batch_no)
) WITHOUT ROWID;

-- The deletions to apply to the index and the vector index: an outbox written
-- in the same transaction that removes the item from the snapshot, so a crash
-- between "forgotten" and "hidden" cannot leave a document retrievable
-- (SEC-028). applied_ns is set once both were done; the row stays as the
-- record of the deletion until it is purged.
CREATE TABLE ragingest_tombstone (
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    source_id      TEXT    NOT NULL,
    item_key       TEXT    NOT NULL,
    document_id    TEXT    NOT NULL,
    reason         TEXT    NOT NULL,
    last_hash      TEXT    NOT NULL,
    at_ns          INTEGER NOT NULL,
    purge_after_ns INTEGER NOT NULL DEFAULT 0,
    applied_ns     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (application_id, tenant_id, workspace_id, source_id, item_key, at_ns)
) WITHOUT ROWID;
CREATE INDEX ragingest_tombstone_open
    ON ragingest_tombstone (application_id, tenant_id, workspace_id, source_id)
    WHERE applied_ns = 0;

-- The parse result of a version, between the parse job and the index job. It
-- exists only while the index job has not run; the index job deletes it, and
-- so does anything that supersedes or fails the version.
CREATE TABLE ragingest_stage (
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    version_id     TEXT    NOT NULL,
    body           BLOB    NOT NULL,
    created_ns     INTEGER NOT NULL,
    PRIMARY KEY (application_id, tenant_id, workspace_id, version_id)
) WITHOUT ROWID;
