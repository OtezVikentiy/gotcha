#!/usr/bin/env bash
# coverage.sh — интегрированный замер покрытия с раздельными порогами для
# фронтенда (SSR: internal/web + internal/web/templates) и бэкенда (остальные
# internal/*, кроме cmd/gotcha — точка входа не тестируется юнитами).
#
# Зачем один прогон с -coverpkg на весь набор, а не пер-пакетный -cover:
# большая часть строк шаблонов и веб-хендлеров исполняется ТОЛЬКО через
# интеграционные тесты соседних пакетов; пер-пакетный замер засчитал бы им
# почти ноль. -coverpkg=<все> сшивает вклад всех тестов в один профиль.
#
# testcontainers кладут машину при параллельном старте — поэтому -p 1 и один
# вызов go test (а не цикл по пакетам). Прогон занимает несколько минут.
#
# Использование:
#   scripts/coverage.sh            # проверить пороги (CI-режим, exit 1 при провале)
#   scripts/coverage.sh -html      # + HTML-отчёт в /tmp
set -euo pipefail
cd "$(dirname "$0")/.."

# Пороги-храповик: не ниже зафиксированного уровня. Поднимать при росте,
# НИКОГДА не опускать — это защита от «отрывания» покрытия при рефакторинге.
FRONT_MIN=${FRONT_MIN:-85.0}
BACK_MIN=${BACK_MIN:-85.0}

PROFILE=$(mktemp /tmp/gotcha-cover.XXXXXX.out)
trap 'rm -f "$PROFILE"' EXIT

PKGS_CSV=$(go list ./internal/... | paste -sd,)
PKGS=$(go list ./internal/... | tr '\n' ' ')

echo "Замер покрытия (-p 1, testcontainers, несколько минут)…" >&2
nice -n 19 go test -p 1 -coverpkg="$PKGS_CSV" -coverprofile="$PROFILE" $PKGS >&2

# Дедуп-aware разбор: с -coverpkg один и тот же блок появляется в профиле по
# разу на тест-бинарь; берём максимум count по уникальному ключу блока (как
# это делает `go tool cover`), затем суммируем строки по двум группам.
awk -v front_min="$FRONT_MIN" -v back_min="$BACK_MIN" '
NR==1 { next }                       # строка "mode:"
{
  key=$1; stmts[key]=$2; if ($3+0 > cnt[key]) cnt[key]=$3
}
END {
  for (key in stmts) {
    n=split(key,a,":"); file=a[1]
    grp = (file ~ /\/internal\/web\//) ? "front" : "back"
    tot[grp]+=stmts[key]; if (cnt[key]>0) cov[grp]+=stmts[key]
  }
  fp = tot["front"] ? 100*cov["front"]/tot["front"] : 0
  bp = tot["back"]  ? 100*cov["back"]/tot["back"]   : 0
  printf "FRONTEND (web+templates): %.1f%% (%d/%d)  порог %.1f%%\n", fp, cov["front"], tot["front"], front_min
  printf "BACKEND  (internal/*):    %.1f%% (%d/%d)  порог %.1f%%\n", bp, cov["back"],  tot["back"],  back_min
  fail=0
  if (fp+0.05 < front_min) { printf "FAIL: фронтенд %.1f%% < %.1f%%\n", fp, front_min; fail=1 }
  if (bp+0.05 < back_min)  { printf "FAIL: бэкенд %.1f%% < %.1f%%\n",  bp, back_min;  fail=1 }
  if (fail) exit 1
  print "OK: оба порога соблюдены."
}' "$PROFILE"

if [[ "${1:-}" == "-html" ]]; then
  OUT=/tmp/gotcha-coverage.html
  go tool cover -html="$PROFILE" -o "$OUT"
  echo "HTML-отчёт: $OUT" >&2
fi
