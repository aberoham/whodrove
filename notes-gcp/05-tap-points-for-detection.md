# 05 — Tap points for detection

## 30-second summary

A GCP-side privileged-user classifier has five realistic places to
tap audit data. Each has a different auth model, latency budget,
fidelity, and cost profile. None is strictly better than the
others; the right answer is usually a hybrid (BigQuery for batch
features + Pub/Sub for real-time signals if alerting latency
demands it). This file enumerates the five options and gives a
recommendation matrix; the actual pipeline design lives in
`06-pipeline-design.md`.

## The five taps

### (a) BigQuery direct on the aggregated audit dataset

**What.** Run SQL against
`<logging-project>.<bq-dataset>.cloudaudit_googleapis_com_*`
tables. See `04` for the schema.

**Auth.** GCP IAM. The classifier's service account needs
`bigquery.jobs.create` (in its own project) and
`roles/bigquery.dataViewer` (or a custom read role) on the audit
dataset. If the dataset is in a VPC SC perimeter, the SA's project
must be inside or bridged.

**Latency.** Per-query: 5-60 seconds depending on partition scan
size. End-to-end emit-to-queryable latency is 1-3 minutes from
event time. Not for real-time alerting; great for periodic batch.

**Fidelity.** 100 % for everything in the documented schema. Some
fields ship as JSON-as-STRING and need `JSON_EXTRACT_SCALAR`; cost
is per-row.

**Bytes you get.** Audit events only. No PTY contents (none
exist). No VPC Flow Logs (separate dataset). No GKE workload
application logs (separate dataset).

**Cost.** Per byte scanned. Date-partition aggressively; filter on
`serviceName` and `principalEmail` early; avoid `SELECT *`.

**Operational complexity.** Low. SQL + a workflow runner (cron,
Cloud Scheduler, Cloud Workflows). Zero servers.

**Recommended for.**
- Phase-1 classifier features: per-`(principal, time-window)`
  cadence, call-count, distinct-services, denial-count, UA
  fingerprint.
- Aggregate detections ("more than N denials per principal per
  day").
- Backfilling a year of data with a single date-bounded query.

### (b) Pub/Sub stream from the aggregated sink

**What.** Subscribe to the aggregated sink's Pub/Sub topic. Each
message is the JSON-encoded `LogEntry`.

**Auth.** GCP IAM with `pubsub.subscriptions.consume` on the
subscription.

**Latency.** Seconds. The lowest-latency option.

**Fidelity.** 100 %. Same `LogEntry` body as BigQuery just
delivered as a Pub/Sub message.

**Bytes you get.** Audit events only.

**Cost.** Per-message + storage. At org-level audit-log volume
this is a meaningful ongoing cost; size the subscription against
actual EPS.

**Operational complexity.** Medium. Long-lived subscriber process
that has to ack messages, handle redelivery, and dedupe by
`insertId`. Standard pattern but real engineering.

**Recommended for.**
- Real-time alerting ("page in 30 seconds when X happens").
- Streaming classification where the feature window is short.

### (c) Cloud Logging API direct (`logging.entries.list` / `tail`)

**What.** Call the Cloud Logging API directly from the classifier.

**Auth.** GCP IAM with `roles/logging.privateLogViewer` (covers
Data Access stream) or `roles/logging.viewer` (Admin Activity
only).

**Latency.** Polling: as fast as you poll, but rate-limited at 60
req/min default. Streaming with `entries.tail`: seconds.

**Fidelity.** 100 %.

**Bytes you get.** Audit events. The `tail` API is gRPC and
supports push-based streaming similar to Pub/Sub.

**Cost.** Free for reads, but rate limits bite at scale.
Aggressive polling against a busy org can hit quota.

**Operational complexity.** Medium. Stateful cursor management;
re-poll dedupe.

**Recommended for.**
- Local dev / one-off investigation. Easier than spinning up a
  Pub/Sub subscriber.
- Cases where the classifier doesn't have access to the
  aggregated sink (e.g. running in a peer project without dataset
  perms).

### (d) GCS archive direct read

**What.** Read JSON-line files from the aggregated sink's GCS
bucket.

**Auth.** GCP IAM with `storage.objects.get` on the archive
bucket.

**Latency.** Files land hourly (or per the sink's batch settings).
Not real-time.

**Fidelity.** 100 %. Each line is a JSON-encoded `LogEntry`.

**Bytes you get.** Audit events.

**Cost.** Storage class read cost. With Nearline / Archive
lifecycle transitions, retrieval is cheap but slower (Archive has
~hours retrieval SLA).

**Operational complexity.** Low. List files, fetch, parse.

**Recommended for.**
- Bulk backfill of historical data ("classify everything from
  2025").
- Cold-storage analysis where BigQuery query cost is prohibitive.

### (e) Chronicle / Google SecOps UDM Search

**What.** Query Chronicle's UDM-normalized event store via the UDM
Search API.

**Auth.** Chronicle-specific (per-instance API token + IAM).

**Latency.** Seconds for queries; ingest lag from Cloud Logging is
~minutes.

**Fidelity.** Lossy. UDM normalization drops some audit-log fields
into `additional.fields` blobs that are awkward to query. The
`request`/`response` payloads usually don't survive in queryable
form.

**Bytes you get.** UDM-normalized events with one-year+ retention.

**Cost.** Bundled in Chronicle license.

**Operational complexity.** Medium. UDM Search is its own query
language; YARA-L for rules; a separate API surface from the rest
of GCP.

**Recommended for.**
- Long-tail historical investigation ("what did this user do six
  months ago").
- Org-wide rule-based detections in YARA-L.
- Anything where Chronicle's existing detection content is
  reusable.

## Side-by-side

| | (a) BigQuery | (b) Pub/Sub | (c) Logging API | (d) GCS | (e) Chronicle |
|---|---|---|---|---|---|
| Auth | GCP IAM | GCP IAM | GCP IAM | GCP IAM | Chronicle API |
| Latency | per-query 5-60 s | seconds | minutes | hours | minutes |
| Real-time? | no | yes | partial | no | partial |
| Audit events | ✓ | ✓ | ✓ | ✓ | ✓ (UDM) |
| Backfill historical | natural | painful | painful | natural | natural |
| Aggregate query | one SQL | manual | manual | manual | YARA-L / UDM Search |
| Cost driver | bytes scanned | per message | quota | GCS reads | license |
| Single point of failure | BQ | Pub/Sub | Logging API | GCS | Chronicle |
| Operational complexity | very low | medium | medium | low | medium |
| Best for | phase-1 features, batch | real-time alerts | local dev | cold backfill | historical / rules |

## "What about reading from your existing SIEM?"

If the org already pays for Splunk, Elastic, Sentinel, or Datadog
and audit logs already land there, pulling from the SIEM is
sometimes the easiest tap. You get:

- Pre-enriched, pre-normalized events.
- Existing RBAC + audit story.
- Detection logic in the SIEM's native language.

But you lose the same things as on the Teleport side:

- **Schema fidelity drift.** SIEMs flatten and selectively project
  fields. Obscure sub-fields of rare event types may not survive.
- **Per-event cost.** SIEM ingest pricing is often per-GB or
  per-EPS; doubling up the audit-log feed for the classifier
  doubles the bill.

For the classifier, reading from BigQuery directly (option a) is
usually cheaper and higher-fidelity than going through the SIEM,
unless the SIEM is the only place certain logs are kept.

## Recommendation matrix

| Detection goal | Tap | Notes |
|----|----|----|
| Phase-1 classifier features per `(principal, window)` | (a) BigQuery | Single SQL query with date partition |
| Real-time anomaly streaming ("alert in 30 s") | (b) Pub/Sub | Subscriber dedupes by insertId |
| Bulk backfill of a year of data | (d) GCS archive then (a) BigQuery | GCS for cost, BQ for query convenience |
| One-off investigation | (c) Logging API or Logs Explorer | UI is fine for this |
| Long-tail historical | (e) Chronicle | If deployed |
| YARA-L style rules | (e) Chronicle | If deployed |

The hybrid that probably best fits this user, mirroring the
Teleport design in `notes/06`:

```
            ┌────────────────────────────┐
            │   Phase-1 features         │
            │   (a) BigQuery             │
            │   - per-(principal, window)│
            │     cadence + denial count │
            │     + UA fingerprint       │
            │     + service breadth      │
            └────────────┬───────────────┘
                         │
                         ▼
            ┌────────────────────────────┐
            │   Decision: classify       │
            │   this synthesised session?│
            └────────────┬───────────────┘
                         │ if yes:
                         ▼
            ┌────────────────────────────┐
            │   Phase-2 enrichment       │
            │   - GKE k8s.io request     │
            │     bodies                 │
            │   - IAP tunnel metadata    │
            │   - asset feed context     │
            └────────────┬───────────────┘
                         │
                         ▼
            ┌────────────────────────────┐
            │   Classifier (rules /      │
            │   LLM / both)              │
            │   → operator type, etc.    │
            └────────────────────────────┘

       ┌──────────────────────────────────┐
       │   Real-time signals if needed    │
       │   (b) Pub/Sub                    │
       │   - SA key creation / use        │
       │   - SetIamPolicy on root         │
       │   - Policy Denied surge          │
       └──────────────────────────────────┘
```

But this is sketch only. Step 2 is where this gets a real plan.

## Out of scope for step 1

- Picking the actual tap or hybrid.
- Designing the classifier prompt / model / rules.
- Storing detection state.
- Deploying anything.

All of that is `06-pipeline-design.md` material to be filled in
next.

## Cross-references

- `01-cloud-audit-logs.md` for the `LogEntry` shape.
- `02-session-and-edge-capture.md` for IAP / GKE / OS Login
  specifics.
- `04-org-aggregation-and-storage.md` for the BigQuery schema and
  example queries.
- `99-open-questions.md` for the live-tenant facts you'd need
  before committing to any one of these.
