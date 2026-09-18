# Усиление установки

Базовый чек-лист продакшена — в [Установке](/docs/installation), таблица переменных HSTS и
их взаимные ограничения — в [Конфигурации](/docs/configuration) (секция Security). Эта
страница — свод: что из защиты периметра закрывает обратный прокси перед приложением, что
закрывает само приложение, и как проверить обе половины после деплоя.

## Граница ответственности

Обратный прокси закрывает то, до чего приложению снизу не дотянуться: терминирует TLS,
подменяет собой дефолтные страницы ошибок веб-сервера/панели хостинга (в которых обычно
светится версия nginx или Traefik), ограничивает набор HTTP-методов и решает, какие пути
вообще видны снаружи. Приложение закрывает то, что живёт внутри самого ответа: security-заголовки
на каждой странице, `Strict-Transport-Security` при HTTPS-`GOTCHA_BASE_URL`, отсутствие
обслуживания TRACE и страницы ошибок без номера версии и стека вызовов. Ни одна половина не
подменяет другую — прокси без этих настроек оставляет дыры, которые приложение закрыть не
может в принципе (версия nginx уходит до того, как запрос вообще доедет до Go-процесса), а
голое приложение без прокси перед ним — это HTTP без TLS и открытые наружу служебные пути.

## Обратный прокси

Три настройки, которые стоит выставить в любом прокси перед Gotcha: `server_tokens off`,
чтобы не светить версию самого прокси в заголовках и дефолтных страницах ошибок; собственный
`error_page` вместо страницы nginx/Traefik/панели хостинга по умолчанию; ограничение методов
до тех, что реально нужны, — `GET`, `POST`, `HEAD`.

```nginx
server_tokens off;

location / {
    limit_except GET POST HEAD { deny all; }
    proxy_intercept_errors on;
    error_page 404 500 502 503 504 /error.html;
    proxy_pass http://127.0.0.1:59080;
}
```

`proxy_intercept_errors on` обязателен вместе с `error_page` — без него nginx проксирует
готовую страницу ошибки от бэкенда как есть, а не подменяет её своей.

## Что не выставлять наружу

`/metrics`, `/version`, `/healthz`, `/readyz` — служебные ручки, нужные изнутри (оркестратору,
системе мониторинга, вам самим по SSH-туннелю), а не публике. Ни одна не требует
аутентификации. `/version`, `/healthz` и `/readyz` анонимно отдают точную версию сборки
(поля `version`/`commit` в теле ответа) — а точная версия сужает атакующему поиск
уязвимостей до тех, что закрыты именно в этом релизе, вместо перебора вслепую. Подробнее об
этих ручках и о том, что именно они отдают, — [Мониторинг самого gotcha](/docs/self-monitoring).

Приложение не может определить, опубликован ли его порт наружу (`GOTCHA_COMPOSE_BIND` —
переменная самого Docker Compose, до процесса не доходит), поэтому при каждом старте пишет
в лог предупреждение об этих четырёх ручках безусловно, даже если вы уже всё закрыли прокси.
Это не диагностика поломки — это напоминание проверить конфигурацию ниже.

```nginx
location ~ ^/(metrics|version|healthz|readyz)$ {
    allow 10.0.0.0/8;
    allow 127.0.0.1;
    deny all;
    proxy_pass http://127.0.0.1:59080;
}
```

```caddy
@internal path /metrics /version /healthz /readyz
handle @internal {
    @allowed remote_ip 10.0.0.0/8 127.0.0.1
    handle @allowed { reverse_proxy localhost:59080 }
    respond 403
}
```

Замените `10.0.0.0/8` на диапазон, из которого реально приходят ваши пробы (оркестратор,
Prometheus, ваша сеть) — открытый по умолчанию диапазон бессмысленен как ограничение.

На bare-metal сайт nginx, который ставит `install-bare-metal.sh`, этого ограничения по
умолчанию не содержит — он проксирует весь `/` без разбора путей (см.
[Установку без Docker](/docs/installation-bare-metal)). Добавьте `location`-блок для
`/metrics`/`/version`/`/healthz`/`/readyz` из примера выше в
`/etc/nginx/sites-available/gotcha` вручную и перезагрузите конфиг (`nginx -t &&
systemctl reload nginx`) — иначе эти четыре ручки открыты всему интернету так же, как без
любого прокси вообще.

То же относится к базам. Штатный `docker-compose.yml` не публикует порты PostgreSQL и
ClickHouse на хост — до них добираются только контейнеры той же docker-сети, — но пароль
у обеих по умолчанию `gotcha` / `gotcha`, и он одинаков у каждой установки в мире. Смените
его через `GOTCHA_COMPOSE_PG_PASSWORD` / `GOTCHA_COMPOSE_CH_PASSWORD` **до первого старта**
(после инициализации тома переменная сама по себе уже ничего не меняет), а на живой
установке — сначала `ALTER USER` в самой базе, потом переменная; команды — в
[Конфигурации](/docs/configuration#peremennye-tolko-dlya-compose-konteynery-baz). И не
добавляйте базам `ports:` «для удобства»: с дефолтным паролем это открытая база на публичном
адресе. На bare-metal этой дыры нет по конструкции: `install-bare-metal.sh` генерирует
случайный пароль каждой базе при первой установке (`openssl rand -hex 24`), общего для всех
инсталляций пароля там не существует — PostgreSQL и ClickHouse к тому же слушают только
`127.0.0.1`, порты наружу не открыты вовсе, если не задавать их вручную.

## TLS и HSTS

TLS — не ниже версии 1.2, с редиректом с голого HTTP на HTTPS. HSTS настраивается ровно в
ОДНОМ месте — либо на прокси, либо в приложении, никогда в обоих сразу: два источника
заголовка на одном ответе не складываются, а просто маскируют друг друга. Если HSTS уже
шлёт прокси, приложению задайте `GOTCHA_HSTS_ENABLED=false`.

Приложение собирает заголовок из четырёх переменных:

| Переменная | Дефолт | Смысл |
|---|---|---|
| `GOTCHA_HSTS_ENABLED` | `true` | Отправлять ли `Strict-Transport-Security` вообще (только на https-ответах). |
| `GOTCHA_HSTS_MAX_AGE_SECONDS` | `31536000` | На сколько секунд браузеру запомнить требование HTTPS (дефолт — год); `0` — не «выключено», а осознанный аварийный откат, см. ниже. |
| `GOTCHA_HSTS_INCLUDE_SUBDOMAINS` | `false` | Распространять требование HTTPS на все поддомены хоста из `GOTCHA_BASE_URL`. |
| `GOTCHA_HSTS_PRELOAD` | `false` | Помечать инстанс кандидатом на списки предзагрузки браузеров. |

Точные правила отказа старта, поведение при `MAX_AGE_SECONDS=0` и почему выключение HSTS не
снимает уже выданный браузером пин — в [Конфигурации](/docs/configuration#security-bezopasnost).

Включайте `includeSubDomains`, только если контролируете (или уже проверили HTTPS на) весь
родительский домен целиком: `gotcha.example.com` с этим флагом требует HTTPS не только от
себя, а от каждого сервиса на `example.com`, включая те, что вы не администрируете и которые
могут быть не готовы к HTTPS.

Preload — билет в один конец: попав в список предзагрузки, домен зашивается в релизы
браузеров на месяцы вперёд, и снять его оттуда — вопрос месяцев, а не минут. Выход при
аварии — строго в этом порядке, иначе приложение откажется стартовать (валидация конфига
требует max-age не меньше года, пока `PRELOAD=true`, — см.
[Конфигурацию](/docs/configuration#security-bezopasnost)):

1. `GOTCHA_HSTS_PRELOAD=false` — снять требование годового `max-age`, которое иначе не даст
   уйти в шаг 2.
2. `GOTCHA_HSTS_MAX_AGE_SECONDS=0`, оставив `GOTCHA_HSTS_ENABLED=true` — заголовок с нулевым
   max-age реально уходит клиентам и снимает пин.
3. Дождаться, пока пин истечёт у уже посетивших инстанс клиентов.
4. Только теперь `GOTCHA_HSTS_ENABLED=false`, если заголовок больше не нужен вовсе.

Выключенный HSTS пин сам по себе **не снимает** — он лишь перестаёт продлеваться, поэтому
шаг 4 без шагов 1–3 не отменяет аварию, а замораживает её на срок ранее выданного max-age.

## Bare-metal: юнит systemd вместо рантайма контейнера

На установке без Docker ([Установка без Docker](/docs/installation-bare-metal)) то же
усиление процесса, которое в Docker-пути даёт рантайм контейнера, обеспечивает
systemd-юнит `/etc/systemd/system/gotcha.service`, который пишет `install-bare-metal.sh`.
Паритет построчно:

| Docker Compose | systemd-юнит |
|---|---|
| `read_only: true` | `ProtectSystem=strict` (запись разрешена только в `StateDirectory=`) |
| `tmpfs: [/tmp]` | `PrivateTmp=yes` |
| `cap_drop: [ALL]` | `CapabilityBoundingSet=` и `AmbientCapabilities=` (пустые) |
| `no-new-privileges` | `NoNewPrivileges=yes` |
| `pids_limit: 512` | `TasksMax=512` |
| `mem_limit: 1g` | `MemoryMax=` + `MemoryAccounting=yes` + явный `GOMEMLIMIT` в `gotcha.env` |
| `stop_grace_period: 90s` | `TimeoutStopSec=90` |
| `restart: unless-stopped` | `Restart=always`, `RestartSec=5` |
| том выгрузок | `StateDirectory=gotcha`, `StateDirectoryMode=0700` |
| `logging: json-file` | journald (`journalctl -u gotcha`) |
| `depends_on: service_healthy` | `After=postgresql.service clickhouse-server.service network-online.target` |

Юнит идёт дальше паритета: `ProtectHome`, `PrivateDevices`, `ProtectKernelTunables`,
`ProtectKernelModules`, `ProtectKernelLogs`, `ProtectControlGroups`, `ProtectClock`,
`ProtectHostname`, `ProtectProc=invisible`, `RestrictNamespaces`, `RestrictRealtime`,
`RestrictSUIDSGID`, `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`, `LockPersonality`,
`SystemCallFilter=@system-service`, `SystemCallArchitectures=native`, `UMask=0077` и
`MemoryDenyWriteExecute=yes` — этих ограничений у контейнерного рантайма попросту нет, они
специфичны для systemd.

`After=` без `Requires=` — намеренно: перезапуск PostgreSQL или ClickHouse не должен тянуть
за собой перезапуск приложения, оно переживает временную недоступность базы и само
восстановится через `Restart=always`, когда база вернётся, — то же поведение, что и в
Docker-пути.

Проверьте фактически применённые ограничения после установки:

```bash
systemctl show gotcha.service -p MemoryDenyWriteExecute,ProtectSystem,NoNewPrivileges
systemd-analyze security gotcha.service
```

`systemd-analyze security` печатает по каждой директиве, что она даёт, и итоговую оценку —
это тот же по духу инструмент, что `docker inspect` для контейнерного рантайма, только
для юнита.

## security.txt

Приложение не отдаёт `/.well-known/security.txt` само — это осознанное решение, не
недоработка. Контакт для сообщений об уязвимостях — свойство ДОМЕНА, а не конкретного
приложения на нём: значительная часть установок Gotcha живёт на поддомене чужого домена
(общий хостинг, корпоративный портал), и владеет контактом безопасности владелец домена, а
не разработчик Gotcha. Положите файл на прокси:

```nginx
location = /.well-known/security.txt {
    default_type text/plain;
    return 200 "Contact: mailto:security@example.com\nExpires: 2027-01-01T00:00:00.000Z\n";
}
```

## Самопроверка

После деплоя проверьте обе половины периметра — прокси и приложение — набором `curl`:

```bash
# security-заголовки приложения на странице входа
curl -sI https://gotcha.example/login | grep -Ei 'content-security-policy|x-frame-options|strict-transport'

# HSTS есть на https-инстансе...
curl -sI https://gotcha.example/login | grep -i strict-transport
# ...и его нет на голом http-деплое, независимо от конфига
# (пусто, если только ваш прокси сам не вешает HSTS на редиректе http->https —
# рекомендуемая топология выше это допускает; тогда заголовок на 301 ожидаем)
curl -sI http://gotcha.example/login | grep -i strict-transport   # ожидается: пусто

# служебные пути закрыты прокси
curl -s -o /dev/null -w '%{http_code}\n' https://gotcha.example/metrics   # ожидается 403
curl -s -o /dev/null -w '%{http_code}\n' https://gotcha.example/version  # ожидается 403

# TRACE не обслуживается
curl -s -o /dev/null -w '%{http_code}\n' -X TRACE https://gotcha.example/login  # ожидается 404
```

Про TRACE отдельно: ожидается именно **404, а не 405**. Веб-слой перехватывает любой метод
на этом пути общим catch-all'ом и всегда отвечает стилизованной страницей 404 — отсутствие
405 здесь не признак того, что метод TRACE чем-то обслуживается, это признак того, что
маршрут вообще не различает методы на уровне mux.
