#!/usr/bin/env bash
# Прогоняет install-bare-metal.sh НА ЭТОЙ МАШИНЕ и проверяет пакеты/юниты/порты/конфиги; тарбол не собирает — ночная матрица гоняет его без Go.
set -uo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
INSTALLER="$SCRIPT_DIR/../internal/docs/install-bare-metal.sh"

TARBALL=""
UPGRADE_FROM=""
FORCE=""

usage() {
    cat <<'EOF'
Usage: bare-metal-e2e.sh --tarball PATH [--upgrade-from PATH] [--i-know-this-wipes-the-host]

Installs PostgreSQL, ClickHouse and gotcha on THIS host via
install-bare-metal.sh and asserts the result against the real system.
Destructive: only run inside a disposable container or in CI.

--upgrade-from PATH is a tarball for a version older than --tarball: when
given, it is installed first and the upgrade path (backup, binary swap,
migrations) is asserted on top of it. Omit it to skip that coverage.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --tarball)
            TARBALL="${2:-}"
            shift 2
            ;;
        --upgrade-from)
            UPGRADE_FROM="${2:-}"
            shift 2
            ;;
        --i-know-this-wipes-the-host)
            FORCE=1
            shift
            ;;
        -h | --help)
            usage
            exit 0
            ;;
        *)
            printf 'bare-metal-e2e: unknown argument: %s\n' "$1" >&2
            usage >&2
            exit 2
            ;;
    esac
done

[ -n "$TARBALL" ] || { printf 'bare-metal-e2e: --tarball is required\n' >&2; exit 2; }
[ -f "$TARBALL" ] || { printf 'bare-metal-e2e: tarball not found: %s\n' "$TARBALL" >&2; exit 2; }
[ -z "$UPGRADE_FROM" ] || [ -f "$UPGRADE_FROM" ] || { printf 'bare-metal-e2e: --upgrade-from tarball not found: %s\n' "$UPGRADE_FROM" >&2; exit 2; }
[ -f "$INSTALLER" ] || { printf 'bare-metal-e2e: installer not found: %s\n' "$INSTALLER" >&2; exit 1; }

# Источник PG_MAJOR/CH_VERSION для ассертов ниже — та же переменная, что ставит install-bare-metal.sh, а не отдельно вписанное число.
# shellcheck source=/dev/null
. "$INSTALLER"

# Единственный предохранитель между этим скриптом и переустановкой СУБД на
# рабочей машине оператора: без одного из трёх сигналов ниже — отказ.
is_disposable_environment() {
    [ "${CI:-}" = "true" ] && return 0
    [ -f /.dockerenv ] && return 0
    grep -qE '(docker|containerd|kubepods)' /proc/1/cgroup 2>/dev/null && return 0
    return 1
}

if [ -z "$FORCE" ] && ! is_disposable_environment; then
    cat >&2 <<'EOF'
bare-metal-e2e: refusing to run — this does not look like a disposable
environment (no CI=true, no container markers in /.dockerenv or
/proc/1/cgroup). This script installs PostgreSQL and ClickHouse on THIS
machine and will disrupt anything already running there.

Run it inside a privileged container, in CI, or pass
--i-know-this-wipes-the-host if you are certain this host is disposable.
EOF
    exit 1
fi

FAILURES=0

# Точка расширения: следующие задачи (приложение, nginx, снятие установки)
# дописывают свои assert_* и добавляют их вызов в run_assertions ниже.
assert() {
    local desc="$1"
    shift
    if "$@"; then
        printf 'ok: %s\n' "$desc"
    else
        printf 'FAIL: %s\n' "$desc" >&2
        FAILURES=$((FAILURES + 1))
    fi
}

pkg_installed() {
    dpkg -s "$1" >/dev/null 2>&1
}

unit_active() {
    systemctl is-active --quiet "$1"
}

# ss отдаёт Local Address:Port как "127.0.0.1:5432"/"[::1]:5432". Публикация
# наружу — регресс: в compose эти порты вообще не выставлены.
port_loopback_only() {
    local port="$1" addrs addr
    addrs=$(ss -ltn 2>/dev/null | awk -v p=":${1}\$" '$4 ~ p {print $4}')
    [ -n "$addrs" ] || { printf 'not listening on port %s\n' "$port" >&2; return 1; }
    while IFS= read -r addr; do
        case "$addr" in
            127.0.0.1:"$port" | \[::1\]:"$port") ;;
            *)
                printf 'port %s listens on non-loopback address %s\n' "$port" "$addr" >&2
                return 1
                ;;
        esac
    done <<<"$addrs"
}

pg_conf_dir() {
    find /etc/postgresql -mindepth 2 -maxdepth 2 -type d -name main 2>/dev/null | head -n1
}

pg_gotcha_conf_present() {
    local dir conf
    dir=$(pg_conf_dir)
    conf="$dir/conf.d/10-gotcha.conf"
    [ -f "$conf" ] || { printf 'missing %s\n' "$conf" >&2; return 1; }
    grep -qE '^random_page_cost = 1\.1$' "$conf" || { printf 'random_page_cost missing in %s\n' "$conf" >&2; return 1; }
    grep -qE '^effective_io_concurrency = 200$' "$conf" || { printf 'effective_io_concurrency missing in %s\n' "$conf" >&2; return 1; }
}

pg_role_exists() {
    [ "$(sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname = 'gotcha'" 2>/dev/null)" = "1" ]
}

pg_database_exists() {
    [ "$(sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname = 'gotcha'" 2>/dev/null)" = "1" ]
}

ch_common_config_present() {
    [ -f /etc/clickhouse-server/config.d/00-common.xml ]
}

ch_limit_nofile() {
    local value
    value=$(systemctl show clickhouse-server --property=LimitNOFILE --value 2>/dev/null)
    [ "$value" = "262144" ] || { printf 'clickhouse-server LimitNOFILE=%s, want 262144\n' "$value" >&2; return 1; }
}

ch_gotcha_user_configured() {
    grep -q 'password_sha256_hex' /etc/clickhouse-server/users.d/10-gotcha.xml 2>/dev/null
}

ch_gotcha_database_exists() {
    [ "$(clickhouse-client --query "EXISTS DATABASE gotcha" 2>/dev/null)" = "1" ]
}

readyz_via_nginx() {
    [ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:80/readyz)" = "200" ]
}

nginx_site_config_ok() {
    local conf=/etc/nginx/sites-available/gotcha
    [ -f "$conf" ] || { printf 'missing %s\n' "$conf" >&2; return 1; }
    grep -qF 'proxy_pass http://127.0.0.1:8080' "$conf" || { printf 'proxy_pass missing in %s\n' "$conf" >&2; return 1; }
    grep -qF 'proxy_set_header Host' "$conf" || { printf 'Host forwarding missing in %s\n' "$conf" >&2; return 1; }
    grep -qF 'X-Forwarded-For' "$conf" || { printf 'X-Forwarded-For missing in %s\n' "$conf" >&2; return 1; }
    grep -qF 'X-Forwarded-Proto' "$conf" || { printf 'X-Forwarded-Proto missing in %s\n' "$conf" >&2; return 1; }
    grep -qF 'client_max_body_size' "$conf" || { printf 'client_max_body_size missing in %s\n' "$conf" >&2; return 1; }
}

nginx_site_backed_up() {
    local backup
    backup=$(find /etc/nginx/sites-available -maxdepth 1 -name 'gotcha.bak-*' -print -quit 2>/dev/null)
    [ -n "$backup" ] || { printf 'no gotcha.bak-* found in /etc/nginx/sites-available\n' >&2; return 1; }
    grep -qF 'pre-existing site placed by someone else' "$backup" \
        || { printf 'backup does not preserve the original foreign content: %s\n' "$backup" >&2; return 1; }
}

# Порт 80 проверяется в preflight до любых побочных эффектов (код 3, как и
# весь preflight), поэтому installer можно звать реальными флагами безопасно.
port80_busy_blocks_preflight() {
    command -v python3 >/dev/null 2>&1 || {
        apt-get update -qq >/dev/null
        DEBIAN_FRONTEND=noninteractive apt-get install -y -qq python3-minimal >/dev/null
    }

    python3 -c '
import socket, time
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", 80))
s.listen(1)
time.sleep(30)
' &
    local listener_pid=$!

    local tries=0
    until ss -ltn 2>/dev/null | awk '{print $4}' | grep -q ':80$'; do
        tries=$((tries + 1))
        if [ "$tries" -ge 10 ]; then
            printf 'dummy listener on port 80 never came up\n' >&2
            kill "$listener_pid" 2>/dev/null
            return 1
        fi
        sleep 1
    done

    local output rc
    output=$(bash "$INSTALLER" --version "$tarball_version" --from-tarball "$WORK_TARBALL" --yes 2>&1)
    rc=$?
    kill "$listener_pid" 2>/dev/null
    wait "$listener_pid" 2>/dev/null

    [ "$rc" -eq 3 ] || { printf 'expected exit 3 with port 80 busy, got %d:\n%s\n' "$rc" "$output" >&2; return 1; }
    printf '%s\n' "$output" | grep -q 'port 80 is already in use' \
        || { printf 'missing "port 80 is already in use" in output:\n%s\n' "$output" >&2; return 1; }
}

# fetch_tarball требует SHA256SUMS.txt рядом с тарболом; release.sh её пока не
# публикует, а каталог тарбола часто read-only — считаем сумму в своей копии.
WORK_DIR=$(mktemp -d)
cp "$TARBALL" "$WORK_DIR/"
WORK_TARBALL="$WORK_DIR/$(basename "$TARBALL")"
(cd "$WORK_DIR" && sha256sum "$(basename "$WORK_TARBALL")") >"$WORK_DIR/SHA256SUMS.txt"

# Оба тарбола делят один WORK_DIR/SHA256SUMS.txt — fetch_tarball ищет свою строку
# по имени файла, порядок строк в файле не важен.
WORK_UPGRADE_TARBALL=""
if [ -n "$UPGRADE_FROM" ]; then
    cp "$UPGRADE_FROM" "$WORK_DIR/"
    WORK_UPGRADE_TARBALL="$WORK_DIR/$(basename "$UPGRADE_FROM")"
    (cd "$WORK_DIR" && sha256sum "$(basename "$WORK_UPGRADE_TARBALL")") >>"$WORK_DIR/SHA256SUMS.txt"
fi

# Отдельный процесс, не подоболочка текущего: значение переживает его и
# читается после завершения install-bare-metal.sh и всех проверок.
RSS_PEAK_FILE="$WORK_DIR/rss_peak_kb"
printf '0\n' >"$RSS_PEAK_FILE"
(
    while :; do
        kb=$(ps -eo rss,comm 2>/dev/null | awk '/postgres|clickhouse|gotcha/{sum+=$1} END{print sum+0}')
        peak=$(cat "$RSS_PEAK_FILE" 2>/dev/null || echo 0)
        [ "${kb:-0}" -gt "${peak:-0}" ] && printf '%s\n' "$kb" >"$RSS_PEAK_FILE"
        sleep 1
    done
) &
RSS_SAMPLER_PID=$!
trap 'kill "$RSS_SAMPLER_PID" 2>/dev/null; rm -rf "$WORK_DIR"' EXIT

# fetch_tarball сверяет ARG_VERSION с VERSION-файлом внутри тарбола — версия
# здесь берётся из имени файла (тот же формат, что печатает dist_url).
tarball_version=$(basename "$WORK_TARBALL" | sed -E 's/^gotcha-(.+)-linux-[^-]+\.tar\.gz$/\1/')
if [ -z "$tarball_version" ] || [ "$tarball_version" = "$(basename "$WORK_TARBALL")" ]; then
    printf 'bare-metal-e2e: cannot parse version out of tarball name: %s\n' "$WORK_TARBALL" >&2
    exit 2
fi

upgrade_from_version=""
if [ -n "$WORK_UPGRADE_TARBALL" ]; then
    upgrade_from_version=$(basename "$WORK_UPGRADE_TARBALL" | sed -E 's/^gotcha-(.+)-linux-[^-]+\.tar\.gz$/\1/')
    if [ -z "$upgrade_from_version" ] || [ "$upgrade_from_version" = "$(basename "$WORK_UPGRADE_TARBALL")" ]; then
        printf 'bare-metal-e2e: cannot parse version out of --upgrade-from tarball name: %s\n' "$WORK_UPGRADE_TARBALL" >&2
        exit 2
    fi
fi

assert "a busy port 80 blocks preflight before anything is installed" port80_busy_blocks_preflight

# Симулирует чужой конфиг сайта, уже лежащий на месте нашего: install_nginx
# обязан унести его в *.bak-<метка времени>, а не переписать без следа.
mkdir -p /etc/nginx/sites-available
printf '# pre-existing site placed by someone else\n' >/etc/nginx/sites-available/gotcha

# Ставит более старую версию первой, чтобы запуск ниже был обновлением (§4.5), а не
# свежей установкой — "старая" версия детектится только по уже установленному бинарю.
if [ -n "$WORK_UPGRADE_TARBALL" ]; then
    printf 'bare-metal-e2e: running install-bare-metal.sh --version %s --from-tarball %s --yes (upgrade baseline)\n' \
        "$upgrade_from_version" "$WORK_UPGRADE_TARBALL"
    bash "$INSTALLER" --version "$upgrade_from_version" --from-tarball "$WORK_UPGRADE_TARBALL" --yes
    baseline_rc=$?
    printf 'bare-metal-e2e: upgrade baseline install exited %d\n' "$baseline_rc"
fi

printf 'bare-metal-e2e: running install-bare-metal.sh --version %s --from-tarball %s --yes\n' \
    "$tarball_version" "$WORK_TARBALL"
bash "$INSTALLER" --version "$tarball_version" --from-tarball "$WORK_TARBALL" --yes
installer_rc=$?
printf 'bare-metal-e2e: install-bare-metal.sh exited %d\n' "$installer_rc"

installer_succeeded() {
    [ "$installer_rc" -eq 0 ]
}

baseline_install_succeeded() {
    [ "$baseline_rc" -eq 0 ]
}

upgrade_took_backup() {
    local found
    found=$(find /var/lib/gotcha/backup -maxdepth 1 -name 'postgres-*.sql.gz' -print -quit 2>/dev/null)
    [ -n "$found" ] || { printf 'no postgres-*.sql.gz found in /var/lib/gotcha/backup\n' >&2; return 1; }
}

upgrade_kept_previous_binary() {
    [ -f "/opt/gotcha/backup/gotcha-$upgrade_from_version" ] \
        || { printf 'missing /opt/gotcha/backup/gotcha-%s\n' "$upgrade_from_version" >&2; return 1; }
}

readyz_ok() {
    [ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/readyz)" = "200" ]
}

dir_secured() {
    local dir="$1" mode owner
    [ -d "$dir" ] || { printf 'missing %s\n' "$dir" >&2; return 1; }
    mode=$(stat -c '%a' "$dir")
    owner=$(stat -c '%U' "$dir")
    [ "$mode" = "700" ] || { printf '%s mode is %s, want 700\n' "$dir" "$mode" >&2; return 1; }
    [ "$owner" = "gotcha" ] || { printf '%s owner is %s, want gotcha\n' "$dir" "$owner" >&2; return 1; }
}

# StateDirectoryMode юнита задаёт режим только /var/lib/gotcha — каталог выгрузок
# app пересоздаёт сам с фиксированным 0700, поэтому проверяются оба каталога.
state_dir_secured() {
    dir_secured /var/lib/gotcha
}

exports_dir_secured() {
    dir_secured /var/lib/gotcha/exports
}

env_file_secured() {
    local file=/etc/gotcha/gotcha.env got
    [ -f "$file" ] || { printf 'missing %s\n' "$file" >&2; return 1; }
    got=$(stat -c '%U:%G %a' "$file")
    [ "$got" = "root:gotcha 640" ] || { printf '%s is %s, want root:gotcha 640\n' "$file" "$got" >&2; return 1; }
}

# SHA256SUMS вида "хэш  имя_файла" (sha256sum build-dist.sh) — сверяется имя,
# не первая строка, чтобы порядок архитектур в файле был не важен.
agent_binary_download_matches_sums() {
    local name=gotcha-agent-linux-amd64 out want got
    out="$WORK_DIR/$name"
    curl -fsS -o "$out" "http://127.0.0.1:8080/agent/$name" \
        || { printf 'download of /agent/%s failed\n' "$name" >&2; return 1; }
    want=$(awk -v n="$name" '$2 == n {print $1}' /opt/gotcha/agent-dist/SHA256SUMS)
    [ -n "$want" ] || { printf 'no checksum entry for %s\n' "$name" >&2; return 1; }
    got=$(sha256sum "$out" | awk '{print $1}')
    [ "$got" = "$want" ] || { printf 'checksum mismatch for %s: got %s, want %s\n' "$name" "$got" "$want" >&2; return 1; }
}

# TimeoutStopSec — имя директивы юнита, но её D-Bus-свойство называется
# TimeoutStopUSec (systemd 255): systemctl show -p TimeoutStopSec отдаёт пустоту.
unit_hardening_directives() {
    local out
    out=$(systemctl show gotcha -p NoNewPrivileges,ProtectSystem,TasksMax,MemoryMax,CapabilityBoundingSet,TimeoutStopUSec 2>&1) \
        || { printf 'systemctl show gotcha failed:\n%s\n' "$out" >&2; return 1; }
    local directive
    for directive in NoNewPrivileges=yes ProtectSystem=strict TasksMax=512 CapabilityBoundingSet= 'TimeoutStopUSec=1min 30s'; do
        printf '%s\n' "$out" | grep -qx "$directive" \
            || { printf 'missing %s in:\n%s\n' "$directive" "$out" >&2; return 1; }
    done
    printf '%s\n' "$out" | grep -qE '^MemoryMax=[1-9][0-9]*$' \
        || { printf 'MemoryMax is not a positive byte count:\n%s\n' "$out" >&2; return 1; }
}

survives_postgresql_restart() {
    systemctl restart postgresql || { printf 'systemctl restart postgresql failed\n' >&2; return 1; }
    local tries=0
    until readyz_ok; do
        tries=$((tries + 1))
        [ "$tries" -lt 30 ] || { printf '/readyz did not recover after postgresql restart\n' >&2; return 1; }
        sleep 1
    done
}

# Регистрация, онбординг и приём события — путь живого инстанса, не только systemctl/ss.
# App слушает 127.0.0.1:8080 напрямую; Origin шлём равным GOTCHA_BASE_URL, не адресу запроса.
e2e_ingest_roundtrip() {
    local app=http://127.0.0.1:8080 origin jar msg key project_id tries
    origin=$(sed -n 's/^GOTCHA_BASE_URL=//p' /etc/gotcha/gotcha.env)
    [ -n "$origin" ] || { printf 'GOTCHA_BASE_URL missing from gotcha.env\n' >&2; return 1; }
    jar="$WORK_DIR/e2e-cookies.txt"
    : >"$jar"
    msg="bare-metal-e2e-probe-$$"

    curl -fsS -c "$jar" -b "$jar" -o /dev/null -H "Origin: $origin" \
        --data-urlencode "email=e2e-$$@example.invalid" \
        --data-urlencode "password=Str0ng-Passw0rd" \
        --data-urlencode "password2=Str0ng-Passw0rd" \
        "$app/register" || { printf 'POST /register failed\n' >&2; return 1; }

    curl -fsS -c "$jar" -b "$jar" -o /dev/null -H "Origin: $origin" \
        --data-urlencode "org_slug=e2e-org" \
        --data-urlencode "org_name=e2e org" \
        --data-urlencode "project_slug=e2e-project" \
        --data-urlencode "project_name=e2e project" \
        --data-urlencode "platform=go" \
        "$app/onboarding" || { printf 'POST /onboarding failed\n' >&2; return 1; }

    key=$(sudo -u postgres psql -d gotcha -tAc "SELECT public_key FROM project_keys ORDER BY id LIMIT 1")
    project_id=$(sudo -u postgres psql -d gotcha -tAc "SELECT project_id FROM project_keys ORDER BY id LIMIT 1")
    [ -n "$key" ] && [ -n "$project_id" ] \
        || { printf 'could not read a project key out of PostgreSQL\n' >&2; return 1; }

    curl -fsS -o /dev/null "$app/api/$project_id/store/?sentry_key=$key" \
        --data "{\"message\":\"$msg\"}" || { printf 'event submission to /api/%s/store/ failed\n' "$project_id" >&2; return 1; }

    tries=0
    until curl -fsS -c "$jar" -b "$jar" "$app/projects/$project_id/issues" | grep -qF "$msg"; do
        tries=$((tries + 1))
        [ "$tries" -lt 15 ] || { printf 'event never appeared in the issues list\n' >&2; return 1; }
        sleep 1
    done
}

# Действие и проверка вместе, как survives_postgresql_restart выше: §4.4 требует
# юнит/конфиги/права привести к целевому состоянию заново, но не трогать env/пароли/данные.
survives_idempotent_rerun() {
    local env_before="$WORK_DIR/gotcha.env.before-rerun" bak_before bak_after output rc
    cp /etc/gotcha/gotcha.env "$env_before" || { printf 'failed to snapshot gotcha.env\n' >&2; return 1; }
    bak_before=$(find /etc/nginx/sites-available -maxdepth 1 -name 'gotcha.bak-*' | wc -l)

    output=$(bash "$INSTALLER" --version "$tarball_version" --from-tarball "$WORK_TARBALL" --yes 2>&1)
    rc=$?
    [ "$rc" -eq 0 ] || { printf 'idempotent re-run exited %d:\n%s\n' "$rc" "$output" >&2; return 1; }

    unit_active gotcha || { printf 'gotcha unit not active after an idempotent re-run\n' >&2; return 1; }
    cmp -s "$env_before" /etc/gotcha/gotcha.env \
        || { printf 'gotcha.env changed after an idempotent re-run\n' >&2; return 1; }

    bak_after=$(find /etc/nginx/sites-available -maxdepth 1 -name 'gotcha.bak-*' | wc -l)
    [ "$bak_after" -eq "$bak_before" ] \
        || { printf 'nginx config re-backed-up on an idempotent re-run (count %s -> %s)\n' "$bak_before" "$bak_after" >&2; return 1; }

    local tries=0
    until readyz_ok; do
        tries=$((tries + 1))
        [ "$tries" -lt 30 ] || { printf '/readyz did not recover after an idempotent re-run\n' >&2; return 1; }
        sleep 1
    done
}

# Действие и проверка вместе: --uninstall снимает только юнит и бинарь, данные,
# базы, пакеты и apt-репозитории остаются (§4.5/§4.6 — на хосте ими может пользоваться что-то ещё).
uninstall_removes_unit_and_binary_keeps_data() {
    bash "$INSTALLER" --uninstall
    local rc=$?
    [ "$rc" -eq 0 ] || { printf '--uninstall exited %d\n' "$rc" >&2; return 1; }

    [ ! -f /etc/systemd/system/gotcha.service ] || { printf 'gotcha.service still present after --uninstall\n' >&2; return 1; }
    [ ! -f /usr/local/bin/gotcha ] || { printf '/usr/local/bin/gotcha still present after --uninstall\n' >&2; return 1; }
    [ -d /var/lib/gotcha ] || { printf '/var/lib/gotcha missing after --uninstall\n' >&2; return 1; }
    pg_role_exists || { printf 'postgresql role gotcha missing after --uninstall\n' >&2; return 1; }
    pg_database_exists || { printf 'postgresql database gotcha missing after --uninstall\n' >&2; return 1; }
    ch_gotcha_database_exists || { printf 'clickhouse database gotcha missing after --uninstall\n' >&2; return 1; }
    pkg_installed "postgresql-$PG_MAJOR" || { printf 'postgresql package removed by --uninstall\n' >&2; return 1; }
    pkg_installed clickhouse-server || { printf 'clickhouse-server package removed by --uninstall\n' >&2; return 1; }
    pkg_installed nginx || { printf 'nginx package removed by --uninstall\n' >&2; return 1; }
    [ -f /etc/apt/sources.list.d/gotcha-pgdg.list ] || { printf 'PGDG apt repository removed by --uninstall\n' >&2; return 1; }
    [ -f /etc/apt/sources.list.d/gotcha-clickhouse.list ] || { printf 'ClickHouse apt repository removed by --uninstall\n' >&2; return 1; }
}

reinstall_after_uninstall_succeeds() {
    bash "$INSTALLER" --version "$tarball_version" --from-tarball "$WORK_TARBALL" --yes
    local rc=$?
    [ "$rc" -eq 0 ] || { printf 're-install after --uninstall exited %d\n' "$rc" >&2; return 1; }
    unit_active gotcha || { printf 'gotcha unit not active after re-install\n' >&2; return 1; }
    readyz_ok || { printf '/readyz not ok after re-install\n' >&2; return 1; }
}

# --purge дополнительно снимает наши базы/роли/данные/системного пользователя,
# но так же не трогает пакеты и apt-репозитории — только --uninstall уже проверил это.
purge_removes_data_and_databases_keeps_packages() {
    bash "$INSTALLER" --uninstall --purge
    local rc=$?
    [ "$rc" -eq 0 ] || { printf '--uninstall --purge exited %d\n' "$rc" >&2; return 1; }

    [ ! -d /var/lib/gotcha ] || { printf '/var/lib/gotcha still present after --purge\n' >&2; return 1; }
    [ ! -d /opt/gotcha ] || { printf '/opt/gotcha still present after --purge\n' >&2; return 1; }
    [ ! -e /etc/gotcha ] || { printf '/etc/gotcha still present after --purge\n' >&2; return 1; }
    ! pg_role_exists || { printf 'postgresql role gotcha still exists after --purge\n' >&2; return 1; }
    ! pg_database_exists || { printf 'postgresql database gotcha still exists after --purge\n' >&2; return 1; }
    ! ch_gotcha_database_exists || { printf 'clickhouse database gotcha still exists after --purge\n' >&2; return 1; }
    ! id -u gotcha >/dev/null 2>&1 || { printf 'system user gotcha still exists after --purge\n' >&2; return 1; }
    pkg_installed "postgresql-$PG_MAJOR" || { printf 'postgresql package removed by --purge\n' >&2; return 1; }
    pkg_installed clickhouse-server || { printf 'clickhouse-server package removed by --purge\n' >&2; return 1; }
    pkg_installed nginx || { printf 'nginx package removed by --purge\n' >&2; return 1; }
    [ -f /etc/apt/sources.list.d/gotcha-pgdg.list ] || { printf 'PGDG apt repository removed by --purge\n' >&2; return 1; }
    [ -f /etc/apt/sources.list.d/gotcha-clickhouse.list ] || { printf 'ClickHouse apt repository removed by --purge\n' >&2; return 1; }
}

run_assertions() {
    assert "install-bare-metal.sh exited 0" installer_succeeded

    if [ -n "$WORK_UPGRADE_TARBALL" ]; then
        assert "upgrade baseline install (older version) exited 0" baseline_install_succeeded
        assert "upgrade took a pre-migration pg_dump backup" upgrade_took_backup
        assert "upgrade kept the previous binary in /opt/gotcha/backup" upgrade_kept_previous_binary
    else
        printf 'note: --upgrade-from not given, skipping upgrade-path assertions\n'
    fi

    assert "postgresql package installed" pkg_installed "postgresql-$PG_MAJOR"
    assert "postgresql unit active" unit_active postgresql
    assert "clickhouse-server package installed" pkg_installed clickhouse-server
    assert "clickhouse-server unit active" unit_active clickhouse-server

    assert "port 5432 loopback-only" port_loopback_only 5432
    assert "port 8123 loopback-only" port_loopback_only 8123
    assert "port 9000 loopback-only" port_loopback_only 9000

    assert "postgresql role gotcha exists" pg_role_exists
    assert "postgresql database gotcha exists" pg_database_exists
    assert "postgresql conf.d/10-gotcha.conf present and tuned" pg_gotcha_conf_present

    assert "clickhouse config.d/00-common.xml present" ch_common_config_present
    assert "clickhouse-server LimitNOFILE=262144" ch_limit_nofile
    assert "clickhouse user gotcha configured" ch_gotcha_user_configured
    assert "clickhouse database gotcha exists" ch_gotcha_database_exists

    assert "gotcha /readyz responds 200" readyz_ok
    assert "state directory is 0700 and owned by gotcha" state_dir_secured
    assert "exports directory is 0700 and owned by gotcha" exports_dir_secured
    assert "gotcha.env is root:gotcha 640" env_file_secured
    assert "agent binary downloads and matches SHA256SUMS" agent_binary_download_matches_sums
    assert "gotcha unit hardening directives in effect" unit_hardening_directives
    assert "gotcha survives a postgresql restart" survives_postgresql_restart
    assert "register+onboarding+ingest round trip is visible" e2e_ingest_roundtrip

    assert "nginx package installed" pkg_installed nginx
    assert "nginx unit active" unit_active nginx
    assert "nginx site config proxies to gotcha with required headers" nginx_site_config_ok
    assert "pre-existing nginx site config was backed up, not clobbered" nginx_site_backed_up
    assert "gotcha /readyz responds 200 via nginx on :80" readyz_via_nginx

    assert "re-running the installer with the same version is idempotent (unit alive, env untouched, /readyz ok)" survives_idempotent_rerun

    assert "--uninstall removes the unit and binary, keeps data/databases/packages/repos" uninstall_removes_unit_and_binary_keeps_data
    assert "re-installing after --uninstall succeeds" reinstall_after_uninstall_succeeds
    assert "--purge removes data/databases/system user, keeps packages/repos" purge_removes_data_and_databases_keeps_packages
}

run_assertions

kill "$RSS_SAMPLER_PID" 2>/dev/null
printf 'bare-metal-e2e: peak RSS (postgresql+clickhouse+gotcha) during the run: %s KB\n' "$(cat "$RSS_PEAK_FILE" 2>/dev/null || echo 0)"

if [ "$FAILURES" -gt 0 ]; then
    printf '%d assertion(s) failed\n' "$FAILURES" >&2
    exit 1
fi
printf 'bare-metal-e2e: all assertions passed\n'
