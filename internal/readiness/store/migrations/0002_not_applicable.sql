-- readiness module, migration 0002: the count of not-applicable requirements (ADR 0037).
--
-- MCP, OpenAPI and a native adapter are alternative transports: once one of them is met, a missing
-- requirement of another is not applicable. The report summary carries that count; this column
-- repeats it next to the other counts so the history lists it. Reports stored before this
-- migration have none, so the default is 0.

ALTER TABLE readiness_reports
    ADD COLUMN not_applicable INTEGER NOT NULL DEFAULT 0 CHECK (not_applicable >= 0);
