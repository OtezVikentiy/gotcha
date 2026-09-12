#!/usr/bin/env bash
# Один прогон с -coverpkg на весь набор: пер-пакетный -cover даёт шаблонам и
# веб-хендлерам почти ноль — они исполняются только через интеграционные тесты
# соседних пакетов. -p 1: testcontainers кладут машину при параллельном старте.
#
# Использование:
#   scripts/coverage.sh            # проверить пороги (CI-режим, exit 1 при провале)
#   scripts/coverage.sh -html      # + HTML-отчёт в /tmp
set -euo pipefail
cd "$(dirname "$0")/.."

# Пороги-храповик: не ниже зафиксированного уровня. Поднимать при росте,
# НИКОГДА не опускать — это защита от «отрывания» покрытия при рефакторинге.
# FRONT/TEMPL считаются раздельно: сгенерированные *_templ.go покрывает по
# построению любой рендер-тест и размывали бы метрику рукописного фронта.
# Допуск небольшой, потому что покрытие слегка плавает от прогона к прогону —
# часть строк исполняется только при определённом порядке интеграционных
# тестов. Значения ниже — договорённость, зафиксированная В КОДЕ; переопределить
# их окружением можно только ВВЕРХ, иначе «понизить порог» и «уронить покрытие»
# делаются одним коммитом.
FRONT_FLOOR=83.0
BACK_FLOOR=85.0
TEMPL_FLOOR=85.0
FRONT_MIN=${FRONT_MIN:-$FRONT_FLOOR}
BACK_MIN=${BACK_MIN:-$BACK_FLOOR}
# TEMPL_MIN — отдельный храповик для *_templ.go: внутри .templ живут АВТОРСКИЕ
# ветвления (`if canManage`, `switch status`, циклы), и без своего пола покрытие
# шаблонов могло бы уехать в ноль, а гейт сказал бы OK.
#
# Сгенерированный error-glue (if templ_7745c5c3_Err != nil { return ... } после
# каждой записи) исключён из знаменателя этой группы: он меряет не логику
# шаблона, а полноту перебора границ записи в TestRenderPropagatesWriteErrors —
# гейт мерил бы стоимость теста, а не качество кода.
TEMPL_MIN=${TEMPL_MIN:-$TEMPL_FLOOR}
# CMD_MIN — пол точки входа (health-ручки, разбор конфига, healthcheck-подкоманда).
CMD_FLOOR=53.0
CMD_MIN=${CMD_MIN:-$CMD_FLOOR}

# Формат порога: число (с необязательной дробной частью). Проверяется ДО
# сравнения с полом — awk '{ g < f }' сравнивает численно только если ОБА
# операнда выглядят числами; если given нечисловой ("disabled", "NaN"),
# сравнение молча становится строковым, а буквы лексически больше цифр, так
# что "disabled" < "85.0" — ложь, и отказ не срабатывает. Дальше по конвейеру,
# в финальном awk-разборе, нечисловая строка тем же способом превращается в
# 0 через `+0` ("abc"+0 == 0 в awk) — то есть нечисловое значение не просто
# обходит храповик, а обнуляет реальный пол. Проверяем формат заранее и здесь
# же отказываем, а не полагаемся на арифметику ниже по цепочке.
NUM_RE='^[0-9]+(\.[0-9]+)?$'

# Понижение порога окружением — отказ, а не тихое согласие.
for pair in "FRONT_MIN:$FRONT_MIN:$FRONT_FLOOR" "BACK_MIN:$BACK_MIN:$BACK_FLOOR" "TEMPL_MIN:$TEMPL_MIN:$TEMPL_FLOOR" "CMD_MIN:$CMD_MIN:$CMD_FLOOR"; do
	name=${pair%%:*}
	rest=${pair#*:}
	given=${rest%%:*}
	floor=${rest#*:}
	if ! [[ $given =~ $NUM_RE ]]; then
		echo "FAIL: $name=$given — не число." >&2
		echo "      Пороги двигаются только вверх и только правкой scripts/coverage.sh," >&2
		echo "      иначе понижение порога и падение покрытия проходят одним коммитом." >&2
		exit 1
	fi
	if awk -v g="$given" -v f="$floor" 'BEGIN { exit !(g < f) }'; then
		echo "FAIL: $name=$given ниже зафиксированного пола $floor." >&2
		echo "      Пороги двигаются только вверх и только правкой scripts/coverage.sh," >&2
		echo "      иначе понижение порога и падение покрытия проходят одним коммитом." >&2
		exit 1
	fi
done
# Пер-пакетный пол для security-критичных пакетов: общий BACKEND — одно число на
# 24 пакета, и мелкие среди них структурно беззащитны (secretbox — 16 стейтментов,
# netguard — 29), просадка любого не сдвинет агрегат заметно. internal/auth и
# internal/org хранят пароли/сессии/RBAC/тенантность/SSO/квоты — тоже со своим полом.
PKG_MIN_DEFAULT="internal/secretbox=94.5 internal/netguard=92.1 internal/alert=84.2 internal/ingest=85.9 internal/oauth=92.4 internal/auth=87 internal/org=84"
PKG_MIN=${PKG_MIN:-$PKG_MIN_DEFAULT}

# PKG_MIN — тоже храповик, но это не одно число, а набор "пакет=пол", поэтому
# понижение окружением может выглядеть двумя разными способами: занизить
# значение ИЛИ просто не упомянуть пакет (split в awk-разборе ниже даёт ноль
# пар на пустой строке, и без этой проверки исчезновение пакета из PKG_MIN
# тихо снимает с него пол вместо того, чтобы явно отказать). Проверяем обе
# формы здесь, в bash, а не в awk-блоке ниже — тот получает $PKG_MIN уже после
# запуска тестов, а падать нужно ДО дорогого прогона, как и для остальных
# четырёх порогов выше.
declare -A pkg_floor_default
for pair in $PKG_MIN_DEFAULT; do
	pkg_floor_default[${pair%%=*}]=${pair#*=}
done
declare -A pkg_floor_given
for pair in $PKG_MIN; do
	pkg_floor_given[${pair%%=*}]=${pair#*=}
done
for pkg in "${!pkg_floor_default[@]}"; do
	floor=${pkg_floor_default[$pkg]}
	given=${pkg_floor_given[$pkg]:-}
	if [[ -z "$given" ]]; then
		echo "FAIL: PKG_MIN не содержит пакет $pkg (зафиксированный пол $floor%)." >&2
		echo "      Пороги двигаются только вверх и только правкой scripts/coverage.sh," >&2
		echo "      иначе понижение порога и падение покрытия проходят одним коммитом." >&2
		exit 1
	fi
	# Тот же формат-чек, что и у остальных четырёх порогов выше: без него
	# "internal/secretbox=disabled" пройдёт мимо этого цикла (строковое
	# сравнение с полом лживо), а в финальном awk-разборе "disabled"+0 даст 0 —
	# то есть пол для пакета не занизится, а обнулится целиком.
	if ! [[ $given =~ $NUM_RE ]]; then
		echo "FAIL: PKG_MIN $pkg=$given — не число." >&2
		echo "      Пороги двигаются только вверх и только правкой scripts/coverage.sh," >&2
		echo "      иначе понижение порога и падение покрытия проходят одним коммитом." >&2
		exit 1
	fi
	if awk -v g="$given" -v f="$floor" 'BEGIN { exit !(g < f) }'; then
		echo "FAIL: PKG_MIN $pkg=$given ниже зафиксированного пола $floor." >&2
		echo "      Пороги двигаются только вверх и только правкой scripts/coverage.sh," >&2
		echo "      иначе понижение порога и падение покрытия проходят одним коммитом." >&2
		exit 1
	fi
done

PROFILE=$(mktemp /tmp/gotcha-cover.XXXXXX.out)
trap 'rm -f "$PROFILE"' EXIT

# cmd/gotcha входит в замер: тесты там живые и содержательные (health-ручки,
# конфиг, healthcheck-подкоманда).
PKGS_CSV=$(go list ./internal/... ./cmd/... | paste -sd,)
PKGS=$(go list ./internal/... ./cmd/... | tr '\n' ' ')

echo "Замер покрытия (-p 1, testcontainers, несколько минут)…" >&2
# -count=1 обязателен: без него повторный запуск отдаёт закешированный результат,
# и гейт проверяет кеш вместо стенда (все остальные цели Makefile идут с -count=1).
#
# -timeout задан явно и с большим запасом. Умолчание go test — 10 минут НА
# ТЕСТ-БИНАРЬ, и internal/web под инструментацией подошёл к нему вплотную:
# 510 с в одном прогоне, 600 с (таймаут, паника, гейт без чисел) в следующем.
# Отличать «тесты зависли» от «пакет вырос» по умолчанию, привязанному к
# размеру пакета, нельзя: граница ползёт вместе с ним, и однажды гейт просто
# перестаёт выдавать цифры на здоровом дереве. Порог здесь — про зависание,
# а не про длительность.
nice -n 19 go test -p 1 -count=1 -timeout 40m -coverpkg="$PKGS_CSV" -coverprofile="$PROFILE" $PKGS >&2

# Дедуп-aware разбор: с -coverpkg один и тот же блок появляется в профиле по
# разу на тест-бинарь; берём максимум count по уникальному ключу блока (как
# это делает `go tool cover`), затем суммируем строки по двум группам.
awk -v front_min="$FRONT_MIN" -v back_min="$BACK_MIN" -v templ_min="$TEMPL_MIN" -v cmd_min="$CMD_MIN" -v pkg_min="$PKG_MIN" '
NR==1 { next }                       # строка "mode:"
{
  key=$1; stmts[key]=$2; if ($3+0 > cnt[key]) cnt[key]=$3
}

# is_err_glue — блок ли это сгенерированной проверки ошибки записи:
#   if templ_7745c5c3_Err != nil { return templ_7745c5c3_Err }
# Определяется по исходнику, а не по эвристике над числами: ключ блока несёт
# файл и диапазон строк, читаем ровно их. Файл кэшируется целиком (srcline) —
# блоков в одном *_templ.go тысячи, а перечитывать его на каждый накладно.
function is_err_glue(key, file,   rng, a, b, l1, l2, body, i, line, real) {
  rng = key; sub(/^[^:]*:/, "", rng)
  split(rng, a, ",")
  # "12.34,56.78" — номер строки до точки, за ней колонка; нужна только строка.
  split(a[1], b, "."); l1 = b[1]+0
  split(a[2], b, "."); l2 = b[1]+0
  if (l2 - l1 > 2) return 0        # glue всегда 2-3 строки
  if (!(file in srcloaded)) {
    real = file; sub(/^gitflic\.ru\/otezvikentiy\/gotcha\//, "", real)
    i = 0
    while ((getline line < real) > 0) srcline[file, ++i] = line
    close(real)
    # Нечитаемый файл — это не «ноль glue-блоков», а сломанный гейт: он молча
    # вернул бы прежнюю размытую цифру, и понять, почему порог вдруг не сходится,
    # было бы не по чему. Падаем громко.
    if (i == 0) {
      printf "FAIL: не прочитать %s — гейт не может отделить error-glue\n", real
      exit 1
    }
    srcloaded[file] = 1
  }
  body = ""
  for (i = l1; i <= l2; i++) body = body srcline[file, i]
  return (index(body, "templ_7745c5c3_Err != nil") > 0 &&
          index(body, "return templ_7745c5c3_Err") > 0)
}
END {
  # Пер-пакетные полы: "путь=процент путь=процент ..."
  n=split(pkg_min, pairs, " ")
  for (i=1; i<=n; i++) {
    if (split(pairs[i], kv, "=") == 2) floor_of[kv[1]] = kv[2]+0
  }

  for (key in stmts) {
    split(key,a,":"); file=a[1]
    # internal/testenv — тестовая инфраструктура, покрытая всегда: бесплатные
    # проценты в знаменателе, исключаем.
    if (file ~ /\/internal\/testenv\//) continue

    if (file ~ /_templ\.go$/) {
      if (is_err_glue(key, file)) continue
      grp = "templ"
    }
    # cmd/* — отдельная группа, а не часть бэкенда: точка входа состоит из
    # проводки (создать пул, зарегистрировать маршрут, запустить горутину),
    # которая тестируется интеграционно, и вливать её в общий знаменатель
    # значит размывать метрику бэкенда так же, как *_templ.go размывали фронт.
    else if (file ~ /\/cmd\//) grp = "cmd"
    else grp = (file ~ /\/internal\/web\//) ? "front" : "back"
    tot[grp]+=stmts[key]; if (cnt[key]>0) cov[grp]+=stmts[key]

    # Пакет = путь до последнего слэша, обрезанный до internal/... или cmd/...
    pkg = file
    sub(/\/[^\/]*$/, "", pkg)
    sub(/^.*\/(internal\/)/, "internal/", pkg)
    sub(/^.*\/(cmd\/)/, "cmd/", pkg)
    ptot[pkg]+=stmts[key]; if (cnt[key]>0) pcov[pkg]+=stmts[key]
  }
  fp = tot["front"] ? 100*cov["front"]/tot["front"] : 0
  bp = tot["back"]  ? 100*cov["back"]/tot["back"]   : 0
  tp = tot["templ"] ? 100*cov["templ"]/tot["templ"] : 0
  cp = tot["cmd"]   ? 100*cov["cmd"]/tot["cmd"]     : 0
  printf "FRONTEND (рукописный web+templates): %.1f%% (%d/%d)  порог %.1f%%\n", fp, cov["front"], tot["front"], front_min
  printf "BACKEND  (internal/*):               %.1f%% (%d/%d)  порог %.1f%%\n", bp, cov["back"],  tot["back"],  back_min
  printf "TEMPL    (*_templ.go, без err-glue):  %.1f%% (%d/%d)  порог %.1f%%\n", tp, cov["templ"], tot["templ"], templ_min
  printf "CMD      (cmd/*):                    %.1f%% (%d/%d)  порог %.1f%%\n", cp, cov["cmd"], tot["cmd"], cmd_min
  fail=0
  if (fp+0.05 < front_min) { printf "FAIL: фронтенд %.1f%% < %.1f%%\n", fp, front_min; fail=1 }
  if (bp+0.05 < back_min)  { printf "FAIL: бэкенд %.1f%% < %.1f%%\n",  bp, back_min;  fail=1 }
  if (tp+0.05 < templ_min) { printf "FAIL: шаблоны %.1f%% < %.1f%%\n", tp, templ_min; fail=1 }
  if (cp+0.05 < cmd_min)   { printf "FAIL: cmd %.1f%% < %.1f%%\n", cp, cmd_min; fail=1 }
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
