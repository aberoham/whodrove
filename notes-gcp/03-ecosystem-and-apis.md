# 03 — Ecosystem and APIs

## 30-second summary

GCP's audit ecosystem isn't a single service — it's a convergence of
three things: the **services that emit** audit events (essentially
every Google Cloud service), the **Log Router** that fans out the
emitted entries to one or more sinks, and the **identity systems**
that determine what `principalEmail` ends up on each entry. This file
maps that ecosystem and names the APIs a classifier touches.

## Component topology

```
                ┌──────────────────────────────────┐
                │      EVERY GCP SERVICE           │
                │  compute, iam, iap, container,   │
                │  cloudkms, secretmanager, ...    │
                │  emits LogEntry → Cloud Logging  │
                └──────────────┬───────────────────┘
                               │
                               ▼
                ┌──────────────────────────────────┐
                │         CLOUD LOGGING            │
                │   - per-project _Required bucket │
                │   - per-project _Default bucket  │
                │   - per-project user buckets     │
                │                                  │
                │         LOG ROUTER               │
                │   org-aggregated sinks fan out:  │
                └──────────────┬───────────────────┘
                               │
        ┌──────────┬───────────┼───────────┬──────────┐
        ▼          ▼           ▼           ▼          ▼
    BigQuery    GCS       Pub/Sub     another      Chronicle
    dataset    bucket     topic       Logging      (Google
   (workbench) (archive) (streaming)  bucket       SecOps SIEM)
                                      (per-folder)
```

The dual-line entries in the diagram correspond to what an FFF-built
org typically deploys.

## Identity systems behind `principalEmail`

| System | What it is | Where principal comes from |
|--------|------------|----------------------------|
| **Cloud Identity** | GCP's first-party IdP. Owns the `<your-domain>` namespace if you don't federate. | Direct, native principal |
| **Google Workspace** | Same backend as Cloud Identity, just packaged with productivity apps | Direct, native principal |
| **Workforce Identity Federation** | Federates external IdPs (Okta, Entra, Azure AD, Auth0) for *human* access to GCP | Maps `<sub>` claim into a `principal://...` URN that audit logs record |
| **Workload Identity Federation** | Federates external workloads (GitHub Actions, AWS, on-prem) into SA-equivalent access | Same `principal://...` shape but under `workloadIdentityPools` |
| **Service Accounts** | Bot identities first-classed in GCP | `<name>@<project>.iam.gserviceaccount.com` |
| **Service Agents** | Google-managed SAs that GCP services use on your behalf | `service-<num>@<system>.iam.gserviceaccount.com` |
| **Access Transparency** | Logs Google personnel access to your data | `<name>@google.com`, separate stream |

For classifier purposes, the three populations to keep separate are:

- **Humans** — Cloud Identity, Workspace, Workforce Federation
- **Agents you operate** — Service accounts, Workload Federation
  principals
- **Google personnel** — Access Transparency stream, treat as
  out-of-band

## The Log Router

`Log Router` is GCP's name for the routing layer between log
emission and storage. Three building blocks:

- **Sinks.** A filter expression + a destination. Sinks can be
  created at project, folder, or org level. Org-level sinks with
  `--include-children` capture everything below.
- **Buckets.** Storage in Cloud Logging itself. Every project gets
  `_Required` (Admin Activity, can't disable) and `_Default`
  (everything else). User-created buckets give you per-region
  storage with custom retention.
- **Exclusion filters.** Per-sink filters that drop matching entries
  before they go anywhere — used to keep noisy logs out of expensive
  destinations.

For privileged-user activity classification, the org-level
aggregated sink to BigQuery is the workbench. For real-time
alerting, the same sink (or a sibling) to Pub/Sub is the substrate.
See `04` for the specific FFF shape.

## APIs a classifier touches

### Read paths

| API | Purpose | Notes |
|-----|---------|-------|
| `logging.googleapis.com/v2/entries:list` | Direct query of Cloud Logging | 60 req/min default; rate-limited |
| `logging.googleapis.com/v2/entries:tail` | Streaming read | gRPC; useful for low-latency dev |
| BigQuery `googleapis.com/bigquery/v2/jobs:query` | SQL against the BQ-exported audit dataset | The workhorse for batch |
| Pub/Sub `pubsub.googleapis.com/v1/...:pull` or streaming pull | Stream from the routed Pub/Sub topic | The path to real-time |
| GCS `storage.googleapis.com/storage/v1/b/<bucket>/o/<obj>` | Read archived JSON-line logs | Backfill tier |
| Chronicle UDM Search API | Query Chronicle if deployed | Different query language; UDM model |

### IAM permissions a classifier service account needs

For BigQuery direct read:
- `bigquery.jobs.create` (run queries)
- `bigquery.tables.getData` on the audit dataset
- `roles/bigquery.dataViewer` is the easiest predefined role

For Pub/Sub stream:
- `pubsub.subscriptions.consume` on the audit subscription
- `roles/pubsub.subscriber` predefined role

For Cloud Logging direct:
- `logging.privateLogEntries.list` (Data Access stream — broader
  access)
- `logging.logEntries.list` (general)
- `roles/logging.privateLogViewer`

For GCS archive:
- `storage.objects.get`, `storage.objects.list` on the archive
  bucket
- `roles/storage.objectViewer`

In an FFF org, the convention is to put the classifier in its own
project under the security folder, give its SA a custom role that
bundles the read perms above, and pin the SA's perimeter via VPC SC
so its credentials can't be used outside the security perimeter.

## Resource hierarchy and audit-config inheritance

```
Organization (<org-id>)
└── Folder: security
    └── Project: logging-aggregation       ← BigQuery dataset, GCS bucket, Chronicle ingest
└── Folder: prod
    └── Project: prod-app-1
    └── Project: prod-app-2
└── Folder: dev
    └── Project: dev-sandbox-1
```

Audit config (which Data Access streams are on, with what filters)
inherits down: org → folder → project. An FFF setup configures Data
Access at the org level for sensitive services so every project
gets the same coverage. Per-project overrides exist but are rare.

## Asset Inventory and the change feed

Cloud Asset Inventory (`cloudasset.googleapis.com`) is the
not-quite-audit-but-related substrate. It snapshots every resource
in the org and emits a change feed via Pub/Sub. For a classifier,
it's useful as the join target: "this audit event acted on this
resource at that time; what did the resource look like, what labels
did it have, what folder did it live in?"

Two APIs:

- `cloudasset.assets.list` / `searchAllResources` — point-in-time
  query.
- `cloudasset.feeds.create` → Pub/Sub feed of every resource change.

FFF deploys an org-level asset feed by default. The classifier can
join against the feed by resource name + time window to enrich
audit events with resource-side context.

## VPC Service Controls perimeters

VPC SC perimeters wrap a set of projects and limit which APIs can
be called from outside the perimeter. For audit logs, this matters
because:

- The aggregated logging project is typically inside its own
  perimeter.
- The classifier's project should be inside (or bridged into) that
  perimeter so it can read BigQuery / Pub/Sub.
- Audit events that VPC SC denies are emitted into the **Policy
  Denied** stream — useful for catching attempted exfiltration.

Don't try to read across perimeters from a project that isn't
bridged. The error mode is `PERMISSION_DENIED` with a
perimeter-violation reason in the message; surprisingly hard to
diagnose if you're not looking for it.

## Inspection commands

For a human operator (and for understanding what a classifier could
do from a service account):

```bash
# What audit-log buckets exist in a project?
gcloud logging buckets list --location=global --project=<project-id>

# What sinks are routing logs out of an organization?
gcloud logging sinks list --organization=<org-id>

# What does the aggregated sink look like?
gcloud logging sinks describe <sink-name> --organization=<org-id>

# What audit configs are in effect at the org level?
gcloud organizations get-iam-policy <org-id> --format=json \
  | jq '.auditConfigs'

# Inspect a specific recent audit event
gcloud logging read 'logName=~"cloudaudit.googleapis.com"
                     AND protoPayload.authenticationInfo.principalEmail="<user>"' \
   --limit=10 --format=json --project=<project-id>

# What service accounts can be impersonated by what?
gcloud asset search-all-iam-policies \
   --scope=organizations/<org-id> \
   --query='policy:roles/iam.serviceAccountTokenCreator'
```

## Versioning and stability

The `LogEntry` v2 schema and `AuditLog` proto are stable; new
fields get added (the `policy_violation_info` field for VPC SC
denials is a recent example) but old fields don't get removed. The
BigQuery export schema mirrors this: new sub-fields appear as
nested STRUCTs without breaking existing queries.

Chronicle's UDM is a separate stability concern — Google has
changed UDM field names in past major versions. If you depend on
UDM field-name stability, pin the Chronicle parser version.

## Cross-references

- `01-cloud-audit-logs.md` — the `LogEntry`/`AuditLog` shape itself.
- `02-session-and-edge-capture.md` — IAP, OS Login, GKE specifics.
- `04-org-aggregation-and-storage.md` — the FFF aggregated-sink
  shape in detail.
- `05-tap-points-for-detection.md` — picking among the API options
  above for a downstream classifier.
