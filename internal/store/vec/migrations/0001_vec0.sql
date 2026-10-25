-- ragvector_vec0: the sqlite-vec (vec0) candidate for the vector index
-- (ADR 0011, option B). It is created only by a process that registers this
-- module on purpose (vec.Register); the default registry never contains it.
--
-- vecb_vectors holds the vectors. The four partition keys are the scope and the
-- generation: vec0 keeps each distinct combination in its own chunks and a KNN
-- query that constrains all four by equality reads only that partition (cost
-- tracks the partition's size, measured in docs/efficiency/M2-07.md). The
-- dimension is part of the table definition, so this candidate serves exactly
-- one dimension (vec.Dim, the 384 of Granite Embedding 97M).
--
-- vec0 maintains shadow tables of its own (vecb_vectors_vector_chunks00 and
-- others) that cannot carry scope columns: the scope lint must exempt
-- vec.ShadowTables(). They hold only vector bytes keyed by chunk slot, and
-- vec0 zeroes a deleted vector in place.
CREATE VIRTUAL TABLE vecb_vectors USING vec0(
    application_id text partition key,
    tenant_id      text partition key,
    workspace_id   text partition key,
    generation_id  text partition key,
    embedding      int8[384] distance_metric=cosine,
    +chunk_id      text
);

-- vecb_generations and vecb_rows are ordinary scoped tables, as in the flat
-- provider: the generation's fixed source, model and running vector count, and
-- the map from a chunk to its vec0 rowid with the reuse key (SEC-023). Reuse
-- looks a vector up by the same chunk id in another live generation of the
-- scope, so the key needs no index (see the flat provider's migration).
CREATE TABLE vecb_generations (
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    generation_id  TEXT    NOT NULL,
    source_id      TEXT    NOT NULL,
    model_id       TEXT    NOT NULL,
    vectors        INTEGER NOT NULL DEFAULT 0 CHECK (vectors >= 0),
    created_ms     INTEGER NOT NULL,
    PRIMARY KEY (application_id, tenant_id, workspace_id, generation_id)
) WITHOUT ROWID;

CREATE TABLE vecb_rows (
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    generation_id  TEXT    NOT NULL,
    chunk_id       TEXT    NOT NULL,
    vec_rowid      INTEGER NOT NULL,
    input_key      BLOB    NOT NULL CHECK (length(input_key) = 32),
    PRIMARY KEY (application_id, tenant_id, workspace_id, generation_id, chunk_id),
    FOREIGN KEY (application_id, tenant_id, workspace_id, generation_id)
        REFERENCES vecb_generations (application_id, tenant_id, workspace_id, generation_id)
        ON DELETE CASCADE
) WITHOUT ROWID;
