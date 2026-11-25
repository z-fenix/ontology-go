-- Indexer progress (Spec §5): one cursor per datasource-backed object type or join table.
CREATE SCHEMA IF NOT EXISTS indexer;

CREATE TABLE IF NOT EXISTS indexer.sync_state (
  ontology        TEXT NOT NULL,
  source          TEXT NOT NULL,          -- "object:<ObjectType>" or "link:<linkType>"
  cursor_ts       TIMESTAMPTZ,            -- high-water mark of the timestamp column
  cursor_pk       TEXT,                   -- tie-breaker for rows sharing cursor_ts
  last_full_sync  TIMESTAMPTZ,
  last_run_at     TIMESTAMPTZ,
  last_error      TEXT,
  stats           JSONB,
  PRIMARY KEY (ontology, source)
);
