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
#
# FRONT_MIN опущен с 85 до 81 ОДНОРАЗОВО и осознанно — потому что изменился
# знаменатель, а не покрытие. Раньше в него входили сгенерированные *_templ.go:
# 19 938 стейтментов против 5 294 рукописных, то есть 79% метрики измеряли
# машинный код, который любой рендер-тест покрывает по построению. Замерено на
# живом профиле: генерация 86.2%, рукописный фронт 81.0%, смешанное 85.1% при
# пороге 85.0 — то есть порог держался в основном за счёт генерации и допускал
# падение рукописного покрытия почти вдвое, до ~47%, никак на это не реагируя.
#
# Теперь считается только рукописный код. 80.5 — фактический замеренный уровень
# (80.9%) с небольшим допуском: интегральное покрытие слегка плавает от прогона
# к прогону, потому что часть строк исполняется только при определённом порядке
# интеграционных тестов. Дальше поднимать, как и прежде, — вниз не двигать.
FRONT_MIN=${FRONT_MIN:-80.5}
BACK_MIN=${BACK_MIN:-85.0}
# TEMPL_MIN — отдельный храповик для сгенерированных *_templ.go.
#
# Их вывели из знаменателя FRONTEND (они размывали метрику вчетверо), но совсем
# без пола оставлять нельзя: внутри .templ живут АВТОРСКИЕ ветвления —
# `if canManage`, `if len(rows) == 0`, `switch status`, циклы по строкам — и целые
# Go-функции, написанные в .templ-файлах. Без этой группы покрытие шаблонов могло
# уехать с 86% в ноль, а гейт сказал бы OK.
TEMPL_MIN=${TEMPL_MIN:-85.0}
# Пер-пакетный пол для security-критичных пакетов. Общий BACKEND — одно число на
# 24 пакета, и мелкие среди них структурно беззащитны: secretbox это 16
# стейтментов, netguard — 29, обнуление обоих стоит долей процента и порог этого
# не заметит. Здесь у каждого свой минимум.
PKG_MIN_DEFAULT="internal/secretbox=90 internal/netguard=90 internal/alert=80 internal/ingest=80 internal/oauth=80"
PKG_MIN=${PKG_MIN:-$PKG_MIN_DEFAULT}

PROFILE=$(mktemp /tmp/gotcha-cover.XXXXXX.out)
trap 'rm -f "$PROFILE"' EXIT

PKGS_CSV=$(go list ./internal/... | paste -sd,)
PKGS=$(go list ./internal/... | tr '\n' ' ')

echo "Замер покрытия (-p 1, testcontainers, несколько минут)…" >&2
# -count=1 обязателен: без него повторный запуск отдаёт закешированный результат,
# и гейт проверяет кеш вместо стенда (все остальные цели Makefile идут с -count=1).
nice -n 19 go test -p 1 -count=1 -coverpkg="$PKGS_CSV" -coverprofile="$PROFILE" $PKGS >&2

# Дедуп-aware разбор: с -coverpkg один и тот же блок появляется в профиле по
# разу на тест-бинарь; берём максимум count по уникальному ключу блока (как
# это делает `go tool cover`), затем суммируем строки по двум группам.
awk -v front_min="$FRONT_MIN" -v back_min="$BACK_MIN" -v templ_min="$TEMPL_MIN" -v pkg_min="$PKG_MIN" '
NR==1 { next }                       # строка "mode:"
{
  key=$1; stmts[key]=$2; if ($3+0 > cnt[key]) cnt[key]=$3
}
END {
  # Пер-пакетные полы: "путь=процент путь=процент ..."
  n=split(pkg_min, pairs, " ")
  for (i=1; i<=n; i++) {
    if (split(pairs[i], kv, "=") == 2) floor_of[kv[1]] = kv[2]+0
  }

  for (key in stmts) {
    split(key,a,":"); file=a[1]
    # Сгенерированные шаблоны в знаменатель НЕ входят: это машинный код, его
    # покрывает по построению любой рендер-тест, и он размывал метрику фронта
    # вчетверо. То же с internal/testenv — это тестовая инфраструктура,
    # покрытая всегда, то есть бесплатные проценты.
    # internal/testenv — тестовая инфраструктура, покрытая всегда: бесплатные
    # проценты в знаменателе.
    if (file ~ /\/internal\/testenv\//) continue

    if (file ~ /_templ\.go$/) grp = "templ"
    else grp = (file ~ /\/internal\/web\//) ? "front" : "back"
    tot[grp]+=stmts[key]; if (cnt[key]>0) cov[grp]+=stmts[key]

    # Пакет = путь до последнего слэша, обрезанный до internal/...
    pkg = file
    sub(/\/[^\/]*$/, "", pkg)
    sub(/^.*\/(internal\/)/, "internal/", pkg)
    ptot[pkg]+=stmts[key]; if (cnt[key]>0) pcov[pkg]+=stmts[key]
  }
  fp = tot["front"] ? 100*cov["front"]/tot["front"] : 0
  bp = tot["back"]  ? 100*cov["back"]/tot["back"]   : 0
  tp = tot["templ"] ? 100*cov["templ"]/tot["templ"] : 0
  printf "FRONTEND (рукописный web+templates): %.1f%% (%d/%d)  порог %.1f%%\n", fp, cov["front"], tot["front"], front_min
  printf "BACKEND  (internal/*):               %.1f%% (%d/%d)  порог %.1f%%\n", bp, cov["back"],  tot["back"],  back_min
  printf "TEMPL    (*_templ.go):               %.1f%% (%d/%d)  порог %.1f%%\n", tp, cov["templ"], tot["templ"], templ_min
  fail=0
  if (fp+0.05 < front_min) { printf "FAIL: фронтенд %.1f%% < %.1f%%\n", fp, front_min; fail=1 }
  if (bp+0.05 < back_min)  { printf "FAIL: бэкенд %.1f%% < %.1f%%\n",  bp, back_min;  fail=1 }
  if (tp+0.05 < templ_min) { printf "FAIL: шаблоны %.1f%% < %.1f%%\n", tp, templ_min; fail=1 }
  for (pkg in floor_of) {
    if (!(pkg in ptot)) { printf "FAIL: пакет %s не найден в профиле (переименован?)\n", pkg; fail=1; continue }
    pp = 100*pcov[pkg]/ptot[pkg]
    printf "  %-24s %.1f%% (%d/%d)  пол %.1f%%\n", pkg, pp, pcov[pkg], ptot[pkg], floor_of[pkg]
    if (pp+0.05 < floor_of[pkg]) { printf "FAIL: %s %.1f%% < %.1f%%\n", pkg, pp, floor_of[pkg]; fail=1 }
  }
  if (fail) exit 1
  print "OK: пороги соблюдены."
}' "$PROFILE"

if [[ "${1:-}" == "-html" ]]; then
  OUT=/tmp/gotcha-coverage.html
  go tool cover -html="$PROFILE" -o "$OUT"
  echo "HTML-отчёт: $OUT" >&2
fi
