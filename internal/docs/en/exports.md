# Exports

A job for a file listing a project's error groups or raw events — CSV, JSON, or
NDJSON — built in the background and ready to download from the
`/projects/{id}/exports` page.

## What gets exported

Two job kinds:

- **Issue groups** — the same table as the [Issues](/docs/issues) list: title,
  culprit, level, status, times seen, first/last seen, environments, assignee,
  a link back to the group.
- **Events** — raw events, either from one selected group or the whole project:
  timestamp, event id and group id, level, message, exception type and value,
  environment, release, server name, SDK, `trace_id`, user id/IP/email, tags.
  CSV carries only these columns (the full object would turn the table into an
  unreadable wall of data); JSON/NDJSON carry the complete event, including
  stacktrace, contexts, breadcrumbs, and `request`.

A job is queued with the same time range and environment filter selected on the
page at the moment it's created; the range is frozen into the job, so changing
the page's time selector afterward doesn't affect an already-queued job.

Row order is part of the file's contract, not an accident: groups are sorted
by last seen, newest first, and groups with the SAME last-seen time break the
tie by descending id (the most recently created one of them comes first). A
program processing the file can rely on this — the rule is frozen and won't
change silently.

## Formats

| Format | What it is | When to use it |
|---|---|---|
| CSV | Tabular file, comma-separated, UTF-8 with BOM | Open in Excel/spreadsheets, quick look |
| JSON | A single array of objects | Import into another system, programmatic processing |
| NDJSON | One JSON object per line | Streaming a large file without loading it whole into memory |

**If Excel didn't split the CSV into columns.** The file is a valid comma-
separated UTF-8 CSV; on a locale where Excel expects a semicolon (common on
localized Windows), double-clicking the file can dump everything into a single
column. Instead of double-clicking, open it via **Data → From Text/CSV** (or
**File → Import**) and explicitly pick comma as the delimiter — that splits the
columns correctly regardless of Excel's locale.

The format is chosen explicitly at each of the four places a job can be
created: on the Exports page itself, and on the compact collapsible forms —
"Export groups"/"Export events" on the [Issues](/docs/issues) list, and
"Export issue events" on a single issue's page. All four share the same
CSV/JSON/NDJSON dropdown and, where available, the "Export PII unmasked"
checkbox described below.

## Who can export

Any project operator can create jobs, and download or delete their own — the
same access level as alert settings and suppression. Downloading
**re-checks project access at download time**, not just at creation: access
may have been revoked later — a foreign or inaccessible job returns `404`, not
`403`.

In the Exports page list, an org admin or owner sees ALL of the project's
jobs — their own and everyone else's; an operator without that role sees only
their own. The list shows the 50 most recent jobs and doesn't paginate —
older jobs (or, for a plain operator, other people's jobs) simply don't
appear in it.

Deleting someone else's job requires an org admin or owner; deletion is only
available for finished jobs (done, failed, expired) — a job still in progress
has no delete button, just a note that it's still running. Deletion itself is
a two-step action: the button leads to a confirmation page, and the
irreversible delete only happens after confirming there.

## PII: masked by default

`user_email` and `user_ip` are replaced with `[masked]` by default; `user_id`
is replaced with a pseudonym (a random string, stable only within a single
file — see "`user_id` pseudonyms aren't comparable across exports" below),
not left as-is and not replaced with the same static mask as email/IP: unlike
`[masked]`, a pseudonym still lets you count how many DIFFERENT users were
affected without exposing any original value. In JSON/NDJSON the same mask or
scrub applies to `request`, `contexts`, `stacktrace`, and `breadcrumbs` keys
that look like personal data (frame-local variables, URLs with query
parameters).

The "Export PII unmasked" checkbox on the creation form is only shown to org
admins and owners — a project operator without that role never sees it, and a
value submitted anyway is silently ignored (the job is still queued masked,
not rejected). With the checkbox on, `user_id` is also left as-is, with no
pseudonymization. Whether the checkbox was used is visible in the job list to
anyone who can see the job itself.

If server-side scrubbing is enabled on ingest (`GOTCHA_SCRUB_EMAIL`/
`GOTCHA_SCRUB_IP`, see [Privacy](/docs/privacy)), `user_email`/`user_ip` are
already blanked in storage at ingest time. The "Export PII unmasked" checkbox
doesn't resurrect what was never saved — if the fields are already empty,
unmasking on export changes nothing.

In the group export (`kind=issues`), the same checkbox and the same
`[masked]` mask govern the `assignee_email` column — the email of the user
assigned to the group: a direct user identifier, not a property of the group
itself. An empty column (an unassigned group) is not replaced with the mask,
same as `user_email`/`user_ip` above.

### `user_id` pseudonyms aren't comparable across exports

The `user_id` pseudonym is built from a random, one-time key generated fresh
for EACH job and never stored anywhere. As a result: within ONE file, the
same `user_id` always yields the same pseudonym (so you can count unique
users of an event or a group), but across TWO different files — even two jobs
created back to back with the same filter — the same real user gets DIFFERENT
pseudonyms. You cannot stitch two exports together by `user_id`, and that's
deliberate, not a bug: the opposite would correlate one user's activity
across files, which nobody asked for.

The note isn't only here. Export files (CSV/JSON/NDJSON) stay CLEAN: CSV
carries only the column row and data rows with no comment lines at all, JSON
is only an array of records, NDJSON is only homogeneous lines — none of the
three formats carries a service element that a reader would have to tell
apart from data. The machine-readable facts about a job — `scope_issue_id`,
`filter_code`, and, for an unmasked-PII-free events job, `pseudonym_note` —
live NEXT TO the file, delivered three ways:

- **A sibling response on the same download**: `GET .../exports/{jobID}/download?meta=1`
  returns these fields as JSON under the same access gates as the file
  itself — a recipient who only needs the identifier doesn't have to parse a
  translated phrase like "issue #123" out of the human-readable summary;
- **On the Exports page**: the "Filters" column's cell carries
  `data-scope-issue-id`/`data-filter-code`/`data-pseudonym-masked`
  attributes — the same values sitting next to the localized text, readable
  by a script without parsing it;
- **In the readiness email**: a non-localized `gotcha-export-meta: job_id=…
  scope_issue_id=… filter_code=…` line, and the pseudonym note itself when it
  applies, are appended to the body separately from the translated sentence
  above.

## Limits

- 3 active jobs (queued or currently building) per user, 10 per project at a
  time — a new job over that limit is rejected. The per-user limit is shared
  across ALL of that user's projects, not counted separately per project.
- A row cap (`GOTCHA_EXPORT_MAX_ROWS`, default 200,000) and a file size cap
  (`GOTCHA_EXPORT_MAX_BYTES`, default 256 MiB): hitting either stops the build
  and marks the job "truncated" — shown as an explicit note next to the status
  on the page, while the file is still available to download.
- The export directory's overall disk budget (`GOTCHA_EXPORT_DISK_BUDGET_BYTES`,
  5 GiB per instance by default, no per-project quota) and the real free space
  on the filesystem holding `GOTCHA_EXPORT_DIR` — both reserve room for the
  CURRENT job's MaxBytes, not just for files already built. Either refusal is
  TEMPORARY (the job goes back to the queue, up to 3 attempts): running out of
  space self-heals the moment the janitor's next pass frees disk by removing
  expired files. No action is needed from the user; if the job still ends up
  "failed" — all three attempts ran out before disk freed up — free up space
  by hand, or raise `GOTCHA_EXPORT_DISK_BUDGET_BYTES` (see
  [Configuration](/docs/configuration)), and create the job again — a failed
  job doesn't restart itself.
- Exporting **events** without picking one specific group (`kind=events`,
  whole project with a filter) — if the filter resolves to more than 20,000
  issue groups, the refusal is **permanent** (unlike running out of disk
  space, there's no retry): narrow the time range, environment, or search and
  create the job again. The cap is a product constant, not exposed as an
  environment variable. Exporting **issue groups**, and exporting events of a
  single already-known group (from that issue's own page), are unaffected by
  this cap.

## Retention and cleanup

A finished file lives for `GOTCHA_EXPORT_RETENTION_HOURS` hours from completion (168
by default — seven days): once that expires, the janitor removes the file
from disk and marks the job "expired". The job's row in the history outlives
the file — it's kept for at least 30 days from completion, or exactly
`GOTCHA_EXPORT_RETENTION_HOURS` if that's set above 30 days (the row's retention
grows with the file's TTL but never falls below it), and only then does the
janitor remove it from the history without a trace. The author gets exactly
one email per job — when it turns "done" or "failed" (if mail is configured,
see [Configuration](/docs/configuration)); there's no separate reminder email
about the file's expiry. The failure reason doesn't require checking that
email — a job with status "failed" also shows it right in its row on the
Exports page. The status on the Exports page doesn't refresh
itself while a job is still queued or building — the email or a page reload
are the only ways to find out it's ready; download the file before its own
deadline, it won't come back after that.

Export files and the `GOTCHA_EXPORT_DIR` directory itself are the only place
in the product where personal data (user email/IP, contexts, request) lands
on disk — permissions are narrowed to the process owner (file `0600`,
directory `0700`). This only applies to a NEWLY created directory —
`MkdirAll` doesn't change the permissions of one that already exists, so an
instance upgraded from before this change keeps the old permissions until a
manual `chmod 0700`.

## Single-instance limitation

Export files live on the disk of **whichever process** built them
(`GOTCHA_EXPORT_DIR`). This feature is designed for a single-instance
deployment: with multiple application replicas behind a load balancer, a
download request can land on a replica that doesn't physically have the file.
Scaling this feature horizontally would require shared storage (a network
volume, S3-compatible storage, etc.) — that isn't implemented today.

## Process restart

A job the worker was actively building when the process stops normally
(restart, redeploy, `docker compose up -d`) doesn't stay stuck "running"
and isn't treated as a failure: it goes back to the queue without spending
one of its three attempts, and gets rebuilt from scratch on the next start —
whatever partial file existed at the moment of the stop is discarded. No
failure email goes out in this case: from the job's point of view nothing
broke, the build just resumes a minute later. Jobs that were still waiting in
the queue when the process stopped aren't affected by this at all — they were
already waiting their turn.

The export worker and janitor only run in processes started with `--mode=web`
or `--mode=all` — the same processes that serve downloads. With a split
`--mode=ingest`/`--mode=web` deployment (see [Upgrade](/docs/upgrade)), this
removes the most dangerous scenario (a job getting built on a replica with no
export routes at all), but it doesn't remove the single-instance limitation
entirely: if the `web` process itself runs as MULTIPLE replicas behind a load
balancer, each replica's janitor still only sees its own local disk. If an
expired job was built by a neighboring `web` replica, the current replica's
janitor won't find the file (`ENOENT`) and still marks the job "expired" —
the actual file on the other replica's disk isn't removed by this and stays
there until the job's history row is purged (at least 30 days from
completion), eating into that replica's `GOTCHA_EXPORT_DISK_BUDGET_BYTES` the
whole time.
