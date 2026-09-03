# Versioning policy

Before 1.0, gotcha changes the contract between releases freely — the practical
steps for that live in [Upgrade](/docs/upgrade), release by release. That
stops at 1.0: part of the contract gets frozen, and any breaking change to it
means a major release, not a patch. This page describes what is frozen, what
isn't, and what to do in the meantime if you already integrate with gotcha.

## What the compatibility promise covers

Starting at 1.0, everything below only changes in a backward-compatible way,
or with a major version bump:

| What | What's actually promised |
|---|---|
| Environment variables | variable names and the shape of their values (what a value is allowed to be, not today's default — defaults can still change) |
| Ingest paths and body formats | `/api/v1/*` and the other current ingest paths, and the request body format on each |
| The migration schema | migrations are forward-compatible — a database on N reaches N+1 without manual intervention |
| The backup format | a backup taken today restores on a newer version |
| Self-metric names | the names and labels of the metrics documented in [Monitoring gotcha itself](/docs/self-monitoring), except the ones explicitly marked temporary |
| The outgoing webhook body | [the body shape is frozen](/docs/self-monitoring); adding a new field is not a breaking change — parse it tolerant of fields you don't recognize |
| The `GOTCHA_AGENT_*` contract | the agent's environment variables and the protocol it speaks to the server |
| Addresses of existing status pages | a status page's published URL does not change on its own |

## What it doesn't cover

Everything below can change in any release, including a patch, without being
called out as breaking in the CHANGELOG:

- internal Go packages (`internal/...`) — not a public API, gotcha isn't a
  library;
- the shape and URLs of the web UI's pages;
- the PostgreSQL and ClickHouse table schema — integrating by querying the
  database directly is never supported, only through ingest paths and the API;
- the set and order of columns in exports (CSV/JSON from the Exports section);
- the order and exact wording of gotcha's own log messages.

If you need stability in one of these areas, build it on something that is
promised (a self-metric, an ingest path) instead of parsing a log line or
running a raw `SELECT` against the database.

## What counts as breaking

For anything listed under "what the promise covers," this counts as breaking:

- removing or renaming a covered element (a variable name, an ingest path, a
  self-metric, a field of the agent protocol);
- narrowing the set of accepted values (something that used to be accepted is
  now rejected);
- changing what a value means while keeping the same name (the same config
  key now means something else);
- removing an ingest path.

Extension — a new optional body field, a new environment variable, a new
optional parameter — is not breaking and does not require a major version.

## Downgrade is not supported

The promise runs one way only: a newer version understands data and
configuration left behind by an older one. The reverse isn't guaranteed, and
rolling back to a previous release isn't a safe "just in case" move — it's an
action with a real risk of silently corrupting data.

The precedent is the secretbox key rotation in v0.25.0. Secrets in the
database moved to an `enc:v2:<key-id>:...` envelope; the version that started
writing envelopes understands both the old format and the new one. But a
version from before that migration doesn't know the envelope format at
all — it reads `enc:v2:...` as a plain string and hands it back as if it were
the decrypted secret, silently, with no error. Rolling back to a
pre-migration binary after a rotation has run on the instance breaks secrets
quietly.

The practical rule that follows: **back up before you upgrade** (see
[Backup & Restore](/docs/backup-restore)), and if a release doesn't work out,
restore from that backup — don't roll the binary back on a live database.

## Deprecation procedure

Deprecation takes two shapes — one for ingest paths, one for environment
variables — and both guarantee at least one major release of life between the
deprecation announcement and removal.

**An ingest path.** The old path keeps working, but:

- responses on it carry a `Deprecation` header and a `Link; rel="deprecation"`
  header (RFC 9745) pointing at the current path;
- every such request is counted by the
  `gotcha_ingest_deprecated_path_total{path="…"}` self-metric — see
  [Monitoring gotcha itself](/docs/self-monitoring);
- the path is removed no sooner than the next major version.

**An environment variable.** A rename doesn't leave the old name silently
working — that would let an operator with a stale `.env` get quietly swapped
onto a default value instead of the setting they meant. Instead, a process
that sees the old name **refuses to start** and names the new one. The list
of renames and the details live in [Upgrade](/docs/upgrade); dropping an old
name from the rename map also happens no sooner than the next major version.

## Timeline for what's deprecated today

As of 1.0, two deprecations are open in the contract:

| What's deprecated | When it's removed |
|---|---|
| Three ingest aliases — `/logs`, `/profiles/pprof`, `/api/{project}/deployments/` (replaced by `/api/v1/logs`, `/api/v1/profiles/pprof`, `/api/v1/{project}/deployments`) | at 1.0 |
| The renamed-environment-variable registry (startup refusal on an old name; full list in [Upgrade](/docs/upgrade)) | lives until 2.0 |

For the three ingest aliases, the `gotcha_ingest_deprecated_path_total{path="…"}`
counter in [Monitoring gotcha itself](/docs/self-monitoring) shows whether
anything is still hitting the old path — a non-zero rate after upgrading is
your signal to move that sender to the new path before 1.0.

## OIDC's extension path

Today gotcha supports exactly one generic OIDC provider per instance — the
`GOTCHA_OIDC_*` variables (see [Configuration](/docs/configuration)). When
support for a second provider alongside the first arrives, it will come as a
separate, named variable namespace, not as a change in what the existing
`GOTCHA_OIDC_*` variables mean — a second provider won't break the
configuration of anyone who already set up the first.
