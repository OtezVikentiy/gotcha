#!/usr/bin/env bash
# Релиз gotcha: двигает CHANGELOG, бампает internal/version, коммитит и тегает.
# Пуш в remote НЕ делает — печатает команды (пуш запускает оператор).
set -euo pipefail

VERSION="${1:-}"
[ -n "$VERSION" ] || { echo "usage: scripts/release.sh X.Y.Z"; exit 2; }
echo "$VERSION" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$' \
  || { echo "версия должна быть X.Y.Z, получили '$VERSION'"; exit 2; }

ROOT="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"
TAG="v$VERSION"
DATE="$(date -u +%Y-%m-%d)"

git -C "$ROOT" rev-parse -q --verify "refs/tags/$TAG" >/dev/null \
  && { echo "тег $TAG уже существует"; exit 2; }
[ "$(git -C "$ROOT" rev-parse --abbrev-ref HEAD)" = "main" ] \
  || { echo "релиз только с ветки main"; exit 2; }
git -C "$ROOT" diff --quiet && git -C "$ROOT" diff --cached --quiet \
  || { echo "рабочее дерево не чистое — закоммить или спрячь правки"; exit 2; }

# 1. CHANGELOG (оба языка): [Unreleased] → датированная секция + свежий [Unreleased].
for f in "$ROOT/CHANGELOG.md" "$ROOT/CHANGELOG.ru.md"; do
  sed -i "0,/## \[Unreleased\]/s//## [Unreleased]\n\n## [$VERSION] - $DATE/" "$f"
done

# 2. Бамп базовой константы версии.
sed -i -E "s/^const base = \"[0-9]+\.[0-9]+\.[0-9]+\"/const base = \"$VERSION\"/" \
  "$ROOT/internal/version/version.go"

# 3. Коммит и аннотированный тег.
git -C "$ROOT" add CHANGELOG.md CHANGELOG.ru.md internal/version/version.go
git -C "$ROOT" commit -m "release: $TAG"
git -C "$ROOT" tag -a "$TAG" -m "$TAG"

# 4. Пуш в remote. Пуш по умолчанию ОТКЛЮЧЁН: git в этом проекте выполняется
# руками владельца, скрипт только готовит коммит+тег. Явный опт-ин — PUSH=1
# (для CI/автоматики). У origin оба push-URL (gitflic + github) — при PUSH=1
# уедет в оба зеркала.
if [ -n "${PUSH:-}" ]; then
  echo "Пушу main и тег $TAG в origin (gitflic + github)…"
  git -C "$ROOT" push origin main --follow-tags
  echo "Готово: $TAG опубликован в оба зеркала."
else
  cat <<EOF
Готово: коммит "release: $TAG" и тег $TAG созданы локально (пуш по умолчанию
выключен — git руками). Опубликовать: git push origin main --follow-tags
(или прогнать с PUSH=1).
EOF
fi
