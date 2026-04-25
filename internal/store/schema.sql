CREATE TABLE IF NOT EXISTS sessions (
  session_id              TEXT PRIMARY KEY,
  user                    TEXT NOT NULL,
  cluster                 TEXT,
  kind                    TEXT,
  started_at              TEXT,
  ended_at                TEXT,
  uploaded_at             TEXT,
  duration_seconds        REAL,
  recording_uri           TEXT,
  recording_bytes         INTEGER,
  pty_present             INTEGER,
  print_chunks            INTEGER,
  print_bytes             INTEGER,
  median_chunk_gap_ms     REAL,
  idle_gap_count          INTEGER,
  edit_char_count         INTEGER,
  command_count           INTEGER,
  bpf_present             INTEGER,
  single_shot             INTEGER,
  join_count              INTEGER,
  parsed_at               TEXT NOT NULL,
  parser_version          TEXT NOT NULL,
  parse_error             TEXT,
  -- Cross-substrate columns. NULL on Teleport rows that predate the
  -- substrate split. New Teleport upserts default substrate to
  -- 'teleport-recording'; new GCP upserts default to 'gcp-cloud-audit'.
  substrate               TEXT,
  -- GCP-only columns. NULL on Teleport rows.
  gcp_principal           TEXT,
  gcp_ua_sample           TEXT,
  gcp_caller_ip           TEXT,
  gcp_call_count          INTEGER,
  gcp_distinct_services   INTEGER,
  gcp_distinct_methods    INTEGER,
  gcp_impersonation_calls INTEGER,
  gcp_denied_calls        INTEGER,
  gcp_minute_buckets      INTEGER,
  gcp_median_call_gap_ms  REAL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user      ON sessions(user);
CREATE INDEX IF NOT EXISTS idx_sessions_uploaded  ON sessions(uploaded_at);
CREATE INDEX IF NOT EXISTS idx_sessions_kind      ON sessions(kind);
-- Indexes on columns added post-launch (substrate, gcp_principal)
-- live in migrate.go so they're created after the columns exist on
-- pre-existing DBs.

CREATE TABLE IF NOT EXISTS session_labels (
  session_id  TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  key         TEXT NOT NULL,
  value       TEXT NOT NULL,
  set_by      TEXT NOT NULL,
  set_at      TEXT NOT NULL,
  PRIMARY KEY (session_id, key)
);
CREATE INDEX IF NOT EXISTS idx_labels_kv      ON session_labels(key, value);
CREATE INDEX IF NOT EXISTS idx_labels_session ON session_labels(session_id);

CREATE TABLE IF NOT EXISTS notable_events (
  session_id  TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  event_time  TEXT NOT NULL,
  event_type  TEXT NOT NULL,
  payload     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_notable_session ON notable_events(session_id);

-- Per-(synthetic-session, minute) feature rows for the GCP substrate.
-- Counts and APPROX_TOP_COUNT fingerprints only; raw audit-log rows
-- never persist here.
CREATE TABLE IF NOT EXISTS gcp_minute_features (
  session_id          TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  minute_bucket       TEXT NOT NULL,
  call_count          INTEGER NOT NULL,
  distinct_services   INTEGER NOT NULL,
  distinct_methods    INTEGER NOT NULL,
  impersonation_calls INTEGER NOT NULL,
  denied_calls        INTEGER NOT NULL,
  top_services_json   TEXT,
  top_methods_json    TEXT,
  PRIMARY KEY (session_id, minute_bucket)
);
CREATE INDEX IF NOT EXISTS idx_gcp_minute_session ON gcp_minute_features(session_id);
