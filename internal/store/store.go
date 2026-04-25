// Package store wraps the per-session SQLite. One file, three tables; the
// classifier writes its decisions back as Kubernetes-style labels rather
// than fixed columns.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strings"

	"teleport-ai/internal/labels"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type Store struct {
	db *sql.DB
}

type Session struct {
	SessionID        string
	User             string
	Cluster          string
	Kind             string
	StartedAt        string
	EndedAt          string
	UploadedAt       string
	DurationSeconds  float64
	RecordingURI     string
	RecordingBytes   int64
	PTYPresent       bool
	PrintChunks      int64
	PrintBytes       int64
	MedianChunkGapMs float64
	IdleGapCount     int64
	EditCharCount    int64
	CommandCount     int64
	BPFPresent       bool
	SingleShot       bool
	JoinCount        int64
	ParsedAt         string
	ParserVersion    string
	ParseError       string
}

type NotableEvent struct {
	EventTime string
	EventType string
	Payload   string
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// modernc.org/sqlite supports a single connection per *sql.DB safely
	// even in WAL mode, but cap it explicitly so concurrent writers do
	// not race during long pulls.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("pragmas: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// DB exposes the underlying *sql.DB for callers that need to compose
// custom queries (the GCP-side per-minute feature insert in particular).
// Use sparingly — prefer typed helpers on Store.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Close() error { return s.db.Close() }

const upsertSessionSQL = `
INSERT INTO sessions (
  session_id, user, cluster, kind, started_at, ended_at, uploaded_at,
  duration_seconds, recording_uri, recording_bytes, pty_present,
  print_chunks, print_bytes, median_chunk_gap_ms, idle_gap_count,
  edit_char_count, command_count, bpf_present, single_shot, join_count,
  parsed_at, parser_version, parse_error
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(session_id) DO UPDATE SET
  user                = excluded.user,
  cluster             = excluded.cluster,
  kind                = excluded.kind,
  started_at          = excluded.started_at,
  ended_at            = excluded.ended_at,
  uploaded_at         = excluded.uploaded_at,
  duration_seconds    = excluded.duration_seconds,
  recording_uri       = excluded.recording_uri,
  recording_bytes     = excluded.recording_bytes,
  pty_present         = excluded.pty_present,
  print_chunks        = excluded.print_chunks,
  print_bytes         = excluded.print_bytes,
  median_chunk_gap_ms = excluded.median_chunk_gap_ms,
  idle_gap_count      = excluded.idle_gap_count,
  edit_char_count     = excluded.edit_char_count,
  command_count       = excluded.command_count,
  bpf_present         = excluded.bpf_present,
  single_shot         = excluded.single_shot,
  join_count          = excluded.join_count,
  parsed_at           = excluded.parsed_at,
  parser_version      = excluded.parser_version,
  parse_error         = excluded.parse_error
`

func (s *Store) UpsertSession(r Session) error {
	_, err := s.db.Exec(upsertSessionSQL,
		r.SessionID, r.User, nullable(r.Cluster), nullable(r.Kind),
		nullable(r.StartedAt), nullable(r.EndedAt), nullable(r.UploadedAt),
		nullableFloat(r.DurationSeconds),
		nullable(r.RecordingURI), nullableInt(r.RecordingBytes),
		boolToInt(r.PTYPresent),
		r.PrintChunks, r.PrintBytes, r.MedianChunkGapMs, r.IdleGapCount,
		r.EditCharCount, r.CommandCount, boolToInt(r.BPFPresent),
		boolToInt(r.SingleShot), r.JoinCount,
		r.ParsedAt, r.ParserVersion, nullable(r.ParseError),
	)
	if err != nil {
		return fmt.Errorf("upsert %s: %w", r.SessionID, err)
	}
	return nil
}

func (s *Store) SetLabel(sid, key, value, setBy, setAt string) error {
	_, err := s.db.Exec(`
INSERT INTO session_labels (session_id, key, value, set_by, set_at)
VALUES (?,?,?,?,?)
ON CONFLICT(session_id, key) DO UPDATE SET
  value=excluded.value, set_by=excluded.set_by, set_at=excluded.set_at`,
		sid, key, value, setBy, setAt)
	if err != nil {
		return fmt.Errorf("set label %s/%s: %w", sid, key, err)
	}
	return nil
}

func (s *Store) InsertNotable(sid string, n NotableEvent) error {
	_, err := s.db.Exec(`
INSERT INTO notable_events (session_id, event_time, event_type, payload)
VALUES (?,?,?,?)`, sid, n.EventTime, n.EventType, n.Payload)
	if err != nil {
		return fmt.Errorf("insert notable %s/%s: %w", sid, n.EventType, err)
	}
	return nil
}

// ReplaceNotable wipes any prior notable_events rows for sid and inserts
// the supplied set in one transaction. Callers should use this rather
// than bare InsertNotable when re-parsing a session, otherwise re-runs
// of `pull` over an overlapping date range duplicate every notable row.
func (s *Store) ReplaceNotable(sid string, events []NotableEvent) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM notable_events WHERE session_id=?`, sid); err != nil {
		return fmt.Errorf("delete notable %s: %w", sid, err)
	}
	stmt, err := tx.Prepare(`INSERT INTO notable_events (session_id, event_time, event_type, payload) VALUES (?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare notable insert: %w", err)
	}
	defer stmt.Close()
	for _, n := range events {
		if _, err := stmt.Exec(sid, n.EventTime, n.EventType, n.Payload); err != nil {
			return fmt.Errorf("insert notable %s/%s: %w", sid, n.EventType, err)
		}
	}
	return tx.Commit()
}

// ListBySelector returns sessions whose labels satisfy every requirement.
// An empty selector returns every session.
func (s *Store) ListBySelector(sel labels.Selector) ([]Session, error) {
	var (
		query strings.Builder
		args  []any
	)
	query.WriteString("SELECT s.session_id, s.user, s.cluster, s.kind, s.uploaded_at, s.pty_present, s.print_chunks FROM sessions s")
	for i, r := range sel {
		alias := fmt.Sprintf("l%d", i)
		fmt.Fprintf(&query, " JOIN session_labels %s ON %s.session_id=s.session_id AND %s.key=? AND %s.value=?", alias, alias, alias, alias)
		args = append(args, r.Key, r.Value)
	}
	query.WriteString(" ORDER BY uploaded_at DESC")
	rows, err := s.db.Query(query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var (
			r        Session
			cluster  sql.NullString
			kind     sql.NullString
			uploaded sql.NullString
			pty      sql.NullInt64
		)
		if err := rows.Scan(&r.SessionID, &r.User, &cluster, &kind, &uploaded, &pty, &r.PrintChunks); err != nil {
			return nil, err
		}
		r.Cluster = cluster.String
		r.Kind = kind.String
		r.UploadedAt = uploaded.String
		r.PTYPresent = pty.Int64 == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullableFloat(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
