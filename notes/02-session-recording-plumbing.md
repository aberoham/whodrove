# 02 — Session Recording Plumbing

## 30-second summary

A **session recording** is a binary stream of the *same* protobuf
`apievents.AuditEvent` types as the audit log, but containing
recording-specific events (`SessionPrint` for SSH PTY bytes, `DesktopRecording`
for TDP frames, etc.) that are never written to the audit log. The stream is
serialised in Teleport's home-grown `ProtoStreamV1` format — gzipped slices of
≥5 MiB, prefixed with a tiny header — and uploaded to a session backend (S3
for Cloud + EAS) as a single multi-part object whose key is
`<prefix>/<session-id>.tar`. There are five recording **modes** (`off`,
`node`, `node-sync`, `proxy`, `proxy-sync`) which choose between live-streaming
to the backend versus disk-staging on the node followed by an asynchronous
uploader. An `UploadCompleter` running on the auth server picks up multipart
uploads abandoned for ≥ 24 hours and finalises them.

The single most important fact for downstream consumers: the **`session_id`
in the audit log row equals the basename of the S3 object**. That's the join
key that lets a detection pipeline correlate the `session.start` /
`session.end` / `session.upload` events from Athena with the actual
keystrokes-and-output blob in S3.

## What a recording actually contains

The stream is a sequence of protobuf events from the union defined in
`api/proto/teleport/legacy/types/events/events.proto`. The exact set depends
on the protocol:

### SSH (the canonical case)

| Event | Audit log? | Recording? | Notes |
|-------|-----------|-----------|-------|
| `SessionStart` | yes | yes | metadata, server, command, recording mode |
| `SessionPrint` | **no** | yes | raw PTY input/output bytes |
| `Resize` | no | yes | terminal-size changes for playback |
| `SessionJoin` / `SessionLeave` | yes | yes | participant tracking (moderated sessions) |
| `SessionEnd` | yes | yes | participants, exit code, duration |
| `SessionData` | yes | no | aggregate stats: bytes in/out |
| `SessionUpload` | yes | no | emitted when the recording lands in S3 |
| BPF `SessionCommand` / `SessionNetwork` / `SessionDisk` | yes, when enabled | no | enhanced audit events, see below |

`SessionPrint` is where the actual content of the session lives. The bytes are
copied verbatim from the PTY, so a "recording" of an SSH session is in effect
a faithful TTY recording with timing.

### Kubernetes (`lib/kube/proxy/`)

No raw bytes. Each Kube API request is recorded as a `KubeRequest` event with
method, path, request body summary, response code, response body summary.
Streamed through the same `SessionWriter` interface as SSH. So a "Kube
session recording" is really a structured request log, not a screencast.

### Database (`lib/srv/db/`)

`DatabaseSessionStart`, `DatabaseSessionQuery` (the SQL/Mongo/whatever query
text), `DatabaseSessionEnd`. Recorder is wired in
`lib/srv/db/streamer.go::newSessionRecorder`. Again, structured events only;
no raw protocol bytes.

### Application access (`lib/srv/app/`)

HTTP requests recorded as `AppSessionRequest` events. Note that
`LoggingEmitter` explicitly skips `AppSessionRequestEvent` from console
logging because it's so verbose (`lib/events/emitter.go:271-274`).

### Windows Desktop (`lib/srv/desktop/`)

`WindowsDesktopSessionStart` plus a stream of `DesktopRecording` events whose
`Message` is a raw TDP (Teleport Desktop Protocol) frame — PNG screen tiles,
mouse, keyboard, clipboard. So a desktop recording is much closer to the SSH
case in spirit (replayable visual content) but the wire format inside the
events is TDP, not ANSI-coloured bytes.

## On-disk / on-the-wire container format: ProtoStreamV1

`lib/events/stream.go:44-79` lays out the constants:

```go
// v17.7.20
const (
    Int32Size = 4
    Int64Size = 8

    ConcurrentUploadsPerStream = 1
    MaxProtoMessageSizeBytes   = 64 * 1024
    MinUploadPartSizeBytes     = 1024 * 1024 * 5   // 5 MiB

    ProtoStreamV1                 = 1
    ProtoStreamV1PartHeaderSize   = Int64Size * 3  // 24 bytes
    ProtoStreamV1RecordHeaderSize = Int32Size      // 4 bytes
)
```

Layout per "slice" (which is also one S3 multipart upload part):
- 8-byte uint64 big-endian: `ProtoStreamV1` magic = `1`
- 8-byte uint64: meaningful size of the slice in bytes
- 8-byte uint64: padding size at the slice tail (slices are padded so each
  multipart part ≥ 5 MiB, the S3 minimum)
- repeated records, each:
  - 4-byte uint32: protobuf-marshalled record length
  - protobuf-marshalled `apievents.AuditEvent` (max 64 KiB pre-compression)
- gzipped (so the on-wire bytes are smaller than the meaningful size)
- padding bytes to round out to the multipart minimum

Concurrency: `ConcurrentUploadsPerStream = 1`. So even on a chatty session,
parts are uploaded one at a time per stream — the multipart parallelism is
across *sessions*, not across parts of a single session.

## Recording modes

`api/types/constants.go:1276-1296`:

```go
// v17.7.20
RecordAtNode      = "node"        // default
RecordAtProxy     = "proxy"
RecordOff         = "off"
RecordAtNodeSync  = "node-sync"
RecordAtProxySync = "proxy-sync"
var SessionRecordingModes = []string{RecordAtNode, RecordAtProxy, RecordOff,
                                     RecordAtNodeSync, RecordAtProxySync}
```

The async/sync split is dispatched in `lib/events/recorder/recorder.go`. The
distinction:

- **Sync** (`node-sync`, `proxy-sync`) — `SessionWriter` wraps a
  `ProtoStreamer` whose `MultipartUploader` is the auth server's gRPC. Events
  are buffered briefly, then live-streamed. If the network is slow the writer
  enters a backoff window (see `session_writer.go:236-268`); during backoff,
  events are dropped and the `lostEvents` atomic counter increments. There is
  no replay.
- **Async** (`node`, `proxy`) — `SessionWriter` wraps a
  `filesessions.FileStreamer` that writes slices to local disk under
  `/var/lib/teleport/logs/upload/<namespace>/<upload-id>/`. A separate
  `filesessions.Uploader` background job scans that tree and uploads to the
  real backend (S3 / GCS / etc.) on a polling loop. Surviving a node crash
  mid-session is the main point — the next restart picks up where the
  previous run left off.
- **`proxy` modes** are how you get recordings of legacy OpenSSH nodes that
  Teleport doesn't run on; the Teleport proxy MITMs the SSH stream and
  records there. **`node` modes** are the default and let the node record
  itself.
- **`off`** disables recording but does NOT disable session-related audit
  events (`SessionStart`, `SessionEnd` etc. are still emitted).

In a Cloud + EAS tenant, the typical setting is `node-sync` so the customer
S3 bucket gets the recording live. To confirm yours: `tctl get session_recording_config`.

## SSH capture path in detail

`lib/srv/sess.go` is the registry/lifecycle. `lib/srv/term.go` is the PTY
wrapper. The pipeline:

```
shell process (bash, etc.)
  ↕
PTY (creack/pty)
  ↕
io.Writer wrapping SessionWriter           lib/events/session_writer.go:210
   │  - SessionWriter.Write(data) called for every chunk of PTY output
   │  - data is copied (L211-216) because Write is async
   │  - cfg.MakeEvents(dataCopy) → []AuditEvent
   │    default: bytesToSessionPrintEvents (L133-156) — splits on
   │    MaxProtoMessageSizeBytes = 64 KiB so giant outputs become several
   │    SessionPrint events
   │  - cfg.Preparer.PrepareSessionEvent(event) — sets index, time, sigs
   │  - RecordEvent(ctx, event) — pushes to eventsCh
   │
   ▼
processEvents goroutine (started in NewSessionWriter, L60-63)
   │  - drains eventsCh, batches, hands to underlying Stream
   │
   ▼
apievents.Stream (ProtoStream for sync modes, FileStream for async)
   │
   ▼
ProtoStream.RecordEvent(...)               lib/events/stream.go ~L460
   │  - serialise to protobuf, append to gzip buffer
   │  - when buffer reaches MinUploadBytes, flush as a slice:
   │      - allocate from sync.Pool
   │      - prepend 24-byte ProtoStreamV1 header
   │      - upload as next part of the in-progress S3 multipart
   │  - InactivityFlushPeriod (5 min default) forces a flush even if
   │    the buffer is small, so a quiet session still flushes
   │
   ▼
S3 multipart upload
```

Backoff: `SessionWriter` keeps `backoffUntil`, `lostEvents`,
`acceptedEvents`, `slowWrites` atomics
(`session_writer.go:172-176`). When the underlying stream can't keep up, the
writer enters a backoff window and increments `lostEvents`. There is no
disk-spillover for sync modes; the only path to durability is async.

### Moderated sessions / multiple participants

A session with two SSH clients attached (the "moderator" and the "moderated")
is still one `SessionWriter` and one S3 object. Participant identities are
tracked via `SessionJoin` / `SessionLeave` events written to the same stream.

## Enhanced recording (BPF)

`lib/bpf/`. Linux node-side only (build tag `bpf && !386`). When
`enhanced_recording` is enabled on a role, the SSH service:

1. Creates a per-session cgroup keyed by session ID and adds the shell PID to
   it.
2. Runs three BPF programs (`command.go`, `network.go`, `disk.go`) that
   instrument `execve`, `connect`, and `openat` syscalls respectively.
3. Filters BPF events by cgroup ID, mapping back to session ID.
4. Emits each as a `SessionCommand` (`T4000I`), `SessionNetwork` (`T4002I`),
   or `SessionDisk` (`T4001I`) event through the session's audit emitter.

The naming is easy to misread: in v17 these BPF events are "enhanced recording"
signals, but they are emitted through `ctx.Emitter.EmitAuditEvent` in
`lib/bpf/bpf.go:424,483,538`. The BPF session context is wired with
`s.emitter`, not `s.Recorder()` (`lib/srv/sess.go:1391-1401` and
`lib/srv/sess.go:1564-1574`). So they flow to the **audit log** with
category-4xxx codes; they are not written into the ProtoStreamV1 session
recording alongside `SessionPrint` events.

Worth knowing: BPF events are *the only* way to know what a session actually
*did* without scraping the PTY bytes. For a classifier, having (or not having)
enhanced recording makes a huge difference — `SessionCommand.Path +
.Argv` plus `SessionNetwork.DstAddr/Port` are clean, structured signals
available from the audit event stream / Parquet table; ANSI-coloured PTY
bytes from the recording are not.

## The streaming pipeline (ProtoStream end-to-end)

`lib/events/stream.go:109-152`:

```go
// v17.7.20
func NewProtoStreamer(cfg ProtoStreamerConfig) (*ProtoStreamer, error) { ... }

func (s *ProtoStreamer) CreateAuditStream(ctx context.Context, sid session.ID) (apievents.Stream, error) {
    upload, err := s.cfg.Uploader.CreateUpload(ctx, sid)   // S3 CreateMultipartUpload
    if err != nil { return nil, trace.Wrap(err) }
    return s.CreateAuditStreamForUpload(ctx, sid, *upload)
}

func (s *ProtoStreamer) ResumeAuditStream(ctx context.Context, sid session.ID, uploadID string) (apievents.Stream, error) {
    upload := StreamUpload{SessionID: sid, ID: uploadID}
    parts, err := s.cfg.Uploader.ListParts(ctx, upload)    // S3 ListParts
    if err != nil { return nil, trace.Wrap(err) }
    return NewProtoStream(ProtoStreamConfig{ ..., CompletedParts: parts })
}
```

So creating a stream means initiating an S3 `CreateMultipartUpload`; resuming
means listing the already-uploaded parts and continuing from the highest part
number + 1. Resume is what makes async recordings survive an `Uploader`
restart.

`InactivityFlushPeriod` (default 5 min, `lib/events/auditlog.go:104`) prevents
half-full slices from sitting around forever. The flush ticker is in
`stream.go:563-585`.

## The UploadCompleter (orphaned-upload janitor)

`lib/events/complete.go:138-212`. Runs as a background goroutine on the auth
server. Every `CheckPeriod` (default `AbandonedUploadPollingRate =
SessionTrackerTTL/6 ≈ 5 minutes`) it:

1. Optionally acquires a cluster-wide semaphore (`upload-completer`,
   `MaxLeases=1`) so multi-replica auth servers don't race to complete the
   same upload (`L184-205`).
2. Calls `Uploader.ListUploads()` to enumerate active multiparts.
3. For each, if the upload's `Initiated` time is within `GracePeriod`
   (`UploadCompleterGracePeriod = 24 * time.Hour`,
   `lib/events/auditlog.go:110-112`), skip — and because uploads come back
   sorted oldest-first (`complete.go:233-239`), break out of the loop entirely
   the moment we see one in-grace.
4. For each older upload, check the session tracker resource. If the tracker
   is gone (session ended), call `CompleteUpload()` to finalise the multipart
   and emit a `SessionUpload` (`T2005I`) audit event.

Two consequences:

- **A finished session may take up to 24 hours after end before it shows up
  in S3**, if the node crashed mid-upload before the recording was
  complete. The 24h is the conservative default; in practice in sync mode the
  recording is finalised at session end.
- **The `SessionUpload` event is the bridge.** Detection pipelines that want
  to act on "all completed sessions" should listen for `T2005I`, not for
  `T2004I` (`SessionEnd`), because `SessionEnd` fires before the bytes are
  necessarily in S3 in async mode. The Event Handler does exactly this
  (`integrations/event-handler/teleport_event.go:30-31, 87-89`).

## Storage backends for recordings

These are *separate* code paths from the audit-log backends. A Teleport
deployment chooses one per resource (`audit_sessions_uri` in
`cluster_audit_config`).

### S3 (`lib/events/s3sessions/`) — Cloud + EAS

- `s3handler.go:517-522`: object key construction.

  ```go
  // v17.7.20
  func (h *Handler) path(sessionID session.ID) string {
      if h.Path == "" {
          return string(sessionID) + ".tar"
      }
      return strings.TrimPrefix(path.Join(h.Path, string(sessionID)+".tar"), "/")
  }
  ```

  So the object is `<prefix>/<session-id>.tar`. The `.tar` suffix is
  legacy — the actual content is `ProtoStreamV1`, not a tarball. Don't try to
  `tar -xf` it.
- `s3stream.go:45-77`: `CreateMultipartUpload` with optional
  `ServerSideEncryptionAwsKms` and a customer-supplied `SSEKMSKey`.
- `s3stream.go:79-135`: `UploadPart` includes an MD5 over the part for S3
  integrity. Returns ETag tracked on the `StreamPart`.
- `s3handler.go:529-547`: `ensureBucket` is a HeadBucket with a short timeout.
  The `else` branch logs explicitly that **failure is expected when EAS is
  enabled** — Teleport may have write-only IAM perms on the customer bucket.
  Useful debugging hook: if you see this log, the bucket exists and Teleport
  is correctly behaving as if it does, even though it can't `Head` it.
- Versioned bucket: `Download()` fetches the **oldest** version (so an
  attempted in-place tampering would be obvious — earlier version still
  served). Bucket should have versioning on for this to be meaningful.

### GCS (`lib/events/gcssessions/`)

Same multipart upload pattern adapted to GCS APIs.

### Azure (`lib/events/azsessions/`)

Block blob upload to Azure storage.

### File (`lib/events/filesessions/`)

Two roles in this package:
- `filesessions.FileStreamer` — used as the *Streamer* for async modes; writes
  slices into `/var/lib/teleport/logs/upload/<namespace>/<upload-id>/<part>`.
- `filesessions.Uploader` (a separate worker) — scans that tree and uploads
  completed sessions to the real backend.

Reservation pattern: parts are written under `.reserved` then renamed
atomically to the final part name.

### Memory (`lib/events/memsessions/`)

Tests only.

## Session ID lifecycle

`lib/session/session.go` defines `session.ID` (UUIDv4 by default,
`session.NewID()` mints one). The same ID:

1. Is set on the `SessionStart` event's `SessionMetadata.SessionID` field.
2. Threads through every subsequent event in that session (`SessionPrint`,
   `Resize`, `SessionJoin`, etc., `SessionEnd`).
3. Becomes the basename of the S3 object: `<prefix>/<session-id>.tar`.
4. Is referenced by the `SessionUpload` audit event (which carries it
   explicitly so that the event handler can map it back to the bucket key —
   see `teleport_event.go::setSessionID` at L114-120).

For a detection pipeline: a single key is enough to correlate
"audit log row" → "S3 recording bytes" → "playback metadata".

## Playback

`lib/events/playback.go:35-71`:

```go
// v17.7.20 (paraphrased)
func DetectFormat(reader io.ReadSeeker) (Format, error) {
    // peek 8 bytes; if uint64 == ProtoStreamV1 (=1), it's the proto format,
    // else assume legacy gzipped-tar
}
```

`lib/events/playback.go:74-120`: `Export()` decodes a recording to
newline-delimited JSON, one event per line. This is the simplest way to
ingest a recording's bytes outside of Teleport — read the object from S3,
hand it to the proto reader, get a stream of decoded `apievents.AuditEvent`s.

`lib/player/player.go` is the higher-level player used by the Web UI and
`tsh play`; it wraps the proto reader with a state machine for play / pause /
seek / variable speed.

`tctl recordings download` (added in v17.7,
`tool/tctl/common/recordings_command.go:46-131`) lets an admin pull a
recording to local disk *without* needing direct S3 access — it uses
`StreamSessionEvents` over gRPC, then re-serialises into a tar file. Useful
when the customer S3 bucket has tight network restrictions.

## Failure modes and gotchas

1. **Sync mode + S3 outage = data loss.** Backoff window in
   `session_writer.go:236-268` gives the underlying stream a chance to recover,
   but events emitted during backoff are dropped and `lostEvents` counter
   increments. No on-disk spillover.
2. **Async mode + node crash → up to 24 h to recovery.** The
   `UploadCompleter` grace period is 24 hours by default
   (`auditlog.go:110-112`). Until then, the multipart upload sits with
   in-flight parts that S3 charges you for. After the grace, it gets
   completed and emits `SessionUpload`.
3. **`SessionEnd` is not the same as "recording is in S3".** Use
   `SessionUpload` (`T2005I`) for "the bytes are durable" semantics. This
   matters for any classifier that pulls the recording — if you start
   downloading right after `SessionEnd` you'll get a 404.
4. **MD5/ETag is the only integrity signal.** No HMAC, no signing. You're
   trusting AWS S3 + TLS to keep the bytes intact. If the bucket is
   versioned, the `Download()` always-take-oldest behaviour gives one weak
   layer of tamper-evidence.
5. **Encryption is bucket-side, not Teleport-side.** Recordings inherit the
   bucket's SSE setting. If you want CMK-only access, set the bucket policy to
   require KMS and verify Teleport's IAM principal has `kms:Encrypt`,
   `kms:Decrypt`, `kms:GenerateDataKey`, and `kms:DescribeKey`.
6. **Sliced upload sizes can be huge.** Default `MinUploadPartSizeBytes` is
   5 MiB, but with `MaxProtoMessageSizeBytes = 64 KiB`, a slice can hold
   several thousand events. A long quiet shell session followed by a `cat
   bigfile` can cause sudden slice flushes; the timing is **not** smooth.
7. **`.tar` suffix is misleading.** The bytes are `ProtoStreamV1` (or legacy
   tar for very old sessions). Use `playback.DetectFormat`.
8. **Resumable, but only at the slice boundary.** A part that's
   half-uploaded when S3 errors will be retried; events within an in-flight
   slice that haven't been flushed yet are at the mercy of the writer's
   backoff state.

## Cross-references

- `01-audit-log-plumbing.md` — the audit-log pipeline that runs in parallel.
- `04-cloud-and-external-audit-storage.md` — the actual S3 bucket layout in
  Cloud + EAS, KMS, IAM permissions, the `SessionRecordingsURI` field in the
  EAS spec.
- `03-ecosystem-and-grpc-api.md` — the gRPC RPCs (`CreateAuditStream`,
  `ResumeAuditStream`, `StreamSessionEvents`) the streamer uses.
- `05-tap-points-for-detection.md` — option (c), reading recordings directly
  from S3 with `playback.DetectFormat` + a proto reader.
