# Upgrade

## Before you start: take a backup

An upgrade applies database schema migrations — this is not reversible automatically (nobody runs a "just in case" down-migration for you). Before upgrading, back up both PostgreSQL and ClickHouse — see [Backup & Restore](/docs/backup-restore). Don't skip this even if the upgrade looks minor.


## What changes when upgrading from versions before 0.4.2: details no longer reach everyone

Whether an alert carried the error text used to be decided by channel type: Telegram and webhooks counted as external, email as internal — so any mailbox received the full text. It is now decided per recipient: trusted means an address on your instance host, on a domain listed in `GOTCHA_TRUSTED_RECIPIENTS`, or on an internal network.

In practice:

- **mail on a public service** (`@gmail.com`, `@yandex.ru`, and the like) no longer receives the error text — only a link to the issue in the interface;
- **mail on your organization's domain** stops receiving details too, if that domain differs from the instance host, until you list it in `GOTCHA_TRUSTED_RECIPIENTS`;
- **a webhook pointed at your internal infrastructure** now does receive details — previously that required turning `GOTCHA_EXTERNAL_CHANNEL_DETAILS` on globally, which opened up Telegram as well.

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

## Standard upgrade (single server, `--mode=all`)

If you're using the stock `docker-compose.yml` as-is (a single app replica running `--mode=all`) — the common case for a self-hosted setup:

```bash
cd gotcha   # the directory with docker-compose.yml
git pull
docker compose build
docker compose up -d
```

Breaking this down:

1. `git pull` — pulls the new code from the repository.
2. `docker compose build` — rebuilds the `gotcha` application image with the updated code (Postgres/ClickHouse use pre-built official images; you don't need to rebuild those — Compose pulls the version pinned in `docker-compose.yml` automatically if it changed).
3. `docker compose up -d` — recreates the `gotcha` container from the new image. On startup (this is the default behavior, `GOTCHA_AUTO_MIGRATE=true` by default), the app **automatically** applies any missing schema migrations to PostgreSQL and ClickHouse before it starts accepting requests — nothing else to do.

If you only want to update the image without rebuilding from source (e.g. you're pulling a pre-built image from a registry rather than building from `git`), use `docker compose pull` instead of `docker compose build`.

## What automatic migrations mean

By default (`GOTCHA_AUTO_MIGRATE=true`), on every start the app checks the schema version in the database and, if it's behind the version baked into the binary, applies the missing migrations automatically before opening its port. This is convenient for the typical "single server, single process" setup — an upgrade boils down to the three commands above.

## Running migrations as a separate step (multiple app replicas)

If you're running **multiple** `gotcha` processes at once (e.g. separate `--mode=ingest` and `--mode=web` processes, or several replicas behind a load balancer — an advanced deployment scenario beyond the stock `docker-compose.yml`), letting every replica auto-migrate on startup is risky: multiple processes could try to apply migrations at the same time. For that case:

1. Set `GOTCHA_AUTO_MIGRATE=false` for all replicas.
2. Before starting the replicas, run migrations **once** with `--migrate-only`:

   ```bash
   docker compose run --rm --no-deps gotcha --migrate-only
   ```

   That invocation applies the schema and exits 0 without opening the HTTP port or starting ingest or uptime — an init job in the proper sense. The flag turns migration on by itself, so `GOTCHA_AUTO_MIGRATE=false` in the replicas' environment does not get in its way. It is rejected together with `--mode=probe`, and says so: a probe never opens a database connection, and exiting 0 quietly would claim a schema was applied when it was not.
3. After that, start (or restart) all replicas with `GOTCHA_AUTO_MIGRATE=false` — they'll verify the database schema matches the version built into the binary and refuse to start if it doesn't (this is a safeguard against silently accepting data into a stale schema — an explicit refusal at startup beats silent errors on every insert).

For the stock `docker-compose.yml` in this repository (a single `gotcha` service in `all` mode), separate migrations aren't needed — use the standard upgrade flow above.

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
   docker compose build
   docker compose up -d
   ```
2. **Read the startup log.** A line like "schema version N is ahead of the built-in M; version … is marked backward-compatible, running against it" means the rollback worked and the instance is running against a newer schema. That is a supported state, but a temporary one: finish the job — either go back to the new version, or restore the backup taken before the upgrade.
3. **If startup is refused** with an incompatible-schema message, rolling the binary back is not possible: **restore the backup** taken before the upgrade (see [Backup & Restore](/docs/backup-restore)) and bring up the previous version against it.

Compatibility markers did not exist from the first release — `CHANGELOG.md` in the repository names the one that introduced them. You cannot roll back **through** that upgrade: schema versions applied by earlier releases carry no marker, so starting against them is refused.

This is why "take a backup before upgrading" at the top of this page stays mandatory: some rollbacks work without it, but not all.

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
- [Configuration](/docs/configuration) — the full variable reference, including `GOTCHA_AUTO_MIGRATE`.
- [Installation](/docs/installation).
