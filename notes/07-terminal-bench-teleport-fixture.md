# 07 — Terminal-Bench-as-Teleport-fixture

## Purpose & framing

`05-tap-points-for-detection.md` and `06-pipeline-design.md` answered "where do
we read sessions from" and "how do we store features". Step 3's classifier
needs **labeled** sessions to calibrate against — specifically, sessions
where we already know `operator.type=agent` with high confidence. Live
production tenants are mostly *not* agent-driven, so it can't supply the
positive class on its own.

This file is the design for an **artificial-fixture pipeline** that drives a
known agent through Terminal-Bench tasks inside a self-hosted Teleport OSS
cluster, so that every resulting session recording is labeled
`operator.type=agent` by construction. Output goes into the same
`sessions.sqlite` schema from `06-pipeline-design.md` so phase-1 and phase-2
classifiers can train against a mix of fixture rows and live tenant rows.

Step-3 deliverable, not step-2. Step 2's CLI (`teleport-analyze`) is reused
unchanged on the parsing side; this file only adds a fixture *source*.

## What we're trying to produce

For each Terminal-Bench task run, exactly one Teleport session recording at:

```
operator.type=agent                # by construction
agent.tool=<terminus|claude-code|codex|...>
agent.model=<deepseek-chat-v4|claude-opus-4-7|...>
fixture.source=terminal-bench
fixture.task_id=<task-id>
fixture.task_passed=<0|1>
fixture.tbench_dataset_version=<0.1.1|0.2.x>
fixture.harness_run_id=<uuid>
```

…joinable to a real `<sid>.tar` recording that shellscope's existing parser
can ingest. Multiply by N tasks × M agents × K models and we have a labeled
positive cohort on the order of hundreds to low thousands of sessions for
the cost of model API calls plus a single test VM.

## Architecture choice

### Three options, scored

| Option | Where Teleport sits | Recording fidelity | Task isolation | Harness invasion |
|---|---|---|---|---|
| **(a) bare-metal node, no Docker** | host VM acts as Node | low — every task contaminates the same shell history | none | enormous (rip out Docker) |
| **(b) keep Docker, swap `docker exec` for `tsh exec`** | host VM is Node, tasks remain in Docker on it | very low — recording is many short non-PTY sessions | preserved | medium |
| **(c) Teleport node *inside* each task container** | per-task container is its own Node | high *iff* the harness holds one persistent `tsh ssh -t` per task — otherwise degrades to many short PTY recordings co-grouped by node label | preserved | medium |

**Choosing (c).** The classifier in step 3 keys off cadence, idle gaps, and
edit characters in `SessionPrint` chunks (see
`06-pipeline-design.md:36-43`). Option (b) would emit per-keystroke or
per-command sessions with `pty_present=0` and `single_shot=1` — which is the
*edge cohort* we explicitly route elsewhere via
`routing.cohort=single-shot-non-pty`. Using (b) as a fixture would teach
the phase-1 classifier nothing about the main human-vs-agent question.

Option (c) is the only architecture that *can* produce a single
human-shaped PTY recording per task, but only conditionally: it requires
the harness to open one long-lived `tsh ssh -t agent@<container>` per
task and multiplex tmux send-keys / capture-pane through that shell's
stdin/stdout. Without that refactor, Terminal-Bench's `TmuxSession`
issues one `_exec_run` per tmux subcommand
(`harnesses/terminal-bench/terminal_bench/terminal/tmux_session.py:309`),
each of which becomes its own `tsh ssh -t` invocation and its own
recording. The MVP described in "Implementation status" below ships in
that degraded mode; recordings from the same task are joined back
together via the `fixture/run-id=<uuid>` Teleport node label, and
phase-1 cadence features have to be computed on the union rather than
per `<sid>.tar`. The persistent-PTY refactor is the path to the
single-recording-per-task shape originally assumed.

Note that the agent itself never sees `tsh` in any of these options.
The agent sees the tmux pane contents that `capture_pane` returns and
emits commands the harness delivers via `send_keys`. Whether a task
produces one recording or many is entirely a harness-implementation
decision, not a behavioral property of the agent.

### Topology

```
┌────────────────────────────────────────────────────────────┐
│  Test host (Linux VM, brew-installable Docker + Teleport)  │
│                                                            │
│  ┌──────────────────────────────────────┐                  │
│  │ teleport (auth + proxy, OSS, single  │                  │
│  │ binary, --roles=auth,proxy)          │                  │
│  │  ↑ TLS gRPC                          │                  │
│  └────┬─────────────────────────────────┘                  │
│       │ node-join token                                    │
│  ┌────┴──────────────┬──────────────┐ ...                  │
│  │ tbench-task-001   │ tbench-task-002 │                   │
│  │ (Docker container)│ (Docker container)                  │
│  │ ┌──────────────┐  │ ┌──────────────┐                    │
│  │ │ teleport     │  │ │ teleport     │                    │
│  │ │ --roles=node │  │ │ --roles=node │                    │
│  │ │ + sshd-like  │  │ │ + sshd-like  │                    │
│  │ │   PTY        │  │ │   PTY        │                    │
│  │ └──────┬───────┘  │ └──────────────┘                    │
│  │ tmux ◀┘ (driven   │                                     │
│  │    by tb harness  │                                     │
│  │    via `tsh ssh`) │                                     │
│  └───────────────────┘                                     │
└────────────────────────────────────────────────────────────┘
        ▲
        │  tsh ssh agent-user@tbench-task-001
        │  (each session = one task run)
        │
┌───────┴────────────────────────────────────┐
│  Controller (laptop or same VM)            │
│  - terminal-bench harness with TshTerminal │
│  - shellscope teleport-analyze parse       │
└────────────────────────────────────────────┘
```

Single-host clarification: the test host runs Teleport auth+proxy AND hosts
the per-task containers. Each container becomes its own Teleport Node. The
controller (the laptop running `tb run`) connects out to
`https://test-host:3080` over the public internet (or LAN) and uses
`tsh ssh` to enter each container Node, identical to how `tsh` reaches a
node in a real cluster.

The decision to keep Docker is because Terminal-Bench's task isolation, base
images, and asciinema markers all assume Docker. Replacing the substrate
breaks the harness.

### Recording storage on the OSS test cluster

OSS Teleport stores recordings on the auth server's local filesystem under
`/var/lib/teleport/log/upload/streaming/default/<sid>.tar` (or the configured
`storage` path). No S3, no External Audit Storage. Two consequences:

1. The tap-point matrix in `05-tap-points-for-detection.md` collapses to one
   tap: read the local file directly. There is no Athena, no Parquet.
2. Audit-event discovery in step-2's CLI (`teleport-analyze pull` against
   Athena) does not apply on this fixture cluster. We bypass discovery and
   feed `<sid>.tar` paths directly into `teleport-analyze parse`.

This keeps the fixture pipeline cheap and offline-able. The price is that
event-side audit data (e.g. `session.start`, `session.command` BPF events)
is in the cluster's audit-log backend (SQLite by default for `dir`-mode
storage). Step 3 can choose to ingest it via `tctl events ls` or skip it.

## Components to build

### 1. `harnesses/terminal-bench-teleport/cluster/`

Bring up the OSS test cluster.

- `teleport.yaml` — single-host auth+proxy config. `auth_service.enabled:
  yes`, `proxy_service.enabled: yes`, `ssh_service.enabled: no`.
  `cluster_name: tbench-fixture`. ACME off (self-signed). Web UI port 3080,
  reverse-tunnel port 3024, auth gRPC port 3025.
- `up.sh` — `teleport start -c teleport.yaml -d` (foreground, daemonised by
  systemd or `nohup` per environment). Idempotent: detects an existing
  instance.
- `bootstrap.sh` — first-time cluster setup:
  - `tctl users add agent-user --roles=tbench-agent`
  - Generates the `tbench-agent` role: SSH login `agent`, allow-rule
    `node:list,read`, recording mode `node-sync` (fastest path to a
    finalised `<sid>.tar`).
  - Mints a long-lived node-join token (proxy-issued, TTL=7d).
  - Writes `tsh login --proxy=...` profile for the controller.

### 2. `harnesses/terminal-bench-teleport/image/`

The base image every patched task extends.

- `Dockerfile.base` — `FROM ghcr.io/laude-institute/t-bench/python-3-13:20250620`
  (the t-bench default base, see
  `harnesses/terminal-bench/original-tasks/hello-world/Dockerfile`), then:
  - `apt-get install -y openssh-client tmux asciinema curl` (most are
    already in the t-bench base; this is a belt-and-braces line).
  - `curl … | tar xz teleport` to install the matching Teleport binary
    pinned to `27979100040cba4e568b6740d3e94f2eeaa180cb` (v17.7.20), same
    version as `upstream-repo/`.
  - Installs `entrypoint.sh` at `/usr/local/bin/tbench-teleport-entry`.
  - `useradd agent && mkdir -p /home/agent && chown -R agent:agent /home/agent`.
- `entrypoint.sh` —
  1. Reads `TELEPORT_PROXY_ADDR`, `TELEPORT_JOIN_TOKEN`, `TELEPORT_NODE_LABELS`
     from the environment.
  2. Renders `/etc/teleport.yaml` with `nodename` set from
     `${T_BENCH_TASK_DOCKER_CLIENT_CONTAINER_NAME}` (so the recording's
     `server_hostname` is the join key back to the task ID).
  3. `teleport start -c /etc/teleport.yaml -d` in the background.
  4. `exec sleep infinity` — same final form as the t-bench
     `docker-compose.yaml`.
- `build.sh` — `docker build -t tbench-teleport-base:v17.7.20 -f Dockerfile.base .`

### 3. `harnesses/terminal-bench-teleport/runner/`

The orchestration layer.

- `patch_task.py` — given an upstream `original-tasks/<id>/` directory:
  1. `cp -r` it to a temp dir.
  2. Rewrite `Dockerfile`'s `FROM` line to `FROM tbench-teleport-base:v17.7.20`.
     If the task chains intermediate `FROM`s, re-target the first one only;
     the rest of the build still runs on top.
  3. Rewrite `docker-compose.yaml` to inject `TELEPORT_PROXY_ADDR`,
     `TELEPORT_JOIN_TOKEN`, and a unique `TELEPORT_NODE_LABELS=fixture/run-id=<uuid>`
     env var.
  4. Print the rewritten task path; the t-bench harness reads from there.
- `tsh_terminal.py` — Python module exporting `TshTerminal`, a subclass of
  `terminal_bench.terminal.Terminal` that overrides `create_session` to
  return a `TshTmuxSession` instead of the stock `TmuxSession`. The new
  session class:
  - Replaces `docker exec <container> tmux ...` with
    `tsh ssh -t agent-user@<nodename> "tmux ..."`.
  - Reuses tmux semantics unchanged — tmux runs inside the container as
    before; only the entry path differs. Asciinema recording inside the
    container also still works.
  - Test execution (post-agent verification) stays on `docker exec` because
    that's harness-side scaffolding, not part of the operator session and
    must not be part of the recording.
- `run.py` — orchestrator:
  1. Sanity-checks: cluster reachable, base image present, `tsh` logged in.
  2. For each `(task_id, agent, model)` triple:
     - Patches the task.
     - Records a `harness_run_id` UUID + metadata to a sidecar JSON.
     - Invokes `tb run --task-id <id> --agent <a> --model <m> ...` with
       `TshTerminal` injected via `--terminal-class` (or, if t-bench has no
       such hook, via `PYTHONPATH` shim).
     - Waits for the run; captures the t-bench result JSON, the inside-container
       asciinema cast, and the Teleport `<sid>.tar` from the cluster's
       recordings dir.
- `harvest.py` — for each completed run:
  - Calls existing `teleport-analyze parse <sid>.tar` to populate the
    `sessions` table (this CLI is from `06-pipeline-design.md` and already
    exists in `cmd/teleport-analyze/` per the working tree).
  - Calls `teleport-analyze label set --session <sid> --key <k> --value <v>`
    for each fixture-derived label, setting `set_by=tbench-fixture@vN`.
  - Records `task.passed`, `task.duration_seconds`, `agent.total_tokens`
    from the t-bench result JSON as additional labels.

### 4. `harnesses/terminal-bench-teleport/README.md`

Following the `harnesses/README.md` rules: companion writeup may reference
project goals, must not duplicate upstream docs, no absolute paths.

## Why a custom `Terminal` class instead of forking the harness

Terminal-Bench's `Terminal` (`harnesses/terminal-bench/terminal_bench/terminal/terminal.py`)
already separates concerns cleanly: `DockerComposeManager` owns the
container lifecycle, `TmuxSession` owns the agent-facing PTY. We only need
to rewrite the `TmuxSession` half — the container management, the volume
mounts, the asciinema integration, and the test runner all keep using
`docker exec`. So the smallest viable patch is a subclass plus a 5-line CLI
shim.

If upstream lacks a `--terminal-class` flag, the shim becomes a
`monkeypatch` of `terminal_bench.terminal.terminal.Terminal` at import
time — ugly but contained to the runner. We do not modify the submodule.

## Calibration: 100% agent vs the live tenant

The fixture cluster gives us pure-positive labeled sessions
(`operator.type=agent` for all rows). The live tenant (the user's Cloud +
EAS pipeline from notes 01-04) gives us a mostly-negative cohort
(`operator.type=human` is the prior). The phase-1 classifier from
`06-pipeline-design.md:258-273` should be tuned against:

1. **Fixture-only accuracy.** Run phase-1 over only the fixture rows; it
   should label ≥95% as `agent`. Anything else points at thresholds being
   wrong or feature extraction missing something.
2. **Cross-cohort discrimination.** Mix fixture rows with a date-bounded
   sample of live tenant rows. Compute the confusion matrix. Tune
   thresholds until live-tenant precision for `operator.type=agent` is
   acceptable for review workload.
3. **Adversarial diversity.** Vary `agent.tool` (Terminus vs Claude Code
   vs Codex) and `agent.model` (cheap-and-fast vs frontier) in the
   fixture. If accuracy drops sharply when swapping the agent, we are
   detecting *the scaffold*, not *agency*. That answers the third question
   from `harnesses/terminal-bench.md:120-122`.

This work runs *after* phase-1 ships against live data. The fixture is
input to step-3 calibration, not a prerequisite for step-2 to operate.

## Cost estimate

Cheap by design.

- **Test host.** A `t3.medium`-class Linux VM (2 vCPU / 4 GB) is enough for
  one task at a time. ~$30/mo if left running, ~$0 if torn down between
  fixture runs.
- **Model API.** The whole point of using DeepSeek-V4 (or whatever cheap
  open-weights model is current at run time, see open question Q2 below)
  is to make this affordable. At ~$0.30/M input tokens, 200 tasks × 100k
  tokens/task ≈ $6. Frontier-model runs add a multiplier; budget those
  separately.
- **Storage.** Recordings are small (median ~100 KiB compressed for a
  Terminal-Bench task). 1000 fixtures ≈ 100 MiB on local disk.

## Open questions

These are the items that should land in `99-open-questions.md` once this
plan is merged.

- **Q-FX1.** OSS recording format parity. Step-2's parser uses
  `events.NewProtoReader` against `<sid>.tar`. The format is the same
  ProtoStreamV1 between OSS and Cloud (`02-session-recording-plumbing.md`
  is silent on differences) but worth a single-task spot-check before
  committing to the parser path.
- **Q-FX2.** Cheapest current model. The user mentioned "Deepseek-V4" but
  Terminal-Bench's `llms/` integration is via LiteLLM. Confirm at run time
  which `openrouter/...` or `deepseek/...` model id resolves. Don't hard-code.
- **Q-FX3.** `tb run --terminal-class` hook. Does it exist? If not, choose
  between (a) submitting an upstream PR adding the hook, (b) shipping a
  monkey-patch in the runner, or (c) forking the submodule (last resort).
- **Q-FX4.** Recording mode for OSS-on-test-VM. `node-sync` is desirable
  (recording finalised when the session ends, no UploadCompleter delay)
  but requires that the auth server is reachable from the node *during*
  the session. On a single-host setup that's free; spot-check.
- **Q-FX5.** Whether the in-container `teleport start` and the in-container
  asciinema collide on the same PTY. The Terminal-Bench harness already
  pipes through `asciinema rec` for its own per-task cast file. Teleport's
  recording happens at the auth-server side, independent of the in-container
  PTY emulator, so they should not collide — but verify by running one
  task end-to-end and inspecting both artifacts.
- **Q-FX6.** Whether the t-bench tests' `docker exec` post-step trips any
  Teleport audit code. Tests run as root inside the container outside the
  recorded session; they should not generate audit events on the cluster.
  Verify with `tctl events ls` after a single task.
- **Q-FX7.** Single-shot-cohort positives. The Terminal-Bench fixture, even
  after the persistent-PTY refactor, only produces PTY-cadence recordings
  — the agent drives tmux through one long shell. Phase-1's
  `routing.cohort=single-shot-non-pty` edge classifier therefore gets *no*
  labeled positive data from this pipeline. Real-world agents that wrap
  `tsh ssh node 'cmd'` per LLM turn (instead of opening a shell) belong
  in that cohort and need separate fixture data — likely a different
  harness entirely (e.g. an agent driving against a non-tmux target via
  one-shot exec). Decide whether to (a) build a sibling fixture for the
  single-shot cohort, (b) rely on live-tenant rows alone for that cohort
  and accept the lower confidence, or (c) defer the single-shot
  classifier until enough live data accumulates.

## Implementation status (2026-04-25)

MVP scaffold landed under `harnesses/terminal-bench-teleport/`. End-to-end
proven by manual smoke test: cluster up, base image built, smoke node
joins, `tsh ssh agent@<container>` from host succeeds, `<sid>.tar`
recording in ProtoStreamV1 format lands at
`cluster/data/log/records/`.

Two deviations from the design above worth recording:

- **One recording per harness call, not one per task.** The monkey-patch
  routes every `TmuxSession._exec_run` through its own `tsh ssh -t`
  invocation. Each is its own Teleport SSH session and produces its own
  `<sid>.tar`. A single Terminal-Bench task therefore produces *many*
  short recordings (one per tmux subcommand), not the single per-task
  recording originally targeted. The `fixture/run-id=<uuid>` Teleport
  node label is the join key for aggregating recordings produced by the
  same task; phase-1 features that need cross-recording cadence will
  have to be computed on the union, not the individual `<sid>.tar`.
  Persistent-PTY refactor (open one long-lived `tsh ssh` per task,
  multiplex send-keys/capture-pane through its stdin/stdout) is a
  follow-up.
- **PTY-only recording on OSS.** Non-PTY exec sessions emit
  `session.start` + `session.end` audit events but no recording bytes —
  OSS file backend with `node-sync` requires a `SessionPrint` stream.
  The patch forces `tsh ssh -t` everywhere so this isn't a blocker, but
  it bakes in a meaningful constraint: anything we do via `tsh exec`
  in future (k8s, db, app sessions) won't generate recordings on this
  fixture cluster without the same `-t` workaround. Documented in
  `harnesses/terminal-bench-teleport/README.md` under "Known gotchas".

## Out of scope for this file

- The actual phase-1 classifier rules (step 3).
- A long-running fixture service. This is a script, run on demand.
- Multi-user / multi-tenant fixture clusters.
- Fixture data for non-Terminal-Bench harnesses. Future harnesses get
  sibling docs in `harnesses/`.

## Cross-references

- [`harnesses/terminal-bench.md`](../harnesses/terminal-bench.md) for what
  Terminal-Bench is, how it's pinned, and the open experimental questions
  this plan starts to answer.
- [`notes/05-tap-points-for-detection.md`](05-tap-points-for-detection.md)
  for why option (c) above degenerates to "read local file" on an OSS
  cluster.
- [`notes/06-pipeline-design.md`](06-pipeline-design.md) for the SQLite
  schema, label conventions, and `teleport-analyze` CLI that this fixture
  pipeline feeds.
- [`notes/99-open-questions.md`](99-open-questions.md) for where Q-FX1
  through Q-FX6 should land if/when this plan is approved.
