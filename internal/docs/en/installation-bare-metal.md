# Bare-metal install (without Docker)

An alternative to the [Docker install](/docs/installation): Gotcha, PostgreSQL, and ClickHouse are installed as system packages directly on the host and run under systemd. This guide assumes the same level of preparation as the Docker path — a Linux server over SSH, no prior database administration experience.

**Support boundary.** This path is supported on the same footing as Docker — as long as you haven't hand-edited the script or the configs it produces (the systemd unit, `/etc/gotcha/gotcha.env`, the PostgreSQL/ClickHouse configs). Flags that keep full support: `--domain`, `--email`, `--no-proxy`, `--mem-limit`, `--base-url`, `--download-base`, `--version`, `--from-tarball`. One exception is `--skip-databases` with your own PostgreSQL/ClickHouse: since those databases aren't ours, we can help diagnose an issue but can't guarantee a fix — their versions, configuration, and availability are on you.

Installed either by the `install-bare-metal.sh` script (see "Installing via the script" below) or by hand with the same commands (see "Manual installation") — the manual path is not an appendix, it's a full path in its own right; the script only automates it.

## What you need

- **A Debian/Ubuntu-family Linux server** — official Ubuntu (22.04/24.04/26.04) or Debian (12/13), or any distribution derived from them (`ID_LIKE` contains `debian` or `ubuntu`). RedHat-family distributions (AlmaLinux, Rocky, RHEL) aren't supported on this path — for those, use the [Docker install](/docs/installation), which works on any distribution that has Docker. CI runs this path on Ubuntu 24.04 (amd64 and arm64).
- **Architecture:** amd64 or arm64.
- **systemd** — practically any current server already has it; check with `[ -d /run/systemd/system ] && echo ok`.
- **Root access** over SSH — the installer writes to `/etc`, `/opt`, `/usr/local/bin`, `/var/lib` and installs system packages.
- (Optional, but recommended for real use) a domain name pointing at the server's IP — needed for a TLS certificate.

## System requirements

|      | Minimum | Recommended |
|------|---------|-------------|
| CPU  | 2 vCPU  | 4 vCPU      |
| RAM  | 2 GB    | 4 GB or more |
| Disk | 20 GB SSD | 40 GB SSD or more |

The requirements are the same as the Docker path, and for the same reason: the main resource consumer is ClickHouse, not the delivery method. The script's preflight check accepts 1900 MB of RAM and up (not 2048) — some cloud "2 GB" plans never report a full 2048 MB of `MemTotal`, because part of the memory is reserved for firmware/hypervisor before the OS even starts.

Peak memory use on this path was measured separately from Docker (carrying over the Docker figure without measuring it here would be dishonest): across installer acceptance runs, the three processes (PostgreSQL, ClickHouse, the application) together peaked at 886–982 MB — treat the upper end of that range as the conservative figure to plan around. The measurement was taken in a container on a developer machine, not on a CI runner; if a CI run later shows a different peak, this number will need updating. Even the upper end is comparable to the 1006 MB measured for Docker on release 1.6.1 — the 2 GB minimum holds on both delivery paths.

The versions this path installs are pinned in the script and match the Docker delivery: PostgreSQL 17, ClickHouse 25.3, and the application itself is built with Go 1.26.

## Manual installation

Each step here is exactly what the `install-bare-metal.sh` script does, run by hand. Useful if your host's policy doesn't allow running third-party scripts as root, or if something in the automated install went wrong and you need to finish it manually.

### 1. Prepare the host

```bash
apt-get update
apt-get install -y curl tar gnupg openssl coreutils
```

Check the ports you'll need: 8080 (the app), 80 (if you're installing nginx), 5432/8123/9000 (if you're installing PostgreSQL/ClickHouse this way).

```bash
ss -ltn | grep -E ':(8080|80|5432|8123|9000)\b'
```

If something else is already listening on one of these ports, free it or drop it from the list you need (e.g. via `--skip-databases`/`--no-proxy` on the script).

### 2. Install PostgreSQL 17

Find the distribution's codename and add the official PGDG repository:

```bash
CODENAME=$(. /etc/os-release && printf '%s\n' "$VERSION_CODENAME")
curl -fsSL -o /tmp/pgdg.asc https://www.postgresql.org/media/keys/ACCC4CF8.asc
gpg --dearmor </tmp/pgdg.asc >/usr/share/keyrings/gotcha-pgdg.gpg
printf 'deb [signed-by=/usr/share/keyrings/gotcha-pgdg.gpg] https://apt.postgresql.org/pub/repos/apt %s-pgdg main\n' "$CODENAME" \
  >/etc/apt/sources.list.d/gotcha-pgdg.list
apt-get update
apt-get install -y postgresql-17
```

The PGDG signing key fingerprint is `B97B0AFCAA1A47F044F244A07FCC7D46ACCC4CF8` — verify it before importing (`gpg --with-colons --import-options show-only --import /tmp/pgdg.asc`) rather than trusting the download blindly.

If PGDG hasn't published packages for your codename yet (this happens with very fresh distribution releases), check whether the distribution's own repository already ships major version 17 (`apt-cache policy postgresql`) and install plain `postgresql` in that case. If the native major is a different one, you'll either need to wait for PGDG or install PostgreSQL 17 from another source yourself.

Set the tuning parameters that matter with a ClickHouse neighbor sharing the same disk:

```bash
CONF_DIR=$(find /etc/postgresql -mindepth 2 -maxdepth 2 -type d -name main | head -n1)
mkdir -p "$CONF_DIR/conf.d"
cat >"$CONF_DIR/conf.d/10-gotcha.conf" <<'EOF'
random_page_cost = 1.1
effective_io_concurrency = 200
EOF
systemctl restart postgresql
```

Create the role and database:

```bash
sudo -u postgres psql -c "CREATE ROLE gotcha LOGIN PASSWORD 'choose-your-own-password'"
sudo -u postgres psql -c "CREATE DATABASE gotcha OWNER gotcha"
```

DSN for step 6: `postgres://gotcha:<password>@127.0.0.1:5432/gotcha?sslmode=disable`.

### 3. Install ClickHouse 25.3

Add the ClickHouse repository:

```bash
curl -fsSL -o /tmp/clickhouse.asc https://packages.clickhouse.com/rpm/lts/repodata/repomd.xml.key
gpg --dearmor </tmp/clickhouse.asc >/usr/share/keyrings/gotcha-clickhouse.gpg
printf 'deb [signed-by=/usr/share/keyrings/gotcha-clickhouse.gpg] https://packages.clickhouse.com/deb stable main\n' \
  >/etc/apt/sources.list.d/gotcha-clickhouse.list
apt-get update
```

The ClickHouse signing key fingerprint is `3A9EA1193A97B548BE1457D48919F6BD2B48D754`; verify it the same way as PGDG's.

ClickHouse doesn't publish a package without an exact patch number, so find the patch for the major.minor you need and install all three packages pinned to it (otherwise apt pulls `clickhouse-common-static` at the latest major and hits a dependency conflict):

```bash
CH_PKG_VERSION=$(apt-cache madison clickhouse-server | awk -F'|' '{gsub(/^[ \t]+|[ \t]+$/,"",$2)} $2 ~ /^25\.3\./{print $2; exit}')
apt-get install -y "clickhouse-server=$CH_PKG_VERSION" "clickhouse-client=$CH_PKG_VERSION" "clickhouse-common-static=$CH_PKG_VERSION"
```

Copy the tuning configs from the release tarball (the same archive the binary comes from in step 5):

```bash
mkdir -p /etc/clickhouse-server/config.d
cp clickhouse/00-common.xml /etc/clickhouse-server/config.d/00-common.xml
```

On a host with less than 4 GB of RAM, also add the overlay for weak hardware (the same idea as `docker-compose.small.yml` on the Docker path):

```bash
cp clickhouse/10-small.xml /etc/clickhouse-server/config.d/10-small.xml
```

Create the `gotcha` user with a password (ClickHouse stores a SHA-256 hash, not the password itself):

```bash
CH_PASSWORD=$(openssl rand -hex 24)
CH_PASSWORD_HASH=$(printf '%s' "$CH_PASSWORD" | sha256sum | awk '{print $1}')
mkdir -p /etc/clickhouse-server/users.d
cat >/etc/clickhouse-server/users.d/10-gotcha.xml <<EOF
<clickhouse>
    <users>
        <gotcha>
            <password_sha256_hex>$CH_PASSWORD_HASH</password_sha256_hex>
            <networks>
                <ip>::1</ip>
                <ip>127.0.0.1</ip>
            </networks>
            <profile>default</profile>
            <quota>default</quota>
            <default_database>gotcha</default_database>
            <access_management>0</access_management>
        </gotcha>
    </users>
</clickhouse>
EOF
```

Raise the open-files limit (the stock one is too small under ClickHouse's load) and start the service:

```bash
mkdir -p /etc/systemd/system/clickhouse-server.service.d
cat >/etc/systemd/system/clickhouse-server.service.d/override.conf <<'EOF'
[Service]
LimitNOFILE=262144
EOF
systemctl daemon-reload
systemctl enable --now clickhouse-server
```

Wait for it to become ready and create the database:

```bash
until curl -fsS -o /dev/null http://127.0.0.1:8123/ping; do sleep 1; done
clickhouse-client --query "CREATE DATABASE IF NOT EXISTS gotcha"
```

DSN for step 6: `clickhouse://gotcha:<password>@127.0.0.1:9000/gotcha`.

### 4. Create the application's system user

```bash
useradd --system --no-create-home --shell /usr/sbin/nologin gotcha
```

No home directory and no interactive shell — the process runs as this user, nobody logs in as it.

### 5. Download and install the binary

Get the release tarball from GitHub (replace `X.Y.Z` and `<arch>` with amd64 or arm64):

```bash
curl -fsSL -o gotcha.tar.gz "https://github.com/OtezVikentiy/gotcha/releases/download/vX.Y.Z/gotcha-X.Y.Z-linux-<arch>.tar.gz"
curl -fsSL -o SHA256SUMS.txt "https://github.com/OtezVikentiy/gotcha/releases/download/vX.Y.Z/SHA256SUMS.txt"
grep " gotcha.tar.gz\$" SHA256SUMS.txt | sha256sum -c -
tar xzf gotcha.tar.gz
cd gotcha-X.Y.Z-linux-<arch>
```

Install the server binary and the agent distribution (the latter is what `/agent/gotcha-agent-linux-amd64` serves when connecting hosts, see [Hosts](/docs/hosts)):

```bash
install -m 0755 -o root -g root gotcha /usr/local/bin/gotcha
mkdir -p /opt/gotcha/agent-dist
cp -a agent-dist/. /opt/gotcha/agent-dist/
```

### 6. Create the environment file

```bash
mkdir -p /etc/gotcha
GOTCHA_SECRET=$(openssl rand -base64 48)
cat >/etc/gotcha/gotcha.env <<EOF
GOTCHA_PG_DSN=postgres://gotcha:<password-from-step-2>@127.0.0.1:5432/gotcha?sslmode=disable
GOTCHA_CH_DSN=clickhouse://gotcha:<password-from-step-3>@127.0.0.1:9000/gotcha
GOTCHA_SECRET_KEY=$GOTCHA_SECRET
GOTCHA_BASE_URL=https://gotcha.example.com
GOTCHA_DIST_DIR=/opt/gotcha/agent-dist
GOMEMLIMIT=819MiB
GOTCHA_LISTEN_ADDR=127.0.0.1:8080
EOF
chown root:gotcha /etc/gotcha/gotcha.env
chmod 0640 /etc/gotcha/gotcha.env
```

See the 403 warning in "Common issues" below about `GOTCHA_BASE_URL` — set it to the right address, scheme included, from the start. `GOTCHA_LISTEN_ADDR=127.0.0.1:8080` plays the same role as the loopback bind on the Docker path: without a reverse proxy, the port isn't reachable from outside. `GOMEMLIMIT=819MiB` is 80% of the unit's `MemoryMax=1024M` below; if you change the memory limit, recompute both together (the script's `--mem-limit` does this for you).

This file holds secrets (the encryption master key, both database passwords) — `0640` permissions and `root:gotcha` ownership are required, same as the `.env` file on the Docker path (see [Backup & Restore](/docs/backup-restore)).

### 7. Install the systemd unit

```bash
cat >/etc/systemd/system/gotcha.service <<'EOF'
[Unit]
Description=gotcha monitoring server
After=postgresql.service clickhouse-server.service network-online.target

[Service]
Type=simple
User=gotcha
Group=gotcha
EnvironmentFile=/etc/gotcha/gotcha.env
ExecStart=/usr/local/bin/gotcha
Restart=always
RestartSec=5
TimeoutStopSec=90

StateDirectory=gotcha
StateDirectoryMode=0700

ProtectSystem=strict
PrivateTmp=yes
ProtectHome=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LockPersonality=yes
SystemCallFilter=@system-service
SystemCallArchitectures=native
UMask=0077
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
MemoryDenyWriteExecute=yes

TasksMax=512
MemoryAccounting=yes
MemoryMax=1024M

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
```

`After=` without `Requires=` is deliberate: restarting a database shouldn't drag the application down with it — it survives a temporary database outage (see `/readyz` below) and recovers on its own via `Restart=always` once the database comes back.

### 8. Run migrations and start the app

```bash
systemd-run --pipe --wait --collect --uid=gotcha --gid=gotcha \
  --property="EnvironmentFile=/etc/gotcha/gotcha.env" \
  /usr/local/bin/gotcha --migrate-only
systemctl enable --now gotcha
```

Wait for it to pass its healthcheck (see "Self-check" below), or watch the log:

```bash
journalctl -u gotcha -f
```

### 9. Set up nginx (if you need external access)

Skip this step if you're publishing the instance behind an existing proxy, or only reaching it through an SSH tunnel on `127.0.0.1:8080`.

```bash
apt-get install -y nginx
rm -f /etc/nginx/sites-enabled/default
cat >/etc/nginx/sites-available/gotcha <<'EOF'
server {
    listen 80;
    server_name gotcha.example.com;
    client_max_body_size 64m;

    location ~ ^/(metrics|version)$ {
        allow 127.0.0.1;
        allow ::1;
        deny all;
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
EOF
ln -sf ../sites-available/gotcha /etc/nginx/sites-enabled/gotcha
nginx -t
systemctl enable --now nginx
systemctl reload nginx
```

### 10. Get a TLS certificate

This is a basic setup — nginx on port 80 plus a Let's Encrypt certificate. Fine-tuning TLS (protocols, ciphers), HSTS, and rate-limiting at the proxy are beyond this step; that's on the operator to configure for their own requirements.

```bash
apt-get install -y certbot python3-certbot-nginx
certbot --nginx -d gotcha.example.com -m you@example.com --agree-tos --non-interactive --redirect
```

A certbot failure doesn't break the HTTP setup already running on port 80 — the certificate can be obtained later with the same command.

## Installing via the script

`install-bare-metal.sh` performs exactly steps 1–10 above by itself, including an idempotent re-run (safe to run again — existing passwords and the secret key aren't reissued) and upgrade detection (if an older version is already on the host — see [Upgrade](/docs/upgrade)).

The script is attached to every release as a standalone file:

```bash
curl -fsSL -o install-bare-metal.sh \
  https://github.com/OtezVikentiy/gotcha/releases/download/vX.Y.Z/install-bare-metal.sh
chmod +x install-bare-metal.sh
sudo ./install-bare-metal.sh --version X.Y.Z --domain gotcha.example.com --email you@example.com
```

Without `--domain`/`--email` you get an HTTP-only setup with no TLS — a certificate can be added later with the same `certbot --nginx` command.

| Flag | Meaning |
|---|---|
| `--version X.Y.Z` | which release to install (required unless `--from-tarball` is given) |
| `--from-tarball PATH` | use a local tarball instead of downloading one |
| `--download-base URL` | a different download base than GitHub (mirror, closed network) |
| `--base-url URL` | explicit `GOTCHA_BASE_URL`; without it and without `--domain`, the script asks interactively (or warns and falls back to the host's IP with `--yes`) |
| `--domain D` | put nginx in front of this domain, `GOTCHA_BASE_URL` becomes `https://D` |
| `--email E` | contact for certbot (requires `--domain`) |
| `--no-proxy` | don't install or touch nginx at all |
| `--skip-databases` | don't install PostgreSQL/ClickHouse, use `--pg-dsn`/`--ch-dsn` — "diagnose, not guarantee" mode |
| `--pg-dsn DSN` / `--ch-dsn DSN` | external DSNs, required together with `--skip-databases` |
| `--mem-limit N` | `MemoryMax`/`GOMEMLIMIT` in MiB (default 1024, same as `mem_limit: 1g` in the Docker delivery) |
| `--dry-run` | print every command and file content, change nothing |
| `--yes` | don't prompt interactively (for CI and automation) |
| `--no-backup` | skip the pre-upgrade `pg_dump` |
| `--force-version` | allow installing a version older than the script itself |
| `--uninstall` | remove the install (data and databases are kept) |
| `--purge` | with `--uninstall`, also remove data and databases |

## Self-check

After installing (via the script or by hand), verify everything came up:

```bash
/usr/local/bin/gotcha --healthcheck
curl -sf http://127.0.0.1:8080/readyz
```

A `/readyz` response like `{"status":"ready","clickhouse":"ok","postgres":"ok"}` means the app can see both databases. If you installed nginx, do the same through the domain instead: `curl -sf https://gotcha.example.com/readyz`.

Check that agent binary serving works (without this, connecting hosts from the UI won't work, see [Hosts](/docs/hosts)):

```bash
curl -fsSI http://127.0.0.1:8080/agent/gotcha-agent-linux-amd64
```

Expect `200 OK`. Log into the UI, create an organization and a project, and send a test event through the project's DSN — this also exercises the `0700` permissions on the `StateDirectory` (`/var/lib/gotcha`): if the permissions were either too wide or too narrow, the application would either fail to write there or an audit would flag it. The simplest practical test of that same directory is running an error export (see [Exports](/docs/exports)): it writes a file to `/var/lib/gotcha/exports` and needs exactly those write permissions to succeed.

## Common issues

**Registration or any form returns `403`.** This is the origin-forgery check: `Origin`/`Referer` must match `GOTCHA_BASE_URL`. If `/etc/gotcha/gotcha.env` has an address that doesn't match how you actually open the UI (a missing scheme, `www` vs. no `www`, or reaching it by IP when `GOTCHA_BASE_URL` is a domain), the very first POST — including the first registration — is rejected with `403`. Fix `GOTCHA_BASE_URL` in the environment file and restart: `systemctl restart gotcha`.

**The first user.** On a fresh instance, whoever registers first is automatically granted instance-admin rights, regardless of the self-registration mode. Every later signup is governed by `GOTCHA_REGISTRATION_MODE` (see [Configuration](/docs/configuration)).

## Diagnostics

Three log sources:

```bash
journalctl -u gotcha --no-pager -n 100
journalctl -u postgresql --no-pager -n 50
journalctl -u clickhouse-server --no-pager -n 50
```

The install's own progress (survives even a process abort) is in `/var/log/gotcha-install.log`: a timestamped list of completed steps, useful if the install stopped partway through.

If the installer fails, it prints the list of steps already completed and the exit code. Re-running it with the same flags is idempotent: what's already done isn't redone, and a failed step picks up right where it left off.

## Removing the install

```bash
sudo ./install-bare-metal.sh --uninstall
```

Removes the `gotcha` unit and binary; leaves PostgreSQL, ClickHouse, and their data untouched. To remove those too: `--uninstall --purge` — irreversibly drops the `gotcha` role and database in PostgreSQL, the `gotcha` database in ClickHouse, the `gotcha` system user, and the `/var/lib/gotcha`, `/opt/gotcha`, `/etc/gotcha` directories. The database and nginx packages themselves aren't removed — something else on the host might be using them.

## What's next

- [Configuration](/docs/configuration) — the full environment variable reference (the same names as in `/etc/gotcha/gotcha.env` above).
- [Backup & Restore](/docs/backup-restore).
- [Upgrade](/docs/upgrade).
- [Hardening your install](/docs/hardening).
- [Hosts](/docs/hosts) — connecting servers via the agent.
- Deploy with Docker instead — [Installation](/docs/installation).
