# 04 — Org aggregation and storage

This file is the **weighted** one. The user's org runs on Google's
enterprise blueprint (Cloud Foundation Fabric / "FAST"), which means
audit logs are already routed via an org-level aggregated sink into
a centralized logging project, and the classifier most likely taps
the BigQuery dataset that sink populates. So this file goes deeper
than its siblings.

## 30-second summary

An FFF-built GCP org has:

- A dedicated **logging project** under the security folder
  (`logging-aggregation` or similar).
- An **org-level aggregated log sink** (`--include-children`)
  routing Admin Activity, Data Access, System Event, and Policy
  Denied logs from every project below the org into that logging
  project.
- Inside the logging project, sink destinations include at least:
  - A **BigQuery dataset** (the workbench — date-partitioned tables,
    queryable with SQL).
  - A **GCS bucket** (the archive — JSON-line files, long retention,
    cheap).
  - A **Pub/Sub topic** (streaming — for SIEM forwarders or
    real-time detectors).
- Optionally, a **Chronicle / Google SecOps** ingest path if the
  org has the SIEM. FFF ships a Chronicle module.

The classifier substrate is the BigQuery dataset. Everything else
is either real-time (Pub/Sub) or backfill (GCS).

## The aggregated sink, in detail

The canonical FFF sink looks like this conceptually (the actual
deployment is Terraform, but the resulting sink is the same):

```yaml
sink_name: "audit-logs-org"
parent: "organizations/<org-id>"
include_children: true
filter: |
  logName=~"cloudaudit.googleapis.com%2F(activity|data_access|system_event|policy)"
destinations:
  - bigquery: "<logging-project>.<bq-dataset>"
  - storage: "gs://<gcs-archive-bucket>"
  - pubsub: "projects/<logging-project>/topics/<audit-topic>"
```

`include_children: true` is what makes this an *org-level
aggregated* sink — without it, the sink would only catch logs
emitted directly by the org-level resource itself (rare). With it,
every project, folder, and resource under the org sends matching
logs to all destinations.

The sink runs as a service identity GCP creates for it
(`<sink-name>@gcp-sa-logging.iam.gserviceaccount.com`); that
identity needs `roles/bigquery.dataEditor` on the dataset,
`roles/storage.objectCreator` on the bucket, and
`roles/pubsub.publisher` on the topic.

## BigQuery dataset shape

### Naming and partitioning

The dataset name varies by deployment but is typically `audit_logs`
or `organization_audit_logs`. Inside it, Cloud Logging creates
**one table per `logName`**, named like:

```
cloudaudit_googleapis_com_activity        ← entries from 'activity' stream
cloudaudit_googleapis_com_data_access     ← entries from 'data_access' stream
cloudaudit_googleapis_com_system_event
cloudaudit_googleapis_com_policy
```

By default, Cloud Logging exports use **partitioned tables** keyed
on `timestamp` (column-based partitioning); the older legacy mode
created date-suffixed tables
(`cloudaudit_googleapis_com_activity_YYYYMMDD`). FFF deploys
partitioned tables; if you see suffixed tables, the sink was
created with the legacy `useLegacySql`-style option. (Live-tenant
check: `bq ls <bq-dataset>` and look for whether the suffix is
per-day or absent.)

### Schema (the columns a classifier queries)

Cloud Logging's BigQuery export schema is published; the columns
the classifier cares about, in priority order:

| Column | Type | What it is |
|--------|------|-----------|
| `timestamp` | TIMESTAMP | LogEntry.timestamp; partition key |
| `logName` | STRING | Identifies the stream |
| `resource.type` | STRING | e.g. `gce_instance`, `k8s_cluster`, `iam_role` |
| `resource.labels` | RECORD | Per-type labels: `project_id`, `instance_id`, `location`, etc. |
| `severity` | STRING | INFO / NOTICE / WARNING / ERROR |
| `protopayload_auditlog.serviceName` | STRING | Service that handled the call |
| `protopayload_auditlog.methodName` | STRING | API method |
| `protopayload_auditlog.resourceName` | STRING | Target resource |
| `protopayload_auditlog.authenticationInfo.principalEmail` | STRING | The acting identity |
| `protopayload_auditlog.authenticationInfo.serviceAccountKeyName` | STRING | Long-lived SA key, if any |
| `protopayload_auditlog.authenticationInfo.serviceAccountDelegationInfo` | RECORD ARRAY | Impersonation chain |
| `protopayload_auditlog.requestMetadata.callerIp` | STRING | Source IP |
| `protopayload_auditlog.requestMetadata.callerSuppliedUserAgent` | STRING | Client UA — the high-signal field |
| `protopayload_auditlog.authorizationInfo` | RECORD ARRAY | Each entry: `permission`, `granted`, `resource` |
| `protopayload_auditlog.request` | JSON-as-RECORD | The call payload (when present) |
| `protopayload_auditlog.response` | JSON-as-RECORD | The response payload (when present) |
| `protopayload_auditlog.metadata` | JSON-as-RECORD | Service-specific extras |

Note the `protopayload_auditlog` prefix — that's how Cloud Logging
flattens the `protoPayload` discriminated union into the BigQuery
schema. The export adds a `protoPayload.@type` value tagged
`type.googleapis.com/google.cloud.audit.AuditLog` for audit entries.

### Partition pruning is mandatory

Without a `WHERE timestamp BETWEEN ...` clause, queries scan the
entire dataset. FFF org datasets often hold months-to-years of
data; a single unpruned query against a 100-GB+ dataset can cost
real money. **Every classifier query must time-bound.**

Practical query template:

```sql
SELECT
  timestamp,
  protopayload_auditlog.serviceName    AS service_name,
  protopayload_auditlog.methodName     AS method_name,
  protopayload_auditlog.authenticationInfo.principalEmail AS principal,
  protopayload_auditlog.requestMetadata.callerSuppliedUserAgent AS user_agent,
  protopayload_auditlog.requestMetadata.callerIp AS caller_ip,
  ARRAY_LENGTH(
    ARRAY(
      SELECT a FROM UNNEST(protopayload_auditlog.authorizationInfo) a
      WHERE a.granted = false
    )
  ) AS denied_count
FROM
  `<logging-project>.<bq-dataset>.cloudaudit_googleapis_com_activity`
WHERE
  timestamp BETWEEN TIMESTAMP("2026-04-01") AND TIMESTAMP("2026-04-25")
  AND protopayload_auditlog.authenticationInfo.principalEmail
        = "<user>@<your-domain>"
ORDER BY
  timestamp ASC
```

For a classifier feature row, group by `(principal, time_bucket)`:

```sql
SELECT
  protopayload_auditlog.authenticationInfo.principalEmail AS principal,
  TIMESTAMP_TRUNC(timestamp, MINUTE) AS minute_bucket,
  COUNT(*) AS call_count,
  COUNT(DISTINCT protopayload_auditlog.serviceName) AS distinct_services,
  COUNT(DISTINCT protopayload_auditlog.methodName)  AS distinct_methods,
  ANY_VALUE(protopayload_auditlog.requestMetadata.callerSuppliedUserAgent) AS sample_ua,
  COUNTIF(protopayload_auditlog.authenticationInfo.serviceAccountDelegationInfo IS NOT NULL)
    AS impersonation_calls
FROM
  `<logging-project>.<bq-dataset>.cloudaudit_googleapis_com_activity`
WHERE
  timestamp BETWEEN TIMESTAMP("2026-04-24") AND TIMESTAMP("2026-04-25")
GROUP BY
  principal, minute_bucket
HAVING
  call_count > 1
```

This is the rough shape the GCP-side phase-1 cadence detector will
ingest into SQLite — see `06-pipeline-design.md`.

## GCS archive layout

The aggregated sink to GCS produces JSON-line files (one entry per
line, `application/json`) under a date-partitioned prefix:

```
gs://<gcs-archive-bucket>/
└── cloudaudit.googleapis.com/
    ├── activity/
    │   ├── 2026/04/24/00:00:00_00:59:59_S0.json
    │   ├── 2026/04/24/01:00:00_01:59:59_S0.json
    │   └── …
    ├── data_access/
    │   └── …
    ├── system_event/
    │   └── …
    └── policy/
        └── …
```

The exact filename pattern depends on the sink configuration; the
default is `<hour-start>_<hour-end>_S<shard>.json`. Files are
typically MiB-to-GB sized.

For a backfill query (e.g. "rebuild the classifier feature row for
all of 2026-Q1"), GCS is the cheapest source — Standard storage
class is ~$0.02/GB/month and reads are pennies. Just don't use it
for incremental queries; BigQuery's partition pruning beats
line-by-line GCS scans.

## Pub/Sub streaming destination

The aggregated sink to Pub/Sub publishes each `LogEntry` as a
Pub/Sub message with the JSON-encoded entry as the message body.
Subscribers get near-real-time delivery (typical end-to-end
latency: seconds).

Schema notes:

- The Pub/Sub message body is the *full* `LogEntry` JSON, not just
  the audit-log payload.
- Message attributes include `logging.googleapis.com/timestamp` and
  `logging.googleapis.com/insertId` — useful for ordered consumption
  and dedupe.
- Pub/Sub guarantees at-least-once. Subscribers must dedupe by
  `insertId`.

## Chronicle / Google SecOps SIEM

If the org has Chronicle, the FFF Chronicle module deploys a parser
that ingests audit logs from the same aggregated sink into
Chronicle's UDM (Unified Data Model). UDM normalizes fields across
log sources, so in Chronicle the audit event becomes a UDM event
with:

- `principal.user.email_addresses` ← `principalEmail`
- `target.resource.name` ← `resourceName`
- `metadata.event_type` ← method-name-derived
- `network.http.user_agent` ← `callerSuppliedUserAgent`
- ... etc.

Chronicle retention is typically 12 months by default; queries use
YARA-L rules or the UDM Search API. For a classifier, Chronicle is
attractive as a long-retention substrate but the UDM model loses
some audit-log fidelity (the `request` and `response` payloads are
usually stored in `additional.fields` blobs that are awkward to
query).

## Retention and cost

| Substrate | Default retention | Cost driver |
|-----------|-------------------|-------------|
| `_Required` Cloud Logging bucket | 400 days, fixed | Free |
| `_Default` Cloud Logging bucket | 30 days, configurable | Free up to 50 GiB/month per project, then $0.50/GiB ingested |
| BigQuery audit dataset | Unbounded (table-level expiration optional) | Storage + per-byte-scanned for queries |
| GCS archive | Unbounded; FFF often sets lifecycle: Standard → Nearline at 30d → Archive at 90d | Storage class + retrieval |
| Pub/Sub topic + subscription | 7 days unack'd default | Per-message + storage |
| Chronicle | 12 months default | Bundled in license tier |

For the classifier:

- BigQuery is the workbench — pay per query, partition-prune
  aggressively.
- GCS is backfill — cheap, slow, JSON-line.
- Pub/Sub is real-time — used by step 3 if alerting latency
  demands it.
- Chronicle is long-tail historical — used for "what did this user
  do six months ago" investigations.

## VPC SC and the logging project

In an FFF org, the logging project sits inside a VPC SC perimeter
that allows ingress from sinks (which run as GCP-internal service
identities) but blocks egress of log data outside the perimeter.

Implications for the classifier:

- The classifier's project should be inside the same perimeter, or
  bridged into it via an ingress/egress rule.
- Pulling BigQuery results to a developer laptop usually requires
  a perimeter exception — common patterns:
  - Run the classifier in a VM inside the perimeter and export
    only derived features (no raw log payloads).
  - Use BigQuery's authorized views to expose a sanitized view of
    the audit table; grant the classifier's SA on the view, not
    the source table.
- Outbound calls from the classifier (e.g. to an LLM API) violate
  the perimeter — use a VPC SC egress rule with a tight `target`
  to a managed Vertex AI endpoint, or run inference inside the
  perimeter.

## Concrete commands to confirm against the live tenant

(Run these against the user's GCP org. None of this is in product
docs; these are the live observations that populate the gaps in
this document.)

```bash
# 1. The org-level aggregated sink(s).
gcloud logging sinks list --organization=<org-id>
gcloud logging sinks describe audit-logs-org --organization=<org-id>

# 2. The BigQuery dataset and its tables.
bq ls --project_id=<logging-project> <bq-dataset>
bq show --schema --project_id=<logging-project> \
    <bq-dataset>.cloudaudit_googleapis_com_activity

# 3. Confirm partitioning.
bq show --format=prettyjson --project_id=<logging-project> \
    <bq-dataset>.cloudaudit_googleapis_com_activity \
  | jq '.timePartitioning'

# 4. Confirm the GCS archive bucket exists and has the expected prefix.
gsutil ls gs://<gcs-archive-bucket>/cloudaudit.googleapis.com/

# 5. Pub/Sub subscription for real-time access.
gcloud pubsub topics list --project=<logging-project>
gcloud pubsub subscriptions list --project=<logging-project>

# 6. Audit-config status (which Data Access streams are on, where).
gcloud organizations get-iam-policy <org-id> --format=json \
  | jq '.auditConfigs'

# 7. Sample query to confirm the schema is what's documented.
bq query --use_legacy_sql=false --project_id=<logging-project> \
  'SELECT
     COUNT(*) AS rows,
     COUNT(DISTINCT
       protopayload_auditlog.authenticationInfo.principalEmail) AS distinct_principals
   FROM `<logging-project>.<bq-dataset>.cloudaudit_googleapis_com_activity`
   WHERE timestamp > TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 HOUR)'
```

## Failure modes specific to org aggregation

1. **Sink-identity perm drift.** If the aggregated sink's service
   identity loses `bigquery.dataEditor` on the dataset, writes
   silently start failing — entries are dropped at the sink.
   Symptom: gaps in the BQ tables. Check sink errors with
   `gcloud logging sinks describe ... --format='value(writerIdentity,disabled)'`
   and the per-sink error count metric.
2. **VPC SC denials at the sink.** If the destination project is
   in a tighter perimeter than the sink-identity expects, writes
   fail. Symptom: write denials in Policy Denied stream.
3. **BigQuery quota exhaustion.** Aggressive backfill queries can
   hit per-project query quotas. Use BigQuery reservations or
   rate-limit the classifier.
4. **Schema drift across tables.** If a new GCP service adds new
   audit-log fields, the BQ schema gets new columns *but only on
   tables created after the change* (legacy date-suffixed mode).
   Old daily-suffixed tables stay frozen. Cross-day queries that
   select new columns fail on old tables. Mitigation: use the
   partitioned table (single schema across days) or write
   defensive queries with `SAFE.OFFSET` / `IFNULL`.
5. **Pub/Sub backpressure.** A slow subscriber can let the topic
   back up; default retention is 7 days unack'd. After that, lost
   messages.
6. **Org-policy changes mid-flight.** Turning Data Access logging
   on for a new service mid-day means the BQ table starts getting
   new rows mid-day; classifier features computed before the
   cut-over need re-computation.

## Implications for detection (preview of `05`)

Because the org owns the BigQuery dataset, the GCS archive, and
(typically) the Pub/Sub topic, a classifier can plausibly tap any
of:

- **BigQuery SQL** for batch / aggregate / scheduled detections.
  Cheapest mental model; query latency is seconds-to-minutes;
  cost is per-byte-scanned.
- **Direct GCS read** of the archive for backfill or cold-storage
  classification. Cheap, slow.
- **Pub/Sub stream** for real-time detection.
- **Chronicle UDM Search** for long-retention historical queries.

The fifth option — Cloud Logging API directly — is also available
but isn't really "org-aggregation-specific" so it lives in `05`.

## Cross-references

- `01-cloud-audit-logs.md` — the LogEntry/AuditLog shape these
  tables hold.
- `02-session-and-edge-capture.md` — what's actually visible vs the
  PTY gap.
- `03-ecosystem-and-apis.md` — Log Router and identity systems.
- `99-open-questions.md` — items in this file's narrative that
  need live-tenant verification.
