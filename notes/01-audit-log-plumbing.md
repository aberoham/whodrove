# 01 — Audit Log Plumbing

## 30-second summary

An **audit event** is a protobuf message implementing `apievents.AuditEvent`.
It is created at the point of action (a node's SSH session handler, the auth
server's login code path, etc.), wrapped in an `AsyncEmitter` that
fire-and-forgets to the auth server's gRPC, validated and stamped with cluster
metadata by a `CheckingEmitter`, and then handed to one or more storage
backends (Athena for Cloud; DynamoDB / Postgres / Firestore / file for
self-hosted). On the read side, four gRPC RPCs (`SearchEvents`,
`SearchSessionEvents`, plus the v17 `GetUnstructuredEvents` /
`StreamUnstructuredSessionEvents` / `ExportUnstructuredEvents` /
`GetEventExportChunks`) are how anything outside the auth server pulls events
back out — and the Event Handler integration is the canonical client.

Two non-obvious things to internalise up front:
- The `AsyncEmitter` **silently drops events** when its 1024-event channel
  buffer fills (`lib/events/emitter.go:118-120`). It logs at error level but
  returns nil. Lost events are not retried.
- For Cloud tenants with External Audit Storage enabled, there is **only one
  storage backend** — Athena (`lib/service/service.go:1893-1896`). Any other
  configured `audit_events_uri` is silently skipped.

## The AuditEvent shape

The interface lives in `api/types/events/` (generated from
`api/proto/teleport/legacy/types/events/events.proto`). Every event carries
**Metadata** — these are the fields you'll actually filter on:

| Field | Comes from | Notes |
|-------|------------|-------|
| `Type` | per-event constant | e.g. `"user.login"`, `"session.start"`, `"session.command"` |
| `Code` | per-event constant | T-prefixed code; see below |
| `ID` | `utils.UID` (UUID v4 typically) | Set by `CheckingEmitter` if empty (`lib/events/emitter.go:191-205`) |
| `Time` | `clockwork.Clock.Now().UTC()` | Set by `CheckingEmitter` if zero |
| `Index` | per-session monotonic | Index *within* a session recording stream; not unique across sessions |
| `ClusterName` | injected by `CheckingEmitter` | Stamped on the way through |

Plus per-type sub-structs: `UserMetadata` (user, login, impersonator,
user_kind={human,bot}, user_origin={local,sso,okta,scim,…}),
`SessionMetadata` (SessionID, WithMFA, PrivateKeyPolicy), `ServerMetadata`
(ServerID, ServerHostname, ServerAddr, ServerLabels, ServerVersion),
`ConnectionMetadata` (LocalAddr, RemoteAddr, Protocol).

### Event codes (the T-numbers)

Defined in `lib/events/codes.go`. The convention (`lib/events/codes.go:31-43`):

- `T` prefix
- 4-digit number, grouped by topic: `1xxx` = user, `2xxx` = session,
  `4xxx` = enhanced/BPF, `5xxx` = trusted cluster / SSO, `7xxx` = Kubernetes,
  `8xxx` = app/db, `TBL0x` = billing, etc.
- 1-letter severity suffix: `I` info, `W` warn (expected failures), `E` error.

Anchor codes worth memorising:

| Code | Constant | Meaning |
|------|----------|---------|
| `T1000I` / `T1000W` | `UserLocalLoginCode` / `UserLocalLoginFailureCode` | Local login success / failure |
| `T1001I` / `T1001W` | `UserSSOLoginCode` / `UserSSOLoginFailureCode` | SSO login success / failure |
| `T2000I` | `SessionStartCode` | Interactive session started |
| `T2001I` / `T2003I` | `SessionJoinCode` / `SessionLeaveCode` | Participant joined / left |
| `T2004I` | `SessionEndCode` | Session ended (interactive) |
| `T2005I` | `SessionUploadCode` | Recording uploaded — the bridge between audit log and S3 object |
| `T2006I` | `SessionDataCode` | Per-session aggregate stats (bytes, duration) |

Note on the source agent's earlier notes: `SessionStart` is **`T2000I`**, not
`T3000I`. The `T3xxx` range is access-request / role events. Don't trust
synthesised lists — `lib/events/codes.go` is the source of truth and currently
defines ~250 codes. The header comment says when adding a new code, also update
`web/packages/teleport/src/services/audit/types.ts` and `eventsMap` in
`lib/events/events_test.go`; both are useful cross-references.

## Emission: who calls what

```
Caller (node, proxy, auth server itself)
   │
   ├── apievents.Emitter.EmitAuditEvent(ctx, event)
   │
   ▼
AsyncEmitter (lib/events/emitter.go:80-122)
   │  - 1024-event buffered channel (AsyncBufferSize, L39)
   │  - default: drop on overflow, log at error level (L118-120)
   │  - background goroutine forwards to inner emitter
   │
   ▼
gRPC to auth server (or local in-process call when caller IS the auth server)
   │
   ▼
CheckingEmitter (lib/events/emitter.go:148-184)
   │  - validates Type, Code (skip for SessionPrint / DesktopRecording)
   │  - sets ID, Time, ClusterName
   │  - increments Prometheus counters: audit_emit_events,
   │    audit_emitted_event_sizes, audit_failed_emit_events
   │
   ▼
The configured backend (or MultiEmitter wrapping several)
```

### Worked example: SSH session start

`lib/srv/sess.go::emitSessionStartEvent` constructs a `SessionStart`
populated with `ServerMetadata`, `SessionMetadata`, `UserMetadata`,
`ConnectionMetadata`, the `SessionRecording` mode in effect, the initial
command and `Reason` (from the `TELEPORT_SSH_SESSION_REASON` env var). It then
does two things in sequence:
1. Records the event to the **session recording stream** via
   `s.Recorder().PrepareSessionEvent(...)` then `s.recordEvent(...)`. This is
   what becomes the bytes in S3.
2. Emits to the **audit log** via `s.emitAuditEvent(...)`. This goes through
   the `AsyncEmitter` to the auth server.

The same prepared event lands in *both* places. That double-write is the
mechanism by which the audit-log `SessionStart` row and the S3
`<sid>.tar` blob refer to the same session.

### Worked example: user.login

`lib/auth/methods.go::emitAuthAuditEvent` runs on the auth server itself, so
there's no AsyncEmitter — emission is synchronous. The event captures
`Method` (`local`, `oidc`, `saml`, `github`), `MFADevice`, `RemoteAddr`,
`UserAgent` (trimmed), `RequiredPrivateKeyPolicy`, plus a `Status` sub-message
with `Success: true|false` and an error string on failure.

## Auth server fan-out: MultiLog

`lib/events/multilog.go:33-153` defines `MultiLog`. It composes multiple
`AuditLogger` backends:

- **Writes** (via the embedded `MultiEmitter`, `lib/events/emitter.go:300-315`)
  fan out to **all** backends. Errors are aggregated via `trace.NewAggregate`
  but no backend's failure short-circuits the others.
- **Reads** (`SearchEvents` L73, `SearchSessionEvents` L145,
  `ExportUnstructuredEvents` L83, `GetEventExportChunks` L111) walk the
  backend list in order and return the result of the **first backend that
  doesn't return `trace.NotImplemented`**. So you can stack a
  feature-rich primary with a degraded secondary; reads always come from the
  primary.

In v17 Cloud with EAS, only one backend (Athena) is wired up, so `MultiLog`
isn't even constructed — `lib/service/service.go:2034-2038` returns the
single logger directly.

## Where the auth server picks its backend

`lib/service/service.go::initAuthExternalAuditLog` at `L1884-2039`. Iterates
over `auditConfig.AuditEventsURIs()` (the `cluster_audit_config` resource).
Switch on URI scheme:

- `dynamodb://`
- `firestore://`
- `postgres://` (`pgevents`)
- `athena://` (Cloud)
- `file:`
- `stdout:` (testing)

The EAS short-circuit at `L1893-1896`:

```go
// v17.7.20
if externalAuditStorage.IsUsed() && (len(loggers) > 0 || uri.Scheme != teleport.ComponentAthena) {
    process.logger.InfoContext(ctx, "Skipping events URI because External Audit Storage is enabled", "events_uri", eventsURI)
    continue
}
```

Plain English: when EAS is on, only the *first* `athena://…` URI is honored.
Everything else from `cluster_audit_config` is ignored. So in Cloud you get
exactly one backend.

For Athena specifically, `L1962-1968` then patches the config:

```go
// v17.7.20
if externalAuditStorage.IsUsed() {
    // External Audit Storage uses the topicArn, largeEventsS3, and
    // queueURL from the athena audit_events_uri passed by cloud,
    // and overwrites the remaining fields.
    if err := cfg.UpdateForExternalAuditStorage(ctx, externalAuditStorage); err != nil {
        return nil, trace.Wrap(err)
    }
}
```

So three Athena fields stay Cloud-managed (the SNS topic, the large-events S3
bucket for >256 KB events, and the SQS queue URL) and the rest (Glue database
& table, location-S3 prefix for Parquet, Athena workgroup, results URI) come
from the **customer**'s EAS spec. Detail of which is which is in
`04-cloud-and-external-audit-storage.md`.

`L1976` also wraps the logger in `externalAuditStorage.ErrorCounter` so per-write
errors get counted into a Cloud-internal alarm. And `L1978-1989` wraps reads
in `events.NewSearchEventLimiter` (a token-bucket rate limit on `SearchEvents`)
when the Athena config sets `LimiterBurst > 0` — relevant if you find your
SIEM-side polling getting throttled.

## Storage backends: a tour

### Athena (`lib/events/athena/`) — the Cloud one

`Config` at `lib/events/athena/athena.go:64-171`. Defaults:

- `defaultBatchItems = 20000` (L54)
- `defaultBatchInterval = 1 minute` (L56)
- `topicARNBypass = "bypass"` (L61) — magic value to skip SNS and publish
  directly to SQS

Two AWS configs in the struct (`athena.go:142-156`):
- `PublisherConsumerAWSConfig` — always Cloud-internal credentials. Used to
  publish to SNS/SQS and to *download* large events from the Cloud-side
  staging bucket.
- `StorerQuerierAWSConfig` — Cloud-internal *or* the customer's account when
  EAS is on. Used to write Parquet to the long-term bucket and to run Athena
  queries.

Data flow on the **write path**:

1. Auth server emits an event. Athena `Publisher`
   (`lib/events/athena/publisher.go`) base64-encodes the protobuf and:
   - publishes it to SNS as a message attribute `payload_type=raw_proto_event`
     if it fits within ~256 KB; or
   - writes it to the large-events S3 bucket with a generated key, then
     publishes a small SNS message with attribute
     `payload_type=s3_event` whose body is a marshalled
     `apievents.AthenaS3EventPayload{Path, VersionId}`
     (`lib/events/athena/consumer.go:743-770`).
2. The Cloud-managed SQS queue is subscribed to that SNS topic and accumulates
   messages.
3. The Athena `consumer` (`lib/events/athena/consumer.go:117-187`) drains SQS
   in batches up to `BatchMaxItems = 20000` or `BatchMaxInterval = 1 minute`,
   resolves S3-staged large events, converts each event to its Parquet row
   (see schema below), groups rows by event date, and writes one Parquet file
   per date per batch to S3.
4. The Parquet object key is
   `<locationS3Prefix>/<YYYY-MM-DD>/<uuidv7>.parquet`
   (`consumer.go:173`). UUIDv7 is used so files within a day are roughly time-ordered:

   ```go
   // v17.7.20
   key := fmt.Sprintf("%s/%s/%s.parquet", cfg.locationS3Prefix, date, id.String())
   ```

5. Object lock support: every Parquet `PutObject` sets
   `ChecksumAlgorithm = SHA256` (`consumer.go:175-176`), so the customer can
   enable S3 Object Lock on the long-term bucket without breaking writes.

The cap at `consumer.go:70` says **`maxUniqueDaysInSingleBatch = 100`** — this
is to bound memory during cross-date migrations; a normal steady-state batch
contains rows for one day.

#### Parquet column schema (`lib/events/athena/types.go:31-38`)

```go
// v17.7.20
type eventParquet struct {
    EventType string    `parquet:"event_type"`
    EventTime time.Time `parquet:"event_time,timestamp(millisecond)"`
    UID       string    `parquet:"uid"`
    SessionID string    `parquet:"session_id"`
    User      string    `parquet:"user"`
    EventData string    `parquet:"event_data"`
}
```

**Six columns. That's it.** Anything beyond `event_type`, `event_time`,
`uid`, `session_id`, `user` you have to fish out of `event_data`, which is
the FastJSON-marshalled full event payload. So if your detection wants to
filter by `code`, `cluster_name`, `server_addr`, `connection.remote_addr`,
etc. — `json_extract(event_data, '$.code')` from Athena. This shape directly
constrains how you query the bucket; it is the most important fact in this
whole document for someone planning Athena-side detections.

### DynamoDB (`lib/events/dynamoevents/`)

Single-region DynamoDB table, partition + sort key by event ID and time.
Honors `cluster_audit_config` knobs for read/write capacity, auto-scaling,
continuous backups, retention, FIPS endpoints
(`lib/service/service.go:1924-1937`). Standard for self-hosted on AWS without
EAS.

### Postgres (`lib/events/pgevents/`)

Standard relational schema with JSONB payload and time index. For self-hosted
deployments backed by RDS / Cloud SQL.

### Firestore (`lib/events/firestoreevents/`)

GCP-native. Document-per-event.

### File log (`lib/events/filelog.go`)

JSON-per-line into a directory. Symlink `events.log` (`auditlog.go:78`)
points to the day's file. Used by self-hosted small deployments and as a
fallback in dev/test.

### Stdout

Used in tests.

## Read-side APIs

Two generations co-exist in v17:

### Legacy (`api/proto/teleport/legacy/client/proto/authservice.proto`)

- `SearchEvents(SearchEventsRequest) → ([]Event, lastKey)` — date range,
  event-type filter, paginated. Hard cap of 1 MiB of payload per call
  (`lib/events/multilog.go:73-81`).
- `SearchSessionEvents(SearchSessionEventsRequest) → ([]Event, lastKey)` —
  same shape but only `session.end` and `windows.desktop.session.end`.
- `StreamSessionEvents(session_id, start_index) → stream Event` — pull every
  event from one session recording, in order. This is the playback bridge.

### v17 unstructured (`api/proto/teleport/auditlog/v1/auditlog.proto`)

Four RPCs on `AuditLogService`:

- `GetUnstructuredEvents(GetUnstructuredEventsRequest) → EventsUnstructured` —
  same query shape as `SearchEvents` but returns
  `google.protobuf.Struct` (JSON-shaped) events instead of typed protobufs.
  This is what the Event Handler uses for ordered polling.
- `StreamUnstructuredSessionEvents(session_id, start_index) → stream EventUnstructured` —
  unstructured equivalent of `StreamSessionEvents`.
- `GetEventExportChunks(date) → stream EventExportChunk` — returns *chunks*
  (opaque shard tokens) for a date. **The proto comment on L36-37 is critical**:
  > "the returned list isn't ordered and polling for new chunks requires
  > re-consuming the entire stream from the beginning."
  So the consumer must dedupe by event UID across re-polls.
- `ExportUnstructuredEvents({date, chunk, cursor}) → stream ExportEventUnstructured` —
  given a chunk token from above, stream the events in that shard. Optimised
  for bulk export over correctness-of-ordering. The cursor lets you resume an
  interrupted shard mid-stream.

The whole v17 API treats events as JSON `Struct` — shape is type, id, time,
index, and `unstructured` (the JSON-encoded payload).

### The Event Handler integration

`integrations/event-handler/` is Gravitational's binary that polls the auth
server and forwards JSON to Fluentd. Two parallel jobs (`app.go`):

- `EventsJob` — calls `GetUnstructuredEvents` (or `GetEventExportChunks` +
  `ExportUnstructuredEvents` on newer auth servers) on a polling loop, persists
  a cursor to disk, posts each event as JSON to a Fluentd HTTP endpoint.
- `SessionEventsJob` — listens for `session.upload` events (note: that's the
  bridging event, not `session.end` — see
  `integrations/event-handler/teleport_event.go:30-31, L87-89`), then for each
  uploaded session pulls its full recording via
  `StreamUnstructuredSessionEvents` and forwards each event individually.

The output channel is HTTPS to Fluentd with mTLS
(`integrations/event-handler/fluentd_client.go`). Fluentd is then responsible
for routing into Splunk / Elastic / Datadog / your SIEM of choice. The Event
Handler authenticates *to* Teleport using a cert minted from
`tctl auth sign --user=teleport-event-handler` against the role template at
`integrations/event-handler/tpl/teleport-event-handler-role.yaml.tpl` (verbs:
`list`, `read` on resources `event` and `session`).

## Failure modes and gotchas

1. **AsyncEmitter buffer overflow → silent drop.** Watch for spikes in the
   `audit_failed_emit_events` Prometheus counter. The reverse-tunnel /
   auth-server connection being slow is the usual cause. There is no replay.
2. **Cloud + EAS uses one backend only.** Don't expect "events written to the
   customer bucket AND DynamoDB redundancy"; the `service.go:1893` short-circuit
   means only the first Athena URI survives.
3. **Athena write path is asynchronous.** Worst-case latency from emit to
   Parquet-on-disk is roughly `BatchMaxInterval + flush time` ≈ 60-90 s.
   Don't expect events to be queryable in real time.
4. **`SearchEvents` is rate-limited under EAS.** The `SearchEventLimiter`
   wrap (`service.go:1980`) defends the auth server from the customer SIEM.
   Errors look like `LimitExceeded`. The Event Handler avoids this entirely
   by using the bulk export RPCs.
5. **Event size > 256 KB → S3 staging round-trip.** Adds latency and a second
   IAM dependency on the publisher AWS config. Unusual events (huge access
   requests, big SAML responses) can hit this.
6. **Trimming.** `audit_stored_trimmed_events` and
   `audit_queried_trimmed_events` Prometheus counters
   (`lib/events/auditlog.go:160-176`) track events whose fields were trimmed
   to fit a max-payload bound. Exact trim policy is in
   `lib/events/sizelimit.go` (not read here; see open question #1).
7. **`GetEventExportChunks` is not append-only.** Re-polling a date returns
   the same set of chunks (unordered). Consumers MUST dedupe by event UID;
   the Event Handler does this via its on-disk cursor state.

## Things that are *not* in the audit log

- `SessionPrint` events (raw PTY bytes) and `DesktopRecording` events (TDP
  frames). These are recording-only — they go to S3 but never land in Athena.
  See `02-session-recording-plumbing.md`.
- `Resize`. Same.
- BPF `SessionCommand` / `SessionNetwork` / `SessionDisk` events when enhanced
  recording is on. Same — recording-only by default. Confirm against
  `lib/events/auditlog.go` and `02` for the exact list (open question #2).

## Cross-references

- For where this all lives in S3 / Athena physically and how to query it
  from outside: `04-cloud-and-external-audit-storage.md`.
- For the gRPC surface, RBAC, and inspection commands: `03-ecosystem-and-grpc-api.md`.
- For how a downstream system would actually consume any of this:
  `05-tap-points-for-detection.md`.
