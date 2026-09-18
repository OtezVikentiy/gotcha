# Backup & Restore

Gotcha keeps data in two separate databases, and both matter equally — you must back up **both together**, otherwise after a restore they'll be out of sync (e.g. a project exists in one database but its events are in the other, or vice versa).

| Database | What's in it | Container |
|---|---|---|
| **PostgreSQL** | Accounts, organizations, projects, members, alert rules, delivery channels, incidents, settings — everything you configured by hand in the UI. | `postgres` |
| **ClickHouse** | The error events themselves, trace spans, metric points, profiling samples, uptime check results — the full volume of telemetry your applications sent. | `clickhouse` |

Restoring only one of the two either breaks the UI (a project exists but has zero events for it) or, the other way around, loses your actual configuration (alerts, members, DSN keys) even if the telemetry is intact.

Every command below is run **from the repository directory** (`gotcha/`, the same place as `docker-compose.yml`) and uses `docker compose exec` — running a command inside an already-running container, without needing to publish the database ports to the host (they aren't published — see [Installation](/docs/installation)). This is for the Docker install; for a Docker-free install, see "Bare metal: the same thing without Docker" at the end of this page.

## Backup: PostgreSQL

`pg_dump` is PostgreSQL's standard logical-backup tool; it safely takes a copy of a live database without stopping the service:

```bash
mkdir -p backup
docker compose exec -T postgres pg_dump -U gotcha -d gotcha \
  | gzip > backup/postgres-$(date +%F).sql.gz
```

Breaking this down: `docker compose exec -T postgres` runs a command inside the `postgres` container (`-T` disables the pseudo-terminal, needed when redirecting output to a file); `pg_dump -U gotcha -d gotcha` dumps the `gotcha` database as user `gotcha` (the default credentials from `docker-compose.yml`; substitute your own if you changed them); the output is piped through `gzip` and saved to disk on the host with the date in the filename.

Verify the file isn't empty and looks like a dump:

```bash
zcat backup/postgres-$(date +%F).sql.gz | head -20
```

You should see lines like `-- PostgreSQL database dump` and `CREATE TABLE ...`.

## Backup: ClickHouse

ClickHouse holds a much larger volume of data than PostgreSQL, so a different approach is used: dumping each table in ClickHouse's built-in binary `Native` format (compact and fast to restore with the same ClickHouse version).

First, list the tables in the `gotcha` database:

```bash
docker compose exec -T clickhouse clickhouse-client \
  --user gotcha --password gotcha --database gotcha \
  --query "SHOW TABLES"
```

`SHOW TABLES` returns more rows than there are tables to dump — not every one needs its own explanation, but none of them should be a mystery either. Besides the seven tables below, you'll also see:

- `transactions_5m`, `web_vitals_5m` — **materialized views**. Do NOT dump or restore those: they are filled automatically when rows are inserted into the source tables, and restoring their contents alongside `transactions` doubles the aggregates — Performance would report twice the real throughput.
- `.inner_id.<uuid>` (one per materialized view, so two rows) — the view's own backing storage. ClickHouse creates and fills it automatically together with the view; it is not dumped or restored on its own and can't be (it's not a project table, it has no schema of its own to migrate).
- `schema_migrations` — Gotcha's migration tool bookkeeping table, tracking which schema version was applied. It is not dumped: Gotcha restores the schema version itself during the `--migrate-only` step (see "Restore: PostgreSQL" below).

That's 7 (the dump list) + 2 (views) + 2 (their backing storage) + 1 (`schema_migrations`) = 12 rows on the current schema. The list of tables to dump is fixed and shown below.

Dump each of them:

```bash
mkdir -p backup/clickhouse
for t in events transactions spans metric_points profile_samples check_results logs; do
  docker compose exec -T clickhouse clickhouse-client \
    --user gotcha --password gotcha --database gotcha \
    --query "SELECT * FROM $t FORMAT Native" \
    > backup/clickhouse/$t-$(date +%F).native
done
```

This works against a live database without stopping anything — ClickHouse returns a consistent snapshot as of the query for each individual table (not guaranteed to be a single consistent instant across *all* tables together, but for observability data this is rarely a real concern).

**A simpler and fully consistent alternative is a filesystem snapshot with services stopped.** It's guaranteed consistent across PostgreSQL and ClickHouse together, at the cost of brief downtime (usually seconds to tens of seconds):

```bash
docker compose stop gotcha postgres clickhouse
docker run --rm \
  -v gotcha_pgdata:/pgdata:ro \
  -v gotcha_chdata:/chdata:ro \
  -v "$(pwd)/backup:/backup" \
  alpine tar czf /backup/volumes-$(date +%F).tar.gz /pgdata /chdata
docker compose start gotcha postgres clickhouse
```

(the volume names `gotcha_pgdata`/`gotcha_chdata` use the `gotcha_` prefix taken from the project directory's name; verify the exact name with `docker volume ls | grep gotcha` if it differs). This approach works well for a nightly cron job where a brief application outage isn't a problem.

Pick one approach (live dumps via `pg_dump`+`clickhouse-client`, or a volume snapshot with downtime) — both are valid; what matters is doing it **regularly** and **verifying** the backup actually restores (see below).

## Back up `.env` too — not just the databases

`GOTCHA_SECRET_KEY` encrypts secrets at rest: SSO client secrets, Telegram bot
tokens and webhook signing keys. It lives only in your `.env` (or the compose
`environment:` block), never in the database — so a dump of PostgreSQL and
ClickHouse alone is **not** a complete backup.

Restore the databases with a different key and those secrets can no longer be
decrypted. The affected alert channels stop delivering, but stay listed on the
alerts page marked "Secret unreadable": enter the secret again right there and
delivery resumes. SSO for the organization stops working in that case and its
settings have to be entered again.

```bash
cp .env "$BACKUP_DIR/env-$(date +%F)"
chmod 600 "$BACKUP_DIR/env-$(date +%F)"
```

Store it with the same care as the dumps: it is the key to everything encrypted
inside them. There is no re-keying procedure — if the key is lost, the encrypted
secrets have to be entered again by hand.

## Restore: PostgreSQL

Restore into an **empty** database and **before** the application starts. On
startup the application applies migrations itself (`GOTCHA_AUTO_MIGRATE_ENABLED=true` by
default) — that is, it creates every table before opening its port — so a dump
loaded afterwards meets a schema that already exists.

Restoring a full copy (both databases) is one continuous procedure, not two independent ones. The PostgreSQL dump carries its own schema (`CREATE TABLE` statements baked into the dump itself), but a ClickHouse `Native` dump is rows only — Gotcha's own migrations create the schema for it. Between restoring PostgreSQL and inserting into ClickHouse there's a mandatory step in between: apply migrations without starting the application, or ClickHouse has no tables yet to insert into:

If the archive being restored is older than the current `*_RETENTION_DAYS`, step 4 applies the TTL before the rows exist, and after they're inserted in step 5 they only survive until ClickHouse's next background merge — regardless of what step 6's "success" looks like. The tell is a `retention: rows already older than the active window exist` warning in the log of the application's first start in step 6: if you see it, raise the relevant `*_RETENTION_DAYS` (or set it to `0` temporarily) before that start if you need the data for its full original age.

```bash
# 1. Bring up ONLY the databases, without the application, or it creates the schema first.
docker compose up -d postgres clickhouse

# 2. Recreate the PostgreSQL database from scratch.
docker compose exec -T postgres psql -U gotcha -d postgres \
  -c 'DROP DATABASE IF EXISTS gotcha' -c 'CREATE DATABASE gotcha'

# 3. Load the PostgreSQL dump, stopping at the first error.
gunzip -c backup/postgres-2026-07-01.sql.gz \
  | docker compose exec -T postgres psql -v ON_ERROR_STOP=1 --single-transaction -U gotcha -d gotcha

# 4. Apply migrations without starting the application: this creates the
#    ClickHouse schema (and brings PostgreSQL's schema up to the binary's
#    version, if needed) without opening the port or starting any background
#    workers — safe to insert into ClickHouse right after this, with no
#    race against a live application.
docker compose run --rm --no-deps gotcha --migrate-only

# 5. Restore ClickHouse — see "Restore: ClickHouse" below.

# 6. Only now start the full application.
docker compose up -d
```

`ON_ERROR_STOP=1` and `--single-transaction` are load-bearing, not decoration.
Without them `psql` prints a stream of `relation ... already exists` and
`duplicate key`, **exits with status 0**, and looks like a successful restore —
while a `COPY` into a table that gained columns in the newer schema does not
land at all, and no one can tell the expected noise from a real failure. With
both flags the first error stops the restore and the transaction rolls back
whole: either everything is restored, or the database is left empty and that is
visible.

If the application is already running against this database, stop it
(`docker compose stop gotcha`) before step 2 — restoring underneath a running
application means it keeps writing to the same database at the same time.

## Restore: ClickHouse

Restoring a table dumped in `Native` format with the reverse command:

```bash
cat backup/clickhouse/events-2026-07-01.native | \
  docker compose exec -T clickhouse clickhouse-client \
    --user gotcha --password gotcha --database gotcha \
    --query "INSERT INTO events FORMAT Native"
```

Repeat for each table. The table must already exist (created by step 4 in "Restore: PostgreSQL" above, `--migrate-only`) and be empty, otherwise the data is appended to what's already there instead of replacing it.

**Restoring a second time, into a database that already has data?** Clear the materialized views (`transactions_5m`, `web_vitals_5m`) **before** inserting into the source tables, not after. They fill themselves as a side effect of the insert into `transactions` — clearing them afterwards wipes out what the insert just added, and Performance stays empty even though the restore reported success:

```bash
docker compose exec -T clickhouse clickhouse-client \
  --user gotcha --password gotcha --database gotcha \
  --query "TRUNCATE TABLE transactions_5m"
docker compose exec -T clickhouse clickhouse-client \
  --user gotcha --password gotcha --database gotcha \
  --query "TRUNCATE TABLE web_vitals_5m"
```

only then insert the `Native` dumps using the command above.

## Restore from a volume snapshot

If you used the `tar` volume approach:

```bash
docker compose down
docker run --rm \
  -v gotcha_pgdata:/pgdata \
  -v gotcha_chdata:/chdata \
  -v "$(pwd)/backup:/backup" \
  alpine sh -c "rm -rf /pgdata/* /chdata/* && tar xzf /backup/volumes-2026-07-01.tar.gz -C /"
docker compose up -d
```

**This is a destructive operation** — it wipes the current contents of the volumes before extracting the archive. Make sure it's the right archive before running it.

## After restoring — verify

```bash
curl -sf http://localhost:59080/readyz
```

Then open the UI, log in with your user, open a project, and confirm you can see both the configuration (alerts, members) and the data (events under Issues).

## Cron example

Daily backup of PostgreSQL + ClickHouse at 3:30am, keeping the 14 most recent copies:

```bash
crontab -e
```

add this line:

```cron
30 3 * * * cd /path/to/gotcha && /path/to/gotcha/backup.sh >> /var/log/gotcha-backup.log 2>&1
```

where `backup.sh` is a small script with all the dump commands above plus cleanup of old files, e.g.:

```bash
#!/usr/bin/env bash
set -euo pipefail
cd /path/to/gotcha
mkdir -p backup/clickhouse
day=$(date +%F)

# Pruning runs from a trap, registered BEFORE the first command that can fail.
# Otherwise, under set -e, pruning would be skipped exactly when the dump failed:
# a line at the end of the script would never be reached, and disk would fill up
# precisely when backups are not being taken anyway.
trap 'find backup -type f -name "*.tmp" -delete; find backup -type f -mtime +14 -delete' EXIT

# Write to a temporary file and rename only on success. Without this the
# redirection creates the file BEFORE pg_dump gets to run: it fails, and the
# directory holds an empty .sql.gz indistinguishable from a real backup until
# the day you need one.
docker compose exec -T postgres pg_dump -U gotcha -d gotcha \
  | gzip > backup/postgres-$day.sql.gz.tmp
mv backup/postgres-$day.sql.gz.tmp backup/postgres-$day.sql.gz

for t in events transactions spans metric_points profile_samples check_results logs; do
  docker compose exec -T clickhouse clickhouse-client \
    --user gotcha --password gotcha --database gotcha \
    --query "SELECT * FROM $t FORMAT Native" \
    > backup/clickhouse/$t-$day.native.tmp
  mv backup/clickhouse/$t-$day.native.tmp backup/clickhouse/$t-$day.native
done
```

Remember to make the script executable (`chmod +x backup.sh`), and — importantly — copy the `backup/` directory's contents **off this same server** (a different disk, S3-compatible storage, another server). A local-only copy won't help if the server itself fails.

## Bare metal: the same thing without Docker

If Gotcha is installed with `install-bare-metal.sh` ([Installation without Docker](/docs/installation-bare-metal)), PostgreSQL and ClickHouse are system services, not containers: `docker compose exec` is replaced with a direct call to `pg_dump`/`psql`/`clickhouse-client` as a system user, and Docker's named volumes become package paths on disk (`/var/lib/postgresql`, `/var/lib/clickhouse`).

### Backup: PostgreSQL

The installer itself runs exactly this command, automatically, before every upgrade (see [Upgrade](/docs/upgrade), the "Upgrading a bare-metal install" section):

```bash
mkdir -p /var/lib/gotcha/backup && chmod 700 /var/lib/gotcha/backup
sudo -u postgres pg_dump -d gotcha | gzip > /var/lib/gotcha/backup/postgres-$(date +%F).sql.gz
chmod 600 /var/lib/gotcha/backup/postgres-$(date +%F).sql.gz
```

### Backup: ClickHouse

Same seven tables, same caveat about the materialized views (`transactions_5m`, `web_vitals_5m`) and `schema_migrations` as in the Docker section above — only the `clickhouse-client` invocation changes, no `docker compose exec`, with the password from `/etc/clickhouse-server/users.d/10-gotcha.xml` (the same one baked into `GOTCHA_CH_DSN`):

```bash
mkdir -p /var/lib/gotcha/backup/clickhouse
for t in events transactions spans metric_points profile_samples check_results logs; do
  clickhouse-client --user gotcha --password "$CH_PASSWORD" --database gotcha \
    --query "SELECT * FROM $t FORMAT Native" \
    > /var/lib/gotcha/backup/clickhouse/$t-$(date +%F).native
done
```

A filesystem snapshot with services stopped, using the same paths the packages themselves create instead of Docker's named volumes:

```bash
systemctl stop gotcha postgresql clickhouse-server
tar czf /var/lib/gotcha/backup/volumes-$(date +%F).tar.gz \
  /var/lib/postgresql /var/lib/clickhouse
systemctl start postgresql clickhouse-server gotcha
```

Don't forget `/etc/gotcha/gotcha.env` either — the `GOTCHA_SECRET_KEY` warning from "Back up `.env` too" above applies literally, just at a different path:

```bash
cp /etc/gotcha/gotcha.env "/var/lib/gotcha/backup/env-$(date +%F)"
chmod 600 "/var/lib/gotcha/backup/env-$(date +%F)"
```

### Restore: PostgreSQL

The same order of steps as the Docker section above (PostgreSQL first, then migrations without starting the app, then ClickHouse, then the app as a whole) — services are managed with `systemctl`, not `docker compose`:

```bash
# 1. Stop the app, leave PostgreSQL/ClickHouse running.
systemctl stop gotcha

# 2. Recreate the PostgreSQL database from scratch.
sudo -u postgres psql -d postgres \
  -c 'DROP DATABASE IF EXISTS gotcha' -c 'CREATE DATABASE gotcha OWNER gotcha'

# 3. Load the dump, stopping at the first error.
gunzip -c /var/lib/gotcha/backup/postgres-2026-07-01.sql.gz \
  | sudo -u postgres psql -v ON_ERROR_STOP=1 --single-transaction -d gotcha

# 4. Apply migrations without starting the app — this creates the ClickHouse schema.
systemd-run --pipe --wait --collect --uid=gotcha --gid=gotcha \
  --property="EnvironmentFile=/etc/gotcha/gotcha.env" \
  /usr/local/bin/gotcha --migrate-only

# 5. Restore ClickHouse — see "Restore: ClickHouse" below.

# 6. Start the app.
systemctl start gotcha
```

`ON_ERROR_STOP=1` and `--single-transaction` are load-bearing for the same reason as in the Docker section above — without them a partially failed restore looks like a success.

### Restore: ClickHouse

```bash
cat /var/lib/gotcha/backup/clickhouse/events-2026-07-01.native | \
  clickhouse-client --user gotcha --password "$CH_PASSWORD" --database gotcha \
    --query "INSERT INTO events FORMAT Native"
```

Repeat for each table. The same caveat about clearing `transactions_5m`/`web_vitals_5m` **before** inserting, when restoring a second time into a non-empty database — with the same `clickhouse-client`, no `docker compose exec`.

### Restore from a filesystem snapshot

```bash
systemctl stop gotcha postgresql clickhouse-server
rm -rf /var/lib/postgresql/17/main/* /var/lib/clickhouse/*
tar xzf /var/lib/gotcha/backup/volumes-2026-07-01.tar.gz -C /
systemctl start postgresql clickhouse-server gotcha
```

`/var/lib/postgresql/17/main` and `/var/lib/clickhouse` are the standard data directories of the `postgresql-17`/`clickhouse-server` packages on Debian/Ubuntu. **This is a destructive operation**, the same caveat as in the Docker section: make sure it's the right archive before running it.

### Verify and cron

Post-restore verification is the same as in the Docker section above, just without `docker compose exec`:

```bash
/usr/local/bin/gotcha --healthcheck
curl -sf http://127.0.0.1:8080/readyz
```

The cron job has the same shape as the example above — the commands inside `backup.sh` become `sudo -u postgres pg_dump`/`clickhouse-client` without `docker compose exec`, and the backup directory is `/var/lib/gotcha/backup` instead of `backup/` inside the repository checkout.

## What's next

- [Installation](/docs/installation).
- [Installation without Docker](/docs/installation-bare-metal).
- [Upgrade](/docs/upgrade) — take a backup before every upgrade.
- [Configuration](/docs/configuration) — the `GOTCHA_*_RETENTION_DAYS` variables that control how much data accumulates in ClickHouse in the first place.
