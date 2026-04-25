# 05 — Tap points for a detection pipeline

## 30-second summary

A downstream detection / classification system has four realistic places to
tap into Teleport audit + recording data. Each has a different
auth model, latency budget, fidelity, and cost profile. None is strictly
better than the others; the right answer is usually a hybrid (real-time
gRPC for latency-sensitive signals + S3/Athena for batch and recording-content
analysis). This file enumerates the four options and gives you a recommendation
matrix; the actual pipeline design lives in step 2 (`06-pipeline-design-stub.md`).

## The four taps

### (a) Event Handler protocol — gRPC `auditlog.v1`

**What.** Connect to the auth server's TLS gRPC endpoint at
`<your-tenant>.teleport.sh:443` and call `GetUnstructuredEvents` /
`StreamUnstructuredSessionEvents` / `GetEventExportChunks` /
`ExportUnstructuredEvents`. Identical to what
`integrations/event-handler/` does internally.

**Auth.** mTLS with a Teleport-issued certificate. Mint with
`tctl auth sign --user=teleport-event-handler --out=event-handler --ttl=1000h`,
where the `teleport-event-handler` user holds a role with verbs
`event:list,read` and `session:list,read` (template at
`integrations/event-handler/tpl/teleport-event-handler-role.yaml.tpl`).
Cert TTL configurable; renew with cron.

**Latency.** Polling-based, but the polling interval is short (Event Handler
defaults to ~16 s). End-to-end emit-to-consumer is typically under a minute,
limited by the Athena consumer's `BatchMaxInterval = 1 minute` flush. For
near-real-time, this is the lowest-latency option.

**Fidelity.** 100 %. JSON-shaped (`google.protobuf.Struct`) — type info is
preserved as the `event_type` field but the typed protobuf union is unwrapped.

**Bytes you get.** Audit events fully; session recordings via
`StreamUnstructuredSessionEvents` (driven by `session.upload` notifications,
which is the Event Handler's pattern — see `02`). So both audit log AND
recording bytes are reachable through this single API.

**Cost.** Negligible — the auth server pays for the work. Subject to the
`SearchEventLimiter` token bucket (`lib/events/search_limiter.go`,
configured via `LimiterBurst` etc. in the Athena config) — Cloud may have
this set conservatively to defend against runaway clients. The bulk-export
RPCs (`GetEventExportChunks` + `ExportUnstructuredEvents`) bypass the
limiter and are the supported path for high-volume polling.

**Operational complexity.** You're running a stateful long-lived process
with a Teleport-issued cert. The state is just a polling cursor on disk.
Failure modes are all variations on "polling fell behind" or "cert expired".
Reconnection logic is well-trodden — copy from `integrations/event-handler/`.

**Recommended for.** Real-time anomaly detection. "Page someone in 60
seconds when X happens." The general-purpose answer if you only pick one.

### (b) Web UI HTTP endpoints

**What.** `lib/web/apiserver.go` exposes `GET
/webapi/sites/:site/events/search`,
`/webapi/sites/:site/events/search/sessions`, and the playback streaming
endpoints. JSON over HTTPS, browser-friendly.

**Auth.** A Teleport Web session cookie. Realistically that means a service
account user that holds the same `event:list,read` / `session:list,read`
verbs and a way to obtain a session — either WebAuthn-bypassing
service-account flow or a periodic interactive login.

**Latency.** HTTP polling only. Whatever your poll interval is.

**Fidelity.** Same shape as `SearchEvents` — full audit event payloads.
Recording playback is over WebSocket and is the same data the in-browser
player consumes.

**Cost.** Same as (a) plus an extra Web auth dependency.

**Operational complexity.** Higher than (a). You're depending on the
web-session lifetime and the Web UI's slightly different surface
(pagination is shaped differently, error responses are HTTP not gRPC).

**Recommended for.** Almost nothing in production. Useful if you can't run
a long-lived gRPC client (e.g. a constrained Lambda environment where you
don't want to bundle a Teleport gRPC client). Otherwise prefer (a).

### (c) Direct S3 read — the customer EAS bucket

**What.** Read Parquet files at `s3://teleport-longterm-<uuid>/events/`
and session-recording blobs at
`s3://teleport-longterm-<uuid>/sessions/<sid>.tar` directly from the bucket
the user already owns. No Teleport involvement at all.

**Auth.** Plain AWS IAM in the user's account. The detector's IAM principal
needs `s3:GetObject` (and probably `s3:ListBucket`, `s3:GetObjectVersion`)
on the relevant prefixes, plus `kms:Decrypt` if the bucket is SSE-KMS.

**Latency.**
- For events: Parquet files land 1-2 minutes after the originating event
  (Athena consumer's `BatchMaxInterval = 1 min` plus S3 PUT). New objects
  can be discovered via S3 EventBridge notifications or by polling the
  current day's prefix.
- For recordings: arrives when the multipart upload finalises. For
  `node-sync` mode that's near session end; for `node` async, it can be up
  to 24 h via the `UploadCompleter` if the node crashed (`02`).

**Fidelity.** 100 %. The recording bytes are the source of truth — even
`SessionPrint` (PTY content), which the audit log does *not* contain.

**Bytes you get.** Both audit events (Parquet) and recordings (binary
ProtoStreamV1). The `event_data` JSON column gives you full audit-event
detail; the recording blob gives you the actual session content.

**Cost.** S3 data-transfer + GET costs. Negligible at the audit-log scale;
non-trivial if you bulk-download every recording for content analysis. Run
the detector inside the same AWS region as the bucket.

**Operational complexity.** Lowest of all the options. No long-lived
process needed (S3 EventBridge → Lambda is fully serverless). Schema is
*the* most stable surface — the Parquet schema (`01`, `04`) and the
ProtoStreamV1 format (`02`) are both versioned independently of the
Teleport release line.

**Recommended for.**
- Bulk session-content classification (parse `<sid>.tar`, extract commands
  / network connections / typing patterns).
- Backfilling historical detections without hitting the auth server.
- Anything the user wants to keep entirely within their AWS perimeter.

### (d) Athena SQL on the audit table

**What.** Run `SELECT … FROM teleport_events_<uuid>.teleport_events WHERE …`
against the customer's Athena workgroup. See `04-cloud-and-external-audit-storage.md`
for the schema (`event_type, event_time, uid, session_id, user, event_data`)
and example query template.

**Auth.** AWS IAM with `athena:StartQueryExecution`,
`athena:GetQueryResults`, plus `glue:GetTable`/`GetPartitions`, plus
`s3:GetObject` on the events prefix and `s3:PutObject` on the
`AthenaResultsURI` (results bucket).

**Latency.** Per-query: 5-60 seconds depending on the partition scan size.
Not for real-time alerting; great for periodic batch detections (every 5
min, every hour, every day).

**Fidelity.** 100 % for everything in the six top-level columns; for
anything else, `json_extract(event_data, '$.foo')` works but is
per-row-scan expensive. Plan partition pruning by event date.

**Bytes you get.** Audit events only. No recording bytes.

**Cost.** Athena charges per byte scanned. Date-partition aggressively;
filter on `event_type` and `user` early; avoid `SELECT *`. Workgroup-level
bytes-scanned cap is your safety net.

**Operational complexity.** Just SQL + a workflow runner (cron, Step
Functions, Airflow). Zero servers.

**Recommended for.**
- Aggregate detections ("more than 10 failed logins per user per day").
- Backfilling: a single date-bounded query over a year of events is
  routine. Same query against (a)-style polling would take forever.
- Ad-hoc investigation. Athena is what the customer's analysts probably
  already use.

## Side-by-side

| | (a) gRPC | (b) Web UI | (c) S3 direct | (d) Athena |
|---|---|---|---|---|
| Auth | Teleport mTLS cert | Teleport Web session | AWS IAM | AWS IAM |
| Latency | seconds | minutes | minutes | per-query 5-60 s |
| Real-time? | yes | no | depends on EventBridge | no |
| Audit events | ✓ | ✓ | ✓ (as Parquet) | ✓ (as Parquet) |
| Recording bytes | ✓ (via stream RPC) | ✓ (via WS) | ✓ (raw `.tar`) | ✗ |
| Backfill historical | painful | painful | natural | natural |
| Aggregate query | manual | manual | manual | one SQL query |
| Cost driver | auth-server load | auth-server load | S3 GET + transfer | bytes scanned |
| Single point of failure | auth server | auth server | S3 | Athena |
| Operational complexity | medium | high | low | very low |
| Best for | real-time alerts | nothing in prod | recording analysis, backfill | batch / aggregates |

## "What about reading from your existing SIEM?"

Worth saying out loud since the user already pays for events to land in a
SIEM. Pulling from the SIEM is sometimes the easiest tap because:

- The events are already enriched, normalised, and indexed.
- The SIEM has its own RBAC and audit story.
- Detection logic in the SIEM (Splunk SPL, Elastic EQL, Sentinel KQL) is
  often a better fit than rebuilding it elsewhere.

But you lose two things compared to (c)+(d):

- **Recording bytes are not in the SIEM.** Audit events are; the
  `<sid>.tar` blobs are not, and won't be unless you build a separate
  pipeline to ingest them. So content classification (anything that needs
  to look at what was *typed* or what was *executed* under enhanced
  recording) can't run from the SIEM alone.
- **Schema fidelity drift.** Most SIEM ingest pipelines flatten or
  selectively project events. Field names get mangled. If your detection
  needs an obscure sub-field of a rare event type, double-check it survived
  the SIEM's ingest mapping.

For the user's scenario — "follow audit logs and session recordings to find
anomalous and obscure sessions, and classify each session by operator type
(human/bot/AI agent)" — the SIEM alone is insufficient because the
classifier needs the recording content. So the realistic answer is some
combination of (c) for recordings + either SIEM or (d) for events.

## Recommendation matrix

| Detection goal | Tap | Notes |
|----|----|----|
| Real-time anomaly streaming ("alert in 60 s") | (a) gRPC | Copy `integrations/event-handler/` shape |
| Bulk session classifier (parse the recording) | (c) S3 direct | Use `lib/events/playback.go::DetectFormat` + `NewProtoReader` as reference |
| Aggregate / scheduled detections | (d) Athena SQL | Date-partition, JSON-extract, per-day query |
| Replay an investigation | (a) gRPC playback or `tsh play` | gRPC is stateless, `tsh play` is interactive |
| One-off content extract from a single session | `tctl recordings download <sid>` | No AWS creds needed, see `03` |

The hybrid that probably best fits the user:

```
                ┌──────────────────────┐
                │  Real-time signals   │
                │  (a) gRPC            │
                │  - failed logins     │
                │  - new session start │
                │  - role changes      │
                └──────────┬───────────┘
                           │
                           ▼
                ┌──────────────────────┐
                │  Decision: classify  │
                │  this session?       │
                └──────────┬───────────┘
                           │ if yes:
                           ▼
                ┌──────────────────────┐
                │  Pull recording bytes│
                │  (c) S3 direct       │
                │  s3://…/<sid>.tar    │
                └──────────┬───────────┘
                           │
                           ▼
                ┌──────────────────────┐
                │  Classifier (LLM /   │
                │  rules / both)       │
                │  → operator type,    │
                │  work type, etc.     │
                └──────────────────────┘

           ┌────────────────────────────────┐
           │  Periodic / batch detections   │
           │  (d) Athena SQL                │
           │  - session counts by user      │
           │  - daily anomalies             │
           │  - long-tail patterns          │
           └────────────────────────────────┘
```

But this is sketch only. Step 2 is where this gets a real plan.

## Out of scope for step 1

- Picking the actual tap or hybrid.
- Designing the classifier prompt / model / rules.
- Storing detection state.
- Deploying anything.

All of that is `06-pipeline-design-stub.md` material to be filled in next.

## Cross-references

- `01-audit-log-plumbing.md` for the gRPC RPC reference (option a).
- `02-session-recording-plumbing.md` for the ProtoStreamV1 format you'd
  parse if you go option (c) for recordings.
- `04-cloud-and-external-audit-storage.md` for the bucket layout and
  Parquet schema underlying options (c) and (d).
- `99-open-questions.md` for the live-tenant facts you'd need before
  committing to any one of these.
