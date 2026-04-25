# 06 — Pipeline design

This file is the GCP-side counterpart to `notes/06-pipeline-design.md`.
It picks tap points, sketches a Go binary that fits the existing
`shellscope` shape (mirroring `cmd/teleport-analyze`), names the
BigQuery query that drives it, and extends the Teleport-side SQLite
schema (the K8s-style labels) with GCP-specific feature columns.

## Purpose & non-goals

**In scope.** Tap choice for GCP, Go binary shape that reuses the
Teleport-side SQLite labels schema, the BigQuery query that produces
synthesised "sessions", the per-`(principal, window)` feature row,
and how cohort routing handles the substrate differences.

**Out of scope.** The classifier model, the detection rules, the
training data, and any alerting. Those are step 3.

**Scoping decisions** (mirror the Teleport-side ones from
`notes/06`):

1. **Ad-hoc local analysis, not a long-running pipeline.** Same
   dedicated analysis blocks against bounded date ranges (typical
   window 30-90 days). Backfill is the same invocation with a
   wider date range.
2. **KISS dependencies.** Single static Go binary, SQLite,
   brew-installable CLIs. No long-running services unless step 3
   needs them for real-time alerting.
3. **Go over Python.** Use `cloud.google.com/go/bigquery` and
   `google.golang.org/api/option`. The `LogEntry` proto is in
   `google.golang.org/genproto/googleapis/logging/v2` if we ever
   need to deserialize it; for the SQL path we don't.
4. **No PTY substrate to chase.** Phase-1 features are call-graph
   cadence + UA fingerprint + denial shape, not keystroke cadence.

## Architecture

```
macOS / Linux $ shellscope-gcp pull --since 2026-03-25 --until 2026-04-25
    │
    ├── BigQuery query (cloud.google.com/go/bigquery) for per-(principal,
    │   minute) feature rows over the audit dataset, partition-pruned by
    │   timestamp. Returns one row per (principal, minute_bucket).
    │
    ├── Synthesise "sessions" by gluing adjacent minute buckets where
    │   gaps are < idle_threshold_seconds. Each contiguous run becomes
    │   one synthetic session_id (UUIDv7 stamped at compute time).
    │
    ├── For each synthetic session, optionally enrich:
    │     ├── GKE k8s.io request bodies (separate BigQuery query, joined
    │     │   on principal + time-window)
    │     ├── IAP tunnel events (if any)
    │     ├── Asset feed context (resource labels at the time of action)
    │     └── BeyondCorp context-aware-access decisions (if available)
    │
    └── Upsert into local sessions.sqlite — same file, same labels schema
        as the Teleport side. New labels: substrate.kind, gcp.principal.type,
        gcp.ua.tool, gcp.impersonation.depth, gcp.denials.count, …
```

Tap choice: **(a) BigQuery for batch features**. Skip Pub/Sub (no
real-time need at this stage), skip Cloud Logging API direct (rate
limits), skip GCS (BigQuery already has it), skip Chronicle (UDM
fidelity loss).

Step 3 may add a Pub/Sub real-time tap on top of the same SQLite
if alerting latency demands it.

## Binary shape

Decision deferred to start of step 2: **extend `teleport-analyze`
with `--substrate` flag** OR **build sibling `cmd/shellscope-gcp`
binary**. Both write to the same `sessions.sqlite`. Sketch below
assumes the sibling-binary path because it lets `internal/` packages
stay GCP-pure (no AWS imports leaking into the GCP code path); the
extend-existing path is fine if it turns out the GCP code is small.

Subcommands (mirroring the Teleport-side `teleport-analyze`):

- `pull --since DATE --until DATE` — fetch + populate
  `sessions.sqlite`.
- `pull --principal EMAIL` — single user, ad-hoc.
- `pull --no-enrich` — phase-1 features only, skip GKE/IAP
  enrichment.
- `pull --idle-threshold-seconds 600` — synthetic-session boundary.
- `parse <gcs-archive-prefix>` — local-archive mode for offline
  development.
- `label set --session SID --key KEY --value VALUE` — manual
  stamp.
- `label ls --selector KEY=VALUE[,KEY=VALUE…]` — same K8s-style
  selector as the Teleport side.

Idempotent: re-runs over overlapping date ranges are safe. Synthetic
session IDs are deterministic given `(principal, first_bucket,
last_bucket)` so re-runs upsert rather than duplicate.

Auth: standard `application_default_credentials.json` chain
(`gcloud auth application-default login` for dev; SA key for
headless; Workload Identity in CI).

Dependencies:

- `cloud.google.com/go/bigquery` for BQ jobs.
- `cloud.google.com/go/pubsub` (optional, step 3).
- `cloud.google.com/go/asset` (optional, for enrichment).
- `cloud.google.com/go/storage` (optional, for GCS archive mode).
- `modernc.org/sqlite` (pure Go, no CGO) — same dep the
  Teleport-side binary uses.
- `github.com/spf13/cobra` for subcommand wiring — same.

## The phase-1 BigQuery query

```sql
WITH per_minute AS (
  SELECT
    protopayload_auditlog.authenticationInfo.principalEmail
                                                          AS principal,
    TIMESTAMP_TRUNC(timestamp, MINUTE)                    AS minute_bucket,
    COUNT(*)                                              AS call_count,
    COUNT(DISTINCT protopayload_auditlog.serviceName)     AS distinct_services,
    COUNT(DISTINCT protopayload_auditlog.methodName)      AS distinct_methods,
    ANY_VALUE(protopayload_auditlog.requestMetadata.callerSuppliedUserAgent)
                                                          AS sample_ua,
    ANY_VALUE(protopayload_auditlog.requestMetadata.callerIp)
                                                          AS sample_ip,
    COUNTIF(protopayload_auditlog.authenticationInfo.serviceAccountDelegationInfo IS NOT NULL)
                                                          AS impersonation_calls,
    COUNTIF(EXISTS(
      SELECT 1 FROM UNNEST(protopayload_auditlog.authorizationInfo) a
      WHERE a.granted = false
    ))                                                    AS denied_calls,
    APPROX_TOP_COUNT(protopayload_auditlog.serviceName, 5)
                                                          AS top_services,
    APPROX_TOP_COUNT(protopayload_auditlog.methodName,  5)
                                                          AS top_methods
  FROM
    `<logging-project>.<bq-dataset>.cloudaudit_googleapis_com_activity`
  WHERE
    timestamp BETWEEN TIMESTAMP(@since) AND TIMESTAMP(@until)
    AND protopayload_auditlog.authenticationInfo.principalEmail IS NOT NULL
  GROUP BY
    principal, minute_bucket
)
SELECT * FROM per_minute
ORDER BY principal, minute_bucket;
```

Considerations:

- Date partition pruning is mandatory (see `04`).
- Adding the Data Access table to the same query (UNION ALL)
  doubles the data volume but catches IAM token mints and KMS uses,
  which are load-bearing for impersonation detection.
- `APPROX_TOP_COUNT` is cheap and gives the classifier a UA /
  method fingerprint without storing every row.

The binary fetches this in one BQ job, then in Go:

1. **Synthesise sessions.** Walk the rows ordered by
   `(principal, minute_bucket)`. Start a new synthetic session
   when:
   - Principal changes, or
   - Gap to prior bucket > `idle_threshold_seconds` (default 600).
2. **Compute session-level features.** Sum / median / max over the
   per-minute rows in the synthetic session.
3. **Stamp labels.** A first pass of cheap rules sets labels like
   `substrate.kind=gcp-cloud-audit`,
   `gcp.principal.type=user|service-account|workforce-federation`,
   `routing.cohort=phase1-cadence`.

## SQLite schema additions

The Teleport-side schema in `notes/06-pipeline-design.md` has three
tables: `sessions`, `session_labels`, `notable_events`. We extend
`sessions` with GCP-specific columns (additive, NULL on Teleport
rows) and add a fourth table for per-minute feature rows.

```sql
-- Extension to existing 'sessions' table; additive, optional columns
-- so cross-substrate queries still work.
ALTER TABLE sessions ADD COLUMN substrate         TEXT;
ALTER TABLE sessions ADD COLUMN gcp_principal     TEXT;
ALTER TABLE sessions ADD COLUMN gcp_ua_sample     TEXT;
ALTER TABLE sessions ADD COLUMN gcp_caller_ip     TEXT;
ALTER TABLE sessions ADD COLUMN gcp_call_count    INTEGER;
ALTER TABLE sessions ADD COLUMN gcp_distinct_services INTEGER;
ALTER TABLE sessions ADD COLUMN gcp_distinct_methods  INTEGER;
ALTER TABLE sessions ADD COLUMN gcp_impersonation_calls INTEGER;
ALTER TABLE sessions ADD COLUMN gcp_denied_calls  INTEGER;
ALTER TABLE sessions ADD COLUMN gcp_minute_buckets INTEGER;
ALTER TABLE sessions ADD COLUMN gcp_median_call_gap_ms REAL;

-- New table — per-bucket feature row, for classifier model training
-- and post-hoc inspection. Keep counts and fingerprints; never raw
-- audit rows.
CREATE TABLE IF NOT EXISTS gcp_minute_features (
  session_id          TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  minute_bucket       TEXT NOT NULL,
  call_count          INTEGER NOT NULL,
  distinct_services   INTEGER NOT NULL,
  distinct_methods    INTEGER NOT NULL,
  impersonation_calls INTEGER NOT NULL,
  denied_calls        INTEGER NOT NULL,
  top_services_json   TEXT,            -- APPROX_TOP_COUNT result, JSON
  top_methods_json    TEXT,
  PRIMARY KEY (session_id, minute_bucket)
);
CREATE INDEX idx_gcp_minute_session ON gcp_minute_features(session_id);
```

The `session_labels` and `notable_events` tables are reused as-is.
Conventions for GCP-specific labels (extending `notes/06`):

- `substrate.kind` ∈ `teleport-recording | gcp-cloud-audit |
  gcp-gke-k8sio | gcp-iap-tunnel | gcp-os-login`.
- `gcp.principal.type` ∈ `user | service-account |
  workforce-federation | workload-federation | unknown`.
- `gcp.ua.tool` ∈ `gcloud | terraform | kubectl | client-go |
  boto3 | claude-code | unknown` (best-guess from UA string).
- `gcp.impersonation.depth` is numeric (length of
  `serviceAccountDelegationInfo`).
- `gcp.session.synthesised` ∈ `true | false` (always `true` for
  GCP).
- `routing.cohort` ∈ `phase1-cadence | gke-exec-opaque |
  iap-tunnel-only | sa-only | mixed-substrate`.

Selector example, identifying agent-driven GCP sessions:

`shellscope-gcp label ls --selector substrate.kind=gcp-cloud-audit,operator.type=agent,gcp.ua.tool=terraform`

```sql
SELECT s.* FROM sessions s
JOIN session_labels la ON la.session_id=s.session_id
                      AND la.key='substrate.kind' AND la.value='gcp-cloud-audit'
JOIN session_labels lb ON lb.session_id=s.session_id
                      AND lb.key='operator.type' AND lb.value='agent'
JOIN session_labels lc ON lc.session_id=s.session_id
                      AND lc.key='gcp.ua.tool' AND lc.value='terraform';
```

**Privacy invariant.** Raw `request` / `response` payloads from
audit logs are NEVER persisted to SQLite (some contain user data
or VPC SC perimeter details that shouldn't leak to a developer
laptop). Only counts, fingerprints, and derived numeric features.
The full payloads stay in BigQuery; the binary downloads, parses,
extracts, drops.

## Backfill = the same invocation

`pull --since 2026-01-25 --until 2026-04-25` is the backfill —
there is no separate code path. BigQuery handles a multi-month
query natively; partition pruning and BQ slot allocation are the
only concerns.

For very long backfills (year+), use `--from-gcs <gcs-prefix>` to
read the JSON-line archive directly and skip BQ altogether.

## Detection strategy hint (carry to step 3)

- **Phase-1 (cheap, rules-only).** "Human terminal y/n" doesn't
  apply here — there's no terminal. The GCP-side phase-1 question
  is more like "operator-driven y/n" or "automated-pipeline y/n".
  Cheap signals:
  - **Cadence shape.** Median call gap, burst-vs-pause ratio,
    number of distinct minute buckets in the synthetic session.
  - **UA fingerprint.** `gcloud` family vs Terraform vs SDK vs
    unknown.
  - **Impersonation chain.** Length and shape of
    `serviceAccountDelegationInfo`. Humans typically have a
    one-step chain (user → SA); Workload Identity has zero-step
    (the WIF principal IS the actor).
  - **Denial shape.** Burst of denials early in a session is a
    discovery / probing signal.
  - **Service breadth.** A typical human admin session touches
    3-7 services in 30 minutes; an agent often hammers one
    service.
- **Phase-2 (deeper).** For sessions phase-1 stamped as
  `operator.type=human` *and* the principal is in a privileged
  role, fetch the GKE k8s.io entries to enrich with command-level
  shape. This is also where LLM-on-call-graph might earn its
  cost.
- **Edge cohort.** GKE `exec`, IAP-tunneled SSH without host-side
  recorder, and pure-WIF principals all look agent-like by
  construction. Phase-1 should stamp `routing.cohort` accordingly
  so step 3 routes them to a different (yet-unwritten) classifier
  rather than mis-applying the human-vs-agent heuristic.

## Cross-substrate fusion with the Teleport side

Both binaries write into the same `sessions.sqlite`. Where the
same human appears on both substrates in overlapping windows
(Teleport SSH session + concurrent gcloud activity from the same
person), step 3 can:

- Detect the temporal overlap by joining on `user` (Teleport) ↔
  derived-`gcp.principal` (GCP) in a time window.
- Stamp a `correlation.cross-substrate=session-<n>` label on both
  rows.
- Treat the pair as a single operator-action for classification
  purposes.

This is sketch only — the join logic is step 3.

## Open questions resolved by this design

The following entries in `99-open-questions.md` are
resolved-by-design because we tap BigQuery directly and synthesize
sessions from `(principal, time-window)`:

- **Q15** (BigQuery latency calibration) — non-blocking; the
  design tolerates 1-3 minute lag.
- **Q17** (synthetic-session idle threshold) — exposed as a CLI
  flag with a 600 s default; calibrate against real data later.

The remaining live-tenant questions (Q1-Q9) still apply. The
natural follow-up after this doc lands is to knock down those —
they replace the `<org-id>` / `<bq-dataset>` placeholders and
confirm the schema is what the docs say.
