#!/usr/bin/env bash
# Репетиция обновления: снимки инвариантов, апгрейд, откат.
# Работает ТОЛЬКО со стендом. Путей к бою здесь нет и быть не должно.
set -euo pipefail

REH_DIR="${REH_DIR:-/ssd/pet/gotcha-reh}"
REH_PROJECT="${REH_PROJECT:-gotcha-reh}"
REH_PORT="${REH_PORT:-59081}"
# Проверяются не здесь, а в dc(): compare сравнивает два локальных файла, и
# требовать от него доступа к стенду незачем.
REH_ENV="${REH_ENV:-}"
REH_OVERRIDE="${REH_OVERRIDE:-}"

dc() {
  : "${REH_ENV:?путь к .env стенда не задан}"
  : "${REH_OVERRIDE:?путь к compose.override.yml не задан}"
  docker compose -p "$REH_PROJECT" --env-file "$REH_ENV" \
    -f "$REH_DIR/docker-compose.yml" -f "$REH_OVERRIDE" "$@"
}

# </dev/null обязателен: без него docker compose exec держит stdin
# подключённым, и вызов psql_q/ch_q изнутри `while read` съедает остаток
# входного пайпа цикла после первой же итерации.
psql_q() { dc exec -T postgres psql -U gotcha -d gotcha -At -c "$1" </dev/null; }
ch_q() {
  dc exec -T clickhouse clickhouse-client --user gotcha --password gotcha \
    --database gotcha --query "$1" </dev/null
}

snapshot_pg() {
  psql_q "SELECT table_name FROM information_schema.tables
           WHERE table_schema='public' AND table_type='BASE TABLE'
           ORDER BY table_name" |
  while read -r t; do
    [ -n "$t" ] || continue
    n=$(psql_q "SELECT count(*) FROM \"$t\"")
    printf 'pg\ttable\t%s\t%s\n' "$t" "$n"
  done
}

snapshot_ch() {
  ch_q "SELECT name FROM system.tables
         WHERE database=currentDatabase() AND engine NOT LIKE '%View%'
         ORDER BY name" |
  while read -r t; do
    [ -n "$t" ] || continue
    n=$(ch_q "SELECT count(*) FROM \`$t\`")
    printf 'ch\ttable\t%s\t%s\n' "$t" "$n"
  done
}

# Табуляции строит bash, а не SQL: psql не разворачивает \t внутри строкового
# литерала и вернул бы обратный слэш с буквой, из-за чего compare перестал бы
# отличать схемные строки от прочих.
snapshot_schema() {
  psql_q "SELECT version||' '||dirty FROM schema_migrations" |
    while read -r v d; do printf 'pg\tschema\tversion\t%s\tdirty\t%s\n' "$v" "$d"; done
  psql_q "SELECT target||' '||version||' '||backward_compatible
            FROM schema_compat ORDER BY target, version" |
    while read -r tgt v bc; do printf 'pg\tcompat\t%s\t%s\t%s\n' "$tgt" "$v" "$bc"; done
}

snapshot_business() {
  for t in users projects project_keys alert_channels issues \
           notification_outbox maintenance_windows status_pages; do
    printf 'biz\tcount\t%s\t%s\n' "$t" "$(psql_q "SELECT count(*) FROM $t")"
  done
  psql_q "SELECT project_id||' '||count(*) FROM issues
            GROUP BY project_id ORDER BY count(*) DESC, project_id LIMIT 10" |
    while read -r pid n; do printf 'biz\ttop_issues\t%s\t%s\n' "$pid" "$n"; done
}

cmd_snapshot() {
  local out="${1:?куда писать снимок}"
  { snapshot_pg; snapshot_ch; snapshot_schema; snapshot_business; } | sort > "$out"
  echo "снимок: $out ($(wc -l < "$out") строк)"
}

cmd_compare() {
  local a="${1:?снимок до}" b="${2:?снимок после}" allow="${3:-}"
  local fa fb diff_out
  fa=$(mktemp) && fb=$(mktemp)
  # Схема и совместимость обязаны меняться при апгрейде — сверяются отдельно,
  # из сравнения исключаются ещё ДО diff: если фильтровать сам вывод diff по
  # содержимому строк, заголовки блоков ("NcN", "---") под фильтр не попадают
  # и remain — расхождение продолжит "кричать" даже на пустой разнице.
  grep -v -P '^pg\t(schema|compat)\t' "$a" > "$fa" || true
  grep -v -P '^pg\t(schema|compat)\t' "$b" > "$fb" || true
  if [ -n "$allow" ] && [ -s "$allow" ]; then
    while read -r pat; do
      case "$pat" in ''|\#*) continue ;; esac
      grep -Fv "$pat" "$fa" > "$fa.tmp" || true; mv "$fa.tmp" "$fa"
      grep -Fv "$pat" "$fb" > "$fb.tmp" || true; mv "$fb.tmp" "$fb"
    done < "$allow"
  fi
  diff_out=$(diff "$fa" "$fb" || true)
  rm -f "$fa" "$fb"
  if [ -n "$diff_out" ]; then
    echo "РАСХОЖДЕНИЕ вне списка ожидаемых:"; printf '%s\n' "$diff_out"; return 1
  fi
  echo "инварианты совпали"
}

cmd_upgrade() {
  local tag="${1:?тег}"
  git -C "$REH_DIR" checkout "$tag"
  dc build gotcha                     # сборка вне даунтайма: на проде она делается заранее
  local t0 t1
  t0=$(date +%s.%N)
  dc up -d --no-build
  local i=0
  until curl -sf "http://127.0.0.1:$REH_PORT/readyz" >/dev/null 2>&1; do
    i=$((i+1))
    if [ "$i" -gt 900 ]; then           # 900 × 0.2 с = три минуты
      echo "ПРОВАЛ: не готов за три минуты. Хвост журнала:" >&2
      dc logs --tail=30 gotcha >&2
      return 1
    fi
    sleep 0.2
  done
  t1=$(date +%s.%N)
  echo "даунтайм до готовности: $(echo "$t1 - $t0" | bc) с (нижняя оценка, стенд не прод)"
}

cmd_migrate_only() {
  local t0 t1
  t0=$(date +%s.%N)
  dc run --rm --no-deps gotcha --migrate-only
  t1=$(date +%s.%N)
  echo "только миграции: $(echo "$t1 - $t0" | bc) с"
}

cmd_rollback() {
  local tag="${1:?тег}"
  git -C "$REH_DIR" checkout "$tag"
  dc build gotcha
  dc up -d --no-build || true
  local i=0 state status
  while [ "$i" -lt 60 ]; do
    if curl -sf "http://127.0.0.1:$REH_PORT/readyz" >/dev/null 2>&1; then
      echo "ПУСТИЛ: $tag стартовал на текущей схеме"; return 0
    fi
    state=$(dc ps -a --format '{{.State}}' gotcha 2>/dev/null | head -1)
    # "restarting" — не менее верный признак отказа, чем "exited": политика
    # restart:unless-stopped (docker-compose.yml) перезапускает упавший
    # процесс сразу же, и docker при этом никогда не показывает "exited"
    # надолго — состояние прыгает "exited"→"restarting" быстрее, чем успевает
    # попасть в опрос раз в секунду. Без этой ветки любой чистый отказ гейта
    # (процесс валится немедленно и без остановки) неотличим по коду от
    # настоящего зависания — оба тонут в одном и том же таймауте НЕЯСНО.
    # Настоящее зависание (процесс жив, порт не открылся) состояние вообще не
    # меняет: unhealthy сам по себе перезапуск не запускает (см. комментарий
    # у healthcheck в docker-compose.yml), значит "running" 60 раз подряд —
    # надёжный признак именно зависания, а не отказа.
    if [ "$state" = "exited" ] || [ "$state" = "restarting" ]; then
      status=$(dc ps -a --format '{{.Status}}' gotcha 2>/dev/null | head -1)
      echo "ОТКАЗАЛ: $tag не смог стартовать на текущей схеме ($status). Хвост журнала:"
      dc logs --tail=30 gotcha; return 0
    fi
    i=$((i+1)); sleep 1
  done
  echo "НЕЯСНО: $tag за 60 с не вышел и не открыл порт — контейнер висит. Хвост журнала:"
  dc logs --tail=30 gotcha
}

case "${1:-}" in
  snapshot)     shift; cmd_snapshot "$@" ;;
  compare)      shift; cmd_compare "$@" ;;
  upgrade)      shift; cmd_upgrade "$@" ;;
  rollback)     shift; cmd_rollback "$@" ;;
  migrate-only) shift; cmd_migrate_only "$@" ;;
  *) echo "команды: snapshot | compare | upgrade | rollback | migrate-only" >&2; exit 2 ;;
esac
