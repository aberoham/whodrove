// GCP-side session writes. Targets the same `sessions` table as the
// Teleport-side flow but only populates the cross-substrate columns
// plus the gcp_* extension columns. Teleport-only columns stay NULL.
package store

import (
	"database/sql"
	"fmt"
	"strings"

	"teleport-ai/internal/labels"
)

// SubstrateGCPCloudAudit is stamped on every row written by
// shellscope-gcp's `pull` so cross-substrate queries can split.
const SubstrateGCPCloudAudit = "gcp-cloud-audit"

// GCPSession is the session row a GCP-side pull writes. SessionID is
// synthetic (see internal/synthsess); User is the principalEmail.
type GCPSession struct {
	SessionID             string
	User                  string  // principalEmail
	StartedAt             string  // first minute_bucket of the synthetic session, RFC3339
	EndedAt               string  // last minute_bucket + 1 minute, RFC3339
	UploadedAt            string  // mirrors EndedAt; satisfies idx_sessions_uploaded
	DurationSeconds       float64 // EndedAt - StartedAt, seconds
	Cluster               string  // optional: the GCP project_id sample, if available
	GCPPrincipal          string  // principalEmail (same as User; kept for clarity)
	GCPUASample           string  // sample callerSuppliedUserAgent
	GCPCallerIP           string  // sample callerIp
	GCPCallCount          int64
	GCPDistinctServices   int64
	GCPDistinctMethods    int64
	GCPImpersonationCalls int64
	GCPDeniedCalls        int64
	GCPMinuteBuckets      int64
	GCPMedianCallGapMs    float64
	ParsedAt              string
	ParserVersion         string
	ParseError            string
}

// GCPMinuteFeature is the per-minute feature row.
// top_services_json / top_methods_json are pre-rendered JSON strings
// from APPROX_TOP_COUNT.
type GCPMinuteFeature struct {
	SessionID          string
	MinuteBucket       string
	CallCount          int64
	DistinctServices   int64
	DistinctMethods    int64
	ImpersonationCalls int64
	DeniedCalls        int64
	TopServicesJSON    string
	TopMethodsJSON     string
}

const upsertGCPSessionSQL = `
INSERT INTO sessions (
  session_id, user, cluster, started_at, ended_at, uploaded_at,
  duration_seconds, parsed_at, parser_version, parse_error,
  substrate, gcp_principal, gcp_ua_sample, gcp_caller_ip,
  gcp_call_count, gcp_distinct_services, gcp_distinct_methods,
  gcp_impersonation_calls, gcp_denied_calls, gcp_minute_buckets,
  gcp_median_call_gap_ms
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(session_id) DO UPDATE SET
  user                    = excluded.user,
  cluster                 = excluded.cluster,
  started_at              = excluded.started_at,
  ended_at                = excluded.ended_at,
  uploaded_at             = excluded.uploaded_at,
  duration_seconds        = excluded.duration_seconds,
  parsed_at               = excluded.parsed_at,
  parser_version          = excluded.parser_version,
  parse_error             = excluded.parse_error,
  substrate               = excluded.substrate,
  gcp_principal           = excluded.gcp_principal,
  gcp_ua_sample           = excluded.gcp_ua_sample,
  gcp_caller_ip           = excluded.gcp_caller_ip,
  gcp_call_count          = excluded.gcp_call_count,
  gcp_distinct_services   = excluded.gcp_distinct_services,
  gcp_distinct_methods    = excluded.gcp_distinct_methods,
  gcp_impersonation_calls = excluded.gcp_impersonation_calls,
  gcp_denied_calls        = excluded.gcp_denied_calls,
  gcp_minute_buckets      = excluded.gcp_minute_buckets,
  gcp_median_call_gap_ms  = excluded.gcp_median_call_gap_ms
`

func (s *Store) UpsertGCPSession(r GCPSession) error {
	_, err := s.db.Exec(upsertGCPSessionSQL,
		r.SessionID,
		r.User,
		nullable(r.Cluster),
		nullable(r.StartedAt),
		nullable(r.EndedAt),
		nullable(r.UploadedAt),
		nullableFloat(r.DurationSeconds),
		r.ParsedAt,
		r.ParserVersion,
		nullable(r.ParseError),
		SubstrateGCPCloudAudit,
		nullable(r.GCPPrincipal),
		nullable(r.GCPUASample),
		nullable(r.GCPCallerIP),
		nullableInt(r.GCPCallCount),
		nullableInt(r.GCPDistinctServices),
		nullableInt(r.GCPDistinctMethods),
		nullableInt(r.GCPImpersonationCalls),
		nullableInt(r.GCPDeniedCalls),
		nullableInt(r.GCPMinuteBuckets),
		nullableFloat(r.GCPMedianCallGapMs),
	)
	if err != nil {
		return fmt.Errorf("upsert gcp %s: %w", r.SessionID, err)
	}
	return nil
}

const upsertGCPMinuteFeatureSQL = `
INSERT INTO gcp_minute_features (
  session_id, minute_bucket, call_count, distinct_services,
  distinct_methods, impersonation_calls, denied_calls,
  top_services_json, top_methods_json
) VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(session_id, minute_bucket) DO UPDATE SET
  call_count          = excluded.call_count,
  distinct_services   = excluded.distinct_services,
  distinct_methods    = excluded.distinct_methods,
  impersonation_calls = excluded.impersonation_calls,
  denied_calls        = excluded.denied_calls,
  top_services_json   = excluded.top_services_json,
  top_methods_json    = excluded.top_methods_json
`

// ListGCPSessionsBySelector returns GCP-substrate sessions whose
// labels satisfy every requirement in the selector. An empty
// selector returns every GCP session. Always filters to
// substrate = SubstrateGCPCloudAudit so callers don't accidentally
// see Teleport rows in GCP-flavoured output.
func (s *Store) ListGCPSessionsBySelector(sel labels.Selector) ([]GCPSession, error) {
	var (
		query strings.Builder
		args  []any
	)
	query.WriteString(`SELECT session_id, user, started_at, ended_at, uploaded_at,
       gcp_principal, gcp_ua_sample, gcp_caller_ip,
       gcp_call_count, gcp_distinct_services, gcp_distinct_methods,
       gcp_impersonation_calls, gcp_denied_calls, gcp_minute_buckets,
       gcp_median_call_gap_ms
FROM sessions s WHERE substrate = ?`)
	args = append(args, SubstrateGCPCloudAudit)
	for i, r := range sel {
		alias := fmt.Sprintf("l%d", i)
		fmt.Fprintf(&query,
			" AND EXISTS(SELECT 1 FROM session_labels %s WHERE %s.session_id=s.session_id AND %s.key=? AND %s.value=?)",
			alias, alias, alias, alias)
		args = append(args, r.Key, r.Value)
	}
	query.WriteString(" ORDER BY started_at DESC")
	rows, err := s.db.Query(query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("list gcp: %w", err)
	}
	defer rows.Close()
	var out []GCPSession
	for rows.Next() {
		var (
			r                                                                     GCPSession
			startedAt, endedAt, uploadedAt, principal, uaSample, callerIP         sql.NullString
			callCount, distSvc, distMethod, impCalls, denCalls, minuteBuckets     sql.NullInt64
			medianGap                                                             sql.NullFloat64
		)
		if err := rows.Scan(&r.SessionID, &r.User,
			&startedAt, &endedAt, &uploadedAt,
			&principal, &uaSample, &callerIP,
			&callCount, &distSvc, &distMethod,
			&impCalls, &denCalls, &minuteBuckets, &medianGap); err != nil {
			return nil, err
		}
		r.StartedAt = startedAt.String
		r.EndedAt = endedAt.String
		r.UploadedAt = uploadedAt.String
		r.GCPPrincipal = principal.String
		r.GCPUASample = uaSample.String
		r.GCPCallerIP = callerIP.String
		r.GCPCallCount = callCount.Int64
		r.GCPDistinctServices = distSvc.Int64
		r.GCPDistinctMethods = distMethod.Int64
		r.GCPImpersonationCalls = impCalls.Int64
		r.GCPDeniedCalls = denCalls.Int64
		r.GCPMinuteBuckets = minuteBuckets.Int64
		r.GCPMedianCallGapMs = medianGap.Float64
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) UpsertGCPMinuteFeature(f GCPMinuteFeature) error {
	_, err := s.db.Exec(upsertGCPMinuteFeatureSQL,
		f.SessionID, f.MinuteBucket,
		f.CallCount, f.DistinctServices, f.DistinctMethods,
		f.ImpersonationCalls, f.DeniedCalls,
		nullable(f.TopServicesJSON), nullable(f.TopMethodsJSON),
	)
	if err != nil {
		return fmt.Errorf("upsert gcp_minute_feature %s/%s: %w",
			f.SessionID, f.MinuteBucket, err)
	}
	return nil
}
