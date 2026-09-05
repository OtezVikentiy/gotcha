#!/usr/bin/env bash
# Репетиция обновления: снимки инвариантов, апгрейд, откат.
# Работает ТОЛЬКО со стендом. Путей к бою здесь нет и быть не должно.
set -euo pipefail

REH_DIR="${REH_DIR:-/ssd/pet/gotcha-reh}"
REH_PROJECT="${REH_PROJECT:-gotcha-reh}"
REH_PORT="${REH_PORT:-59081}"
REH_ENV="${REH_ENV:?путь к .env стенда не задан}"
REH_OVERRIDE="${REH_OVERRIDE:?путь к compose.override.yml не задан}"

dc() {
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

case "${1:-}" in
  snapshot)     shift; cmd_snapshot "$@" ;;
  compare)      shift; cmd_compare "$@" ;;
  upgrade)      shift; cmd_upgrade "$@" ;;
  rollback)     shift; cmd_rollback "$@" ;;
  migrate-only) shift; cmd_migrate_only "$@" ;;
  *) echo "команды: snapshot | compare | upgrade | rollback | migrate-only" >&2; exit 2 ;;
esac
