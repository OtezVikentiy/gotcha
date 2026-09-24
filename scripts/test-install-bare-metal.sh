#!/usr/bin/env bash
# Юнит-тесты чистых функций internal/docs/install-bare-metal.sh. Без
# фреймворков, в духе "без testify": свои ассерты, счётчик провалов.
set -uo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
INSTALLER="$SCRIPT_DIR/../internal/docs/install-bare-metal.sh"

FAILURES=0

assert_eq() {
    local desc="$1" expected="$2" actual="$3"
    if [ "$expected" != "$actual" ]; then
        printf 'FAIL: %s\n  expected: %s\n  actual:   %s\n' "$desc" "$expected" "$actual" >&2
        FAILURES=$((FAILURES + 1))
    fi
}

assert_contains() {
    local desc="$1" haystack="$2" needle="$3"
    case "$haystack" in
        *"$needle"*) ;;
        *)
            printf 'FAIL: %s\n  missing: %s\n' "$desc" "$needle" >&2
            FAILURES=$((FAILURES + 1))
            ;;
    esac
}

[ -f "$INSTALLER" ] || {
    printf 'FAIL: installer not found: %s\n' "$INSTALLER" >&2
    exit 1
}
# shellcheck source=/dev/null
. "$INSTALLER"

# detect_distro

out=$(detect_distro ubuntu "")
assert_eq "detect_distro ID=ubuntu rc" 0 $?
assert_eq "detect_distro ID=ubuntu family" debian "$out"
out=$(detect_distro debian "")
assert_eq "detect_distro ID=debian family" debian "$out"
out=$(detect_distro linuxmint ubuntu)
assert_eq "detect_distro ID=linuxmint ID_LIKE=ubuntu family" debian "$out"
out=$(detect_distro almalinux "")
assert_eq "detect_distro ID=almalinux family" rhel "$out"
out=$(detect_distro rocky "")
assert_eq "detect_distro ID=rocky family" rhel "$out"
out=$(detect_distro rhel "")
assert_eq "detect_distro ID=rhel family" rhel "$out"
out=$(detect_distro centos "rhel fedora")
assert_eq "detect_distro ID=centos ID_LIKE='rhel fedora' family" rhel "$out"
out=$(detect_distro someel "rhel")
assert_eq "detect_distro unknown ID with ID_LIKE=rhel family" rhel "$out"
detect_distro arch "" >/dev/null
assert_eq "detect_distro ID=arch rejected" 1 $?
detect_distro "" "" >/dev/null
assert_eq "detect_distro empty ID rejected" 1 $?

# detect_el_major

out=$(detect_el_major 9)
assert_eq "detect_el_major 9 value" 9 "$out"
out=$(detect_el_major "9.4")
assert_eq "detect_el_major 9.4 value" 9 "$out"
out=$(detect_el_major 10)
assert_eq "detect_el_major 10 value" 10 "$out"
detect_el_major 8 >/dev/null
assert_eq "detect_el_major 8 rejected" 1 $?
detect_el_major "8.10" >/dev/null
assert_eq "detect_el_major 8.10 rejected" 1 $?
detect_el_major "" >/dev/null
assert_eq "detect_el_major empty rejected" 1 $?

# платформенные пути

# Real assignments, not a prefix: apply_platform_paths and the functions
# below read HOST_FAMILY/EL_MAJOR directly, past this call's scope.
HOST_FAMILY=debian
EL_MAJOR=""
apply_platform_paths
assert_eq "debian PG_UNIT" "postgresql" "$PG_UNIT"
# shellcheck disable=SC2153 # PG_MAJOR — константа из сорсимого файла, не опечатка EL_MAJOR
assert_eq "debian PG_PACKAGE" "postgresql-$PG_MAJOR" "$PG_PACKAGE"
assert_eq "debian PG_BIN_DIR" "/usr/bin" "$PG_BIN_DIR"
assert_eq "debian NGINX_SITE" "/etc/nginx/sites-available/gotcha" "$NGINX_SITE"
assert_eq "debian NGINX_SITE_ENABLED_LINK" "/etc/nginx/sites-enabled/gotcha" "$NGINX_SITE_ENABLED_LINK"
assert_eq "debian REPO_DIR" "/etc/apt/sources.list.d" "$REPO_DIR"
assert_eq "debian pg_conf_dir_label" "/etc/postgresql/*/main" "$(pg_conf_dir_label)"
assert_eq "debian gpg package hint" "gnupg" "${PKG_HINTS[gpg]}"
assert_eq "debian runuser package hint" "util-linux" "${PKG_HINTS[runuser]}"

# shellcheck disable=SC2034 # прочитаны apply_platform_paths/pg_conf_dir_resolve, определёнными в сорсимом файле
HOST_FAMILY=rhel
# shellcheck disable=SC2034 # прочитан apply_platform_paths, определённой в сорсимом файле
EL_MAJOR=9
apply_platform_paths
assert_eq "rhel PG_UNIT" "postgresql-$PG_MAJOR" "$PG_UNIT"
assert_eq "rhel PG_PACKAGE" "postgresql${PG_MAJOR}-server" "$PG_PACKAGE"
assert_eq "rhel PG_BIN_DIR" "/usr/pgsql-$PG_MAJOR/bin" "$PG_BIN_DIR"
assert_eq "rhel NGINX_SITE" "/etc/nginx/conf.d/gotcha.conf" "$NGINX_SITE"
assert_eq "rhel NGINX_SITE_ENABLED_LINK is empty" "" "$NGINX_SITE_ENABLED_LINK"
assert_eq "rhel REPO_DIR" "/etc/yum.repos.d" "$REPO_DIR"
assert_eq "rhel pg_conf_dir_label" "/var/lib/pgsql/$PG_MAJOR/data" "$(pg_conf_dir_label)"
assert_eq "rhel pg_conf_dir_resolve" "/var/lib/pgsql/$PG_MAJOR/data" "$(pg_conf_dir_resolve)"
assert_eq "rhel gpg package hint" "gnupg2" "${PKG_HINTS[gpg]}"
assert_eq "rhel ss package hint" "iproute" "${PKG_HINTS[ss]}"
assert_eq "rhel runuser package hint" "util-linux" "${PKG_HINTS[runuser]}"

# port_owner_units

HOST_FAMILY=rhel
EL_MAJOR=9
apply_platform_paths
assert_eq "rhel port 5432 owner" "postgresql-$PG_MAJOR" "$(port_owner_units 5432)"
assert_eq "rhel port 8080 owner" "gotcha" "$(port_owner_units 8080)"
assert_eq "rhel port 9000 owner" "clickhouse-server" "$(port_owner_units 9000)"

# shellcheck disable=SC2034 # прочитаны apply_platform_paths, определённой в сорсимом файле
HOST_FAMILY=debian
# shellcheck disable=SC2034 # прочитан apply_platform_paths, определённой в сорсимом файле
EL_MAJOR=""
apply_platform_paths
assert_eq "debian port 5432 owner" "postgresql" "$(port_owner_units 5432)"

# required_commands

assert_eq "debian required commands" "curl
tar
gpg
openssl
sha256sum
ss
runuser" "$(required_commands debian "")"
assert_eq "debian required commands, --skip-databases" "curl
tar
gpg
openssl
sha256sum
ss" "$(required_commands debian 1)"
assert_eq "rhel required commands" "curl
tar
gpg
openssl
sha256sum
ss
rpm
dnf
runuser" "$(required_commands rhel "")"
assert_eq "rhel required commands, --skip-databases" "curl
tar
gpg
openssl
sha256sum
ss
rpm
dnf" "$(required_commands rhel 1)"

# detect_arch

out=$(detect_arch x86_64)
assert_eq "detect_arch x86_64 rc" 0 $?
assert_eq "detect_arch x86_64 value" amd64 "$out"
out=$(detect_arch aarch64)
assert_eq "detect_arch aarch64 rc" 0 $?
assert_eq "detect_arch aarch64 value" arm64 "$out"
detect_arch armv7l >/dev/null
assert_eq "detect_arch armv7l rejected" 1 $?

# normalize_version

out=$(normalize_version 1.6.1)
assert_eq "normalize_version leaves a clean X.Y.Z alone" "1.6.1" "$out"
out=$(normalize_version v0.2.0-5-gabcdef-dirty)
assert_eq "normalize_version strips the leading v and the git describe suffix" "0.2.0" "$out"
out=$(normalize_version 1.7.0-rc1)
assert_eq "normalize_version strips a pre-release suffix" "1.7.0" "$out"
out=$(normalize_version 1.7.0+build5)
assert_eq "normalize_version strips a build suffix" "1.7.0" "$out"

# is_semver

is_semver 1.6.1
assert_eq "is_semver accepts X.Y.Z" 0 $?
is_semver 1.7.0-rc1
assert_eq "is_semver rejects a pre-release suffix" 1 $?
is_semver abc
assert_eq "is_semver rejects a non-version" 1 $?
is_semver 1.6
assert_eq "is_semver rejects a two-segment version" 1 $?

# installed_version

fake_bin=$(mktemp)
printf '#!/usr/bin/env bash\necho "gotcha v1.6.1 (abc123, 2026-09-01T00:00:00Z)"\n' >"$fake_bin"
chmod +x "$fake_bin"
out=$(installed_version "$fake_bin")
assert_eq "installed_version strips the leading v from a Docker-style binary" "1.6.1" "$out"

printf '#!/usr/bin/env bash\necho "gotcha 1.6.1 (abc123, 2026-09-01T00:00:00Z)"\n' >"$fake_bin"
chmod +x "$fake_bin"
out=$(installed_version "$fake_bin")
assert_eq "installed_version leaves an old bare-metal-style binary (no v) alone" "1.6.1" "$out"
rm -f "$fake_bin"

out=$(installed_version /nonexistent/gotcha)
assert_eq "installed_version returns empty when the binary is missing" "" "$out"

# version_ge

version_ge 1.10.0 1.9.0
assert_eq "version_ge 1.10.0 >= 1.9.0 (numeric, not lexicographic)" 0 $?
version_ge 1.9.0 1.10.0
assert_eq "version_ge 1.9.0 >= 1.10.0 is false" 1 $?
version_ge 1.6.1 1.6.1
assert_eq "version_ge 1.6.1 >= 1.6.1 (equal is ge)" 0 $?
# Версия уже установленного бинаря приходит из `gotcha --version`, а у собранного
# локально бинаря она выглядит именно так — под set -u это роняло (( )) кодом 1.
out=$(
    version_ge v0.2.0-5-gabcdef-dirty 0.2.0 2>&1
    printf 'rc=%d' $?
)
assert_eq "version_ge survives a git-describe version and compares its numbers" "rc=0" "$out"
out=$(
    version_ge 1.7.0-rc1 1.7.0 2>&1
    printf 'rc=%d' $?
)
assert_eq "version_ge survives a pre-release suffix" "rc=0" "$out"

# Ровно вход main(): prev_version из installed_version (может быть в обоих
# форматах — старый тарбол без "v", новый с ним), ARG_VERSION всегда X.Y.Z.
version_ge 1.6.1 1.7.0
assert_eq "version_ge false: unprefixed previous version older than the target" 1 $?
version_ge v1.6.1 1.7.0
assert_eq "version_ge false: v-prefixed previous version older than the target" 1 $?
version_ge 1.7.0 1.6.1
assert_eq "version_ge true: unprefixed previous version newer than the target" 0 $?
version_ge v1.7.0 1.6.1
assert_eq "version_ge true: v-prefixed previous version newer than the target" 0 $?

# parse_args

parse_args --purge --from-tarball /tmp/x.tar.gz >/dev/null 2>&1
assert_eq "parse_args --purge without --uninstall rejected" 2 $?

parse_args --uninstall --purge --from-tarball /tmp/x.tar.gz >/dev/null 2>&1
assert_eq "parse_args --purge with --uninstall accepted" 0 $?

parse_args >/dev/null 2>&1
assert_eq "parse_args with tree default version (dev) and no --from-tarball rejected" 2 $?

parse_args --from-tarball /tmp/x.tar.gz >/dev/null 2>&1
assert_eq "parse_args --from-tarball alone accepted" 0 $?

parse_args --unknown-flag >/dev/null 2>&1
assert_eq "parse_args unknown flag rejected" 2 $?

parse_args --skip-databases --from-tarball /tmp/x.tar.gz >/dev/null 2>&1
assert_eq "parse_args --skip-databases without DSNs rejected" 2 $?

parse_args --skip-databases --pg-dsn pg://x --ch-dsn ch://x --from-tarball /tmp/x.tar.gz >/dev/null 2>&1
assert_eq "parse_args --skip-databases with both DSNs accepted" 0 $?

parse_args --version 1.7.0-rc1 >/dev/null 2>&1
assert_eq "parse_args rejects a --version with a suffix instead of dying inside version_ge" 2 $?

parse_args --version abc >/dev/null 2>&1
assert_eq "parse_args rejects a non-version --version" 2 $?

parse_args --mem-limit abc --from-tarball /tmp/x.tar.gz >/dev/null 2>&1
assert_eq "parse_args rejects a non-numeric --mem-limit" 2 $?

parse_args --mem-limit 512 --from-tarball /tmp/x.tar.gz >/dev/null 2>&1
assert_eq "parse_args accepts a numeric --mem-limit" 0 $?

out=$(parse_args --domain example.com --from-tarball /tmp/x.tar.gz 2>&1)
rc=$?
assert_eq "parse_args --domain refused with the usage code" 2 "$rc"
assert_contains "parse_args --domain explains the removal" "$out" \
    "install-bare-metal: --domain/--email were removed in 1.9.0: the installer no longer sets up a web server or TLS."
assert_contains "parse_args --domain points at --base-url and the guide" "$out" \
    'Pass --base-url https://<domain> and put your own reverse proxy in front of 127.0.0.1:8080 — see "External access and TLS" in the installation guide.'
out=$(parse_args --email a@b.example --from-tarball /tmp/x.tar.gz 2>&1)
rc=$?
assert_eq "parse_args --email refused with the usage code" 2 "$rc"
assert_contains "parse_args --email explains the removal" "$out" "--domain/--email were removed in 1.9.0"
out=$(parse_args --domain 2>&1)
rc=$?
assert_eq "parse_args bare --domain refused with the usage code" 2 "$rc"
assert_contains "parse_args bare --domain gets the removal text, not 'requires a value'" "$out" \
    "--domain/--email were removed in 1.9.0"

for flag in --no-proxy --no-firewall; do
    out=$(parse_args "$flag" --from-tarball /tmp/x.tar.gz 2>&1)
    rc=$?
    assert_eq "parse_args accepts deprecated $flag" 0 "$rc"
    assert_contains "parse_args says $flag is deprecated" "$out" \
        "install-bare-metal: $flag is deprecated and does nothing"
done
parse_args --no-proxy --no-firewall --from-tarball /tmp/x.tar.gz --yes 2>/dev/null
assert_eq "parse_args keeps parsing after deprecated flags" "/tmp/x.tar.gz|1" "$ARG_FROM_TARBALL|$ARG_YES"

parse_args --base-url https://x.example --version 9.9.9 --dry-run >/dev/null 2>&1
assert_eq "parse_args accepts a full example" 0 $?
assert_eq "parse_args sets ARG_BASE_URL" https://x.example "$ARG_BASE_URL"
assert_eq "parse_args sets ARG_VERSION" 9.9.9 "$ARG_VERSION"
assert_eq "parse_args sets ARG_DRY_RUN" 1 "$ARG_DRY_RUN"

usage_text=$(usage)
for gone in --domain --email --no-proxy --no-firewall; do
    case "$usage_text" in
        *"  $gone "*) printf 'FAIL: usage() still lists %s\n' "$gone" >&2; FAILURES=$((FAILURES + 1)) ;;
    esac
done

# validate_base_url / normalize_base_url

assert_eq "normalize_base_url strips one trailing slash" "https://x" "$(normalize_base_url 'https://x/')"
assert_eq "normalize_base_url strips every trailing slash" "https://x" "$(normalize_base_url 'https://x///')"
assert_eq "normalize_base_url leaves a path alone" "https://x/app" "$(normalize_base_url 'https://x/app')"

for good in \
    "https://gotcha.example.com" \
    "http://10.0.0.5:8080" \
    "http://[::1]:8080" \
    "https://gw.example.com/gotcha" \
    "https://x/a%20b"; do
    out=$(validate_base_url "$good" 2>/dev/null)
    assert_eq "validate_base_url accepts $good" "$good" "$out"
done
out=$(validate_base_url "https://x/" 2>/dev/null)
assert_eq "validate_base_url normalizes a trailing slash" "https://x" "$out"

for bad in \
    "ftp://x" \
    "https://" \
    "gotcha.example.com" \
    "https://x?a=1" \
    "https://x#f" \
    "https://x y" \
    'https://x"' \
    "https://x'" \
    "https://x\\" \
    $'https://x\nhttps://y' \
    "https://x&y" \
    "https://a%zz" \
    "https://user@x" \
    "http://[::1" \
    "https://x/a?a=1" \
    "https://x/a#f" \
    "https://x/a b" \
    'https://x/a"'; do
    out=$(validate_base_url "$bad" 2>&1)
    rc=$?
    assert_eq "validate_base_url rejects $(printf '%q' "$bad")" 1 "$rc"
    assert_contains "validate_base_url names the value and an example for $(printf '%q' "$bad")" \
        "$out" "e.g. https://gotcha.example.com"
done

parse_args --base-url "https://x/" --from-tarball /tmp/x.tar.gz >/dev/null 2>&1
assert_eq "parse_args accepts a valid --base-url" 0 $?
assert_eq "parse_args stores --base-url normalized" "https://x" "$ARG_BASE_URL"
out=$(parse_args --base-url "gotcha.example.com" --from-tarball /tmp/x.tar.gz 2>&1)
assert_eq "parse_args rejects a --base-url without a scheme" 2 $?
assert_contains "parse_args shows an example of a valid --base-url" "$out" "e.g. https://gotcha.example.com"

# В дереве GOTCHA_INSTALL_DEFAULT_VERSION="dev", и §4.7 не исполняется ни в одном
# прогоне — проверяется на копии, какую кладёт в релиз джоба dist.

PATCHED=$(mktemp)
sed 's/^GOTCHA_INSTALL_DEFAULT_VERSION="dev"$/GOTCHA_INSTALL_DEFAULT_VERSION="1.6.1"/' "$INSTALLER" >"$PATCHED"
if ! grep -q '^GOTCHA_INSTALL_DEFAULT_VERSION="1.6.1"$' "$PATCHED"; then
    printf 'FAIL: could not patch GOTCHA_INSTALL_DEFAULT_VERSION — the test below would check nothing\n' >&2
    FAILURES=$((FAILURES + 1))
fi

# shellcheck source=/dev/null
(. "$PATCHED" && parse_args --version 1.5.0) >/dev/null 2>&1
assert_eq "released copy refuses a version older than itself" 2 $?
# shellcheck source=/dev/null
(. "$PATCHED" && parse_args --version 1.7.0) >/dev/null 2>&1
assert_eq "released copy accepts a newer version" 0 $?
# shellcheck source=/dev/null
(. "$PATCHED" && parse_args --version 1.5.0 --force-version) >/dev/null 2>&1
assert_eq "released copy accepts an older version with --force-version" 0 $?
# shellcheck source=/dev/null
(. "$PATCHED" && parse_args) >/dev/null 2>&1
assert_eq "released copy runs without --version at all (its own version is the default)" 0 $?
# shellcheck source=/dev/null
(. "$PATCHED" && parse_args --version 1.7.0-rc1) >/dev/null 2>&1
assert_eq "released copy rejects a suffixed version with the usage code, not a bash error" 2 $?
rm -f "$PATCHED"

# choose_base_url

out=$(choose_base_url "https://explicit.example" "10.0.0.1")
assert_eq "choose_base_url prefers --base-url" "https://explicit.example" "$out"
out=$(choose_base_url "" "10.0.0.1")
assert_eq "choose_base_url falls back to host IP" "http://10.0.0.1" "$out"

# compute_memlimit — константа, паритетная compose (mem_limit: 1g), одна и
# та же независимо от RAM хоста (preflight и так отсекает хосты младше 2 ГБ).

out=$(compute_memlimit 2048)
assert_eq "compute_memlimit at 2 GB RAM" "1024M 819MiB" "$out"
out=$(compute_memlimit 8192)
assert_eq "compute_memlimit at 8 GB RAM" "1024M 819MiB" "$out"
out=$(compute_memlimit)
assert_eq "compute_memlimit with no RAM argument" "1024M 819MiB" "$out"

# resolve_memlimit — что main() реально вызывает: --mem-limit override или
# дефолт из compute_memlimit.

out=$(resolve_memlimit "")
assert_eq "resolve_memlimit falls back to compute_memlimit without --mem-limit" "1024M 819MiB" "$out"
out=$(resolve_memlimit 512)
assert_eq "resolve_memlimit honors an explicit --mem-limit" "512M 409MiB" "$out"

unit=$(render_unit "512M")
assert_contains "render_unit picks up a --mem-limit override" "$unit" "MemoryMax=512M"
env_file=$(render_env_file "pg" "ch" "secret" "https://x.example" "/opt/gotcha/agent-dist" "409MiB" "127.0.0.1:8080")
assert_contains "render_env_file picks up a --mem-limit override" "$env_file" "GOMEMLIMIT=409MiB"

# render_unit — presence of every parity directive from spec §5, as a whole
# list, not a single membership check.

unit=$(render_unit 1024M)
for directive in \
    "ProtectSystem=strict" \
    "PrivateTmp=yes" \
    "CapabilityBoundingSet=" \
    "AmbientCapabilities=" \
    "NoNewPrivileges=yes" \
    "TasksMax=512" \
    "MemoryMax=1024M" \
    "MemoryAccounting=yes" \
    "TimeoutStopSec=90" \
    "Restart=always" \
    "RestartSec=5" \
    "StateDirectory=gotcha" \
    "StateDirectoryMode=0700" \
    "After=postgresql.service postgresql-17.service clickhouse-server.service network-online.target"; do
    assert_contains "render_unit contains $directive" "$unit" "$directive"
done

# Директивы сверх паритета: hardening.md перечисляет каждую как поставленную, и до
# этого списка их удаление из render_unit не ловил ни один прогон.

for directive in \
    "ProtectHome=yes" \
    "PrivateDevices=yes" \
    "ProtectKernelTunables=yes" \
    "ProtectKernelModules=yes" \
    "ProtectKernelLogs=yes" \
    "ProtectControlGroups=yes" \
    "ProtectClock=yes" \
    "ProtectHostname=yes" \
    "ProtectProc=invisible" \
    "RestrictNamespaces=yes" \
    "RestrictRealtime=yes" \
    "RestrictSUIDSGID=yes" \
    "RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX" \
    "LockPersonality=yes" \
    "SystemCallFilter=@system-service" \
    "SystemCallArchitectures=native" \
    "UMask=0077" \
    "MemoryDenyWriteExecute=yes"; do
    assert_contains "render_unit contains beyond-parity $directive" "$unit" "$directive"
done

# render_env_file

env_file=$(render_env_file "pg-dsn" "ch-dsn" "secret" "https://x.example" \
    "/opt/gotcha/agent-dist" "819MiB" "127.0.0.1:8080")
for var in GOTCHA_PG_DSN GOTCHA_CH_DSN GOTCHA_SECRET_KEY GOTCHA_BASE_URL \
    GOTCHA_DIST_DIR GOMEMLIMIT GOTCHA_LISTEN_ADDR; do
    assert_contains "render_env_file contains $var" "$env_file" "$var="
done

# verify_loopback_only — ветка отказа на живом хосте не воспроизводится, поэтому
# ss подменяется функцией; фактический bind проверяет e2e.

# shellcheck disable=SC2317 # вызывается сорсимым файлом как внешняя команда ss, а не отсюда
ss() { printf '%s\n' "$SS_STUB_OUT"; }

SS_STUB_OUT='LISTEN 0 244 127.0.0.1:5432 0.0.0.0:*'
(verify_loopback_only 5432) >/dev/null 2>&1
assert_eq "verify_loopback_only accepts a loopback bind" 0 $?

SS_STUB_OUT='LISTEN 0 244 [::1]:8123 [::]:*'
(verify_loopback_only 8123) >/dev/null 2>&1
assert_eq "verify_loopback_only accepts an IPv6 loopback bind" 0 $?

SS_STUB_OUT='LISTEN 0 244 0.0.0.0:5432 0.0.0.0:*'
out=$( (verify_loopback_only 5432) 2>&1 )
rc=$?
assert_eq "verify_loopback_only refuses a wildcard bind with the database exit code" 5 "$rc"
assert_contains "verify_loopback_only names the port and the address it found" "$out" \
    "port 5432 listens on 0.0.0.0:5432, not loopback only"

# main() сужает IFS до "\n\t", и "$*" склеил бы порты переводами строк: запись в
# журнале стала бы многострочной, а в отчёт о провале попала бы только первая строка.
SS_STUB_OUT='LISTEN 0 244 127.0.0.1:5432 0.0.0.0:*
LISTEN 0 244 127.0.0.1:8123 0.0.0.0:*
LISTEN 0 244 [::1]:9000 [::]:*'
out=$( (IFS=$'\n\t'; verify_loopback_only 5432 8123 9000) 2>&1 )
assert_eq "verify_loopback_only logs a single-line step under main's IFS" \
    "install-bare-metal: databases listen on loopback only: 5432, 8123, 9000" "$out"

SS_STUB_OUT=''
out=$( (verify_loopback_only 9000) 2>&1 )
rc=$?
assert_eq "verify_loopback_only refuses when nothing listens at all" 5 "$rc"
assert_contains "verify_loopback_only says nothing listens" "$out" "nothing listens on port 9000"

unset -f ss

# render_pg_conf

pg_conf=$(render_pg_conf)
assert_contains "render_pg_conf random_page_cost" "$pg_conf" "random_page_cost = 1.1"
assert_contains "render_pg_conf effective_io_concurrency" "$pg_conf" "effective_io_concurrency = 200"

# render_pgdg_repo

# shellcheck disable=SC2034 # прочитана apply_platform_paths, определённой в сорсимом файле
HOST_FAMILY=rhel
# shellcheck disable=SC2034 # прочитана apply_platform_paths, определённой в сорсимом файле
EL_MAJOR=9
apply_platform_paths
out=$(render_pgdg_repo 9)
assert_contains "pgdg repo has pgdg-common section" "$out" "[pgdg-common]"
assert_contains "pgdg repo has the major section" "$out" "[pgdg$PG_MAJOR]"
assert_contains "pgdg repo common baseurl" "$out" \
    "https://download.postgresql.org/pub/repos/yum/common/redhat/rhel-9-\$basearch"
assert_contains "pgdg repo major baseurl" "$out" \
    "https://download.postgresql.org/pub/repos/yum/$PG_MAJOR/redhat/rhel-9-\$basearch"
assert_contains "pgdg repo verifies packages" "$out" "gpgcheck=1"
assert_contains "pgdg repo verifies metadata" "$out" "repo_gpgcheck=1"
assert_contains "pgdg repo trusts a local key file" "$out" "gpgkey=file:///etc/pki/rpm-gpg/gotcha-pgdg.asc"
out=$(render_pgdg_repo 10)
assert_contains "pgdg repo for EL10" "$out" "redhat/rhel-10-\$basearch"

# pgdg_rpm_key_for_arch

out=$(pgdg_rpm_key_for_arch amd64 url)
assert_eq "pgdg url for amd64" "$PGDG_RPM_KEY_URL" "$out"
out=$(pgdg_rpm_key_for_arch amd64 fingerprint)
assert_eq "pgdg fingerprint for amd64" "$PGDG_RPM_KEY_FINGERPRINT" "$out"
out=$(pgdg_rpm_key_for_arch arm64 url)
assert_eq "pgdg url for arm64" "$PGDG_RPM_KEY_URL_ARM64" "$out"
out=$(pgdg_rpm_key_for_arch arm64 fingerprint)
assert_eq "pgdg fingerprint for arm64" "$PGDG_RPM_KEY_FINGERPRINT_ARM64" "$out"

# repo_add_pgdg вызывает pgdg_rpm_key_for_arch дважды через $(...), не read
# по паре "URL отпечаток": IFS сужен main() до "\n\t", read склеил бы поля.
out=$( (
    IFS=$'\n\t'
    key_url=$(pgdg_rpm_key_for_arch arm64 url)
    key_fpr=$(pgdg_rpm_key_for_arch arm64 fingerprint)
    printf '%s|%s\n' "$key_url" "$key_fpr"
) )
assert_eq "pgdg_rpm_key_for_arch call site survives main's narrowed IFS" \
    "$PGDG_RPM_KEY_URL_ARM64|$PGDG_RPM_KEY_FINGERPRINT_ARM64" "$out"

# render_clickhouse_repo

# shellcheck disable=SC2034 # прочитана apply_platform_paths, определённой в сорсимом файле
HOST_FAMILY=rhel
# shellcheck disable=SC2034 # прочитана apply_platform_paths, определённой в сорсимом файле
EL_MAJOR=9
apply_platform_paths
out=$(render_clickhouse_repo)
assert_contains "clickhouse repo baseurl" "$out" "https://packages.clickhouse.com/rpm/stable/"
# gpgcheck=0: пакеты ClickHouse не подписаны индивидуально, как и в их собственном
# packages.clickhouse.com/rpm/clickhouse.repo — доверие даёт repo_gpgcheck ниже.
assert_contains "clickhouse repo does not require per-package signatures" "$out" $'\ngpgcheck=0\n'
assert_contains "clickhouse repo verifies metadata" "$out" "repo_gpgcheck=1"
assert_contains "clickhouse repo trusts a local key file" "$out" \
    "gpgkey=file:///etc/pki/rpm-gpg/gotcha-clickhouse.asc"

# clickhouse_version_from_dnf_list

out=$(printf '%s\n' \
    'clickhouse-server.noarch    25.8.1.1-1    gotcha-clickhouse' \
    'clickhouse-server.noarch    25.3.14.14-1    gotcha-clickhouse' \
    'clickhouse-server.noarch    25.3.9.1-1    gotcha-clickhouse' \
    'clickhouse-server.noarch    125.3.1.1-1    gotcha-clickhouse' \
    'clickhouse-server.noarch    25.30.1.1-1    gotcha-clickhouse' \
    | clickhouse_version_from_dnf_list)
assert_eq "dnf list picks the newest 25.3 patch, not a decoy that merely contains 25.3." \
    "25.3.14.14-1" "$out"

# dnf переносит длинные строки: версия оказывается на следующей строке с отступом
out=$(printf '%s\n' \
    'clickhouse-server.noarch' \
    '                            25.3.14.14-1    gotcha-clickhouse' \
    | clickhouse_version_from_dnf_list)
assert_eq "dnf list wrapped line parsed" "25.3.14.14-1" "$out"

out=$(printf '%s\n' 'clickhouse-server.noarch    24.8.1.1-1    gotcha-clickhouse' \
    | clickhouse_version_from_dnf_list)
assert_eq "dnf list without a matching major returns empty" "" "$out"

# 125.3.1.1-1 содержит "25.3." не с начала, 25.30.1.1-1 — с другим минором:
# обе строки не версия 25.3.x, только похожи на неё как подстрока.
out=$(printf '%s\n' \
    'clickhouse-server.noarch    125.3.1.1-1    gotcha-clickhouse' \
    'clickhouse-server.noarch    25.30.1.1-1    gotcha-clickhouse' \
    | clickhouse_version_from_dnf_list)
assert_eq "dnf list rejects versions that only contain 25.3. as a substring" "" "$out"

# nginx_site_disabled_path

# shellcheck disable=SC2034 # прочитана apply_platform_paths, определённой в сорсимом файле
HOST_FAMILY=rhel
# shellcheck disable=SC2034 # прочитана apply_platform_paths, определённой в сорсимом файле
EL_MAJOR=9
apply_platform_paths
assert_eq "rhel nginx disabled-site path" "/etc/nginx/conf.d/gotcha.conf.disabled" \
    "$(nginx_site_disabled_path)"
# shellcheck disable=SC2034 # прочитана apply_platform_paths, определённой в сорсимом файле
HOST_FAMILY=debian
# shellcheck disable=SC2034 # прочитана apply_platform_paths, определённой в сорсимом файле
EL_MAJOR=""
apply_platform_paths
assert_eq "debian nginx disabled-site path" "/etc/nginx/sites-available/gotcha.disabled" \
    "$(nginx_site_disabled_path)"

# pg_hba_host_method_is_password

printf 'host all all 127.0.0.1/32 scram-sha-256\n' | pg_hba_host_method_is_password
assert_eq "pg_hba scram is a password method" 0 $?
printf 'host all all 127.0.0.1/32 md5\n' | pg_hba_host_method_is_password
assert_eq "pg_hba md5 is a password method" 0 $?
printf 'host all all 127.0.0.1/32 ident\n' | pg_hba_host_method_is_password
assert_eq "pg_hba ident is not a password method" 1 $?
printf 'host all all 127.0.0.1/32 reject\n' | pg_hba_host_method_is_password
assert_eq "pg_hba reject is not a password method" 1 $?
printf '# host all all 127.0.0.1/32 scram-sha-256\n' | pg_hba_host_method_is_password
assert_eq "pg_hba commented line does not count" 1 $?
printf 'host all all ::1/128 scram-sha-256\n' | pg_hba_host_method_is_password
assert_eq "pg_hba ipv6-only line does not count for 127.0.0.1" 1 $?

# ensure_include_dir — идемпотентная дописка

tmpconf=$(mktemp)
printf "#include_dir = 'conf.d'\n" >"$tmpconf"
ensure_include_dir "$tmpconf"
ensure_include_dir "$tmpconf"
assert_eq "include_dir appended exactly once" 1 "$(grep -c "^include_dir = 'conf.d'" "$tmpconf")"
assert_eq "include_dir marker present" 1 "$(grep -cF "$PG_INCLUDE_MARKER" "$tmpconf")"
rm -f "$tmpconf"

# remove_marker_block — снятие дописки по маркеру на реальных файлах RHEL PG17
# (снятые копии /var/lib/pgsql/17/data/{postgresql,pg_hba}.conf после initdb)

tmpconf=$(mktemp)
cp "$SCRIPT_DIR/testdata/postgresql.conf.rhel-sample" "$tmpconf"
ensure_include_dir "$tmpconf"
remove_marker_block "$PG_INCLUDE_MARKER" "$tmpconf"
assert_eq "remove_marker_block restores postgresql.conf byte-for-byte" "" \
    "$(diff "$SCRIPT_DIR/testdata/postgresql.conf.rhel-sample" "$tmpconf")"
remove_marker_block "$PG_INCLUDE_MARKER" "$tmpconf"
assert_eq "remove_marker_block on an already-clean file is a no-op" "" \
    "$(diff "$SCRIPT_DIR/testdata/postgresql.conf.rhel-sample" "$tmpconf")"
rm -f "$tmpconf"

tmphba=$(mktemp)
cp "$SCRIPT_DIR/testdata/pg_hba.conf.rhel-sample" "$tmphba"
printf '%s\nhost all all 127.0.0.1/32 scram-sha-256\n' "$PG_INCLUDE_MARKER" >>"$tmphba"
remove_marker_block "$PG_INCLUDE_MARKER" "$tmphba"
assert_eq "remove_marker_block restores pg_hba.conf byte-for-byte" "" \
    "$(diff "$SCRIPT_DIR/testdata/pg_hba.conf.rhel-sample" "$tmphba")"
rm -f "$tmphba"

remove_marker_block "$PG_INCLUDE_MARKER" /nonexistent/gotcha-purge-test
assert_eq "remove_marker_block on a missing file is a no-op, not an error" 0 $?

# remove_marker_block — контент, дописанный ОПЕРАТОРОМ после нашей пары, обязан уцелеть

tmpconf=$(mktemp)
cp "$SCRIPT_DIR/testdata/postgresql.conf.rhel-sample" "$tmpconf"
ensure_include_dir "$tmpconf"
printf "shared_preload_libraries = 'pg_stat_statements'\n" >>"$tmpconf"
remove_marker_block "$PG_INCLUDE_MARKER" "$tmpconf"
assert_eq "remove_marker_block keeps operator content written after the marker pair (postgresql.conf)" \
    "shared_preload_libraries = 'pg_stat_statements'" "$(tail -n1 "$tmpconf")"
assert_eq "remove_marker_block touches nothing but the marker pair itself (postgresql.conf)" \
    "$(cat "$SCRIPT_DIR/testdata/postgresql.conf.rhel-sample"; printf "shared_preload_libraries = 'pg_stat_statements'\n")" \
    "$(cat "$tmpconf")"
rm -f "$tmpconf"

tmphba=$(mktemp)
cp "$SCRIPT_DIR/testdata/pg_hba.conf.rhel-sample" "$tmphba"
printf '%s\nhost all all 127.0.0.1/32 scram-sha-256\n' "$PG_INCLUDE_MARKER" >>"$tmphba"
printf 'host all all 10.0.0.0/8 reject\n' >>"$tmphba"
remove_marker_block "$PG_INCLUDE_MARKER" "$tmphba"
assert_eq "remove_marker_block keeps operator content written after the marker pair (pg_hba.conf)" \
    "host all all 10.0.0.0/8 reject" "$(tail -n1 "$tmphba")"
rm -f "$tmphba"

# remove_marker_block — маркер как ПОДСТРОКА чужой строки не считается маркерной строкой

tmpconf=$(mktemp)
printf '# note: mentions "%s" for reference\nkeep_this = on\n' "$PG_INCLUDE_MARKER" >"$tmpconf"
remove_marker_block "$PG_INCLUDE_MARKER" "$tmpconf"
assert_eq "remove_marker_block leaves a line where the marker is only a substring" 2 "$(wc -l <"$tmpconf")"
assert_contains "remove_marker_block does not strip the substring-marker line itself" \
    "$(cat "$tmpconf")" "$PG_INCLUDE_MARKER"
rm -f "$tmpconf"

# remove_marker_block — файл без маркера вообще не переписывается (инод и mtime,
# не только содержимое: перезапись через одинаковый контент осталась бы незамеченной)

tmpconf=$(mktemp)
printf 'unrelated = 1\nmore = 2\n' >"$tmpconf"
touch -d '2020-01-01 00:00:00' "$tmpconf"
inode_before=$(stat -c '%i' "$tmpconf")
mtime_before=$(stat -c '%Y' "$tmpconf")
remove_marker_block "$PG_INCLUDE_MARKER" "$tmpconf"
assert_eq "remove_marker_block without the marker does not rewrite the file (inode)" \
    "$inode_before" "$(stat -c '%i' "$tmpconf")"
assert_eq "remove_marker_block without the marker does not rewrite the file (mtime)" \
    "$mtime_before" "$(stat -c '%Y' "$tmpconf")"
rm -f "$tmpconf"

# remove_marker_block — маркер встретился дважды: обе пары снимаются, остальное цело

tmpconf=$(mktemp)
printf 'before = 1\n%s\npayload1 = a\nmiddle = 2\n%s\npayload2 = b\nafter = 3\n' \
    "$PG_INCLUDE_MARKER" "$PG_INCLUDE_MARKER" >"$tmpconf"
remove_marker_block "$PG_INCLUDE_MARKER" "$tmpconf"
assert_eq "remove_marker_block removes every marker+payload pair, not only the first" \
    "before = 1
middle = 2
after = 3" "$(cat "$tmpconf")"
rm -f "$tmpconf"

# remove_marker_block — перезапись сохраняет режим файла; владелец/SELinux-контекст
# проверяет e2e на EL (здесь нет root/postgres для достоверного chown).

tmpconf=$(mktemp)
cp "$SCRIPT_DIR/testdata/postgresql.conf.rhel-sample" "$tmpconf"
chmod 0600 "$tmpconf"
ensure_include_dir "$tmpconf"
remove_marker_block "$PG_INCLUDE_MARKER" "$tmpconf"
assert_eq "remove_marker_block preserves the file mode" "600" "$(stat -c '%a' "$tmpconf")"
rm -f "$tmpconf"

# remove_marker_block — $file симлинк на конфиг под системой конфигурации: симлинк
# остаётся симлинком на тот же таргет, содержимое и права таргета — как в happy-path.

tmpdir=$(mktemp -d)
target="$tmpdir/real-postgresql.conf"
link="$tmpdir/postgresql.conf"
cp "$SCRIPT_DIR/testdata/postgresql.conf.rhel-sample" "$target"
chmod 0640 "$target"
ln -s "$target" "$link"
ensure_include_dir "$link"
printf "shared_preload_libraries = 'pg_stat_statements'\n" >>"$link"
remove_marker_block "$PG_INCLUDE_MARKER" "$link"
assert_eq "remove_marker_block leaves the symlink in place, pointing at its target" \
    "symlink:$target" "$([ -L "$link" ] && printf 'symlink:%s' "$(readlink -f "$link")" || printf 'not-a-symlink')"
assert_eq "remove_marker_block through a symlink keeps content written after the marker pair" \
    "shared_preload_libraries = 'pg_stat_statements'" "$(tail -n1 "$target")"
assert_eq "remove_marker_block through a symlink does not wipe the target" \
    "$(cat "$SCRIPT_DIR/testdata/postgresql.conf.rhel-sample"; printf "shared_preload_libraries = 'pg_stat_statements'\n")" \
    "$(cat "$target")"
assert_eq "remove_marker_block through a symlink preserves the target's mode" "640" "$(stat -c '%a' "$target")"
rm -rf "$tmpdir"

# remove_marker_block — временный файл не создать (директория без прав на запись):
# возвращает 1, ничего не оставляет за собой. Под root пропускается.

if [ "$(id -u)" = 0 ]; then
    printf 'note: running as root, skipping the remove_marker_block read-only-directory case\n'
else
    tmpdir=$(mktemp -d)
    tmpconf="$tmpdir/postgresql.conf"
    cp "$SCRIPT_DIR/testdata/postgresql.conf.rhel-sample" "$tmpconf"
    ensure_include_dir "$tmpconf"
    chmod 555 "$tmpdir"
    remove_marker_block "$PG_INCLUDE_MARKER" "$tmpconf"
    assert_eq "remove_marker_block returns 1 when the temp file cannot be created" 1 $?
    chmod 755 "$tmpdir"
    assert_eq "remove_marker_block leaves no temp file behind after a cp -a failure" "" \
        "$(find "$tmpdir" -maxdepth 1 -name '*.gotcha-tmp')"
    rm -rf "$tmpdir"
fi

# remove_marker_block — awk отказывает: возвращает 1, временный файл убран за собой

stubdir=$(mktemp -d)
printf '#!/bin/sh\nexit 1\n' >"$stubdir/awk"
chmod +x "$stubdir/awk"
tmpconf=$(mktemp)
cp "$SCRIPT_DIR/testdata/postgresql.conf.rhel-sample" "$tmpconf"
ensure_include_dir "$tmpconf"
PATH="$stubdir:$PATH" remove_marker_block "$PG_INCLUDE_MARKER" "$tmpconf"
assert_eq "remove_marker_block returns 1 when awk fails" 1 $?
assert_eq "remove_marker_block removes its temp file when awk fails" "" \
    "$(find "$(dirname "$tmpconf")" -maxdepth 1 -name "$(basename "$tmpconf").gotcha-tmp")"
rm -f "$tmpconf"
rm -rf "$stubdir"

# remove_marker_block — cp -a падает ПОСЛЕ того, как содержимое уже скопировано
# (сохранение прав/контекста не поддержано ФС) — без || return 1 awk и mv прошли бы молча.

stubdir=$(mktemp -d)
# shellcheck disable=SC2016 # literal $2/$3 for the stub script, not expanded here
printf '#!/bin/sh\ncat "$2" >"$3" 2>/dev/null\nexit 1\n' >"$stubdir/cp"
chmod +x "$stubdir/cp"
tmpconf=$(mktemp)
cp "$SCRIPT_DIR/testdata/postgresql.conf.rhel-sample" "$tmpconf"
ensure_include_dir "$tmpconf"
before=$(cat "$tmpconf")
PATH="$stubdir:$PATH" remove_marker_block "$PG_INCLUDE_MARKER" "$tmpconf"
assert_eq "remove_marker_block returns 1 when cp -a fails after creating the temp file" 1 $?
assert_eq "remove_marker_block leaves the original file untouched when cp -a fails" "$before" "$(cat "$tmpconf")"
assert_contains "remove_marker_block leaves the marker in place when cp -a fails" "$(cat "$tmpconf")" "$PG_INCLUDE_MARKER"
rm -f "$tmpconf"
rm -rf "$stubdir"

# verify_key_fingerprint отвергает файл с двумя основными ключами

out=$( (verify_key_fingerprint "$SCRIPT_DIR/testdata/two-pub-keys.asc" DEADBEEF) 2>&1 )
assert_eq "two primary keys rejected" "$EXIT_DATABASE" $?
assert_contains "two primary keys message" "$out" "exactly one primary key"

# dist_url

out=$(dist_url "https://github.com/OtezVikentiy/gotcha/releases/download" "1.6.1" "amd64")
assert_eq "dist_url amd64" \
    "https://github.com/OtezVikentiy/gotcha/releases/download/v1.6.1/gotcha-1.6.1-linux-amd64.tar.gz" "$out"
out=$(dist_url "https://github.com/OtezVikentiy/gotcha/releases/download" "1.6.1" "arm64")
assert_eq "dist_url arm64" \
    "https://github.com/OtezVikentiy/gotcha/releases/download/v1.6.1/gotcha-1.6.1-linux-arm64.tar.gz" "$out"
out=$(dist_url "https://mirror.example/base/" "2.0.0" "amd64")
assert_eq "dist_url honors --download-base (trailing slash stripped)" \
    "https://mirror.example/base/v2.0.0/gotcha-2.0.0-linux-amd64.tar.gz" "$out"

# find_legacy_site / uninstall_legacy_site — на временных путях, systemctl подменён

legacy_dir=$(mktemp -d)
# shellcheck disable=SC2034 # читает log_step из сорсимого файла
INSTALL_JOURNAL="$legacy_dir/journal"
# shellcheck disable=SC2317 # вызывается сорсимым файлом, а не отсюда
systemctl() { printf 'systemctl %s\n' "$*" >>"$legacy_dir/calls"; }
path_state() {
    if [ -e "$1" ] || [ -L "$1" ]; then echo present; else echo gone; fi
}

# shellcheck disable=SC2034 # прочитаны apply_platform_paths, определённой в сорсимом файле
HOST_FAMILY=rhel
# shellcheck disable=SC2034 # прочитан apply_platform_paths, определённой в сорсимом файле
EL_MAJOR=9
apply_platform_paths
NGINX_SITE="$legacy_dir/conf.d/gotcha.conf"
mkdir -p "$legacy_dir/conf.d"

printf 'server { listen 80; }\n' >"$NGINX_SITE"
find_legacy_site >/dev/null
assert_eq "find_legacy_site ignores an unmarked site" 1 $?
: >"$legacy_dir/calls"
(uninstall_legacy_site) 2>/dev/null
assert_eq "uninstall_legacy_site leaves an unmarked EL site alone (no systemctl)" "" "$(cat "$legacy_dir/calls")"
assert_eq "uninstall_legacy_site does not rename an unmarked EL site" present "$(path_state "$NGINX_SITE")"

printf '%s\nserver { listen 80; }\n' "$NGINX_SITE_MARKER" >"$NGINX_SITE"
assert_eq "find_legacy_site finds a marked EL site" "$NGINX_SITE" "$(find_legacy_site)"
: >"$legacy_dir/calls"
(uninstall_legacy_site) 2>/dev/null
assert_eq "uninstall_legacy_site renames a marked EL site to .disabled" "gone|present" \
    "$(path_state "$NGINX_SITE")|$(path_state "$NGINX_SITE.disabled")"
assert_contains "uninstall_legacy_site reloads nginx after disabling" "$(cat "$legacy_dir/calls")" "systemctl reload nginx"

assert_eq "find_legacy_site finds a marked .disabled EL site" "$NGINX_SITE.disabled" "$(find_legacy_site)"
: >"$legacy_dir/calls"
journal_before=$(cat "$legacy_dir/journal" 2>/dev/null)
(uninstall_legacy_site) 2>/dev/null
assert_eq "uninstall_legacy_site leaves an already disabled site alone (no systemctl)" "" "$(cat "$legacy_dir/calls")"
assert_eq "uninstall_legacy_site keeps the .disabled file" present "$(path_state "$NGINX_SITE.disabled")"
assert_eq "uninstall_legacy_site does not attempt to re-disable an already disabled site" \
    "$journal_before" "$(cat "$legacy_dir/journal" 2>/dev/null)"

# shellcheck disable=SC2034 # прочитаны apply_platform_paths, определённой в сорсимом файле
HOST_FAMILY=debian
# shellcheck disable=SC2034 # прочитан apply_platform_paths, определённой в сорсимом файле
EL_MAJOR=""
apply_platform_paths
NGINX_SITE="$legacy_dir/sites-available/gotcha"
NGINX_SITE_ENABLED_LINK="$legacy_dir/sites-enabled/gotcha"
mkdir -p "$legacy_dir/sites-available" "$legacy_dir/sites-enabled"
printf '%s\nserver { listen 80; }\n' "$NGINX_SITE_MARKER" >"$NGINX_SITE"
ln -s "$NGINX_SITE" "$NGINX_SITE_ENABLED_LINK"
assert_eq "find_legacy_site follows the Debian symlink" "$(readlink -f "$NGINX_SITE")" "$(find_legacy_site)"
: >"$legacy_dir/calls"
(uninstall_legacy_site) 2>/dev/null
assert_eq "uninstall_legacy_site removes the Debian symlink, keeps the file" "gone|present" \
    "$(path_state "$NGINX_SITE_ENABLED_LINK")|$(path_state "$NGINX_SITE")"
assert_contains "uninstall_legacy_site reloads nginx on Debian" "$(cat "$legacy_dir/calls")" "systemctl reload nginx"

: >"$legacy_dir/calls"
(uninstall_legacy_site) 2>/dev/null
assert_eq "uninstall_legacy_site with no symlink left does nothing on Debian" "" "$(cat "$legacy_dir/calls")"

printf 'server { listen 80; }\n' >"$legacy_dir/sites-available/own"
ln -s "$legacy_dir/sites-available/own" "$NGINX_SITE_ENABLED_LINK"
rm -f "$NGINX_SITE"
find_legacy_site >/dev/null
assert_eq "find_legacy_site ignores an operator's own unmarked Debian site" 1 $?
: >"$legacy_dir/calls"
(uninstall_legacy_site) 2>/dev/null
assert_eq "uninstall_legacy_site leaves an operator's own Debian symlink alone" "present|" \
    "$(path_state "$NGINX_SITE_ENABLED_LINK")|$(cat "$legacy_dir/calls")"

unset -f systemctl path_state
rm -rf "$legacy_dir"
apply_platform_paths

if [ "$FAILURES" -gt 0 ]; then
    printf '%d assertion(s) failed\n' "$FAILURES" >&2
    exit 1
fi
printf 'install-bare-metal unit tests: all assertions passed\n'
