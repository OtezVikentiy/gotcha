#!/usr/bin/env bash
# Прогоняет install-bare-metal.sh НА ЭТОЙ МАШИНЕ и проверяет реальное состояние
# системы после установки: пакеты, юниты, порты, конфиги. Не собирает тарбол —
# ночная матрица гоняет этот скрипт в debian:12/ubuntu:26.04, где нет Go.
set -uo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
INSTALLER="$SCRIPT_DIR/../internal/docs/install-bare-metal.sh"

TARBALL=""
FORCE=""

usage() {
    cat <<'EOF'
Usage: bare-metal-e2e.sh --tarball PATH [--i-know-this-wipes-the-host]

Installs PostgreSQL, ClickHouse and gotcha on THIS host via
install-bare-metal.sh and asserts the result against the real system.
Destructive: only run inside a disposable container or in CI.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --tarball)
            TARBALL="${2:-}"
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
[ -f "$INSTALLER" ] || { printf 'bare-metal-e2e: installer not found: %s\n' "$INSTALLER" >&2; exit 1; }

# Источник PG_MAJOR/CH_VERSION для ассертов ниже — та же переменная, что
# ставит install-bare-metal.sh, а не отдельно вписанное число.
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

# ss формат Local Address:Port — "127.0.0.1:5432" или "[::1]:5432". Публикация
# наружу (0.0.0.0/*, реальный IP) — регресс относительно compose, где эти три
# порта вообще не публикуются.
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

# fetch_tarball требует SHA256SUMS.txt рядом с тарболом; release.sh пока не
# публикует такой файл (появится с релизным воркфлоу), а исходный каталог с
# тарболом нередко смонтирован read-only. Харнесс копирует тарбол в свой
# рабочий каталог и считает сумму сам, как это будет делать релиз.
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT
cp "$TARBALL" "$WORK_DIR/"
WORK_TARBALL="$WORK_DIR/$(basename "$TARBALL")"
(cd "$WORK_DIR" && sha256sum "$(basename "$WORK_TARBALL")") >"$WORK_DIR/SHA256SUMS.txt"

# fetch_tarball сверяет ARG_VERSION с VERSION-файлом внутри тарбола — версия
# здесь берётся из имени файла (тот же формат, что печатает dist_url).
tarball_version=$(basename "$WORK_TARBALL" | sed -E 's/^gotcha-(.+)-linux-[^-]+\.tar\.gz$/\1/')
if [ -z "$tarball_version" ] || [ "$tarball_version" = "$(basename "$WORK_TARBALL")" ]; then
    printf 'bare-metal-e2e: cannot parse version out of tarball name: %s\n' "$WORK_TARBALL" >&2
    exit 2
fi

printf 'bare-metal-e2e: running install-bare-metal.sh --version %s --from-tarball %s --yes --no-proxy\n' \
    "$tarball_version" "$WORK_TARBALL"
# Не implemented-до-конца шаги (приложение, nginx) сегодня доводят main() до
# намеренного отказа после установки баз — это ожидаемо до задач 5-7, поэтому
# код возврата здесь не проверяется, только реальное состояние системы ниже.
bash "$INSTALLER" --version "$tarball_version" --from-tarball "$WORK_TARBALL" --yes --no-proxy
installer_rc=$?
printf 'bare-metal-e2e: install-bare-metal.sh exited %d\n' "$installer_rc"

run_assertions() {
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

    # Задачи 5-7 добавляют сюда: приложение (юнит gotcha, /readyz), nginx
    # (сайт, certbot), снятие установки (--uninstall, --purge).
}

run_assertions

if [ "$FAILURES" -gt 0 ]; then
    printf '%d assertion(s) failed\n' "$FAILURES" >&2
    exit 1
fi
printf 'bare-metal-e2e: all assertions passed\n'
