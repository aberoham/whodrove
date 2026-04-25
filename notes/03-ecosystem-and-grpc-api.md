# 03 — Ecosystem and gRPC API

## 30-second summary

Teleport's audit and recording subsystems don't exist in isolation; they
plug into a fixed cast of services (Auth, Proxy, Node, Kube, DB, App,
Desktop, Discovery), a small handful of gRPC RPCs that all of these talk
over, three configuration resources that govern behaviour
(`cluster_audit_config`, `session_recording_config`, `external_audit_storage`),
and a tiny RBAC surface (`event:list,read`, `session:list,read`) that any
external consumer has to wear. This file is the ecosystem map: what each
component does, what wire it talks on, what config it reads, what perms it
needs, and what commands a human would use to look at the system.

## Component topology

`lib/service/service.go::TeleportProcess` (~L557) is the giant struct that
constructs every component a Teleport binary might run. Each component
announces readiness via a service event (`AuthIdentityEvent`,
`ProxyIdentityEvent`, `SSHIdentityEvent`, `KubeIdentityEvent`,
`DatabasesIdentityEvent`, `AppsIdentityEvent`,
`WindowsDesktopIdentityEvent`, `DiscoveryIdentityEvent`).

Mapping component → role in the audit/recording flow:

| Component | Audit events emitted | Session recordings produced |
|-----------|----------------------|------------------------------|
| Auth Server | `user.login`, `user.create`, role/cert/recovery/MFA admin events, `session.upload` | None — but it stores everything |
| Proxy | `session.start`/`session.end` for `proxy`/`proxy-sync` recording modes; reverse-tunnel join events | SSH recordings when in proxy mode |
| Node (SSH) | `session.start`, `session.end`, `session.join`, `session.leave`, `session.command/network/disk` (BPF), `port.local` etc. | SSH recordings when in node mode (default); BPF events are audit events, not ProtoStream recording frames |
| Kubernetes Service | `kubernetes.request` per API call | Kube "recordings" = stream of `KubeRequest` events |
| Database Service | `db.session.start`, `db.session.query`, `db.session.end` | DB "recordings" = stream of `DatabaseSessionQuery` events |
| App Service | `app.session.start`, `app.session.request`, `app.session.end` | Recordings as event streams |
| Windows Desktop | `windows.desktop.session.start/end`, `desktop.recording` | TDP-frame recordings |
| Discovery | `discovery.start`, `discovery.success/failure` | None |

All emit through the same path described in `01-audit-log-plumbing.md`: a
caller-side `apievents.Emitter` (usually an `AsyncEmitter`) speaking gRPC
back to the auth server.

## How nodes/proxies reach the auth server

Two link types:

1. **Direct dial** — auth-internal callers (the auth server emitting its own
   events) and components co-located with auth call the in-process emitter.
2. **Reverse tunnel** — nodes behind NAT (the common case) maintain a
   persistent SSH tunnel to a Proxy via `lib/reversetunnel/agent.go`
   (~L1-95). gRPC channels are multiplexed over that tunnel back to the
   auth server.

The auth server exposes its services on its TLS gRPC listener and on the
mux'd reverse-tunnel listener. RPCs are the same in both directions.

## The audit / recording RPCs

Definitions are split across two protos:

**Legacy** (`api/proto/teleport/legacy/client/proto/authservice.proto`):

| Line | RPC | Purpose |
|------|-----|---------|
| 699 | `AuditStreamRequest` | The bidi stream message envelope (see lifecycle below) |
| 3306 | `EmitAuditEvent(events.OneOf) → google.protobuf.Empty` | Fire one audit event |
| 3308 | `CreateAuditStream(stream AuditStreamRequest) → stream events.StreamStatus` | Open or resume a session-recording stream |
| 3702 | `StreamSessionEvents(StreamSessionEventsRequest) → stream events.OneOf` | Stream a recorded session's events for playback |

The audit-stream lifecycle (sent on the bidi stream):
- Open: `AuditStreamRequest_CreateStream{SessionID}`
- (or) Resume: `AuditStreamRequest_ResumeStream{SessionID, UploadID}`
- Send events: `AuditStreamRequest_Event{Event: <OneOf AuditEvent>}` repeatedly
- Flush without finalising: `FlushAndCloseStream{}`
- Finalise: `CompleteStream{}`

Server side acks each batch by emitting `events.StreamStatus{LastEventIndex,
UploadID}` on the response stream — that's what lets the client know
checkpoints and resume IDs.

**v17 unstructured** (`api/proto/teleport/auditlog/v1/auditlog.proto`):

Four RPCs on `AuditLogService`. See `01-audit-log-plumbing.md` for a full
explainer; one-line summaries:
- `GetUnstructuredEvents` — paginated list, JSON-shaped events.
- `StreamUnstructuredSessionEvents` — playback in JSON shape.
- `GetEventExportChunks` — opaque shard tokens for a date.
- `ExportUnstructuredEvents` — given a shard, stream its events.

The v17 RPCs are JSON (`google.protobuf.Struct`) on the wire; the legacy
ones use the typed protobuf union.

## Configuration resources

Three resources govern this whole subsystem. All three are
`tctl get`-readable and `tctl create -f`-writable on self-hosted; on Cloud
they're typically managed by Gravitational (audit config) or the customer
(session recording, EAS).

### `cluster_audit_config` — `api/types/audit.go`

Singleton resource (one per cluster). Key accessors on the Go interface
`ClusterAuditConfig` (`audit.go:42-78`):

| Method | What it returns |
|--------|----------------|
| `Type()` | Backend type name (`"dynamodb"`, `"athena"`, `"file"`, …). Mostly informational. |
| `Region()` | AWS region for cloud backends. |
| `ShouldUploadSessions()` (L42-44) | True iff `AuditSessionsURI` is non-empty — i.e. recordings are being uploaded somewhere off the node. |
| `AuditSessionsURI()` (L46-49) | The URI for the **session recording** backend (S3, GCS, Azure, file). |
| `AuditEventsURIs()` (L51-54) | List of URIs for the **audit log** backends. Multiple = `MultiLog` fan-out (but see EAS short-circuit in `01`). |
| `RetentionPeriod()` (L77) | Audit retention (used by DynamoDB at least). |
| Various DynamoDB knobs (read/write capacity, autoscaling, continuous backups, FIPS) | … |

This resource is generally Gravitational-managed in Cloud and you won't
edit it directly — but `tctl get cluster_audit_config` will show what's
in effect.

### `session_recording_config` — `api/types/sessionrecording.go`

Singleton. Interface (`sessionrecording.go:31-48`):

```go
// v17.7.20
type SessionRecordingConfig interface {
    ResourceWithOrigin
    GetMode() string             // "node" | "proxy" | "off" | "node-sync" | "proxy-sync"
    SetMode(string)
    GetProxyChecksHostKeys() bool
    SetProxyChecksHostKeys(bool)
    Clone() SessionRecordingConfig
}
```

The only meaningful field for our purposes is `Mode`. Constants are at
`api/types/constants.go:1276-1296`. A Cloud + EAS tenant typically uses
`node-sync`.

### `external_audit_storage` — `api/types/externalauditstorage/externalauditstorage.go`

Enterprise-only. Has the most interesting fields, all detailed in
`04-cloud-and-external-audit-storage.md`:

```go
// v17.7.20 (lib/.../externalauditstorage.go:52-75)
type ExternalAuditStorageSpec struct {
    IntegrationName        string  // OIDC integration name
    PolicyName             string  // IAM policy name (e.g. "ExternalAuditStoragePolicy-<uuid>")
    Region                 string
    SessionRecordingsURI   string  // s3://teleport-longterm-<uuid>/sessions
    AthenaWorkgroup        string  // teleport_events_<uuid>
    GlueDatabase           string  // teleport_events_<uuid>
    GlueTable              string  // teleport_events
    AuditEventsLongTermURI string  // s3://teleport-longterm-<uuid>/events
    AthenaResultsURI       string  // s3://teleport-transient-<uuid>/query_results
}
```

Two named instances: a `draft` (resource name
`MetaNameExternalAuditStorageDraft`) generated by Cloud as a template the
customer applies their CloudFormation/Terraform against, and the active
`cluster` instance (`MetaNameExternalAuditStorageCluster`) created by
"promoting" the draft. See `04` for the full lifecycle.

## RBAC for an external consumer

Any service that wants to read events or pull recordings out of Teleport
needs a Teleport identity with the right verbs on the right resources. The
canonical role template is at
`integrations/event-handler/tpl/teleport-event-handler-role.yaml.tpl`:

```yaml
# v17.7.20 (verbatim)
kind: role
metadata:
  name: teleport-event-handler
spec:
  allow:
    rules:
      - resources: ['event', 'session']
        verbs: ['list','read']
version: v4
---
kind: user
metadata:
  name: teleport-event-handler
spec:
  roles: ['teleport-event-handler']
version: v2
```

Decoding:
- `event:list` — required by `SearchEvents` and `GetUnstructuredEvents`.
- `event:read` — required by individual event reads.
- `session:list` and `session:read` — required by `SearchSessionEvents`,
  `StreamSessionEvents`, `StreamUnstructuredSessionEvents`,
  `GetEventExportChunks`, `ExportUnstructuredEvents`.

RBAC is enforced in `lib/auth/auth_with_roles.go` — `SearchEvents` checks
at L6418 (rough region; verify with grep against your release tag),
`SearchSessionEvents` similarly. Failure looks like
`AccessDenied("user does not have permission to perform action…")`.

To mint credentials for a service running this role: on the auth server (or
via tctl from anywhere with admin),

```bash
tctl auth sign --user=teleport-event-handler --out=event-handler --ttl=1000h
```

This produces `event-handler.crt`, `event-handler.key`, and `event-handler.cas`
which together let the holder dial the auth gRPC with mTLS for ~6 weeks.

## Web UI is just another consumer

`lib/web/apiserver.go` exposes audit data over HTTPS for the browser:

| Endpoint (rough line) | What it does |
|-----|----|
| `GET /webapi/sites/:site/events/search` (~L912 region) | `SearchEvents`-equivalent. Query params: `from`, `to`, `limit`, `startKey`, `include` (event types), `order`. Returns JSON. |
| `GET /webapi/sites/:site/events/search/sessions` (~L913) | `SearchSessionEvents`-equivalent. |
| `GET /webapi/sites/:site/sessionlength/:sid` (~L915) | Session duration / metadata. |
| `GET /webapi/sites/:site/ttyplayback/:sid` and friends | Playback over WebSocket — pulls from `StreamSessionEvents`. |

Auth is the user's Web session cookie; RBAC is enforced through
`auth_with_roles.go`. Useful as a tap if you can issue a Web session, but
the protocol is HTTP-polling not push so latency is high.

## tctl / tsh inspection commands

For a human operator (and for understanding what an agent could do via
`tctl exec` or by reusing the same code paths):

`tool/tctl/common/recordings_command.go` (~L46-131):

```bash
tctl recordings ls                                # list completed sessions
   --from-utc <RFC3339>     # default: 7 days ago
   --to-utc <RFC3339>       # default: now
   --last <duration>        # e.g. 24h
   --limit <N>
   --format text|json|yaml

tctl recordings download <session-id>             # v17.7+ feature
   --output-dir <path>
```

`recordings download` is interesting because it pulls the recording over
gRPC (`StreamSessionEvents`) and re-tars it locally — no S3 access needed.
This is probably the easiest hands-on way for the user to grab a sample
recording for offline analysis without needing AWS credentials.

`tool/tsh/common/play.go` (~L42-100):

```bash
tsh play <session-id>                  # interactive replay in current terminal
   --speed 1x|2x|4x|8x
   --format pty|json|yaml              # pty = visual replay; json = events as text
tsh recordings ls                      # like tctl recordings ls but uses tsh creds
tsh recordings export <sid>            # like tsh play --format json
```

`tsh play --format json` is *the* command for "show me one session as a
stream of decoded audit events." Useful for understanding the event order
and shape before writing any consumer code.

Other useful inspection:

```bash
tctl get cluster_audit_config        # what backend does the audit log use
tctl get session_recording_config    # what mode is recording in
tctl get external_audit_storage      # the EAS spec — bucket URIs, Glue names
tctl get role/teleport-event-handler # the role template (if present)
tctl tokens ls                       # service tokens (sometimes used by event handler)
```

## Open-source vs Enterprise gating

- The audit log + session recording code paths in `lib/events/`, `lib/srv/`,
  `lib/bpf/`, `lib/player/`, all event protos, the legacy and v17 RPCs, the
  Event Handler integration, and the storage backends — all OSS.
- **External Audit Storage** is Enterprise-only. The resource type lives in
  OSS (`api/types/externalauditstorage/`) but its activation logic and the
  Cloud-controller side that hands the customer a draft is in `e/`. From the
  OSS code you can see all the *interfaces*; you cannot see the controller.
- The runtime `externalAuditStorage.IsUsed()` check
  (`lib/service/service.go:1893`) is a clean seam: it's compiled in but its
  return value depends on whether the Enterprise license enables it.

For the user's tenant (Enterprise Cloud), EAS is presumed enabled — confirm
with `tctl get external_audit_storage`.

## Versioning & stability

From `CHANGELOG.md` highlights for the v17.x series:

- v17.7: `tctl recordings download` added (#63727).
- Several fixes to event handler under audit-log-on-S3 (#62149, #62142,
  #62519). Confirms the Athena / EAS / Event Handler path is the actively
  maintained line.
- The legacy `SearchEvents` / `StreamSessionEvents` RPCs are stable since
  v15+. The v17 unstructured RPCs (`GetUnstructuredEvents` etc.) were
  introduced specifically to support the Event Handler's bulk-export pattern;
  consider them stable for v17.

## Cross-references

- `01-audit-log-plumbing.md` — what the events themselves look like and how
  the auth server picks a backend.
- `02-session-recording-plumbing.md` — what gets streamed over
  `CreateAuditStream` and how it lands as an S3 object.
- `04-cloud-and-external-audit-storage.md` — concrete Cloud + EAS bucket /
  Glue / KMS layout, with `tctl` and AWS commands you can run today.
- `05-tap-points-for-detection.md` — how to actually consume any of this
  from outside.
