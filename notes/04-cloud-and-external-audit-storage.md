# 04 — Teleport Cloud & External Audit Storage

This file is the **weighted** one. The user's tenant runs on Teleport Cloud
with External Audit Storage (EAS) on, which means the audit log lands as
Parquet in the customer's own S3 bucket (queryable via Athena) and session
recordings stream into the same bucket as `<sid>.tar` blobs. Step 2 will
almost certainly tap one or both of those, so this file is correspondingly
deeper than its siblings.

## 30-second summary

Teleport Cloud is a managed control plane. With EAS turned on, the auth log
and recordings are *streamed* through Cloud-managed plumbing (SNS + SQS for
events, multipart upload for recordings) but *terminate* in S3 buckets that
the customer owns and controls, with credentials obtained on the fly via an
**AWS OIDC integration** that lets the Cloud-side code assume an IAM role
in the customer's account. The Cloud side keeps just enough state to buffer
in flight (SQS queue, "large events" staging bucket); everything durable is
in the customer account.

Three concrete artifacts to remember:

1. The EAS resource spec (`tctl get external_audit_storage`) names every
   bucket, prefix, Glue object, and Athena workgroup the system uses.
2. Audit events are stored as **Parquet** with a **six-column schema**.
   Anything not in those six columns has to be `json_extract`-ed from the
   `event_data` blob.
3. Session recordings live at `<SessionRecordingsURI>/<session-id>.tar`,
   one object per session, written via S3 multipart upload (so an in-flight
   session is a multipart-upload-in-progress, not yet a finalised object).

## Topology — what runs where

```
                     ┌─────────────────────────────────────┐
                     │  TELEPORT CLOUD (Gravitational AWS) │
                     │                                     │
                     │   Auth Server  Proxy Service  …    │
                     │       │            │                │
                     │       │ events     │ recordings     │
                     │       │            │                │
                     │       ▼            ▼                │
                     │   Athena Publisher │                │
                     │      ↳ SNS (Cloud)│                 │
                     │      ↳ SQS (Cloud)│                 │
                     │   Large-events S3 │                 │
                     │   (Cloud, staging)│                 │
                     │       │            │                │
                     └───────┼────────────┼────────────────┘
                             │            │
                AssumeRoleWithWebIdentity │  PutObject (multipart)
                (OIDC token from Cloud →   │  via OIDC-assumed role
                 IAM role in customer acct)│  in customer account
                             │            │
                     ┌───────┼────────────┼────────────────┐
                     │       ▼            ▼                │
                     │  Athena consumer  S3 multipart      │
                     │  → Parquet writer  uploader         │
                     │       │            │                │
                     │       ▼            ▼                │
                     │  s3://teleport-                     │
                     │     longterm-<uuid>/                │
                     │     events/  (Parquet, partitioned) │
                     │     sessions/<sid>.tar              │
                     │                                     │
                     │  s3://teleport-                     │
                     │     transient-<uuid>/               │
                     │     query_results/  (Athena results)│
                     │                                     │
                     │  Glue: db=teleport_events_<uuid>    │
                     │        table=teleport_events        │
                     │  Athena workgroup:                  │
                     │        teleport_events_<uuid>       │
                     │                                     │
                     │   CUSTOMER AWS ACCOUNT              │
                     └─────────────────────────────────────┘
```

What's Cloud-side and stays Cloud-side:
- The Athena `Publisher` that sends each event to SNS (or directly to SQS
  with `topicARN=bypass`).
- An SNS topic and an SQS queue subscribed to it.
- A staging S3 bucket for events larger than the SNS message limit
  (~256 KiB). Events are written to this bucket and the SNS message becomes
  a small `AthenaS3EventPayload{Path, VersionId}` reference
  (`lib/events/athena/consumer.go:737-770`).
- Service identity: TLS / mTLS for nodes and proxies dialling the auth
  server.

What's customer-side once EAS is active:
- The Athena `consumer` writes Parquet directly to the customer's S3 (using
  customer-account credentials it obtained via OIDC).
- Glue catalog database + table.
- Athena workgroup (so query costs and scan limits show up on the customer
  bill, in the customer's CloudTrail).
- Athena query results bucket.
- The session recordings S3 bucket.
- All KMS keys and IAM policies governing the above.

The Cloud side keeps the SQS buffer and the large-events staging bucket
because keeping SNS/SQS in the customer account would mean handing
Cloud the right to send to it, which is messier than the
read-side-only model.

## EAS resource lifecycle

The EAS resource type and lifecycle helpers are in
`api/types/externalauditstorage/externalauditstorage.go`. The OSS code
defines the *shape* and the runtime hooks; the *controller* that walks a
customer through enabling EAS lives in the closed-source `e/` tree, but the
behaviour we care about is fully observable from OSS.

There are **two named singletons**:

- `external_audit_storage/draft` (`MetaNameExternalAuditStorageDraft`)
- `external_audit_storage/cluster` (`MetaNameExternalAuditStorageCluster`)

Lifecycle (the typical onboarding flow, paraphrased from
`externalauditstorage.go:77-121` + `lib/integrations/externalauditstorage/configurator.go:154-178`):

1. **Cloud generates a draft.** A call into
   `GenerateDraftExternalAuditStorage(integrationName, region)` (L100-121)
   mints a draft with randomised resource names — for a fresh nonce
   `<uuid>`:
   - `PolicyName  = "ExternalAuditStoragePolicy-<uuid>"`
   - `SessionRecordingsURI   = "s3://teleport-longterm-<uuid>/sessions"`
   - `AuditEventsLongTermURI = "s3://teleport-longterm-<uuid>/events"`
   - `AthenaResultsURI       = "s3://teleport-transient-<uuid>/query_results"`
   - `AthenaWorkgroup        = "teleport_events_<uuid_underscored>"`
   - `GlueDatabase           = "teleport_events_<uuid_underscored>"`
   - `GlueTable              = "teleport_events"` (always — fixed)
2. **Customer applies the IaC.** The Cloud UI / docs hand the customer a
   CloudFormation (or Terraform) template that creates: the two S3 buckets
   (`teleport-longterm-<uuid>`, `teleport-transient-<uuid>`), the Glue
   database + table with the right Parquet schema, the Athena workgroup,
   and an IAM role with the policy `ExternalAuditStoragePolicy-<uuid>`. The
   IAM role's trust policy permits `sts:AssumeRoleWithWebIdentity` from
   Teleport Cloud's OIDC provider.
3. **Customer creates the AWS OIDC integration in Teleport.** A Teleport
   `integration` resource of kind `aws-oidc` referencing that IAM role.
4. **Connection test.** `NewDraftConfigurator` (L171) instantiates a
   `Configurator` against the draft and exercises `AssumeRoleWithWebIdentity`
   via the `stscreds` helper (`configurator.go:99-114`). If the round trip
   works, the draft is healthy.
5. **Promote draft → cluster.** A Cloud-side action copies the draft spec
   to the `cluster` named resource. After this, `NewConfigurator`
   (`configurator.go:154-163`) returns `IsUsed() = true` and the auth
   server's `initAuthExternalAuditLog` (`lib/service/service.go:1884`)
   takes the EAS code path.
6. **Disable.** Removing the `cluster` instance flips `IsUsed()` back to
   false on the next reload. Spec can never be edited in place — every
   change triggers an Auth restart (see comment at
   `configurator.go:81-84`: "spec won't change, because every change of
   spec triggers an Auth service reload").

`L181-184` of `configurator.go` is also worth knowing: EAS requires
*both* `modules.Cloud` and the
`entitlements.ExternalAuditStorage` license entitlement. Self-hosted
Enterprise without the entitlement gets `IsUsed() = false` and falls back
to the standard backends.

## OIDC credential exchange

The `Configurator` is the broker. Each time the Athena consumer (or the S3
session uploader) needs to talk to the customer account, it calls
`Configurator.CredentialsProvider()` (or the SDK-v1 equivalent), which
hands back AWS credentials sourced from a `CredentialsCache` in
`lib/integrations/awsoidc/credprovider/credentialscache.go`. Constants from
`configurator.go:42-49`:

```go
// v17.7.20
TokenLifetime                 = time.Hour          // OIDC token TTL
refreshBeforeExpirationPeriod = 15 * time.Minute   // pre-expiry refresh
refreshCheckInterval          = 30 * time.Second   // background ticker
retrieveTimeout               = 30 * time.Second   // per-call timeout
```

Three reliability properties to internalise:

1. **Async first, credentials later.** The Athena publisher writes to SNS
   *immediately* using Cloud creds; the customer credentials are only
   needed when the consumer drains SQS to write a Parquet batch. So a
   broken OIDC integration doesn't block emit; it just delays storage.
   `configurator.go:68-74` is explicit about this.
2. **Always retry.** When the consumer can't get credentials, it retries
   indefinitely. Events sit in SQS (which has its own ~14-day retention by
   default — confirm in your tenant). So **a broken OIDC trust policy is
   degraded availability, not data loss**, *up to* the SQS retention
   horizon.
3. **Auth bootstraps, then OIDC.** During auth-server startup the OIDC
   token signer isn't ready yet. `SetGenerateOIDCTokenFn()` is called
   later. Events emitted during this window are still SNS-published (no
   creds needed); they're held in SQS until the consumer can authenticate
   and write the batch.

## What the Athena consumer actually writes (Parquet schema)

`lib/events/athena/types.go:31-38` — six columns, no more:

```go
// v17.7.20 (verbatim)
type eventParquet struct {
    EventType string    `parquet:"event_type"`
    EventTime time.Time `parquet:"event_time,timestamp(millisecond)"`
    UID       string    `parquet:"uid"`
    SessionID string    `parquet:"session_id"`
    User      string    `parquet:"user"`
    EventData string    `parquet:"event_data"`
}
```

`event_data` is the entire audit-event JSON payload. So your Athena query
can filter cheaply on `event_type`, `event_time`, `uid`, `session_id`, and
`user`. For every other field, you `json_extract(event_data, '$.path')` —
which Athena does support, but it costs a full row scan on the partitions
the date range pulls in.

Implications for any detection-side query design:

- **Time-bound every query.** Without a `WHERE event_time BETWEEN …` you
  scan everything.
- **Filter on `event_type` and `user` early.** Both are top-level columns,
  cheap.
- **`session_id` is the join key** between session events and recording
  blobs in S3. (See `02-session-recording-plumbing.md` on the
  `session_id ↔ object key` invariant.)
- **For richer fields, JSON-extract once into a CTE and reuse.** E.g.

  ```sql
  WITH e AS (
    SELECT event_time, event_type, session_id, user,
           json_extract_scalar(event_data, '$.code')        AS code,
           json_extract_scalar(event_data, '$.cluster_name') AS cluster,
           json_extract_scalar(event_data, '$.addr.remote') AS src_ip
    FROM teleport_events_<uuid>.teleport_events
    WHERE event_time BETWEEN timestamp '2026-04-01' AND timestamp '2026-04-25'
      AND event_type = 'user.login'
  )
  SELECT user, src_ip, COUNT(*) FROM e
  WHERE code = 'T1000W' GROUP BY 1,2 HAVING COUNT(*) > 5;
  ```

- **Partition pruning is per-day.** The consumer writes one Parquet file
  (or a few, capped at `maxUniqueDaysInSingleBatch = 100`) per
  `<YYYY-MM-DD>` directory (`consumer.go:70`, key format L173). The matching
  Athena table is expected to have an `event_date DATE` partition with
  partition projection and a storage template like
  `<events-prefix>/${event_date}/`; the integration-test DDL documents this
  shape at `lib/events/athena/integration_test.go:231-257`, and the querier
  always filters `event_date BETWEEN date(?) AND date(?)`
  (`lib/events/athena/querier.go:718`).

## Bucket layout, in detail

For the user's tenant, replace `<uuid>` with the nonce baked into the
bucket names. From `tctl get external_audit_storage` you'll see:

```
SessionRecordingsURI:   s3://teleport-longterm-<uuid>/sessions
AuditEventsLongTermURI: s3://teleport-longterm-<uuid>/events
AthenaResultsURI:       s3://teleport-transient-<uuid>/query_results
```

So inside the **long-term** bucket:

```
teleport-longterm-<uuid>/
├── sessions/
│   ├── 0a1b2c3d-….tar          ← one file per session (ProtoStreamV1, despite the .tar suffix)
│   ├── 0e4f5d6c-….tar
│   └── …
└── events/
    ├── 2026-04-23/
    │   ├── 018f3a31-7a92-7d62-….parquet   ← UUIDv7-named, in-day approximate time order
    │   ├── 018f3a31-7d11-7a04-….parquet
    │   └── …
    ├── 2026-04-24/
    │   └── …
    └── 2026-04-25/
        └── …
```

And the **transient** bucket holds Athena query result manifests + CSVs at
`query_results/`. Lifecycle policy on this bucket should be aggressive —
results are intermediate.

A few details worth knowing about the long-term bucket:

- **Versioning.** The S3 session uploader's `Download()` always fetches the
  oldest version of an object (`lib/events/s3sessions/s3handler.go`,
  general behaviour). For this to be useful as a tamper-evidence layer the
  bucket needs versioning enabled. The Cloud-supplied CloudFormation
  enables it; verify with `aws s3api get-bucket-versioning`.
- **Object lock.** Every Parquet `PutObject` sets
  `ChecksumAlgorithm = SHA256` (`consumer.go:175-176`). That's what S3
  object lock requires for compliance mode. Whether object lock is *enabled*
  on the bucket is a customer choice (the CloudFormation template ships
  it off by default, I believe — confirm with `aws s3api get-object-lock-configuration`).
- **KMS.** SSE is bucket-side. `lib/events/s3sessions/s3stream.go` uses
  `ServerSideEncryptionAwsKms` if configured with an `SSEKMSKey`; otherwise
  the bucket's default SSE policy applies. Recordings inherit whatever the
  bucket says. Confirm with `aws s3api get-bucket-encryption`.

## Athena Config split (Cloud-managed vs customer-managed)

`lib/events/athena/athena.go:438-462` — `UpdateForExternalAuditStorage`
makes the cleanest possible statement of which fields come from where:

```go
// v17.7.20 (verbatim)
func (cfg *Config) UpdateForExternalAuditStorage(ctx context.Context, externalAuditStorage *externalauditstorage.Configurator) error {
    cfg.externalAuditStorage = true

    spec := externalAuditStorage.GetSpec()
    cfg.LocationS3      = spec.AuditEventsLongTermURI
    cfg.Workgroup       = spec.AthenaWorkgroup
    cfg.QueryResultsS3  = spec.AthenaResultsURI
    cfg.Database        = spec.GlueDatabase
    cfg.TableName       = spec.GlueTable
    cfg.Region          = spec.Region

    awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
        awsconfig.WithRegion(cfg.Region),
        awsconfig.WithCredentialsProvider(externalAuditStorage.CredentialsProvider()),
    )
    // …
    cfg.StorerQuerierAWSConfig = &awsCfg
    cfg.ObserveWriteEventsError = externalAuditStorage.ErrorCounter.ObserveEmitError
    return nil
}
```

So:
- **From the EAS spec (customer-controlled)**: `LocationS3`, `Workgroup`,
  `QueryResultsS3`, `Database`, `TableName`, `Region`, plus all credentials
  for the consumer's writes and the querier's reads (`StorerQuerierAWSConfig`).
- **From the Cloud-passed `audit_events_uri` (untouched by Update)**:
  `TopicARN` (SNS), `LargeEventsS3` (Cloud-side staging bucket for >256 KB
  events), `QueueURL` (SQS), plus the publisher/consumer-side credentials
  (`PublisherConsumerAWSConfig`) which always come from Cloud — see
  `athena.go:142-156` for the comment that documents this split.

So when you ever wonder "is this thing in Cloud's account or in mine?", the
mapping is field-by-field.

## Concrete commands to confirm against the live tenant

(Run these against your `<your-tenant>.teleport.sh` cluster and the customer AWS account.
None of this is in the local source code; these are the live observations
that will populate the gaps in this document.)

```bash
# 1. The EAS spec (the thing that names every other resource).
tctl get external_audit_storage

# 2. The audit log backend in effect.
tctl get cluster_audit_config

# 3. The recording mode in effect.
tctl get session_recording_config

# 4. Confirm the recordings bucket layout.
aws s3 ls s3://teleport-longterm-<uuid>/sessions/ --human-readable | head
# Expect: <session-id>.tar entries.

# 5. Confirm the events bucket layout (date-partitioned Parquet).
aws s3 ls s3://teleport-longterm-<uuid>/events/  | head
aws s3 ls s3://teleport-longterm-<uuid>/events/2026-04-25/ | head

# 6. Inspect the Glue table schema.
aws glue get-table --database-name teleport_events_<uuid> --name teleport_events
# Expect 6 data columns plus an event_date DATE partition projection backed by the events/ prefix.

# 7. Run a trivial Athena query (uses the customer workgroup, customer credentials).
aws athena start-query-execution \
  --query-string "SELECT event_type, COUNT(*) AS n
                  FROM teleport_events_<uuid>.teleport_events
                  WHERE event_time > timestamp '2026-04-24'
                  GROUP BY event_type ORDER BY n DESC LIMIT 20;" \
  --work-group  teleport_events_<uuid>
# (Then aws athena get-query-execution + get-query-results to fetch.)

# 8. KMS / encryption posture on the recordings bucket.
aws s3api get-bucket-encryption --bucket teleport-longterm-<uuid>
aws s3api get-bucket-versioning  --bucket teleport-longterm-<uuid>
aws s3api get-object-lock-configuration --bucket teleport-longterm-<uuid> 2>/dev/null

# 9. Sample a session recording (versioned Get).
aws s3api get-object --bucket teleport-longterm-<uuid> \
                     --key sessions/<session-id>.tar \
                     /tmp/sample-session
file /tmp/sample-session    # should be "data" (binary), not "POSIX tar archive"
```

## What's *not* in the customer bucket

`integrations/event-handler/teleport_event.go:30-31` calls out two event
types that get special handling: `session.upload` (for end-of-session
correlation) and `user.login` (for failed-login bookkeeping). Both end up
as ordinary rows in the Parquet table. There is no separate "Cloud-only"
class of event the customer is denied — Cloud may add internal alerting on
top of the same bucket, but the bucket *itself* is the canonical store.

The two things that visibly stay on Cloud's side: the SNS topic, SQS queue,
and large-events staging bucket. None of these are queryable from the
customer account, and none have a stable schema worth exposing.

## Failure modes specific to Cloud + EAS

1. **Broken OIDC trust policy.** Events back up in SQS. SQS retention
   defaults to 14 days. Beyond that, SQS deletes messages and you lose them.
   Symptom: missing events in Athena, `ErrorCounter` cluster-alert in the
   Web UI, growing SQS queue depth in CloudWatch (Cloud-side metrics — the
   customer can't see them directly).
2. **Glue schema drift.** If the customer's Glue table doesn't match
   `eventParquet`'s 6-column shape, Parquet writes succeed but Athena
   queries fail (or worse, return wrong types). Source of truth: the
   CloudFormation template; ensure changes go through that, not via direct
   Glue console edits.
3. **Athena workgroup limits / cost controls.** The customer can put
   per-query bytes-scanned limits on the workgroup. If those are too tight,
   the Event Handler's polling can fail with `LimitExceeded`. Conversely,
   without limits, an enthusiastic ad-hoc analyst can ring up large bills.
4. **Region drift.** EAS spec's `Region` is what `Configurator` propagates
   into the customer-side AWS config. If the buckets are in a different
   region, requests fail with mismatched region errors. Single source of
   truth: the EAS spec.
5. **In-flight multipart uploads on bucket-policy change.** If the bucket
   policy is tightened mid-session, uploads in progress can fail mid-part.
   `UploadCompleter` will eventually pick up the orphaned multipart after
   the 24-h grace and either complete it (if the new policy still allows
   `CompleteMultipartUpload`) or it'll keep failing. Use S3 lifecycle to
   abort orphaned multiparts after N days as a backstop.

## Implications for detection (preview of `05`)

Because the customer owns both buckets and the Glue/Athena objects, a
detection pipeline can plausibly tap either:

- **Athena SQL on the events table** for batch / aggregate / scheduled
  detections. Cheapest mental model; query latency is seconds-to-minutes;
  cost is per-byte-scanned.
- **Direct S3 read of `<sid>.tar` recordings** for content-based session
  classification (parse the ProtoStreamV1, extract `SessionPrint`s,
  `SessionCommand`s, etc.). No Teleport credentials required — pure
  AWS IAM.
- **Both, hybrid.** Trigger off Athena queries (e.g. "session.start with
  user.kind=bot"), and for each match, fetch and classify the recording
  bytes from S3.

The fourth tap point — Event Handler / direct gRPC — is also available but
isn't really "Cloud-specific" so it lives in `05-tap-points-for-detection.md`.

## Cross-references

- `01-audit-log-plumbing.md` — the SNS/SQS/Athena pipeline these buckets
  are the destination of.
- `02-session-recording-plumbing.md` — the ProtoStreamV1 format the
  `<sid>.tar` blobs are in, and the multipart upload that produces them.
- `03-ecosystem-and-grpc-api.md` — RBAC and configuration resources.
- `99-open-questions.md` — items in this file's narrative that need
  live-tenant verification.
