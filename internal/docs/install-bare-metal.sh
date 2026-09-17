#!/usr/bin/env bash
# gotcha bare-metal installer (Debian/Ubuntu family, systemd).

GOTCHA_INSTALL_DEFAULT_VERSION="dev"
GOTCHA_INSTALL_DEFAULT_DOWNLOAD_BASE="https://github.com/OtezVikentiy/gotcha/releases/download"

EXIT_OK=0
EXIT_OTHER=1
EXIT_USAGE=2
EXIT_PREFLIGHT=3
EXIT_DOWNLOAD=4

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
  --purge                   with --uninstall, also remove data and databases
EOF
}

# Принимает ID/ID_LIKE как аргументы, а не читает /etc/os-release сама —
# так функция остаётся чистой и тестируемой, preflight передаёт значения.
detect_distro() {
    local id="$1" id_like="${2:-}"
    case "$id" in
        ubuntu | debian) return 0 ;;
    esac
    case " $id_like " in
        *" debian "* | *" ubuntu "*) return 0 ;;
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

# Сравнение по числовым сегментам X.Y.Z — лексикографическое здесь неверно
# (1.10.0 < 1.9.0 посимвольно, хотя 1.10.0 новее).
version_ge() {
    local a="$1" b="$2"
    local -a av bv
    IFS=. read -r -a av <<<"$a"
    IFS=. read -r -a bv <<<"$b"
    local i x y
    for i in 0 1 2; do
        x="${av[i]:-0}"
        y="${bv[i]:-0}"
        if ((10#$x > 10#$y)); then
            return 0
        fi
        if ((10#$x < 10#$y)); then
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
                # Consumed by the upgrade step's pre-migration pg_dump, not read elsewhere in this file.
                # shellcheck disable=SC2034
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
# MemoryDenyWriteExecute не выставлена: не подтверждена на всей матрице e2e.
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

render_nginx_site() {
    local domain="$1"
    cat <<EOF
server {
    listen 80;
    server_name $domain;
    client_max_body_size 64m;

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

log_step() {
    INSTALL_LOG+=("$1")
    # stderr, не stdout: шаги (fetch_tarball и далее) отдают в stdout свой
    # результат, и лог прогресса не должен в него подмешиваться.
    printf 'install-bare-metal: %s\n' "$1" >&2
}

on_err() {
    local code=$?
    printf 'install-bare-metal: FAILED (exit %d)\n' "$code" >&2
    if [ "${#INSTALL_LOG[@]}" -gt 0 ]; then
        printf 'install-bare-metal: completed steps so far:\n' >&2
        printf '  - %s\n' "${INSTALL_LOG[@]}" >&2
        printf 'install-bare-metal: re-run to retry (idempotent) or pass --uninstall to remove what was done\n' >&2
    fi
}

cleanup_tmp_dirs() {
    local d
    for d in "${TMP_DIRS[@]}"; do
        [ -n "$d" ] && rm -rf "$d"
    done
}

# Единственное место, читающее реальное состояние хоста (os-release, uname,
# порты, RAM, диск) — остальные решения идут через чистые функции выше.
preflight() {
    [ "$(id -u)" = 0 ] || fail "$EXIT_PREFLIGHT" "must run as root"
    [ -d /run/systemd/system ] || fail "$EXIT_PREFLIGHT" "systemd is required (PID 1 is not systemd)"

    [ -r /etc/os-release ] || fail "$EXIT_PREFLIGHT" "cannot read /etc/os-release"
    local os_id os_id_like
    # IFS=' ': main() сузила глобальный IFS до "\n\t", обычный read по
    # пробелу здесь бы не разбил строку на два поля.
    IFS=' ' read -r os_id os_id_like < <(
        # shellcheck source=/dev/null
        . /etc/os-release
        printf '%s %s\n' "$ID" "${ID_LIKE:-}"
    )
    detect_distro "$os_id" "$os_id_like" \
        || fail "$EXIT_PREFLIGHT" "unsupported distribution: $os_id (Debian/Ubuntu family required)"

    HOST_ARCH=$(detect_arch "$(uname -m)") \
        || fail "$EXIT_PREFLIGHT" "unsupported architecture: $(uname -m) (amd64/arm64 only)"

    local cmd
    for cmd in curl tar gpg openssl sha256sum; do
        command -v "$cmd" >/dev/null 2>&1 || fail "$EXIT_PREFLIGHT" "$cmd is required"
    done

    local -a ports=(8080)
    [ -n "$ARG_NO_PROXY" ] || ports+=(80)
    [ -n "$ARG_SKIP_DATABASES" ] || ports+=(5432 8123 9000)
    local port
    for port in "${ports[@]}"; do
        if ss -ltn 2>/dev/null | awk '{print $4}' | grep -q ":${port}\$"; then
            fail "$EXIT_PREFLIGHT" "port $port is already in use"
        fi
    done

    # 1900, не 2048: облачные образы на "2 ГБ" нередко отдают в MemTotal
    # немного меньше номинала (память под firmware/hypervisor).
    local ram_mb
    ram_mb=$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo)
    [ "$ram_mb" -ge 1900 ] || fail "$EXIT_PREFLIGHT" "at least 2 GB RAM required (found ${ram_mb} MB)"

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

main() {
    set -euo pipefail
    IFS=$'\n\t'
    trap on_err ERR
    trap cleanup_tmp_dirs EXIT

    # Не local: exit() во вложенных функциях рвёт цепочку динамических областей
    # видимости раньше, чем сработает EXIT/ERR-трап, и он увидел бы их пустыми.
    INSTALL_LOG=()
    TMP_DIRS=()

    parse_args "$@"
    if [ -n "$ARG_HELP" ]; then
        exit "$EXIT_OK"
    fi

    preflight

    local tarball_root
    tarball_root=$(fetch_tarball "$ARG_VERSION" "$HOST_ARCH" "$ARG_DOWNLOAD_BASE" "$ARG_FROM_TARBALL")

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

    fail "$EXIT_OTHER" "tarball verified at $tarball_root, but installation steps beyond preflight and download are not built into this copy of the script yet"
}

# Guards main() from running on source — the test runner sources this file
# to reach the pure functions above without executing anything.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then main "$@"; fi
