-- Relationship tuples derived from object data (Spec §8.3) and the outbox consumer position.
CREATE SCHEMA IF NOT EXISTS authz;

CREATE TABLE IF NOT EXISTS authz.derived_tuple (
  object_type TEXT NOT NULL,      -- ontology object type apiName
  object_pk   TEXT NOT NULL,
  fga_user    TEXT NOT NULL,      -- e.g. base:SIN
  relation    TEXT NOT NULL,
  fga_object  TEXT NOT NULL,      -- e.g. aircraft:B-1234
  PRIMARY KEY (object_type, object_pk, fga_user, relation)
);

CREATE TABLE IF NOT EXISTS authz.sync_cursor (
  name         TEXT PRIMARY KEY,
  last_id      BIGINT NOT NULL DEFAULT 0,
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
