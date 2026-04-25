# Prior Art Notes

This directory tracks concise factual analysis of third-party work. These
files are not mirrors of vendor documentation, papers, or blog posts. They
are local research notes that record what a source is, what it does, and how
it is documented.

See `AGENTS.md` for the binding rules — notably: no comparisons to this
project, no positioning, no gap analysis. Prior-art notes describe what
exists in the source. Nothing more.

## Layout

- One Markdown file per project, paper, product feature, or closely related
  source cluster.
- Use kebab-case filenames, e.g. `teleport-session-recording-summaries.md`.
- Put source links and access dates near the top of each note.
- Prefer primary sources: product docs, official blogs, papers, standards, or
  source repositories.

## Baseline Template

Each note should usually include:

- `Source`: canonical upstream URLs, with access date and source version if
  known.
- `What it is`: short factual description in our own words.
- `Relevant capabilities`: what the source can do, per its own documentation.
- `Requirements and operating model`: what has to be deployed, enabled, or
  configured.
- `Outputs and integration points`: artifacts, events, APIs, dashboards, logs,
  or data formats.
- `Limitations and risks`: limitations stated by the source.
- `Open questions`: claims the source does not pin down and that need
  follow-up verification against the source itself.

## Index of Notes

Append new entries to the end of this list as additional prior art is
analyzed. See `AGENTS.md` for the appending rule.

1. [asciinema and the asciicast file format](asciinema-asciicast-format.md) —
   Open-source terminal session recorder, player, and self-hostable server,
   plus the `asciicast` v1/v2/v3 file formats. Captures terminal output (and
   optionally input) under a PTY into newline-delimited JSON `.cast` files.
   Distributed as a single binary; the format and tooling are reused by
   third-party recorders such as Tailscale.

2. [Delinea Iris AI / AI-Driven Auditing (AIDA)](delinea-iris-aida-session-analysis.md) —
   Delinea Platform feature that analyzes recorded privileged SSH sessions and
   the PowerShell portion of RDP sessions using time-aligned OCR frames,
   keystroke logs, and process traces. Outputs a narrative summary, a
   timeline-aligned activity panel, an anomaly heat-map, and labels drawn from
   a fixed ~25-category vocabulary. Inference runs on Azure Computer Vision
   plus Azure OpenAI and is metered in pre-purchased "AI-Driven Auditing
   hours."

3. [PAM session monitoring vendors](pam-session-monitoring-vendors.md) —
   Consolidated note on CyberArk PSM/PSMP, BeyondTrust PRA/Password Safe,
   Delinea Secret Server (with Privileged Behavior Analytics), and
   ManageEngine PAM360. All four are recording-centric jump-host or appliance
   platforms with web replay, keyword/forensics search, and live shadowing.
   AI surfaces are uneven: only ManageEngine ships generative session
   summaries in the base product, and it does so by integrating with OpenAI.

4. [RACONTEUR shell command explainer (NDSS)](paper-raconteur-shell-explainer.md) —
   Research prototype that explains a single shell command or compound
   one-liner and maps the inferred behavior to MITRE ATT&CK tactics and
   techniques. Built on a fine-tuned 6B ChatGLM2 model plus two retrieval
   components (intent matcher and documentation retriever). Granularity is
   capped at one command; multi-command sessions are explicitly future work.

5. [Shell session anomaly detection research](shell-session-anomaly-detection-research.md) —
   Two anchor papers plus historical context. Liu and Buford (JPMorgan, 2023)
   train a from-scratch DistilBERT on enterprise shell sessions and detect
   anomalies via a PyOD ensemble (unsupervised) or SetFit (weakly supervised).
   PASTRAL (AWS, NeurIPS 2025 workshop) combines CodeBERT and Tree-sitter AST
   embeddings in a conditional VAE with Gaussian differential privacy added
   at the user side.

6. [Tailscale SSH and Kubernetes session recording](tailscale-ssh-session-recording.md) —
   Optional layer over Tailscale SSH and the Kubernetes Operator's API server
   proxy that streams matched sessions to one or more `tsrecorder` nodes
   storing `asciicast` v2 files on disk or S3-compatible object storage.
   Output-only capture (`stdin` is excluded by design); ACL-driven opt-in
   with `enforceRecorder` controlling fail-open vs fail-closed. No native
   LLM analysis layer; the product stops at record-and-replay-with-audit.

7. [Teleport session recording summaries](teleport-session-recording-summaries.md) —
   Teleport Identity Security feature that uses LLM inference to summarize
   recorded SSH, Kubernetes, and database sessions after they end. Driven by
   `inference_model`, `inference_secret`, and `inference_policy` resources;
   supports OpenAI, OpenAI-compatible gateways, Amazon Bedrock, and a
   Teleport-managed Bedrock model on Cloud. Emits a `session.summarized`
   audit event and surfaces the summary in the recording UI.
