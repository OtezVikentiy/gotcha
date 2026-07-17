# Backup & Restore

Gotcha keeps data in two separate databases, and both matter equally — you must back up **both together**, otherwise after a restore they'll be out of sync (e.g. a project exists in one database but its events are in the other, or vice versa).

| Database | What's in it | Container |
|---|---|---|
| **PostgreSQL** | Accounts, organizations, projects, members, alert rules, delivery channels, incidents, settings — everything you configured by hand in the UI. | `postgres` |
| **ClickHouse** | The error events themselves, trace spans, metric points, profiling samples, uptime check results — the full volume of telemetry your applications sent. | `clickhouse` |

Restoring only one of the two either breaks the UI (a project exists but has zero events for it) or, the other way around, loses your actual configuration (alerts, members, DSN keys) even if the telemetry is intact.

Every command below is run **from the repository directory** (`gotcha/`, the same place as `docker-compose.yml`) and uses `docker compose exec` — running a command inside an already-running container, without needing to publish the database ports to the host (they aren't published — see [Installation](/docs/installation)).

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

Then dump each table from that list (repeat for every row `SHOW TABLES` returned — e.g. `events`, `transactions`, `spans`, `metric_points`, `profile_samples`, `check_results`):

```bash
mkdir -p backup/clickhouse
for t in events transactions spans metric_points profile_samples check_results; do
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

## Restore: PostgreSQL

Restoring a `pg_dump` archive into an **empty** database (for a non-empty one, recreate the database first, or use `docker compose down -v`, which wipes all data — be careful):

```bash
gunzip -c backup/postgres-2026-07-01.sql.gz \
  | docker compose exec -T postgres psql -U gotcha -d gotcha
```

If restoring onto a new instance, first bring up the containers (`docker compose up -d`), wait for migrations to apply (see [Installation](/docs/installation)), make sure the schema is empty (freshly created), and only then load the dump. Restoring a dump onto a database where migrations have already run and it's non-empty causes primary key conflicts.

## Restore: ClickHouse

Restoring a table dumped in `Native` format with the reverse command:

```bash
cat backup/clickhouse/events-2026-07-01.native | \
  docker compose exec -T clickhouse clickhouse-client \
    --user gotcha --password gotcha --database gotcha \
    --query "INSERT INTO events FORMAT Native"
```

Repeat for each table. The table must already exist (created by migrations on app startup) and be empty, otherwise the data is appended to what's already there instead of replacing it.

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
curl -sf http://localhost:59080/healthz
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
docker compose exec -T postgres pg_dump -U gotcha -d gotcha \
  | gzip > backup/postgres-$(date +%F).sql.gz
for t in events transactions spans metric_points profile_samples check_results; do
  docker compose exec -T clickhouse clickhouse-client \
    --user gotcha --password gotcha --database gotcha \
    --query "SELECT * FROM $t FORMAT Native" \
    > backup/clickhouse/$t-$(date +%F).native
done
# keep 14 days
find backup -type f -mtime +14 -delete
```

Remember to make the script executable (`chmod +x backup.sh`), and — importantly — copy the `backup/` directory's contents **off this same server** (a different disk, S3-compatible storage, another server). A local-only copy won't help if the server itself fails.

## What's next

- [Installation](/docs/installation).
- [Upgrade](/docs/upgrade) — take a backup before every upgrade.
- [Configuration](/docs/configuration) — the `GOTCHA_*_RETENTION_DAYS` variables that control how much data accumulates in ClickHouse in the first place.
