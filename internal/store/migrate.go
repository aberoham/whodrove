package store

import "fmt"

// expectedSessionsExtras lists columns that were added to `sessions`
// after the original schema. Pre-existing databases need ALTER TABLE
// to catch up; new databases get them via CREATE TABLE in schema.sql
// and the migration is a no-op.
var expectedSessionsExtras = []struct {
	name string
	typ  string
}{
	{"substrate", "TEXT"},
	{"gcp_principal", "TEXT"},
	{"gcp_ua_sample", "TEXT"},
	{"gcp_caller_ip", "TEXT"},
	{"gcp_call_count", "INTEGER"},
	{"gcp_distinct_services", "INTEGER"},
	{"gcp_distinct_methods", "INTEGER"},
	{"gcp_impersonation_calls", "INTEGER"},
	{"gcp_denied_calls", "INTEGER"},
	{"gcp_minute_buckets", "INTEGER"},
	{"gcp_median_call_gap_ms", "REAL"},
}

func (s *Store) migrate() error {
	have := make(map[string]bool)
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('sessions')`)
	if err != nil {
		return fmt.Errorf("pragma table_info(sessions): %w", err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		have[n] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, c := range expectedSessionsExtras {
		if have[c.name] {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE sessions ADD COLUMN %s %s", c.name, c.typ)
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("alter sessions add %s: %w", c.name, err)
		}
	}
	// Indexes on post-launch columns. SQLite's `CREATE INDEX IF NOT
	// EXISTS` errors if the column itself is missing, so these have
	// to live here (after the ALTER TABLEs) instead of in schema.sql.
	postLaunchIndexes := []string{
		"CREATE INDEX IF NOT EXISTS idx_sessions_substrate ON sessions(substrate)",
		"CREATE INDEX IF NOT EXISTS idx_sessions_gcp_principal ON sessions(gcp_principal)",
	}
	for _, stmt := range postLaunchIndexes {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("post-launch index: %w", err)
		}
	}
	return nil
}
