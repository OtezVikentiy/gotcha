# Installation

This guide assumes you've never deployed a Docker application or administered a Linux server before. Every command below is ready to copy and paste.

## What you need

- **A Linux server** (VPS/dedicated) — Ubuntu 22.04/24.04, Debian 12, or a RedHat-family distribution (AlmaLinux 9/10, Rocky Linux 9/10, RHEL 9/10) all work. CPU/RAM/disk requirements are in the table below.
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

- **OS:** Ubuntu 22.04/24.04, Debian 12, or a RedHat-family distribution (AlmaLinux 9/10, Rocky Linux 9/10, RHEL 9/10), x86-64 (amd64) architecture. Gotcha runs in Docker, so the distribution barely matters — the differences are only in how you install Docker/git and in the firewall, and they are called out along the way.
- **RAM.** 2 GB is a workable minimum for getting started and light load (personal projects, staging). For production with a real stream of events, budget 4 GB and up: under load ClickHouse is more stable the more memory it has.
- **Disk.** Grows with the volume of telemetry and how long you retain it. 20 GB is enough to start; with noticeable traffic or long retention, plan for more and keep an eye on free space — for how, see [Monitoring gotcha itself](/docs/self-monitoring). Use an SSD — both ClickHouse and PostgreSQL are sensitive to disk latency.
- **CPU.** Two cores are enough; extra cores speed up ingesting bursts of events and ClickHouse queries.
- **Network.** Only a single application port is published to the host (59080 by default), but by default it only listens on loopback (`127.0.0.1`) — unreachable from outside the server until you explicitly open it (see step 4). PostgreSQL and ClickHouse aren't exposed at all — they're reachable only inside the docker network.

### If your server is at the minimum (2 vCPU / 2 GB)

Stock PostgreSQL and ClickHouse settings target large servers. On a minimal box they waste a noticeable share of it: ClickHouse keeps detailed system logs with no retention limit by default, and on a small machine servicing them costs more than the useful work does.

For such servers there's a ready-made overlay — start with both files:

```bash
docker compose -f docker-compose.yml -f docker-compose.small.yml up -d
```

Measured on a 2-core / 2 GB / 20 GB SSD VPS: ClickHouse memory dropped from 880 to 295 MB, disk usage from 12 to 9.2 GB, load average from 1.15 to 0.79.

The overlay sets ceilings that only make sense on weak hardware. **Do not apply it on a server with resources to spare** — there it would cap event ingestion and slow down queries. The regular `docker compose up -d` already includes the settings that help on any machine.

## Step 1. Check whether Docker is already installed

SSH into the server and run:

```bash
docker --version
docker compose version
```

If both commands print a version number, Docker is already there — skip to step 2.

If you see `command not found`, install Docker. The commands depend on which distribution family the server runs — pick your section.

### Ubuntu, Debian and other deb-based distributions

The official Docker script does everything for you:

```bash
curl -fsSL https://get.docker.com | sudo sh
```

Docker Compose (the `docker compose` command, with a space) comes with Docker; no separate install is needed. The service starts on its own.

### AlmaLinux, Rocky Linux, RHEL and other rpm-based distributions

There are two traps here that leave you with a server that looks installed but isn't.

**Trap one: `dnf install docker` does not install Docker.** The stock RedHat-family repositories carry no Docker package; what they have is `podman-docker`, a shim that emulates the `docker` command on top of Podman. It looks like this:

```
# dnf install docker
...
Installed: podman-docker-5.8.2-5.el9_8.noarch

# docker --version
Emulate Docker CLI using podman. Create /etc/containers/nodocker to quiet msg.
podman version 5.8.2

# sudo usermod -aG docker $USER
usermod: group 'docker' does not exist

# sudo systemctl enable --now docker
Failed to enable unit: Unit file docker.service does not exist.
```

There is no `docker` group and no `docker` service — because there is no Docker.

**Trap two: the official install script does not support AlmaLinux.** `curl -fsSL https://get.docker.com | sudo sh` answers `ERROR: Unsupported distribution 'almalinux'`.

The right path is Docker's own repository. If you installed `podman-docker` earlier you have to remove it: it owns the same `/usr/bin/docker` path, and installing Docker CE on top of it fails with a package conflict.

```bash
# 1. Remove the Podman shim if it was installed
sudo dnf remove -y podman-docker

# 2. Add Docker's official repository
sudo dnf install -y dnf-plugins-core
sudo dnf config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo

# 3. Install Docker and the Compose plugin
sudo dnf install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin

# 4. Start the service and enable it at boot (it does not start on its own)
sudo systemctl enable --now docker
```

The repository is the `centos` one on purpose: Docker doesn't build separate packages for AlmaLinux and Rocky, and the CentOS Stream builds work on them. Verified on AlmaLinux 9 and 10.

### After installing (any distribution)

Add your user to the `docker` group so you don't need `sudo` for every command, then log back in for it to take effect:

```bash
sudo usermod -aG docker $USER
exit
```

SSH back in and run `docker --version` and `docker compose version` — both should print a version number.

## Step 2. Get the Gotcha source

### If the server has git

```bash
# gitflic (main, anonymous HTTPS)
git clone https://gitflic.ru/project/otezvikentiy/gotcha.git
# GitHub (mirror)
git clone https://github.com/OtezVikentiy/gotcha.git
# Contributors with SSH access can use:
# git clone git@gitflic.ru:otezvikentiy/gotcha.git
cd gotcha
```

### If git is not installed

You don't have to install it — download an archive from the GitHub mirror and unpack it instead. Both `curl` and `tar` ship with every distribution:

```bash
curl -fsSL https://github.com/OtezVikentiy/gotcha/archive/refs/heads/main.tar.gz -o gotcha.tar.gz
tar xzf gotcha.tar.gz
cd gotcha-main
```

The only difference from `git clone` is how you update later: you download a fresh archive rather than running `git pull`. If you plan to update the instance regularly, git is still more convenient: `sudo apt install -y git` on Ubuntu/Debian, `sudo dnf install -y git` on AlmaLinux/Rocky/RHEL.

The unpacked directory contains `docker-compose.yml` — a recipe file describing which three containers to start and how they're wired together. Run every command below from that directory.

## Step 3. Start the containers

```bash
make up-rebuild
```

(`make` computes the git version and stamps it into the build — `/version` and the About page will name the exact release. If `make` isn't installed, `docker compose up -d` works too, but the instance will report "no build metadata" instead of a verifiable version.)

Dependencies are vendored (a `vendor/` directory in the repository), so building the image **does not reach the internet** for Go modules — it works in closed networks with no outbound access. If a build fails with `go mod download ... proxy.golang.org ... no route to host`, you're on an older, un-vendored version or a custom Dockerfile — update the repository to the current release.

What this does:

1. Docker builds the Gotcha application image (compiles the Go program inside a container — the first run can take a couple of minutes).
2. Three containers come up:
   - **`gotcha`** — the app itself: HTTP server, web UI, event ingestion from SDKs, database schema migrations on startup.
   - **`postgres`** (PostgreSQL 17) — stores "regular" state: users, organizations, projects, alert rules, incidents.
   - **`clickhouse`** (ClickHouse 25.3) — stores high-volume telemetry: the error events themselves, traces, metrics, profiles, uptime check results.
3. The `-d` flag ("detached") means "run in the background and give the terminal back" — the containers keep running after you close your SSH session.

Postgres and ClickHouse are **not** exposed to the host machine — they're only reachable inside the Docker network, between containers. The app's port is published to the host, but by default only on loopback — see step 4 below for the details and how to open it externally.

Check the status:

```bash
docker compose ps
```

All three rows should show `Up` (`postgres` and `clickhouse` show `Up (healthy)`; `gotcha` can take up to a minute to come up on the very first run while migrations apply).

## Step 4. Verify it's up

The app listens on host port **59080** by default (see `docker-compose.yml`: `"${GOTCHA_BIND:-127.0.0.1}:${GOTCHA_PORT:-59080}:8080"` — the host address and port are on the left, the container port on the right; a non-standard port `59080` was chosen so it doesn't clash with other services on the server). The host address on the left isn't `0.0.0.0` — it's **loopback** (`127.0.0.1`): the port is reachable from the server itself and over an SSH tunnel, but not from outside. That's a deliberate default: without it, a bare HTTP entry point and an unauthenticated `/metrics` (self-monitoring telemetry) would be reachable from outside, bypassing the HTTPS reverse proxy from the checklist below. Check the health endpoint:

```bash
curl -sf http://localhost:59080/readyz
```

A response like `{"status":"ready","clickhouse":"ok","postgres":"ok"}` with HTTP 200 means the app came up and both databases are answering it. If curl hangs or errors out, see "Troubleshooting" below.

There are two endpoints, and they answer different questions:

| Endpoint | Question | When it returns 503 |
|---|---|---|
| `/healthz` | is the process alive? | never, as long as it serves HTTP |
| `/readyz` | is it ready to work? | while PostgreSQL or ClickHouse is unreachable |

The difference matters when configuring an orchestrator: `/healthz` belongs on the liveness probe (restart a hung process), `/readyz` on readiness (stop sending traffic). Point liveness at `/readyz` and a storage outage turns into a restart of a healthy container — and every restart throws away whatever the buffers were holding while they waited for storage to come back.

Ready-made shortcuts exist in the `Makefile` if you prefer `make`:

```bash
make up       # docker compose up -d
make ps       # docker compose ps
make health   # curl /readyz
make logs     # docker compose logs -f gotcha (Ctrl+C to exit)
```

Open `http://localhost:59080` in a browser (if you're browsing from the server itself, or through an SSH tunnel) — the Gotcha login page should load. A direct `http://<your-server-IP>:59080` from another machine will **not** load by default — the port only listens on loopback (see above). From here, there are two paths:

- **Recommended.** Put a reverse proxy in front of the app on the same host (nginx/Caddy — see the checklist at the end of this guide): `proxy_pass`/`reverse_proxy` to `127.0.0.1:59080` reaches the app with no change to `docker-compose.yml`, and only the proxy's port 80/443 is exposed.
- **Quick, for a first look.** Add `GOTCHA_BIND=0.0.0.0` to `.env` and apply it with `docker compose up -d` — the port `59080` then listens on every interface, as it used to. Keep in mind: until you put an HTTPS proxy in front of it, a bare HTTP entry point and an unauthenticated `/metrics` (self-monitoring telemetry) are reachable from any address.

## Step 5. Set a secret key (required for a real server)

These two configuration steps come **before** creating the first user on purpose: registration is a POST request, and with a wrong `GOTCHA_BASE_URL` that very first POST is rejected with `403` (see step 6).

By default Gotcha uses `GOTCHA_SECRET_KEY=insecure-dev-secret`. That value is **public** — it's sitting right there in the source code on GitFlic, anyone can read it. It signs session cookies and OAuth state cookies; leaving the default on a server reachable over the internet lets an attacker who knows this key forge cookies and take over accounts through OAuth login (account takeover).

Because of this: if your `GOTCHA_BASE_URL` isn't `localhost`/`127.0.0.1` (i.e. you're running a real server, not local development), the app **refuses to start** in the `web`, `all`, `ingest`, and `uptime` modes (everywhere except `probe`) until you set your own key.

Generate a random key:

```bash
openssl rand -base64 32
```

Create an `.env` file **next to `docker-compose.yml`** — Docker Compose reads it automatically. Restrict its permissions in the same command: this file will hold the master key that encrypts alert channel and SSO secrets, and possibly an SMTP password ([Backup & Restore](/docs/backup-restore) already requires `600` for a copy of this file — the original must not be weaker):

```bash
cp .env.example .env && chmod 600 .env
nano .env
```

Uncomment `GOTCHA_SECRET_KEY` and replace its value with the output of the command above:

```env
GOTCHA_SECRET_KEY=paste-your-random-string-from-openssl-here
```

Apply the change (this recreates the `gotcha` container with the new environment variable):

```bash
docker compose up -d
```

## Step 6. Set your public address (`GOTCHA_BASE_URL`)

`GOTCHA_BASE_URL` is the address users and SDKs actually reach your instance at. It's used to build: project DSNs (what you paste into your apps' code), links in invite emails, and incident links in alerts (Telegram/webhook/email). It is also the reference for the origin check protecting every form: a POST coming from an address other than `GOTCHA_BASE_URL` is rejected with `403` — including the registration form on the next step.

Uncomment in the same `.env`:

```env
GOTCHA_BASE_URL=https://gotcha.example.com
```

(or `http://<server-IP>:59080` if you don't have a domain/HTTPS yet — but note that address is only reachable from outside once you've set `GOTCHA_BIND=0.0.0.0` as described in step 4; see the checklist below for why HTTPS matters). Apply it:

```bash
docker compose up -d
```

## Step 7. Create the first user

Open `<GOTCHA_BASE_URL>/register` (the address from step 6 — a domain behind your reverse proxy, or `http://<server-IP>:59080` with `GOTCHA_BIND=0.0.0.0` already set) and register.

**Important:** the very first user on a fresh instance is always allowed to register, regardless of the self-registration mode (`GOTCHA_REGISTRATION`), and is automatically granted **instance-admin** rights. This is the "bootstrap" step — it's how you get your first admin on a brand-new install without touching the database by hand. Every later signup is governed by `GOTCHA_REGISTRATION` (details in [Configuration](/docs/configuration)).

After logging in: create an organization, then a project inside it. The project's **"Setup"** page (a URL like `/projects/<id>/setup`, also reachable via the **"Setup"** button in the projects list) shows its DSN — the address your app's SDK sends data to (any language's official Sentry SDK works with Gotcha unmodified, since it speaks the same ingestion protocol). See [Getting Started](/docs/getting-started) and [SDK & Integrations](/docs/sdk) for the full walkthrough.

## Minimal production checklist

Before pointing real users or real application traffic at this instance, make sure:

- [ ] **`GOTCHA_SECRET_KEY`** — set to your own random value (step 5), not the default.
- [ ] **`GOTCHA_BASE_URL`** — points at the real public address.
- [ ] **HTTPS** — Gotcha doesn't terminate TLS itself; put a reverse proxy in front of it:
  - **nginx**: a config with `proxy_pass http://127.0.0.1:59080;` and a Let's Encrypt certificate (`certbot --nginx`).
  - **Caddy**: even simpler, HTTPS is automatic — a `Caddyfile` line like `gotcha.example.com { reverse_proxy localhost:59080 }` is all you need.

  Without HTTPS, session cookies travel over the network in plain text — the server even warns about this in its logs (`GOTCHA_BASE_URL is non-local plain HTTP`). If the proxy restricts paths to an allowlist, add `/install.sh` and `/agent/` to it — otherwise installing the host-metrics agent (see [Hosts](/docs/hosts)) won't work.
- [ ] **SMTP** — without it, invite emails and the email alert channel don't work. Setup is covered in [Configuration](/docs/configuration).
- [ ] **Backups** — set these up before real data accumulates in the database. See [Backup & Restore](/docs/backup-restore).
- [ ] **Quotas** — if a project DSN could leak publicly (e.g. frontend JS), set `GOTCHA_DEFAULT_*_QUOTA` (unlimited by default in the oss edition). See [Configuration](/docs/configuration).
- [ ] **Perimeter** — close off service paths (`/metrics`, `/version`, `/healthz`, `/readyz`) at the reverse proxy, turn off `server_tokens`, and set HSTS in exactly one place. See [Hardening your install](/docs/hardening).

## Troubleshooting

**Containers won't start / keep restarting.**
Check the logs:
```bash
docker compose logs -f gotcha
docker compose logs -f postgres
docker compose logs -f clickhouse
```
A common cause is a configuration error message (e.g. the requirement to set `GOTCHA_SECRET_KEY`, see step 5) right there in the `gotcha` log.

**Port already in use** (`bind: address already in use`).
Something on the server is already listening on 59080. Pick a different host port via `.env`:
```env
GOTCHA_PORT=8081
```
then `docker compose up -d`. The app inside the container still listens on 8080 — only which host port it's mapped to changes.

**The web UI doesn't load, even though containers show `Up`.**
- Check this first: by default the port only listens on loopback (`GOTCHA_BIND=127.0.0.1`), so direct access over the server's IP from outside **won't work until you add a reverse proxy or set `GOTCHA_BIND=0.0.0.0`** — see step 4. That's the default behavior, not a bug.
- If `GOTCHA_BIND=0.0.0.0` is already set (or you're using a reverse proxy listening on 80/443 rather than port 59080 itself), check the server's firewall. Ubuntu/Debian: `sudo ufw status` — if ufw is enabled, allow the port: `sudo ufw allow 59080/tcp`. AlmaLinux/Rocky/RHEL: firewalld is enabled by default — allow the port: `sudo firewall-cmd --permanent --add-port=59080/tcp && sudo firewall-cmd --reload`.
- If your server is with a cloud provider/hosting panel, check its Security Group / firewall separately from `ufw` — traffic is often blocked there instead.
- Run `curl -sf http://localhost:59080/healthz` **from the server itself** — if that works but access from outside doesn't, the problem is networking (firewall/provider), not Gotcha.

**Forms, registration or login return `forbidden` (403).**
Gotcha protects POST requests with an origin check: the request's `Origin`/`Referer` must match `GOTCHA_BASE_URL` by scheme and host. If you open the UI at an address other than `GOTCHA_BASE_URL` (e.g. via `http://localhost` while `BASE_URL` is a public HTTPS domain, or through a tunnel/proxy with a different host), any POST is rejected with `403`. Open the UI strictly at the `GOTCHA_BASE_URL` address.

**`/readyz` returns `503` with `unavailable` for postgres/clickhouse.**
The app is alive (`/healthz` returns 200) but can't reach one of the databases. This usually means the database hasn't finished starting yet (ClickHouse's first boot can take up to a minute) — wait and retry. If it persists, check `docker compose logs postgres` / `docker compose logs clickhouse`.

## What's next

- [Configuration](/docs/configuration) — the full environment variable reference.
- [Backup & Restore](/docs/backup-restore).
- [Upgrade](/docs/upgrade).
- [Getting Started](/docs/getting-started) — creating a project and your first event.
- [SSO](/docs/sso) — logging in via OIDC/Yandex ID/VK ID.
