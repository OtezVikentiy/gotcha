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

## Who can export

Any project operator can create jobs, and download or delete their own — the
same access level as alert settings and suppression. Downloading
**re-checks project access at download time**, not just at creation: access
may have been revoked later — a foreign or inaccessible job returns `404`, not
`403`.

Deleting someone else's job requires an org admin or owner; deletion is only
available for finished jobs (done, failed, expired) — a job still in progress
has no delete button, just a note that it's still running.

## PII: masked by default

`user_email` and `user_ip` are replaced with `[masked]` by default; `user_id`
is left as-is. In JSON/NDJSON the same mask applies to keys in `request` and
`contexts` that look like personal data.

The "export as-is" checkbox on the creation form is only shown to org admins
and owners — a project operator without that role never sees it, and a value
submitted anyway is silently ignored (the job is still queued masked, not
rejected). Whether the checkbox was used is visible in the job list to anyone
who can see the job itself.

If server-side scrubbing is enabled on ingest (`GOTCHA_SCRUB_EMAIL`/
`GOTCHA_SCRUB_IP`, see [Privacy](/docs/privacy)), `user_email`/`user_ip` are
already blanked in storage at ingest time. The "as-is" checkbox doesn't
resurrect what was never saved — if the fields are already empty, unmasking on
export changes nothing.

## Limits

- 3 active jobs (queued or currently building) per user, 10 per project at a
  time — a new job over that limit is rejected.
- A row cap (`GOTCHA_EXPORT_MAX_ROWS`, default 200,000) and a file size cap
  (`GOTCHA_EXPORT_MAX_BYTES`, default 256 MiB): hitting either stops the build
  and marks the job "truncated" — shown as an explicit note next to the status
  on the page, while the file is still available to download.

## Retention and cleanup

A finished file and its job row live for `GOTCHA_EXPORT_TTL_HOURS` hours from
completion (168 by default — seven days), then a background janitor removes
both the file and the row without a trace. If mail is configured (see
[Configuration](/docs/configuration)), the author gets an email when the job
finishes or fails — download before the deadline.

## Single-instance limitation

Export files live on the disk of **whichever process** built them
(`GOTCHA_EXPORT_DIR`). This feature is designed for a single-instance
deployment: with multiple application replicas behind a load balancer, a
download request can land on a replica that doesn't physically have the file.
Scaling this feature horizontally would require shared storage (a network
volume, S3-compatible storage, etc.) — that isn't implemented today.
