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
# Сорсинг сам по себе даёт только константы файлового уровня — без явного
# вызова этот скрипт сверял бы своё же семейство с самим собой.
detect_platform

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
    if [ "$HOST_FAMILY" = rhel ]; then
        rpm -q "$1" >/dev/null 2>&1
        return $?
    fi
    dpkg -s "$1" >/dev/null 2>&1
}

unit_active() {
    systemctl is-active --quiet "$1"
}

pgdg_repo_file_present() {
    if [ "$HOST_FAMILY" = rhel ]; then
        [ -f "$REPO_DIR/gotcha-pgdg.repo" ]
        return $?
    fi
    [ -f "$REPO_DIR/gotcha-pgdg.list" ]
}

clickhouse_repo_file_present() {
    if [ "$HOST_FAMILY" = rhel ]; then
        [ -f "$REPO_DIR/gotcha-clickhouse.repo" ]
        return $?
    fi
    [ -f "$REPO_DIR/gotcha-clickhouse.list" ]
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

pg_gotcha_conf_present() {
    local dir conf
    dir=$(pg_conf_dir_resolve)
    conf="$dir/conf.d/10-gotcha.conf"
    [ -f "$conf" ] || { printf 'missing %s\n' "$conf" >&2; return 1; }
    grep -qE '^random_page_cost = 1\.1$' "$conf" || { printf 'random_page_cost missing in %s\n' "$conf" >&2; return 1; }
    grep -qE '^effective_io_concurrency = 200$' "$conf" || { printf 'effective_io_concurrency missing in %s\n' "$conf" >&2; return 1; }
}

# На EL postgresql.conf.sample несёт include_dir закомментированной, и
# ensure_include_dir дописывает строку: два подряд запуска не должны продублировать её.
pg_include_dir_set_once() {
    local conf_dir
    conf_dir=$(pg_conf_dir_resolve)
    [ "$(grep -c "^include_dir = 'conf.d'" "$conf_dir/postgresql.conf" 2>/dev/null)" = "1" ] \
        || { printf "include_dir = 'conf.d' does not appear exactly once in %s/postgresql.conf\n" "$conf_dir" >&2; return 1; }
}

# install_postgresql перенаправляет болтливость dnf/apt в /dev/null именно затем,
# чтобы она не подмешалась в DSN, возвращаемый через stdout.
gotcha_env_pg_dsn_clean() {
    local dsn
    dsn=$(grep '^GOTCHA_PG_DSN=' /etc/gotcha/gotcha.env | cut -d= -f2-)
    case "$dsn" in
        postgres://*) return 0 ;;
    esac
    printf 'GOTCHA_PG_DSN in gotcha.env does not start with postgres://: %s\n' "$dsn" >&2
    return 1
}

pg_role_exists() {
    [ "$(sudo -u postgres "$PG_BIN_DIR/psql" -tAc "SELECT 1 FROM pg_roles WHERE rolname = 'gotcha'" 2>/dev/null)" = "1" ]
}

pg_database_exists() {
    [ "$(sudo -u postgres "$PG_BIN_DIR/psql" -tAc "SELECT 1 FROM pg_database WHERE datname = 'gotcha'" 2>/dev/null)" = "1" ]
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

epel_absent_without_domain() {
    [ "$HOST_FAMILY" = rhel ] || return 0
    ! dnf -q repolist enabled 2>/dev/null | grep -qi '^epel' \
        || { printf 'EPEL enabled although --domain was not given\n' >&2; return 1; }
}

# .invalid: certbot гарантированно не выпустит сертификат — тот же прогон
# проверяет, что этот отказ не откатывает установку и не роняет юнит gotcha.
epel_enabled_with_domain_run() {
    local output rc
    output=$(bash "$INSTALLER" --version "$tarball_version" --from-tarball "$WORK_TARBALL" \
        --domain gotcha-e2e.invalid --email admin@gotcha-e2e.invalid --yes 2>&1)
    rc=$?
    [ "$rc" -eq 0 ] || { printf 'install with --domain exited %d:\n%s\n' "$rc" "$output" >&2; return 1; }

    dnf -q repolist enabled 2>/dev/null | grep -qi '^epel' \
        || { printf 'EPEL not enabled although --domain was given\n' >&2; return 1; }
    command -v certbot >/dev/null 2>&1 \
        || { printf 'certbot not installed although --domain was given\n' >&2; return 1; }
    unit_active gotcha \
        || { printf 'gotcha unit is down after a certbot failure — the install must not roll back\n' >&2; return 1; }

    output=$(bash "$INSTALLER" --version "$tarball_version" --from-tarball "$WORK_TARBALL" --yes 2>&1)
    rc=$?
    [ "$rc" -eq 0 ] || { printf 'reverting the --domain install exited %d:\n%s\n' "$rc" "$output" >&2; return 1; }
    readyz_via_nginx \
        || { printf 'readyz via nginx did not recover after reverting the --domain install\n' >&2; return 1; }
}

# Restores $NGINX_SITE on every exit of the check above, success or not — an early
# failure must not also leave the domain detour's TLS-wiping render for later asserts.
epel_enabled_with_domain() {
    [ "$HOST_FAMILY" = rhel ] || return 0
    local site_before rc baks_before baks_new
    site_before=$(cat "$NGINX_SITE" 2>/dev/null)
    baks_before=$(find "$(dirname "$NGINX_SITE")" -maxdepth 1 -name 'gotcha*.bak-*' | sort)

    epel_enabled_with_domain_run
    rc=$?

    if [ -n "$site_before" ]; then
        if ! printf '%s\n' "$site_before" >"$NGINX_SITE" || ! nginx -t >/dev/null 2>&1 || ! systemctl reload nginx; then
            printf 'failed to restore %s to its pre-detour content\n' "$NGINX_SITE" >&2
            [ "$rc" -eq 0 ] && rc=1
        fi
    fi
    # The domain detour's own re-renders back up $NGINX_SITE twice; those backups are
    # noise certbot_edits_survive_a_rerun never asked for, so remove exactly the new ones.
    baks_new=$(comm -13 <(printf '%s\n' "$baks_before") \
        <(find "$(dirname "$NGINX_SITE")" -maxdepth 1 -name 'gotcha*.bak-*' | sort))
    [ -z "$baks_new" ] || xargs -r rm -f <<<"$baks_new"
    return "$rc"
}

firewall_untouched_with_flag() {
    [ "$HOST_FAMILY" = rhel ] || return 0
    local stub=/tmp/fwstub log=/tmp/fw-calls.log
    mkdir -p "$stub"; : >"$log"
    # shellcheck disable=SC2016 # literal $@ for the stub script, not expanded here
    printf '#!/bin/sh\necho "$@" >>%s\n[ "$1" = --state ] && { echo running; exit 0; }\nexit 0\n' "$log" >"$stub/firewall-cmd"
    chmod +x "$stub/firewall-cmd"
    PATH="$stub:$PATH" bash "$INSTALLER" --version "$tarball_version" \
        --from-tarball "$WORK_TARBALL" --yes --no-firewall >/dev/null 2>&1
    ! grep -q -- '--add-service' "$log" \
        || { printf 'firewall-cmd --add-service called although --no-firewall was given\n' >&2; return 1; }
}

firewall_opened_without_flag() {
    [ "$HOST_FAMILY" = rhel ] || return 0
    local stub=/tmp/fwstub log=/tmp/fw-calls-positive.log
    mkdir -p "$stub"; : >"$log"
    # shellcheck disable=SC2016 # literal $@ for the stub script, not expanded here
    printf '#!/bin/sh\necho "$@" >>%s\n[ "$1" = --state ] && { echo running; exit 0; }\nexit 0\n' "$log" >"$stub/firewall-cmd"
    chmod +x "$stub/firewall-cmd"
    PATH="$stub:$PATH" bash "$INSTALLER" --version "$tarball_version" \
        --from-tarball "$WORK_TARBALL" --yes >/dev/null 2>&1
    grep -q -- '--add-service=http' "$log" \
        || { printf 'firewall-cmd --add-service=http not called although firewalld reports running\n' >&2; return 1; }
    grep -q -- '--reload' "$log" \
        || { printf 'firewall-cmd --reload not called, the rules would not apply until reboot\n' >&2; return 1; }
}

# Enforcing только под ручным прогоном на VM (задача 10) — здесь фиксируется пропуск,
# а не тишина, если SELinux в этом контейнере не enforcing.
selinux_boolean_set_when_enforcing() {
    [ "$HOST_FAMILY" = rhel ] || return 0
    local state=""
    command -v getenforce >/dev/null 2>&1 && state=$(getenforce 2>/dev/null)
    if [ "$state" != Enforcing ]; then
        printf 'note: SELinux is not enforcing here, the boolean branch was not exercised\n'
        return 0
    fi
    getsebool httpd_can_network_connect 2>/dev/null | grep -q -- '--> on$' \
        || { printf 'httpd_can_network_connect is not on although SELinux is Enforcing\n' >&2; return 1; }
}

readyz_via_nginx() {
    [ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:80/readyz)" = "200" ]
}

# 127.0.0.1 is on nginx's own allowlist, so curling it can't tell an open path
# from a blocked one; the host's own routable IP is treated as external.
external_ip() {
    hostname -I 2>/dev/null | awk '{print $1}'
}

metrics_blocked_via_nginx() {
    [ "$(curl -s -o /dev/null -w '%{http_code}' "http://$(external_ip):80/metrics")" = "403" ]
}

version_blocked_via_nginx() {
    [ "$(curl -s -o /dev/null -w '%{http_code}' "http://$(external_ip):80/version")" = "403" ]
}

# Запрос по IP попадает в gotcha, а не на приветственную страницу nginx — на EL
# это следствие порядка include в штатном nginx.conf, а не нашей правки.
ip_request_reaches_gotcha() {
    local body
    body=$(curl -fsS "http://$(external_ip)/readyz") || return 1
    case "$body" in *ok*) return 0 ;; esac
    printf 'GET / by IP did not reach gotcha: %s\n' "$body" >&2
    return 1
}

nginx_site_config_ok() {
    local conf="$NGINX_SITE"
    [ -f "$conf" ] || { printf 'missing %s\n' "$conf" >&2; return 1; }
    grep -qF 'proxy_pass http://127.0.0.1:8080' "$conf" || { printf 'proxy_pass missing in %s\n' "$conf" >&2; return 1; }
    grep -qF 'proxy_set_header Host' "$conf" || { printf 'Host forwarding missing in %s\n' "$conf" >&2; return 1; }
    grep -qF 'X-Forwarded-For' "$conf" || { printf 'X-Forwarded-For missing in %s\n' "$conf" >&2; return 1; }
    grep -qF 'X-Forwarded-Proto' "$conf" || { printf 'X-Forwarded-Proto missing in %s\n' "$conf" >&2; return 1; }
    grep -qF 'client_max_body_size' "$conf" || { printf 'client_max_body_size missing in %s\n' "$conf" >&2; return 1; }
    for directive in 'location ~ ^/(metrics|version)$ {' 'allow 127.0.0.1;' 'allow ::1;' 'deny all;'; do
        grep -qF "$directive" "$conf" || { printf '%s missing in %s\n' "$directive" "$conf" >&2; return 1; }
    done
}

nginx_site_backed_up() {
    local dir backup
    dir=$(dirname "$NGINX_SITE")
    backup=$(find "$dir" -maxdepth 1 -name 'gotcha*.bak-*' -print -quit 2>/dev/null)
    [ -n "$backup" ] || { printf 'no gotcha*.bak-* found in %s\n' "$dir" >&2; return 1; }
    grep -qF 'pre-existing site placed by someone else' "$backup" \
        || { printf 'backup does not preserve the original foreign content: %s\n' "$backup" >&2; return 1; }
}

# Порт 80 проверяется в preflight до любых побочных эффектов (код 3, как и
# весь preflight), поэтому installer можно звать реальными флагами безопасно.
port80_busy_blocks_preflight() {
    command -v python3 >/dev/null 2>&1 || {
        pkg_refresh
        pkg_install python3
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
    until grep -q ':80$' <<<"$(ss -ltn 2>/dev/null | awk '{print $4}')"; do
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
    grep -q 'port 80 is already in use' <<<"$output" \
        || { printf 'missing "port 80 is already in use" in output:\n%s\n' "$output" >&2; return 1; }
}

# fetch_tarball требует SHA256SUMS.txt рядом с тарболом; release.sh её пока не
# публикует, а каталог тарбола часто read-only — считаем сумму в своей копии.
# /var/tmp, не /tmp: на свежезагруженном systemd-контейнере правила tmpfiles
# могут пересоздать /tmp уже после того, как мы сюда что-то положили.
WORK_DIR=$(mktemp -d -p /var/tmp)
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

# §4.6: отказ на середине обязан напечатать код выхода, шаги и подсказку.
# --skip-databases с нерабочими DSN бьёт по run_migrations, не трогая настоящие СУБД.
policy_failure_reports_steps_and_hint() {
    rm -f /etc/gotcha/gotcha.env
    local output rc
    output=$(bash "$INSTALLER" --version "$tarball_version" --from-tarball "$WORK_TARBALL" \
        --skip-databases --pg-dsn 'postgres://nobody:nobody@127.0.0.1:1/nope?sslmode=disable' \
        --ch-dsn 'clickhouse://nobody:nobody@127.0.0.1:2/nope' --yes 2>&1)
    rc=$?
    rm -f /etc/gotcha/gotcha.env

    [ "$rc" -eq 6 ] || { printf 'expected exit 6 (EXIT_APP) from a broken DB step, got %d:\n%s\n' "$rc" "$output" >&2; return 1; }
    grep -q 'FAILED (exit 6)' <<<"$output" \
        || { printf 'missing "FAILED (exit 6)" in output:\n%s\n' "$output" >&2; return 1; }
    grep -q 'completed steps so far' <<<"$output" \
        || { printf 'missing the completed-steps list in output:\n%s\n' "$output" >&2; return 1; }
    grep -q -- '--uninstall' <<<"$output" \
        || { printf 'missing the --uninstall hint in output:\n%s\n' "$output" >&2; return 1; }
}

# Прячется сам файл, а не каталог в PATH: `command -v` находит команду по любому
# каталогу PATH, и вычёркивание одного из них увело бы отказ на соседнюю команду.
preflight_requires_command() {
    local cmd="$1" path hidden output rc
    path=$(command -v "$cmd") || { printf '%s is not installed here, cannot check\n' "$cmd" >&2; return 1; }
    hidden="$path.hidden-by-e2e"
    mv "$path" "$hidden" || { printf 'failed to hide %s\n' "$path" >&2; return 1; }

    output=$(bash "$INSTALLER" --version "$tarball_version" --from-tarball "$WORK_TARBALL" --yes 2>&1)
    rc=$?
    mv "$hidden" "$path" || { printf 'FAILED TO RESTORE %s — the host is now missing %s\n' "$hidden" "$cmd" >&2; return 1; }

    [ "$rc" -eq 3 ] || { printf 'expected exit 3 without %s, got %d:\n%s\n' "$cmd" "$rc" "$output" >&2; return 1; }
    grep -q "$cmd is required" <<<"$output" \
        || { printf 'missing "%s is required" in output:\n%s\n' "$cmd" "$output" >&2; return 1; }
}

# --dry-run обязан печатать пути из платформенных констант, а не debian-литерал —
# отдельная проверка без побочных эффектов, до самого запуска установки.
dry_run_prints_platform_paths() {
    local out
    out=$(bash "$INSTALLER" --version "$tarball_version" --from-tarball "$WORK_TARBALL" \
        --yes --dry-run 2>&1)
    case "$out" in
        *"$(pg_conf_dir_label)/conf.d/10-gotcha.conf"*) ;;
        *) printf 'dry-run did not print %s/conf.d/10-gotcha.conf:\n%s\n' "$(pg_conf_dir_label)" "$out" >&2; return 1 ;;
    esac
    case "$out" in
        *"$NGINX_SITE"*) ;;
        *) printf 'dry-run did not name the nginx site path (%s):\n%s\n' "$NGINX_SITE" "$out" >&2; return 1 ;;
    esac
}

assert "a busy port 80 blocks preflight before anything is installed" port80_busy_blocks_preflight
assert "preflight refuses without ss, which the port check needs" preflight_requires_command ss
assert "preflight refuses without sudo, which the database steps need" preflight_requires_command sudo
if [ "$HOST_FAMILY" = rhel ]; then
    assert "preflight refuses without rpm, which package queries need" preflight_requires_command rpm
    assert "preflight refuses without dnf, which package installs need" preflight_requires_command dnf
fi
assert "a mid-install failure reports the exit code, completed steps and a hint" policy_failure_reports_steps_and_hint
assert "--dry-run prints paths from the platform layer, not a debian literal" dry_run_prints_platform_paths

# Симулирует чужой конфиг сайта, уже лежащий на месте нашего: install_nginx
# обязан унести его в *.bak-<метка времени>, а не переписать без следа.
mkdir -p "$(dirname "$NGINX_SITE")"
printf '# pre-existing site placed by someone else\n' >"$NGINX_SITE"

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
        grep -qx "$directive" <<<"$out" \
            || { printf 'missing %s in:\n%s\n' "$directive" "$out" >&2; return 1; }
    done
    grep -qE '^MemoryMax=[1-9][0-9]*$' <<<"$out" \
        || { printf 'MemoryMax is not a positive byte count:\n%s\n' "$out" >&2; return 1; }
}

survives_postgresql_restart() {
    systemctl restart "$PG_UNIT" || { printf 'systemctl restart %s failed\n' "$PG_UNIT" >&2; return 1; }
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

    # Сразу после survives_postgresql_restart в пуле может остаться стухшее соединение.
    # Успех — 303 (redirectLocal); повтор только на 500, другой код падает сразу.
    tries=0
    local code
    while :; do
        code=$(curl -s -o /dev/null -w '%{http_code}' -c "$jar" -b "$jar" -H "Origin: $origin" \
            --data-urlencode "email=e2e-$$-$tries@example.invalid" \
            --data-urlencode "password=Str0ng-Passw0rd" \
            --data-urlencode "password2=Str0ng-Passw0rd" \
            "$app/register")
        [ "$code" = 303 ] && break
        [ "$code" = 500 ] || { printf 'POST /register failed with HTTP %s\n' "$code" >&2; return 1; }
        tries=$((tries + 1))
        [ "$tries" -lt 10 ] || { printf 'POST /register kept returning 500 after %d tries\n' "$tries" >&2; return 1; }
        sleep 1
    done

    curl -fsS -c "$jar" -b "$jar" -o /dev/null -H "Origin: $origin" \
        --data-urlencode "org_slug=e2e-org" \
        --data-urlencode "org_name=e2e org" \
        --data-urlencode "project_slug=e2e-project" \
        --data-urlencode "project_name=e2e project" \
        --data-urlencode "platform=go" \
        "$app/onboarding" || { printf 'POST /onboarding failed\n' >&2; return 1; }

    key=$(sudo -u postgres "$PG_BIN_DIR/psql" -d gotcha -tAc "SELECT public_key FROM project_keys ORDER BY id LIMIT 1")
    project_id=$(sudo -u postgres "$PG_BIN_DIR/psql" -d gotcha -tAc "SELECT project_id FROM project_keys ORDER BY id LIMIT 1")
    if [ -z "$key" ] || [ -z "$project_id" ]; then
        printf 'could not read a project key out of PostgreSQL\n' >&2
        return 1
    fi

    curl -fsS -o /dev/null "$app/api/$project_id/store/?sentry_key=$key" \
        --data "{\"message\":\"$msg\"}" || { printf 'event submission to /api/%s/store/ failed\n' "$project_id" >&2; return 1; }

    tries=0
    until grep -qF "$msg" <<<"$(curl -fsS -c "$jar" -b "$jar" "$app/projects/$project_id/issues")"; do
        tries=$((tries + 1))
        [ "$tries" -lt 15 ] || { printf 'event never appeared in the issues list\n' >&2; return 1; }
        sleep 1
    done
}

# Пароль ClickHouse живёт только в DSN внутри gotcha.env — в users.d лежит его sha256.
# Проверяется ровно то извлечение, которое печатает backup-restore.md.
ch_password_from_env_file_works() {
    local password
    password=$(sed -n 's#^GOTCHA_CH_DSN=clickhouse://gotcha:\([^@]*\)@.*#\1#p' /etc/gotcha/gotcha.env)
    [ -n "$password" ] || { printf 'no password inside GOTCHA_CH_DSN in gotcha.env\n' >&2; return 1; }
    [ "$(clickhouse-client --user gotcha --password "$password" --query 'SELECT 1' 2>&1)" = "1" ] \
        || { printf 'the password taken from gotcha.env does not authenticate against ClickHouse\n' >&2; return 1; }
}

# ALTER USER для gotcha невозможен (пользователь описан в XML с access_management=0),
# поэтому configuration.md описывает правку password_sha256_hex — она и проверяется.
documented_ch_password_change_works() {
    local users_file=/etc/clickhouse-server/users.d/10-gotcha.xml new_password hash tries
    new_password="e2e-$(openssl rand -hex 12)"
    hash=$(printf '%s' "$new_password" | sha256sum | awk '{print $1}')

    sed -i "s#<password_sha256_hex>[0-9a-f]*</password_sha256_hex>#<password_sha256_hex>$hash</password_sha256_hex>#" \
        "$users_file" || { printf 'failed to rewrite %s\n' "$users_file" >&2; return 1; }
    grep -qF "$hash" "$users_file" || { printf 'the new hash did not land in %s\n' "$users_file" >&2; return 1; }
    systemctl restart clickhouse-server || { printf 'clickhouse-server restart failed\n' >&2; return 1; }

    tries=0
    until curl -fsS -o /dev/null http://127.0.0.1:8123/ping 2>/dev/null; do
        tries=$((tries + 1))
        [ "$tries" -lt 30 ] || { printf 'clickhouse-server did not come back after the restart\n' >&2; return 1; }
        sleep 1
    done
    [ "$(clickhouse-client --user gotcha --password "$new_password" --query 'SELECT 1' 2>&1)" = "1" ] \
        || { printf 'the new password does not authenticate after editing users.d and restarting\n' >&2; return 1; }

    sed -i "s#^GOTCHA_CH_DSN=clickhouse://gotcha:[^@]*@#GOTCHA_CH_DSN=clickhouse://gotcha:$new_password@#" \
        /etc/gotcha/gotcha.env || { printf 'failed to update GOTCHA_CH_DSN\n' >&2; return 1; }
    systemctl restart gotcha || { printf 'gotcha restart failed\n' >&2; return 1; }

    tries=0
    until readyz_ok; do
        tries=$((tries + 1))
        [ "$tries" -lt 30 ] || { printf '/readyz did not recover after the documented password change\n' >&2; return 1; }
        sleep 1
    done
}

# certbot правит ровно тот файл, который рендерит install_nginx. В контейнере его не
# выпустить, поэтому результат имитируется своим сертификатом.
certbot_edits_survive_a_rerun() {
    local site="$NGINX_SITE" crt=/etc/ssl/gotcha-e2e.crt key=/etc/ssl/gotcha-e2e.key
    local bak_before bak_after output rc
    openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=localhost' \
        -keyout "$key" -out "$crt" >/dev/null 2>&1 \
        || { printf 'failed to generate a self-signed certificate\n' >&2; return 1; }

    sed -i "\$i\\    listen 443 ssl;" "$site"
    sed -i "\$i\\    ssl_certificate $crt;" "$site"
    sed -i "\$i\\    ssl_certificate_key $key;" "$site"
    nginx -t >/dev/null 2>&1 || { printf 'the planted TLS block does not pass nginx -t\n' >&2; return 1; }
    systemctl reload nginx || { printf 'nginx reload failed after planting the TLS block\n' >&2; return 1; }

    bak_before=$(find "$(dirname "$NGINX_SITE")" -maxdepth 1 -name 'gotcha*.bak-*' | wc -l)
    output=$(bash "$INSTALLER" --version "$tarball_version" --from-tarball "$WORK_TARBALL" --yes 2>&1)
    rc=$?
    [ "$rc" -eq 0 ] || { printf 'a re-run over a certbot-edited site exited %d:\n%s\n' "$rc" "$output" >&2; return 1; }

    grep -qF 'listen 443 ssl;' "$site" \
        || { printf 'the TLS block is gone from %s after a re-run — the host fell back to plain HTTP\n' "$site" >&2; return 1; }
    bak_after=$(find "$(dirname "$NGINX_SITE")" -maxdepth 1 -name 'gotcha*.bak-*' | wc -l)
    [ "$bak_after" -eq "$bak_before" ] \
        || { printf 'the site was backed up and re-rendered on a re-run (count %s -> %s)\n' "$bak_before" "$bak_after" >&2; return 1; }
    [ "$(curl -sk -o /dev/null -w '%{http_code}' https://127.0.0.1:443/readyz)" = "200" ] \
        || { printf 'HTTPS stopped answering after the re-run\n' >&2; return 1; }
}

# Шаги СУБД возвращают DSN через $( ), их log_step наполняет INSTALL_LOG подоболочки.
# Ломается install_unit уже после обеих СУБД: отчёт обязан назвать их шаги.
failure_report_lists_database_steps() {
    local unit=/etc/systemd/system/gotcha.service output rc
    rm -f "$unit"
    mkdir -p "$unit" || { printf 'failed to plant a directory at %s\n' "$unit" >&2; return 1; }

    output=$(bash "$INSTALLER" --version "$tarball_version" --from-tarball "$WORK_TARBALL" --yes 2>&1)
    rc=$?
    rmdir "$unit" || { printf 'FAILED TO REMOVE the planted directory %s\n' "$unit" >&2; return 1; }

    [ "$rc" -ne 0 ] || { printf 'expected a non-zero exit with the unit path blocked:\n%s\n' "$output" >&2; return 1; }

    # Только отчёт: те же шаги log_step печатает и по ходу установки, и ассерт по
    # всему выводу был бы зелёным даже с потерянным списком.
    local report
    report=$(sed -n '/^install-bare-metal: FAILED (exit /,$p' <<<"$output")
    [ -n "$report" ] || { printf 'no failure report in output:\n%s\n' "$output" >&2; return 1; }
    grep -q 'completed steps so far' <<<"$report" \
        || { printf 'missing the completed-steps list:\n%s\n' "$report" >&2; return 1; }
    grep -qx "  - PostgreSQL $PG_MAJOR installed and configured" <<<"$report" \
        || { printf 'the PostgreSQL step is missing from the failure report:\n%s\n' "$report" >&2; return 1; }
    grep -qx "  - ClickHouse $CH_VERSION installed and configured" <<<"$report" \
        || { printf 'the ClickHouse step is missing from the failure report:\n%s\n' "$report" >&2; return 1; }
}

# Действие и проверка вместе, как survives_postgresql_restart выше: §4.4 требует
# юнит/конфиги/права привести к целевому состоянию заново, но не трогать env/пароли/данные.
survives_idempotent_rerun() {
    local env_before="$WORK_DIR/gotcha.env.before-rerun" bak_before bak_after output rc
    cp /etc/gotcha/gotcha.env "$env_before" || { printf 'failed to snapshot gotcha.env\n' >&2; return 1; }
    bak_before=$(find "$(dirname "$NGINX_SITE")" -maxdepth 1 -name 'gotcha*.bak-*' | wc -l)

    output=$(bash "$INSTALLER" --version "$tarball_version" --from-tarball "$WORK_TARBALL" --yes 2>&1)
    rc=$?
    [ "$rc" -eq 0 ] || { printf 'idempotent re-run exited %d:\n%s\n' "$rc" "$output" >&2; return 1; }

    unit_active gotcha || { printf 'gotcha unit not active after an idempotent re-run\n' >&2; return 1; }
    # sha256sum, не cmp/diff: diffutils не входит в required_commands и не
    # гарантирован на минимальном EL-хосте, sha256sum — гарантирован преflight'ом.
    [ "$(sha256sum <"$env_before")" = "$(sha256sum </etc/gotcha/gotcha.env)" ] \
        || { printf 'gotcha.env changed after an idempotent re-run\n' >&2; return 1; }

    bak_after=$(find "$(dirname "$NGINX_SITE")" -maxdepth 1 -name 'gotcha*.bak-*' | wc -l)
    [ "$bak_after" -eq "$bak_before" ] \
        || { printf 'nginx config re-backed-up on an idempotent re-run (count %s -> %s)\n' "$bak_before" "$bak_after" >&2; return 1; }

    local tries=0
    until readyz_ok; do
        tries=$((tries + 1))
        [ "$tries" -lt 30 ] || { printf '/readyz did not recover after an idempotent re-run\n' >&2; return 1; }
        sleep 1
    done
}

# Роль/пользователь — наши; потерянный gotcha.env не повод падать на аутентификации,
# install_postgresql/install_clickhouse обязаны сами перевыпустить пароль.
recovers_after_env_file_lost() {
    rm -f /etc/gotcha/gotcha.env

    local output rc
    output=$(bash "$INSTALLER" --version "$tarball_version" --from-tarball "$WORK_TARBALL" --yes 2>&1)
    rc=$?
    [ "$rc" -eq 0 ] || { printf 'recovering from a lost env file exited %d:\n%s\n' "$rc" "$output" >&2; return 1; }
    grep -qi 'regenerating' <<<"$output" \
        || { printf 'missing a loud password-regeneration notice in output:\n%s\n' "$output" >&2; return 1; }

    unit_active gotcha || { printf 'gotcha unit not active after env recovery\n' >&2; return 1; }
    local tries=0
    until readyz_ok; do
        tries=$((tries + 1))
        [ "$tries" -lt 30 ] || { printf '/readyz did not recover after env recovery\n' >&2; return 1; }
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

    # Оставленный включённым сайт — 502 на всё. На EL это переименование в .disabled,
    # на Debian — снятие симлинка (файл в sites-available остаётся под своим именем).
    if [ "$HOST_FAMILY" = rhel ]; then
        [ ! -e "$NGINX_SITE" ] \
            || { printf 'the nginx site is still enabled after --uninstall\n' >&2; return 1; }
        [ -f "$(nginx_site_disabled_path)" ] \
            || { printf '%s missing after --uninstall (the certbot TLS block lives there)\n' "$(nginx_site_disabled_path)" >&2; return 1; }
    else
        [ ! -e /etc/nginx/sites-enabled/gotcha ] \
            || { printf 'the nginx site is still enabled after --uninstall\n' >&2; return 1; }
        [ -f "$NGINX_SITE" ] \
            || { printf '%s removed by --uninstall (the certbot TLS block lives there)\n' "$NGINX_SITE" >&2; return 1; }
    fi
    # Перезагрузка nginx асинхронна: старые воркеры доживают свои соединения, и первые
    # доли секунды хост ещё отвечает 502 — ждём, а не меряем один раз.
    local tries=0 code
    until code=$(curl -s -o /dev/null -w '%{http_code}' "http://$(external_ip):80/readyz"); [ "$code" != "502" ]; do
        tries=$((tries + 1))
        [ "$tries" -lt 15 ] || { printf 'nginx still answers 502 %s s after --uninstall\n' "$tries" >&2; return 1; }
        sleep 1
    done

    pg_role_exists || { printf 'postgresql role gotcha missing after --uninstall\n' >&2; return 1; }
    pg_database_exists || { printf 'postgresql database gotcha missing after --uninstall\n' >&2; return 1; }
    ch_gotcha_database_exists || { printf 'clickhouse database gotcha missing after --uninstall\n' >&2; return 1; }
    pkg_installed "$PG_PACKAGE" || { printf 'postgresql package removed by --uninstall\n' >&2; return 1; }
    pkg_installed clickhouse-server || { printf 'clickhouse-server package removed by --uninstall\n' >&2; return 1; }
    pkg_installed nginx || { printf 'nginx package removed by --uninstall\n' >&2; return 1; }
    pgdg_repo_file_present || { printf 'PGDG repository removed by --uninstall\n' >&2; return 1; }
    clickhouse_repo_file_present || { printf 'ClickHouse repository removed by --uninstall\n' >&2; return 1; }
}

# После --uninstall gotcha больше нет за проксёй — запрос по IP обязан попасть
# на штатную страницу nginx, а не зависнуть в 502 (стек всё ещё висит на порту 80).
ip_request_falls_back_to_default_nginx_page() {
    local code
    code=$(curl -s -o /dev/null -w '%{http_code}' "http://$(external_ip)/")
    [ "$code" != 502 ] || { printf 'nginx answers 502 after --uninstall\n' >&2; return 1; }
}

certbot_site_survives_uninstall_reinstall() {
    grep -qF 'listen 443 ssl;' "$NGINX_SITE" \
        || { printf 'TLS block missing from %s after --uninstall + reinstall\n' "$NGINX_SITE" >&2; return 1; }
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
purge_without_confirmation_refuses() {
    local output rc
    output=$(bash "$INSTALLER" --uninstall --purge </dev/null 2>&1)
    rc=$?
    [ "$rc" -eq "$EXIT_USAGE" ] \
        || { printf 'expected exit %d refusing an unconfirmed --purge, got %d:\n%s\n' "$EXIT_USAGE" "$rc" "$output" >&2; return 1; }
    pg_role_exists || { printf 'postgresql role gotcha gone after a refused --purge\n' >&2; return 1; }
}

# main() вызывается из-под if в конце скрипта — set -e внутри него инертен,
# и без явного warning ниже --purge рапортовал бы успех, оставив маркер в конфиге.
purge_marker_removal_failure_is_reported_not_silent() {
    [ "$HOST_FAMILY" = rhel ] || return 0
    local stub=/tmp/gotcha-awk-fail-stub output rc
    mkdir -p "$stub"
    printf '#!/bin/sh\nexit 1\n' >"$stub/awk"
    chmod +x "$stub/awk"
    output=$(PATH="$stub:$PATH" bash "$INSTALLER" --uninstall --purge --yes 2>&1)
    rc=$?
    rm -rf "$stub"

    [ "$rc" -eq 0 ] \
        || { printf '--purge with a failing marker removal exited %d, expected 0 (non-fatal continuation):\n%s\n' "$rc" "$output" >&2; return 1; }
    grep -q 'could not remove the gotcha include_dir marker' <<<"$output" \
        || { printf 'missing the marker-removal-failure warning in output:\n%s\n' "$output" >&2; return 1; }
}

purge_removes_data_and_databases_keeps_packages() {
    # До снятия: --purge правит postgresql.conf/pg_hba.conf на месте — владелец,
    # права и SELinux-контекст файла СУБД обязаны пережить это нетронутыми.
    local pg_dir_before pg_conf_attrs_before pg_hba_attrs_before
    if [ "$HOST_FAMILY" = rhel ]; then
        pg_dir_before=$(pg_conf_dir_resolve)
        pg_conf_attrs_before=$(stat -c '%U:%G %a %C' "$pg_dir_before/postgresql.conf")
        pg_hba_attrs_before=$(stat -c '%U:%G %a %C' "$pg_dir_before/pg_hba.conf")
    fi

    bash "$INSTALLER" --uninstall --purge --yes
    local rc=$?
    [ "$rc" -eq 0 ] || { printf '--uninstall --purge exited %d\n' "$rc" >&2; return 1; }

    [ ! -d /var/lib/gotcha ] || { printf '/var/lib/gotcha still present after --purge\n' >&2; return 1; }
    [ ! -d /opt/gotcha ] || { printf '/opt/gotcha still present after --purge\n' >&2; return 1; }
    [ ! -e /etc/gotcha ] || { printf '/etc/gotcha still present after --purge\n' >&2; return 1; }
    ! pg_role_exists || { printf 'postgresql role gotcha still exists after --purge\n' >&2; return 1; }
    ! pg_database_exists || { printf 'postgresql database gotcha still exists after --purge\n' >&2; return 1; }
    ! ch_gotcha_database_exists || { printf 'clickhouse database gotcha still exists after --purge\n' >&2; return 1; }
    ! id -u gotcha >/dev/null 2>&1 || { printf 'system user gotcha still exists after --purge\n' >&2; return 1; }

    # Наши дропины в каталогах чужих пакетов: пакеты остаются, конфиги уходят.
    local pg_conf pg_dir
    pg_dir=$(pg_conf_dir_resolve)
    pg_conf="$pg_dir/conf.d/10-gotcha.conf"
    [ ! -f "$pg_conf" ] || { printf '%s still present after --purge\n' "$pg_conf" >&2; return 1; }
    if [ "$HOST_FAMILY" = rhel ]; then
        ! grep -qF "$PG_INCLUDE_MARKER" "$pg_dir/postgresql.conf" \
            || { printf '%s/postgresql.conf still carries %s after --purge\n' "$pg_dir" "$PG_INCLUDE_MARKER" >&2; return 1; }
        ! grep -qF "$PG_INCLUDE_MARKER" "$pg_dir/pg_hba.conf" \
            || { printf '%s/pg_hba.conf still carries %s after --purge\n' "$pg_dir" "$PG_INCLUDE_MARKER" >&2; return 1; }
        [ "$(stat -c '%U:%G %a %C' "$pg_dir/postgresql.conf")" = "$pg_conf_attrs_before" ] \
            || { printf 'postgresql.conf owner/mode/SELinux context changed by --purge (%s -> %s)\n' \
                "$pg_conf_attrs_before" "$(stat -c '%U:%G %a %C' "$pg_dir/postgresql.conf")" >&2; return 1; }
        [ "$(stat -c '%U:%G %a %C' "$pg_dir/pg_hba.conf")" = "$pg_hba_attrs_before" ] \
            || { printf 'pg_hba.conf owner/mode/SELinux context changed by --purge (%s -> %s)\n' \
                "$pg_hba_attrs_before" "$(stat -c '%U:%G %a %C' "$pg_dir/pg_hba.conf")" >&2; return 1; }
    fi
    [ ! -f /etc/clickhouse-server/config.d/00-common.xml ] \
        || { printf 'clickhouse config.d/00-common.xml still present after --purge\n' >&2; return 1; }
    [ ! -f /etc/clickhouse-server/config.d/10-small.xml ] \
        || { printf 'clickhouse config.d/10-small.xml still present after --purge\n' >&2; return 1; }
    [ ! -f /etc/systemd/system/clickhouse-server.service.d/override.conf ] \
        || { printf 'the clickhouse-server systemd override still present after --purge\n' >&2; return 1; }
    [ ! -f /var/log/gotcha-install.log ] \
        || { printf '/var/log/gotcha-install.log still present after --purge\n' >&2; return 1; }
    pkg_installed "$PG_PACKAGE" || { printf 'postgresql package removed by --purge\n' >&2; return 1; }
    pkg_installed clickhouse-server || { printf 'clickhouse-server package removed by --purge\n' >&2; return 1; }
    pkg_installed nginx || { printf 'nginx package removed by --purge\n' >&2; return 1; }
    pgdg_repo_file_present || { printf 'PGDG repository removed by --purge\n' >&2; return 1; }
    clickhouse_repo_file_present || { printf 'ClickHouse repository removed by --purge\n' >&2; return 1; }
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

    assert "postgresql package installed" pkg_installed "$PG_PACKAGE"
    assert "postgresql unit active" unit_active "$PG_UNIT"
    assert "clickhouse-server package installed" pkg_installed clickhouse-server
    assert "clickhouse-server unit active" unit_active clickhouse-server

    assert "port 5432 loopback-only" port_loopback_only 5432
    assert "port 8123 loopback-only" port_loopback_only 8123
    assert "port 9000 loopback-only" port_loopback_only 9000

    assert "postgresql role gotcha exists" pg_role_exists
    assert "postgresql database gotcha exists" pg_database_exists
    assert "postgresql conf.d/10-gotcha.conf present and tuned" pg_gotcha_conf_present
    assert "gotcha.env GOTCHA_PG_DSN carries no dnf/apt install chatter" gotcha_env_pg_dsn_clean

    assert "clickhouse config.d/00-common.xml present" ch_common_config_present
    assert "clickhouse-server LimitNOFILE=262144" ch_limit_nofile
    assert "clickhouse user gotcha configured" ch_gotcha_user_configured
    assert "clickhouse database gotcha exists" ch_gotcha_database_exists
    assert "EPEL is not enabled when --domain was not given" epel_absent_without_domain

    assert "gotcha /readyz responds 200" readyz_ok
    assert "state directory is 0700 and owned by gotcha" state_dir_secured
    assert "exports directory is 0700 and owned by gotcha" exports_dir_secured
    assert "gotcha.env is root:gotcha 640" env_file_secured
    assert "agent binary downloads and matches SHA256SUMS" agent_binary_download_matches_sums
    assert "gotcha unit hardening directives in effect" unit_hardening_directives
    assert "gotcha survives a postgresql restart" survives_postgresql_restart
    assert "register+onboarding+ingest round trip is visible" e2e_ingest_roundtrip
    assert "the ClickHouse password from gotcha.env authenticates (the documented backup path)" ch_password_from_env_file_works
    assert "the documented ClickHouse password change (users.d + restart) works" documented_ch_password_change_works

    assert "nginx package installed" pkg_installed nginx
    assert "nginx unit active" unit_active nginx
    assert "nginx site config proxies to gotcha with required headers" nginx_site_config_ok
    assert "pre-existing nginx site config was backed up, not clobbered" nginx_site_backed_up
    assert "gotcha /readyz responds 200 via nginx on :80" readyz_via_nginx
    assert "gotcha /metrics responds 403 via nginx on :80" metrics_blocked_via_nginx
    assert "gotcha /version responds 403 via nginx on :80" version_blocked_via_nginx
    assert "a request by IP reaches gotcha, not the nginx welcome page" ip_request_reaches_gotcha

    assert "a certbot-edited nginx site survives a re-run, TLS included" certbot_edits_survive_a_rerun
    assert "a failure after the database steps lists them in the report" failure_report_lists_database_steps
    assert "re-running the installer with the same version is idempotent (unit alive, env untouched, /readyz ok)" survives_idempotent_rerun
    if [ "$HOST_FAMILY" = rhel ]; then
        assert "include_dir 'conf.d' appears exactly once after two installer runs" pg_include_dir_set_once
    fi
    assert "a lost gotcha.env is recovered by regenerating the PostgreSQL/ClickHouse passwords" recovers_after_env_file_lost
    assert "firewall-cmd is not called to open ports when --no-firewall is given" firewall_untouched_with_flag
    assert "firewall-cmd opens http/https and reloads when firewalld reports running" firewall_opened_without_flag
    assert "SELinux httpd_can_network_connect is on when SELinux is Enforcing" selinux_boolean_set_when_enforcing
    assert "EPEL and certbot are installed when --domain is given, without rolling back on a certbot failure" epel_enabled_with_domain

    assert "--uninstall removes the unit and binary, keeps data/databases/packages/repos" uninstall_removes_unit_and_binary_keeps_data
    assert "a request by IP falls back to the default nginx page after --uninstall" ip_request_falls_back_to_default_nginx_page
    assert "--purge without confirmation and without --yes refuses" purge_without_confirmation_refuses
    assert "re-installing after --uninstall succeeds" reinstall_after_uninstall_succeeds
    assert "a certbot-edited nginx site survives --uninstall and a reinstall" certbot_site_survives_uninstall_reinstall
    assert "a failing marker removal is reported, not swallowed, and --purge still exits 0" purge_marker_removal_failure_is_reported_not_silent
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
