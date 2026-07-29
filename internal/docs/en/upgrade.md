# Upgrade

## Before you start: take a backup

An upgrade applies database schema migrations — this is not reversible automatically (nobody runs a "just in case" down-migration for you). Before upgrading, back up both PostgreSQL and ClickHouse — see [Backup & Restore](/docs/backup-restore). Don't skip this even if the upgrade looks minor.


## One-off step when upgrading from versions before 0.4.2: rotate channel secrets

Before this version a delivery channel's secret (Telegram bot token, webhook signing key) was written into the `notification_outbox` queue **in the clear** — the `payload` column is a plain `jsonb`, which made the encryption of `alert_channels.secret` pointless. Migration `0025` strips those values from the queue during the upgrade, and the new code no longer puts them there: the secret is fetched by channel id at send time.

The migration only cleans the **live database**, though. If the instance ran an older version, those secrets sit in the clear in **every PostgreSQL backup taken before the upgrade** — and you are, we hope, taking backups regularly. So after upgrading:

1. Rotate the Telegram bot tokens Gotcha uses (`/revoke` in @BotFather, then put the new token in the channel).
2. Change the webhook signing keys on the receiving side and in the channel.
3. Delete the old backups if you no longer need them; if you do need them, store them at the same protection level as secrets.

You can skip this only if you had no delivery channels at all.

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
2. Before starting the replicas, run migrations **once**, as a separate one-off invocation of the binary with `GOTCHA_AUTO_MIGRATE=true` (or simply not overriding it — that's the default), e.g.:

   ```bash
   docker compose run --rm \
     -e GOTCHA_AUTO_MIGRATE=true \
     gotcha /bin/sh -c "true"
   ```

   In practice, starting a normal `gotcha` container once with `GOTCHA_AUTO_MIGRATE=true` is enough — migrations are applied at the start of startup, before the HTTP port opens. This holds for `ingest`, `web`, `uptime` and `all`; `--mode=probe` is the exception, as a probe never opens a database connection at all.
3. After that, start (or restart) all replicas with `GOTCHA_AUTO_MIGRATE=false` — they'll verify the database schema matches the version built into the binary and refuse to start if it doesn't (this is a safeguard against silently accepting data into a stale schema — an explicit refusal at startup beats silent errors on every insert).

For the stock `docker-compose.yml` in this repository (a single `gotcha` service in `all` mode), separate migrations aren't needed — use the standard upgrade flow above.

## Rolling back

Gotcha's schema migrations are written forward-only — don't count on an automatic, safe rollback of the schema. If something goes wrong after an upgrade:

1. **Rolling back the application** is simple — switch to the previous commit/tag and rebuild:
   ```bash
   git checkout <previous-tag-or-commit>
   docker compose build
   docker compose up -d
   ```
   This only rolls back the application's code. If the new version already applied new schema migrations to the database, the old binary might not work against the new schema (backward incompatibility) — or, if compatibility was preserved, it'll work fine.
2. **If rolling back the code without rolling back the schema doesn't work** (the old version explicitly requires the old schema — on startup you'll see an error like "schema version is ahead of the built-in version") — the reliable path is to **restore the backup** taken before the upgrade (see [Backup & Restore](/docs/backup-restore)) and bring up the previous application version against it.

This is exactly why "take a backup before upgrading" at the top of this page isn't a formality — it's the only reliable way back.

## Verify after upgrading

```bash
docker compose ps
curl -sf http://localhost:59080/healthz
```

`docker compose ps` — all containers should show `Up` (`postgres`/`clickhouse` show `Up (healthy)`). `/healthz` should return `{"clickhouse":"ok","postgres":"ok"}` with HTTP 200. Also check the logs for any startup errors:

```bash
docker compose logs --tail=100 gotcha
```

A line reading `applying migrations` not followed by an error message means migrations succeeded. Then open the UI in a browser and confirm you can see your organizations, projects, and data.

## What's next

- [Backup & Restore](/docs/backup-restore).
- [Configuration](/docs/configuration) — the full variable reference, including `GOTCHA_AUTO_MIGRATE`.
- [Installation](/docs/installation).
