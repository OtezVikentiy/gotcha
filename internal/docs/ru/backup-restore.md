# Резервное копирование и восстановление

Gotcha хранит данные в двух разных базах, и обе одинаково важны — резервную копию нужно снимать **из обеих сразу**, иначе после восстановления они разойдутся (например, проект есть в одной базе, а его события — в другой, или наоборот).

| База | Что в ней | Контейнер |
|---|---|---|
| **PostgreSQL** | Аккаунты, организации, проекты, участники, правила алертов, каналы доставки, инциденты, настройки — всё, что вы настраивали руками в интерфейсе. | `postgres` |
| **ClickHouse** | Сами события об ошибках, спаны трейсов, точки метрик, сэмплы профилей, результаты аптайм-проверок — весь объём телеметрии, которую прислали ваши приложения. | `clickhouse` |

Если восстановить только одну из баз — интерфейс либо сломается (проект есть в UI, но для него нет ни одного события), либо, наоборот, вы потеряете саму настройку (алерты, участников, DSN-ключи), даже если телеметрия цела.

Все команды ниже выполняются **из папки репозитория** (`gotcha/`, там же, где `docker-compose.yml`) и используют `docker compose exec` — то есть выполняют команду внутри уже запущенного контейнера, без необходимости пробрасывать порты баз наружу (они и не проброшены — см. [Установку](/docs/installation)). Это относится к Docker-поставке; для установки без Docker — раздел «Bare-metal: то же самое без Docker» в конце страницы.

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

`SHOW TABLES` вернёт больше строк, чем таблиц для выгрузки — не все из них нужно объяснять по отдельности, но ни одна не должна остаться загадкой. Помимо семи таблиц ниже, вы увидите:

- `transactions_5m`, `web_vitals_5m` — **материализованные представления**. Их выгружать и восстанавливать НЕ нужно: они наполняются автоматически при вставке в исходные таблицы, и восстановление их содержимого рядом с восстановлением `transactions` удвоит агрегаты — «Производительность» покажет вдвое завышенный трафик.
- `.inner_id.<uuid>` (по одной на каждое материализованное представление, то есть две строки) — служебное хранилище самого представления. ClickHouse создаёт и наполняет его автоматически вместе с представлением; отдельно выгружать и восстанавливать не нужно и невозможно (это не проект-таблица, у неё нет собственной схемы для миграций).
- `schema_migrations` — служебная таблица инструмента миграций Gotcha, хранит номер применённой версии схемы. Её не выгружают: версию схемы восстанавливает сам Gotcha на шаге `--migrate-only` (см. «Restore: PostgreSQL» ниже).

Итого 7 (список для выгрузки) + 2 (представления) + 2 (их служебное хранилище) + 1 (`schema_migrations`) = 12 строк на актуальной схеме. Список таблиц для выгрузки фиксирован и приведён ниже.

Выгрузите каждую из них:

```bash
mkdir -p backup/clickhouse
for t in events transactions spans metric_points profile_samples check_results logs; do
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

## Бэкапьте `.env`, а не только базы

`GOTCHA_SECRET_KEY` шифрует секреты at-rest: client secret SSO, токены
Telegram-ботов и ключи подписи вебхуков. Он живёт только в вашем `.env` (или в
блоке `environment:` compose-файла) и никогда не попадает в базу — поэтому дамп
PostgreSQL и ClickHouse сам по себе **не** является полным бэкапом.

Восстановите базы с другим ключом — и эти секреты больше не расшифруются.
Затронутые каналы алертов перестанут доставлять уведомления, но останутся
видны на странице оповещений с пометкой «Секрет не читается»: введите секрет
заново прямо там, и доставка восстановится. SSO организации в этом случае
перестанет работать, и его настройки придётся ввести повторно.

```bash
cp .env "$BACKUP_DIR/env-$(date +%F)"
chmod 600 "$BACKUP_DIR/env-$(date +%F)"
```

Храните его так же бережно, как сами дампы: это ключ ко всему зашифрованному
внутри них. Процедуры перевыпуска ключа нет — если он потерян, зашифрованные
секреты придётся ввести заново руками.

## Restore: PostgreSQL

Восстанавливать нужно в **пустую** базу и **до** старта приложения. Приложение при старте само применяет миграции (`GOTCHA_AUTO_MIGRATE_ENABLED=true` по умолчанию) — то есть создаёт все таблицы прежде, чем откроет порт, — и дамп, накатанный поверх, встретит уже существующую схему.

Восстановление полной копии (обе базы) — это один сквозной порядок, а не две независимые процедуры. Дамп PostgreSQL несёт собственную схему (`CREATE TABLE` внутри самого дампа), а вот в ClickHouse `Native`-дамп — это только строки: схему для него создаёт миграциями сам Gotcha. Между восстановлением PostgreSQL и вставкой в ClickHouse обязателен промежуточный шаг — применение миграций без запуска приложения, иначе таблиц ClickHouse ещё не существует и вставлять некуда:

Если восстанавливаемый архив старше текущего значения `*_RETENTION_DAYS`, шаг 4 применит TTL раньше, чем строки окажутся в таблице, — и после вставки на шаге 5 они переживут только до ближайшего фонового мерджа ClickHouse, а не отчёт об успехе. Признак этого — предупреждение `retention: rows already older than the active window exist` в логе первого старта приложения на шаге 6: увидев его, поднимите нужный `*_RETENTION_DAYS` (или временно `0`) до этого старта, если данные нужны на весь их исходный срок.

```bash
# 1. Поднять ТОЛЬКО базы, без приложения: иначе оно создаст схему раньше дампа.
docker compose up -d postgres clickhouse

# 2. Пересоздать базу PostgreSQL начисто.
docker compose exec -T postgres psql -U gotcha -d postgres \
  -c 'DROP DATABASE IF EXISTS gotcha' -c 'CREATE DATABASE gotcha'

# 3. Накатить дамп PostgreSQL, останавливаясь на первой же ошибке.
gunzip -c backup/postgres-2026-07-01.sql.gz \
  | docker compose exec -T postgres psql -v ON_ERROR_STOP=1 --single-transaction -U gotcha -d gotcha

# 4. Применить миграции без запуска приложения: это создаёт схему ClickHouse
#    (и, если нужно, доводит схему PostgreSQL до версии бинаря), но не
#    открывает порт и не поднимает фоновые обработчики — вставлять в CH
#    можно сразу после этой команды, не опасаясь гонки с живым приложением.
docker compose run --rm --no-deps gotcha --migrate-only

# 5. Восстановить ClickHouse — см. раздел «Restore: ClickHouse» ниже.

# 6. И только теперь поднять приложение целиком.
docker compose up -d
```

`ON_ERROR_STOP=1` и `--single-transaction` здесь обязательны, а не для красоты. Без них `psql` печатает поток ошибок `relation ... already exists` и `duplicate key`, **завершается с кодом 0** и выглядит как успешное восстановление. При этом `COPY` в таблицу, у которой в новой схеме появились колонки, не проходит вовсе — и отличить ожидаемый шум от настоящего провала по такому выводу невозможно. С этими двумя флагами первая же ошибка останавливает восстановление, а транзакция откатывается целиком: либо восстановилось всё, либо база осталась пустой и это видно.

Если приложение уже работает на этой базе — остановите его (`docker compose stop gotcha`) до шага 2. Восстановление под работающим приложением означает, что оно продолжает писать в ту же базу параллельно.

## Restore: ClickHouse

Восстановление таблицы, выгруженной в формате `Native`, обратной командой:

```bash
cat backup/clickhouse/events-2026-07-01.native | \
  docker compose exec -T clickhouse clickhouse-client \
    --user gotcha --password gotcha --database gotcha \
    --query "INSERT INTO events FORMAT Native"
```

Повторите для каждой таблицы. Таблица должна существовать (её создаёт шаг 4 из раздела «Restore: PostgreSQL» выше, `--migrate-only`) и быть пустой, иначе данные добавятся к уже имеющимся, а не заменят их.

**Восстанавливаете во второй раз, в уже наполненную базу?** Материализованные представления (`transactions_5m`, `web_vitals_5m`) нужно очистить **до** вставки в исходные таблицы, а не после. Они наполняются самой вставкой в `transactions` — очистка, выполненная постфактум, стирает и то, что вставка только что туда добавила, и раздел «Производительность» останется пустым при формально успешном восстановлении:

```bash
docker compose exec -T clickhouse clickhouse-client \
  --user gotcha --password gotcha --database gotcha \
  --query "TRUNCATE TABLE transactions_5m"
docker compose exec -T clickhouse clickhouse-client \
  --user gotcha --password gotcha --database gotcha \
  --query "TRUNCATE TABLE web_vitals_5m"
```

и только потом — вставка `Native`-дампов по команде выше.

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
curl -sf http://localhost:59080/readyz
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
day=$(date +%F)

# Чистка старого — в trap, и регистрируется он ДО первой команды, которая может
# упасть. Иначе при set -e чистка не выполнилась бы именно тогда, когда выгрузка
# упала: до строки в конце скрипта он просто не дошёл бы, и место кончилось бы
# ровно тогда, когда бэкапы и так не снимаются.
trap 'find backup -type f -name "*.tmp" -delete; find backup -type f -mtime +14 -delete' EXIT

# Пишем во временный файл и переименовываем только после успеха. Без этого
# перенаправление создаёт файл ДО того, как pg_dump успеет отработать: он упал
# — а в папке лежит пустой .sql.gz, неотличимый от настоящей резервной копии,
# пока она не понадобится.
docker compose exec -T postgres pg_dump -U gotcha -d gotcha \
  | gzip > backup/postgres-$day.sql.gz.tmp
mv backup/postgres-$day.sql.gz.tmp backup/postgres-$day.sql.gz

for t in events transactions spans metric_points profile_samples check_results logs; do
  docker compose exec -T clickhouse clickhouse-client \
    --user gotcha --password gotcha --database gotcha \
    --query "SELECT * FROM $t FORMAT Native" \
    > backup/clickhouse/$t-$day.native.tmp
  mv backup/clickhouse/$t-$day.native.tmp backup/clickhouse/$t-$day.native
done
```

Не забудьте сделать скрипт исполняемым (`chmod +x backup.sh`) и, что важно, копировать содержимое папки `backup/` **за пределы этого же сервера** (другой диск, S3-совместимое хранилище, другой сервер) — локальная копия не спасёт при выходе из строя самого сервера.

## Bare-metal: то же самое без Docker

Если Gotcha установлен `install-bare-metal.sh` ([Установка без Docker](/docs/installation-bare-metal)), PostgreSQL и ClickHouse — системные сервисы, а не контейнеры: `docker compose exec` заменяется прямым вызовом `pg_dump`/`psql`/`clickhouse-client` от системного пользователя, а именованные тома Docker — путями пакетов на диске (`/var/lib/postgresql`, `/var/lib/clickhouse`).

### Backup: PostgreSQL

Ровно эту команду, только автоматически и перед каждым обновлением, выполняет сам инсталлятор (см. [Обновление](/docs/upgrade) — раздел «Обновление bare-metal-инсталляции»):

```bash
mkdir -p /var/lib/gotcha/backup && chmod 700 /var/lib/gotcha/backup
sudo -u postgres pg_dump -d gotcha | gzip > /var/lib/gotcha/backup/postgres-$(date +%F).sql.gz
chmod 600 /var/lib/gotcha/backup/postgres-$(date +%F).sql.gz
```

### Backup: ClickHouse

Тот же список из семи таблиц, та же оговорка про материализованные представления (`transactions_5m`, `web_vitals_5m`) и `schema_migrations`, что и в Docker-разделе выше — меняется только вызов `clickhouse-client`, без `docker compose exec`.

Пароль лежит ровно в одном месте — в `GOTCHA_CH_DSN` внутри `/etc/gotcha/gotcha.env`; в `/etc/clickhouse-server/users.d/10-gotcha.xml` хранится только его `password_sha256_hex`, из которого пароль не восстановить:

```bash
CH_PASSWORD=$(sed -n 's#^GOTCHA_CH_DSN=clickhouse://gotcha:\([^@]*\)@.*#\1#p' \
  /etc/gotcha/gotcha.env)
mkdir -p /var/lib/gotcha/backup/clickhouse
for t in events transactions spans metric_points profile_samples check_results logs; do
  clickhouse-client --user gotcha --password "$CH_PASSWORD" --database gotcha \
    --query "SELECT * FROM $t FORMAT Native" \
    > /var/lib/gotcha/backup/clickhouse/$t-$(date +%F).native
done
```

Снапшот файловой системы с остановкой сервисов — теми же путями, которые создаёт сам пакет, вместо именованных томов Docker:

```bash
systemctl stop gotcha postgresql clickhouse-server
tar czf /var/lib/gotcha/backup/volumes-$(date +%F).tar.gz \
  /var/lib/postgresql /var/lib/clickhouse
systemctl start postgresql clickhouse-server gotcha
```

Не забудьте и `/etc/gotcha/gotcha.env` — предупреждение про `GOTCHA_SECRET_KEY` из раздела «Бэкапьте `.env`, а не только базы» выше действует буквально, только путь другой:

```bash
cp /etc/gotcha/gotcha.env "/var/lib/gotcha/backup/env-$(date +%F)"
chmod 600 "/var/lib/gotcha/backup/env-$(date +%F)"
```

### Restore: PostgreSQL

Тот же порядок шагов, что и в Docker-разделе выше (сначала PostgreSQL, потом миграции без запуска приложения, потом ClickHouse, потом приложение целиком) — сервисами управляет `systemctl`, а не `docker compose`:

```bash
# 1. Остановить приложение, оставить PostgreSQL/ClickHouse работающими.
systemctl stop gotcha

# 2. Пересоздать базу PostgreSQL начисто.
sudo -u postgres psql -d postgres \
  -c 'DROP DATABASE IF EXISTS gotcha' -c 'CREATE DATABASE gotcha OWNER gotcha'

# 3. Накатить дамп, останавливаясь на первой же ошибке.
gunzip -c /var/lib/gotcha/backup/postgres-2026-07-01.sql.gz \
  | sudo -u postgres psql -v ON_ERROR_STOP=1 --single-transaction -d gotcha

# 4. Применить миграции без запуска приложения — создаёт схему ClickHouse.
systemd-run --pipe --wait --collect --uid=gotcha --gid=gotcha \
  --property="EnvironmentFile=/etc/gotcha/gotcha.env" \
  /usr/local/bin/gotcha --migrate-only

# 5. Восстановить ClickHouse — раздел «Restore: ClickHouse» ниже.

# 6. Запустить приложение.
systemctl start gotcha
```

`ON_ERROR_STOP=1` и `--single-transaction` обязательны по той же причине, что и в Docker-разделе выше — без них частичный провал восстановления выглядит как успех.

### Restore: ClickHouse

```bash
cat /var/lib/gotcha/backup/clickhouse/events-2026-07-01.native | \
  clickhouse-client --user gotcha --password "$CH_PASSWORD" --database gotcha \
    --query "INSERT INTO events FORMAT Native"
```

Повторите для каждой таблицы. Та же оговорка про очистку `transactions_5m`/`web_vitals_5m` **до** вставки при повторном восстановлении в непустую базу — тем же `clickhouse-client`, без `docker compose exec`.

### Restore из снапшота файловой системы

```bash
systemctl stop gotcha postgresql clickhouse-server
rm -rf /var/lib/postgresql/17/main/* /var/lib/clickhouse/*
tar xzf /var/lib/gotcha/backup/volumes-2026-07-01.tar.gz -C /
systemctl start postgresql clickhouse-server gotcha
```

Пути `/var/lib/postgresql/17/main` и `/var/lib/clickhouse` — стандартные каталоги данных пакетов `postgresql-17`/`clickhouse-server` на Debian/Ubuntu. **Разрушительная операция**, та же оговорка, что и в Docker-разделе: убедитесь, что архив тот, что нужен, прежде чем запускать.

### Проверка и cron

Самопроверка после восстановления та же, что в Docker-разделе выше, только без `docker compose exec`:

```bash
/usr/local/bin/gotcha --healthcheck
curl -sf http://127.0.0.1:8080/readyz
```

Структура cron-задания та же, что в примере выше — команды внутри `backup.sh` заменяются на `sudo -u postgres pg_dump`/`clickhouse-client` без `docker compose exec`, каталог бэкапов — `/var/lib/gotcha/backup` вместо `backup/` в папке репозитория.

## Что дальше

- [Установка](/docs/installation).
- [Установка без Docker](/docs/installation-bare-metal).
- [Обновление](/docs/upgrade) — резервную копию нужно снимать перед каждым обновлением.
- [Конфигурация](/docs/configuration) — переменные `GOTCHA_*_RETENTION_DAYS`, влияющие на то, сколько данных вообще накапливается в ClickHouse.
