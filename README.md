# shellscope

A study of how various popular systems (Teleport, GCP, AWS) capture audit events and session recordings,
and the design for an agent-based session classifier on top of a live Cloud + External Audit Storage tenant. 

## Layout

| Directory          | What's in it                                                                                                          | Entry point                                                                  |
|--------------------|-----------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------|
| `notes/`           | Durable research notes (Teleport substrate) — plumbing, storage topology, tap options, the step-2 pipeline design, open questions. | [`notes/README.md`](./notes/README.md)                                       |
| `notes-gcp/`       | Parallel research notes for the GCP substrate — Cloud Audit Logs, FFF aggregated sink to BigQuery, tap points, GCP-side pipeline design. | [`notes-gcp/README.md`](./notes-gcp/README.md)                               |
| `prior-art/`       | Factual notes on third-party work. By `prior-art/AGENTS.md`, these never compare to or position this project.         | [`prior-art/README.md`](./prior-art/README.md)                               |
| `harnesses/`       | Third-party benchmarks/harnesses we expect to run as fixtures (labeled, replayable sessions for downstream detection). | [`harnesses/README.md`](./harnesses/README.md)                               |
| `upstream-repo/`   | Pinned read-only Teleport v17 source (shallow submodule at `2797910` = `Release 17.7.20`).                            | [github.com/gravitational/teleport](https://github.com/gravitational/teleport) |

## Step model

The work has a three-step plan; each step's output is a durable
artifact under `notes/` (Teleport substrate) or `notes-gcp/` (GCP
substrate). Both substrates feed the same `sessions.sqlite` /
labels schema in step 3.

- **Step 1 — Plumbing research.**
  - *Teleport:* Done 2026-04-25.
    See [`notes/01-audit-log-plumbing.md`](./notes/01-audit-log-plumbing.md)
    through [`notes/05-tap-points-for-detection.md`](./notes/05-tap-points-for-detection.md),
    with what couldn't be answered from source alone collected in
    [`notes/99-open-questions.md`](./notes/99-open-questions.md).
  - *GCP:* Done 2026-04-25.
    See [`notes-gcp/01-cloud-audit-logs.md`](./notes-gcp/01-cloud-audit-logs.md)
    through [`notes-gcp/05-tap-points-for-detection.md`](./notes-gcp/05-tap-points-for-detection.md),
    with live-tenant questions in
    [`notes-gcp/99-open-questions.md`](./notes-gcp/99-open-questions.md).
- **Step 2 — Pipeline design.**
  - *Teleport:* Done 2026-04-25.
    See [`notes/06-pipeline-design.md`](./notes/06-pipeline-design.md):
    a single static Go binary `teleport-analyze` that taps Athena for
    `session.upload` events and S3 directly for recordings, parses
    ProtoStreamV1 via `lib/events.NewProtoReader`, and upserts
    per-session features plus Kubernetes-style classification labels
    into a local `sessions.sqlite`.
  - *GCP:* Done 2026-04-25.
    See [`notes-gcp/06-pipeline-design.md`](./notes-gcp/06-pipeline-design.md):
    a sibling Go binary (or extension of `teleport-analyze`) that
    queries BigQuery for per-`(principal, minute)` feature rows,
    synthesises sessions by gluing adjacent buckets, and writes into
    the same `sessions.sqlite` with GCP-flavoured labels
    (`substrate.kind=gcp-cloud-audit`, `gcp.ua.tool`, etc.). No code
    yet — design only.
- **Step 3 — Classifier.** Outstanding. Reads from the shared SQLite
  extract; phase-1 is rules-only (Teleport: "is this a human
  terminal?"; GCP: "is this an operator-driven session?"), phase-2 is
  LLM-on-call-graph (or PTY bytes, on the Teleport side) for sessions
  phase-1 routes to it.

## Conventions

Shared across `notes/`, `prior-art/`, and `harnesses/`:

- `path:line` references in `notes/` are relative to `upstream-repo/`
  unless prefixed. `notes-gcp/` has no equivalent pin (GCP is a
  managed service); it cites product names + log names + documented
  schemas instead.
- No absolute filesystem paths in committed files. Repo-relative only
  (`notes/05-tap-points-for-detection.md`,
  `harnesses/terminal-bench/...`, etc.).
- Tenant-specific values use placeholders. Teleport: `<your-tenant>`
  for the Cloud hostname, `<uuid>` for AWS resource nonces. GCP:
  `<org-id>`, `<logging-project>`, `<bq-dataset>`,
  `<gcs-archive-bucket>`, `<your-domain>`.
- Code blocks marked `// v17.7.20` are copied directly from
  `upstream-repo/` at the pinned commit; everything else is
  illustrative.
- New facts that can't be sourced get added to the relevant
  `99-open-questions.md` with a verification recipe, not inlined as
  if known.

## Cloning

```bash
git clone --recurse-submodules <this-repo>
# or, if already cloned without --recurse-submodules:
git submodule update --init --recursive
```

The submodules are shallow by design; pinning is what guarantees
reproducibility, not history depth.
