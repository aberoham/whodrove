# 06 — Pipeline design

This file is the step-2 deliverable of the project plan in `00-step1-plan.md` / `notes/README.md`. 
Step 3 picks up from §8 below.

## Purpose & non-goals

**In scope.** Tap choice, Go CLI shape, the Athena query that drives it, the
ProtoStreamV1 parser plan, and the SQLite schema for per-session features
plus Kubernetes-style classification labels.

**Out of scope.** The classifier model, the detection rules, the training
data, and any alerting. Those are later steps.

**Scoping decisions.** The earlier draft of this file
proposed a long-running k8s/Postgres pipeline. Four user-supplied
constraints collapsed that:

1. **Ad-hoc local analysis, not a long-running pipeline.** Runs from macOS
   or a dedicated Linux VM, in dedicated analysis blocks, against bounded
   date ranges (typical window 30-90 days). Backfill is the same CLI
   invocation with a wider date range — no separate code path.
2. **KISS dependencies.** Single static Go binary, SQLite, brew-installable
   CLIs. No k8s, no Postgres, no Vault, no Docker unless absolutely
   necessary.
3. **Go over Python**, because upstream Teleport is Go and we can import
   `lib/events.NewProtoReader` and the audit-event protobuf types under
   `api/types/events` directly. No `protoc` step, no protobuf generation.
4. **Recording-content from day 1.** Parse `<sid>.tar` PTY bytes for the
   human-vs-agent signal; do not gate it on a later phase.

**Detection-strategy hint that shapes the schema.** Phase-1 of the
classifier (step 3) answers "is this a human terminal?" first. Cheap signal:
`SessionPrint` chunk-arrival cadence, idle gaps, and edit characters
(backspace, arrow keys, `ctrl-w`). Coding agents probably drive `tsh` as sequences
of single-shot exec calls, not long-lived `tsh ssh` PTYs, so the
human-vs-agent split shows up clearly in cadence features. Caveat:
`kubectl`, `tbot`, and non-`tsh` SSH are naturally single-shot or non-PTY,
so the heuristic weakens or inverts there. Step-2's job is to record
`kind` and the relevant command-shape features so step-3 can route those
sessions to a separate cohort.

## Architecture

```
macOS / Linux $ teleport-analyze pull --since 2026-03-25 --until 2026-04-25
    │
    ├── Athena query (aws-sdk-go-v2) for T2005I session.upload events,
    │   partition-pruned by event_date
    │     → list of (session_id, user, recording_uri, ...)
    │
    ├── for each session_id:
    │     ├── s3:GetObject <prefix>/<sid>.tar
    │     ├── events.NewProtoReader → audit events incl. SessionPrint
    │     ├── extract per-session features (timings, command shape,
    │     │   PTY-ness, BPF events if present)
    │     └── upsert into local sessions.sqlite
    │
    └── exit. analyze sessions.sqlite with `sqlite3` or any ad-hoc tool.
```

Tap choice: **(d) Athena for event discovery + (c) S3 direct for
recordings**, per the recommendation in `05-tap-points-for-detection.md:196-212`.
Skip (a) gRPC (we have no real-time need; this also avoids cert rotation
and the `AsyncEmitter` silent-drop concern at `01-audit-log-plumbing.md:19-21`)
and (b) the Web UI HTTP path (never recommended). Step 3 may add a gRPC
real-time tap on top of the same SQLite if alerting latency demands it.

## CLI shape

Single Go binary, `go install ./cmd/teleport-analyze`. Static, no CGO when
built with `modernc.org/sqlite`. Runs on macOS and Linux.

Subcommands:

- `pull --since DATE --until DATE` — fetch + populate `sessions.sqlite`.
- `pull --session-id SID` — single session, ad-hoc.
- `pull --no-recordings` — events-only sweep, no S3 GETs.
- `parse <path/to/sid.tar>` — local-file mode for offline development.
- `label set --session SID --key KEY --value VALUE` — manual stamp.
- `label ls --selector KEY=VALUE[,KEY=VALUE…]` — Kubernetes-style label
  selector query.

Idempotent: re-runs over overlapping date ranges are safe (`session_id` is
the primary key; upserts use `INSERT … ON CONFLICT DO UPDATE`).

Auth: standard AWS chain — `AWS_PROFILE`, `aws-vault exec`, or env. No
Vault, no OIDC machinery, no Teleport cert.

Dependencies:

- `github.com/aws/aws-sdk-go-v2/{config,service/athena,service/s3}`.
- `modernc.org/sqlite` (pure Go, no CGO; or `mattn/go-sqlite3` if a team
  member already has the CGO toolchain).
- `github.com/gravitational/teleport`, pinned to commit
  `27979100040cba4e568b6740d3e94f2eeaa180cb` (matches the submodule pinned
  in `notes/README.md`). Used for `lib/events.NewProtoReader` and the
  audit-event types under `api/types/events`. `api/` is a nested Go module
  inside that repo, but downstream consumers do not need a `replace`
  directive — it resolves transitively.
- `github.com/spf13/cobra` for subcommand wiring.

## Athena query

```sql
SELECT uid, event_time, session_id, user,
       json_extract_scalar(event_data, '$.session_recording_url') AS recording_uri,
       json_extract_scalar(event_data, '$.cluster_name')          AS cluster
FROM   teleport_events_<uuid>.teleport_events
WHERE  event_date BETWEEN date(?) AND date(?)
  AND  event_type = 'session.upload'
ORDER  BY event_time ASC;
```

- `event_date` partition prune is mandatory; without it Athena scans every
  partition (`04-cloud-and-external-audit-storage.md:248-252`).
- `session.upload` (T2005I) is the durability signal for "the recording is
  finalised in S3", not `session.end` (T2004I)
  (`02-session-recording-plumbing.md:288-292`).
- Set a per-query bytes-scanned cap on the customer Athena workgroup as a
  safety net against accidental full-history scans.

## Recording parsing (ProtoStreamV1)

Import `github.com/gravitational/teleport/lib/events` and call
`events.NewProtoReader(r io.Reader) *ProtoReader` against the S3 GET
body (`upstream-repo/lib/events/stream.go:933`). Iterate with
`Read(ctx) (apievents.AuditEvent, error)` until `io.EOF`; or call
`ReadAll(ctx)` for small recordings. Returned values are the same
`apievents.AuditEvent` types Teleport itself emits — no separate protobuf
generation step, no `protoc` install.

Format reference (only relevant if we ever need to re-implement; from
`upstream-repo/lib/events/stream.go:65-74` and the layout doc at `:231-249`):
24-byte header (3×Int64Size — version, meaningful size, padding size),
gzip-compressed body, 4-byte length-prefixed records, max 64 KiB per record
pre-compression.

Pin `github.com/gravitational/teleport` in `go.mod` to commit
`27979100040cba4e568b6740d3e94f2eeaa180cb`. When the upstream submodule is
re-pinned, bump the `go.mod` entry in the same commit. Do not plan to
vendor `stream.go` alone as a fallback — its imports
(`github.com/gravitational/trace`, `clockwork`, `lib/defaults`,
`lib/session`, `lib/utils`) tie it to a non-trivial subset of the upstream
tree. If transitive deps ever become painful, the answer is to import a
narrower subset, not to vendor.

## SQLite schema

One file `sessions.sqlite`, three tables. Classifier output is stored as
Kubernetes-style key/value labels rather than fixed columns, so a classifier
can stamp many independent facts (`operator.type=human`,
`confidence.operator-type=0.92`, `agent.tool=claude-code`,
`work.kind=deploy`, …) without schema migrations, and selection queries
become first-class.

```sql
CREATE TABLE sessions (
  session_id            TEXT PRIMARY KEY,
  user                  TEXT NOT NULL,
  cluster               TEXT,
  kind                  TEXT,         -- ssh|kube|db|app|desktop
  started_at            TEXT NOT NULL,
  ended_at              TEXT,
  uploaded_at           TEXT,         -- T2005I event_time
  duration_seconds      REAL,
  -- recording metadata
  recording_uri         TEXT,
  recording_bytes       INTEGER,
  -- phase-1 features (human-vs-agent), extracted at parse time
  pty_present           INTEGER,      -- 0/1: did SessionPrint exist?
  print_chunks          INTEGER,      -- count of SessionPrint records
  print_bytes           INTEGER,
  median_chunk_gap_ms   REAL,         -- inter-chunk PTY arrival gap
  idle_gap_count        INTEGER,      -- gaps > N seconds (configurable)
  edit_char_count       INTEGER,      -- backspace/arrow/ctrl-w/etc.
  -- command-shape features
  command_count         INTEGER,      -- if BPF SessionCommand present
  bpf_present           INTEGER,      -- 0/1
  single_shot           INTEGER,      -- 0/1: exec-only invocation?
  join_count            INTEGER,      -- moderation/co-shell joiners
  -- bookkeeping
  parsed_at             TEXT NOT NULL,
  parser_version        TEXT NOT NULL, -- the teleport-analyze version
  parse_error           TEXT
);
CREATE INDEX idx_sessions_user      ON sessions(user);
CREATE INDEX idx_sessions_uploaded  ON sessions(uploaded_at);
CREATE INDEX idx_sessions_kind      ON sessions(kind);

-- Kubernetes-style labels. Many key/value pairs per session.
-- Stable keys: lowercase, optional dotted prefix
--   (e.g. operator.type, work.kind, agent.tool, review.note,
--   confidence.operator-type, routing.cohort). No spaces.
CREATE TABLE session_labels (
  session_id  TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  key         TEXT NOT NULL,
  value       TEXT NOT NULL,           -- stringly-typed; numeric labels
                                       -- store the canonical decimal form
  set_by      TEXT NOT NULL,           -- "phase1-classifier@v0.3"
                                       -- or "manual:abe@2026-04-25"
  set_at      TEXT NOT NULL,
  PRIMARY KEY (session_id, key)        -- one value per (session,key);
                                       -- re-stamp = update + bump set_at
);
CREATE INDEX idx_labels_kv      ON session_labels(key, value);
CREATE INDEX idx_labels_session ON session_labels(session_id);

-- Optional companion table for events the classifier might want to
-- revisit; never the full stream.
CREATE TABLE notable_events (
  session_id  TEXT NOT NULL REFERENCES sessions(session_id) ON DELETE CASCADE,
  event_time  TEXT NOT NULL,
  event_type  TEXT NOT NULL,           -- session.exec, session.join,
                                       -- session.command (BPF), etc.
  payload     TEXT NOT NULL            -- JSON; no raw PTY bytes
);
CREATE INDEX idx_notable_session ON notable_events(session_id);
```

Label conventions, documented in this doc and enforced loosely by the CLI:

- `operator.type` ∈ `human | agent | unknown`.
- `agent.tool` ∈ `claude-code | codex | gemini-cli | unknown` (only set
  when `operator.type=agent`).
- `work.kind` ∈ `deploy | debug | exploration | automation | data-exfil | unknown`.
- `confidence.<labelkey>` is numeric in `[0, 1]`.
- `review.*` is reserved for human-stamped review notes.
- `routing.cohort` is reserved for step-3 cohort routing
  (e.g. `routing.cohort=single-shot-non-pty`).

Selector example (Kubernetes-style):

`teleport-analyze label ls --selector operator.type=agent,agent.tool=claude-code`

```sql
SELECT s.* FROM sessions s
JOIN session_labels la ON la.session_id=s.session_id
                      AND la.key='operator.type' AND la.value='agent'
JOIN session_labels lb ON lb.session_id=s.session_id
                      AND lb.key='agent.tool'   AND lb.value='claude-code';
```

**Privacy invariant.** Raw `SessionPrint` bytes are NEVER persisted to
SQLite. Only derived numeric features and labels. Recordings stay in S3;
the binary downloads, parses, extracts, drops.

## Backfill = the same CLI

`pull --since 2026-01-25 --until 2026-04-25` is the backfill — there is no
separate code path. Serial recording fetches by default. Add
`--parallel-fetch N` only if cross-AZ egress is acceptable. Use
`--no-recordings` for a fast events-only sweep when you don't yet care
about PTY features.

## Detection strategy hint (carry to step 3)

- **Phase-1 (cheap, rules-only).** "Human terminal y/n" using
  `pty_present`, `print_chunks`, `median_chunk_gap_ms`, `idle_gap_count`,
  `edit_char_count`, `single_shot`. Plain SQL or a small Go rule-set; no
  LLM needed. Stamps labels: `operator.type=human|agent|unknown`,
  `confidence.operator-type=<float>`, with `set_by=phase1-classifier@vN`.
- **Phase-2 (deeper).** Only runs for sessions where phase-1 stamped
  `operator.type=agent` and `kind='ssh' AND pty_present=1`. This is where
  LLM-on-PTY-bytes might earn its cost. Stamps additional labels:
  `agent.tool`, `work.kind`, plus matching `confidence.*`.
- **Edge cohort.** `kind IN ('kube','db','app','desktop')` plus
  `single_shot=1` non-PTY ssh — these look agent-like by construction.
  Phase-1 should stamp `operator.type=unknown` and a label like
  `routing.cohort=single-shot-non-pty` so step-3 can route them to a
  different (yet-unwritten) classifier rather than mis-applying the
  human-vs-agent heuristic.

## What step-3 picks up from here

Three seams: phase-1 classifier, phase-2 classifier, and an anomaly
detector reading from the same SQLite extract. No further design here.

## Open questions resolved by this design

The following entries in `99-open-questions.md` are resolved-by-design
because we tap Athena + S3 directly and use neither the Event Handler nor
the gRPC export-chunks API:

- **Q7** (Event Handler in production) — N/A. We do not depend on the
  Event Handler.
- **Q17** (`GetEventExportChunks` re-poll dedupe) — N/A. We do not call
  `GetEventExportChunks`; Parquet rows are addressable by `event_date`
  partition + `uid`, and recordings are addressed by `<sid>.tar`.

The remaining open questions Q1-Q6, Q8-Q16 still apply. The natural
follow-up after this doc lands is to knock down Q1, Q2, Q4, Q5, Q6 against
the live tenant — those replace the `<uuid>` placeholders, confirm the
partition projection, and tell us whether BPF events are available to the
classifier.
