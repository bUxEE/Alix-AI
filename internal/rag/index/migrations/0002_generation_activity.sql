-- Module "ragindex", migration 0002: the last activity of a generation.
--
-- updated_at is stamped by every write to a generation (a document put or
-- deleted, a state change, an embedding count). Compaction judges a build as
-- abandoned by how long it has been silent, not by how long ago it began, so a
-- large build that is still progressing is never failed for its age. NULL on a
-- row written before this migration; created_at stands in for it.
ALTER TABLE ragindex_generation ADD COLUMN updated_at TEXT;
