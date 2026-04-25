# 00 — The plan we used for step 1

> Historical artifact, preserved for context. This is the plan that was
> agreed at the start of step 1 and that produced the rest of `notes/`.
> Future sessions don't need to read this to use the notes — start at
> `README.md` for that. Read this only if you want to know **how** the notes
> came to look the way they do, or if you're about to do step 2 and want a
> template for that round's plan.
>
> Two notable things about this artifact:
>
> 1. The "What we already learned" section below was a synthesis from three
>    parallel Explore agents *before* we read source ourselves. Some of what
>    those agents said turned out to be wrong (`SessionStart` is `T2000I`
>    not `T3000I`; the completer grace period is 24 h not 4 h; Cloud + EAS
>    actually only wires one backend, not multiple). The verified versions
>    landed in `notes/01..05`. The unverified-but-still-useful planning text
>    is left in below for honesty — see "Corrections during execution" at
>    the end of this file for the full list of what changed.
> 2. Absolute filesystem paths in the original plan have been replaced with
>    project-relative paths (the original lived under
>    `~/.claude/plans/...` and was never meant to be checked in).

---

# Plan — Step 1: Understand Teleport v17 Audit & Session Recording Plumbing

## Context

The user is the administrative owner of a Teleport Enterprise Cloud tenant
(`<your-tenant>.teleport.sh`) hosted by Gravitational on AWS, running v17. Audit
events are forwarded to their SIEM and session recordings stream to a
customer-owned S3 bucket (i.e. they have **External Audit Storage** enabled).

The end goal is a multi-step program:

1. **(this plan)** Build first-principles understanding of how Teleport
   captures audit events and session recordings, how those are transported, and
   where they ultimately land. Documented as durable markdown research notes
   that future Claude Code sessions and humans can refer back to.
2. Decide where the user's detection apparatus should tap into the system.
3. Build the detections / agent-driven session classifier.

This plan covers **only step 1**. It produces a small set of markdown notes
under `notes/` (a sibling of `upstream-repo/` at the project root) derived
strictly from the v17 source at `upstream-repo/` (currently detached at
`Release 17.7.20`, commit `2797910`).

## What we already learned (Phase 1 exploration synthesis)

Three Explore agents read the v17 source. These are the load-bearing facts that
the notes must capture; details and `path:line` references live in the file
outlines below.

### Audit log

- **Event model**: an `apievents.AuditEvent` is a protobuf message defined in
  `api/proto/teleport/legacy/types/events/events.proto`. It carries
  `Metadata{ID, Type, Code, Time, Index, ClusterName}` plus per-type
  fields. ~250 distinct codes, T-prefixed (`T1000I` = local login success,
  `T3000I` = `session.start`, `T4000I` = BPF `session.command`, etc.) — defined
  in `lib/events/codes.go`.
- **Emission**: callers use `apievents.Emitter`. Nodes/proxies wrap it in
  `lib/events/emitter.go` `AsyncEmitter` (1024-event buffer, **silently drops on
  overflow** — important gotcha). Auth-server-side emission is synchronous.
- **Auth server pipeline**: events fan out through `lib/events/multilog.go`
  `MultiLog` to one or more storage backends, optionally wrapped in
  `lib/events/search_limiter.go` for read-side rate limiting. Wiring lives in
  `lib/service/service.go::initAuthExternalAuditLog` (~line 1883).
- **Storage backends**: `lib/events/{athena,dynamoevents,pgevents,firestoreevents,filelog.go,azsessions}`.
  Cloud uses **Athena**: events publish to SNS → SQS → batched Parquet in S3,
  queryable via Athena SQL; events >250 KB are staged in S3 with a pointer in
  SNS.
- **Read APIs (gRPC)**: `SearchEvents`, `SearchSessionEvents`,
  `StreamSessionEvents`, and the newer `GetEventExportChunks` /
  `GetUnstructuredEvents` / `StreamUnstructuredSessionEvents` (defined in
  `api/proto/teleport/auditlog/v1/auditlog.proto`). The newer "unstructured"
  RPCs return JSON-shaped `google.protobuf.Struct` and are what the Event
  Handler uses.
- **Event Handler** (`integrations/event-handler/`) is Gravitational's
  canonical bridge: mTLS-authenticates with a cert minted by `tctl auth sign`
  for the `teleport-event-handler` role (`event:list,read`,
  `session:list,read`), polls events, persists a cursor in a state file,
  forwards JSON to Fluentd → SIEM.

### Session recording

- **What a recording is**: a binary stream of protobuf
  `apievents.AuditEvent`s — `SessionStart`, `SessionPrint` (raw PTY bytes),
  `Resize`, `SessionJoin/Leave`, `SessionEnd`, plus protocol-specific
  variants (`KubeRequest`, `DatabaseSessionQuery`, `DesktopRecording`/TDP
  frames, BPF `SessionCommand/Network/Disk`).
- **Container format**: `ProtoStreamV1` with an 8-byte magic header, slices of
  ≥5 MiB, gzip-compressed records. Final S3 object key is
  `<path>/<session-id>.tar` — code at `lib/events/s3sessions/s3handler.go`
  `path()` ~L517.
- **Recording modes**: `off`, `node`, `node-sync`, `proxy`, `proxy-sync`
  (defined in `api/types/constants.go`). Sync streams live to backend via
  `ProtoStreamer`; async writes slices to local disk via
  `lib/events/filesessions/` and a separate uploader drains them to S3.
- **Capture path**: `lib/srv/sess.go` + `lib/srv/term.go` for SSH; PTY output
  is wrapped by `SessionWriter` (`lib/events/session_writer.go`). For Kube/DB,
  the recording is structured request/query events, not raw bytes. For Windows
  Desktop, it's TDP frames.
- **Enhanced recording (BPF)**: `lib/bpf/` produces command/network/disk
  events, keyed to a per-session cgroup. Linux node-side only.
- **Multipart upload**: `lib/events/stream.go::ProtoStream` formats slices and
  drives S3 `CreateMultipartUpload` / `UploadPart` / `CompleteMultipartUpload`,
  with `ResumeAuditStream` re-attaching to interrupted uploads.
  `lib/events/complete.go` `UploadCompleter` finds abandoned uploads after a
  grace period (default 4 h) and finalizes them.
- **Playback**: `lib/events/playback.go::DetectFormat` + `NewProtoReader`,
  used by `tsh play`, `tctl recordings download`, the Web UI player, and
  `lib/player/`.

### Ecosystem & tap points

- **Components**: Auth, Proxy, SSH (Node), Kubernetes, Database, App, Windows
  Desktop, Discovery — all wired in `lib/service/service.go::TeleportProcess`
  (~line 557). All emit events via gRPC to Auth, often through the reverse
  tunnel (`lib/reversetunnel/agent.go`).
- **Configuration resources** (the things you'd `tctl get`):
  `cluster_audit_config` (`api/types/audit.go`),
  `session_recording_config` (`api/types/sessionrecording.go`),
  `external_audit_storage`
  (`api/types/externalauditstorage/externalauditstorage.go`). The last is
  Enterprise-only and is what wires Cloud events/recordings to the customer's
  own S3 + Athena + Glue.
- **Four realistic tap points** for a detection pipeline:
  1. **Event Handler protocol** (mTLS gRPC) — real-time, low-latency, fully
     supported. Best general-purpose answer.
  2. **Web UI HTTP endpoints** (`/webapi/sites/:site/events/search` etc.) —
     polling only, but no extra binary needed.
  3. **Direct S3 read** of External Audit Storage bucket — both Parquet audit
     events and raw `<session-id>.tar` recordings; needs AWS IAM not Teleport
     auth. **The user already has this enabled.**
  4. **Athena SQL** over the Parquet audit table — best for batch / aggregate
     analysis.

## Recommended approach

Create a `notes/` directory at the project root (a sibling of `upstream-repo/`)
containing eight markdown files. Each file is short enough to read end-to-end
in one sitting (~600-1500 words, with file 04 a bit longer given its weight),
opinionated about what matters, and dense with `path:line` references back
into `upstream-repo/` so a future session can re-verify any claim against
source.

### Files to create

| # | Path | Purpose |
|---|------|---------|
| 0 | `notes/README.md` | Index + project goals + reading order + how to validate |
| 1 | `notes/01-audit-log-plumbing.md` | Event schema, emitter, MultiLog, storage backends, search APIs |
| 2 | `notes/02-session-recording-plumbing.md` | Capture per protocol, ProtoStream format, S3 upload, completer, playback |
| 3 | `notes/03-ecosystem-and-grpc-api.md` | Components, reverse tunnel, gRPC RPCs, config resources, RBAC |
| 4 | `notes/04-cloud-and-external-audit-storage.md` | **Weighted file** — Cloud topology + deep dive on EAS bucket layout, Parquet/Glue schema, KMS, OIDC credential exchange. Most relevant file for this user. |
| 5 | `notes/05-tap-points-for-detection.md` | The four tap-point options with auth, latency, fidelity, cost; bridge to step 2 |
| 6 | `notes/06-pipeline-design-stub.md` | **Step-2 handoff stub** — open questions and decision points for designing the detection pipeline. Near-empty by design; populated in the next phase. |
| 7 | `notes/99-open-questions.md` | Things we couldn't answer from source alone — material for a second-opinion review |

### File outlines

Each outline lists the sections to write and the **specific code references**
the section must cite (so future sessions can verify without rerunning the
Explore agents).

**`notes/README.md`** (~300-500 words)
- "What this is" — research notes for understanding Teleport v17 audit + recording
- "Why we wrote it" — feeds the detection-apparatus work
- Reading order (1 → 2 → 3 → 4 → 5 → 99)
- Source pin: `upstream-repo/` at `Release 17.7.20` (commit `2797910`); how to
  re-pin if the user wants a different version
- Conventions: `path:line` references are relative to `upstream-repo/`
- "How to validate these notes" — see `99-open-questions.md` and the
  validation section below

**`notes/01-audit-log-plumbing.md`** (~1500 words)
- 30-second summary
- The `AuditEvent` interface and `Metadata` struct
  - `lib/events/api.go`, `api/proto/teleport/legacy/types/events/events.proto`
  - Code naming convention (T-prefix, severity suffix); pointer to
    `lib/events/codes.go` for the full table
- Emission pattern
  - `lib/events/emitter.go` `Emitter`, `AsyncEmitter`
  - The 1024-event buffer + silent drop gotcha (line ~119)
  - Worked example: SSH `session.start` from `lib/srv/sess.go::emitSessionStartEvent` (~L1027)
  - Worked example: `user.login` from `lib/auth/methods.go::emitAuthAuditEvent` (~L157)
- Auth server fan-out
  - `lib/events/multilog.go` `MultiLog`, `MultiEmitter`
  - Wiring: `lib/service/service.go::initAuthExternalAuditLog` (~L1883)
  - `lib/events/search_limiter.go` (read-side rate limit)
- Storage backends
  - Athena: `lib/events/athena/` — SNS → SQS → Parquet → S3, large-event S3
    fallback, default thresholds
  - DynamoDB / Postgres / Firestore / FileLog — one paragraph each
- Read APIs
  - Old: `SearchEvents`, `SearchSessionEvents`, `StreamSessionEvents`
  - New: `GetEventExportChunks`, `GetUnstructuredEvents`,
    `StreamUnstructuredSessionEvents`
    (`api/proto/teleport/auditlog/v1/auditlog.proto`)
  - RBAC enforcement: `lib/auth/auth_with_roles.go` SearchEvents check (~L6418)
- ASCII data-flow diagram: Source → AsyncEmitter → Auth → MultiLog →
  {Athena, …} → consumer
- Failure modes & gotchas: silent drop on buffer overflow; SearchEvents rate
  limit; Athena ~1-2 min batch lag; events >250 KB go via S3 staging

**`notes/02-session-recording-plumbing.md`** (~1500 words)
- 30-second summary
- What a recording IS (the protobuf event stream)
  - Per-protocol event variants — SSH (`SessionPrint`), Kube (`KubeRequest`),
    DB (`DatabaseSessionQuery`), Desktop (`DesktopRecording` / TDP)
  - On-disk container format: `ProtoStreamV1` magic, slices, gzip, MD5
- Recording modes
  - `off` / `node` / `node-sync` / `proxy` / `proxy-sync`
  - Where dispatched: `lib/events/recorder/recorder.go`
- SSH capture path
  - `lib/srv/sess.go` + `lib/srv/term.go` PTY wrap
  - `lib/events/session_writer.go::SessionWriter.Write` (~L209)
  - Recording-only vs. recording-and-audit events: `SessionPrint` is recording-only
- Enhanced (BPF) recording: `lib/bpf/`, cgroup keying, Linux-only
- Streaming pipeline
  - `lib/events/stream.go` `ProtoStreamer`/`ProtoStream`
  - Slice formation, multipart upload, resume
  - `lib/events/complete.go` `UploadCompleter` and the 4 h grace
- Backends
  - `lib/events/s3sessions/{s3handler,s3stream}.go` — bucket, KMS, ACL,
    object key `<path>/<session-id>.tar` at L517
  - `filesessions` (async disk staging)
  - GCS / Azure (mention only)
- Session ID lifecycle
  - `session.NewID()` in `lib/session/session.go`
  - SessionID in `SessionStart` audit event ↔ S3 key (1:1) — this is the bridge
    a detection system uses to correlate audit log → recording bytes
- Playback: `lib/events/playback.go`, `lib/player/`
- Failure modes: node crash mid-session (completer recovers async; sync may
  drop tail), S3 outage backoff with `lostEvents` counter, no end-to-end
  cryptographic integrity (relies on TLS + S3 ETags)
- ASCII diagram: PTY → SessionWriter → ProtoStream → S3 multipart;
  parallel async path through disk + uploader

**`notes/03-ecosystem-and-grpc-api.md`** (~900 words)
- Component topology (Auth, Proxy, Node, Kube, DB, App, Desktop, Discovery)
  - All wire-up in `lib/service/service.go::TeleportProcess` (~L557)
- Reverse tunnel: `lib/reversetunnel/agent.go`
- The audit-stream gRPC dance:
  - `CreateAuditStream`, `ResumeAuditStream`, `CompleteStream`,
    `FlushAndCloseStream` from
    `api/proto/teleport/legacy/client/proto/authservice.proto` (~L699)
- Configuration resources, with the meaningful fields:
  - `cluster_audit_config` — `api/types/audit.go`
  - `session_recording_config` — `api/types/sessionrecording.go`
  - `external_audit_storage` —
    `api/types/externalauditstorage/externalauditstorage.go`
- RBAC for an external consumer:
  - The role template at
    `integrations/event-handler/tpl/teleport-event-handler-role.yaml.tpl`
  - `event:list,read` and `session:list,read`
- Inspection commands:
  - `tctl recordings ls/download` —
    `tool/tctl/common/recordings_command.go` (~L46)
  - `tsh play` and `tsh recordings` — `tool/tsh/common/play.go`
  - Web UI endpoints — `lib/web/apiserver.go` (~L912, ~L4263)

**`notes/04-cloud-and-external-audit-storage.md`** (~1500-2000 words — the
weighted file). The user already has EAS enabled and detection is most likely
to tap their customer S3 bucket / Athena, so this file goes deeper than the
others.
- Cloud topology: what Gravitational manages (auth server,
  base SNS/SQS/S3, Athena workgroup) vs. what the customer can see (events
  via Web UI / Event Handler, S3 bucket if EAS is enabled)
- `external_audit_storage` resource lifecycle:
  - Draft generated by Cloud
    (`api/types/externalauditstorage/externalauditstorage.go::GenerateDraftExternalAuditStorage`,
    or equivalent — confirm exact symbol when writing the note)
  - Customer applies CloudFormation/Terraform for the S3 + IAM + OIDC
  - `Promote` (draft → cluster) action
  - `Disable` and what happens to in-flight uploads
- OIDC credential exchange:
  - The integration resource type, role assumption flow, and how it
    replaces static AWS creds. Cite the integration code path under
    `lib/integrations/awsoidc/` (verify exact subpath while writing).
- **Bucket layout** (concrete, with examples):
  - Session recordings at `<SessionRecordingsURI>/<session-id>.tar`
    where `<SessionRecordingsURI>` is the EAS spec field
  - Audit events as Parquet at `<AuditEventsLongTermURI>` — drill into the
    Athena consumer code (`lib/events/athena/consumer.go` or wherever the
    Parquet writer lives) to document the directory structure (Hive-style
    partitions like `year=YYYY/month=MM/day=DD/`?), object naming, and
    rollover triggers
  - Athena query results staging at `<AthenaResultsURI>`
- **Glue catalog & Parquet schema**:
  - The Glue database/table referenced by `GlueDatabase` / `GlueTable`
  - The Parquet column schema — derive from
    `lib/events/athena/` (whichever file defines the Arrow/Parquet schema)
    and call out the columns most useful for detection (event_type, code,
    user, session_id, server, time, raw event JSON blob)
  - Partitioning scheme as enforced by the consumer
- KMS / encryption posture:
  - Default SSE for the bucket; whether the EAS spec lets the customer pin a
    CMK; what's documented in `lib/events/s3sessions/s3stream.go` SSE config
  - Same for Athena query results bucket
- **Concrete commands the user can run today** to confirm the live shape:
  - `tctl get external_audit_storage`
  - `tctl get cluster_audit_config`
  - `tctl get session_recording_config`
  - `aws s3 ls <SessionRecordingsURI>` (sanity-check object key shape)
  - `aws glue get-table --database-name <db> --name <table>` (schema)
  - `aws athena start-query-execution` against a trivial `SELECT
    event_type, COUNT(*) FROM <db>.<table> WHERE ... LIMIT 10`
- **What ends up where, exhaustively**:
  - In the customer bucket: audit events (Parquet) + session recordings
    (`<sid>.tar`)
  - NOT in the customer bucket: anything Gravitational deems
    internal-only (note any examples surfaced by `lib/integrations/externalauditstorage`)
- Caveats: Cloud default retention; behavior during EAS draft state; what
  happens if the OIDC integration is broken (do recordings buffer? drop?);
  any per-region constraints
- Implications for detection (preview of `05-tap-points`): "you already have
  the bucket, so direct S3 + Athena are first-class options"

**`notes/05-tap-points-for-detection.md`** (~900 words)
- Frame: "your agent needs to follow audit events and session recordings —
  here are the four ways to tap in"
- Per option, a uniform table: protocol, auth, latency, completeness, cost,
  operational complexity, recommended use
  1. Event Handler / `auditlog.v1` gRPC
  2. Web UI HTTP endpoints
  3. Direct S3 read (Parquet audit events + `<sid>.tar` recordings) — the
     user's EAS bucket
  4. Athena SQL over the Parquet audit table
- Recommendation matrix:
  - Real-time anomaly streaming → Event Handler
  - Bulk session classification (offline) → S3 read of `.tar` + parse with
    `lib/events/playback.go`-equivalent client
  - Aggregate "who logged in from where this week" → Athena SQL
  - One-off investigation → Web UI / `tsh play`
- Honest "what about reading from your existing SIEM?" sub-section: this is
  often the easiest tap (you already pay to put events there) but loses
  recording bytes
- Explicit non-goal: actually building the pipeline (that's step 2)

**`notes/06-pipeline-design-stub.md`** (~200 words — intentionally near-empty)
- One-line statement that this is a placeholder for step-2 work
- Frame: "given the four tap points in `05`, what do we still need to know
  before designing the actual pipeline?"
- Bullet list of decision points that will be answered in the next phase, not
  this one:
  - Real-time vs. batch (or hybrid)
  - Whether to consume from the user's existing SIEM or tap upstream
  - Recording-content analysis (parse `.tar` recordings) vs. metadata-only
  - Where the agent runs (in-VPC near the EAS bucket vs. elsewhere)
  - Detection storage / state model
  - How to backfill historical data
- Pointer back to `05-tap-points.md` and `99-open-questions.md`
- Explicit "do not populate this file in step 1"

**`notes/99-open-questions.md`** (~400 words)
- Things the Explore agents could not answer from source alone — these are
  the items worth a second-opinion review or live verification against the
  user's actual cluster. Carry forward at minimum:
  - Cloud retention period defaults (need to check `tctl get
    cluster_audit_config` on the live cluster)
  - Exact Cloud Athena partition scheme + table schema (verify by inspecting
    a sample Parquet file or running `tctl get external_audit_storage`)
  - Whether session recordings in the customer S3 bucket are encrypted at rest
    by default and which KMS key (read S3 object metadata)
  - Event ordering guarantees across sessions vs. within a session
  - Event Handler delivery semantics on crash mid-Fluentd-write
  - Whether `GetEventExportChunks` is the API used in v17 against Athena, or
    the older `SearchEvents` path, and how that affects polling cost
- Each item phrased as a question, with how to verify

### Validation approach

After the notes are written, sanity-check them by:

1. **Source spot-checks** — pick 5 random `path:line` claims and `Read` them in
   `upstream-repo/` to confirm. Update the notes if any drift.
2. **Live cluster spot-checks** the user can run themselves:
   - `tctl get external_audit_storage` — confirms EAS shape and bucket URI
   - `tctl get cluster_audit_config` — confirms Cloud's actual storage config
   - `tctl get session_recording_config` — confirms recording mode in effect
   - `aws s3 ls <SessionRecordingsURI>` — confirms object key shape
3. **Second opinion** — feed `notes/01..05` to a fresh Claude Code session
   (or another Explore agent) and ask "find anything wrong or oversimplified
   in these notes against the source at `upstream-repo/`." Carry corrections
   into the notes.

### Out of scope for step 1

- No code changes to anything (we are reading `upstream-repo/`, not modifying it)
- No connection to the live `<your-tenant>.teleport.sh` cluster
- No detection logic, classifier, or pipeline implementation
- No decision yet on which tap point to use — `05-tap-points` lays out
  the options; the choice is step 2

### What comes after this plan

When step 1's notes exist and have been spot-checked, step 2 takes
`notes/06-pipeline-design-stub.md` from a placeholder to a real design,
picking one or two of the four tap points (a hybrid of Event Handler for
real-time signals + direct S3 + Athena for the user's EAS bucket is the
likely shape) and laying out the actual data path into the user's detection
apparatus. The same `notes/` directory grows further files and the same
spot-check + second-opinion validation pattern repeats.

## Verification (for step 1's deliverable)

The plan is "done" when:

- [x] All 8 files exist under `notes/`
- [x] `notes/README.md` correctly indexes the others
- [x] Every `path:line` reference in the notes resolves in `upstream-repo/`
      (spot-check 5)
- [x] `notes/04` includes a concrete documented Parquet/Glue schema and bucket
      layout, not just a hand-wave
- [x] `notes/06-pipeline-design-stub.md` is intentionally short and clearly
      labeled as a placeholder for step 2
- [x] `notes/99-open-questions.md` lists at least the 6 unanswered items above
      with a verification recipe per item
- [x] A fresh reader can answer "where in S3 is session `<sid>` stored?" by
      reading only `notes/02` and `notes/04`
- [x] A fresh reader can answer "how would I subscribe to audit events from a
      sidecar?" by reading only `notes/05` (with cross-refs to `01` and `03`)

---

## Corrections during execution

The "What we already learned" synthesis above came from three Explore agents
running in parallel **before** anyone read source line-by-line. The execution
phase that turned the plan into `notes/01..05` opened the cited files and
verified the load-bearing claims. A handful turned out to be wrong, and the
notes carry the corrected versions. The plan above is left intact for
historical accuracy; the deltas are catalogued here so a future reader doesn't
mistakenly trust the old text.

| Where in the plan | Plan said | Source actually said | Where it's now correct |
|---|---|---|---|
| Audit log → bullet 1 | `T3000I = session.start` | `T2000I = SessionStartCode` (`lib/events/codes.go:108`); `T3xxx` is access-request / role events | `notes/01-audit-log-plumbing.md` "Anchor codes" table |
| Audit log → bullet 4 | "events >250 KB are staged in S3" | `lib/events/athena/athena.go:76-78` comment says "temporary large events (>256KB)" | `notes/01` and `notes/04` use ~256 KB |
| Session recording → "Multipart upload" bullet | "grace period (default 4 h)" | `UploadCompleterGracePeriod = 24 * time.Hour` (`lib/events/auditlog.go:110-112`) | `notes/02-session-recording-plumbing.md` "The UploadCompleter" section |
| Audit log → "Auth server pipeline" | implies `MultiLog` fan-out is the common Cloud shape | `lib/service/service.go:1893-1896` short-circuits when EAS is on — only the first Athena URI is honoured, no `MultiLog` is constructed in Cloud | `notes/01` "Where the auth server picks its backend" and `notes/04` topology section |
| Audit log → "Storage backends" | enumerates Athena/Dynamo/PG/Firestore/file/Azure as the matrix | accurate, but missed that the Athena consumer writes one Parquet file per `<YYYY-MM-DD>/<uuidv7>.parquet` partition with SHA256 checksums (for object lock) | `notes/04` "Bucket layout, in detail" |
| Session recording → "Container format" | "8-byte magic header, slices of ≥5 MiB, gzip-compressed records" | accurate but undersold — header is 24 bytes (3×8: version, meaningful size, padding size), individual records are length-prefixed (4-byte uint32), max record size 64 KiB pre-compression | `notes/02` "On-disk / on-the-wire container format: ProtoStreamV1" |
| Notes file 04 outline | "verify exact symbol while writing" for OIDC code path under `lib/integrations/awsoidc/` | confirmed: credentials cache at `lib/integrations/awsoidc/credprovider/credentialscache.go`; the OIDC token TTL is 1h, refresh 15m before expiry, ticker 30s, retrieve timeout 30s (`lib/integrations/externalauditstorage/configurator.go:42-49`) | `notes/04` "OIDC credential exchange" |
| Notes file 04 outline | "Glue catalog & Parquet schema — derive from lib/events/athena/" | confirmed: schema at `lib/events/athena/types.go:31-38` is exactly six columns: `event_type`, `event_time` (timestamp millisecond), `uid`, `session_id`, `user`, `event_data` (JSON blob). Anything else needs `json_extract` from `event_data`. | `notes/04` "What the Athena consumer actually writes" |

The lesson, as it applies to step 2's plan: don't trust agent-synthesised
"what we already learned" bullets without grounding them in source. They're
useful as a starting hypothesis and as a way to size the work, but the
source-spot-check step caught five real errors — none catastrophic, but each
would have misled a downstream design decision. The same discipline applies
to every phase that follows.
