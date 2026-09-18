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

detect_distro ubuntu ""
assert_eq "detect_distro ID=ubuntu accepted" 0 $?
detect_distro debian ""
assert_eq "detect_distro ID=debian accepted" 0 $?
detect_distro linuxmint ubuntu
assert_eq "detect_distro ID=linuxmint ID_LIKE=ubuntu accepted" 0 $?
detect_distro fedora ""
assert_eq "detect_distro ID=fedora rejected" 1 $?

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

parse_args --domain example.com --no-proxy >/dev/null 2>&1
assert_eq "parse_args --domain with --no-proxy rejected" 2 $?

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

parse_args --email a@b.example --from-tarball /tmp/x.tar.gz >/dev/null 2>&1
assert_eq "parse_args --email without --domain rejected" 2 $?

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

parse_args --domain example.com --email a@b.example --version 9.9.9 --dry-run >/dev/null 2>&1
assert_eq "parse_args accepts a full example" 0 $?
assert_eq "parse_args sets ARG_DOMAIN" example.com "$ARG_DOMAIN"
assert_eq "parse_args sets ARG_EMAIL" a@b.example "$ARG_EMAIL"
assert_eq "parse_args sets ARG_VERSION" 9.9.9 "$ARG_VERSION"
assert_eq "parse_args sets ARG_DRY_RUN" 1 "$ARG_DRY_RUN"

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

out=$(choose_base_url "https://explicit.example" "domain.example" "10.0.0.1")
assert_eq "choose_base_url prefers --base-url" "https://explicit.example" "$out"
out=$(choose_base_url "" "domain.example" "10.0.0.1")
assert_eq "choose_base_url falls back to --domain" "https://domain.example" "$out"
out=$(choose_base_url "" "" "10.0.0.1")
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
    "After=postgresql.service clickhouse-server.service network-online.target"; do
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

# render_nginx_site

site=$(render_nginx_site example.com)
assert_contains "render_nginx_site proxy_pass" "$site" "proxy_pass http://127.0.0.1:8080"
# literal nginx variable in the single-quoted needle below, must not expand
# shellcheck disable=SC2016
assert_contains "render_nginx_site forwards Host" "$site" 'proxy_set_header Host $host'
assert_contains "render_nginx_site forwards X-Forwarded-For" "$site" "X-Forwarded-For"
assert_contains "render_nginx_site forwards X-Forwarded-Proto" "$site" "X-Forwarded-Proto"
assert_contains "render_nginx_site sets client_max_body_size" "$site" "client_max_body_size"
for directive in \
    "location ~ ^/(metrics|version)$ {" \
    "allow 127.0.0.1;" \
    "allow ::1;" \
    "deny all;"; do
    assert_contains "render_nginx_site restricts /metrics and /version to loopback ($directive)" "$site" "$directive"
done

assert_contains "render_nginx_site marks the file as ours" "$site" "$NGINX_SITE_MARKER"

# verify_loopback_only — ветка отказа на живом хосте не воспроизводится, поэтому
# ss подменяется функцией; фактический bind проверяет e2e.

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

if [ "$FAILURES" -gt 0 ]; then
    printf '%d assertion(s) failed\n' "$FAILURES" >&2
    exit 1
fi
printf 'install-bare-metal unit tests: all assertions passed\n'
