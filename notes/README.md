# Teleport v17 Audit & Session Recording — Research Notes

These notes are a first-principles map of how Teleport v17 captures, transports,
and stores audit events and session recordings. They are the **step 1** output
of a multi-step program whose later steps are:

  2. Decide where a downstream detection / classifier apparatus should tap into
     the system (see `05-tap-points-for-detection.md`) and design the local
     analysis CLI that drives it (`06-pipeline-design.md`).
  3. Build the actual detections / agent-driven session classifier on top of
     the SQLite extract that step 2 produces.

The notes are written for a senior engineer who already understands the
operational surface of Teleport (you have Web UI access, you can run `tctl`, you
know what a Proxy and a Node are) but who wants a load-bearing mental model of
what is happening *inside* the binaries.

## Source pin

All `path:line` references are relative to the `upstream-repo/` directory at
the project root (a sibling of `notes/`). The repo is pinned to git commit
`2797910` — `Release 17.7.20`. To re-pin to a different release, from the
project root:

```bash
cd upstream-repo
git fetch --tags
git checkout v17.X.Y     # or another release tag
```

Then re-run the spot-check from `99-open-questions.md` against any references
that look stale. Line numbers will drift across patch releases; symbol names
generally won't.

## Reading order

Read sequentially the first time. Cross-reference freely after that.

| File | Read for |
|------|----------|
| [01 — Audit log plumbing](01-audit-log-plumbing.md) | Event schema, codes, emitter, fan-out, storage backends, search APIs, gotchas |
| [02 — Session recording plumbing](02-session-recording-plumbing.md) | Per-protocol capture, ProtoStreamV1 format, S3 multipart upload, completer, playback |
| [03 — Ecosystem & gRPC API](03-ecosystem-and-grpc-api.md) | Components, reverse tunnel, gRPC RPCs, config resources, RBAC, inspection commands |
| [04 — Cloud & External Audit Storage](04-cloud-and-external-audit-storage.md) | **Weighted** — Cloud topology, EAS lifecycle, bucket layout, Parquet/Glue schema. Most relevant file for this user. |
| [05 — Tap points for a detection pipeline](05-tap-points-for-detection.md) | The four ways to subscribe to events / recordings, with auth, latency, fidelity, cost |
| [06 — Pipeline design](06-pipeline-design.md) | KISS Go-CLI design: Athena + S3 direct, ProtoStreamV1 parsing, SQLite with K8s-style classification labels |
| [99 — Open questions](99-open-questions.md) | What we couldn't answer from source alone, with a verification recipe per item |

Plus one historical / meta artifact, optional reading:

| File | Read for |
|------|----------|
| [00 — The plan we used for step 1](00-step1-plan.md) | How these notes came to look the way they do, and which of the early agent-synthesised claims got corrected during execution. Useful when you're about to plan step 2 and want a template. |

## Conventions

- `lib/...:LNNN` means line `NNN` of the file at that path under `upstream-repo/`.
- Code blocks marked `// v17.7.20` have been read directly out of the source.
  Other code is illustrative.
- Where the notes paraphrase rather than quote, they say so.
- Where a fact came from a sub-agent's synthesis but was later corrected by
  reading source, the corrected version is what appears here. The original
  Explore-agent transcripts are not preserved.
- Anything we *couldn't* verify from source is listed in
  `99-open-questions.md`, never inlined as if it were known.

## How to validate these notes

Three layers:

1. **Source spot-check.** Pick five `path:line` references at random and `Read`
   them in `upstream-repo/`. Confirm the symbol still exists at (or near) the
   stated line. Update the note if it drifted by more than a handful of lines.
2. **Live cluster spot-check.** A handful of `tctl get …` and `aws s3 ls …`
   commands listed in `04-cloud-and-external-audit-storage.md` will confirm the
   shape of your specific tenant.
3. **Second opinion.** Hand `01..05` to a fresh Claude Code session and ask
   "find anything wrong or oversimplified in these notes against the source at
   `upstream-repo/`." Carry corrections in.

## What is *not* in scope here

- No live cluster access — these notes are derived purely from the open-source
  code at the pinned commit, which is what runs inside the Cloud tenant.
- No detection logic, classifier, or pipeline implementation. That's step 2/3.
- No Enterprise-only `e/` source code. External Audit Storage is an Enterprise
  feature, but its public-facing types and the OSS code paths it interacts with
  are in this repo and that's enough to understand the data flow.
