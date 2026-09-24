# Bare-metal install (without Docker)

An alternative to the [Docker install](/docs/installation): Gotcha, PostgreSQL, and ClickHouse are installed as system packages directly on the host and run under systemd. This guide assumes the same level of preparation as the Docker path — a Linux server over SSH, no prior database administration experience.

**Support boundary.** This path is supported on the same footing as Docker — as long as you haven't hand-edited the script or the configs it produces (the systemd unit, `/etc/gotcha/gotcha.env`, the PostgreSQL/ClickHouse configs). Flags that keep full support: `--domain`, `--email`, `--no-proxy`, `--no-firewall`, `--mem-limit`, `--base-url`, `--download-base`, `--version`, `--from-tarball`. One exception is `--skip-databases` with your own PostgreSQL/ClickHouse: since those databases aren't ours, we can help diagnose an issue but can't guarantee a fix — their versions, configuration, and availability are on you.

Installed either by the `install-bare-metal.sh` script (see "Installing via the script" below) or by hand with the same commands (see "Manual installation") — the manual path is not an appendix, it's a full path in its own right; the script only automates it.

## What you need

- **A Linux server.** The list claims exactly the distributions and architectures CI installs on for every release — table below. Distributions derived from the claimed ones (`ID_LIKE` contains `debian`, `ubuntu`, `rhel`, or `fedora`) are accepted by the script too and most likely work — but we don't run them, so we don't claim them. **AlmaLinux/Rocky/RHEL 8 isn't supported** — preflight stops with an explicit error, 9 or 10 is required.
- **Architecture:** amd64 or arm64.
- **systemd** — practically any current server already has it; check with `[ -d /run/systemd/system ] && echo ok`.
- **Root access** over SSH — the installer writes to `/etc`, `/opt`, `/usr/local/bin`, `/var/lib` and installs system packages.
- (Optional, but recommended for real use) a domain name pointing at the server's IP — needed for a TLS certificate.

Distributions and architectures CI runs on every release:

| OS | Architectures |
|---|---|
| AlmaLinux 9 | amd64, arm64 |
| RHEL 9 | amd64, arm64 |
| AlmaLinux 10 | amd64, arm64 |
| RHEL 10 | amd64, arm64 |
| Rocky Linux 9 | amd64, arm64 |
| Rocky Linux 10 | amd64, arm64 |
| Debian 12 | amd64 |
| Debian 13 | amd64 |
| Ubuntu 24.04 | amd64, arm64 |
| Ubuntu 26.04 | amd64 |

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
DEBIAN_FRONTEND=noninteractive apt-get install -y curl tar gnupg openssl coreutils iproute2
```

`iproute2` (the `ss` command) is needed by the steps below and by the script: without it
there's no way to check the ports. A minimal Debian/Ubuntu image may lack it — the script's
preflight refuses with exit code 3 and names the missing package. The script creates the
PostgreSQL role as the `postgres` user via `runuser`, which ships in `util-linux`: it is
there by default on Debian, Ubuntu and EL 9, but RHEL 10 and its rebuilds install only
`util-linux-core`, so the full package has to be added. The preflight names a missing
`runuser` like any other missing command.

**On AlmaLinux/Rocky/RHEL 9 and 10:**

```bash
dnf install -y curl tar gnupg2 openssl coreutils iproute util-linux
```

Check the ports you'll need: 8080 (the app), 80 (if you're installing nginx), 5432/8123/9000 (if you're installing PostgreSQL/ClickHouse this way).

```bash
ss -ltn | grep -E ':(8080|80|5432|8123|9000)\b'
```

If something else is already listening on one of these ports, free it or drop it from the list you need (e.g. via `--skip-databases`/`--no-proxy` on the script).

### 2. Install PostgreSQL 17

The commands below split by distribution family from here on — the repository,
package manager, and data paths differ; the application's behavior doesn't.

**On Debian/Ubuntu:**

Find the distribution's codename and add the official PGDG repository:

```bash
CODENAME=$(. /etc/os-release && printf '%s\n' "$VERSION_CODENAME")
curl -fsSL -o /tmp/pgdg.asc https://www.postgresql.org/media/keys/ACCC4CF8.asc
gpg --dearmor </tmp/pgdg.asc >/usr/share/keyrings/gotcha-pgdg.gpg
KEY=/usr/share/keyrings/gotcha-pgdg.gpg
REPO=https://apt.postgresql.org/pub/repos/apt
printf 'deb [signed-by=%s] %s %s-pgdg main\n' "$KEY" "$REPO" "$CODENAME" \
  >/etc/apt/sources.list.d/gotcha-pgdg.list
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y postgresql-17
```

The PGDG (apt) signing key fingerprint is `B97B0AFCAA1A47F044F244A07FCC7D46ACCC4CF8` — verify it before importing (`gpg --with-colons --import-options show-only --import /tmp/pgdg.asc`) rather than trusting the download blindly.

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
runuser -u postgres -- psql -c "CREATE ROLE gotcha LOGIN PASSWORD 'choose-your-own-password'"
runuser -u postgres -- psql -c "CREATE DATABASE gotcha OWNER gotcha"
```

**On AlmaLinux/Rocky/RHEL 9 and 10:**

Add the official PGDG repository (the rpm key is different from the apt one) and
install the package together with `contrib`: the `citext` extension, which
migrations need, lives in a separate package on EL instead of inside `-server`,
as it does on Debian/Ubuntu.

PGDG signs the aarch64 repository metadata with a separate key from x86_64 —
a shared key for both architectures fails signature verification on arm64.

```bash
EL_MAJOR=$(. /etc/os-release && printf '%s\n' "${VERSION_ID%%.*}")
PGDG_RPM_KEY_URL=https://download.postgresql.org/pub/repos/yum/keys/PGDG-RPM-GPG-KEY-RHEL
if [ "$(uname -m)" = aarch64 ]; then
  PGDG_RPM_KEY_URL=https://download.postgresql.org/pub/repos/yum/keys/PGDG-RPM-GPG-KEY-AARCH64-RHEL
fi
curl -fsSL -o /tmp/pgdg.asc "$PGDG_RPM_KEY_URL"
mkdir -p /etc/pki/rpm-gpg
cp /tmp/pgdg.asc /etc/pki/rpm-gpg/gotcha-pgdg.asc
rpm --import /etc/pki/rpm-gpg/gotcha-pgdg.asc
cat >/etc/yum.repos.d/gotcha-pgdg.repo <<EOF
[pgdg-common]
name=PostgreSQL common RPMs for RHEL \$releasever - \$basearch
baseurl=https://download.postgresql.org/pub/repos/yum/common/redhat/rhel-$EL_MAJOR-\$basearch
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/gotcha-pgdg.asc

[pgdg17]
name=PostgreSQL 17 for RHEL \$releasever - \$basearch
baseurl=https://download.postgresql.org/pub/repos/yum/17/redhat/rhel-$EL_MAJOR-\$basearch
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/gotcha-pgdg.asc
EOF
[ "$EL_MAJOR" = 9 ] && dnf -y module disable postgresql
dnf install -y postgresql17-server postgresql17-contrib
```

The PGDG (rpm) signing key fingerprint is `D4BF08AE67A0B4C7A1DBCCD240BCA2B408B40D20`,
different from the apt key above; verify it the same way. On EL9, the distribution's
own `postgresql` module conflicts with the PGDG package and is disabled first; on
EL10 there's no such module, and the command is a no-op.

Initialize the cluster and set the same parameters — `postgresql.conf.sample`, which
`initdb` copies its config from, ships `include_dir` commented out, so the drop-in
below won't be picked up without the explicit append:

```bash
/usr/pgsql-17/bin/postgresql-17-setup initdb
mkdir -p /var/lib/pgsql/17/data/conf.d
cat >/var/lib/pgsql/17/data/conf.d/10-gotcha.conf <<'EOF'
random_page_cost = 1.1
effective_io_concurrency = 200
EOF
printf '%s\ninclude_dir = %s\n' '# gotcha: conf.d include' "'conf.d'" \
  >>/var/lib/pgsql/17/data/postgresql.conf
awk '$1=="host" && $4=="127.0.0.1/32" && ($5=="scram-sha-256" || $5=="md5"){f=1} END{exit f?0:1}' \
  /var/lib/pgsql/17/data/pg_hba.conf \
  || printf '%s\nhost all all 127.0.0.1/32 scram-sha-256\n' '# gotcha: conf.d include' \
       >>/var/lib/pgsql/17/data/pg_hba.conf
systemctl enable --now postgresql-17
```

Create the role and database — the PGDG package's `psql` isn't on `PATH`:

```bash
runuser -u postgres -- /usr/pgsql-17/bin/psql \
  -c "CREATE ROLE gotcha LOGIN PASSWORD 'choose-your-own-password'"
runuser -u postgres -- /usr/pgsql-17/bin/psql -c "CREATE DATABASE gotcha OWNER gotcha"
```

DSN for step 6 — the same on both families: `postgres://gotcha:<password>@127.0.0.1:5432/gotcha?sslmode=disable`.

### 3. Install ClickHouse 25.3

**On Debian/Ubuntu:**

Add the ClickHouse repository:

```bash
curl -fsSL -o /tmp/clickhouse.asc https://packages.clickhouse.com/rpm/lts/repodata/repomd.xml.key
gpg --dearmor </tmp/clickhouse.asc >/usr/share/keyrings/gotcha-clickhouse.gpg
KEY=/usr/share/keyrings/gotcha-clickhouse.gpg
REPO=https://packages.clickhouse.com/deb
printf 'deb [signed-by=%s] %s stable main\n' "$KEY" "$REPO" \
  >/etc/apt/sources.list.d/gotcha-clickhouse.list
apt-get update
```

The ClickHouse signing key fingerprint is `3A9EA1193A97B548BE1457D48919F6BD2B48D754`; verify it the same way as PGDG's.

ClickHouse doesn't publish a package without an exact patch number, so find the patch for the major.minor you need and install all three packages pinned to it (otherwise apt pulls `clickhouse-common-static` at the latest major and hits a dependency conflict):

```bash
CH_PKG_VERSION=$(apt-cache madison clickhouse-server \
  | awk -F'|' '{gsub(/^[ \t]+|[ \t]+$/,"",$2)} $2~/^25\.3\./{print $2; exit}')
DEBIAN_FRONTEND=noninteractive apt-get install -y \
  "clickhouse-server=$CH_PKG_VERSION" \
  "clickhouse-client=$CH_PKG_VERSION" \
  "clickhouse-common-static=$CH_PKG_VERSION"
```

`DEBIAN_FRONTEND=noninteractive` is not about a quiet log here: on a live terminal the `clickhouse-server` post-install script asks for a password for the `default` user, and any password set there breaks the database creation two steps below. The script installs the packages the same way.

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
printf 'ClickHouse, password of the gotcha user: %s\n' "$CH_PASSWORD"
```

Write the printed password down: the ClickHouse config holds only the SHA-256 hash, the password itself cannot be recovered from it, and step 6 needs it for the DSN. Losing it is not fatal, but you will have to issue a new one (recipe in "Common issues" below).

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

**On AlmaLinux/Rocky/RHEL 9 and 10:**

Add the repository the same way as for PGDG: download the key, verify its
fingerprint, import it with `rpm`, render the `.repo` file.

```bash
curl -fsSL -o /tmp/clickhouse.asc https://packages.clickhouse.com/rpm/stable/repodata/repomd.xml.key
mkdir -p /etc/pki/rpm-gpg
cp /tmp/clickhouse.asc /etc/pki/rpm-gpg/gotcha-clickhouse.asc
rpm --import /etc/pki/rpm-gpg/gotcha-clickhouse.asc
cat >/etc/yum.repos.d/gotcha-clickhouse.repo <<EOF
[gotcha-clickhouse]
name=ClickHouse
baseurl=https://packages.clickhouse.com/rpm/stable/
enabled=1
gpgcheck=0
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/gotcha-clickhouse.asc
EOF
```

The ClickHouse (rpm) signing key fingerprint is the same
`3A9EA1193A97B548BE1457D48919F6BD2B48D754` as the apt repository's; verify it the
same way. `gpgcheck=0` here isn't a weaker check: ClickHouse packages aren't
individually signed, not by us and not in their own `clickhouse.repo` either —
trust comes from `repo_gpgcheck=1` (the metadata is signed by the verified key and
carries a SHA-256 for every package).

ClickHouse doesn't publish a package without an exact patch number in it —
`dnf`'s version listing wraps long lines, so the version can end up as a
separate field on the next line:

```bash
CH_PKG_VERSION=$(dnf -qy --showduplicates list clickhouse-server 2>/dev/null | awk -v v=25.3. '
  function is_arch(s) { return s ~ /\.(noarch|x86_64|aarch64)$/ }
  function has_prefix(s) { return substr(s, 1, length(v)) == v }
  NF >= 2 && is_arch($1) { if (has_prefix($2)) print $2; next }
  NF >= 1 && !is_arch($1) { if (has_prefix($1)) print $1 }
' | sort -V | tail -n1)
dnf install -y \
  "clickhouse-server-$CH_PKG_VERSION" \
  "clickhouse-client-$CH_PKG_VERSION" \
  "clickhouse-common-static-$CH_PKG_VERSION"
```

From here it's the same as Debian/Ubuntu above, starting with copying the tuning
configs from the tarball: the `/etc/clickhouse-server/config.d`,
`/etc/clickhouse-server/users.d` paths, the systemd override, and the `/ping`
readiness check are identical on both families.

DSN for step 6: `clickhouse://gotcha:<password>@127.0.0.1:9000/gotcha`.

### 4. Create the application's system user

```bash
useradd --system --no-create-home --shell /usr/sbin/nologin gotcha
```

No home directory and no interactive shell — the process runs as this user, nobody logs in as it.

### 5. Download and install the binary

Get the release tarball from GitHub (replace `X.Y.Z` and `<arch>` with amd64 or arm64):

```bash
URL="https://github.com/OtezVikentiy/gotcha/releases/download/vX.Y.Z"
TARBALL="gotcha-X.Y.Z-linux-<arch>.tar.gz"
curl -fsSL -o "$TARBALL" "$URL/$TARBALL"
curl -fsSL -o SHA256SUMS.txt "$URL/SHA256SUMS.txt"
grep " $TARBALL\$" SHA256SUMS.txt | sha256sum -c -
tar xzf "$TARBALL"
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

If step 3 ran in this same shell session, you can put `$CH_PASSWORD` in place of `<password-from-step-3>` — the variable is still set and the heredoc expands it.

See the 403 warning in "Common issues" below about `GOTCHA_BASE_URL` — set it to the right address, scheme included, from the start. `GOTCHA_LISTEN_ADDR=127.0.0.1:8080` plays the same role as the loopback bind on the Docker path: without a reverse proxy, the port isn't reachable from outside. `GOMEMLIMIT=819MiB` is 80% of the unit's `MemoryMax=1024M` below; if you change the memory limit, recompute both together (the script's `--mem-limit` does this for you).

This file holds secrets (the encryption master key, both database passwords) — `0640` permissions and `root:gotcha` ownership are required, same as the `.env` file on the Docker path (see [Backup & Restore](/docs/backup-restore)).

### 7. Install the systemd unit

```bash
cat >/etc/systemd/system/gotcha.service <<'EOF'
[Unit]
Description=gotcha monitoring server
After=postgresql.service postgresql-17.service clickhouse-server.service network-online.target

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

**On Debian/Ubuntu:**

```bash
DEBIAN_FRONTEND=noninteractive apt-get install -y nginx
rm -f /etc/nginx/sites-enabled/default
cat >/etc/nginx/sites-available/gotcha <<'EOF'
# gotcha site: install-bare-metal.sh keeps local edits below on re-run
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

**On AlmaLinux/Rocky/RHEL 9 and 10:** the site path and package manager differ, the
config itself doesn't; EL has no separate `sites-enabled` directory or symlink —
`nginx.conf` includes everything from `conf.d` by default.

```bash
dnf install -y nginx
cat >/etc/nginx/conf.d/gotcha.conf <<'EOF'
# gotcha site: install-bare-metal.sh keeps local edits below on re-run
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
nginx -t
systemctl enable --now nginx
systemctl reload nginx
```

The first line of the file is a marker for the script: seeing it, a re-run of
`install-bare-metal.sh` leaves the site alone instead of re-rendering it, as long as the
`server_name` is still the same. That is how the TLS block certbot adds in step 11
survives an upgrade. To get a freshly rendered site, delete or rename the file and run the
script again; changing `--domain` re-renders it too (keeping a `gotcha.bak-<stamp>` copy
next to it), and the certificate is issued again after that.

### 10. SELinux and firewalld (AlmaLinux/Rocky/RHEL only)

Debian/Ubuntu doesn't have this step — SELinux and firewalld aren't part of the
install path there. On EL it runs right after step 9, if nginx is installed (no
`--no-proxy`), and before the certificate in step 11.

**SELinux.** If the policy is `Enforcing` (check with `getenforce`), let nginx reach
the app on `127.0.0.1:8080` — by default the `httpd_t` domain nginx runs under can't:

```bash
[ "$(getenforce 2>/dev/null)" = Enforcing ] && setsebool -P httpd_can_network_connect 1
```

**firewalld.** If it's running (`firewall-cmd --state` prints `running`), open
ports 80 and 443:

```bash
firewall-cmd --permanent --add-service=http --add-service=https
firewall-cmd --reload
```

If it's not installed or not running — the case on a stock AlmaLinux/Rocky
GenericCloud image, which ships without firewalld — the installer prints a notice
and leaves the host as it is; open the ports yourself if something else on the
host filters them. `--no-firewall` skips this step silently instead, since that's
an explicit request, not a surprise.

Both changes are global host settings, not files that belong to this install:
`--uninstall` doesn't revert either one, not on its own and not with `--purge`.
Another service on the same host may depend on them, so revert deliberately rather
than as one sweep with removing gotcha:

```bash
setsebool -P httpd_can_network_connect 0
firewall-cmd --permanent --remove-service=http --remove-service=https
firewall-cmd --reload
```

**EPEL for certbot.** The `python3-certbot-nginx` package step 11 needs lives in
EPEL, not the stock repositories:

```bash
dnf -y repolist enabled 2>/dev/null | grep -qi '^epel' \
  || dnf install -y "https://dl.fedoraproject.org/pub/epel/epel-release-latest-$EL_MAJOR.noarch.rpm"
```

That's enough on AlmaLinux and Rocky. On RHEL with an active subscription, some
EPEL dependencies also need the CodeReady Builder repository enabled — without it,
installing `certbot`/`python3-certbot-nginx` can fail on dependency resolution:

```bash
subscription-manager repos --enable "codeready-builder-for-rhel-$EL_MAJOR-$(arch)-rpms"
```

This path isn't exercised in CI (it needs an active RHEL subscription, not just an
image) — treat it as expected to work, not as verified automatically.

### 11. Get a TLS certificate

This is a basic setup — nginx on port 80 plus a Let's Encrypt certificate. Fine-tuning TLS (protocols, ciphers), HSTS, and rate-limiting at the proxy are beyond this step; that's on the operator to configure for their own requirements.

**On Debian/Ubuntu:**

```bash
DEBIAN_FRONTEND=noninteractive apt-get install -y certbot python3-certbot-nginx
certbot --nginx -d gotcha.example.com -m you@example.com --agree-tos --non-interactive --redirect
```

**On AlmaLinux/Rocky/RHEL 9 and 10:** EPEL from step 10 is already enabled, the rest
is the same:

```bash
dnf install -y certbot python3-certbot-nginx
certbot --nginx -d gotcha.example.com -m you@example.com --agree-tos --non-interactive --redirect
```

A certbot failure doesn't break the HTTP setup already running on port 80 on either
family — the certificate can be obtained later with the same command.

## Installing via the script

`install-bare-metal.sh` performs exactly steps 1–11 above by itself, including an idempotent re-run (safe to run again — existing passwords and the secret key aren't reissued) and upgrade detection (if an older version is already on the host — see [Upgrade](/docs/upgrade)).

The script is attached to every release as a standalone file:

```bash
URL="https://github.com/OtezVikentiy/gotcha/releases/download/vX.Y.Z"
curl -fsSL -o install-bare-metal.sh "$URL/install-bare-metal.sh"
chmod +x install-bare-metal.sh
sudo ./install-bare-metal.sh --version X.Y.Z --domain gotcha.example.com --email you@example.com
```

Without `--domain`/`--email` you get an HTTP-only setup with no TLS — a certificate can be added later with the same `certbot --nginx` command.

| Flag | Meaning |
|---|---|
| `--version X.Y.Z` | which release to install (required unless `--from-tarball` is given) |
| `--from-tarball PATH` | use a local tarball instead of downloading one |
| `--download-base URL` | a different download base than GitHub (mirror, closed network) |
| `--base-url URL` | explicit `GOTCHA_BASE_URL`; without it the script asks interactively (or warns and falls back to the host's IP with `--yes`) |
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

A `/readyz` response like `{"clickhouse":"ok","postgres":"ok","status":"ready","version":"X.Y.Z"}` means the app can see both databases. If you installed nginx, do the same through the domain instead: `curl -sf https://gotcha.example.com/readyz`.

The `version` field in that body is the exact build version, and both `/healthz` and `/readyz` hand it out without authentication. The nginx site the script installs leaves those two open on purpose: external availability checks of the instance itself use them. `/metrics` and `/version` are closed — both answer 403 from outside. If you would rather not expose the version, close the probes as well, see [Hardening](/docs/hardening).

Check that agent binary serving works (without this, connecting hosts from the UI won't work, see [Hosts](/docs/hosts)):

```bash
curl -fsSI http://127.0.0.1:8080/agent/gotcha-agent-linux-amd64
```

Expect `200 OK`. Log into the UI, create an organization and a project, and send a test event through the project's DSN — this also exercises the `0700` permissions on the `StateDirectory` (`/var/lib/gotcha`): if the permissions were either too wide or too narrow, the application would either fail to write there or an audit would flag it. The simplest practical test of that same directory is running an error export (see [Exports](/docs/exports)): it writes a file to `/var/lib/gotcha/exports` and needs exactly those write permissions to succeed.

## Common issues

**Registration or any form returns `403`.** This is the origin-forgery check: `Origin`/`Referer` must match `GOTCHA_BASE_URL`. If `/etc/gotcha/gotcha.env` has an address that doesn't match how you actually open the UI (a missing scheme, `www` vs. no `www`, or reaching it by IP when `GOTCHA_BASE_URL` is a domain), the very first POST — including the first registration — is rejected with `403`. Fix `GOTCHA_BASE_URL` in the environment file and restart: `systemctl restart gotcha`.

**The first user.** On a fresh instance, whoever registers first is automatically granted instance-admin rights, regardless of the self-registration mode. Every later signup is governed by `GOTCHA_REGISTRATION_MODE` (see [Configuration](/docs/configuration)).

**`clickhouse-client` answers `Code: 516 … default: Authentication failed`.** The ClickHouse `default` user has a password: either you answered the package post-install prompt (it appears when the packages are installed without `DEBIAN_FRONTEND=noninteractive`), or ClickHouse was already on this host. You can create the database with that password — `clickhouse-client --password --query "CREATE DATABASE IF NOT EXISTS gotcha"`. If the password is unknown and you have no use for the `default` user, drop it: `rm -f /etc/clickhouse-server/users.d/default-password.xml && systemctl restart clickhouse-server`. None of this affects the application — it connects to ClickHouse as the `gotcha` user.

**The application won't start: `clickhouse ping: code: 516 … gotcha: Authentication failed`.** The password in `GOTCHA_CH_DSN` doesn't match the one set for the `gotcha` user. After a script install the working password is in `/etc/gotcha/gotcha.env` itself — fix the DSN from it. After a manual install there is nowhere to recover it from (`users.d/10-gotcha.xml` holds only the SHA-256 hash), so issue a new one:

```bash
CH_PASSWORD=$(openssl rand -hex 24)
HASH=$(printf '%s' "$CH_PASSWORD" | sha256sum | awk '{print $1}')
TAG=password_sha256_hex
DSN="clickhouse://gotcha:$CH_PASSWORD@127.0.0.1:9000/gotcha"
sed -i "s#<$TAG>[a-f0-9]*</$TAG>#<$TAG>$HASH</$TAG>#" \
  /etc/clickhouse-server/users.d/10-gotcha.xml
systemctl restart clickhouse-server
sed -i "s#^GOTCHA_CH_DSN=.*#GOTCHA_CH_DSN=$DSN#" /etc/gotcha/gotcha.env
systemctl restart gotcha
```

**The install stops with exit code 5 and `port 5432 listens on 0.0.0.0:5432, not loopback only`** (same for 8123 and 9000). After installing the databases the script checks which addresses they actually listen on and refuses to go further unless that is `127.0.0.1`/`::1`. The refusal means exactly one thing: the host already had a PostgreSQL or ClickHouse configured for all interfaces, and the script reused it — so the promise that the databases are not exposed does not hold for this install. The check exists because silently ending up with a database on a public address is worse than an interrupted install.

Two ways out:

- put the database back on loopback and run the script again (it is idempotent): for PostgreSQL that is `listen_addresses = 'localhost'` in `postgresql.conf` (or in your own file under `conf.d/`) plus `systemctl restart postgresql`; for ClickHouse it is `<listen_host>` under `/etc/clickhouse-server/config.d/` plus `systemctl restart clickhouse-server`;
- if external access to that database is deliberate and it isn't "ours", install with `--skip-databases` and your own `--pg-dsn`/`--ch-dsn` — the script then neither touches nor checks the databases, and their availability and configuration stay with the operator.

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

Removes the `gotcha` unit and binary, and disables the nginx site (`/etc/nginx/sites-enabled/gotcha`) with a config reload: otherwise the host would answer 502 to everything, since the installer removed the default nginx site. The file itself, `/etc/nginx/sites-available/gotcha`, is kept — it holds the certbot TLS block, which is useful if you come back. PostgreSQL, ClickHouse and their data are left untouched.

To remove those too: `--uninstall --purge` — irreversibly drops the `gotcha` role and database in PostgreSQL, the `gotcha` database in ClickHouse, the `gotcha` system user, the `/var/lib/gotcha`, `/opt/gotcha`, `/etc/gotcha` directories, the install journal `/var/log/gotcha-install.log`, and the configs the script dropped into other packages' directories: `conf.d/10-gotcha.conf` for PostgreSQL, `config.d/00-common.xml` and `config.d/10-small.xml` for ClickHouse, and the `clickhouse-server.service.d/override.conf` systemd override. The databases are not restarted — that moment is the operator's to pick, and until they are, they keep running with the old settings.

Kept on purpose: the database and nginx packages (something else on the host might be using them), the PGDG and ClickHouse apt repositories together with their keyrings (removing only a keyring would break `apt-get update`), the site file in `sites-available`, and the databases' own data directories.

## What's next

- [Configuration](/docs/configuration) — the full environment variable reference (the same names as in `/etc/gotcha/gotcha.env` above).
- [Backup & Restore](/docs/backup-restore).
- [Upgrade](/docs/upgrade).
- [Hardening your install](/docs/hardening).
- [Hosts](/docs/hosts) — connecting servers via the agent.
- Deploy with Docker instead — [Installation](/docs/installation).
