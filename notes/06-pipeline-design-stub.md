# 06 — Pipeline design (STUB for step 2)

> **This file is intentionally near-empty.** It is a placeholder for step 2
> of the project — designing the actual detection / classification pipeline.
> Step 1 deliberately stops at "here are the four ways you could tap in"
> (`05-tap-points-for-detection.md`); this file is where the next session
> picks up.

## Purpose of step 2

Design the data path that takes audit events + session recordings out of
Teleport and into:

- a real-time anomaly detector that surfaces "this session looks weird"
  alerts;
- a classifier that buckets each session by **operator kind** (human / bot
  / AI agent) and **work kind** (deploy, debug, exploration, automation,
  data exfil, …).

Step 2 will not write detection rules or train a classifier (that's step 3).
It will produce:

- a chosen tap (or hybrid) from `05-tap-points-for-detection.md`,
- a deployment topology (where the consumer runs, what it stores state in),
- a normalised-event schema the downstream stages consume,
- a backfill plan for historical data,
- a security / RBAC story for the tap credentials,
- and a `notes/06-pipeline-design.md` (no `-stub`) replacing this file.

## Decision points the next session needs to resolve

These are the open knobs. Each is referenced from elsewhere in the notes
where context lives.

1. **Real-time vs. batch vs. hybrid.** See `05-tap-points-for-detection.md`.
   Likely answer: hybrid (gRPC for real-time, S3+Athena for batch and for
   recording-content classification). Confirm requirement on alert latency
   first.
2. **Tap from existing SIEM, or upstream of it?** Discussed in
   `05-tap-points-for-detection.md` "What about reading from your existing SIEM?".
   The recording-content classifier can't run from the SIEM alone; that
   forces *some* upstream tap. Question is whether the event-level signals
   come from SIEM (cheaper) or from gRPC/S3 (no projection drift).
3. **Recording-content analysis.** Yes/no/sometimes. If yes, parse strategy:
   pure rules (regex over `SessionPrint` bytes), structured-event extraction
   (`SessionCommand` from BPF), or LLM-on-PTY-bytes? Cost and privacy tradeoffs.
4. **Where the consumer runs.** AWS account-local (lowest S3 latency,
   simplest IAM), separate VPC, on-prem. Drives whether (c) and (d) are
   first-class.
5. **State model.** Cursor only? Per-session classification cache?
   Long-term detection store? What schema?
6. **Backfill.** How far back? Athena can scan the entire EAS bucket from
   day one; recordings can be re-classified by re-reading `<sid>.tar`. The
   constraint is cost and time, not capability.
7. **Failure semantics.** What happens when the classifier crashes mid-batch?
   Idempotent re-runs require dedup by event UID (relevant because
   `GetEventExportChunks` re-emits chunks on re-poll — see `01`).
8. **Privacy & data retention.** Recording bytes contain typed credentials,
   secrets, customer data. Classifier output may be more or less sensitive
   than the input; storage and access have to be designed accordingly.
9. **Cost ceiling.** Athena bytes-scanned per day; recording bytes
   downloaded per day; LLM tokens per session (if applicable). Bound up
   front, not after the first bill.

## What to do *not* do in step 1

- Do not pre-commit to any tap (this file's whole point).
- Do not add detection rules to the notes — they belong in step 3.
- Do not flesh this stub out into a full design without a step-2 conversation.

## Pointers

- `05-tap-points-for-detection.md` — the four tap options with trade-offs.
- `04-cloud-and-external-audit-storage.md` — what the user already has
  (EAS bucket + Glue + Athena ready to go).
- `99-open-questions.md` — live-tenant facts to gather before deciding.
