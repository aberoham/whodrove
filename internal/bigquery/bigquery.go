// Package bigquery wraps the Cloud Audit Log BigQuery export and
// returns per-(principal, minute) feature rows for downstream
// session synthesis. See notes-gcp/04-org-aggregation-and-storage.md
// for the schema this query targets and notes-gcp/06-pipeline-design.md
// for why these specific aggregates were chosen.
//
// Auth: Application Default Credentials. Run
//
//	gcloud auth application-default login
//
// or set GOOGLE_APPLICATION_CREDENTIALS to a service-account key
// before invoking. The classifier's principal needs
// roles/bigquery.dataViewer on the audit dataset and
// roles/bigquery.jobUser in its own project.
package bigquery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	bq "cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"

	"teleport-ai/internal/synthsess"
)

// Config wires the audit dataset coordinates and an optional
// per-query bytes-scanned cap.
type Config struct {
	// BillingProject is the GCP project that owns the BQ job (where
	// query bytes are billed). Often the classifier's own project.
	BillingProject string
	// AuditProject is the project that hosts the audit dataset
	// (typically the FFF "logging-aggregation" project under the
	// security folder).
	AuditProject string
	// Dataset is the BigQuery dataset that the org-level aggregated
	// log sink writes to (often `audit_logs` or
	// `organization_audit_logs`).
	Dataset string
	// Table is the table name; defaults to
	// "cloudaudit_googleapis_com_activity" if empty.
	Table string
	// BytesScanCap is a per-query bytes-scanned safety cap.
	// 0 disables the cap.
	BytesScanCap int64
	// Location of the dataset, e.g. "US", "EU", "us-central1". Set if
	// the dataset is not in the multi-region the API would default
	// to; otherwise leave empty.
	Location string
}

// Client is the BigQuery query driver.
type Client struct {
	api *bq.Client
	cfg Config
}

func New(ctx context.Context, c Config) (*Client, error) {
	if c.BillingProject == "" {
		return nil, errors.New("bigquery: BillingProject is required")
	}
	if c.AuditProject == "" || c.Dataset == "" {
		return nil, errors.New("bigquery: AuditProject and Dataset are required")
	}
	if c.Table == "" {
		c.Table = "cloudaudit_googleapis_com_activity"
	}
	if !validIdent(c.AuditProject) || !validIdent(c.Dataset) || !validIdent(c.Table) {
		return nil, fmt.Errorf("bigquery: invalid identifier in {project=%q, dataset=%q, table=%q}",
			c.AuditProject, c.Dataset, c.Table)
	}
	api, err := bq.NewClient(ctx, c.BillingProject)
	if err != nil {
		return nil, fmt.Errorf("bigquery client: %w", err)
	}
	if c.Location != "" {
		api.Location = c.Location
	}
	return &Client{api: api, cfg: c}, nil
}

func (c *Client) Close() error { return c.api.Close() }

// queryMinuteFeaturesSQL aggregates per-(principal, minute)
// features over the requested time window. Top-N services and
// methods are JSON-rendered so callers don't have to grok BigQuery's
// STRUCT-array shape.
const queryMinuteFeaturesSQL = `
WITH per_minute AS (
  SELECT
    protopayload_auditlog.authenticationInfo.principalEmail        AS principal,
    TIMESTAMP_TRUNC(timestamp, MINUTE)                             AS minute_bucket,
    COUNT(*)                                                       AS call_count,
    COUNT(DISTINCT protopayload_auditlog.serviceName)              AS distinct_services,
    COUNT(DISTINCT protopayload_auditlog.methodName)               AS distinct_methods,
    ANY_VALUE(protopayload_auditlog.requestMetadata.callerSuppliedUserAgent)
                                                                   AS sample_ua,
    ANY_VALUE(protopayload_auditlog.requestMetadata.callerIp)      AS sample_ip,
    COUNTIF(ARRAY_LENGTH(protopayload_auditlog.authenticationInfo.serviceAccountDelegationInfo) > 0)
                                                                   AS impersonation_calls,
    COUNTIF(EXISTS(
      SELECT 1 FROM UNNEST(protopayload_auditlog.authorizationInfo) a
      WHERE a.granted = false
    ))                                                             AS denied_calls,
    TO_JSON_STRING(APPROX_TOP_COUNT(protopayload_auditlog.serviceName, 5))
                                                                   AS top_services_json,
    TO_JSON_STRING(APPROX_TOP_COUNT(protopayload_auditlog.methodName, 5))
                                                                   AS top_methods_json
  FROM ` + "`%s.%s.%s`" + `
  WHERE timestamp BETWEEN @since AND @until
    AND protopayload_auditlog.authenticationInfo.principalEmail IS NOT NULL
    %s
  GROUP BY principal, minute_bucket
)
SELECT * FROM per_minute
ORDER BY principal, minute_bucket
`

// QueryMinuteFeatures runs the aggregation against [since, until]
// and returns one MinuteFeature per (principal, minute) bucket.
//
// If principal != "", the query filters to that principal —
// useful for `pull --principal alice@example.com`.
func (c *Client) QueryMinuteFeatures(ctx context.Context, since, until time.Time, principal string) ([]synthsess.MinuteFeature, error) {
	principalFilter := ""
	if principal != "" {
		principalFilter = "AND protopayload_auditlog.authenticationInfo.principalEmail = @principal"
	}
	q := fmt.Sprintf(queryMinuteFeaturesSQL,
		c.cfg.AuditProject, c.cfg.Dataset, c.cfg.Table, principalFilter)
	job := c.api.Query(q)
	job.Parameters = []bq.QueryParameter{
		{Name: "since", Value: since},
		{Name: "until", Value: until},
	}
	if principal != "" {
		job.Parameters = append(job.Parameters, bq.QueryParameter{Name: "principal", Value: principal})
	}
	if c.cfg.BytesScanCap > 0 {
		job.MaxBytesBilled = c.cfg.BytesScanCap
	}

	it, err := job.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("bigquery read: %w", err)
	}

	type rawRow struct {
		Principal          bq.NullString    `bigquery:"principal"`
		MinuteBucket       bq.NullTimestamp `bigquery:"minute_bucket"`
		CallCount          int64            `bigquery:"call_count"`
		DistinctServices   int64            `bigquery:"distinct_services"`
		DistinctMethods    int64            `bigquery:"distinct_methods"`
		SampleUA           bq.NullString    `bigquery:"sample_ua"`
		SampleIP           bq.NullString    `bigquery:"sample_ip"`
		ImpersonationCalls int64            `bigquery:"impersonation_calls"`
		DeniedCalls        int64            `bigquery:"denied_calls"`
		TopServicesJSON    bq.NullString    `bigquery:"top_services_json"`
		TopMethodsJSON     bq.NullString    `bigquery:"top_methods_json"`
	}

	var out []synthsess.MinuteFeature
	for {
		var r rawRow
		err := it.Next(&r)
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("bigquery row: %w", err)
		}
		if !r.Principal.Valid || !r.MinuteBucket.Valid {
			continue
		}
		out = append(out, synthsess.MinuteFeature{
			Principal:          r.Principal.StringVal,
			MinuteBucket:       r.MinuteBucket.Timestamp,
			CallCount:          r.CallCount,
			DistinctServices:   r.DistinctServices,
			DistinctMethods:    r.DistinctMethods,
			SampleUA:           r.SampleUA.StringVal,
			SampleIP:           r.SampleIP.StringVal,
			ImpersonationCalls: r.ImpersonationCalls,
			DeniedCalls:        r.DeniedCalls,
			TopServicesJSON:    r.TopServicesJSON.StringVal,
			TopMethodsJSON:     r.TopMethodsJSON.StringVal,
		})
	}
	return out, nil
}

// validIdent allows only [A-Za-z0-9_-] and a single optional dot
// for dataset names. We interpolate these into SQL because the
// BigQuery client doesn't accept parameterized table references;
// the safe path is strict input validation.
func validIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	// Disallow leading dash to avoid edge cases.
	return !strings.HasPrefix(s, "-")
}
