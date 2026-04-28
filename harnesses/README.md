# Harnesses

This directory holds third-party benchmarks and harnesses that produce
realistic terminal / shell / agent activity. Their value to this project is
as reproducible **fixtures** — sources of labeled, replayable sessions we can
drive through Teleport-managed nodes to exercise downstream detection and
classification work (the step 2 / step 3 components sketched in
[`notes/00-step1-plan.md`](../notes/00-step1-plan.md) and
[`notes/06-pipeline-design.md`](../notes/06-pipeline-design.md)).

## How this differs from the other top-level directories

| Directory      | Purpose                                                                |
|----------------|------------------------------------------------------------------------|
| `upstream-repo/` | Pinned, read-only copy of the system being studied (Teleport v17). |
| `prior-art/`     | Factual notes on third-party work. Bound by `prior-art/AGENTS.md` — must not mention this project, position our work, or speculate about gaps. |
| `harnesses/`     | Third-party tools we expect to **run**. Companion notes here *may* reference this project's goals and open experimental questions, because that framing is the reason a harness lives in the repo at all. |

## Conventions

- One submodule per harness, declared `shallow = true` in `.gitmodules`,
  mirroring the `upstream-repo/` pattern.
- Each submodule gets a sibling `<harness>.md` writeup at the top of this
  directory. The writeup records: source URLs, pinned commit and what version
  it corresponds to, what the harness produces, how to run it locally, and
  the open experimental angles it opens up for this project.
- File names are lowercase kebab-case (e.g. `terminal-bench.md`).
- No absolute filesystem paths in any committed file; use repo-relative paths
  (`harnesses/terminal-bench/...`, `notes/05-tap-points-for-detection.md`,
  etc.).
- Do not commit copies of upstream documentation, blog posts, or papers.
  Link to primary sources; paraphrase in our own words.
- Step-2 / step-3 design work belongs in `notes/`, not here. Writeups in
  this directory may *pose* experimental questions about a harness, but they
  should not contain commitments, schedules, or design decisions.

## Index

| Harness | Submodule | Writeup |
|---------|-----------|---------|
| Terminal-Bench (Stanford / Laude Institute) | [`terminal-bench/`](./terminal-bench) | [`terminal-bench.md`](./terminal-bench.md) |
