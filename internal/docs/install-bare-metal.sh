#!/usr/bin/env bash
# gotcha bare-metal installer (Debian/Ubuntu and RHEL families, systemd).

GOTCHA_INSTALL_DEFAULT_VERSION="dev"
GOTCHA_INSTALL_DEFAULT_DOWNLOAD_BASE="https://github.com/OtezVikentiy/gotcha/releases/download"

# Источник истины — docker-compose.yml (postgres:17-alpine,
# clickhouse-server:25.3-alpine); сверяет internal/guards/docs_versions_test.go.
PG_MAJOR="17"
CH_VERSION="25.3"

# Отпечатки подписывающих ключей вендоров, тот же принцип, что и digest баз в
# Dockerfile: значение фиксируется руками, не берётся с сервера доверчиво.
# PGDG_RPM_KEY_FINGERPRINT — другой ключ, чем PGDG_KEY_FINGERPRINT: rpm и apt
# репозитории PGDG подписаны разными ключами.
PGDG_KEY_FINGERPRINT="B97B0AFCAA1A47F044F244A07FCC7D46ACCC4CF8"
PGDG_RPM_KEY_FINGERPRINT="D4BF08AE67A0B4C7A1DBCCD240BCA2B408B40D20"
CLICKHOUSE_KEY_FINGERPRINT="3A9EA1193A97B548BE1457D48919F6BD2B48D754"

PGDG_RPM_KEY_URL="https://download.postgresql.org/pub/repos/yum/keys/PGDG-RPM-GPG-KEY-RHEL"
PGDG_RPM_KEY_PATH=/etc/pki/rpm-gpg/gotcha-pgdg.asc
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
  --domain D              put nginx in front of this domain
  --email E                contact for certbot (requires --domain)
  --no-proxy               do not install or touch nginx
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

# Внутренняя часть detect_platform, выставляющая пути по уже известным
# HOST_FAMILY/EL_MAJOR — тестируется отдельно, detect_platform целиком читает
# /etc/os-release и на машине разработчика всегда дала бы debian.
apply_platform_paths() {
    declare -gA PKG_HINTS=(
        [curl]=curl [tar]=tar [openssl]=openssl [sha256sum]=coreutils [sudo]=sudo
    )
    if [ "$HOST_FAMILY" = rhel ]; then
        PG_UNIT="postgresql-$PG_MAJOR"
        PG_PACKAGE="postgresql${PG_MAJOR}-server"
        PG_BIN_DIR="/usr/pgsql-$PG_MAJOR/bin"
        NGINX_SITE=/etc/nginx/conf.d/gotcha.conf
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

# Глобальные ARG_* вместо структуры — main/preflight читают их напрямую.
# Каждый вызов сбрасывает их к дефолтам для повторных вызовов тест-раннера.
parse_args() {
    ARG_VERSION="$GOTCHA_INSTALL_DEFAULT_VERSION"
    ARG_FROM_TARBALL=""
    ARG_DOWNLOAD_BASE="$GOTCHA_INSTALL_DEFAULT_DOWNLOAD_BASE"
    ARG_BASE_URL=""
    ARG_DOMAIN=""
    ARG_EMAIL=""
    ARG_NO_PROXY=""
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
            --no-proxy)
                ARG_NO_PROXY=1
                shift
                continue
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
            --domain) ARG_DOMAIN="$val" ;;
            --email) ARG_EMAIL="$val" ;;
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

    if [ -n "$ARG_DOMAIN" ] && [ -n "$ARG_NO_PROXY" ]; then
        printf 'install-bare-metal: --domain and --no-proxy are mutually exclusive\n' >&2
        return "$EXIT_USAGE"
    fi
    if [ -n "$ARG_PURGE" ] && [ -z "$ARG_UNINSTALL" ]; then
        printf 'install-bare-metal: --purge requires --uninstall\n' >&2
        return "$EXIT_USAGE"
    fi
    if [ -n "$ARG_EMAIL" ] && [ -z "$ARG_DOMAIN" ]; then
        printf 'install-bare-metal: --email requires --domain\n' >&2
        return "$EXIT_USAGE"
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

# Порядок приоритета §4.3: --base-url, иначе --domain (HTTPS), иначе IP хоста
# (HTTP). Интерактивный вопрос и --yes-предупреждение — в determine_base_url.
choose_base_url() {
    local base_url_flag="$1" domain_flag="$2" host_ip="$3"
    if [ -n "$base_url_flag" ]; then
        printf '%s\n' "$base_url_flag"
    elif [ -n "$domain_flag" ]; then
        printf 'https://%s\n' "$domain_flag"
    else
        printf 'http://%s\n' "$host_ip"
    fi
}

# §4.3 шаг 2 без --base-url/--domain: интерактивный вопрос, либо (--yes)
# громкое предупреждение. Побочные эффекты — вне чистых функций теста.
determine_base_url() {
    local base_url_flag="$1" domain_flag="$2" host_ip="$3" yes="$4"
    if [ -n "$base_url_flag" ] || [ -n "$domain_flag" ]; then
        choose_base_url "$base_url_flag" "$domain_flag" "$host_ip"
        return
    fi
    if [ -n "$yes" ]; then
        printf 'install-bare-metal: WARNING: no --base-url/--domain given, defaulting to http://%s. GOTCHA_BASE_URL must match exactly what users type in their browser, or every POST (including the first registration) is rejected with 403. Change it later by editing /etc/gotcha/gotcha.env and restarting the service.\n' "$host_ip" >&2
        choose_base_url "" "" "$host_ip"
        return
    fi
    local answer
    read -r -p "GOTCHA_BASE_URL [http://$host_ip]: " answer
    if [ -n "$answer" ]; then
        printf '%s\n' "$answer"
    else
        choose_base_url "" "" "$host_ip"
    fi
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
MemoryMax=$memory_max

[Install]
WantedBy=multi-user.target
EOF
}

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
EOF
}

NGINX_SITE_MARKER="# gotcha site: install-bare-metal.sh keeps local edits below on re-run"

render_nginx_site() {
    local domain="$1"
    cat <<EOF
$NGINX_SITE_MARKER
server {
    listen 80;
    server_name $domain;
    client_max_body_size 64m;

    # /healthz, /readyz stay open below for external uptime checks;
    # /metrics and /version leak internal/build detail, loopback-only.
    location ~ ^/(metrics|version)$ {
        allow 127.0.0.1;
        allow ::1;
        deny all;
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host \$host;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
    }

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host \$host;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
    }
}
EOF
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
    # sudo нужен только своим СУБД: psql от пользователя postgres и pg_dump перед обновлением.
    [ -n "$skip_databases" ] || printf 'sudo\n'
}

port_owner_units() {
    case "$1" in
        8080) printf 'gotcha\n' ;;
        # angie: на части хостов штатный веб-сервер — он, и отказ по занятому
        # 80 порту там был бы отказом установке на исправном хосте.
        80) printf 'nginx\nangie\n' ;;
        5432) printf '%s\n' "$PG_UNIT" ;;
        *) printf 'clickhouse-server\n' ;;
    esac
}

# Читает реальное состояние хоста (uname, порты, RAM, диск) — платформа уже
# определена detect_platform, остальные решения идут через чистые функции выше.
preflight() {
    [ "$(id -u)" = 0 ] || fail "$EXIT_PREFLIGHT" "must run as root"
    [ -d /run/systemd/system ] || fail "$EXIT_PREFLIGHT" "systemd is required (PID 1 is not systemd)"

    HOST_ARCH=$(detect_arch "$(uname -m)") \
        || fail "$EXIT_PREFLIGHT" "unsupported architecture: $(uname -m) (amd64/arm64 only)"

    # Пакеты в сообщении не украшение: на минимальном Debian нет ни ss, ни sudo,
    # и без подсказки отказ выглядит как поломка скрипта.
    local cmd
    while IFS= read -r cmd; do
        command -v "$cmd" >/dev/null 2>&1 \
            || fail "$EXIT_PREFLIGHT" "$cmd is required ($PKG_HINT_LABEL: ${PKG_HINTS[$cmd]})"
    done < <(required_commands "$HOST_FAMILY" "$ARG_SKIP_DATABASES")

    local -a ports=(8080)
    [ -n "$ARG_NO_PROXY" ] || ports+=(80)
    [ -n "$ARG_SKIP_DATABASES" ] || ports+=(5432 8123 9000)
    local port owner owned
    for port in "${ports[@]}"; do
        ss -ltn 2>/dev/null | awk '{print $4}' | grep -q ":${port}\$" || continue
        # Занятый порт — отказ, только если это не наш же юнит с прошлого запуска;
        # иначе идемпотентный повторный запуск (§4.4) не проходил бы преflight.
        owned=""
        while IFS= read -r owner; do
            systemctl is-active --quiet "$owner" && { owned=1; break; }
        done < <(port_owner_units "$port")
        [ -n "$owned" ] && continue
        if [ "$port" = 80 ]; then
            fail "$EXIT_PREFLIGHT" "port 80 is already in use by something that is not nginx or angie (pass --no-proxy to keep your own web server)"
        fi
        fail "$EXIT_PREFLIGHT" "port $port is already in use"
    done

    # 1900, не 2048: облачные "2 ГБ" урезают MemTotal под firmware/hypervisor.
    # Не local — install_clickhouse переиспользует значение для 10-small.xml.
    HOST_RAM_MB=$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo)
    [ "$HOST_RAM_MB" -ge 1900 ] || fail "$EXIT_PREFLIGHT" "at least 2 GB RAM required (found ${HOST_RAM_MB} MB)"

    local disk_gb
    disk_gb=$(($(df --output=avail -k / | tail -n1) / 1024 / 1024))
    [ "$disk_gb" -ge 20 ] || fail "$EXIT_PREFLIGHT" "at least 20 GB free disk required (found ${disk_gb} GB)"
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

# gpg --with-colons: формат вывода стабилен для парсинга скриптом, в отличие
# от --fingerprint, рассчитанного на человека. Подключи остаются принятыми по
# самоподписи основного ключа намеренно: пин подключей ронял бы установку при
# их штатной ротации вендором.
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

# deb-ветка не подставляет нативный мажор молча: PGDG публикует EL9/EL10 всегда,
# и штатный AppStream мажора 17 не содержит.
repo_add_pgdg() {
    local codename="$1"
    if [ "$HOST_FAMILY" = rhel ]; then
        local tmp
        tmp=$(mktemp -d)
        TMP_DIRS+=("$tmp")
        curl -fsSL -o "$tmp/pgdg.asc" "$PGDG_RPM_KEY_URL" \
            || fail "$EXIT_DATABASE" "failed to download the PGDG signing key from $PGDG_RPM_KEY_URL"
        verify_key_fingerprint "$tmp/pgdg.asc" "$PGDG_RPM_KEY_FINGERPRINT"
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

    pkg_install "$package" || fail "$EXIT_DATABASE" "failed to install $package"

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
    role_exists=$(sudo -u postgres "$PG_BIN_DIR/psql" -tAc "SELECT 1 FROM pg_roles WHERE rolname = 'gotcha'" 2>/dev/null)
    if [ "$role_exists" != "1" ]; then
        password=$(openssl rand -hex 24)
    elif [ ! -f "$env_file" ]; then
        password=$(openssl rand -hex 24)
        log_step "WARNING: gotcha role exists but $env_file is missing — regenerating its PostgreSQL password"
    fi
    if [ -n "$password" ] && ! sudo -u postgres "$PG_BIN_DIR/psql" -v ON_ERROR_STOP=1 -q >/dev/null <<SQL
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
    if ! sudo -u postgres "$PG_BIN_DIR/psql" -v ON_ERROR_STOP=1 -q >/dev/null <<'SQL'
SELECT 'CREATE DATABASE gotcha OWNER gotcha'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'gotcha')\gexec
SQL
    then
        fail "$EXIT_DATABASE" "failed to create the gotcha database in PostgreSQL"
    fi

    log_step "PostgreSQL $PG_MAJOR installed and configured"
    printf 'postgres://gotcha:%s@127.0.0.1:5432/gotcha?sslmode=disable\n' "$password"
}

# ClickHouse не публикует пакет без патч-версии в номере — apt-cache madison
# находит конкретный патч для мажора.минора из CH_VERSION.
clickhouse_package_version() {
    apt-cache madison clickhouse-server 2>/dev/null \
        | awk -F'|' -v v="$CH_VERSION." '{gsub(/^[ \t]+|[ \t]+$/, "", $2)} $2 ~ ("^" v) {print $2; exit}'
}

# Возвращает через stdout DSN на 127.0.0.1; ставит пакет, конфиги из тарбола,
# пользователя gotcha и лимит файловых дескрипторов. Отказ любого шага — код 5.
install_clickhouse() {
    local ram_mb="$1" tarball_root="$2" env_file="$3"
    local tmp keyring
    tmp=$(mktemp -d)
    TMP_DIRS+=("$tmp")
    curl -fsSL -o "$tmp/clickhouse.asc" https://packages.clickhouse.com/rpm/lts/repodata/repomd.xml.key \
        || fail "$EXIT_DATABASE" "failed to download the ClickHouse signing key"
    verify_key_fingerprint "$tmp/clickhouse.asc" "$CLICKHOUSE_KEY_FINGERPRINT"
    keyring=/usr/share/keyrings/gotcha-clickhouse.gpg
    gpg --dearmor <"$tmp/clickhouse.asc" >"$keyring"
    printf 'deb [signed-by=%s] https://packages.clickhouse.com/deb stable main\n' "$keyring" \
        >/etc/apt/sources.list.d/gotcha-clickhouse.list
    pkg_refresh || fail "$EXIT_DATABASE" "apt-get update failed after adding the ClickHouse repository"

    local version
    version=$(clickhouse_package_version)
    [ -n "$version" ] || fail "$EXIT_DATABASE" "no clickhouse-server package matches version $CH_VERSION"

    # clickhouse-common-static нужен явной версией: без него apt подтягивает
    # последний мажор из репозитория и ловит конфликт зависимостей.
    pkg_install \
        "clickhouse-server=$version" "clickhouse-client=$version" "clickhouse-common-static=$version" \
        || fail "$EXIT_DATABASE" "failed to install clickhouse-server $version"

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
    sudo -u postgres "$PG_BIN_DIR/pg_dump" -d gotcha | gzip >"$dump" || fail "$EXIT_DATABASE" "pre-upgrade pg_dump failed"
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
    chown root:gotcha "$tmp" || { rm -f "$tmp"; fail "$EXIT_OTHER" "failed to set ownership/permissions on $env_file"; }
    chmod 0640 "$tmp" || { rm -f "$tmp"; fail "$EXIT_OTHER" "failed to set ownership/permissions on $env_file"; }
    mv "$tmp" "$env_file" || fail "$EXIT_OTHER" "failed to install $env_file"
    log_step "config file created: $env_file"
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
    systemctl enable --now gotcha || fail "$EXIT_APP" "failed to enable/start the gotcha service"

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

# Снимает штатный дефолтный сайт (конфликтовал бы default_server'ом на 80). Копия
# делается ДО перезаписи; при точном совпадении с прошлым рендером — не бэкапится вовсе.
install_nginx() {
    local domain="$1" site="$NGINX_SITE" rendered
    pkg_install nginx || fail "$EXIT_OTHER" "failed to install nginx"

    rm -f /etc/nginx/sites-enabled/default

    rendered=$(render_nginx_site "$domain")
    if [ ! -f "$site" ]; then
        printf '%s\n' "$rendered" >"$site" || fail "$EXIT_OTHER" "failed to render $site"
    elif [ "$(cat "$site")" != "$rendered" ]; then
        # Местные правки нашего файла — это TLS-блок certbot, перезапись вернула бы
        # хост на голый HTTP. Сменившийся домен — другое дело, сайт рендерится заново.
        if grep -qF "$NGINX_SITE_MARKER" "$site" \
            && grep -qE "^[[:space:]]*server_name[[:space:]]+$domain;" "$site"; then
            log_step "nginx site kept as it is — it is ours and has local edits (certbot TLS, most likely): $site"
        else
            cp "$site" "$site.bak-$(date +%s)" || fail "$EXIT_OTHER" "failed to back up existing $site"
            printf '%s\n' "$rendered" >"$site" || fail "$EXIT_OTHER" "failed to render $site"
        fi
    fi
    ln -sf ../sites-available/gotcha /etc/nginx/sites-enabled/gotcha \
        || fail "$EXIT_OTHER" "failed to enable $site"

    nginx -t || fail "$EXIT_OTHER" "nginx configuration test failed"
    systemctl enable --now nginx || fail "$EXIT_OTHER" "failed to start nginx"
    systemctl reload nginx || fail "$EXIT_OTHER" "failed to reload nginx"

    log_step "nginx installed, proxying to gotcha for $domain"
}

# Отказ certbot не откатывает установку: HTTP-стенд остаётся рабочим, скрипт
# лишь печатает команду для повтора и возвращается с кодом 0.
install_certificate() {
    local domain="$1" email="$2"
    pkg_install certbot python3-certbot-nginx || fail "$EXIT_OTHER" "failed to install certbot"

    if certbot --nginx -d "$domain" -m "$email" --agree-tos --non-interactive --redirect >/dev/null 2>&1; then
        log_step "TLS certificate issued for $domain"
    else
        printf 'install-bare-metal: certbot failed to obtain a certificate for %s; HTTP on port 80 still works, retry later with:\n' "$domain" >&2
        printf '  certbot --nginx -d %s -m %s --agree-tos --redirect\n' "$domain" "$email" >&2
    fi
}

# Не трогает пакеты СУБД/nginx и apt-репозитории — на хосте ими может пользоваться
# что-то ещё. --purge снимает только объекты, которые этот скрипт сам и создал.
uninstall_app() {
    local purge="$1"
    systemctl disable --now gotcha >/dev/null 2>&1 || true
    rm -f /etc/systemd/system/gotcha.service
    systemctl daemon-reload || true
    rm -f /usr/local/bin/gotcha
    log_step "gotcha unit and binary removed"

    # Включённый сайт без бэкенда — 502 на всё: sites-enabled/default скрипт снял при
    # установке. Сам sites-available остаётся, в нём TLS-блок certbot.
    if [ -L /etc/nginx/sites-enabled/gotcha ] || [ -e /etc/nginx/sites-enabled/gotcha ]; then
        rm -f /etc/nginx/sites-enabled/gotcha
        systemctl reload nginx >/dev/null 2>&1 || true
        log_step "nginx site disabled (the file in sites-available is kept)"
    fi

    [ -n "$purge" ] || return 0

    if id -u postgres >/dev/null 2>&1; then
        sudo -u postgres "$PG_BIN_DIR/psql" -v ON_ERROR_STOP=1 -q >/dev/null <<'SQL' || fail "$EXIT_DATABASE" "failed to drop the gotcha role/database in PostgreSQL"
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
    [ -z "$pg_conf_dir" ] || rm -f "$pg_conf_dir/conf.d/10-gotcha.conf"
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
    # Метка запуска: по ней on_exit отбирает из общего журнала шаги текущего
    # запуска, включая записанные подоболочками.
    INSTALL_RUN_ID="$$-$(date -u +%s)"
    trap on_exit EXIT

    parse_args "$@"
    if [ -n "$ARG_HELP" ]; then
        exit "$EXIT_OK"
    fi
    detect_platform
    if [ -n "$ARG_UNINSTALL" ]; then
        [ "$(id -u)" = 0 ] || fail "$EXIT_PREFLIGHT" "must run as root"
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

    preflight

    local tarball_root
    tarball_root=$(fetch_tarball "$ARG_VERSION" "$HOST_ARCH" "$ARG_DOWNLOAD_BASE" "$ARG_FROM_TARBALL")

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

    local host_ip base_url
    host_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
    base_url=$(determine_base_url "$ARG_BASE_URL" "$ARG_DOMAIN" "$host_ip" "$ARG_YES")

    if [ -n "$ARG_DRY_RUN" ]; then
        printf '[dry-run] tarball ready at %s\n' "$tarball_root"
        printf '[dry-run] GOTCHA_BASE_URL=%s\n' "$base_url"
        printf '[dry-run] MemoryMax=%s GOMEMLIMIT=%s\n' "$mem_max" "$gomemlimit"
        printf '[dry-run] would write /etc/systemd/system/gotcha.service:\n'
        render_unit "$mem_max"
        printf '[dry-run] would write /etc/gotcha/gotcha.env:\n'
        render_env_file \
            "postgres://gotcha:<generated>@127.0.0.1:5432/gotcha?sslmode=disable" \
            "clickhouse://gotcha:<generated>@127.0.0.1:9000/gotcha" \
            "<generated>" "$base_url" "/opt/gotcha/agent-dist" "$gomemlimit" "127.0.0.1:8080"
        if [ -z "$ARG_NO_PROXY" ]; then
            printf '[dry-run] would write nginx site (%s):\n' "${ARG_DOMAIN:-$host_ip}"
            render_nginx_site "${ARG_DOMAIN:-$host_ip}"
        fi
        if [ -z "$ARG_SKIP_DATABASES" ]; then
            printf '[dry-run] would write /etc/postgresql/*/main/conf.d/10-gotcha.conf:\n'
            render_pg_conf
        fi
        exit "$EXIT_OK"
    fi

    local env_file=/etc/gotcha/gotcha.env
    # Без env-файла install_postgresql/install_clickhouse ниже могут перевыпустить
    # пароль под уже работающим сервисом — останавливаем его первым, пока не поздно.
    [ -f "$env_file" ] || systemctl stop gotcha 2>/dev/null || true
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
    install_unit "$mem_max"
    run_migrations "$env_file"
    start_app

    if [ -z "$ARG_NO_PROXY" ]; then
        install_nginx "${ARG_DOMAIN:-$host_ip}"
        if [ -n "$ARG_DOMAIN" ] && [ -n "$ARG_EMAIL" ]; then
            install_certificate "$ARG_DOMAIN" "$ARG_EMAIL"
        fi
    fi
}

# Guards main() from running on source — the test runner sources this file
# to reach the pure functions above without executing anything.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then main "$@"; fi
