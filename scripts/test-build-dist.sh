#!/usr/bin/env bash
# Собирает под архитектуру ХОСТА — иначе gotcha-бинарь из тарбола нельзя
# было бы запустить, чтобы проверить версию.
set -uo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
BUILD_DIST="$SCRIPT_DIR/build-dist.sh"

fail=0
assert_eq() { # want got сообщение
    if [ "$1" != "$2" ]; then printf 'FAIL: %s\n  want: %s\n  got:  %s\n' "$3" "$1" "$2"; fail=1; fi
}

[ -f "$BUILD_DIST" ] || {
    echo "FAIL: scripts/build-dist.sh not found"
    exit 1
}

case "$(uname -m)" in
    x86_64) ARCH=amd64 ;;
    aarch64) ARCH=arm64 ;;
    *)
        echo "SKIP: host arch $(uname -m) not amd64/arm64, cannot exec the built binary"
        exit 0
        ;;
esac

VERSION=9.9.9
DISTNAME="gotcha-$VERSION-linux-$ARCH"
OUT_DIR=$(mktemp -d)
EXTRACT_DIR=$(mktemp -d)
trap 'rm -rf "$OUT_DIR" "$EXTRACT_DIR"' EXIT

if ! bash "$BUILD_DIST" --version "$VERSION" --arch "$ARCH" --out "$OUT_DIR"; then
    echo "FAIL: build-dist.sh exited non-zero"
    exit 1
fi

TARBALL="$OUT_DIR/$DISTNAME.tar.gz"
if [ ! -f "$TARBALL" ]; then
    echo "FAIL: tarball not found: $TARBALL"
    exit 1
fi

tar -xzf "$TARBALL" -C "$EXTRACT_DIR"

DIST="$EXTRACT_DIR/$DISTNAME"
if [ ! -d "$DIST" ]; then
    echo "FAIL: root dir $DISTNAME missing in tarball"
    exit 1
fi

want_files=$(printf '%s\n' \
    agent-dist/SHA256SUMS \
    agent-dist/gotcha-agent-linux-amd64 \
    agent-dist/gotcha-agent-linux-arm64 \
    clickhouse/00-common.xml \
    clickhouse/10-small.xml \
    gotcha \
    install-bare-metal.sh \
    VERSION | sort | tr '\n' ' ')
want_files="${want_files% }"
got_files=$(cd "$DIST" && find . -type f | sed 's#^\./##' | sort | tr '\n' ' ')
got_files="${got_files% }"
assert_eq "$want_files" "$got_files" "tarball file listing"

got_version=$(cat "$DIST/VERSION")
assert_eq "$VERSION" "$got_version" "VERSION file contents"

got_version_out=$("$DIST/gotcha" version)
case "$got_version_out" in
    *"$VERSION"*) ;;
    *) assert_eq "contains $VERSION" "$got_version_out" "gotcha version output" ;;
esac

if ! (cd "$DIST/agent-dist" && sha256sum -c SHA256SUMS >/dev/null 2>&1); then
    assert_eq "sha256sum -c OK" "sha256sum -c FAILED" "agent-dist/SHA256SUMS matches binaries next to it"
fi

if [ "$fail" -eq 0 ]; then
    echo "OK: dist tarball layout as expected"
fi
exit "$fail"
