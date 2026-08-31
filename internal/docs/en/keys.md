# Ingest keys

Every project accepts telemetry through a **DSN key** — a public string that
both names the project and authorizes sending data to it (see
[Glossary](/docs/glossary)). Each key has a **type**, which limits what
telemetry that particular key may send. Types exist because not all keys are
equally sensitive: a browser key is published on purpose — it sits in plain
text in the page's own code, visible to any visitor — and until now that same
key could also register a host or post a deployment marker. A leaked (and it
doesn't really "leak" — it's published by design) browser key let an outsider
fill a project's host fleet with fake machines up to the 1000-host ceiling,
or post a fake deployment marker.

This page is the one place that lists the full permission matrix; every other
page in the docs links back to it instead of repeating the table.

## Key types

A new project gets **three** keys right away — one per source class, with no
shared "main" key:

| Type | For | Allows |
|---|---|---|
| `browser` | browser SDKs (Sentry JS and similar) | events, transactions, metrics, logs |
| `server` | server SDKs, CI, application-metrics collector | same as `browser`, plus profiles and deployment markers |
| `agent` | host-metrics sources | metrics only; **the only type that can register a host** |
| `legacy` | keys issued before types existed (see below) | full access: everything `server` gets, plus host registration |
| *(untyped)* | — | nothing; this shouldn't occur, see below |

A key's type tracks how the data technically arrives, not who is sending it:
the difference between `server` and `agent` is not "server vs. agent", it's
**whether the source registers a host**. A key with no type set gets nothing —
that's a guard against a forgotten initialization, not a working mode: this
value shouldn't show up on a live key, only on an empty field somewhere in
code, by mistake.

## `agent` means any host-metrics source

The `agent` type is named after its main role, but it doesn't mean "only
Gotcha's own agent" — it means **any host-metrics source**. Both ways of
connecting a host from the [Hosts](/docs/hosts) page use an `agent` key:

- the built-in `gotcha-agent`;
- a third-party `otelcol-contrib` with the `hostmetrics` receiver and the
  `resourcedetection` processor — the config shown on the same Hosts page.

Both send metrics carrying the `host.name` resource attribute, and that's
exactly why both register a host — the line is drawn by whether
`resourcedetection` is in the config, not by "it's a collector" as such (the
[service monitoring recipes](/docs/recipes) also run a collector, but a
different one — see below).

## Why service recipes need `server`, not `agent`

[Monitoring recipes](/docs/recipes) (PostgreSQL, MariaDB, nginx, Redis,
Docker) also connect through `otelcol-contrib`, but their snippets
**deliberately don't set** `resourcedetection` — a recipe watches the service
itself, not the host it runs on, and it doesn't register a host and never
did. The DSN filled into a recipe's config is a `server`-type key. Handing a
recipe an `agent` key would grant host-registration rights to a source that
doesn't need them — exactly what the types exist to prevent.

## Which key for which source

| Source | Type |
|---|---|
| Sentry SDK in the browser | `browser` |
| Sentry SDK on the server (PHP, Go, Python, server-side Node) | `server` |
| Deployment markers from CI | `server` |
| The direct pprof endpoint | `server` |
| The `gotcha-agent` agent | `agent` |
| The collector from the Hosts page (`hostmetrics` + `resourcedetection`) | `agent` |
| The collector from a service monitoring recipe | `server` |

See each source's own page for details: [SDK & integrations](/docs/sdk),
[Deployments](/docs/deployments), [Profiling](/docs/profiling),
[Logs](/docs/logs), [Hosts](/docs/hosts), [Monitoring recipes](/docs/recipes).

## Untyped (`legacy`) keys

Keys issued before types existed keep working unchanged, with no time limit —
upgrading breaks nothing. In the project settings such a key is marked with
an **"Untyped"** badge — that's not a problem, just a fact: it keeps full
access indefinitely, and you're not required to replace it.

To split sources across types without any downtime:

1. issue new typed keys (`browser`/`server`/`agent`) in the project settings —
   the old key keeps working the whole time;
2. switch each source to the key of the type it needs (see the table above),
   one at a time, with no shared downtime window: until every source is
   switched, the old key keeps serving the rest;
3. once every source has moved, revoke the old untyped key in the project
   settings.

An untyped key can't be reissued through the UI — the key-creation form
always requires picking one of the three types.

## A key's type is fixed

An existing key's type can't be changed. The only way to give a source a
different set of permissions is to issue a new key of the right type and
revoke the old one (steps above). This is deliberate: a key's type is cached
together with the key for 30 seconds, and making the type unchangeable
removes the question of what to do with an already-cached grant.

## What a rejection looks like

A request made with the wrong type of key is rejected with **HTTP 403** (not
401 — the key is recognized and belongs to the project, it just isn't
permitted for this particular endpoint). If you run gotcha yourself, this
also shows up in observability — see [Monitoring gotcha
itself](/docs/self-monitoring):

- `gotcha_ingest_rejected_total{reason="key_scope",signal="…"}` — rejected by
  key type, labeled with the signal (`event`, `transaction`, `metric`,
  `log`, `profile`, `deploy`);
- `gotcha_ingest_key_rejections_total{reason="scope"}` — the same rejection at
  the key-authentication stage;
- `gotcha_host_registrations_scope_skipped_total` — a separate case: metrics
  arriving on a non-`agent` key carry `host.name` attributes. The export is
  still ACCEPTED and its data points are written — only the host registration
  is skipped;
- a log line naming the endpoint path the wrong key was used against — for a
  type rejection just like for any other key rejection.

A single request with mixed content (say, a browser SDK envelope that
happens to also carry a profile item) is filtered item by item: whichever
item the key isn't permitted to send gets dropped, and the rest of the
request is accepted as usual.
