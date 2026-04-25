# 99 — Open questions

Things this round of doc-reading couldn't answer with confidence.
Each item has a way to verify (against the live tenant or further
reading). Carry these forward into step 2 and update the notes when
answers come in.

## Tenant-state questions (need live `<org-id>` access)

### Q1. What's the exact aggregated sink configuration?
Plausibly an FFF-default sink to BigQuery + GCS + Pub/Sub, but the
specifics (dataset name, bucket name, topic name, exclusion
filters) are tenant-specific. Affects every other section that
uses placeholders.

```bash
gcloud logging sinks list --organization=<org-id>
gcloud logging sinks describe <sink-name> --organization=<org-id>
```

### Q2. Which Data Access streams are on, at what level?
FFF defaults turn on Data Access for KMS, Secret Manager, IAM,
IAMCredentials. Some variants add Storage and Cloud Functions.
Affects which methodNames the classifier sees and which the audit
log silently drops.

```bash
gcloud organizations get-iam-policy <org-id> --format=json \
  | jq '.auditConfigs'
```

### Q3. What's the BigQuery dataset retention?
Default is unbounded; FFF often applies table-level expiration.
Affects how far back the classifier can backfill.

```bash
bq show --project_id=<logging-project> <bq-dataset> \
  | grep -i 'expiration\|partition'
```

### Q4. Does the GCS archive bucket have lifecycle rules?
Standard → Nearline → Archive transitions affect retrieval cost
and SLA for backfill queries.

```bash
gsutil lifecycle get gs://<gcs-archive-bucket>
```

### Q5. Is Chronicle / Google SecOps deployed?
If yes, it's a fifth tap option (`05` (e)). If no, ignore that
section.

```bash
# No clean gcloud command; check the Chronicle console or:
gcloud asset search-all-resources \
  --scope=organizations/<org-id> \
  --asset-types=chronicle.googleapis.com/Instance
```

### Q6. Is Workforce Identity Federation in use?
Affects how many `principalEmail` shapes the classifier sees. If
yes, external-IdP-mapped users have `principal://...` URNs, not
`<email>@<domain>` strings.

```bash
gcloud iam workforce-pools list --location=global \
  --organization=<org-id>
```

### Q7. Is OS Login required org-wide?
If `constraints/compute.requireOsLogin` is enforced, every SSH
session to Compute is GCP-principal-tagged. If not, some hosts
allow SSH-key login that bypasses OS Login and produces no
GCP-side principal attribution.

```bash
gcloud resource-manager org-policies describe \
  constraints/compute.requireOsLogin --organization=<org-id>
```

### Q8. What VPC SC perimeters protect the logging project?
Affects whether the classifier's project can read BigQuery /
Pub/Sub without a perimeter exception.

```bash
gcloud access-context-manager perimeters list \
  --policy=$(gcloud access-context-manager policies list \
    --organization=<org-id> --format='value(name)' | head -1)
```

### Q9. Is BeyondCorp Enterprise / Context-Aware Access deployed?
If yes, device-posture and access-context decisions are logged
and provide the strongest "is this a managed device" signal. If
no, this enrichment isn't available for `06`'s phase-2.

```bash
gcloud access-context-manager levels list \
  --policy=$(gcloud access-context-manager policies list \
    --organization=<org-id> --format='value(name)' | head -1)
```

## Doc-side / source-side questions (need code reading, not live access)

### Q10. Exact BigQuery export schema for the live tenant
The schema published in product docs is the canonical reference,
but new audit-log fields may appear in newly-emitted entries
before the docs catch up. Run `bq show --schema` against the live
table and diff against `04`'s documented schema.

```bash
bq show --schema --format=prettyjson --project_id=<logging-project> \
  <bq-dataset>.cloudaudit_googleapis_com_activity \
  > /tmp/live-schema.json
# diff /tmp/live-schema.json against the schema in notes-gcp/04
```

### Q11. APPROX_TOP_COUNT semantics inside per-minute aggregations
The phase-1 query in `06` uses `APPROX_TOP_COUNT` on an
already-grouped expression. Verify this gives sensible results vs
explicit `ARRAY_AGG(... LIMIT 5)` for small per-minute populations.

### Q12. Pub/Sub message dedupe by insertId
At-least-once delivery means duplicates are normal. Confirm
`insertId` is included as a Pub/Sub message attribute (it should
be, by spec) and is unique per LogEntry.

### Q13. Chronicle UDM mapping for `serviceAccountDelegationInfo`
If Chronicle is in scope, verify which UDM field carries the
impersonation chain. Documented mapping has historically gone
into `additional.fields.serviceAccountDelegationInfo`, which is a
JSON-as-string blob.

### Q14. callerSuppliedUserAgent reliability for Workforce Federation
When access goes through an external IdP via Workforce Federation,
does the callerSuppliedUserAgent still reflect the actual client's
UA string, or does the federation layer rewrite it? Test against
a known WIF principal.

## Behaviour questions (need a small experiment)

### Q15. End-to-end emit-to-BigQuery latency in this tenant
Run a known-shape benign event (e.g. `gcloud compute instances
list --project=<sandbox>`), then time how long until it shows up
in the BQ table. Confirms (or refutes) the "1-3 minute" estimate.

```sql
SELECT MAX(timestamp)
FROM `<logging-project>.<bq-dataset>.cloudaudit_googleapis_com_activity`
WHERE timestamp > TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 5 MINUTE);
```

**Resolved-by-design (2026-04-25, see `06-pipeline-design.md`).**
The step-2 design tolerates a 1-3 minute lag for batch features.
Calibration is still nice to have but doesn't block step 3.

### Q16. Pub/Sub end-to-end latency
Same as Q15 but measured through the Pub/Sub subscription. Should
be sub-minute. Only matters if step 3 adds a real-time tap.

### Q17. Synthetic session boundaries — appropriate idle threshold
`06`'s heuristic is `idle_threshold_seconds=600`. For a sample of
known human admin sessions, measure the actual gap distribution
and calibrate.

**Resolved-by-design (2026-04-25, see `06-pipeline-design.md`).**
Exposed as a CLI flag with a 600 s default; recalibrate against
real data later without code changes.

## Cross-substrate questions

### Q18. Identity mapping between Teleport and GCP
For users who appear on both substrates: does the Teleport `user`
(an `@<your-domain>` email) match the GCP `principalEmail`
exactly? Or is there a translation step (e.g. Teleport user maps
to `<user>+teleport@<your-domain>` SA)?

(Ask the user; not derivable from either side's docs.)

### Q19. Time-window correlation
What window catches "the same operator action on Teleport and on
GCP"? A user might `tsh ssh` to a bastion and then `gcloud auth
login` from inside that session — events on both substrates
within seconds of each other. Calibrate against a known sample.

## Things explicitly *not* worth running down right now

(Recorded so we don't keep tripping over them.)

- Whether GCP audit logs include the bytes of an IAP-tunneled SSH
  session. They do not — IAP is a TCP proxy and sees only
  encrypted bytes.
- Whether GKE `exec` audit logs include stdin/stdout. They do
  not — the kube-apiserver hijacks the connection at the upgrade
  handshake.
- Whether `callerSuppliedUserAgent` can be trusted as
  authoritative. It cannot — it's client-controlled and trivially
  spoofable. Treat as strong but not authoritative signal.
- Whether the BigQuery export has at-least-once delivery
  semantics. Cloud Logging's docs are explicit: BQ export is
  best-effort with possible duplicates; the classifier should
  dedupe by `insertId` if exact counts matter.

## How to use this list

Step 2 will resolve Q1-Q9 against the live tenant — they're
cheap and replace the `<org-id>` / `<bq-dataset>` placeholders
that the design and the classifier rules will reference. Q10-Q14
are doc-side checks to answer when a specific step-3 design
depends on them. Q15-Q17 are calibration with two already
resolved-by-design. Q18-Q19 unlock cross-substrate fusion in
step 3.
