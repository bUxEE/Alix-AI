-- Module "ragindex": the scoped lexical index of the RAG pipeline (master plan
-- 12.3, 12.4, 19). Every table carries application_id, tenant_id and
-- workspace_id; every query filters on them.
--
-- Isolation design (see doc.go): the full-text index is ONE FTS5 table, but
-- each workspace scope owns a contiguous rowid range (scope_no << 36 | seq),
-- so a query constrains `rowid BETWEEN lo AND hi` inside the FTS5 scan itself.
-- A filter on an UNINDEXED FTS5 column is applied only after the match and was
-- measured at p95 ~190 ms for 100k rows (M0-05); a rowid range is pushed into
-- the index scan and shrinks the work to the scope's own rows.

-- One row per (application, tenant, workspace) that ever held index data. It
-- allocates the rowid range and carries the epoch, a counter bumped by every
-- change that can alter what a query in this scope returns (cache keys use it).
CREATE TABLE ragindex_scope (
    scope_no       INTEGER PRIMARY KEY AUTOINCREMENT,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    next_seq       INTEGER NOT NULL DEFAULT 1,
    epoch          INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT    NOT NULL,
    UNIQUE (application_id, tenant_id, workspace_id)
);

-- One index generation of one source (schemas/rag/index-generation). The state
-- machine is building -> validating -> active -> superseded, with failed and
-- invalidated as terminal side states. The partial unique index below is the
-- active pointer: at most one active generation per source, enforced by the
-- database, flipped in a single transaction.
CREATE TABLE ragindex_generation (
    generation_id   TEXT    PRIMARY KEY,
    application_id  TEXT    NOT NULL,
    tenant_id       TEXT    NOT NULL,
    workspace_id    TEXT    NOT NULL,
    source_id       TEXT    NOT NULL,
    state           TEXT    NOT NULL CHECK (state IN ('building', 'validating', 'active', 'superseded', 'failed', 'invalidated')),
    components      TEXT    NOT NULL,
    documents       INTEGER NOT NULL DEFAULT 0 CHECK (documents >= 0),
    chunks          INTEGER NOT NULL DEFAULT 0 CHECK (chunks >= 0),
    embedded_chunks INTEGER NOT NULL DEFAULT 0 CHECK (embedded_chunks >= 0),
    created_at      TEXT    NOT NULL,
    activated_at    TEXT,
    retired_at      TEXT,
    error_code      TEXT,
    error_message   TEXT
);
CREATE UNIQUE INDEX ragindex_generation_active
    ON ragindex_generation (application_id, tenant_id, workspace_id, source_id)
    WHERE state = 'active';
CREATE INDEX ragindex_generation_source
    ON ragindex_generation (application_id, tenant_id, workspace_id, source_id, state);
-- At most one build in flight per source: a second one must wait for, or fail,
-- the first, so an abandoned build is never silently accumulated.
CREATE UNIQUE INDEX ragindex_generation_building
    ON ragindex_generation (application_id, tenant_id, workspace_id, source_id)
    WHERE state IN ('building', 'validating');

-- A source item (document) at an indexed version, inside one generation.
CREATE TABLE ragindex_document (
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    generation_id  TEXT    NOT NULL,
    source_id      TEXT    NOT NULL,
    document_id    TEXT    NOT NULL,
    version        TEXT    NOT NULL,
    chunks         INTEGER NOT NULL CHECK (chunks >= 0),
    created_at     TEXT    NOT NULL,
    tombstoned_at  TEXT,
    PRIMARY KEY (generation_id, document_id)
) WITHOUT ROWID;
CREATE INDEX ragindex_document_source
    ON ragindex_document (application_id, tenant_id, workspace_id, source_id, document_id);

-- Parent chunks (sections). Not full-text indexed: retrieval finds children,
-- parent expansion reads these by id under the same scope and visibility.
CREATE TABLE ragindex_parent (
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    generation_id  TEXT    NOT NULL,
    source_id      TEXT    NOT NULL,
    document_id    TEXT    NOT NULL,
    chunk_id       TEXT    NOT NULL,
    kind           TEXT    NOT NULL,
    ordinal        INTEGER NOT NULL,
    part           INTEGER NOT NULL,
    text           TEXT    NOT NULL,
    section        TEXT    NOT NULL,
    path           TEXT    NOT NULL,
    span_start     INTEGER NOT NULL,
    span_end       INTEGER NOT NULL,
    page           INTEGER NOT NULL,
    page_end       INTEGER NOT NULL,
    est_toks       INTEGER NOT NULL,
    hidden         INTEGER NOT NULL CHECK (hidden IN (0, 1)),
    tombstoned_at  TEXT,
    PRIMARY KEY (generation_id, document_id, chunk_id)
) WITHOUT ROWID;
CREATE INDEX ragindex_parent_doc
    ON ragindex_parent (application_id, tenant_id, workspace_id, source_id, document_id, generation_id);

-- Child chunks: the retrieval unit. rid is the FTS5 rowid and carries the scope
-- number in its high bits (scope_no << 36 | seq). path is a JSON array of the
-- heading breadcrumb (written without HTML escaping); it is indexed as is, the
-- brackets and quotes being separators to the tokenizer. Chunk text is only
-- ever read through this table under a scope predicate; the FTS5 table below is
-- external-content and holds only the index, never a second copy of the text.
-- The columns a query filters on come first and the long text last, so the
-- visibility check reads the first page of the row and never an overflow page.
CREATE TABLE ragindex_chunk (
    rid            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    generation_id  TEXT    NOT NULL,
    tombstoned_at  TEXT,
    hidden         INTEGER NOT NULL CHECK (hidden IN (0, 1)),
    split          INTEGER NOT NULL CHECK (split IN (0, 1)),
    source_id      TEXT    NOT NULL,
    document_id    TEXT    NOT NULL,
    chunk_id       TEXT    NOT NULL,
    parent_id      TEXT    NOT NULL,
    kind           TEXT    NOT NULL,
    ordinal        INTEGER NOT NULL,
    span_start     INTEGER NOT NULL,
    span_end       INTEGER NOT NULL,
    page           INTEGER NOT NULL,
    page_end       INTEGER NOT NULL,
    est_toks       INTEGER NOT NULL,
    content_hash   TEXT    NOT NULL,
    section        TEXT    NOT NULL,
    header         TEXT    NOT NULL,
    path           TEXT    NOT NULL,
    text           TEXT    NOT NULL
);
CREATE INDEX ragindex_chunk_doc
    ON ragindex_chunk (application_id, tenant_id, workspace_id, source_id, document_id, generation_id);
-- A chunk id (a content and position hash) is unique among the live rows of a
-- generation; tombstoned leftovers of an earlier version may repeat it.
CREATE UNIQUE INDEX ragindex_chunk_live
    ON ragindex_chunk (generation_id, chunk_id, document_id)
    WHERE tombstoned_at IS NULL;
CREATE INDEX ragindex_chunk_tombstoned
    ON ragindex_chunk (application_id, tenant_id, workspace_id, tombstoned_at)
    WHERE tombstoned_at IS NOT NULL;

-- Exact identifiers (codes, ids, SKUs) found in a chunk, normalised (see
-- ident.go). The lookup is a B-tree range scan inside one scope: independent of
-- corpus size and of the tokenizer.
CREATE TABLE ragindex_ident (
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    ident          TEXT    NOT NULL,
    rid            INTEGER NOT NULL,
    PRIMARY KEY (application_id, tenant_id, workspace_id, ident, rid)
) WITHOUT ROWID;
CREATE INDEX ragindex_ident_rid ON ragindex_ident (rid);

-- The lexical index. Heading text lives only in the PARENT chunk, so each
-- child is indexed with its section title, heading path and table header next
-- to its own text, or a query on a heading or column term would miss it.
-- The scope and source columns are UNINDEXED: they exist so the table obeys the
-- scope rule and so a row read back from the index names its owner. They are
-- never used to filter; the rowid range and the content-table join do that.
CREATE VIRTUAL TABLE ragindex_fts USING fts5 (
    text, section, path, header,
    application_id UNINDEXED, tenant_id UNINDEXED, workspace_id UNINDEXED, source_id UNINDEXED,
    content = 'ragindex_chunk', content_rowid = 'rid',
    tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER ragindex_chunk_ai AFTER INSERT ON ragindex_chunk BEGIN
    INSERT INTO ragindex_fts (rowid, text, section, path, header, application_id, tenant_id, workspace_id, source_id)
    VALUES (new.rid, new.text, new.section, new.path, new.header, new.application_id, new.tenant_id, new.workspace_id, new.source_id);
END;

CREATE TRIGGER ragindex_chunk_ad AFTER DELETE ON ragindex_chunk BEGIN
    INSERT INTO ragindex_fts (ragindex_fts, rowid, text, section, path, header, application_id, tenant_id, workspace_id, source_id)
    VALUES ('delete', old.rid, old.text, old.section, old.path, old.header, old.application_id, old.tenant_id, old.workspace_id, old.source_id);
END;

CREATE TRIGGER ragindex_chunk_au AFTER UPDATE OF text, section, path, header ON ragindex_chunk BEGIN
    INSERT INTO ragindex_fts (ragindex_fts, rowid, text, section, path, header, application_id, tenant_id, workspace_id, source_id)
    VALUES ('delete', old.rid, old.text, old.section, old.path, old.header, old.application_id, old.tenant_id, old.workspace_id, old.source_id);
    INSERT INTO ragindex_fts (rowid, text, section, path, header, application_id, tenant_id, workspace_id, source_id)
    VALUES (new.rid, new.text, new.section, new.path, new.header, new.application_id, new.tenant_id, new.workspace_id, new.source_id);
END;

-- Identifier rows follow their chunk, so deleting a chunk (compaction, a
-- replaced document) can never leave an identifier pointing at a dead rid.
CREATE TRIGGER ragindex_chunk_ad_ident AFTER DELETE ON ragindex_chunk BEGIN
    DELETE FROM ragindex_ident WHERE rid = old.rid;
END;
