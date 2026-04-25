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
Specifically: is the `teleport_events` table partitioned by `event_date`? If
so, how is the partition key derived from the object path? Without partition
pruning Athena queries are far more expensive.

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

## Source-side questions (need more code reading, not live access)

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

### Q10. Whether the publisher's SNS large-event threshold is exactly 256 KB
Comments say "~256 KB" matching SNS limits, but the actual number is set in
the publisher code (`lib/events/athena/publisher.go`). This round didn't
open it. Matters mildly because a hard cliff in event-size handling can
explain weird latency outliers.

```bash
( cd upstream-repo && grep -n 'maxDirectMessageSize\|MaxMessageSize\|sizeLimit' lib/events/athena/publisher.go | head -20 )
```

### Q11. Where exactly is the v17 RBAC check on `SearchEvents`?
Notes cite ~L6418 of `lib/auth/auth_with_roles.go` based on agent
synthesis. Verify and update.

```bash
( cd upstream-repo && grep -n 'func .*ServerWithRoles.* SearchEvents\|func .*ServerWithRoles.* SearchSessionEvents' lib/auth/auth_with_roles.go )
```

### Q12. Whether the legacy `SearchEvents` path is still wired through
in v17 against Athena, or if everything has migrated to the unstructured
RPCs. Both APIs exist; what does the auth server actually serve from? Affects
which path is rate-limited and which is not.

(Trace `MultiLog.SearchEvents` and Athena `Log.SearchEvents` to the gRPC
handler to confirm.)

## Behaviour questions (need a small experiment)

### Q13. End-to-end emit-to-Parquet latency in this tenant
Run a known-shape benign event (e.g. a fake `tsh login`, or just any
`session.start`), then time how long until it shows up in an Athena query.
Confirms (or refutes) the "1-2 minute" estimate in the notes.

```sql
SELECT MAX(event_time)
FROM teleport_events_<uuid>.teleport_events
WHERE event_time > current_timestamp - interval '5' minute;
```
Run repeatedly after generating a known event.

### Q14. End-to-end node-side-emit to S3-recording-finalised
For `node-sync`: time `session.start` audit row → `session.upload` audit row
(when the multipart finalises). Tells you the "earliest moment a classifier
could fetch the recording".

### Q15. Behaviour of `GetEventExportChunks` re-poll dedup
The proto says re-polling re-emits chunks unordered. Implement a tiny
consumer that re-polls a date and verify the chunks really are duplicates by
event UID. Important for any consumer not using the Event Handler.

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

When step 2 starts, knock down Q1-Q7 first (live tenant facts) — they're
cheap and will sharpen most of the design choices in
`06-pipeline-design-stub.md`. Q8-Q12 only need answering once a specific
design depends on them. Q13-Q15 are calibration; do them once a tap is
chosen so the latency numbers in the design have actual measurements
behind them.
