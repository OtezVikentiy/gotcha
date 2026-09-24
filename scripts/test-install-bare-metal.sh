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

# env_get

envdir=$(mktemp -d)
envf="$envdir/gotcha.env"
printf 'GOTCHA_BASE_URL=https://first.example\n#GOTCHA_BASE_URL=https://commented.example\nGOTCHA_BASE_URL=https://last.example\n' >"$envf"
assert_eq "env_get takes the last occurrence and skips comments" "https://last.example" "$(env_get GOTCHA_BASE_URL "$envf")"
printf 'GOTCHA_BASE_URL="https://dq.example"\n' >"$envf"
assert_eq "env_get strips paired double quotes" "https://dq.example" "$(env_get GOTCHA_BASE_URL "$envf")"
printf "GOTCHA_BASE_URL='https://sq.example'\n" >"$envf"
assert_eq "env_get strips paired single quotes" "https://sq.example" "$(env_get GOTCHA_BASE_URL "$envf")"
printf 'GOTCHA_BASE_URL=https://crlf.example\r\n' >"$envf"
assert_eq "env_get drops a CRLF line ending (RF-1)" "https://crlf.example" "$(env_get GOTCHA_BASE_URL "$envf")"
printf 'GOTCHA_BASE_URL=https://ws.example  \n' >"$envf"
assert_eq "env_get drops trailing whitespace (RF-1)" "https://ws.example" "$(env_get GOTCHA_BASE_URL "$envf")"
printf 'GOTCHA_BASE_URL=https://slash.example/\n' >"$envf"
assert_eq "env_get returns the raw value, normalizing is the caller's job" "https://slash.example/" "$(env_get GOTCHA_BASE_URL "$envf")"
printf 'GOTCHA_BASE_URL_EXTRA=x\n' >"$envf"
env_get GOTCHA_BASE_URL "$envf" >/dev/null
assert_eq "env_get does not match a longer key with the same prefix" 1 $?
printf 'GOTCHA_TRUSTED_PROXIES=\n' >"$envf"
out=$(env_get GOTCHA_TRUSTED_PROXIES "$envf")
assert_eq "env_get reports an empty value as present" "0|" "$?|$out"
env_get GOTCHA_BASE_URL "$envdir/missing.env" >/dev/null
assert_eq "env_get on a missing file" 1 $?

# resolve_base_url — stdin_is_tty подменяется, как ss выше

# shellcheck disable=SC2317 # вызывается сорсимым файлом, а не отсюда
stdin_is_tty() { [ -n "$STUB_TTY" ]; }
missing="$envdir/none.env"

printf 'GOTCHA_BASE_URL=https://env.example\n' >"$envf"
out=$( (resolve_base_url "https://flag.example" "$envf" "") 2>/dev/null )
assert_eq "resolve_base_url: the flag wins over env" "https://flag.example" "$out"
printf 'GOTCHA_BASE_URL=https://env.example/\n' >"$envf"
out=$( (resolve_base_url "" "$envf" "") 2>/dev/null )
assert_eq "resolve_base_url: env is used and normalized when no flag" "https://env.example" "$out"

printf 'GOTCHA_SECRET_KEY=x\n' >"$envf"
out=$( (resolve_base_url "" "$envf" 1) 2>&1 )
rc=$?
assert_eq "resolve_base_url: env without GOTCHA_BASE_URL and no flag is refused (RF-2)" 2 "$rc"
assert_contains "resolve_base_url: the refusal names --base-url (RF-2)" "$out" "--base-url"

STUB_TTY=""
out=$( (resolve_base_url "" "$missing" 1) 2>&1 )
rc=$?
assert_eq "resolve_base_url: --yes without an address on a clean host is refused" 2 "$rc"
assert_contains "resolve_base_url: the refusal text" "$out" \
    "install-bare-metal: --base-url is required for a new installation (the address users will type in the browser, e.g. https://gotcha.example.com)"
out=$( (resolve_base_url "" "$missing" "") 2>&1 </dev/null )
assert_eq "resolve_base_url: no terminal and no address is refused" 2 $?

STUB_TTY=1
out=$( (resolve_base_url "" "$missing" 1 <<<"https://ok.example") 2>/dev/null )
assert_eq "resolve_base_url: --yes beats a terminal, nothing is read" "2|" "$?|$out"
out=$( (resolve_base_url "" "$missing" "" <<<$'\nftp://x\nhttps://ok.example/') 2>"$envdir/err" )
assert_eq "resolve_base_url: empty and invalid answers are asked again" "https://ok.example" "$out"
assert_contains "resolve_base_url: an invalid answer explains why" "$(cat "$envdir/err")" "invalid address 'ftp://x'"
out=$( (resolve_base_url "" "$missing" "" </dev/null) 2>&1 )
assert_eq "resolve_base_url: EOF on the question is refused" 2 $?
STUB_TTY=""
unset -f stdin_is_tty

# plain_http_warning

out=$(plain_http_warning "http://10.0.0.5")
assert_eq "plain_http_warning fires for http://" 0 $?
assert_contains "plain_http_warning mentions session cookies" "$out" "session cookies"
plain_http_warning "https://x.example" >/dev/null
assert_eq "plain_http_warning is silent for https://" 1 $?
rm -rf "$envdir"

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
    GOTCHA_DIST_DIR GOMEMLIMIT GOTCHA_LISTEN_ADDR GOTCHA_TRUSTED_PROXIES; do
    assert_contains "render_env_file contains $var" "$env_file" "$var="
done
assert_contains "render_env_file trusts the local reverse proxy" "$env_file" \
    $'\nGOTCHA_TRUSTED_PROXIES=127.0.0.1/32,::1/128'

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
journal_before=$(cat "$legacy_dir/journal" 2>/dev/null)
(uninstall_legacy_site) 2>/dev/null
assert_eq "uninstall_legacy_site leaves an unmarked EL site alone (no systemctl)" "" "$(cat "$legacy_dir/calls")"
assert_eq "uninstall_legacy_site does not rename an unmarked EL site" present "$(path_state "$NGINX_SITE")"
assert_eq "uninstall_legacy_site logs nothing for an unmarked EL site" \
    "$journal_before" "$(cat "$legacy_dir/journal" 2>/dev/null)"

printf '%s\nserver { listen 80; }\n' "$NGINX_SITE_MARKER" >"$NGINX_SITE"
assert_eq "find_legacy_site finds a marked EL site" "$NGINX_SITE" "$(find_legacy_site)"
: >"$legacy_dir/calls"
(uninstall_legacy_site) 2>/dev/null
assert_eq "uninstall_legacy_site renames a marked EL site to .disabled" "gone|present" \
    "$(path_state "$NGINX_SITE")|$(path_state "$NGINX_SITE.disabled")"
assert_contains "uninstall_legacy_site reloads nginx after disabling" "$(cat "$legacy_dir/calls")" "systemctl reload nginx"
assert_contains "uninstall_legacy_site logs the disabled-site step" "$(cat "$legacy_dir/journal")" \
    "nginx site from a previous version disabled"
assert_contains "uninstall_legacy_site logs the SELinux/firewalld disclaimer" "$(cat "$legacy_dir/journal")" \
    "SELinux boolean httpd_can_network_connect"

assert_eq "find_legacy_site finds a marked .disabled EL site" "$NGINX_SITE.disabled" "$(find_legacy_site)"
: >"$legacy_dir/calls"
journal_before=$(cat "$legacy_dir/journal" 2>/dev/null)
(uninstall_legacy_site) 2>/dev/null
assert_eq "uninstall_legacy_site leaves an already disabled site alone (no systemctl)" "" "$(cat "$legacy_dir/calls")"
assert_eq "uninstall_legacy_site keeps the .disabled file" present "$(path_state "$NGINX_SITE.disabled")"
assert_eq "uninstall_legacy_site does not attempt to re-disable an already disabled site" \
    "$journal_before" "$(cat "$legacy_dir/journal" 2>/dev/null)"

rm -f "$NGINX_SITE.disabled"
printf '%s\nserver { listen 80; }\n' "$NGINX_SITE_MARKER" >"$NGINX_SITE"
: >"$legacy_dir/calls"
chmod 555 "$legacy_dir/conf.d"
(uninstall_legacy_site) 2>/dev/null
rc=$?
chmod 755 "$legacy_dir/conf.d"
assert_eq "uninstall_legacy_site returns 0 even when mv fails" 0 "$rc"
assert_contains "uninstall_legacy_site logs a WARNING when mv fails" "$(cat "$legacy_dir/journal")" \
    "WARNING: could not disable the nginx site"
assert_eq "uninstall_legacy_site does not reload nginx when mv fails" "" "$(cat "$legacy_dir/calls")"
assert_eq "uninstall_legacy_site leaves the site in place when mv fails" present "$(path_state "$NGINX_SITE")"

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

# Симлинк на размеченный файл в ДРУГОМ месте, а не на сам $NGINX_SITE — без
# readlink-кандидата find_legacy_site его не увидит вовсе (NGINX_SITE отсутствует).
rm -f "$NGINX_SITE"
mkdir -p "$legacy_dir/elsewhere"
printf '%s\nserver { listen 80; }\n' "$NGINX_SITE_MARKER" >"$legacy_dir/elsewhere/gotcha.conf"
ln -sf "$legacy_dir/elsewhere/gotcha.conf" "$NGINX_SITE_ENABLED_LINK"
assert_eq "find_legacy_site follows a Debian symlink to a marked file elsewhere" \
    "$legacy_dir/elsewhere/gotcha.conf" "$(find_legacy_site)"
: >"$legacy_dir/calls"
(uninstall_legacy_site) 2>/dev/null
assert_eq "uninstall_legacy_site removes a Debian symlink pointing elsewhere, keeps its target" "gone|present" \
    "$(path_state "$NGINX_SITE_ENABLED_LINK")|$(path_state "$legacy_dir/elsewhere/gotcha.conf")"
assert_contains "uninstall_legacy_site reloads nginx for a symlink pointing elsewhere" "$(cat "$legacy_dir/calls")" "systemctl reload nginx"

: >"$legacy_dir/calls"
(uninstall_legacy_site) 2>/dev/null
assert_eq "uninstall_legacy_site with no symlink left does nothing on Debian" "" "$(cat "$legacy_dir/calls")"

printf 'server { listen 80; }\n' >"$legacy_dir/sites-available/own"
ln -s "$legacy_dir/sites-available/own" "$NGINX_SITE_ENABLED_LINK"
rm -f "$NGINX_SITE"
find_legacy_site >/dev/null
assert_eq "find_legacy_site ignores an operator's own unmarked Debian site" 1 $?
: >"$legacy_dir/calls"
journal_before=$(cat "$legacy_dir/journal" 2>/dev/null)
(uninstall_legacy_site) 2>/dev/null
assert_eq "uninstall_legacy_site leaves an operator's own Debian symlink alone" "present|" \
    "$(path_state "$NGINX_SITE_ENABLED_LINK")|$(cat "$legacy_dir/calls")"
assert_eq "uninstall_legacy_site logs nothing for an operator's own Debian site" \
    "$journal_before" "$(cat "$legacy_dir/journal" 2>/dev/null)"

unset -f systemctl path_state
rm -rf "$legacy_dir"
apply_platform_paths

# env_set / reconcile_env_file

envdir=$(mktemp -d)
envf="$envdir/gotcha.env"
# shellcheck disable=SC2034 # читают env_set/reconcile_env_file из сорсимого файла
ENV_FILE_OWNER="$(id -un):$(id -gn)"
# shellcheck disable=SC2034 # читает log_step из сорсимого файла
INSTALL_JOURNAL="$envdir/journal"

printf 'A=1\nGOTCHA_BASE_URL=https://old.example\n# comment\n#GOTCHA_BASE_URL=https://commented\nGOTCHA_BASE_URL=https://dup.example\nB=two words\n' >"$envf"
env_set GOTCHA_BASE_URL https://new.example "$envf"
assert_eq "env_set rc" 0 $?
assert_eq "env_set collapses every occurrence into one line at the first one's place, leaving a commented-out key as-is" \
    "A=1
GOTCHA_BASE_URL=https://new.example
# comment
#GOTCHA_BASE_URL=https://commented
B=two words" "$(cat "$envf")"
assert_eq "env_set leaves the file 0640" 640 "$(stat -c '%a' "$envf")"
env_set NEW_KEY v "$envf"
assert_eq "env_set appends a missing key at the end" "NEW_KEY=v" "$(tail -n1 "$envf")"
env_set GOTCHA_BASE_URL 'https://x/%7e~a:b,c' "$envf"
assert_eq "env_set writes % ~ : / , literally" "https://x/%7e~a:b,c" "$(env_get GOTCHA_BASE_URL "$envf")"
env_set GOTCHA_BASE_URL 'https://x/a&b\c#d' "$envf"
assert_eq "env_set writes & \\ # literally (no sed substitution)" 'https://x/a&b\c#d' "$(env_get GOTCHA_BASE_URL "$envf")"
before=$(cat "$envf")
env_set GOTCHA_BASE_URL 'https://x/a&b\c#d' "$envf"
assert_eq "env_set with the same value keeps the file byte-for-byte" "$before" "$(cat "$envf")"

before=$(cat "$envf")
ENV_FILE_OWNER="nosuchuser-gotcha:nosuchgroup-gotcha"
env_set GOTCHA_BASE_URL https://fail.example "$envf" 2>/dev/null
assert_eq "env_set reports a chown failure" 1 $?
assert_eq "env_set leaves the original intact on failure" "$before" "$(cat "$envf")"
assert_eq "env_set leaves no temp file on failure" "" "$(find "$envdir" -name '.gotcha.env.*')"
# shellcheck disable=SC2034 # читает env_set из сорсимого файла
ENV_FILE_OWNER="$(id -un):$(id -gn)"

printf 'GOTCHA_SECRET_KEY=s3cret\nGOTCHA_BASE_URL=https://same.example/\nGOTCHA_TRUSTED_PROXIES=10.0.0.1\n' >"$envf"
before=$(cat "$envf")
ENV_CHANGED=""
(reconcile_env_file "$envf" https://same.example) 2>/dev/null
assert_eq "reconcile: same address modulo trailing slash changes nothing" "$before" "$(cat "$envf")"
ENV_CHANGED=""
reconcile_env_file "$envf" https://same.example 2>/dev/null
assert_eq "reconcile: nothing changed, no restart" "" "$ENV_CHANGED"

out=$(reconcile_env_file "$envf" https://new.example 2>&1; printf '|%s' "$ENV_CHANGED")
assert_contains "reconcile logs the address change" "$out" \
    "GOTCHA_BASE_URL changed: https://same.example -> https://new.example"
assert_contains "reconcile reminds to update the reverse proxy" "$out" "update your reverse proxy"
assert_contains "reconcile asks for a restart after an address change" "$out" "|1"
assert_eq "reconcile writes the new address once" 1 "$(grep -c '^GOTCHA_BASE_URL=' "$envf")"
assert_eq "reconcile never touches the secret" "GOTCHA_SECRET_KEY=s3cret" "$(grep '^GOTCHA_SECRET_KEY=' "$envf")"
assert_eq "reconcile keeps an operator's own GOTCHA_TRUSTED_PROXIES" "10.0.0.1" "$(env_get GOTCHA_TRUSTED_PROXIES "$envf")"

printf 'GOTCHA_BASE_URL=https://a.example\nGOTCHA_TRUSTED_PROXIES=\n' >"$envf"
before=$(cat "$envf")
reconcile_env_file "$envf" "" 2>/dev/null
assert_eq "reconcile keeps an empty GOTCHA_TRUSTED_PROXIES as the operator's choice (RF-3)" "$before" "$(cat "$envf")"

printf 'GOTCHA_BASE_URL=https://a.example\n' >"$envf"
out=$(reconcile_env_file "$envf" "" 2>&1; printf '|%s' "$ENV_CHANGED")
assert_eq "reconcile adds GOTCHA_TRUSTED_PROXIES to a 1.8 env" "127.0.0.1/32,::1/128" "$(env_get GOTCHA_TRUSTED_PROXIES "$envf")"
assert_contains "reconcile logs the added key" "$out" "GOTCHA_TRUSTED_PROXIES=127.0.0.1/32,::1/128 added"
assert_contains "reconcile asks for a restart after adding the key" "$out" "|1"

printf 'GOTCHA_SECRET_KEY=s3cret\n' >"$envf"
reconcile_env_file "$envf" https://flag.example 2>/dev/null
assert_eq "reconcile appends GOTCHA_BASE_URL to an env that lacks it (RF-2)" "https://flag.example" "$(env_get GOTCHA_BASE_URL "$envf")"

printf 'GOTCHA_BASE_URL=https://old-b.example\nGOTCHA_TRUSTED_PROXIES=10.0.0.1\n' >"$envf"
before=$(cat "$envf")
ENV_FILE_OWNER="nosuchuser-gotcha:nosuchgroup-gotcha"
out=$(reconcile_env_file "$envf" https://fail-b.example 2>&1)
assert_eq "reconcile: failure to update GOTCHA_BASE_URL exits 1" 1 $?
assert_contains "reconcile: failure to update GOTCHA_BASE_URL reports it" "$out" "failed to update GOTCHA_BASE_URL"
assert_eq "reconcile: failure to update GOTCHA_BASE_URL leaves the file untouched" "$before" "$(cat "$envf")"
# shellcheck disable=SC2034 # читает env_set из сорсимого файла
ENV_FILE_OWNER="$(id -un):$(id -gn)"

printf 'GOTCHA_BASE_URL=https://c.example\n' >"$envf"
before=$(cat "$envf")
ENV_FILE_OWNER="nosuchuser-gotcha:nosuchgroup-gotcha"
out=$(reconcile_env_file "$envf" "" 2>&1)
assert_eq "reconcile: failure to add GOTCHA_TRUSTED_PROXIES exits 1" 1 $?
assert_contains "reconcile: failure to add GOTCHA_TRUSTED_PROXIES reports it" "$out" "failed to add GOTCHA_TRUSTED_PROXIES"
assert_eq "reconcile: failure to add GOTCHA_TRUSTED_PROXIES leaves the file untouched" "$before" "$(cat "$envf")"
# shellcheck disable=SC2034 # читает env_set из сорсимого файла
ENV_FILE_OWNER="$(id -un):$(id -gn)"

rm -rf "$envdir"

# legacy_site_enable_hint / render_summary

sumdir=$(mktemp -d)
# shellcheck disable=SC2034
HOST_FAMILY=rhel
# shellcheck disable=SC2034
EL_MAJOR=9
apply_platform_paths
NGINX_SITE="$sumdir/conf.d/gotcha.conf"
mkdir -p "$sumdir/conf.d"
assert_eq "EL .disabled site: the hint moves it back and reloads" \
    "mv $NGINX_SITE.disabled $NGINX_SITE && systemctl reload nginx" \
    "$(legacy_site_enable_hint "$NGINX_SITE.disabled")"
legacy_site_enable_hint "$NGINX_SITE" >/dev/null
assert_eq "EL active site: no hint" 1 $?

printf 'server { listen 80; }\n' >"$NGINX_SITE"
legacy_site_enable_hint "$NGINX_SITE.disabled" >/dev/null
assert_eq "EL .disabled site with the operator's own config in place: no hint, no mv (I-1)" 1 $?
rm -f "$NGINX_SITE"

# shellcheck disable=SC2034
HOST_FAMILY=debian
# shellcheck disable=SC2034
EL_MAJOR=""
apply_platform_paths
NGINX_SITE="$sumdir/sites-available/gotcha"
NGINX_SITE_ENABLED_LINK="$sumdir/sites-enabled/gotcha"
mkdir -p "$sumdir/sites-available" "$sumdir/sites-enabled"
: >"$NGINX_SITE"
assert_eq "Debian site without a symlink: the hint links it (RF-5)" \
    "ln -s $NGINX_SITE $NGINX_SITE_ENABLED_LINK && systemctl reload nginx" \
    "$(legacy_site_enable_hint "$NGINX_SITE")"
ln -s "$NGINX_SITE" "$NGINX_SITE_ENABLED_LINK"
legacy_site_enable_hint "$NGINX_SITE" >/dev/null
assert_eq "Debian site with its symlink: no hint" 1 $?

rm -f "$NGINX_SITE_ENABLED_LINK"
ln -s "$sumdir/sites-available/does-not-exist" "$NGINX_SITE_ENABLED_LINK"
legacy_site_enable_hint "$NGINX_SITE" >/dev/null
assert_eq "Debian dangling symlink in sites-enabled: no hint, would collide with ln -s (I-4)" 1 $?
rm -f "$NGINX_SITE" "$NGINX_SITE_ENABLED_LINK"

: >"$NGINX_SITE.disabled"
assert_eq "Debian .disabled site: the hint moves it back and links it (Minor-6)" \
    "mv $NGINX_SITE.disabled $NGINX_SITE && ln -s $NGINX_SITE $NGINX_SITE_ENABLED_LINK && systemctl reload nginx" \
    "$(legacy_site_enable_hint "$NGINX_SITE.disabled")"

: >"$NGINX_SITE"
legacy_site_enable_hint "$NGINX_SITE.disabled" >/dev/null
assert_eq "Debian .disabled site with the operator's own config in place: no hint, no mv (I-1)" 1 $?
rm -f "$NGINX_SITE" "$NGINX_SITE.disabled"

rm -rf "$sumdir"
apply_platform_paths

assert_eq "readyz_probe_addr normalizes :PORT to loopback (I-2)" "127.0.0.1:8080" "$(readyz_probe_addr :8080)"
assert_eq "readyz_probe_addr normalizes 0.0.0.0:PORT to loopback (I-2)" "127.0.0.1:8080" "$(readyz_probe_addr 0.0.0.0:8080)"
assert_eq "readyz_probe_addr leaves a non-loopback address as is (I-2)" "10.0.0.5:8080" "$(readyz_probe_addr 10.0.0.5:8080)"
assert_eq "readyz_probe_addr leaves an already-loopback address as is (I-2)" "127.0.0.1:8080" "$(readyz_probe_addr 127.0.0.1:8080)"

assert_eq "summary_effective_version prefers the installed binary's version (I-2)" "1.9.0" \
    "$(summary_effective_version 1.9.0 1.9.1)"
assert_eq "summary_effective_version falls back to --version when nothing is installed (I-2)" "1.9.1" \
    "$(summary_effective_version "" 1.9.1)"

assert_eq "summary_is_fresh: no prior env means a fresh install (I-2)" 1 "$(summary_is_fresh "")"
assert_eq "summary_is_fresh: an env that already existed is not fresh (I-2)" "" "$(summary_is_fresh 1)"

out=$(render_summary 1.9.0 '{"status":"ready"}' https://gotcha.example.com 127.0.0.1:8080 "" "" 1)
assert_eq "render_summary, fresh host" \
"Gotcha 1.9.0 is installed and running.
  readiness:  {\"status\":\"ready\"}
  listens on: 127.0.0.1:8080 (this host only)
  address:    https://gotcha.example.com (GOTCHA_BASE_URL)
  config:     /etc/gotcha/gotcha.env
  logs:       journalctl -u gotcha -f

Next:
  1. Put a reverse proxy (nginx, angie, Apache, Caddy...) in front of 127.0.0.1:8080
     so that https://gotcha.example.com reaches it. Requirements and examples:
     https://getgotcha.ru/docs/installation-bare-metal/
  2. Open https://gotcha.example.com and create the first administrator." "$out"

out=$(render_summary 1.9.0 ok https://x.example 127.0.0.1:8080 /etc/nginx/conf.d/gotcha.conf "" "")
assert_contains "render_summary names the kept legacy site" "$out" \
    "  1. Your nginx site from a previous version is kept as is and is yours to maintain: /etc/nginx/conf.d/gotcha.conf"
case "$out" in
    *"Put a reverse proxy"*) printf 'FAIL: render_summary asks for a new proxy on a legacy host\n' >&2; FAILURES=$((FAILURES + 1)) ;;
esac
assert_eq "render_summary, legacy site already enabled: whole output, no 'not enabled' line (I-3)" \
"Gotcha 1.9.0 is installed and running.
  readiness:  ok
  listens on: 127.0.0.1:8080 (this host only)
  address:    https://x.example (GOTCHA_BASE_URL)
  config:     /etc/gotcha/gotcha.env
  logs:       journalctl -u gotcha -f

Next:
  1. Your nginx site from a previous version is kept as is and is yours to maintain: /etc/nginx/conf.d/gotcha.conf" "$out"
out=$(render_summary 1.9.0 ok https://x.example 127.0.0.1:8080 /etc/nginx/conf.d/gotcha.conf.disabled "mv a b && systemctl reload nginx" "")
assert_contains "render_summary gives the enable command for a disabled legacy site" "$out" \
    "     It is not enabled now; to enable it: mv a b && systemctl reload nginx"
out=$(render_summary 1.9.0 "no answer (see logs)" https://x.example 127.0.0.1:8080 "" "" 1)
assert_contains "render_summary shows a failed readiness probe as is" "$out" "  readiness:  no answer (see logs)"

out=$(render_summary 1.9.0 ok https://x.example 127.0.0.1:8080 "" "" "")
case "$out" in
    *"first administrator"*) printf 'FAIL: render_summary asks to create the first administrator on a re-run\n' >&2; FAILURES=$((FAILURES + 1)) ;;
esac
out=$(render_summary 1.9.0 ok https://x.example 10.0.0.5:8080 "" "" "")
assert_contains "render_summary shows a non-loopback listen address from env" "$out" \
    "  listens on: 10.0.0.5:8080 (reachable from other hosts: allow only your proxy)"
case "$out" in
    *"this host only"*) printf 'FAIL: render_summary claims loopback for 10.0.0.5:8080\n' >&2; FAILURES=$((FAILURES + 1)) ;;
esac

out=$(render_summary 1.9.0 ok https://x.example localhost:8080 "" "" "")
assert_contains "render_summary treats localhost:PORT as loopback (I-4)" "$out" \
    "  listens on: localhost:8080 (this host only)"
out=$(render_summary 1.9.0 ok https://x.example '[::1]:8080' "" "" "")
assert_contains "render_summary treats [::1]:PORT as loopback (I-4)" "$out" \
    "  listens on: [::1]:8080 (this host only)"

out=$(render_summary 1.9.0 ok https://x.example :8080 "" "" "")
assert_contains "render_summary points the proxy hint at the loopback probe address for :PORT (Minor-5)" "$out" \
    "  1. Put a reverse proxy (nginx, angie, Apache, Caddy...) in front of 127.0.0.1:8080"
out=$(render_summary 1.9.0 ok https://x.example 0.0.0.0:8080 "" "" "")
assert_contains "render_summary points the proxy hint at the loopback probe address for 0.0.0.0:PORT (Minor-5)" "$out" \
    "  1. Put a reverse proxy (nginx, angie, Apache, Caddy...) in front of 127.0.0.1:8080"

# preflight: порядок и доставка утилит

for hint_family in debian rhel; do
    # shellcheck disable=SC2034 # читает apply_platform_paths/required_commands из сорсимого файла
    HOST_FAMILY="$hint_family"
    # shellcheck disable=SC2034
    EL_MAJOR=""
    [ "$hint_family" = rhel ] && EL_MAJOR=9
    apply_platform_paths
    while IFS= read -r hint_cmd; do
        assert_eq "PKG_HINTS has a hint for $hint_cmd on $hint_family" "has-hint" \
            "$([ -n "${PKG_HINTS[$hint_cmd]:-}" ] && printf has-hint || printf missing-hint)"
    done < <(required_commands "$hint_family" "")
done

pfdir=$(mktemp -d)
# shellcheck disable=SC2034 # читает log_step из сорсимого файла
INSTALL_JOURNAL="$pfdir/journal"
# shellcheck disable=SC2317 # подмены вызываются сорсимым файлом, а не отсюда
have_command() { ! grep -qx "$1" "$pfdir/missing"; }
# shellcheck disable=SC2317
pkg_install() {
    [ -z "$STUB_INSTALL_FAILS" ] || return 1
    printf '%s\n' "$@" >>"$pfdir/installed"
    [ -z "$STUB_DELIVERY_FIXES" ] || : >"$pfdir/missing"
}
# shellcheck disable=SC2317
pkg_refresh() {
    [ -z "$STUB_REFRESH_FAILS" ] || return 1
    printf 'refresh\n' >>"$pfdir/installed"
}
STUB_INSTALL_FAILS=""
STUB_REFRESH_FAILS=""

# shellcheck disable=SC2034
HOST_FAMILY=rhel
# shellcheck disable=SC2034
EL_MAJOR=9
apply_platform_paths
# shellcheck disable=SC2034 # читает preflight_prerequisites/preflight_ports из сорсимого файла
ARG_SKIP_DATABASES=""
ARG_DRY_RUN=""

printf 'tar\nrunuser\n' >"$pfdir/missing"; : >"$pfdir/installed"; : >"$pfdir/journal"
STUB_DELIVERY_FIXES=1
out=$( (preflight_prerequisites) 2>&1 )
assert_eq "delivery succeeds on rhel" 0 $?
assert_eq "rhel delivers the packages of the missing commands, one argument each" "tar
util-linux" "$(cat "$pfdir/installed")"
assert_contains "delivery announces progress before installing (M-5)" "$out" "install-bare-metal: installing missing prerequisites: tar util-linux"
assert_contains "delivery is logged as completed on one line" "$out" "install-bare-metal: installed missing prerequisites: tar util-linux"
case "$out" in
    *"installing missing prerequisites: tar util-linux"*"installed missing prerequisites: tar util-linux"*) order=ordered ;;
    *) order=unordered ;;
esac
assert_eq "the progress notice comes before the completion log (M-5)" "ordered" "$order"
assert_eq "a successful delivery adds exactly one journal line, the completion (I-5)" \
    "installed missing prerequisites: tar util-linux" \
    "$(sed -E 's/^[^ ]+ \[[^]]*\] //' "$pfdir/journal")"

printf 'tar\nrunuser\n' >"$pfdir/missing"; : >"$pfdir/installed"
out=$( (IFS=$'\n\t'; preflight_prerequisites) 2>&1 )
assert_eq "under main's IFS pkg_install still gets separate arguments (RF-4)" "tar
util-linux" "$(cat "$pfdir/installed")"
assert_contains "under main's IFS the log line stays single-line (RF-4)" "$out" \
    "install-bare-metal: installed missing prerequisites: tar util-linux"

printf 'rpm\ntar\n' >"$pfdir/missing"; : >"$pfdir/installed"
out=$( (preflight_prerequisites) 2>&1 )
assert_eq "a missing rpm is a hard refusal" 3 $?
assert_contains "the rpm refusal names the package" "$out" "rpm is required (RHEL-family package: rpm)"
assert_eq "nothing is installed when rpm is missing" "" "$(cat "$pfdir/installed")"

printf 'dnf\ntar\n' >"$pfdir/missing"; : >"$pfdir/installed"
out=$( (preflight_prerequisites) 2>&1 )
assert_eq "a missing dnf is a hard refusal" 3 $?
assert_contains "the dnf refusal names the package" "$out" "dnf is required (RHEL-family package: dnf)"
assert_eq "nothing is installed when dnf is missing" "" "$(cat "$pfdir/installed")"

printf 'tar\n' >"$pfdir/missing"; : >"$pfdir/installed"
STUB_DELIVERY_FIXES=""
out=$( (preflight_prerequisites) 2>&1 )
assert_eq "delivery that does not provide the command still refuses" 3 $?
assert_contains "the post-delivery refusal keeps the old text" "$out" "tar is required (RHEL-family package: tar)"

printf 'tar\n' >"$pfdir/missing"; : >"$pfdir/installed"; : >"$pfdir/journal"
STUB_INSTALL_FAILS=1
out=$( (set -e; preflight_prerequisites) 2>&1 )
assert_eq "a failing package install still refuses through the intended exit code, not set -e's" 3 $?
assert_contains "the install failure is logged as a warning" "$out" "WARNING: could not install: tar"
assert_contains "the install failure still refuses with the missing-command message" "$out" "tar is required (RHEL-family package: tar)"
assert_contains "a failing delivery still announces progress up front (M-5)" "$out" "install-bare-metal: installing missing prerequisites: tar"
assert_eq "a failed delivery never reaches stdout/stderr as completed (I-5)" "not-logged" \
    "$(case "$out" in *'installed missing prerequisites'*) printf logged ;; *) printf not-logged ;; esac)"
assert_eq "a failed delivery adds only the warning to the journal, no delivery progress or completion line (I-5)" \
    "WARNING: could not install: tar" \
    "$(sed -E 's/^[^ ]+ \[[^]]*\] //' "$pfdir/journal")"
STUB_INSTALL_FAILS=""

printf 'tar\n' >"$pfdir/missing"; : >"$pfdir/installed"
ARG_DRY_RUN=1
out=$( (preflight_prerequisites) 2>&1 )
assert_eq "dry-run does not refuse over a missing command" 0 $?
assert_contains "dry-run names what it would install" "$out" "[dry-run] would install: tar"
assert_eq "dry-run installs nothing" "" "$(cat "$pfdir/installed")"
ARG_DRY_RUN=""

: >"$pfdir/missing"; : >"$pfdir/installed"
out=$( (preflight_prerequisites) 2>&1 )
assert_eq "nothing missing: nothing installed, nothing logged" "|" "$(cat "$pfdir/installed")|$out"

# shellcheck disable=SC2034
HOST_FAMILY=debian
# shellcheck disable=SC2034
EL_MAJOR=""
apply_platform_paths
printf 'ss\nsha256sum\nrunuser\n' >"$pfdir/missing"; : >"$pfdir/installed"
STUB_DELIVERY_FIXES=1
(preflight_prerequisites) >/dev/null 2>&1
assert_eq "debian refreshes indexes first, then installs packages in required_commands order" "refresh
coreutils
iproute2
util-linux" "$(cat "$pfdir/installed")"

printf 'ss\nsha256sum\nrunuser\n' >"$pfdir/missing"; : >"$pfdir/installed"
STUB_DELIVERY_FIXES=1
STUB_REFRESH_FAILS=1
out=$( (set -e; preflight_prerequisites) 2>&1 )
assert_eq "a failing apt-get update does not block delivery" 0 $?
assert_contains "the refresh failure is logged as a warning" "$out" "WARNING: apt-get update failed before installing: coreutils iproute2 util-linux"
assert_eq "delivery still runs after a failed refresh" "coreutils
iproute2
util-linux" "$(cat "$pfdir/installed")"
STUB_REFRESH_FAILS=""

assert_eq "packages_for_commands dedupes" "coreutils" "$(packages_for_commands sha256sum sha256sum)"

printf 'ss\n' >"$pfdir/missing"
ARG_DRY_RUN=1
out=$( (preflight_ports) 2>&1 )
assert_contains "dry-run says the port check is skipped without ss" "$out" "[dry-run] port checks skipped: ss is missing"
ARG_DRY_RUN=""

# tarball_prereqs_missing (I-4)

: >"$pfdir/missing"
assert_eq "tarball_prereqs_missing: nothing missing when tar/sha256sum/curl are all present" "" \
    "$(tarball_prereqs_missing "")"

printf 'tar\n' >"$pfdir/missing"
assert_eq "tarball_prereqs_missing: a missing tar is reported" "tar" \
    "$(tarball_prereqs_missing "")"

printf 'sha256sum\n' >"$pfdir/missing"
assert_eq "tarball_prereqs_missing: a missing sha256sum is reported" "sha256sum" \
    "$(tarball_prereqs_missing "")"

printf 'curl\n' >"$pfdir/missing"
assert_eq "tarball_prereqs_missing: a missing curl is reported without --from-tarball" "curl" \
    "$(tarball_prereqs_missing "")"

printf 'curl\n' >"$pfdir/missing"
assert_eq "tarball_prereqs_missing: curl is not required with --from-tarball" "" \
    "$(tarball_prereqs_missing /tmp/whatever.tar.gz)"

printf 'tar\nsha256sum\ncurl\n' >"$pfdir/missing"
assert_eq "tarball_prereqs_missing: tar, sha256sum and curl reported in that order without --from-tarball" "tar
sha256sum
curl" "$(tarball_prereqs_missing "")"

printf 'tar\nsha256sum\ncurl\n' >"$pfdir/missing"
assert_eq "tarball_prereqs_missing: with --from-tarball only tar and sha256sum are reported" "tar
sha256sum" "$(tarball_prereqs_missing /tmp/whatever.tar.gz)"

: >"$pfdir/missing"
unset -f have_command pkg_install pkg_refresh

# shellcheck disable=SC2317
preflight_platform() { printf 'platform '; }
# shellcheck disable=SC2317
preflight_resources() { printf 'resources '; }
# shellcheck disable=SC2317
preflight_prerequisites() { printf 'prerequisites '; }
# shellcheck disable=SC2317
preflight_ports() { printf 'ports'; }
assert_eq "preflight checks resources before changing the host" "platform resources prerequisites ports" "$(preflight)"
rm -rf "$pfdir"

if [ "$FAILURES" -gt 0 ]; then
    printf '%d assertion(s) failed\n' "$FAILURES" >&2
    exit 1
fi
printf 'install-bare-metal unit tests: all assertions passed\n'
