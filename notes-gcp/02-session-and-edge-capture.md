# 02 — Session and edge capture

## 30-second summary

GCP does not have a Teleport-equivalent "session recording". The
closest substrates are session-edge audit events plus a few
specialized streams that capture "what was done" without "what was
typed". This file enumerates the four substrates that get a
classifier closest to a notion of "interactive privileged session",
names the gap where PTY contents *should* be, and points to where
each stream lands in the aggregated sink.

The substrates, ranked by how much the classifier can do with them:

1. **GKE kube-apiserver audit** (`k8s.io`) — closest to a recording.
   Every kubectl action, including `exec`, surfaces with
   `requestObject` / `responseObject`. Includes the command for
   `exec` (and stdin/stdout if the cluster has it enabled at the
   right audit level — usually it doesn't).
2. **IAP audit logs** (`iap.googleapis.com`) — strong session-edge:
   who opened a tunnel to which instance, when, for how long. The
   bytes that flowed inside the tunnel are not captured.
3. **OS Login** — when enabled on a Compute instance, every SSH
   session is tagged with the GCP principal, and the session edges
   (login/logout) are exported to Cloud Logging.
4. **Cloud Workstations / Cloud Shell** — admin events around
   session creation; no in-session contents.

## (1) GKE kube-apiserver audit — `k8s.io`

### What it is

The Kubernetes audit log inside a GKE cluster, exported to Cloud
Logging with `logName =
projects/<id>/logs/cloudaudit.googleapis.com%2Fdata_access` (or
`activity` for mutating calls) and
`protopayload_auditlog.serviceName = k8s.io`. Each event corresponds
to one kube-apiserver request — Get, List, Watch, Create, Update,
Patch, Delete, and crucially **Exec / Attach / PortForward**.

### Why it's the closest thing to a session recording

For non-`exec` activity, every kubectl action is structured: method,
verb, resource, namespace, name, full request body in
`protopayload_auditlog.request`. So a kubectl-driven session shows
up as an ordered series of fully-typed events that the classifier
can replay.

For `exec` and `attach`, the audit log captures the API request to
*establish* the streaming connection, but **not the bytes that
stream through it**. Even with the most permissive audit policy,
kube-apiserver does not record stdin/stdout for an `exec` because
the connection is hijacked into a websocket; the audit event ends
at the upgrade handshake.

### Load-bearing fields beyond what `01` covers

| Field | Tells you |
|-------|-----------|
| `protopayload_auditlog.resourceName` | `core/v1/namespaces/<ns>/pods/<pod>/exec` for an exec |
| `protopayload_auditlog.request.command` | The command being exec'd (when present) |
| `protopayload_auditlog.request.stdin` / `tty` | Whether the call requested an interactive TTY |
| `protopayload_auditlog.authenticationInfo.principalEmail` | The GCP-resolved identity (after Connect Gateway / IAP-Proxy normalization) |
| `protopayload_auditlog.requestMetadata.callerIp` | The Connect Gateway IP if going through it; otherwise the user's egress IP |

### Audit policy levels

GKE clusters have an *audit policy* that controls how much of each
request is logged. The four levels:

- `None` — log nothing (rare).
- `Metadata` — request metadata only, no bodies.
- `Request` — metadata + request body.
- `RequestResponse` — metadata + request body + response body.

The default GKE audit policy is `Metadata` for most resources and
`Request` for sensitive ones (Secrets, RBAC, etc.). To get
`RequestResponse` for `exec` you'd need a custom audit policy
applied via `--audit-policy-file` at cluster creation, which is rare
and not the FFF default. So for the classifier, assume `exec` audit
events have the request command available but no in-session
contents.

### Gotcha

`kubectl` UA strings are noisy: `kubectl/v1.28.0 (linux/amd64)` is
the human pattern; `client-go/v0.28.0` is the agent / SDK pattern.
But agents that drive `kubectl` (rather than `client-go`) look like
humans. Cross-check with cadence.

## (2) IAP audit — `iap.googleapis.com`

### What it is

Identity-Aware Proxy is the gate for both browser-app access (IAP
for HTTPS) and Compute SSH/RDP access (IAP TCP forwarding tunnels).
Audit events land with `serviceName = iap.googleapis.com`.

### Two products under one service

- **IAP for HTTPS** — wraps web apps behind a GCP-enforced auth
  gate. Audit events show every authorization decision; per-request
  access logs go to the load balancer's HTTP request log, not here.
- **IAP TCP forwarding** — opens a TCP tunnel from the user's
  laptop to a Compute instance, used by `gcloud compute ssh
  --tunnel-through-iap` and by direct `gcloud compute
  start-iap-tunnel`. Audit events show *that* a tunnel was opened
  and to which instance; the bytes that flowed are not captured.

### Load-bearing fields

| Field | Tells you |
|-------|-----------|
| `protopayload_auditlog.authenticationInfo.principalEmail` | The acting user |
| `protopayload_auditlog.resourceName` | `projects/<p>/iap_tunnel/zones/<z>/instances/<i>` for tunnels |
| `protopayload_auditlog.requestMetadata.callerIp` | The user's source IP |
| `protopayload_auditlog.metadata.connectionInfo` | Tunnel parameters (port, target) |

### What's missing

The actual SSH session content. Once the tunnel is up, IAP is a TCP
proxy and sees only encrypted bytes. To classify what happened
*inside* the SSH session, you need either:

- **OS Login session export** (next section), which gives you
  session edges + tag with the GCP principal,
- A **host-side recorder** (Teleport, auditd, BPF agent), which is
  exactly the substrate the parent project's `notes/` covers, or
- **Cloud Logging agent + Linux auditd integration** on the
  instance, which forwards `execve` / file-open / network-connect
  events back to Cloud Logging tagged with the OS Login GCP
  principal.

In practice, an FFF-built org expects either Teleport or the
auditd-forwarding pattern on instances that take privileged inbound
SSH. Pure-IAP-only-no-recorder is acceptable for short-lived dev
instances but not for prod.

## (3) OS Login

### What it is

When OS Login is enabled on a Compute instance (org policy
`constraints/compute.requireOsLogin` is the FFF default), Linux PAM
authenticates SSH sessions via Cloud Identity. The SSH session
itself runs as a Linux user whose name is derived from the GCP
principal (typically `<username>_<your_domain>_com`).

### What ends up in Cloud Logging

The Compute Engine instance, with the Logging agent (Ops Agent)
installed, forwards `/var/log/auth.log` (or journald-equivalent) to
Cloud Logging. Each session login/logout becomes a log entry with
the OS Login user name. Because OS Login user names are
deterministic from the GCP principal, the classifier can join back:
`<username>_<your_domain>_com` in the host log ↔
`<username>@<your-domain>` in the audit log.

In addition, OS Login itself emits Admin Activity audit events under
`oslogin.googleapis.com` for SSH key import / posix-account changes.

### What's missing

In-session keystrokes / commands. Unless the host runs auditd /
sysmon-for-Linux / a Teleport node, the kernel events that would
reveal "what commands ran" are not captured. The GCP-side host
substrate stops at "user logged in".

## (4) Cloud Workstations / Cloud Shell

### What it is

Cloud Workstations is GCP's managed dev-environment-as-a-service.
Cloud Shell is the smaller browser-based shell every GCP user gets.

### What ends up in audit logs

Admin events: workstation creation, start, stop, update.
`serviceName = workstations.googleapis.com`. No in-session contents.

For Cloud Shell, the same pattern: session start/stop events under
`cloudshell.googleapis.com`, no contents.

These two substrates are interesting only as session-edge markers —
they tell you "this user spent N minutes in a managed shell" but
nothing about what ran inside it. For classifier purposes, treat
them as session-edge events that route into the
`routing.cohort=managed-shell` cohort.

## The PTY gap, named explicitly

The Teleport-side classifier's phase-1 rule engine is keystroke-
cadence on `SessionPrint` bytes. **There is no GCP-side substrate
that emits keystroke-level data.** This is a hard gap, not a
documentation oversight. The classifier has to substitute features
from elsewhere:

- **For GKE non-exec kubectl activity:** call-graph cadence per
  `(principalEmail, time-window)`. Humans pause between commands;
  agents emit tight bursts.
- **For GKE exec sessions:** the gap is mostly opaque. The audit
  log captures the exec start; OS-side capture inside the pod (if
  any) is in the workload's stdout, not in audit logs.
- **For IAP-tunneled SSH:** if the host runs Teleport or auditd,
  the classifier has substrate; otherwise, only tunnel-edge events.
- **For pure gcloud / Terraform / kubectl-from-laptop:** call-graph
  cadence is all there is. This is the dominant case for the
  agent-driven cohort and is the focus of `06-pipeline-design.md`.

This means the GCP-side phase-1 detector looks structurally
different from the Teleport-side phase-1 detector: it groups events
by `(principalEmail, time-window)` instead of by `session_id`,
because *there is no session_id in audit logs*. A "session" is a
synthetic construct the classifier builds.

## Cross-references

- `01-cloud-audit-logs.md` — the underlying `LogEntry`/`AuditLog`
  shape every substrate above shares.
- `03-ecosystem-and-apis.md` — Connect Gateway and the
  identity-resolution path GKE audit logs flow through.
- `06-pipeline-design.md` — how the classifier synthesizes
  "session" windows from per-principal call streams.
