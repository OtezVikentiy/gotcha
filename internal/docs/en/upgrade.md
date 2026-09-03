# Upgrade

## Before you start: take a backup

An upgrade applies database schema migrations — this is not reversible automatically (nobody runs a "just in case" down-migration for you). Before upgrading, back up both PostgreSQL and ClickHouse — see [Backup & Restore](/docs/backup-restore). Don't skip this even if the upgrade looks minor.


## What changes when upgrading from versions before 0.4.2: details no longer reach everyone

Whether an alert carried the error text used to be decided by channel type: Telegram and webhooks counted as external, email as internal — so any mailbox received the full text. It is now decided per recipient: trusted means an address on your instance host, on a domain listed in `GOTCHA_TRUSTED_RECIPIENTS`, or on an internal network.

In practice:

- **mail on a public service** (`@gmail.com`, `@yandex.ru`, and the like) no longer receives the error text — only a link to the issue in the interface;
- **mail on your organization's domain** stops receiving details too, if that domain differs from the instance host, until you list it in `GOTCHA_TRUSTED_RECIPIENTS`;
- **a webhook pointed at your internal infrastructure** now does receive details — previously that required turning `GOTCHA_EXTERNAL_CHANNEL_DETAILS_ENABLED` on globally, which opened up Telegram as well.

If your mail lives on an organization domain, add it before upgrading:

```
GOTCHA_TRUSTED_RECIPIENTS=corp.example
```

The startup log confirms what took effect: the line `alert details: sent only to trusted recipients` lists the active set. Details are in [Privacy and 152-FZ](/docs/privacy).

## One-off step when upgrading from versions before 0.4.2: rotate channel secrets

Before this version a delivery channel's secret (Telegram bot token, webhook signing key) was written into the `notification_outbox` queue **in the clear** — the `payload` column is a plain `jsonb`, which made the encryption of `alert_channels.secret` pointless. Migration `0025` strips those values from the queue during the upgrade, and the new code no longer puts them there: the secret is fetched by channel id at send time.

The migration only cleans the **live database**, though. If the instance ran an older version, those secrets sit in the clear in **every PostgreSQL backup taken before the upgrade** — and you are, we hope, taking backups regularly. So after upgrading:

1. Rotate the Telegram bot tokens Gotcha uses (`/revoke` in @BotFather, then put the new token in the channel).
2. Change the webhook signing keys on the receiving side and in the channel.
3. Delete the old backups if you no longer need them; if you do need them, store them at the same protection level as secrets.

You can skip this only if you had no delivery channels at all.

## What changes when upgrading from versions before 0.4.2: excluded members lose team access they had kept

Removing someone from an organization used to leave their team memberships untouched, so they kept access to the projects of any team they belonged to — even after they were no longer a member of the organization itself. Migration `0029` fixes this in the schema: a team membership can no longer exist without the matching organization membership.

Applying the migration does two things:

- it deletes any team memberships that had already gone stale this way (someone no longer in the organization but still listed on one of its teams) — the migration log reports how many rows it removed;
- from then on the database enforces the rule itself: removing an organization member cascades to their team memberships automatically, so the same drift can't build up again.

If anyone in your organization was removed in the past but kept reaching a project through a team, that access goes away after this upgrade. That's the fix working as intended, but it can look like a regression if nobody saw it coming — worth checking who currently has team access before you upgrade, or being ready to explain the change afterwards.

This migration is marked `backward-compatible: no`. Once it has run, the schema gate refuses to start the previous version's binary against the database (see "Rolling back" below) — for this one, the usual rollback path isn't available, only restoring a backup taken beforehand.

Invitation links issued before the upgrade are unaffected; their tokens keep working exactly as before.

## What changes when upgrading from versions before 0.4.2: regression card durations were shown a thousand times too high

Duration values written to a performance regression before this version were stored in microseconds while every reader — the regression cards, the alert emails, webhooks, and Telegram — treated the number as milliseconds, so a real 200 ms endpoint showed up as 200 seconds. Web-vital metrics (`lcp`, `fcp`, `ttfb`, `inp`) were unaffected; only `duration` rows carried the wrong unit. The same confusion also meant the absolute floor that suppresses false alarms on small values never engaged, since a genuinely small regression still looked huge.

Migration `0030` divides every stored `duration` value by 1000 to bring existing rows in line with the unit the rest of the system now uses. No manual action is needed — the recompute runs automatically as part of the upgrade, exactly like any other migration. Rolling back re-multiplies the same rows, returning them to their previous numbers to the precision floating-point arithmetic allows (values that were an exact multiple of 1000 round-trip exactly; others may differ in a distant decimal digit) — there is nothing to reconcile by hand either way.

## What changes when upgrading from versions before 0.4.2: this upgrade's new indexes build without locking — but check them after upgrading

This upgrade adds twenty-four new indexes across several tables, in three batches: six for the hourly entity cleanup (the janitor, `internal/telemetry/entity_janitor.go`, purges `issues`, `perf_issues`, `incidents`, `perf_regressions`, `profile_regressions`, and `metric_incidents` by last-seen or closed time — none of these tables had an index that matched such a filter, so every pass was a full table scan); sixteen for foreign keys that previously had no index on the referencing side (slows down cascading deletes and joins through them); and two for substring search in the issue list (GIN trigram indexes on `issues.title`/`issues.culprit`).

That many new indexes on disk isn't free — expect a noticeable increase in disk usage, especially from the two GIN trigram indexes: indexes of this kind on text columns typically take up a sizeable fraction of the column's own size. How much your volume actually grows depends on your data volume; we haven't measured a specific figure — after upgrading, keep an eye on the free-space metrics (`gotcha_storage_free_bytes`/`gotcha_storage_total_bytes`/`gotcha_storage_used_bytes`, see [Monitoring gotcha itself](/docs/self-monitoring)).

All twenty-four indexes are built with `CREATE INDEX CONCURRENTLY` — a deliberate choice: a plain `CREATE INDEX` holds the table locked against writes for the entire build, and on a busy table that would have stalled event ingestion for a noticeable stretch. `CONCURRENTLY` avoids that lock, at the cost of one operational quirk.

**If the build is interrupted** (a dropped database connection, an out-of-memory kill, a manual cancel, the process restarting mid-migration) — PostgreSQL does not remove the unfinished index; it leaves it in the catalog marked invalid. On the next run of the same migration, `CREATE INDEX CONCURRENTLY IF NOT EXISTS` sees an object with that name already there and **silently skips creating it** — it does not repair an invalid index. The migration reports success, the schema gate stays green, and the working index is simply not there: the planner ignores invalid indexes, and whatever problem that particular index exists to fix stays unfixed with nothing pointing at it.

So after upgrading — regardless of how uneventful it looked — check:

```sql
SELECT indexrelid::regclass, indisvalid FROM pg_index WHERE NOT indisvalid;
```

If the list isn't empty — **any of this upgrade's twenty-four indexes** showing up there had its build interrupted (not just the six janitor indexes — the same goes for the foreign-key and search indexes added by this same upgrade; the list is deliberately not spelled out by name here so it doesn't go stale on the next migration). Fix it:

```sql
DROP INDEX CONCURRENTLY <index-name>;
```

then re-apply migrations (restart with `GOTCHA_AUTO_MIGRATE_ENABLED=true`, or run `--migrate-only` again) — this time `IF NOT EXISTS` won't find an object under that name and will build the index from scratch.

## What changes when upgrading: ingest DSN keys get a type

Starting with this upgrade, a DSN key has a type (`browser`/`server`/`agent`)
that limits what telemetry it may send — see [Ingest keys](/docs/keys) for
the full breakdown. For an existing install, this upgrade breaks nothing:

- every key issued before the upgrade keeps working with no action on your
  part — it automatically becomes type `legacy` with full access,
  indefinitely;
- in the project settings such keys are marked with an "Untyped" badge —
  that's not an error and not a reason to rush a change;
- projects created after the upgrade get three keys right away — one each
  for `browser`/`server`/`agent` — instead of a single shared one;
- a host still registers automatically after upgrading, but **only** from an
  export sent with an `agent`-type key (or an old untyped key); a metrics
  export with a key of another type is still accepted, as before, it just
  doesn't register a host — if you already have an agent or collector set up
  from the old config template, there's nothing to change: it keeps using
  the same key it used before the upgrade.

Splitting your sources across the new typed keys without any ingest downtime
is a separate, optional task — see [Ingest keys](/docs/keys) for the steps.

## What changes when upgrading: seventeen environment variables renamed

This upgrade renames seventeen server environment variables — the unit of
measurement, the modifier, and the subsystem now read honestly from the
name itself, rather than from what sits next to it. A variable set under
its old name with a non-empty value refuses to start with an explicit
"old name → new name" message, instead of silently falling back to a
default.

| Before | After |
|---|---|
| `GOTCHA_ADDR` | `GOTCHA_LISTEN_ADDR` |
| `GOTCHA_LOG_LEVEL` | `GOTCHA_LOGGING_LEVEL` |
| `GOTCHA_LOG_FORMAT` | `GOTCHA_LOGGING_FORMAT` |
| `GOTCHA_LOCAL_REGION` | `GOTCHA_UPTIME_LOCAL_REGION` |
| `GOTCHA_REGISTRATION` | `GOTCHA_REGISTRATION_MODE` |
| `GOTCHA_EXPORT_TTL_HOURS` | `GOTCHA_EXPORT_RETENTION_HOURS` |
| `GOTCHA_SCRUB_KEYS` | `GOTCHA_SCRUB_DENY_KEYS` |
| `GOTCHA_SCRUB_ALLOW_KEYS` | `GOTCHA_SCRUB_KEEP_KEYS` |
| `GOTCHA_RUN_EVALUATORS` | `GOTCHA_EVALUATORS_ENABLED` |
| `GOTCHA_AUTO_MIGRATE` | `GOTCHA_AUTO_MIGRATE_ENABLED` |
| `GOTCHA_ALLOW_INSECURE_SECRET` | `GOTCHA_SECRET_KEY_ALLOW_INSECURE` |
| `GOTCHA_MAX_BUFFER_BYTES` | `GOTCHA_MAX_WRITER_BUFFER_BYTES` |
| `GOTCHA_MAX_QUEUE_BYTES` | `GOTCHA_MAX_INGEST_QUEUE_BYTES` |
| `GOTCHA_PROBE_TOKEN` | `GOTCHA_PROBE_KEY` |
| `GOTCHA_EXTERNAL_CHANNEL_DETAILS` | `GOTCHA_EXTERNAL_CHANNEL_DETAILS_ENABLED` |
| `GOTCHA_OIDC_NAME` | `GOTCHA_OIDC_DISPLAY_NAME` |
| `GOTCHA_PURGE_RECONCILE_HOURS` | `GOTCHA_PROJECT_PURGE_RECONCILE_HOURS` |

After upgrading the server, go through **every** place where you set
`GOTCHA_*` variables, not just the `.env` next to `docker-compose.yml`:
systemd units, separate CI/CD `.env` files, and, separately, **the `.env` of
remote probes (`--mode=probe`) running on other hosts** — the server upgrade
physically cannot reach them, and they'll keep running with the old names
until you update them by hand.

The most visible case of this blast radius is the remote probe's token
variable: its new name is `GOTCHA_PROBE_KEY` (the before/after line is in
the table above). Probes that are already registered and
running hold the old name in their host's environment. Restart them with
the variable under its new name right after upgrading the central server —
otherwise, the next time a probe restarts (an image update, a host reboot,
and so on), it will refuse to start instead of quietly reconnecting with
its old value. See [Probes](/docs/probes) and
[Configuration](/docs/configuration).

The same blast radius applies to three environment variables of the
`gotcha-agent` binary itself — one with a type change: the collection
interval is now a whole number of seconds, not a duration string like
"30s". The agent is also a separate process on a remote host that the
server upgrade can't reach: if `/etc/gotcha-agent/gotcha-agent.env` still
holds them under their old names, the next time you run the update command
it refuses with a `config check failed` message — the installer validates
the config with the new binary (`--check`) before it ever touches the
systemd unit, so the old agent keeps running unchanged on its old binary
until you fix the names in that file by hand (per the table below) and run
the update command again. The current names and ranges are in the variable
reference on the [Hosts](/docs/hosts) page.

| Before | After |
|---|---|
| `GOTCHA_AGENT_INTERVAL` | `GOTCHA_AGENT_INTERVAL_SECONDS` |
| `GOTCHA_AGENT_KEY` | `GOTCHA_AGENT_INGEST_KEY` |
| `GOTCHA_AGENT_TLS_SKIP_VERIFY` | `GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY` |

## What changes when upgrading: compose and build variables renamed

The same wave renames eleven more variables — eight Docker Compose
substitution variables (the database containers' passwords and memory
ceilings, the app container's memory ceiling and network MTU, the published
port and bind address) get the `GOTCHA_COMPOSE_` prefix, and the three
variables the `Makefile` uses to pass build metadata into
`docker-compose.yml` (`DOCKER_BUILD_ENV`) get the `GOTCHA_BUILD_` prefix.
The variables themselves and their defaults are documented in
[Configuration](/docs/configuration), under the "Compose-only variables"
and "Build-only variables" sections.

| Before | After |
|---|---|
| `GOTCHA_PG_PASSWORD` | `GOTCHA_COMPOSE_PG_PASSWORD` |
| `GOTCHA_CH_PASSWORD` | `GOTCHA_COMPOSE_CH_PASSWORD` |
| `GOTCHA_PG_MEM_LIMIT` | `GOTCHA_COMPOSE_PG_MEM_LIMIT` |
| `GOTCHA_CH_MEM_LIMIT` | `GOTCHA_COMPOSE_CH_MEM_LIMIT` |
| `GOTCHA_MEM_LIMIT` | `GOTCHA_COMPOSE_MEM_LIMIT` |
| `GOTCHA_NET_MTU` | `GOTCHA_COMPOSE_NET_MTU` |
| `GOTCHA_PORT` | `GOTCHA_COMPOSE_PORT` |
| `GOTCHA_BIND` | `GOTCHA_COMPOSE_BIND` |
| `GOTCHA_VERSION` | `GOTCHA_BUILD_VERSION` |
| `GOTCHA_COMMIT` | `GOTCHA_BUILD_COMMIT` |
| `GOTCHA_DATE` | `GOTCHA_BUILD_DATE` |

Unlike the server and agent variables, no gotcha process reads these
eleven at all — only Docker Compose itself (the `${...}` substitution in
the compose file) and `make` ever see them. The safety net is the same
one: `docker-compose.yml` pulls in `.env` wholesale (`env_file`), so an
old name left in `.env` still reaches the `gotcha` process's environment
and gets caught by the same "old name → new name" startup refusal, even
though the process itself never reads it and never did.

If you set these variables somewhere other than `.env` — directly in the
compose file's `environment:`/`build.args` block, or passed separately to
`make` (e.g. `make up GOTCHA_COMPOSE_PORT=...`) — rename them there by
hand, using the table above: the startup refusal can't catch that edit,
because it never reaches the `gotcha` process's environment.

## Standard upgrade (single server, `--mode=all`)

If you're using the stock `docker-compose.yml` as-is (a single app replica running `--mode=all`) — the common case for a self-hosted setup:

```bash
cd gotcha   # the directory with docker-compose.yml
git pull
make up-rebuild
```

Breaking this down:

1. `git pull` — pulls the new code from the repository.
2. `make up-rebuild` — rebuilds the `gotcha` application image with the updated code and recreates the container (Postgres/ClickHouse use pre-built official images; you don't need to rebuild those — Compose pulls the version pinned in `docker-compose.yml` automatically if it changed). On startup (this is the default behavior, `GOTCHA_AUTO_MIGRATE_ENABLED=true` by default), the app **automatically** applies any missing schema migrations to PostgreSQL and ClickHouse before it starts accepting requests — nothing else to do.

Build through `make`, not through a bare `docker compose build`: only `make` computes the git version and stamps it into the binary. An image built with bare compose commands works, but reports itself as "no build metadata" in `/version`, on the About page and in the `gotcha_build_info` metric — you lose the ability to verify that what is deployed is what you think it is.

If you only want to update the image without rebuilding from source (e.g. you're pulling a pre-built image from a registry rather than building from `git`), use `docker compose pull` + `docker compose up -d` instead.

## What automatic migrations mean

By default (`GOTCHA_AUTO_MIGRATE_ENABLED=true`), on every start the app checks the schema version in the database and, if it's behind the version baked into the binary, applies the missing migrations automatically before opening its port. This is convenient for the typical "single server, single process" setup — an upgrade boils down to the three commands above.

## Running migrations as a separate step (multiple app replicas)

If you're running **multiple** `gotcha` processes at once (e.g. separate `--mode=ingest` and `--mode=web` processes, or several replicas behind a load balancer — an advanced deployment scenario beyond the stock `docker-compose.yml`), letting every replica auto-migrate on startup is risky: multiple processes could try to apply migrations at the same time. For that case:

1. Set `GOTCHA_AUTO_MIGRATE_ENABLED=false` for all replicas.
2. Before starting the replicas, run migrations **once** with `--migrate-only`:

   ```bash
   docker compose run --rm --no-deps gotcha --migrate-only
   ```

   That invocation applies the schema and exits 0 without opening the HTTP port or starting ingest or uptime — an init job in the proper sense. The flag turns migration on by itself, so `GOTCHA_AUTO_MIGRATE_ENABLED=false` in the replicas' environment does not get in its way. It is rejected together with `--mode=probe`, and says so: a probe never opens a database connection, and exiting 0 quietly would claim a schema was applied when it was not.
3. After that, start (or restart) all replicas with `GOTCHA_AUTO_MIGRATE_ENABLED=false` — they'll verify the database schema matches the version built into the binary and refuse to start if it doesn't (this is a safeguard against silently accepting data into a stale schema — an explicit refusal at startup beats silent errors on every insert).

For the stock `docker-compose.yml` in this repository (a single `gotcha` service in `all` mode), separate migrations aren't needed — use the standard upgrade flow above.

## If a migration was interrupted (dirty)

If the process is killed **while** a migration is running (power loss, `docker kill`, an OOM), the migration tool leaves the schema marked **dirty**: "migration N started and never confirmed finishing". From then on the app refuses to start — deliberately, because it cannot know whether migration N half-applied or fully applied — and the startup error names the stuck version:

```bash
docker compose logs gotcha
# ... база в состоянии dirty на версии N ...
```

First look at what migration N actually did to the schema (the migration files live in `internal/db/migrations/` in the repository, numbered). Then clear the flag with the version that matches reality:

```bash
# Migration N did complete (or you finished it by hand) — stay on N:
docker compose run --rm gotcha --migrate-force=N

# You rolled migration N back by hand — step back to N-1:
docker compose run --rm gotcha --migrate-force=N-1
```

For the ClickHouse schema the flag is `--migrate-force-ch=N`; the startup error says which database is stuck. Only these two targets are accepted — anything else is treated as a typo, because it would silently shift the starting point of every future migration.

**`--migrate-force` does not finish the migration** — it only clears the "unfinished" marker. If migration N half-applied and you clear the flag at N without checking, the next start will happily run against a schema missing half of migration N, and every insert into the missing part will fail. Verify the schema first, then clear the flag, then `docker compose up -d` — the app will apply the remaining migrations (N+1 and up) itself.

## Rolling back

Gotcha's schema migrations are forward-only: the product cannot roll the schema itself back. Rolling back the **binary** while leaving the schema in place is possible, though — provided the migrations the new version applied were additive.

Every migration carries a backward-compatibility marker, and applying it records that marker in the database (the `schema_compat` table). The older version reads it at startup and decides for itself:

| What the new version applied | What the old binary does at startup |
|---|---|
| Additive migrations only (new tables, columns with defaults, indexes) | starts and runs; logs a warning listing the versions |
| At least one breaking migration (dropped or renamed column, type change) | refuses to start and names the version in the way |
| No record for a version in `schema_compat` | refuses to start: the marker is unknown, and data is not worth the risk |

What to do:

1. **Roll the application back** — switch to the previous commit/tag and rebuild:
   ```bash
   git checkout <previous-tag-or-commit>
   make up-rebuild
   ```
2. **Read the startup log.** A line like "schema version N is ahead of the built-in M; version … is marked backward-compatible, running against it" means the rollback worked and the instance is running against a newer schema. That is a supported state, but a temporary one: finish the job — either go back to the new version, or restore the backup taken before the upgrade.
3. **If startup is refused** with an incompatible-schema message, rolling the binary back is not possible: **restore the backup** taken before the upgrade (see [Backup & Restore](/docs/backup-restore)) and bring up the previous version against it.

Compatibility markers did not exist from the first release — `CHANGELOG.md` in the repository names the one that introduced them. You cannot roll back **through** that upgrade: schema versions applied by earlier releases carry no marker, so starting against them is refused.

This is why "take a backup before upgrading" at the top of this page stays mandatory: some rollbacks work without it, but not all.

## Agents on hosts are updated separately

Upgrading the instance doesn't touch the `gotcha-agent` installed on your servers: they keep reporting on the old version. A host's card shows an "Update available" badge with the command ready to copy — updating is the same install command, run on the host without the environment variables (see [Hosts](/docs/hosts)). An older agent stays compatible: metric names and the protocol didn't change, the update carries fixes to the agent itself. The rename of the agent's own environment variables in this upgrade is covered in the previous section.

## Verify after upgrading

```bash
docker compose ps
curl -sf http://localhost:59080/readyz
```

`docker compose ps` — all containers should show `Up (healthy)`, `gotcha` included. `/readyz` should return `{"status":"ready",…}` with HTTP 200. Also check the logs for any startup errors:

```bash
docker compose logs --tail=100 gotcha
```

A line reading `applying migrations` not followed by an error message means migrations succeeded. Then open the UI in a browser and confirm you can see your organizations, projects, and data.

## What's next

- [Backup & Restore](/docs/backup-restore).
- [Configuration](/docs/configuration) — the full variable reference, including `GOTCHA_AUTO_MIGRATE_ENABLED`.
- [Installation](/docs/installation).
