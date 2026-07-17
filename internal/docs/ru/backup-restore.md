# Резервное копирование и восстановление

Gotcha хранит данные в двух разных базах, и обе одинаково важны — резервную копию нужно снимать **из обеих сразу**, иначе после восстановления они разойдутся (например, проект есть в одной базе, а его события — в другой, или наоборот).

| База | Что в ней | Контейнер |
|---|---|---|
| **PostgreSQL** | Аккаунты, организации, проекты, участники, правила алертов, каналы доставки, инциденты, настройки — всё, что вы настраивали руками в интерфейсе. | `postgres` |
| **ClickHouse** | Сами события об ошибках, спаны трейсов, точки метрик, сэмплы профилей, результаты аптайм-проверок — весь объём телеметрии, которую прислали ваши приложения. | `clickhouse` |

Если восстановить только одну из баз — интерфейс либо сломается (проект есть в UI, но для него нет ни одного события), либо, наоборот, вы потеряете саму настройку (алерты, участников, DSN-ключи), даже если телеметрия цела.

Все команды ниже выполняются **из папки репозитория** (`gotcha/`, там же, где `docker-compose.yml`) и используют `docker compose exec` — то есть выполняют команду внутри уже запущенного контейнера, без необходимости пробрасывать порты баз наружу (они и не проброшены — см. [Установку](/docs/installation)).

## Backup: PostgreSQL

`pg_dump` — стандартная утилита логического бэкапа PostgreSQL, безопасно снимает копию с работающей базы без остановки сервиса:

```bash
mkdir -p backup
docker compose exec -T postgres pg_dump -U gotcha -d gotcha \
  | gzip > backup/postgres-$(date +%F).sql.gz
```

Разбор команды: `docker compose exec -T postgres` — выполнить внутри контейнера `postgres` (`-T` отключает псевдо-терминал, нужно при перенаправлении вывода в файл); `pg_dump -U gotcha -d gotcha` — выгрузить базу `gotcha` от имени пользователя `gotcha` (это дефолтные учётные данные из `docker-compose.yml`; если вы их меняли — подставьте свои); результат уходит на стандартный вывод, который мы сжимаем `gzip` и сохраняем на диск хоста с датой в имени файла.

Проверить, что файл не пустой и похож на дамп:

```bash
zcat backup/postgres-$(date +%F).sql.gz | head -20
```

Вы должны увидеть строки вида `-- PostgreSQL database dump` и `CREATE TABLE ...`.

## Backup: ClickHouse

ClickHouse хранит несравнимо больший объём данных, чем PostgreSQL, поэтому для него используется другой подход: выгрузка каждой таблицы во встроенном бинарном формате `Native` (компактный и быстрый для последующего восстановления той же версией ClickHouse).

Сначала узнайте список таблиц базы `gotcha`:

```bash
docker compose exec -T clickhouse clickhouse-client \
  --user gotcha --password gotcha --database gotcha \
  --query "SHOW TABLES"
```

Затем выгрузите каждую таблицу из списка (повторите команду для каждой строки, которую вернул `SHOW TABLES`, например `events`, `transactions`, `spans`, `metric_points`, `profile_samples`, `check_results`):

```bash
mkdir -p backup/clickhouse
for t in events transactions spans metric_points profile_samples check_results; do
  docker compose exec -T clickhouse clickhouse-client \
    --user gotcha --password gotcha --database gotcha \
    --query "SELECT * FROM $t FORMAT Native" \
    > backup/clickhouse/$t-$(date +%F).native
done
```

Это работает на «живой» базе без остановки — ClickHouse отдаёт консистентный снепшот на момент запроса для каждой отдельной таблицы (снепшот не гарантированно единый момент времени сразу для всех таблиц, но для метрик наблюдаемости это в подавляющем большинстве случаев не критично).

**Более простой и абсолютно надёжный вариант — снапшот файловой системы с остановкой сервисов.** Он гарантированно консистентен и для PostgreSQL, и для ClickHouse одновременно, ценой короткого простоя (обычно секунды-десятки секунд):

```bash
docker compose stop gotcha postgres clickhouse
docker run --rm \
  -v gotcha_pgdata:/pgdata:ro \
  -v gotcha_chdata:/chdata:ro \
  -v "$(pwd)/backup:/backup" \
  alpine tar czf /backup/volumes-$(date +%F).tar.gz /pgdata /chdata
docker compose start gotcha postgres clickhouse
```

(имена томов `gotcha_pgdata`/`gotcha_chdata` — префикс `gotcha_` берётся из имени папки проекта; проверьте точное имя командой `docker volume ls | grep gotcha`, если оно отличается). Этот вариант хорошо подходит для ночного cron-задания, когда короткая недоступность приложения не критична.

Выбирайте один из двух подходов (живая выгрузка `pg_dump`+`clickhouse-client`, либо снапшот томов с простоем) — оба валидны, важно делать это **регулярно** и **проверять**, что бэкап действительно восстанавливается (см. ниже).

## Restore: PostgreSQL

Восстановление из `pg_dump`-архива в **пустую** базу (для непустой сначала пересоздайте базу данных или используйте `docker compose down -v`, что удалит все данные — будьте осторожны):

```bash
gunzip -c backup/postgres-2026-07-01.sql.gz \
  | docker compose exec -T postgres psql -U gotcha -d gotcha
```

Если восстанавливаете на новый инстанс — сначала поднимите контейнеры (`docker compose up -d`), дождитесь применения миграций (см. [Установку](/docs/installation)), убедитесь, что схема пустая (только что созданная), и только потом накатывайте дамп. Восстановление дампа поверх базы, где миграции уже применились и она непустая, приведёт к конфликтам первичных ключей.

## Restore: ClickHouse

Восстановление таблицы, выгруженной в формате `Native`, обратной командой:

```bash
cat backup/clickhouse/events-2026-07-01.native | \
  docker compose exec -T clickhouse clickhouse-client \
    --user gotcha --password gotcha --database gotcha \
    --query "INSERT INTO events FORMAT Native"
```

Повторите для каждой таблицы. Таблица должна существовать (создаётся миграциями при старте приложения) и быть пустой, иначе данные добавятся к уже имеющимся, а не заменят их.

## Restore из снапшота томов

Если использовался вариант с `tar` томов:

```bash
docker compose down
docker run --rm \
  -v gotcha_pgdata:/pgdata \
  -v gotcha_chdata:/chdata \
  -v "$(pwd)/backup:/backup" \
  alpine sh -c "rm -rf /pgdata/* /chdata/* && tar xzf /backup/volumes-2026-07-01.tar.gz -C /"
docker compose up -d
```

**Это разрушительная операция** — она стирает текущее содержимое томов перед распаковкой архива. Убедитесь, что архив тот, что нужен, прежде чем запускать.

## После восстановления — проверка

```bash
curl -sf http://localhost:59080/healthz
```

Затем откройте интерфейс, зайдите под своим пользователем, откройте проект и проверьте, что видны и настройки (алерты, участники), и данные (события в разделе «Проблемы»).

## Пример cron-задания

Ежедневный бэкап PostgreSQL + ClickHouse в 3:30 ночи, с хранением 14 последних копий:

```bash
crontab -e
```

добавьте строку:

```cron
30 3 * * * cd /path/to/gotcha && /path/to/gotcha/backup.sh >> /var/log/gotcha-backup.log 2>&1
```

где `backup.sh` — небольшой скрипт со всеми командами выгрузки выше плюс чистка старых файлов, например:

```bash
#!/usr/bin/env bash
set -euo pipefail
cd /path/to/gotcha
mkdir -p backup/clickhouse
docker compose exec -T postgres pg_dump -U gotcha -d gotcha \
  | gzip > backup/postgres-$(date +%F).sql.gz
for t in events transactions spans metric_points profile_samples check_results; do
  docker compose exec -T clickhouse clickhouse-client \
    --user gotcha --password gotcha --database gotcha \
    --query "SELECT * FROM $t FORMAT Native" \
    > backup/clickhouse/$t-$(date +%F).native
done
# храним 14 дней
find backup -type f -mtime +14 -delete
```

Не забудьте сделать скрипт исполняемым (`chmod +x backup.sh`) и, что важно, копировать содержимое папки `backup/` **за пределы этого же сервера** (другой диск, S3-совместимое хранилище, другой сервер) — локальная копия не спасёт при выходе из строя самого сервера.

## Что дальше

- [Установка](/docs/installation).
- [Обновление](/docs/upgrade) — резервную копию нужно снимать перед каждым обновлением.
- [Конфигурация](/docs/configuration) — переменные `GOTCHA_*_RETENTION_DAYS`, влияющие на то, сколько данных вообще накапливается в ClickHouse.
