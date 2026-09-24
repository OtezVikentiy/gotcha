#!/usr/bin/env bash
# gotcha bare-metal installer (Debian/Ubuntu and RHEL families, systemd).

GOTCHA_INSTALL_DEFAULT_VERSION="dev"
GOTCHA_INSTALL_DEFAULT_DOWNLOAD_BASE="https://github.com/OtezVikentiy/gotcha/releases/download"
GOTCHA_DOCS_BARE_METAL_URL="https://getgotcha.ru/docs/installation-bare-metal/"

# Источник истины — docker-compose.yml (postgres:17-alpine,
# clickhouse-server:25.3-alpine); сверяет internal/guards/docs_versions_test.go.
PG_MAJOR="17"
CH_VERSION="25.3"

# Отпечатки фиксируются руками (как digest баз в Dockerfile), не с сервера.
# PGDG_RPM_KEY_FINGERPRINT ≠ PGDG_KEY_FINGERPRINT: rpm и apt — разные ключи.
PGDG_KEY_FINGERPRINT="B97B0AFCAA1A47F044F244A07FCC7D46ACCC4CF8"
PGDG_RPM_KEY_FINGERPRINT="D4BF08AE67A0B4C7A1DBCCD240BCA2B408B40D20"
CLICKHOUSE_KEY_FINGERPRINT="3A9EA1193A97B548BE1457D48919F6BD2B48D754"

PGDG_RPM_KEY_URL="https://download.postgresql.org/pub/repos/yum/keys/PGDG-RPM-GPG-KEY-RHEL"
PGDG_RPM_KEY_PATH=/etc/pki/rpm-gpg/gotcha-pgdg.asc

# PGDG подписывает метаданные rpm-репозитория aarch64 отдельным ключом от
# x86_64 — общий gpgkey= на оба провалит проверку подписи на arm64.
PGDG_RPM_KEY_URL_ARM64="https://download.postgresql.org/pub/repos/yum/keys/PGDG-RPM-GPG-KEY-AARCH64-RHEL"
PGDG_RPM_KEY_FINGERPRINT_ARM64="B031F89FC983E98262906B6E177B343BB9738825"
CLICKHOUSE_RPM_KEY_PATH=/etc/pki/rpm-gpg/gotcha-clickhouse.asc
PG_INCLUDE_MARKER="# gotcha: conf.d include"

EXIT_OK=0
EXIT_OTHER=1
EXIT_USAGE=2
EXIT_PREFLIGHT=3
EXIT_DOWNLOAD=4
EXIT_DATABASE=5
EXIT_APP=6

usage() {
    cat <<'EOF'
Usage: install-bare-metal.sh [flags]

  --version X.Y.Z       release to install (default: the version this script ships with)
  --from-tarball PATH    use a local tarball instead of downloading one
  --download-base URL    base URL to download from instead of GitHub
  --base-url URL         GOTCHA_BASE_URL
  --skip-databases         do not install PostgreSQL/ClickHouse, use --pg-dsn/--ch-dsn
  --pg-dsn DSN            external PostgreSQL DSN
  --ch-dsn DSN            external ClickHouse DSN
  --mem-limit N           MemoryMax/GOMEMLIMIT in MiB (default: 1G, same as the app's docker-compose mem_limit)
  --dry-run                print every command and full file contents, change nothing
  --yes                    do not prompt (CI and automation)
  --no-backup              skip the pre-migration pg_dump on upgrade
  --force-version          allow installing a version older than this script
  --uninstall               remove the installation (data and databases are kept)
  --purge                   with --uninstall, also remove data and databases (prompts unless --yes)
EOF
}

# Принимает ID/ID_LIKE как аргументы, а не читает /etc/os-release сама —
# так функция остаётся чистой и тестируемой, detect_platform передаёт значения.
detect_distro() {
    local id="$1" id_like="${2:-}"
    case "$id" in
        ubuntu | debian) printf 'debian\n'; return 0 ;;
        almalinux | rocky | rhel | centos) printf 'rhel\n'; return 0 ;;
    esac
    case " $id_like " in
        *" debian "* | *" ubuntu "*) printf 'debian\n'; return 0 ;;
        *" rhel "* | *" fedora "*) printf 'rhel\n'; return 0 ;;
    esac
    return 1
}

detect_el_major() {
    case "${1%%.*}" in
        9 | 10) printf '%s\n' "${1%%.*}"; return 0 ;;
    esac
    return 1
}

detect_arch() {
    case "$1" in
        x86_64) printf 'amd64\n' ;;
        aarch64) printf 'arm64\n' ;;
        *) return 1 ;;
    esac
}

# Внутренняя часть detect_platform по уже известным HOST_FAMILY/EL_MAJOR —
# тестируется отдельно, целиком detect_platform читает /etc/os-release.
apply_platform_paths() {
    declare -gA PKG_HINTS=(
        [curl]=curl [tar]=tar [openssl]=openssl [sha256sum]=coreutils [runuser]=util-linux
    )
    if [ "$HOST_FAMILY" = rhel ]; then
        PG_UNIT="postgresql-$PG_MAJOR"
        PG_PACKAGE="postgresql${PG_MAJOR}-server"
        PG_BIN_DIR="/usr/pgsql-$PG_MAJOR/bin"
        NGINX_SITE=/etc/nginx/conf.d/gotcha.conf
        NGINX_SITE_ENABLED_LINK=""
        REPO_DIR=/etc/yum.repos.d
        PKG_HINT_LABEL="RHEL-family package"
        PKG_HINTS[gpg]=gnupg2
        PKG_HINTS[ss]=iproute
        PKG_HINTS[rpm]=rpm
        PKG_HINTS[dnf]=dnf
        return 0
    fi
    PG_UNIT=postgresql
    PG_PACKAGE="postgresql-$PG_MAJOR"
    PG_BIN_DIR=/usr/bin
    NGINX_SITE=/etc/nginx/sites-available/gotcha
    NGINX_SITE_ENABLED_LINK=/etc/nginx/sites-enabled/gotcha
    REPO_DIR=/etc/apt/sources.list.d
    PKG_HINT_LABEL="Debian/Ubuntu package"
    PKG_HINTS[gpg]=gnupg
    PKG_HINTS[ss]=iproute2
}

# Единственное место, читающее /etc/os-release во всём скрипте.
detect_platform() {
    [ -r /etc/os-release ] || fail "$EXIT_PREFLIGHT" "cannot read /etc/os-release"
    local id id_like version_id
    # Поля разделены переводом строки, не пробелом: ID_LIKE на EL — это
    # "rhel centos fedora", и чтение трёх полей через IFS=' ' склеило бы их.
    IFS=$'\n' read -r -d '' id id_like version_id < <(
        # shellcheck source=/dev/null
        . /etc/os-release
        printf '%s\n%s\n%s\0' "$ID" "${ID_LIKE:-}" "${VERSION_ID:-}"
    ) || true

    HOST_FAMILY=$(detect_distro "$id" "$id_like") \
        || fail "$EXIT_PREFLIGHT" "unsupported distribution: $id (Debian/Ubuntu or AlmaLinux/Rocky/RHEL family required)"

    EL_MAJOR=""
    HOST_CODENAME=""
    if [ "$HOST_FAMILY" = rhel ]; then
        # shellcheck disable=SC2034 # EL_MAJOR — часть интерфейса платформы для остальных шагов установки
        EL_MAJOR=$(detect_el_major "$version_id") || {
            case "${version_id%%.*}" in
                8) fail "$EXIT_PREFLIGHT" "AlmaLinux/Rocky/RHEL 8 is not supported, 9 or 10 required" ;;
                *) fail "$EXIT_PREFLIGHT" "unsupported $id release: $version_id (9 or 10 required)" ;;
            esac
        }
    else
        HOST_CODENAME=$(
            # shellcheck source=/dev/null
            . /etc/os-release
            printf '%s\n' "${VERSION_CODENAME:-}"
        )
    fi
    apply_platform_paths
}

pg_conf_dir_resolve() {
    if [ "$HOST_FAMILY" = rhel ]; then
        printf '%s\n' "/var/lib/pgsql/$PG_MAJOR/data"
        return 0
    fi
    find /etc/postgresql -mindepth 2 -maxdepth 2 -type d -name main 2>/dev/null | head -n1
}

pg_conf_dir_label() {
    if [ "$HOST_FAMILY" = rhel ]; then
        printf '%s\n' "/var/lib/pgsql/$PG_MAJOR/data"
        return 0
    fi
    printf '%s\n' '/etc/postgresql/*/main'
}

pkg_refresh() {
    if [ "$HOST_FAMILY" = rhel ]; then
        dnf -qy makecache >/dev/null
        return $?
    fi
    apt-get update -qq >/dev/null
}

# >/dev/null обязателен в обеих функциях: install_postgresql возвращает DSN
# через stdout, и болтливость dnf/dpkg подставилась бы в GOTCHA_PG_DSN.
pkg_install() {
    if [ "$HOST_FAMILY" = rhel ]; then
        dnf -qy install "$@" >/dev/null
        return $?
    fi
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@" >/dev/null
}

# "v0.2.0-5-gabcdef-dirty" от локально собранного бинаря — такой же законный вход,
# как "1.6.1": ведущий v и всё после первого дефиса/плюса к сравнению не относятся.
normalize_version() {
    local v="${1#v}"
    v="${v%%-*}"
    printf '%s\n' "${v%%+*}"
}

is_semver() {
    [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
}

# Путь параметром — тестируемость, тот же принцип, что у detect_distro. Тарболы до
# этой правки несли версию без "v", новые — с ним; снимаем префикс ради одного формата.
installed_version() {
    local binary="$1"
    [ -x "$binary" ] || return 0
    local out
    out=$("$binary" --version 2>/dev/null | awk '{print $2}')
    printf '%s\n' "${out#v}"
}

# Сравнение по числовым сегментам X.Y.Z — лексикографическое здесь неверно
# (1.10.0 < 1.9.0 посимвольно, хотя 1.10.0 новее).
version_ge() {
    local a b
    a=$(normalize_version "$1")
    b=$(normalize_version "$2")
    local -a av bv
    IFS=. read -r -a av <<<"$a"
    IFS=. read -r -a bv <<<"$b"
    local i x y
    for i in 0 1 2; do
        # Сегмент режется до ведущих цифр: под set -u нечисловой остаток уронил бы
        # (( )) кодом 1 вместо контрактного отказа.
        x="${av[i]:-0}"
        x="${x%%[!0-9]*}"
        y="${bv[i]:-0}"
        y="${y%%[!0-9]*}"
        if ((10#${x:-0} > 10#${y:-0})); then
            return 0
        fi
        if ((10#${x:-0} < 10#${y:-0})); then
            return 1
        fi
    done
    return 0
}

BASE_URL_EXAMPLE="https://gotcha.example.com"

normalize_base_url() {
    local url="$1"
    while [ "${url%/}" != "$url" ]; do
        url="${url%/}"
    done
    printf '%s\n' "$url"
}

# Значение пишется в env без кавычек, поэтому белый список, а не «всё, что примет Go»:
# сторож internal/guards держит его подмножеством baseurl.Normalize.
validate_base_url() {
    local raw="$1"
    local re='^https?://([A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?|\[[0-9A-Fa-f:.]+\])(:[0-9]{1,5})?(/([A-Za-z0-9._~:/-]|%[0-9A-Fa-f]{2})*)?$'
    if [[ ! "$raw" =~ $re ]]; then
        printf "install-bare-metal: invalid address '%s': need http(s)://host[:port][/path] without query, fragment, spaces or quotes (e.g. %s)\n" \
            "$raw" "$BASE_URL_EXAMPLE" >&2
        return 1
    fi
    normalize_base_url "$raw"
}

# Семантика systemd EnvironmentFile: последняя строка ключа побеждает, внешние кавычки
# снимаются. rc 1 — ключа нет; пустое значение — это «есть».
env_get() {
    local key="$1" file="$2" line value
    [ -r "$file" ] || return 1
    line=$(grep -E "^${key}=" "$file" | tail -n1) || true
    [ -n "$line" ] || return 1
    value="${line#"$key"=}"
    value="${value%"${value##*[![:space:]]}"}"
    case "$value" in
        \"*\") value="${value#\"}"; value="${value%\"}" ;;
        \'*\') value="${value#\'}"; value="${value%\'}" ;;
    esac
    printf '%s\n' "$value"
}

stdin_is_tty() {
    [ -t 0 ]
}

resolve_base_url() {
    local flag="$1" env_file="$2" yes="$3" from_env answer url
    if [ -n "$flag" ]; then
        printf '%s\n' "$flag"
        return 0
    fi
    if [ -f "$env_file" ]; then
        from_env=$(env_get GOTCHA_BASE_URL "$env_file") \
            || fail "$EXIT_USAGE" "$env_file has no GOTCHA_BASE_URL line; pass --base-url (the address users will type in the browser, e.g. $BASE_URL_EXAMPLE)"
        normalize_base_url "$from_env"
        return 0
    fi
    if [ -n "$yes" ] || ! stdin_is_tty; then
        fail "$EXIT_USAGE" "--base-url is required for a new installation (the address users will type in the browser, e.g. $BASE_URL_EXAMPLE)"
    fi
    while :; do
        read -r -p "Address users will open Gotcha at (e.g. $BASE_URL_EXAMPLE): " answer \
            || fail "$EXIT_USAGE" "no address given; pass --base-url"
        if [ -z "$answer" ]; then
            printf 'install-bare-metal: an address is required\n' >&2
            continue
        fi
        url=$(validate_base_url "$answer") && { printf '%s\n' "$url"; return 0; }
    done
}

plain_http_warning() {
    case "$1" in
        http://*)
            printf 'WARNING: %s is plain HTTP: session cookies and passwords travel unencrypted, use it only inside a closed network\n' "$1"
            return 0
            ;;
    esac
    return 1
}

# Глобальные ARG_* вместо структуры — main/preflight читают их напрямую.
# Каждый вызов сбрасывает их к дефолтам для повторных вызовов тест-раннера.
parse_args() {
    ARG_VERSION="$GOTCHA_INSTALL_DEFAULT_VERSION"
    ARG_FROM_TARBALL=""
    ARG_DOWNLOAD_BASE="$GOTCHA_INSTALL_DEFAULT_DOWNLOAD_BASE"
    ARG_BASE_URL=""
    ARG_SKIP_DATABASES=""
    ARG_PG_DSN=""
    ARG_CH_DSN=""
    ARG_MEM_LIMIT=""
    ARG_DRY_RUN=""
    ARG_YES=""
    ARG_NO_BACKUP=""
    ARG_FORCE_VERSION=""
    ARG_UNINSTALL=""
    ARG_PURGE=""
    ARG_HELP=""

    local key val
    while [ $# -gt 0 ]; do
        key="$1"
        case "$key" in
            --no-proxy | --no-firewall)
                printf 'install-bare-metal: %s is deprecated and does nothing: the installer no longer touches a web server or firewalld\n' "$key" >&2
                shift
                continue
                ;;
            --domain | --email)
                printf '%s\n' 'install-bare-metal: --domain/--email were removed in 1.9.0: the installer no longer sets up a web server or TLS. Pass --base-url https://<domain> and put your own reverse proxy in front of 127.0.0.1:8080 — see "External access and TLS" in the installation guide.' >&2
                return "$EXIT_USAGE"
                ;;
            --skip-databases)
                ARG_SKIP_DATABASES=1
                shift
                continue
                ;;
            --dry-run)
                ARG_DRY_RUN=1
                shift
                continue
                ;;
            --yes)
                ARG_YES=1
                shift
                continue
                ;;
            --no-backup)
                ARG_NO_BACKUP=1
                shift
                continue
                ;;
            --force-version)
                ARG_FORCE_VERSION=1
                shift
                continue
                ;;
            --uninstall)
                ARG_UNINSTALL=1
                shift
                continue
                ;;
            --purge)
                ARG_PURGE=1
                shift
                continue
                ;;
            -h | --help)
                ARG_HELP=1
                usage
                return "$EXIT_OK"
                ;;
        esac
        if [ $# -lt 2 ]; then
            printf 'install-bare-metal: %s requires a value\n' "$key" >&2
            return "$EXIT_USAGE"
        fi
        val="$2"
        case "$key" in
            --version) ARG_VERSION="$val" ;;
            --from-tarball) ARG_FROM_TARBALL="$val" ;;
            --download-base) ARG_DOWNLOAD_BASE="$val" ;;
            --base-url) ARG_BASE_URL="$val" ;;
            --pg-dsn) ARG_PG_DSN="$val" ;;
            --ch-dsn) ARG_CH_DSN="$val" ;;
            --mem-limit) ARG_MEM_LIMIT="$val" ;;
            *)
                printf 'install-bare-metal: unknown argument: %s\n' "$key" >&2
                return "$EXIT_USAGE"
                ;;
        esac
        shift 2
    done

    if [ -n "$ARG_PURGE" ] && [ -z "$ARG_UNINSTALL" ]; then
        printf 'install-bare-metal: --purge requires --uninstall\n' >&2
        return "$EXIT_USAGE"
    fi
    if [ -n "$ARG_BASE_URL" ]; then
        ARG_BASE_URL=$(validate_base_url "$ARG_BASE_URL") || return "$EXIT_USAGE"
    fi
    if [ -n "$ARG_SKIP_DATABASES" ] && { [ -z "$ARG_PG_DSN" ] || [ -z "$ARG_CH_DSN" ]; }; then
        printf 'install-bare-metal: --skip-databases requires --pg-dsn and --ch-dsn\n' >&2
        return "$EXIT_USAGE"
    fi
    if [ -n "$ARG_MEM_LIMIT" ] && [[ ! "$ARG_MEM_LIMIT" =~ ^[0-9]+$ ]]; then
        printf 'install-bare-metal: --mem-limit must be a whole number of MiB (got: %s)\n' "$ARG_MEM_LIMIT" >&2
        return "$EXIT_USAGE"
    fi
    # --uninstall/--purge take no version at all — the checks below are about
    # what to install, not relevant to removing what is already there.
    if [ -z "$ARG_UNINSTALL" ]; then
        if [ "$ARG_VERSION" != "dev" ] && ! is_semver "$ARG_VERSION"; then
            printf 'install-bare-metal: --version must be X.Y.Z without a suffix (got: %s)\n' "$ARG_VERSION" >&2
            return "$EXIT_USAGE"
        fi
        if [ "$ARG_VERSION" = "dev" ] && [ -z "$ARG_FROM_TARBALL" ]; then
            printf 'install-bare-metal: this is a repository copy (version "dev"); pass --version X.Y.Z or --from-tarball, or download the script from a release instead\n' >&2
            return "$EXIT_USAGE"
        fi
        if [ "$GOTCHA_INSTALL_DEFAULT_VERSION" != "dev" ] && [ -z "$ARG_FORCE_VERSION" ]; then
            if ! version_ge "$ARG_VERSION" "$GOTCHA_INSTALL_DEFAULT_VERSION"; then
                printf 'install-bare-metal: refusing to install %s, older than this script (%s); pass --force-version to override\n' \
                    "$ARG_VERSION" "$GOTCHA_INSTALL_DEFAULT_VERSION" >&2
                return "$EXIT_USAGE"
            fi
        fi
    fi

    return "$EXIT_OK"
}

# MemoryMax паритетно compose (mem_limit: 1g) независимо от RAM хоста —
# preflight и так отсекает хосты младше 2 ГБ. 0.8 — тот же запас, что defaultRatio.
compute_memlimit() {
    local mem_max=1024
    printf '%sM %sMiB\n' "$mem_max" "$((mem_max * 8 / 10))"
}

# Сводит --mem-limit и дефолт compute_memlimit в одну проверяемую точку,
# которую main вызывает без ветвления.
resolve_memlimit() {
    local mem_limit_arg="$1"
    if [ -n "$mem_limit_arg" ]; then
        printf '%sM %sMiB\n' "$mem_limit_arg" "$((mem_limit_arg * 8 / 10))"
        return
    fi
    compute_memlimit
}

# Паритет с compose построчно (спека §5) плюс усиление сверх него.
render_unit() {
    local memory_max="$1"
    cat <<EOF
[Unit]
Description=gotcha monitoring server
After=postgresql.service postgresql-$PG_MAJOR.service clickhouse-server.service network-online.target

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
MemoryMax=$memory_max

[Install]
WantedBy=multi-user.target
EOF
}

ENV_FILE_OWNER=root:gotcha
TRUSTED_PROXIES_LOOPBACK="127.0.0.1/32,::1/128"

render_env_file() {
    local pg_dsn="$1" ch_dsn="$2" secret_key="$3" base_url="$4" \
        dist_dir="$5" gomemlimit="$6" listen_addr="$7"
    cat <<EOF
GOTCHA_PG_DSN=$pg_dsn
GOTCHA_CH_DSN=$ch_dsn
GOTCHA_SECRET_KEY=$secret_key
GOTCHA_BASE_URL=$base_url
GOTCHA_DIST_DIR=$dist_dir
GOMEMLIMIT=$gomemlimit
GOTCHA_LISTEN_ADDR=$listen_addr
GOTCHA_TRUSTED_PROXIES=$TRUSTED_PROXIES_LOOPBACK
EOF
}

NGINX_SITE_MARKER="# gotcha site: install-bare-metal.sh keeps local edits below on re-run"

nginx_site_disabled_path() {
    printf '%s.disabled\n' "$NGINX_SITE"
}

find_legacy_site() {
    local candidate
    local -a candidates=()
    if [ -n "$NGINX_SITE_ENABLED_LINK" ] && [ -L "$NGINX_SITE_ENABLED_LINK" ]; then
        candidates+=("$(readlink -f "$NGINX_SITE_ENABLED_LINK")")
    fi
    candidates+=("$NGINX_SITE" "$(nginx_site_disabled_path)")
    for candidate in "${candidates[@]}"; do
        if [ -f "$candidate" ] && grep -qF "$NGINX_SITE_MARKER" "$candidate"; then
            printf '%s\n' "$candidate"
            return 0
        fi
    done
    return 1
}

# Без маркера сайт настроен оператором сам (в том числе по нашей доке) — не наш.
uninstall_legacy_site() {
    local site
    site=$(find_legacy_site) || return 0
    [ "$site" != "$(nginx_site_disabled_path)" ] || return 0
    if [ "$HOST_FAMILY" = rhel ]; then
        mv "$NGINX_SITE" "$(nginx_site_disabled_path)" \
            || { log_step "WARNING: could not disable the nginx site $NGINX_SITE, disable it by hand"; return 0; }
        systemctl reload nginx >/dev/null 2>&1 || true
        log_step "nginx site from a previous version disabled (kept as $(nginx_site_disabled_path))"
        log_step "SELinux boolean httpd_can_network_connect and firewalld services http/https, if a previous version set them, are left as they are (revert: setsebool -P httpd_can_network_connect 0; firewall-cmd --permanent --remove-service=http --remove-service=https && firewall-cmd --reload)"
        return 0
    fi
    [ -L "$NGINX_SITE_ENABLED_LINK" ] || [ -e "$NGINX_SITE_ENABLED_LINK" ] || return 0
    rm -f "$NGINX_SITE_ENABLED_LINK"
    systemctl reload nginx >/dev/null 2>&1 || true
    log_step "nginx site from a previous version disabled (the file in sites-available is kept)"
}

legacy_site_enable_hint() {
    local site="$1"
    if [ "$site" = "$(nginx_site_disabled_path)" ]; then
        # Рабочий $NGINX_SITE уже занят (оператором или новой доступной доке) — mv затёр бы его.
        [ ! -e "$NGINX_SITE" ] || return 1
        if [ "$HOST_FAMILY" = rhel ]; then
            printf 'mv %s %s && systemctl reload nginx\n' "$site" "$NGINX_SITE"
            return 0
        fi
        if [ -n "$NGINX_SITE_ENABLED_LINK" ] && [ ! -e "$NGINX_SITE_ENABLED_LINK" ] && [ ! -L "$NGINX_SITE_ENABLED_LINK" ]; then
            printf 'mv %s %s && ln -s %s %s && systemctl reload nginx\n' \
                "$site" "$NGINX_SITE" "$NGINX_SITE" "$NGINX_SITE_ENABLED_LINK"
            return 0
        fi
        return 1
    fi
    if [ -n "$NGINX_SITE_ENABLED_LINK" ] && [ ! -e "$NGINX_SITE_ENABLED_LINK" ] && [ ! -L "$NGINX_SITE_ENABLED_LINK" ]; then
        printf 'ln -s %s %s && systemctl reload nginx\n' "$site" "$NGINX_SITE_ENABLED_LINK"
        return 0
    fi
    return 1
}

# :PORT и 0.0.0.0:PORT слушают все интерфейсы — curl/proxy бьют по loopback того же хоста.
readyz_probe_addr() {
    local addr="$1"
    case "$addr" in
        :*) addr="127.0.0.1$addr" ;;
        0.0.0.0:*) addr="127.0.0.1:${addr#*:}" ;;
    esac
    printf '%s\n' "$addr"
}

summary_effective_version() {
    printf '%s\n' "${1:-$2}"
}

summary_is_fresh() {
    [ -n "$1" ] || printf '1\n'
}

render_summary() {
    local version="$1" readiness="$2" base_url="$3" listen_addr="$4" legacy_site="$5" \
        enable_hint="$6" fresh="$7"
    printf 'Gotcha %s is installed and running.\n' "$version"
    printf '  readiness:  %s\n' "$readiness"
    case "$listen_addr" in
        127.* | localhost:* | '[::1]':*) printf '  listens on: %s (this host only)\n' "$listen_addr" ;;
        *) printf '  listens on: %s (reachable from other hosts: allow only your proxy)\n' "$listen_addr" ;;
    esac
    printf '  address:    %s (GOTCHA_BASE_URL)\n' "$base_url"
    printf '  config:     /etc/gotcha/gotcha.env\n'
    printf '  logs:       journalctl -u gotcha -f\n'
    printf '\nNext:\n'
    if [ -n "$legacy_site" ]; then
        printf '  1. Your nginx site from a previous version is kept as is and is yours to maintain: %s\n' "$legacy_site"
        [ -z "$enable_hint" ] || printf '     It is not enabled now; to enable it: %s\n' "$enable_hint"
    else
        printf '  1. Put a reverse proxy (nginx, angie, Apache, Caddy...) in front of %s\n' "$(readyz_probe_addr "$listen_addr")"
        printf '     so that %s reaches it. Requirements and examples:\n' "$base_url"
        printf '     %s\n' "$GOTCHA_DOCS_BARE_METAL_URL"
    fi
    [ -z "$fresh" ] || printf '  2. Open %s and create the first administrator.\n' "$base_url"
}

render_pg_conf() {
    cat <<'EOF'
random_page_cost = 1.1
effective_io_concurrency = 200
EOF
}

# Единственное место, знающее форму адреса релизного ассета; тег совпадает
# со scripts/release.sh (TAG="v$VERSION").
dist_url() {
    local base="$1" version="$2" arch="$3"
    printf '%s/v%s/gotcha-%s-linux-%s.tar.gz\n' "${base%/}" "$version" "$version" "$arch"
}

fail() {
    local code="$1"
    shift
    printf 'install-bare-metal: %s\n' "$*" >&2
    exit "$code"
}

INSTALL_JOURNAL=/var/log/gotcha-install.log

log_step() {
    INSTALL_LOG+=("$1")
    # stderr, не stdout: шаги (fetch_tarball и далее) отдают в stdout свой
    # результат, и лог прогресса не должен в него подмешиваться.
    printf 'install-bare-metal: %s\n' "$1" >&2
    # Переживает обрыв процесса (§4.6), append вместо truncate; 2>/dev/null раньше
    # >>: отказ самого перенаправления печатает bash, мимо перенаправленного stderr.
    printf '%s [%s] %s\n' "$(date -u +%FT%TZ)" "${INSTALL_RUN_ID:-?}" "$1" \
        2>/dev/null >>"$INSTALL_JOURNAL" || true
}

# fetch_tarball/install_postgresql/install_clickhouse возвращают значение через $( ),
# и их log_step наполняют INSTALL_LOG подоболочки — до трапа доживает только журнал.
completed_steps() {
    local -a from_file=()
    if [ -r "$INSTALL_JOURNAL" ] && [ -n "${INSTALL_RUN_ID:-}" ]; then
        mapfile -t from_file < <(sed -n "s/^[^ ]* \[$INSTALL_RUN_ID\] //p" "$INSTALL_JOURNAL" 2>/dev/null)
    fi
    if [ "${#from_file[@]}" -gt 0 ]; then
        printf '%s\n' "${from_file[@]}"
    elif [ "${#INSTALL_LOG[@]}" -gt 0 ]; then
        printf '%s\n' "${INSTALL_LOG[@]}"
    fi
}

# ERR не годится: без errtrace он не наследуется функциями, а fail() выходит
# обычным exit — EXIT срабатывает на любом коде, это единственное надёжное место.
on_exit() {
    local code=$?
    cleanup_tmp_dirs
    [ "$code" -eq 0 ] && return 0

    local -a steps=()
    mapfile -t steps < <(completed_steps)

    local report
    report=$(
        printf 'install-bare-metal: FAILED (exit %d)\n' "$code"
        if [ "${#steps[@]}" -gt 0 ]; then
            printf 'install-bare-metal: completed steps so far:\n'
            printf '  - %s\n' "${steps[@]}"
        else
            printf 'install-bare-metal: no steps completed yet\n'
        fi
        printf 'install-bare-metal: re-run to retry (idempotent) or pass --uninstall to remove what was done\n'
    )
    printf '%s\n' "$report" >&2
    printf '%s\n' "$report" 2>/dev/null >>"$INSTALL_JOURNAL" || true
}

cleanup_tmp_dirs() {
    local d
    for d in "${TMP_DIRS[@]}"; do
        [ -n "$d" ] && rm -rf "$d"
    done
}

required_commands() {
    local family="$1" skip_databases="$2" cmd
    for cmd in curl tar gpg openssl sha256sum ss; do
        printf '%s\n' "$cmd"
    done
    if [ "$family" = rhel ]; then
        printf 'rpm\n'
        printf 'dnf\n'
    fi
    # runuser нужен только своим СУБД: psql от пользователя postgres и pg_dump перед обновлением.
    [ -n "$skip_databases" ] || printf 'runuser\n'
}

port_owner_units() {
    case "$1" in
        8080) printf 'gotcha\n' ;;
        5432) printf '%s\n' "$PG_UNIT" ;;
        *) printf 'clickhouse-server\n' ;;
    esac
}

have_command() {
    command -v "$1" >/dev/null 2>&1
}

packages_for_commands() {
    local cmd pkg seen=" "
    for cmd in "$@"; do
        pkg="${PKG_HINTS[$cmd]}"
        case "$seen" in *" $pkg "*) continue ;; esac
        seen+="$pkg "
        printf '%s\n' "$pkg"
    done
}

preflight_platform() {
    [ -d /run/systemd/system ] || fail "$EXIT_PREFLIGHT" "systemd is required (PID 1 is not systemd)"
    HOST_ARCH=$(detect_arch "$(uname -m)") \
        || fail "$EXIT_PREFLIGHT" "unsupported architecture: $(uname -m) (amd64/arm64 only)"
}

preflight_resources() {
    # 1900, не 2048: облачные "2 ГБ" урезают MemTotal под firmware/hypervisor.
    # Не local — install_clickhouse переиспользует значение для 10-small.xml.
    HOST_RAM_MB=$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo)
    [ "$HOST_RAM_MB" -ge 1900 ] || fail "$EXIT_PREFLIGHT" "at least 2 GB RAM required (found ${HOST_RAM_MB} MB)"
    local disk_gb
    disk_gb=$(($(df --output=avail -k / | tail -n1) / 1024 / 1024))
    [ "$disk_gb" -ge 20 ] || fail "$EXIT_PREFLIGHT" "at least 20 GB free disk required (found ${disk_gb} GB)"
}

# rpm/dnf не доставляются: без них доставлять нечем. Провал установки пакета не отдельный
# отказ — повторная проверка ниже даёт тот же exit 3 с именем пакета.
preflight_prerequisites() {
    local cmd joined
    local -a missing=() packages=()
    while IFS= read -r cmd; do
        have_command "$cmd" || missing+=("$cmd")
    done < <(required_commands "$HOST_FAMILY" "$ARG_SKIP_DATABASES")
    [ "${#missing[@]}" -gt 0 ] || return 0
    for cmd in "${missing[@]}"; do
        case "$cmd" in
            rpm | dnf) fail "$EXIT_PREFLIGHT" "$cmd is required ($PKG_HINT_LABEL: ${PKG_HINTS[$cmd]})" ;;
        esac
    done
    mapfile -t packages < <(packages_for_commands "${missing[@]}")
    printf -v joined '%s ' "${packages[@]}"
    joined="${joined% }"
    if [ -n "$ARG_DRY_RUN" ]; then
        printf '[dry-run] would install: %s\n' "$joined"
        return 0
    fi
    printf 'install-bare-metal: installing missing prerequisites: %s\n' "$joined" >&2
    if [ "$HOST_FAMILY" != rhel ]; then
        pkg_refresh || log_step "WARNING: apt-get update failed before installing: $joined"
    fi
    pkg_install "${packages[@]}" || log_step "WARNING: could not install: $joined"
    for cmd in "${missing[@]}"; do
        have_command "$cmd" || fail "$EXIT_PREFLIGHT" "$cmd is required ($PKG_HINT_LABEL: ${PKG_HINTS[$cmd]})"
    done
    # Только после повторной проверки: провал доставки не должен попасть в completed steps.
    log_step "installed missing prerequisites: $joined"
}

preflight_ports() {
    if ! have_command ss; then
        [ -z "$ARG_DRY_RUN" ] || printf '[dry-run] port checks skipped: ss is missing\n'
        return 0
    fi
    local -a ports=(8080)
    [ -n "$ARG_SKIP_DATABASES" ] || ports+=(5432 8123 9000)
    local port owner owned
    for port in "${ports[@]}"; do
        ss -ltn 2>/dev/null | awk '{print $4}' | grep -q ":${port}\$" || continue
        # Занятый порт — отказ, только если это не наш же юнит с прошлого запуска;
        # иначе идемпотентный повторный запуск не проходил бы preflight.
        owned=""
        while IFS= read -r owner; do
            systemctl is-active --quiet "$owner" && { owned=1; break; }
        done < <(port_owner_units "$port")
        [ -n "$owned" ] && continue
        fail "$EXIT_PREFLIGHT" "port $port is already in use"
    done
}

preflight() {
    preflight_platform
    preflight_resources
    preflight_prerequisites
    preflight_ports
}

# --dry-run не доставляет пакеты (preflight_prerequisites пропускает установку) —
# перед fetch_tarball нужно знать, есть ли чем его выполнить, а не звонить и падать.
tarball_prereqs_missing() {
    local from_tarball="$1" cmd
    for cmd in tar sha256sum; do
        have_command "$cmd" || printf '%s\n' "$cmd"
    done
    [ -n "$from_tarball" ] || have_command curl || printf 'curl\n'
}

# Возвращает путь к распакованному каталогу через stdout; временные
# каталоги регистрируются в TMP_DIRS, чтобы EXIT-трап их удалил.
fetch_tarball() {
    local version="$1" arch="$2" download_base="$3" from_tarball="$4"
    local tarball sums_file

    if [ -n "$from_tarball" ]; then
        tarball="$from_tarball"
        [ -f "$tarball" ] || fail "$EXIT_DOWNLOAD" "tarball not found: $tarball"
        sums_file="$(dirname "$tarball")/SHA256SUMS.txt"
        [ -f "$sums_file" ] || fail "$EXIT_DOWNLOAD" "checksum file not found next to tarball: $sums_file"
    else
        local workdir url sums_url
        workdir=$(mktemp -d)
        TMP_DIRS+=("$workdir")
        url=$(dist_url "$download_base" "$version" "$arch")
        sums_url="${download_base%/}/v${version}/SHA256SUMS.txt"
        tarball="$workdir/gotcha-${version}-linux-${arch}.tar.gz"
        sums_file="$workdir/SHA256SUMS.txt"
        curl -fsSL --retry 3 -o "$tarball" -- "$url" || fail "$EXIT_DOWNLOAD" "download failed: $url"
        curl -fsSL --retry 3 -o "$sums_file" -- "$sums_url" || fail "$EXIT_DOWNLOAD" "download failed: $sums_url"
    fi

    (cd "$(dirname "$tarball")" && grep " $(basename "$tarball")\$" "$(basename "$sums_file")" | sha256sum -c -) >/dev/null \
        || fail "$EXIT_DOWNLOAD" "SHA-256 mismatch for $(basename "$tarball") — download is corrupted, retry"

    local extract_dir
    extract_dir=$(mktemp -d)
    TMP_DIRS+=("$extract_dir")
    tar -xzf "$tarball" -C "$extract_dir" || fail "$EXIT_DOWNLOAD" "failed to extract $tarball"

    local root
    root=$(find "$extract_dir" -mindepth 1 -maxdepth 1 -type d | head -n1)
    [ -n "$root" ] || fail "$EXIT_DOWNLOAD" "unexpected tarball layout: no top-level directory"

    local found_version
    found_version=$(cat "$root/VERSION" 2>/dev/null || true)
    [ "$found_version" = "$version" ] \
        || fail "$EXIT_DOWNLOAD" "tarball VERSION ($found_version) does not match requested $version"

    log_step "tarball fetched and verified: version $version, arch $arch"
    printf '%s\n' "$root"
}

# gpg --with-colons: формат стабилен для парсинга, не --fingerprint (для людей).
# Подключи приняты по самоподписи умышленно — пин ронял бы установку при ротации.
verify_key_fingerprint() {
    local keyfile="$1" expected="$2" got pubs
    local colons
    colons=$(gpg --with-colons --import-options show-only --import "$keyfile" 2>/dev/null)
    pubs=$(printf '%s\n' "$colons" | grep -c '^pub:')
    [ "$pubs" = 1 ] \
        || fail "$EXIT_DATABASE" "signing key file must contain exactly one primary key, found $pubs"
    got=$(printf '%s\n' "$colons" | awk -F: '/^pub:/{p=1; next} p && /^fpr:/{print $10; exit}')
    [ "$got" = "$expected" ] \
        || fail "$EXIT_DATABASE" "signing key fingerprint mismatch: got '$got', expected '$expected'"
}

# PGDG публикует Release-манифест только для codename, которые поддерживает —
# его отсутствие и есть сигнал "ещё не добавлен", без парсинга HTML/API.
pgdg_has_codename() {
    curl -fsSL -o /dev/null "https://apt.postgresql.org/pub/repos/apt/dists/${1}-pgdg/Release"
}

# Требует apt-get update по репозиториям дистрибутива (без PGDG) до вызова —
# гарантирует install_postgresql.
native_pg_major() {
    apt-cache policy postgresql 2>/dev/null | awk '/Candidate:/{print $2}' | grep -oE '^[0-9]+'
}

# $basearch/$releasever в файле остаются литералами для dnf/yum — экранируем
# в heredoc.
render_pgdg_repo() {
    cat <<EOF
[pgdg-common]
name=PostgreSQL common RPMs for RHEL \$releasever - \$basearch
baseurl=https://download.postgresql.org/pub/repos/yum/common/redhat/rhel-$1-\$basearch
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=file://$PGDG_RPM_KEY_PATH

[pgdg$PG_MAJOR]
name=PostgreSQL $PG_MAJOR for RHEL \$releasever - \$basearch
baseurl=https://download.postgresql.org/pub/repos/yum/$PG_MAJOR/redhat/rhel-$1-\$basearch
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=file://$PGDG_RPM_KEY_PATH
EOF
}

# Ключ PGDG для rpm — по архитектуре хоста, как и $basearch репозитория.
# Одно значение за вызов, без read: под IFS=$'\n\t' из main() read склеил бы пару полей.
pgdg_rpm_key_for_arch() {
    local arch="$1" field="$2" url fpr
    case "$arch" in
        arm64) url="$PGDG_RPM_KEY_URL_ARM64"; fpr="$PGDG_RPM_KEY_FINGERPRINT_ARM64" ;;
        *) url="$PGDG_RPM_KEY_URL"; fpr="$PGDG_RPM_KEY_FINGERPRINT" ;;
    esac
    case "$field" in
        url) printf '%s\n' "$url" ;;
        fingerprint) printf '%s\n' "$fpr" ;;
    esac
}

# deb-ветка не подставляет нативный мажор молча: PGDG публикует EL9/EL10 всегда,
# и штатный AppStream мажора 17 не содержит.
repo_add_pgdg() {
    local codename="$1"
    if [ "$HOST_FAMILY" = rhel ]; then
        local tmp key_url key_fpr
        tmp=$(mktemp -d)
        TMP_DIRS+=("$tmp")
        key_url=$(pgdg_rpm_key_for_arch "$HOST_ARCH" url)
        key_fpr=$(pgdg_rpm_key_for_arch "$HOST_ARCH" fingerprint)
        curl -fsSL -o "$tmp/pgdg.asc" "$key_url" \
            || fail "$EXIT_DATABASE" "failed to download the PGDG signing key from $key_url"
        verify_key_fingerprint "$tmp/pgdg.asc" "$key_fpr"
        mkdir -p "$(dirname "$PGDG_RPM_KEY_PATH")"
        cp "$tmp/pgdg.asc" "$PGDG_RPM_KEY_PATH"
        rpm --import "$PGDG_RPM_KEY_PATH" >/dev/null
        render_pgdg_repo "$EL_MAJOR" >"$REPO_DIR/gotcha-pgdg.repo"
        return 0
    fi

    if pgdg_has_codename "$codename"; then
        local tmp keyring
        tmp=$(mktemp -d)
        TMP_DIRS+=("$tmp")
        curl -fsSL -o "$tmp/pgdg.asc" https://www.postgresql.org/media/keys/ACCC4CF8.asc \
            || fail "$EXIT_DATABASE" "failed to download the PGDG signing key"
        verify_key_fingerprint "$tmp/pgdg.asc" "$PGDG_KEY_FINGERPRINT"
        keyring=/usr/share/keyrings/gotcha-pgdg.gpg
        gpg --dearmor <"$tmp/pgdg.asc" >"$keyring"
        printf 'deb [signed-by=%s] https://apt.postgresql.org/pub/repos/apt %s-pgdg main\n' \
            "$keyring" "$codename" >"$REPO_DIR/gotcha-pgdg.list"
        pkg_refresh || fail "$EXIT_DATABASE" "apt-get update failed after adding the PGDG repository"
        return 0
    fi

    pkg_refresh || fail "$EXIT_DATABASE" "apt-get update failed"
    local native
    native=$(native_pg_major)
    [ "$native" = "$PG_MAJOR" ] \
        || fail "$EXIT_PREFLIGHT" "PGDG has no packages for $codename yet and the distribution ships PostgreSQL $native, not $PG_MAJOR; wait for PGDG to add this codename or install PostgreSQL $PG_MAJOR by hand"
    printf 'native\n'
}

# stdin: строка pg_hba.conf для 127.0.0.1/32. rc 0 — метод требует пароль
# (scram-sha-256/md5), rc 1 — нет (ident/peer/reject/закомментировано/другой хост).
pg_hba_host_method_is_password() {
    awk '
        $1 == "host" && $4 == "127.0.0.1/32" && ($5 == "scram-sha-256" || $5 == "md5") { found=1 }
        END { exit found ? 0 : 1 }
    '
}

# postgresql.conf.sample несёт include_dir закомментированной — initdb копирует
# её как есть, и дропин сам по себе не подхватится.
ensure_include_dir() {
    grep -qF "$PG_INCLUDE_MARKER" "$1" && return 0
    printf '%s\ninclude_dir = %s\n' "$PG_INCLUDE_MARKER" "'conf.d'" >>"$1"
}

# Снимает marker+payload по содержимому маркера. readlink -f обязателен: cp -a на
# симлинк $file дал бы tmp-симлинк на тот же таргет, усекаемый раньше, чем прочитает awk.
remove_marker_block() {
    local marker="$1" file="$2" real tmp
    [ -f "$file" ] || return 0
    grep -qF "$marker" "$file" || return 0
    real=$(readlink -f "$file") || return 1
    tmp="$real.gotcha-tmp"
    cp -a "$real" "$tmp" || return 1
    if awk -v m="$marker" '
        $0 == m { skip = 1; next }
        skip > 0 { skip--; next }
        { print }
    ' "$real" >"$tmp"; then
        mv "$tmp" "$real"
    else
        rm -f "$tmp"
        return 1
    fi
}

# Возвращает через stdout DSN на 127.0.0.1; ставит пакет, роль и базу gotcha.
# Код 3 — только для решения по мажору ниже, прочие отказы шага — код 5.
install_postgresql() {
    local env_file="$1" package="$PG_PACKAGE"

    local repo_result
    repo_result=$(repo_add_pgdg "$HOST_CODENAME")
    [ "$repo_result" != "native" ] || package="postgresql"

    if [ "$HOST_FAMILY" = rhel ] && [ "$EL_MAJOR" = 9 ]; then
        dnf -qy module disable postgresql >/dev/null
    fi

    if [ "$HOST_FAMILY" = rhel ] && [ "$repo_result" != "native" ]; then
        # citext (migrations/pg) живёт в отдельном PGDG-пакете на EL — на Debian он
        # уже внутри postgresql-$PG_MAJOR, здесь без него миграции падают на CREATE EXTENSION.
        pkg_install "$package" "postgresql${PG_MAJOR}-contrib" \
            || fail "$EXIT_DATABASE" "failed to install $package"
    else
        pkg_install "$package" || fail "$EXIT_DATABASE" "failed to install $package"
    fi

    local conf_dir
    conf_dir=$(pg_conf_dir_resolve)
    [ -n "$conf_dir" ] || fail "$EXIT_DATABASE" "PostgreSQL installed but $(pg_conf_dir_label) is missing"

    if [ "$HOST_FAMILY" = rhel ] && [ -z "$(ls -A "$conf_dir" 2>/dev/null)" ]; then
        "$PG_BIN_DIR/postgresql-$PG_MAJOR-setup" initdb >/dev/null \
            || fail "$EXIT_DATABASE" "postgresql-$PG_MAJOR-setup initdb failed"
    fi

    mkdir -p "$conf_dir/conf.d" || fail "$EXIT_DATABASE" "failed to create $conf_dir/conf.d"
    render_pg_conf >"$conf_dir/conf.d/10-gotcha.conf"

    if [ "$HOST_FAMILY" = rhel ]; then
        ensure_include_dir "$conf_dir/postgresql.conf"
        if ! pg_hba_host_method_is_password <"$conf_dir/pg_hba.conf"; then
            printf '%s\nhost all all 127.0.0.1/32 scram-sha-256\n' \
                "$PG_INCLUDE_MARKER" >>"$conf_dir/pg_hba.conf"
        fi
        systemctl enable --now "$PG_UNIT" || fail "$EXIT_DATABASE" "failed to start $PG_UNIT"
    else
        # policy-rc.d в контейнерных образах блокирует автозапуск postinst-скрипта
        # пакета — сервер поднимает явный systemctl, а не установка сама по себе.
        systemctl restart "$PG_UNIT" || fail "$EXIT_DATABASE" "failed to start $PG_UNIT"
    fi

    # Пароль перевыпускается, только если роли ещё нет, либо она есть, а
    # gotcha.env — нет: тогда старый пароль всё равно потерян и никого не сломает.
    local password="" role_exists=""
    role_exists=$(runuser -u postgres -- "$PG_BIN_DIR/psql" -tAc "SELECT 1 FROM pg_roles WHERE rolname = 'gotcha'" 2>/dev/null)
    if [ "$role_exists" != "1" ]; then
        password=$(openssl rand -hex 24)
    elif [ ! -f "$env_file" ]; then
        password=$(openssl rand -hex 24)
        log_step "WARNING: gotcha role exists but $env_file is missing — regenerating its PostgreSQL password"
    fi
    if [ -n "$password" ] && ! runuser -u postgres -- "$PG_BIN_DIR/psql" -v ON_ERROR_STOP=1 -q >/dev/null <<SQL
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'gotcha') THEN
    CREATE ROLE gotcha LOGIN PASSWORD '$password';
  ELSE
    ALTER ROLE gotcha PASSWORD '$password';
  END IF;
END \$\$;
SQL
    then
        fail "$EXIT_DATABASE" "failed to create/reset the gotcha role in PostgreSQL"
    fi
    if ! runuser -u postgres -- "$PG_BIN_DIR/psql" -v ON_ERROR_STOP=1 -q >/dev/null <<'SQL'
SELECT 'CREATE DATABASE gotcha OWNER gotcha'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'gotcha')\gexec
SQL
    then
        fail "$EXIT_DATABASE" "failed to create the gotcha database in PostgreSQL"
    fi

    log_step "PostgreSQL $PG_MAJOR installed and configured"
    printf 'postgres://gotcha:%s@127.0.0.1:5432/gotcha?sslmode=disable\n' "$password"
}

# gpgcheck=0: пакеты ClickHouse не подписаны индивидуально, как и в их собственном
# packages.clickhouse.com/rpm/clickhouse.repo — доверие даёт repo_gpgcheck=1.
render_clickhouse_repo() {
    cat <<EOF
[gotcha-clickhouse]
name=ClickHouse
baseurl=https://packages.clickhouse.com/rpm/stable/
enabled=1
gpgcheck=0
repo_gpgcheck=1
gpgkey=file://$CLICKHOUSE_RPM_KEY_PATH
EOF
}

# rpm/stable и deb-ветка ниже качают ключ с разных путей одного вендора, но файл
# побайтово равен уже используемому rpm/lts — отпечаток один, CLICKHOUSE_KEY_FINGERPRINT.
repo_add_clickhouse() {
    local tmp
    tmp=$(mktemp -d)
    TMP_DIRS+=("$tmp")

    if [ "$HOST_FAMILY" = rhel ]; then
        curl -fsSL -o "$tmp/clickhouse.asc" https://packages.clickhouse.com/rpm/stable/repodata/repomd.xml.key \
            || fail "$EXIT_DATABASE" "failed to download the ClickHouse signing key"
        verify_key_fingerprint "$tmp/clickhouse.asc" "$CLICKHOUSE_KEY_FINGERPRINT"
        mkdir -p "$(dirname "$CLICKHOUSE_RPM_KEY_PATH")"
        cp "$tmp/clickhouse.asc" "$CLICKHOUSE_RPM_KEY_PATH"
        rpm --import "$CLICKHOUSE_RPM_KEY_PATH" >/dev/null
        render_clickhouse_repo >"$REPO_DIR/gotcha-clickhouse.repo"
        return 0
    fi

    curl -fsSL -o "$tmp/clickhouse.asc" https://packages.clickhouse.com/rpm/lts/repodata/repomd.xml.key \
        || fail "$EXIT_DATABASE" "failed to download the ClickHouse signing key"
    verify_key_fingerprint "$tmp/clickhouse.asc" "$CLICKHOUSE_KEY_FINGERPRINT"
    local keyring=/usr/share/keyrings/gotcha-clickhouse.gpg
    gpg --dearmor <"$tmp/clickhouse.asc" >"$keyring"
    printf 'deb [signed-by=%s] https://packages.clickhouse.com/deb stable main\n' "$keyring" \
        >"$REPO_DIR/gotcha-clickhouse.list"
    pkg_refresh || fail "$EXIT_DATABASE" "apt-get update failed after adding the ClickHouse repository"
}

# dnf переносит длинные строки: версия может оказаться на следующей строке отдельным
# полем — поле определяется по позиции, не фиксированным $2.
clickhouse_version_from_dnf_list() {
    awk -v v="$CH_VERSION." '
        function is_arch(s) { return s ~ /\.(noarch|x86_64|aarch64)$/ }
        function has_prefix(s) { return substr(s, 1, length(v)) == v }
        NF >= 2 && is_arch($1) { if (has_prefix($2)) print $2; next }
        NF >= 1 && !is_arch($1) { if (has_prefix($1)) print $1 }
    ' | sort -V | tail -n1
}

# ClickHouse не публикует пакет без патч-версии в номере — apt-cache madison/dnf list
# находят конкретный патч для мажора.минора из CH_VERSION.
clickhouse_package_version() {
    if [ "$HOST_FAMILY" = rhel ]; then
        # -y: без него dnf на первом обращении к репозиторию молча отказывает в
        # неинтерактивном подтверждении ключа, список выходит пустым.
        dnf -qy --showduplicates list clickhouse-server 2>/dev/null | clickhouse_version_from_dnf_list
        return 0
    fi
    apt-cache madison clickhouse-server 2>/dev/null \
        | awk -F'|' -v v="$CH_VERSION." '{gsub(/^[ \t]+|[ \t]+$/, "", $2)} $2 ~ ("^" v) {print $2; exit}'
}

# Возвращает через stdout DSN на 127.0.0.1; ставит пакет, конфиги из тарбола,
# пользователя gotcha и лимит файловых дескрипторов. Отказ любого шага — код 5.
install_clickhouse() {
    local ram_mb="$1" tarball_root="$2" env_file="$3"
    repo_add_clickhouse

    local version
    version=$(clickhouse_package_version)
    [ -n "$version" ] || fail "$EXIT_DATABASE" "no clickhouse-server package matches version $CH_VERSION"

    if [ "$HOST_FAMILY" = rhel ]; then
        pkg_install \
            "clickhouse-server-$version" "clickhouse-client-$version" "clickhouse-common-static-$version" \
            || fail "$EXIT_DATABASE" "failed to install clickhouse-server $version"
    else
        # clickhouse-common-static нужен явной версией: без него apt подтягивает
        # последний мажор из репозитория и ловит конфликт зависимостей.
        pkg_install \
            "clickhouse-server=$version" "clickhouse-client=$version" "clickhouse-common-static=$version" \
            || fail "$EXIT_DATABASE" "failed to install clickhouse-server $version"
    fi

    mkdir -p /etc/clickhouse-server/config.d
    cp "$tarball_root/clickhouse/00-common.xml" /etc/clickhouse-server/config.d/00-common.xml \
        || fail "$EXIT_DATABASE" "failed to install 00-common.xml"
    if [ "$ram_mb" -lt 4096 ]; then
        cp "$tarball_root/clickhouse/10-small.xml" /etc/clickhouse-server/config.d/10-small.xml \
            || fail "$EXIT_DATABASE" "failed to install 10-small.xml"
    fi

    # Пароль и users.d перевыпускаются, только если файла ещё нет, либо он есть,
    # а gotcha.env — нет: старый пароль в этом случае всё равно потерян.
    local password="" hash
    if [ ! -f /etc/clickhouse-server/users.d/10-gotcha.xml ] || [ ! -f "$env_file" ]; then
        if [ -f /etc/clickhouse-server/users.d/10-gotcha.xml ]; then
            log_step "WARNING: clickhouse user gotcha exists but $env_file is missing — regenerating its password"
        fi
        password=$(openssl rand -hex 24)
        hash=$(printf '%s' "$password" | sha256sum | awk '{print $1}')
        mkdir -p /etc/clickhouse-server/users.d
        cat >/etc/clickhouse-server/users.d/10-gotcha.xml <<EOF || fail "$EXIT_DATABASE" "failed to write 10-gotcha.xml"
<clickhouse>
    <users>
        <gotcha>
            <password_sha256_hex>$hash</password_sha256_hex>
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
    fi

    mkdir -p /etc/systemd/system/clickhouse-server.service.d
    cat >/etc/systemd/system/clickhouse-server.service.d/override.conf <<'EOF' || fail "$EXIT_DATABASE" "failed to write the LimitNOFILE override"
[Service]
LimitNOFILE=262144
EOF
    systemctl daemon-reload || fail "$EXIT_DATABASE" "systemctl daemon-reload failed"
    systemctl enable clickhouse-server >/dev/null 2>&1 || true
    systemctl restart clickhouse-server || fail "$EXIT_DATABASE" "failed to start clickhouse-server"

    local tries=0
    until curl -fsS -o /dev/null http://127.0.0.1:8123/ping 2>/dev/null; do
        tries=$((tries + 1))
        [ "$tries" -lt 30 ] || fail "$EXIT_DATABASE" "clickhouse-server did not become ready on 127.0.0.1:8123"
        sleep 1
    done

    # От пользователя gotcha база не создаётся: он не подключится, пока не существует
    # его default_database, то есть та самая база. Отсюда default и подсказка про пароль.
    clickhouse-client --query "CREATE DATABASE IF NOT EXISTS gotcha" \
        || fail "$EXIT_DATABASE" "failed to create the gotcha database in ClickHouse (if the ClickHouse default user has a password, create the database manually and re-run)"

    log_step "ClickHouse $CH_VERSION installed and configured"
    printf 'clickhouse://gotcha:%s@127.0.0.1:9000/gotcha\n' "$password"
}

# Фактический bind, а не дефолт пакета: уже настроенный на 0.0.0.0 PostgreSQL
# инсталлятор переиспользует, и обещание доки «только 127.0.0.1» станет ложным.
verify_loopback_only() {
    local port addrs addr
    for port in "$@"; do
        addrs=$(ss -ltn 2>/dev/null | awk -v p=":${port}\$" '$4 ~ p {print $4}')
        [ -n "$addrs" ] \
            || fail "$EXIT_DATABASE" "nothing listens on port $port after installing the databases"
        while IFS= read -r addr; do
            case "$addr" in
                127.0.0.1:"$port" | \[::1\]:"$port") ;;
                *) fail "$EXIT_DATABASE" "port $port listens on $addr, not loopback only — gotcha's databases must not be reachable from outside the host; fix listen_addresses/listen_host and re-run" ;;
            esac
        done <<<"$addrs"
    done
    local checked=""
    for port in "$@"; do
        checked="${checked:+$checked, }$port"
    done
    log_step "databases listen on loopback only: $checked"
}

create_app_user() {
    id -u gotcha >/dev/null 2>&1 && return 0
    useradd --system --no-create-home --shell /usr/sbin/nologin gotcha \
        || fail "$EXIT_OTHER" "failed to create the gotcha system user"
    log_step "system user gotcha created"
}

install_app_files() {
    local tarball_root="$1"
    install -m 0755 -o root -g root "$tarball_root/gotcha" /usr/local/bin/gotcha \
        || fail "$EXIT_OTHER" "failed to install /usr/local/bin/gotcha"
    mkdir -p /opt/gotcha/agent-dist || fail "$EXIT_OTHER" "failed to create /opt/gotcha/agent-dist"
    cp -a "$tarball_root/agent-dist/." /opt/gotcha/agent-dist/ \
        || fail "$EXIT_OTHER" "failed to install the agent distribution"
    log_step "gotcha binary and agent distribution installed"
}

# upgrade.md требует бэкап перед форвард-онли миграциями, обход — только --no-backup.
# Без своей СУБД (--skip-databases) дамп снять нечем — бэкап там на операторе.
backup_before_upgrade() {
    local from_version="$1" no_backup="$2" skip_databases="$3"
    if [ -n "$no_backup" ]; then
        log_step "pre-upgrade backup skipped (--no-backup)"
        return 0
    fi
    if [ -n "$skip_databases" ]; then
        log_step "pre-upgrade backup skipped (--skip-databases, database is not ours to dump)"
        return 0
    fi
    mkdir -p /var/lib/gotcha/backup || fail "$EXIT_DATABASE" "failed to create /var/lib/gotcha/backup"
    chmod 700 /var/lib/gotcha/backup || fail "$EXIT_DATABASE" "failed to create /var/lib/gotcha/backup"
    local dump
    dump="/var/lib/gotcha/backup/postgres-${from_version}-$(date -u +%Y%m%dT%H%M%SZ).sql.gz" \
        || fail "$EXIT_DATABASE" "failed to build backup file name"
    runuser -u postgres -- "$PG_BIN_DIR/pg_dump" -d gotcha | gzip >"$dump" || fail "$EXIT_DATABASE" "pre-upgrade pg_dump failed"
    # Дамп несёт те же секреты (схема, данные), что и gotcha.env — не мирочитаем.
    chmod 600 "$dump" || fail "$EXIT_DATABASE" "failed to secure $dump"
    log_step "pre-upgrade backup: $dump"
}

# Снимок ДО перезаписи install_app_files — единственный путь отката на предыдущий
# бинарь, который описывает upgrade.md ("Rolling back").
backup_previous_binary() {
    local from_version="$1"
    mkdir -p /opt/gotcha/backup || fail "$EXIT_OTHER" "failed to create /opt/gotcha/backup"
    cp -a /usr/local/bin/gotcha "/opt/gotcha/backup/gotcha-$from_version" \
        || fail "$EXIT_OTHER" "failed to back up the previous gotcha binary"
    log_step "previous binary backed up: /opt/gotcha/backup/gotcha-$from_version"
}

# Пишется один раз: повторный запуск не перевыпускает пароли и GOTCHA_SECRET_KEY.
# Временный файл + mv: точные права ставит скрипт, не umask процесса.
write_env_file() {
    local pg_dsn="$1" ch_dsn="$2" base_url="$3" gomemlimit="$4" env_file="$5"
    if [ -f "$env_file" ]; then
        log_step "config file already present, left untouched: $env_file"
        return 0
    fi
    mkdir -p /etc/gotcha || fail "$EXIT_OTHER" "failed to create /etc/gotcha"
    local tmp secret_key
    tmp=$(mktemp /etc/gotcha/.gotcha.env.XXXXXX) || fail "$EXIT_OTHER" "failed to create a temp file in /etc/gotcha"
    secret_key=$(openssl rand -base64 48)
    render_env_file "$pg_dsn" "$ch_dsn" "$secret_key" "$base_url" \
        /opt/gotcha/agent-dist "$gomemlimit" 127.0.0.1:8080 >"$tmp" \
        || { rm -f "$tmp"; fail "$EXIT_OTHER" "failed to render $env_file"; }
    chown "$ENV_FILE_OWNER" "$tmp" || { rm -f "$tmp"; fail "$EXIT_OTHER" "failed to set ownership/permissions on $env_file"; }
    chmod 0640 "$tmp" || { rm -f "$tmp"; fail "$EXIT_OTHER" "failed to set ownership/permissions on $env_file"; }
    mv "$tmp" "$env_file" || fail "$EXIT_OTHER" "failed to install $env_file"
    log_step "config file created: $env_file"
}

# Значение через ENVIRON, не через sed/awk -v: & # \ в адресе ломали бы подстановку.
env_set() {
    local key="$1" value="$2" file="$3" tmp
    tmp=$(mktemp "$(dirname "$file")/.gotcha.env.XXXXXX") || return 1
    if ! ENV_SET_KEY="$key" ENV_SET_VALUE="$value" awk '
        BEGIN { k = ENVIRON["ENV_SET_KEY"]; v = ENVIRON["ENV_SET_VALUE"]; p = k "=" }
        substr($0, 1, length(p)) == p { if (!done) { print k "=" v; done = 1 } next }
        { print }
        END { if (!done) print k "=" v }
    ' "$file" >"$tmp" \
        || ! chown "$ENV_FILE_OWNER" "$tmp" \
        || ! chmod 0640 "$tmp" \
        || ! mv "$tmp" "$file"; then
        rm -f "$tmp"
        return 1
    fi
}

reconcile_env_file() {
    local env_file="$1" base_url_flag="$2" current
    if [ -n "$base_url_flag" ]; then
        current=$(env_get GOTCHA_BASE_URL "$env_file") || current=""
        current=$(normalize_base_url "$current")
        if [ "$current" != "$base_url_flag" ]; then
            env_set GOTCHA_BASE_URL "$base_url_flag" "$env_file" \
                || fail "$EXIT_OTHER" "failed to update GOTCHA_BASE_URL in $env_file"
            log_step "GOTCHA_BASE_URL changed: ${current:-<unset>} -> $base_url_flag"
            log_step "update your reverse proxy (server name, TLS certificate) for $base_url_flag"
            ENV_CHANGED=1
        fi
    fi
    if ! env_get GOTCHA_TRUSTED_PROXIES "$env_file" >/dev/null; then
        env_set GOTCHA_TRUSTED_PROXIES "$TRUSTED_PROXIES_LOOPBACK" "$env_file" \
            || fail "$EXIT_OTHER" "failed to add GOTCHA_TRUSTED_PROXIES to $env_file"
        log_step "GOTCHA_TRUSTED_PROXIES=$TRUSTED_PROXIES_LOOPBACK added to $env_file (login rate limiting behind a reverse proxy on this host)"
        ENV_CHANGED=1
    fi
}

install_unit() {
    local memory_max="$1"
    render_unit "$memory_max" >/etc/systemd/system/gotcha.service \
        || fail "$EXIT_OTHER" "failed to write /etc/systemd/system/gotcha.service"
    systemctl daemon-reload || fail "$EXIT_OTHER" "systemctl daemon-reload failed"
    log_step "systemd unit installed"
}

# --wait пробрасывает код возврата самого gotcha, не только факт запуска юнита;
# --collect убирает транзитный юнит, чтобы повторный запуск не наткнулся на имя.
run_migrations() {
    local env_file="$1"
    systemd-run --quiet --pipe --wait --collect \
        --uid=gotcha --gid=gotcha \
        --property="EnvironmentFile=$env_file" \
        /usr/local/bin/gotcha --migrate-only \
        || fail "$EXIT_APP" "database migrations failed"
    log_step "database migrations applied"
}

start_app() {
    local restart="$1"
    systemctl enable --now gotcha || fail "$EXIT_APP" "failed to enable/start the gotcha service"
    if [ -n "$restart" ]; then
        systemctl restart gotcha || fail "$EXIT_APP" "failed to restart the gotcha service after changing its environment"
    fi

    local tries=0
    until /usr/local/bin/gotcha --healthcheck >/dev/null 2>&1; do
        tries=$((tries + 1))
        if [ "$tries" -ge 30 ]; then
            printf 'install-bare-metal: gotcha did not become ready; recent journal:\n' >&2
            journalctl -u gotcha --no-pager -n 50 >&2
            fail "$EXIT_APP" "gotcha did not pass its healthcheck"
        fi
        sleep 1
    done
    log_step "gotcha service started and healthy"
}

# Не трогает пакеты СУБД и apt-репозитории — на хосте ими может пользоваться
# что-то ещё. --purge снимает только объекты, которые этот скрипт сам и создал.
uninstall_app() {
    local purge="$1"
    systemctl disable --now gotcha >/dev/null 2>&1 || true
    rm -f /etc/systemd/system/gotcha.service
    systemctl daemon-reload || true
    rm -f /usr/local/bin/gotcha
    log_step "gotcha unit and binary removed"

    uninstall_legacy_site

    [ -n "$purge" ] || return 0

    if id -u postgres >/dev/null 2>&1; then
        runuser -u postgres -- "$PG_BIN_DIR/psql" -v ON_ERROR_STOP=1 -q >/dev/null <<'SQL' || fail "$EXIT_DATABASE" "failed to drop the gotcha role/database in PostgreSQL"
DROP DATABASE IF EXISTS gotcha;
DROP ROLE IF EXISTS gotcha;
SQL
    fi
    if command -v clickhouse-client >/dev/null 2>&1; then
        clickhouse-client --query "DROP DATABASE IF EXISTS gotcha" \
            || fail "$EXIT_DATABASE" "failed to drop the gotcha database in ClickHouse (if the ClickHouse default user has a password, drop the database manually)"
        # Пароль пользователя живёт в этом файле, не в СУБД как роль Postgres —
        # без удаления следующая установка сочла бы пользователя уже настроенным.
        rm -f /etc/clickhouse-server/users.d/10-gotcha.xml
    fi

    # Наши файлы в каталогах чужих пакетов: сами пакеты остаются, дропины уезжают.
    # Перезапуск СУБД не делается намеренно — это чужие сервисы, их время выбирает оператор.
    local pg_conf_dir
    pg_conf_dir=$(pg_conf_dir_resolve)
    if [ -n "$pg_conf_dir" ]; then
        rm -f "$pg_conf_dir/conf.d/10-gotcha.conf"
        if [ "$HOST_FAMILY" = rhel ]; then
            remove_marker_block "$PG_INCLUDE_MARKER" "$pg_conf_dir/postgresql.conf" \
                || printf 'install-bare-metal: could not remove the gotcha include_dir marker from %s, clean it up by hand\n' \
                    "$pg_conf_dir/postgresql.conf" >&2
            remove_marker_block "$PG_INCLUDE_MARKER" "$pg_conf_dir/pg_hba.conf" \
                || printf 'install-bare-metal: could not remove the gotcha include_dir marker from %s, clean it up by hand\n' \
                    "$pg_conf_dir/pg_hba.conf" >&2
        fi
    fi
    rm -f /etc/clickhouse-server/config.d/00-common.xml /etc/clickhouse-server/config.d/10-small.xml
    rm -f /etc/systemd/system/clickhouse-server.service.d/override.conf
    rmdir /etc/systemd/system/clickhouse-server.service.d 2>/dev/null || true
    systemctl daemon-reload || true

    rm -rf /var/lib/gotcha /opt/gotcha /etc/gotcha
    if id -u gotcha >/dev/null 2>&1; then
        userdel gotcha 2>/dev/null || true
    fi
    log_step "gotcha data, databases, drop-in configs and system user removed (--purge)"
    rm -f "$INSTALL_JOURNAL"
}

main() {
    set -euo pipefail
    IFS=$'\n\t'

    # Не local: exit() во вложенных функциях рвёт цепочку динамических областей
    # видимости раньше, чем сработает EXIT-трап, и он увидел бы их пустыми.
    INSTALL_LOG=()
    TMP_DIRS=()
    ENV_CHANGED=""
    # Метка запуска: по ней on_exit отбирает из общего журнала шаги текущего
    # запуска, включая записанные подоболочками.
    INSTALL_RUN_ID="$$-$(date -u +%s)"
    trap on_exit EXIT

    parse_args "$@"
    if [ -n "$ARG_HELP" ]; then
        exit "$EXIT_OK"
    fi
    detect_platform
    [ "$(id -u)" = 0 ] || fail "$EXIT_PREFLIGHT" "must run as root"
    if [ -n "$ARG_UNINSTALL" ]; then
        if [ -n "$ARG_PURGE" ] && [ -z "$ARG_YES" ]; then
            local purge_answer=""
            # read возвращает ненулевой статус на EOF (закрытый stdin) — под set -e
            # это уронило бы скрипт кодом 1 раньше, чем сработал бы case ниже.
            read -r -p "This deletes gotcha's data and databases permanently. Continue? [y/N] " purge_answer || true
            case "$purge_answer" in
                y | Y | yes | YES) ;;
                *) fail "$EXIT_USAGE" "purge cancelled (confirm with 'y' or pass --yes)" ;;
            esac
        fi
        uninstall_app "$ARG_PURGE"
        exit "$EXIT_OK"
    fi

    local env_file=/etc/gotcha/gotcha.env base_url warning
    base_url=$(resolve_base_url "$ARG_BASE_URL" "$env_file" "$ARG_YES") || exit "$?"
    if [ -n "$ARG_BASE_URL" ] || [ ! -f "$env_file" ]; then
        warning=$(plain_http_warning "$base_url") && printf 'install-bare-metal: %s\n' "$warning" >&2
    fi

    preflight

    local tarball_root="" tcmd
    local -a tarball_missing=()
    if [ -n "$ARG_DRY_RUN" ]; then
        mapfile -t tarball_missing < <(tarball_prereqs_missing "$ARG_FROM_TARBALL")
    fi
    if [ "${#tarball_missing[@]}" -gt 0 ]; then
        for tcmd in "${tarball_missing[@]}"; do
            printf '[dry-run] tarball check skipped: %s is missing\n' "$tcmd"
        done
    else
        tarball_root=$(fetch_tarball "$ARG_VERSION" "$HOST_ARCH" "$ARG_DOWNLOAD_BASE" "$ARG_FROM_TARBALL")
    fi

    # Версия уже установленного бинаря, не версия этого скрипта (GOTCHA_INSTALL_DEFAULT_VERSION):
    # решает, идёт ли речь об обновлении (§4.5) или об идемпотентном повторе/первой установке.
    local prev_version
    prev_version=$(installed_version /usr/local/bin/gotcha)
    local need_upgrade=""
    if [ -n "$prev_version" ] && ! version_ge "$prev_version" "$ARG_VERSION"; then
        need_upgrade=1
    fi

    local mem_max gomemlimit
    # IFS=' ': main() выше сузила глобальный IFS до "\n\t", и обычный read
    # больше не бьёт по пробелу, разбирая обе колонки в mem_max целиком.
    IFS=' ' read -r mem_max gomemlimit <<<"$(resolve_memlimit "$ARG_MEM_LIMIT")"

    if [ -n "$ARG_DRY_RUN" ]; then
        [ -z "$tarball_root" ] || printf '[dry-run] tarball ready at %s\n' "$tarball_root"
        printf '[dry-run] GOTCHA_BASE_URL=%s\n' "$base_url"
        printf '[dry-run] MemoryMax=%s GOMEMLIMIT=%s\n' "$mem_max" "$gomemlimit"
        printf '[dry-run] would write /etc/systemd/system/gotcha.service:\n'
        render_unit "$mem_max"
        if [ -f "$env_file" ]; then
            local current
            current=$(env_get GOTCHA_BASE_URL "$env_file") || current=""
            current=$(normalize_base_url "$current")
            [ -z "$ARG_BASE_URL" ] || [ "$current" = "$ARG_BASE_URL" ] \
                || printf '[dry-run] would change GOTCHA_BASE_URL in %s: %s -> %s\n' "$env_file" "${current:-<unset>}" "$ARG_BASE_URL"
            env_get GOTCHA_TRUSTED_PROXIES "$env_file" >/dev/null \
                || printf '[dry-run] would add GOTCHA_TRUSTED_PROXIES=%s to %s\n' "$TRUSTED_PROXIES_LOOPBACK" "$env_file"
        else
            printf '[dry-run] would write /etc/gotcha/gotcha.env:\n'
            render_env_file \
                "postgres://gotcha:<generated>@127.0.0.1:5432/gotcha?sslmode=disable" \
                "clickhouse://gotcha:<generated>@127.0.0.1:9000/gotcha" \
                "<generated>" "$base_url" "/opt/gotcha/agent-dist" "$gomemlimit" "127.0.0.1:8080"
        fi
        if [ -z "$ARG_SKIP_DATABASES" ]; then
            printf '[dry-run] would write %s/conf.d/10-gotcha.conf:\n' "$(pg_conf_dir_label)"
            render_pg_conf
        fi
        exit "$EXIT_OK"
    fi

    # Без env-файла install_postgresql/install_clickhouse ниже могут перевыпустить
    # пароль под уже работающим сервисом — останавливаем его первым, пока не поздно.
    local env_existed=""
    if [ -f "$env_file" ]; then
        env_existed=1
    else
        systemctl stop gotcha 2>/dev/null || true
    fi
    if [ -z "$ARG_SKIP_DATABASES" ]; then
        ARG_PG_DSN=$(install_postgresql "$env_file")
        ARG_CH_DSN=$(install_clickhouse "$HOST_RAM_MB" "$tarball_root" "$env_file")
        verify_loopback_only 5432 8123 9000
    fi

    create_app_user

    if [ -n "$need_upgrade" ]; then
        backup_before_upgrade "$prev_version" "$ARG_NO_BACKUP" "$ARG_SKIP_DATABASES"
        systemctl stop gotcha 2>/dev/null || true
        backup_previous_binary "$prev_version"
    fi

    install_app_files "$tarball_root"
    write_env_file "$ARG_PG_DSN" "$ARG_CH_DSN" "$base_url" "$gomemlimit" "$env_file"
    [ -z "$env_existed" ] || reconcile_env_file "$env_file" "$ARG_BASE_URL"
    install_unit "$mem_max"
    run_migrations "$env_file"
    start_app "$ENV_CHANGED"

    local readiness legacy_site enable_hint="" listen_addr version
    listen_addr=$(env_get GOTCHA_LISTEN_ADDR "$env_file") || listen_addr=127.0.0.1:8080
    readiness=$(curl -fsS --max-time 5 "http://$(readyz_probe_addr "$listen_addr")/readyz" 2>/dev/null) \
        || readiness="no answer (see logs)"
    version=$(summary_effective_version "$(installed_version /usr/local/bin/gotcha)" "$ARG_VERSION")
    legacy_site=$(find_legacy_site) || legacy_site=""
    # .disabled найден, но $NGINX_SITE уже занят чужим рабочим конфигом — это не наш
    # сайт больше, итог не должен ни называть его «сохранённым», ни советовать mv.
    if [ -n "$legacy_site" ] && [ "$legacy_site" = "$(nginx_site_disabled_path)" ] && [ -e "$NGINX_SITE" ]; then
        legacy_site=""
    fi
    if [ -n "$legacy_site" ]; then
        enable_hint=$(legacy_site_enable_hint "$legacy_site") || enable_hint=""
    fi
    render_summary "$version" "$readiness" "$base_url" "$listen_addr" \
        "$legacy_site" "$enable_hint" "$(summary_is_fresh "$env_existed")"
}

# Guards main() from running on source — the test runner sources this file
# to reach the pure functions above without executing anything.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then main "$@"; fi
