# 00 — Plan: GCP-side privileged-user activity classifier

> Companion to the Teleport `notes/00-step1-plan.md`. This plan extends
> the `shellscope` classifier scope from Teleport session audit +
> recordings to GCP Cloud Audit Logs and friends. The end state is the
> same: a per-session record in SQLite with K8s-style labels
> (`operator.type`, `agent.tool`, `work.kind`, `routing.cohort`, …)
> that downstream rules and human review can act on.

## Context

The user is the administrative owner of:

- A Teleport Enterprise Cloud tenant — substrate covered in `notes/`.
- A GCP organization that has been built out following Google's
  enterprise blueprint (Cloud Foundation Fabric / "FAST"), with
  org-level logging, aggregated sinks, VPC SC perimeters, and the
  standard FFF security defaults.

A growing share of system-administration activity bypasses Teleport
entirely:

- Operators (and operator-driven coding agents) using `gcloud`, the
  Google Cloud SDK, or `terraform` against GCP APIs from a developer
  laptop or a CI runner.
- IAP-tunneled SSH that goes straight from the operator's laptop to a
  Compute instance, bypassing any Teleport node we operate.
- `kubectl exec` against GKE clusters, where the audit log is GCP-side,
  not Teleport-side.

The GCP audit substrate is what catches all of this. This plan covers
the *understanding* phase only; the actual classifier ports from the
Teleport side are a later step.

## Multi-step program (mirrors the Teleport side)

1. **(this plan)** Build first-principles understanding of the GCP
   audit substrate: what gets logged, where it lands, how it's queried,
   what load-bearing fields the classifier should care about. Documented
   as durable markdown research notes under `notes-gcp/`.
2. Decide where the GCP-side classifier should tap (BigQuery direct
   vs Pub/Sub vs Chronicle), and design a Go binary analogous to
   `teleport-analyze` that ingests into the same SQLite labels schema.
3. Build the actual GCP-side classifier — including the call-graph
   cadence model that has to substitute for PTY cadence.

This plan covers **only step 1** for the GCP substrate. It produces
the markdown notes under `notes-gcp/` (a sibling of `notes/` at the
project root) derived from GCP product documentation and the assumed
FFF baseline.

## What we already understand (from prior conversation, 2026-04-25)

The conversation that led to this plan established:

- **Audit-log streams.** Cloud Audit Logs has four streams: Admin
  Activity (always on, free, generous retention), Data Access (off by
  default, FFF-enabled for sensitive services), System Event (always
  on), Policy Denied (always on).
- **Aggregated sink.** FFF deploys an org-level aggregated log sink
  that fans out to BigQuery (workbench), GCS (archive), and usually
  Pub/Sub (for SIEM forwarding) and/or Chronicle (Google SecOps SIEM).
- **Load-bearing fields for classification.** Inside `protoPayload`:
  `authenticationInfo.principalEmail`, `serviceAccountDelegationInfo`
  (impersonation chain), `requestMetadata.callerSuppliedUserAgent`
  (gcloud subcommand visible here), `requestMetadata.callerIp`,
  `authorizationInfo[].granted`, `serviceName`, `methodName`.
- **Identity types.** Human SSO (`<user>@<your-domain>`), service
  accounts (`*.iam.gserviceaccount.com`), Workforce Federation
  (`principal://...`), Google personnel (`*@google.com`, via Access
  Transparency).
- **Session-edge substrates.** IAP TCP tunnel logs (metadata only),
  OS Login (session edges tagged with GCP principal), GKE kube-apiserver
  audit (the closest thing to a "session recording" — kubectl actions
  with `requestObject`/`responseObject`), Cloud Workstations / Cloud
  Shell audit.
- **The PTY gap.** GCP does not record terminal contents. IAP-SSH
  session bytes are invisible to GCP unless an OS-level recorder runs
  on the host. This forces a different phase-1 strategy: call-graph
  pacing/sequencing per `(principalEmail, time-window)` instead of
  keystroke cadence.

## Recommended approach

Create a `notes-gcp/` directory at the project root with the same
eight-file shape as `notes/`. Each file is short enough to read
end-to-end in one sitting; opinionated about what matters; references
back to GCP product documentation rather than `path:line` into a repo.

### Files to create

| # | Path | Purpose |
|---|------|---------|
| 0 | `notes-gcp/README.md` | Index + project goals + reading order + assumptions about the tenant |
| 0' | `notes-gcp/00-plan.md` | This file |
| 1 | `notes-gcp/01-cloud-audit-logs.md` | The four audit-log streams, the `LogEntry`/`AuditLog` proto, the fields that carry the human-vs-agent signal |
| 2 | `notes-gcp/02-session-and-edge-capture.md` | IAP, OS Login, GKE k8s.io, Cloud Workstations / Cloud Shell — what "interactive privileged session" looks like in GCP, and the PTY gap |
| 3 | `notes-gcp/03-ecosystem-and-apis.md` | Components, identity systems, Log Router, Asset Inventory, the GCP equivalents of "components emit through gRPC to auth" |
| 4 | `notes-gcp/04-org-aggregation-and-storage.md` | **Weighted.** FFF aggregated sink, BigQuery dataset shape, GCS archive layout, Chronicle path, retention, cost. |
| 5 | `notes-gcp/05-tap-points-for-detection.md` | BigQuery direct, Pub/Sub stream, Cloud Logging API, GCS archive, Chronicle — auth, latency, fidelity, cost |
| 6 | `notes-gcp/06-pipeline-design.md` | KISS Go-CLI mirroring `teleport-analyze`: BigQuery extract → SQLite with K8s-style classification labels; per-`(principal, window)` synthetic sessions |
| 7 | `notes-gcp/99-open-questions.md` | What we couldn't answer from product docs alone, with a verification recipe per item |

### Validation approach

Same three layers as the Teleport notes:

1. **Doc spot-check.** Pick five claims at random and verify against
   the live GCP product documentation.
2. **Live tenant spot-check.** A handful of `gcloud`, `bq`, and
   `gsutil` commands listed in `04` will confirm the shape of the
   user's specific org.
3. **Second opinion.** Hand `01..05` to a fresh Claude Code session
   and ask for corrections against the live product docs.

### Out of scope for step 1

- No code changes to the existing `cmd/teleport-analyze` binary or
  `internal/` packages.
- No connection to the live GCP org (these notes are derived from
  product docs + the FFF blueprint as published).
- No detection logic, classifier, or pipeline implementation.
- No decision yet on which tap point to use — `05` lays out the
  options; the choice is step 2.

### What comes after this plan

When step 1's notes exist and have been spot-checked, step 2 takes
`notes-gcp/06-pipeline-design.md` and either:

- Extends the existing `teleport-analyze` Go binary with a
  `gcp-pull` subcommand family that queries BigQuery, OR
- Builds a sibling `cmd/gcp-analyze` binary that writes into the same
  `sessions.sqlite` labels schema.

Either way, the SQLite output is shared with the Teleport side, so
the step-3 classifier rules can ingest both substrates uniformly.
The schema decision tilts toward "extend `teleport-analyze` with a
`--substrate` flag" rather than a separate binary, but the call is
deferred to the start of step 2 when we have a clearer picture of
how much GCP-specific code is needed.

## Verification (for step 1's deliverable)

Step 1 is "done" when:

- [ ] All 8 files exist under `notes-gcp/`.
- [ ] `notes-gcp/README.md` correctly indexes the others.
- [ ] Every product-doc reference resolves to a current Google Cloud
      doc page (spot-check 5).
- [ ] `notes-gcp/04-org-aggregation-and-storage.md` includes a
      concrete documented BigQuery schema and example query, not just
      a hand-wave.
- [ ] `notes-gcp/99-open-questions.md` lists at least the
      live-tenant questions that must be answered before the
      `<org-id>` / `<bq-dataset>` placeholders can be replaced.
- [ ] A fresh reader can answer "where do privileged-user `gcloud`
      calls land, and how do I query them?" by reading only `01` and
      `04`.
- [ ] A fresh reader can answer "how would the classifier subscribe
      to GCP audit events from a sidecar?" by reading only `05`
      (with cross-refs to `01` and `03`).

## Why this stays separate from the Teleport notes

Two reasons to keep `notes-gcp/` and `notes/` as parallel directories
rather than one merged tree:

1. **Pin discipline.** The Teleport notes anchor every claim to
   `upstream-repo/` at a specific commit. The GCP notes have no
   equivalent pin — Google Cloud is a managed service. Mixing the
   two would require two different validation regimes in one tree.
2. **Reading flow.** A reader investigating Teleport audit shouldn't
   have to skim past GCP-specific tangents, and vice versa. Each
   directory is internally consistent; cross-refs go through the
   top-level `README.md`.

The eventual classifier code (step 2/3) is *not* split this way —
both substrates write into the same `sessions.sqlite` and the same
labels schema. The split is purely a research-and-docs convention.
