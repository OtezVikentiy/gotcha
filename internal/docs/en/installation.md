# Installation

This guide assumes you've never deployed a Docker application or administered a Linux server before. Every command below is ready to copy and paste.

## What you need

- **A Linux server** (VPS/dedicated) — Ubuntu 22.04/24.04 or Debian 12 both work. CPU/RAM/disk requirements are in the table below.
- **Docker and Docker Compose** — the only dependency. You don't need to install PHP, nginx, or a database by hand — all of that is already packaged into containers.
- SSH access to the server.
- (Optional, but recommended for a real deployment) a domain name pointing at the server's IP.

## System requirements

Gotcha runs three processes on a single server: the app itself (Go), PostgreSQL, and ClickHouse. The main consumer of memory and disk is ClickHouse, which stores the telemetry (events, traces, metrics, profiles) — so it drives the requirements.

|      | Minimum | Recommended |
|------|---------|-------------|
| CPU  | 2 vCPU  | 4 vCPU      |
| RAM  | 2 GB    | 4 GB or more |
| Disk | 20 GB SSD | 40 GB SSD or more |

- **OS:** Ubuntu 22.04/24.04 or Debian 12, x86-64 (amd64) architecture.
- **RAM.** 2 GB is a workable minimum for getting started and light load (personal projects, staging). For production with a real stream of events, budget 4 GB and up: under load ClickHouse is more stable the more memory it has.
- **Disk.** Grows with the volume of telemetry and how long you retain it. 20 GB is enough to start; with noticeable traffic or long retention, plan for more and keep an eye on free space. Use an SSD — both ClickHouse and PostgreSQL are sensitive to disk latency.
- **CPU.** Two cores are enough; extra cores speed up ingesting bursts of events and ClickHouse queries.
- **Network.** Only a single application port needs to be exposed (59080 by default). PostgreSQL and ClickHouse are not exposed externally — they're reachable only inside the docker network.

## Step 1. Check whether Docker is already installed

SSH into the server and run:

```bash
docker --version
docker compose version
```

If both commands print a version number, Docker is already there — skip to step 2.

If you see `command not found`, install Docker with the official convenience script (works on Ubuntu/Debian):

```bash
curl -fsSL https://get.docker.com | sudo sh
```

After installing, add your user to the `docker` group so you don't need `sudo` for every command, then log back in for it to take effect:

```bash
sudo usermod -aG docker $USER
exit
```

SSH back in and run `docker --version` again — it should work now. Docker Compose (the `docker compose` command, with a space) is installed together with Docker by this script; no separate install is needed.

## Step 2. Get the Gotcha source

If `git` isn't installed, install it first (`sudo apt update && sudo apt install -y git` on Ubuntu/Debian). Then:

```bash
# gitflic (main, anonymous HTTPS)
git clone https://gitflic.ru/project/otezvikentiy/gotcha.git
# GitHub (mirror)
git clone https://github.com/OtezVikentiy/gotcha.git
# Contributors with SSH access can use:
# git clone git@gitflic.ru:otezvikentiy/gotcha.git
cd gotcha
```

This directory contains `docker-compose.yml` — a recipe file describing which three containers to start and how they're wired together. Run every command below from this directory (`gotcha/`).

## Step 3. Start the containers

```bash
docker compose up -d
```

What this does:

1. Docker builds the Gotcha application image (compiles the Go program inside a container — the first run can take a couple of minutes).
2. Three containers come up:
   - **`gotcha`** — the app itself: HTTP server, web UI, event ingestion from SDKs, database schema migrations on startup.
   - **`postgres`** (PostgreSQL 17) — stores "regular" state: users, organizations, projects, alert rules, incidents.
   - **`clickhouse`** (ClickHouse 25.3) — stores high-volume telemetry: the error events themselves, traces, metrics, profiles, uptime check results.
3. The `-d` flag ("detached") means "run in the background and give the terminal back" — the containers keep running after you close your SSH session.

Postgres and ClickHouse are **not** exposed to the host machine — they're only reachable inside the Docker network, between containers. Only the app's port is published to the host.

Check the status:

```bash
docker compose ps
```

All three rows should show `Up` (`postgres` and `clickhouse` show `Up (healthy)`; `gotcha` can take up to a minute to come up on the very first run while migrations apply).

## Step 4. Verify it's up

The app listens on host port **59080** by default (see `docker-compose.yml`: `"${GOTCHA_PORT:-59080}:8080"` — the host port is on the left, the container port on the right; a non-standard `59080` was chosen so it doesn't clash with other services on the server). Check the health endpoint:

```bash
curl -sf http://localhost:59080/healthz
```

A response like `{"clickhouse":"ok","postgres":"ok"}` with HTTP 200 means the app is alive and both databases are answering it. If curl hangs or errors out, see "Troubleshooting" below.

Ready-made shortcuts exist in the `Makefile` if you prefer `make`:

```bash
make up       # docker compose up -d
make ps       # docker compose ps
make health   # curl /healthz
make logs     # docker compose logs -f gotcha (Ctrl+C to exit)
```

Open `http://<your-server-IP>:59080` in a browser (or `http://localhost:59080` if you're browsing from the server itself / through an SSH tunnel) — the Gotcha login page should load.

## Step 5. Create the first user

Open `http://<your-server-address>:59080/register` and register.

**Important:** the very first user on a fresh instance is always allowed to register, regardless of the self-registration mode (`GOTCHA_REGISTRATION`), and is automatically granted **instance-admin** rights. This is the "bootstrap" step — it's how you get your first admin on a brand-new install without touching the database by hand. Every later signup is governed by `GOTCHA_REGISTRATION` (details in [Configuration](/docs/configuration)).

After logging in: create an organization, then a project inside it. The project's **"Setup"** page (a URL like `/projects/<id>/setup`, also reachable via the **"Setup"** button in the projects list) shows its DSN — the address your app's SDK sends data to (any language's official Sentry SDK works with Gotcha unmodified, since it speaks the same ingestion protocol). See [Getting Started](/docs/getting-started) and [SDK & Integrations](/docs/sdk) for the full walkthrough.

## Step 6. Set a secret key (required for a real server)

By default Gotcha uses `GOTCHA_SECRET_KEY=insecure-dev-secret`. That value is **public** — it's sitting right there in the source code on GitFlic, anyone can read it. It signs session cookies and OAuth state cookies; leaving the default on a server reachable over the internet lets an attacker who knows this key forge cookies and take over accounts through OAuth login (account takeover).

Because of this: if your `GOTCHA_BASE_URL` isn't `localhost`/`127.0.0.1` (i.e. you're running a real server, not local development), the app **refuses to start** in `web`/`all` mode until you set your own key.

Generate a random key:

```bash
openssl rand -base64 32
```

Create (or edit) an `.env` file **next to `docker-compose.yml`** — Docker Compose reads it automatically:

```bash
nano .env
```

and add (replace the value with the output of the command above):

```env
GOTCHA_SECRET_KEY=paste-your-random-string-from-openssl-here
```

Apply the change (this recreates the `gotcha` container with the new environment variable):

```bash
docker compose up -d
```

## Step 7. Set your public address (`GOTCHA_BASE_URL`)

`GOTCHA_BASE_URL` is the address users and SDKs actually reach your instance at. It's used to build: project DSNs (what you paste into your apps' code), links in invite emails, and incident links in alerts (Telegram/webhook/email). If it doesn't match the real address, those links will point to the wrong place.

Add to the same `.env`:

```env
GOTCHA_BASE_URL=https://gotcha.example.com
```

(or `http://<server-IP>:59080` if you don't have a domain/HTTPS yet — see the checklist below for why HTTPS matters). Apply it:

```bash
docker compose up -d
```

## Minimal production checklist

Before pointing real users or real application traffic at this instance, make sure:

- [ ] **`GOTCHA_SECRET_KEY`** — set to your own random value (step 6), not the default.
- [ ] **`GOTCHA_BASE_URL`** — points at the real public address.
- [ ] **HTTPS** — Gotcha doesn't terminate TLS itself; put a reverse proxy in front of it:
  - **nginx**: a config with `proxy_pass http://127.0.0.1:59080;` and a Let's Encrypt certificate (`certbot --nginx`).
  - **Caddy**: even simpler, HTTPS is automatic — a `Caddyfile` line like `gotcha.example.com { reverse_proxy localhost:59080 }` is all you need.

  Without HTTPS, session cookies travel over the network in plain text — the server even warns about this in its logs (`GOTCHA_BASE_URL is non-local plain HTTP`).
- [ ] **SMTP** — without it, invite emails and the email alert channel don't work. Setup is covered in [Configuration](/docs/configuration).
- [ ] **Backups** — set these up before real data accumulates in the database. See [Backup & Restore](/docs/backup-restore).
- [ ] **Quotas** — if a project DSN could leak publicly (e.g. frontend JS), set `GOTCHA_DEFAULT_*_QUOTA` (unlimited by default in the oss edition). See [Configuration](/docs/configuration).

## Troubleshooting

**Containers won't start / keep restarting.**
Check the logs:
```bash
docker compose logs -f gotcha
docker compose logs -f postgres
docker compose logs -f clickhouse
```
A common cause is a configuration error message (e.g. the requirement to set `GOTCHA_SECRET_KEY`, see step 6) right there in the `gotcha` log.

**Port already in use** (`bind: address already in use`).
Something on the server is already listening on 59080. Pick a different host port via `.env`:
```env
GOTCHA_PORT=8081
```
then `docker compose up -d`. The app inside the container still listens on 8080 — only which host port it's mapped to changes.

**The web UI doesn't load, even though containers show `Up`.**
- Check the server's firewall: `sudo ufw status` — if ufw is enabled, allow the port: `sudo ufw allow 59080/tcp`.
- If your server is with a cloud provider/hosting panel, check its Security Group / firewall separately from `ufw` — traffic is often blocked there instead.
- Run `curl -sf http://localhost:59080/healthz` **from the server itself** — if that works but access from outside doesn't, the problem is networking (firewall/provider), not Gotcha.

**Forms, registration or login return `forbidden` (403).**
Gotcha protects POST requests with an origin check: the request's `Origin`/`Referer` must match `GOTCHA_BASE_URL` by scheme and host. If you open the UI at an address other than `GOTCHA_BASE_URL` (e.g. via `http://localhost` while `BASE_URL` is a public HTTPS domain, or through a tunnel/proxy with a different host), any POST is rejected with `403`. Open the UI strictly at the `GOTCHA_BASE_URL` address.

**`/healthz` returns `503` with `unavailable` for postgres/clickhouse.**
The app is alive but can't reach one of the databases. This usually means the database hasn't finished starting yet (ClickHouse's first boot can take up to a minute) — wait and retry. If it persists, check `docker compose logs postgres` / `docker compose logs clickhouse`.

## What's next

- [Configuration](/docs/configuration) — the full environment variable reference.
- [Backup & Restore](/docs/backup-restore).
- [Upgrade](/docs/upgrade).
- [Getting Started](/docs/getting-started) — creating a project and your first event.
- [SSO](/docs/sso) — logging in via OIDC/Yandex ID/VK ID.
