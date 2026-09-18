#!/usr/bin/env bash
# Флаги ldflags и способ генерации SHA256SUMS повторяют Dockerfile дословно.
set -euo pipefail

usage() {
    cat <<'EOF'
Usage: build-dist.sh --version X.Y.Z --arch amd64|arm64 --out DIR

Builds DIR/gotcha-X.Y.Z-linux-<arch>.tar.gz: the server binary for <arch>,
both gotcha-agent binaries (amd64 and arm64), the ClickHouse tuning configs,
the bare-metal installer script and a VERSION file.
EOF
}

VERSION=""
ARCH=""
OUT=""

while [ $# -gt 0 ]; do
    case "$1" in
        --version)
            VERSION="${2:-}"
            shift 2
            ;;
        --arch)
            ARCH="${2:-}"
            shift 2
            ;;
        --out)
            OUT="${2:-}"
            shift 2
            ;;
        -h | --help)
            usage
            exit 0
            ;;
        *)
            echo "build-dist.sh: unknown argument: $1" >&2
            usage >&2
            exit 2
            ;;
    esac
done

[ -n "$VERSION" ] || { echo "build-dist.sh: --version is required" >&2; exit 2; }
[ -n "$ARCH" ] || { echo "build-dist.sh: --arch is required" >&2; exit 2; }
[ -n "$OUT" ] || { echo "build-dist.sh: --out is required" >&2; exit 2; }

echo "$VERSION" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$' \
    || { echo "build-dist.sh: version must be X.Y.Z, got '$VERSION'" >&2; exit 2; }

case "$ARCH" in
    amd64 | arm64) ;;
    *)
        echo "build-dist.sh: --arch must be amd64 or arm64, got '$ARCH'" >&2
        exit 2
        ;;
esac

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMMIT="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
VPKG="gitflic.ru/otezvikentiy/gotcha/internal/version"
LDFLAGS="-X $VPKG.version=$VERSION -X $VPKG.commit=$COMMIT -X $VPKG.date=$DATE"

mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

DISTNAME="gotcha-$VERSION-linux-$ARCH"
STAGE="$WORKDIR/$DISTNAME"
mkdir -p "$STAGE/agent-dist" "$STAGE/clickhouse"

cd "$ROOT"

CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -mod=vendor \
    -ldflags "$LDFLAGS" \
    -o "$STAGE/gotcha" ./cmd/gotcha

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor \
    -ldflags "-s -w $LDFLAGS" \
    -o "$STAGE/agent-dist/gotcha-agent-linux-amd64" ./cmd/gotcha-agent
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -mod=vendor \
    -ldflags "-s -w $LDFLAGS" \
    -o "$STAGE/agent-dist/gotcha-agent-linux-arm64" ./cmd/gotcha-agent

(cd "$STAGE/agent-dist" && sha256sum gotcha-agent-linux-amd64 gotcha-agent-linux-arm64 > SHA256SUMS)

cp "$ROOT/deploy/clickhouse/00-common.xml" "$STAGE/clickhouse/00-common.xml"
cp "$ROOT/deploy/clickhouse/10-small.xml" "$STAGE/clickhouse/10-small.xml"
cp "$ROOT/internal/docs/install-bare-metal.sh" "$STAGE/install-bare-metal.sh"
printf '%s\n' "$VERSION" > "$STAGE/VERSION"

tar -czf "$OUT/$DISTNAME.tar.gz" -C "$WORKDIR" "$DISTNAME"
echo "$OUT/$DISTNAME.tar.gz"
