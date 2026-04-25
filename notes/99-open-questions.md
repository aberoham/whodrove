# 99 — Open questions

Things this round of source-reading couldn't answer with confidence. Each item
has a way to verify (against either the live tenant or further code reading).
Carry these forward into step 2 and update the notes when answers come in.

## Tenant-state questions (need live `<your-tenant>.teleport.sh` access)

### Q1. What's the actual recording mode?
Plausibly `node-sync` for Cloud + EAS, but `tctl get session_recording_config`
is the source of truth. Affects `02-session-recording-plumbing.md` (sync vs
async) and the failure-mode analysis.

```bash
tctl get session_recording_config
```

### Q2. What's the EAS spec, exactly?
Names every bucket / Glue object / Athena workgroup. Used to fill in the
`<uuid>` placeholders in `04-cloud-and-external-audit-storage.md`.

```bash
tctl get external_audit_storage
```

### Q3. What's the audit retention period and the cluster_audit_config in detail?
`RetentionPeriod()` is exposed but Cloud may have a tenant-specific value.

```bash
tctl get cluster_audit_config
```

### Q4. Does the recordings bucket have versioning + object lock + KMS?
Affects tamper evidence story. The CloudFormation defaults usually enable
versioning and SSE-KMS; object lock varies.

```bash
aws s3api get-bucket-versioning --bucket teleport-longterm-<uuid>
aws s3api get-bucket-encryption --bucket teleport-longterm-<uuid>
aws s3api get-object-lock-configuration --bucket teleport-longterm-<uuid> 2>/dev/null
```

### Q5. Glue table partition + schema
Source expects six data columns plus an `event_date DATE` partition projection
whose storage template maps to `<events-prefix>/${event_date}/`
(`lib/events/athena/integration_test.go:231-257`). Verify the live Glue table
matches that shape; direct Glue edits or template drift would make Athena
queries expensive or wrong.

```bash
aws glue get-table --database-name teleport_events_<uuid> --name teleport_events
aws glue get-partitions --database-name teleport_events_<uuid> --table-name teleport_events --max-results 5
```

### Q6. Is BPF / enhanced session recording enabled on any roles?
`SessionCommand` / `SessionNetwork` / `SessionDisk` events only exist if a
role with `enhanced_recording` is in use, AND the node is Linux with BPF
support. Materially changes what a classifier can do without parsing PTY bytes.

```bash
tctl get roles | grep -A5 enhanced_recording
# or
tctl get roles --format=json | jq '.[] | {name: .metadata.name, enhanced_recording: .spec.options.enhanced_recording}'
```

### Q7. Event Handler in production?
Is the user already running the Event Handler against this tenant? If so, what
output target (Splunk, Fluentd-to-Elastic, Datadog)? Affects step-2 decision
about tapping the SIEM vs tapping upstream.

(Ask the user; not derivable from source.)

**Resolved-by-design (2026-04-25, see `06-pipeline-design.md`).** The step-2
design taps Athena + S3 directly and does not depend on the Event Handler,
so this question no longer blocks step 3. Still worth knowing operationally
if the user later wants the SIEM path back, but not on the critical path.

## Source-side checks and questions (need code reading, not live access)

### Q8. Exact event-trim policy
`MetricStoredTrimmedEvents` and `MetricQueriedTrimmedEvents` Prometheus
counters confirm trimming happens, but the precise field-trim rules live in
`lib/events/sizelimit.go`, which this round didn't open. Important if the
classifier needs full payloads on rare large events.

```bash
( cd upstream-repo && wc -l lib/events/sizelimit.go && grep -n 'Trim\|MaxSize\|max_size' lib/events/sizelimit.go | head -20 )
```

### Q9. SQS retention default in EAS-backing infra
Affects how long a broken OIDC integration can be down before events are
permanently lost. AWS default is 4 days, max 14. Cloud likely sets it
explicitly, but the value isn't in OSS source — it's set by the Cloud
provisioning code.

(Cloud-side, not knowable from this repo. Ask Gravitational support, or
infer from `aws sqs get-queue-attributes` if you can identify the queue,
which the customer typically can't.)

### Q10. Publisher SNS large-event threshold
The publisher sets `maxDirectMessageSize = 250 * 1024`
(`lib/events/athena/publisher.go:52-55`), intentionally below AWS's 256 KiB
limit to leave room for headers. Keep this as a source-verified fact rather
than a live-tenant question; it matters mildly because a hard cliff in
event-size handling can explain latency outliers.

```bash
( cd upstream-repo && grep -n 'maxDirectMessageSize\|MaxMessageSize\|sizeLimit' lib/events/athena/publisher.go | head -20 )
```

### Q11. v17 RBAC check locations for event reads
Source spot-check: `SearchEvents` is at
`lib/auth/auth_with_roles.go:6419`; `SearchSessionEvents` is at
`lib/auth/auth_with_roles.go:6453`; `StreamSessionEvents` is at
`lib/auth/auth_with_roles.go:6557`. Keep these line references fresh if
the repo is re-pinned.

```bash
( cd upstream-repo && grep -n 'func .*ServerWithRoles.* SearchEvents\|func .*ServerWithRoles.* SearchSessionEvents' lib/auth/auth_with_roles.go )
```

### Q12. Legacy `SearchEvents` path under v17 Athena
Source spot-check: it is. `GetUnstructuredEvents` calls
`ServerWithRoles.SearchEvents` (`lib/auth/grpcserver.go:6011-6024`), which
calls the audit log's `SearchEvents` path. Athena implements that at
`lib/events/athena/athena.go:528-529`. The practical open question for step 2
is not existence, but whether a custom consumer should use ordered polling
(`GetUnstructuredEvents`) or bulk export (`GetEventExportChunks` +
`ExportUnstructuredEvents`) for its latency/cost target.

(Trace `MultiLog.SearchEvents` and Athena `Log.SearchEvents` to the gRPC
handler to confirm.)

### Q13. Event ordering guarantees
What ordering can a detector safely assume across sessions and within a single
session? Within a ProtoStream recording, `StreamSessionEvents` reads events
sequentially and filters by event index (`lib/events/auditlog.go:546-577`).
Across sessions / audit-log queries, Athena search orders by query parameters,
while bulk export chunks are explicitly unordered. Verify the exact guarantee
before designing dedupe and state transitions.

```bash
( cd upstream-repo && grep -n 'ORDER BY\\|event_time\\|event_index\\|session_id' lib/events/athena/querier.go | head -40 )
```

### Q14. Event Handler delivery semantics on crash mid-Fluentd-write
The notes say the Event Handler persists cursor state and forwards to Fluentd,
but step 2 needs the exact delivery semantics: at-least-once vs at-most-once
around "sent to Fluentd but cursor not persisted" and "cursor persisted but
Fluentd write failed". Verify before relying on it for alerting correctness.

```bash
( cd upstream-repo/integrations/event-handler && grep -R -n 'cursor\\|state\\|fluentd\\|Update' *.go )
```

## Behaviour questions (need a small experiment)

### Q15. End-to-end emit-to-Parquet latency in this tenant
Run a known-shape benign event (e.g. a fake `tsh login`, or just any
`session.start`), then time how long until it shows up in an Athena query.
Confirms (or refutes) the "1-2 minute" estimate in the notes.

```sql
SELECT MAX(event_time)
FROM teleport_events_<uuid>.teleport_events
WHERE event_time > current_timestamp - interval '5' minute;
```
Run repeatedly after generating a known event.

### Q16. End-to-end node-side-emit to S3-recording-finalised
For `node-sync`: time `session.start` audit row → `session.upload` audit row
(when the multipart finalises). Tells you the "earliest moment a classifier
could fetch the recording".

### Q17. Behaviour of `GetEventExportChunks` re-poll dedup
The proto says re-polling re-emits chunks unordered. Implement a tiny
consumer that re-polls a date and verify the chunks really are duplicates by
event UID. Important for any consumer not using the Event Handler.

**Resolved-by-design (2026-04-25, see `06-pipeline-design.md`).** The step-2
design does not call `GetEventExportChunks`. Athena rows are addressable by
`event_date` partition + `uid`, and recordings are addressed by `<sid>.tar`.
Re-poll dedupe semantics are not on the critical path.

## Things explicitly *not* worth running down right now

(Recorded so we don't keep tripping over them.)

- Whether session recordings are end-to-end signed. They aren't —
  integrity is bucket-side via S3 ETags / KMS / object lock. Don't
  expect HMACs.
- Whether the auth server replays dropped `AsyncEmitter` events on
  reconnect. It doesn't — silent drop is the documented behaviour.
- Whether multiple Event Handlers can run against the same cursor for HA.
  They can't (no distributed lock); for HA, run a single instance with
  k8s `replicas: 1` + restart-on-failure, or shard by date range.

## How to use this list

Step 2 closed Q7 and Q17 by design (`06-pipeline-design.md`). Before
step 3, knock down Q1-Q6 (live tenant facts) — they're cheap and replace
the `<uuid>` / `<your-tenant>` placeholders that the design and the
classifier rules will reference. Q8-Q14 are source-side checks to answer
when a specific step-3 design depends on them. Q15-Q16 are calibration:
do them once the tap is exercised against the live tenant so the latency
numbers have actual measurements behind them.
