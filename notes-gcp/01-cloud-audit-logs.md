# 01 — Cloud Audit Logs anatomy

## 30-second summary

Cloud Audit Logs is GCP's first-party audit substrate. Every API call
to every Google Cloud service that opts in (essentially all of them)
emits a `LogEntry` whose `protoPayload` is an `AuditLog` proto carrying
who, what, when, where, and whether the call was permitted. Four
streams exist (Admin Activity, Data Access, System Event, Policy
Denied) with different default-on behavior, retention, and cost. With
an org-level aggregated sink in place, all four streams land in a
single BigQuery dataset where a classifier can query them with
date-partitioned SQL.

Two non-obvious things to internalize up front:

- **There is no PTY equivalent.** Audit logs capture API calls, not
  terminal contents. IAP-SSH session bytes never appear here. The
  phase-1 classifier signal has to come from call-graph pacing,
  sequence shape, and `userAgent` strings — not keystroke cadence.
- **`callerSuppliedUserAgent` is the single highest-signal field.**
  `google-cloud-sdk/<ver> command/gcloud.compute.ssh` reveals the
  exact gcloud subcommand. Terraform, Pulumi, kubectl, and most
  coding agents also surface here. It is client-controlled and
  trivially spoofable, but spoofing is rare in practice.

## The four streams

| Stream | Default | What it captures | Cost concern |
|--------|---------|------------------|--------------|
| **Admin Activity** | always on, can't disable | Mutating API calls: create/update/delete on resources, IAM grants, config changes | free; ~400-day retention in `_Required` |
| **Data Access** | off by default; FFF enables for sensitive services | Read-only and data-plane operations: GCS object reads, KMS key uses, secret accesses, IAM SA token mints | billable per GiB ingested |
| **System Event** | always on, can't disable | GCP-side automation: live migration, instance preemption, automatic GKE node repair | free |
| **Policy Denied** | always on, can't disable | Calls denied by org policy or VPC Service Controls | free |

In an FFF-built org, Data Access logs are explicitly turned on for at
least: `cloudkms.googleapis.com`, `secretmanager.googleapis.com`,
`iam.googleapis.com`, `iamcredentials.googleapis.com`. Some FFF
variants also enable it for `storage.googleapis.com` (DATA_READ +
DATA_WRITE) and `cloudfunctions.googleapis.com`. Verify the exact set
with the audit-config query (see `99-open-questions.md` Q2).

## The `LogEntry` and `AuditLog` shape

Every audit log row is a `google.logging.v2.LogEntry` whose `payload`
field is `protoPayload` of type `google.cloud.audit.AuditLog`. Reading
the BigQuery export is the easiest way to internalize the shape; the
top-level columns of the BQ-exported table are:

| Column | Source | Notes |
|--------|--------|-------|
| `timestamp` | `LogEntry.timestamp` | UTC, sub-second precision |
| `logName` | `LogEntry.logName` | One of `projects/<id>/logs/cloudaudit.googleapis.com%2F{activity,data_access,system_event,policy}` |
| `resource.type` / `resource.labels` | `LogEntry.resource` | e.g. `gce_instance`, `k8s_cluster`, `iam_role` |
| `severity` | `LogEntry.severity` | INFO / NOTICE / WARNING / ERROR |
| `protopayload_auditlog.serviceName` | the service that handled the call | e.g. `compute.googleapis.com`, `iap.googleapis.com` |
| `protopayload_auditlog.methodName` | the API method | e.g. `v1.compute.instances.get`, `google.cloud.iap.v1.IdentityAwareProxyAdminService.GetIamPolicy` |
| `protopayload_auditlog.resourceName` | dotted path to the target resource | e.g. `projects/<p>/zones/us-central1-a/instances/<name>` |
| `protopayload_auditlog.authenticationInfo` | who | see below |
| `protopayload_auditlog.authorizationInfo` | what perms were checked | array, each with `granted: bool` |
| `protopayload_auditlog.requestMetadata` | the wire-level call context | see below |
| `protopayload_auditlog.request` / `protopayload_auditlog.response` | the call payload | usually present in Data Access; partial in Admin Activity |
| `protopayload_auditlog.metadata` | service-specific extra | e.g. for IAP, the connection info |

The `protopayload_auditlog` prefix is how Cloud Logging flattens the
`protoPayload` discriminated union into the BQ schema. The export
also adds a `protoPayload.@type` value of
`type.googleapis.com/google.cloud.audit.AuditLog` for audit entries.

### `authenticationInfo` (the WHO)

The fields a classifier should reach for first:

| Field | Tells you |
|-------|-----------|
| `principalEmail` | The acting identity. Shape: `<user>@<your-domain>`, `*.iam.gserviceaccount.com`, `*@google.com` (Access Transparency only), or — for federated identities — `principal://iam.googleapis.com/projects/.../subject/...` |
| `principalSubject` | The federated identity subject claim, when WIF is in play |
| `serviceAccountKeyName` | If a long-lived SA key was used, the key name. Presence is itself a signal — most modern setups don't use SA keys |
| `serviceAccountDelegationInfo[]` | Impersonation chain. If the call was made via `gcloud --impersonate-service-account` or via `iamcredentials.generateAccessToken`, the chain is here. Each entry has `principalSubject` and the principal that impersonated. Strongest "human ran a tool that became an SA" signal. |
| `principalType` | Documented enum; primarily `USER`, `SERVICE_ACCOUNT`, `SERVICE_AGENT`, `WORKFORCE_IDENTITY`, `WORKLOAD_IDENTITY` |

### `requestMetadata` (the HOW)

| Field | Tells you |
|-------|-----------|
| `callerIp` | Source IP. Corp / VPN range vs unknown egress. Cross-reference against BeyondCorp / Context-Aware Access logs to see device posture. |
| `callerSuppliedUserAgent` | Client UA. The single highest-signal field. Examples: `google-cloud-sdk/<ver> command/gcloud.compute.ssh.tunnel-through-iap`, `Terraform/1.6.0 (+https://www.terraform.io) terraform-provider-google/4.x`, `kubectl/v1.28.x (linux/amd64) kubernetes/...`, `Boto3/1.x Python/3.x`. Coding agents (Claude Code, Cursor, Codex) often have distinct UA strings; some passthrough is the underlying gcloud UA. |
| `requestAttributes.time` | When the call was issued |
| `destinationAttributes.principal` | The identity *being acted on* in IAM-shaped APIs (e.g. when granting a role, this is the grantee) |

### `authorizationInfo[]` (the WHAT-WAS-CHECKED)

Each entry carries `permission` (e.g. `compute.instances.get`),
`resource` (the path), `granted` (bool), and optional
`resourceAttributes`.

Denials are gold for classification: humans fumble permissions in
characteristic shapes (try → realize → re-auth → retry); agents
fumble in a different characteristic shape (try → fail → try with
slightly modified params → fail → escalate). Don't filter
`granted=false` rows out of your dataset.

## Anchor `methodName`s worth memorizing

These are the calls that show up most often in privileged-user
activity:

| `serviceName` | `methodName` | What it's a marker of |
|---------------|--------------|----------------------|
| `iap.googleapis.com` | `AuthorizeUser`, `GetIamPolicy`, `SetIamPolicy` | IAP gate decisions and admin |
| `iap.googleapis.com` | tunnel-related | An IAP TCP tunnel was opened — see `02` |
| `compute.googleapis.com` | `v1.compute.instances.get`, `.list`, `.osLogin` | Compute reads; OS Login session edges |
| `iam.googleapis.com` | `google.iam.admin.v1.IAM.SetIamPolicy` | Direct IAM grant |
| `iamcredentials.googleapis.com` | `GenerateAccessToken`, `SignBlob`, `SignJwt` | SA impersonation — strong agent-tool signal |
| `cloudkms.googleapis.com` | `Encrypt`, `Decrypt`, `AsymmetricSign` | KMS key use — Data Access |
| `secretmanager.googleapis.com` | `AccessSecretVersion` | Secret read — Data Access |
| `container.googleapis.com` | `v1.GoogleContainer*.GetCluster` | GKE control plane reads |
| `k8s.io` (special — see `02`) | `io.k8s.core.v1.pods.exec.create` | `kubectl exec` — closest thing to "session start" inside GKE |
| `cloudresourcemanager.googleapis.com` | `SetIamPolicy` | Project / folder / org IAM |

The `methodName`s look like fully-qualified RPC names because that's
what they are. Filtering by `serviceName` first then by `methodName`
is cheaper in BigQuery than substring-matching on `methodName`
alone.

## Identity types and where they come from

| `principalEmail` shape | Identity type | Notes |
|------------------------|---------------|-------|
| `<name>@<your-domain>` | Cloud Identity / Workspace user | Direct SSO |
| `<name>@<your-domain>` (federated) | Workforce Identity Federation | The IdP (Okta, Entra, Azure AD) is the actual identity source; this email is the federated mapping |
| `<sa>@<project>.iam.gserviceaccount.com` | User-managed service account | Created in your project |
| `<service>@<project-num>.iam.gserviceaccount.com` | Google-managed service agent | e.g. `service-<num>@compute-system.iam.gserviceaccount.com` |
| `principal://iam.googleapis.com/projects/<num>/locations/global/workforcePools/<pool>/subject/<id>` | Workforce Federation, raw form | For federated users that don't get an email mapping |
| `principal://iam.googleapis.com/projects/<num>/locations/global/workloadIdentityPools/<pool>/subject/<id>` | Workload Identity Federation | GKE workloads, GitHub Actions, etc. |
| `<name>@google.com` | Google personnel | Access Transparency only — different stream, see `03` |

For classification, the *shape* of `principalEmail` is the first cut.
Then `serviceAccountDelegationInfo` tells you whether the call was
human-initiated through impersonation.

## v1 vs v2 audit log API

The Cloud Logging API has two generations:

- **v1** (`google.logging.v1`) — deprecated for new code; some legacy
  exporters still use it.
- **v2** (`google.logging.v2`) — current. `entries.list`,
  `entries.write`, `entries.tail` are the read APIs; the LogEntry
  shape above is v2.

The BigQuery export schema is v2. Anything reading from the
aggregated sink is reading v2 data.

## Quotas, retention, and limits

(Defaults; verify against your tenant in `99-open-questions.md`.)

| Aspect | Default |
|--------|---------|
| Admin Activity retention (in `_Required` bucket) | 400 days, can't be reduced |
| Data Access retention (in `_Default` bucket) | 30 days; configurable up to 3650 |
| LogEntry max size | 256 KiB after JSON serialization |
| `entries.list` quota | 60 requests/min per project (default; raisable) |
| BigQuery export latency | ~minutes; not real-time |
| Pub/Sub export latency | ~seconds |

## Failure modes and gotchas

1. **Data Access logs cost real money at scale.** A chatty service
   with per-request DATA_READ logging can ingest tens of GiB/day.
   FFF defaults are conservative; expanding the set is a deliberate
   cost decision.
2. **Org-policy denials don't always include `request`/`response`.**
   The Policy Denied stream gives you `protoPayload.status` and the
   violated rule, but the original request shape may be redacted.
3. **`callerSuppliedUserAgent` is client-controlled.** Trivially
   spoofable. In practice spoofing is rare; treat it as a strong but
   not authoritative signal.
4. **`principalEmail` can be empty.** For some service-to-service
   calls inside GCP's own infrastructure, the field is omitted.
   Filter on `principalEmail IS NOT NULL` for human-attributable
   activity.
5. **Method-name format is service-dependent.** Some services use
   the gRPC-fully-qualified style
   (`google.iam.v1.IAMPolicy.SetIamPolicy`), others use
   `v1.compute.instances.get`. Don't write a single regex that
   assumes one shape.
6. **Aggregated-sink delay.** With an org-level sink to BigQuery,
   end-to-end emit-to-queryable latency is typically 1-3 minutes;
   on bad days, longer. Not real-time. For real-time alerting, use
   Pub/Sub instead (see `05`).
7. **`logName` URL-encodes the slash.**
   `cloudaudit.googleapis.com/activity` appears as
   `cloudaudit.googleapis.com%2Factivity` in the field. Easy to trip
   over when filtering.

## Things that are *not* in Cloud Audit Logs

- Terminal session contents (anything that happens *after* `iap
  tunnel` hands off the SSH stream).
- VM-internal activity (anything Linux auditd / sysdig / falco would
  capture). VPC Flow Logs see network metadata; the OS sees process
  syscalls. Cloud Audit Logs see neither.
- GKE workload application logs (use Cloud Logging's standard
  application logs).
- CDN cache hits (use Cloud CDN logs separately).

## Cross-references

- `02-session-and-edge-capture.md` — what fills the PTY-shaped gap
  (or more honestly, what doesn't).
- `03-ecosystem-and-apis.md` — the wider GCP audit ecosystem and
  where this fits.
- `04-org-aggregation-and-storage.md` — the BigQuery schema you
  actually query against.
- `05-tap-points-for-detection.md` — how a downstream classifier
  consumes any of this.
