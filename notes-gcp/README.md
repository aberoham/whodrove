# shellscope GCP Notes — Cloud Audit Logs & Privileged-User Activity

These notes are the GCP-side counterpart to `notes/`. They map the same
problem — "where does privileged-user activity live, and how does a
downstream classifier tap into it?" — onto Google Cloud, under the
assumption that the target organization has followed Google's enterprise
blueprint (Cloud Foundation Fabric / "FAST", or equivalent) and has
org-level logging, aggregated sinks, and security defaults already
enabled.

The classifier goal is the same as the Teleport side: tag each
privileged-user session as human-driven, agent-driven, or agent-assisted,
with cohort routing for substrates that don't fit the heuristic.

## How this differs from `notes/`

The Teleport notes are grounded in source code we can read at a pinned
commit (`upstream-repo/` at `Release 17.7.20`). GCP is a managed service:
the source of truth is product documentation, the on-the-wire `LogEntry`
schema, and the resources you can `gcloud` against in your own org.
References in this set are to **product names + log names + documented
schemas** rather than `path:line` into a repo.

The substrate is also fundamentally different:

- **No PTY recording.** GCP audit logs capture API calls, not keystrokes.
  The phase-1 PTY-cadence rules from the Teleport classifier
  (`notes/06-pipeline-design.md`) do not transfer; the GCP-side phase-1
  has to be call-graph cadence and `userAgent` shape instead.
- **No `<sid>.tar` blob.** There is no "session recording" to pull.
  IAP-tunneled SSH gives you tunnel metadata only; GKE kube-apiserver
  audit gives you per-call records but no terminal contents; OS Login
  emits session-edge events but not in-session bytes.
- **One big BigQuery dataset, not Athena + S3.** With the FFF aggregated
  sink, audit events land in BigQuery already partitioned and queryable.
  The classifier substrate is BigQuery from day one — not Parquet-on-S3
  with Athena over the top.
- **No session_id.** Audit events do not group into "sessions" the way
  Teleport recordings do. The classifier has to *synthesize* sessions
  from `(principalEmail, time-window)` clusters.

## Reading order

Read sequentially the first time. Cross-reference freely after that.

| File | Read for |
|------|----------|
| [00 — Plan](00-plan.md) | The plan we used to produce these notes; what's done, what's outstanding |
| [01 — Cloud Audit Logs anatomy](01-cloud-audit-logs.md) | The four audit-log streams, `LogEntry`/`AuditLog` proto, the load-bearing fields for classification |
| [02 — Session-edge and interactive capture](02-session-and-edge-capture.md) | IAP, OS Login, GKE k8s.io audit, Cloud Workstations / Cloud Shell — what "interactive privileged session" looks like in GCP, and the explicit gap where PTY contents would be |
| [03 — Ecosystem and APIs](03-ecosystem-and-apis.md) | Components, identity systems, Log Router, Asset Inventory, the GCP equivalents of "components emit through gRPC to auth" |
| [04 — Org aggregation and storage](04-org-aggregation-and-storage.md) | **Weighted.** The FFF aggregated sink, BigQuery dataset shape, GCS archive layout, Chronicle path, retention. Most relevant file for this work. |
| [05 — Tap points for detection](05-tap-points-for-detection.md) | BigQuery direct, Pub/Sub stream, Cloud Logging API, GCS archive, Chronicle — auth, latency, fidelity, cost |
| [06 — Pipeline design](06-pipeline-design.md) | KISS Go-CLI mirroring `teleport-analyze`: BigQuery extract → SQLite with K8s-style classification labels |
| [99 — Open questions](99-open-questions.md) | What we couldn't answer from product docs alone, with a verification recipe per item |

## Conventions

- Placeholders: `<org-id>`, `<project-id>`, `<logging-project>`,
  `<bq-dataset>`, `<gcs-archive-bucket>`, `<audit-topic>`,
  `<chronicle-instance>`, `<region>`, `<your-domain>`. Replace with
  values from your tenant.
- Where a fact is anchored to a specific GCP resource type or API, the
  note cites the canonical product name (e.g.
  `cloudaudit.googleapis.com/activity`, `iap.googleapis.com`, `k8s.io`).
- Where the notes paraphrase rather than quote, they say so.
- Anything we couldn't verify against documentation or a live tenant
  is listed in `99-open-questions.md`, never inlined as if it were
  known.

## Assumptions about the tenant

These notes assume an FFF-style org build-out:

- Org-level Cloud Audit Logs are enabled (Admin Activity is always-on
  by default; Data Access is enabled at least for the FFF default set —
  Cloud KMS, Secret Manager, IAM, IAM Service Account Credentials).
- An org-level aggregated log sink routes audit logs into a centralized
  logging project, with at least a BigQuery destination (and typically
  also GCS + Pub/Sub).
- VPC Flow Logs are enabled on shared-VPC subnets used by privileged
  workloads.
- An external IdP federates via Workforce Identity Federation, or
  Cloud Identity is the IdP.
- Service accounts use Workload Identity Federation or impersonation
  rather than long-lived JSON keys (or, if keys exist, their use is
  audited).
- VPC Service Controls perimeters wrap the logging project.

If your tenant deviates from these defaults, refer to
`99-open-questions.md` for which assumptions need revisiting.

## How to validate these notes

Three layers, mirroring `notes/README.md`:

1. **Doc spot-check.** Pick five claims at random and verify against
   the live GCP product documentation. Update if the doc has drifted.
2. **Live tenant spot-check.** A handful of `gcloud`, `bq`, and
   `gsutil` commands listed in `04-org-aggregation-and-storage.md`
   will confirm the shape of the user's specific org.
3. **Second opinion.** Hand `01..05` to a fresh Claude Code session
   and ask "find anything wrong or oversimplified in these notes
   against the GCP product docs". Carry corrections in.

## What is *not* in scope here

- No live tenant access — these notes are derived from product
  documentation and the Cloud Foundation Fabric / FAST blueprint as
  published.
- No detection logic, classifier, or pipeline implementation.
- No GCP-managed-service-only internals (e.g. how Chronicle's parsers
  work) beyond the consumer surface.
