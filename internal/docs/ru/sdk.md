# SDK и интеграции

Gotcha не имеет собственного протокола отправки данных — она принимает события и транзакции по протоколу приёма Sentry, а метрики и профили — по OTLP и pprof соответственно. Это значит, что для подключения приложения не нужен никакой особый «Gotcha SDK»: вы ставите **официальный Sentry SDK** своего языка и передаёте ему DSN своего проекта Gotcha. Дальше SDK работает ровно так, как описано в его собственной документации — Gotcha просто оказывается на другом конце DSN вместо `sentry.io`.

Ниже — установка и минимальная инициализация для PHP (в том числе Laravel), JavaScript/Node, JavaScript в браузере, Python и Go, затем — окружение и релиз, отправка производительности (трейсинг), указатели на приём метрик и профилей, и раздел с типичными проблемами подключения.

## Где взять DSN

DSN проекта — на странице **«Подключение»**: после создания проекта вас автоматически перенаправляет туда (URL вида `/projects/<id>/setup`), а вернуться позже можно кнопкой **«Подключение SDK»** в списке проектов или из блока **«DSN-ключи»** на странице «Настройки проекта». DSN выглядит так:

```
https://<PUBLIC_KEY>@<адрес_gotcha>/<ID_ПРОЕКТА>
```

Во всех примерах ниже замените `<ВАШ_DSN>` на эту строку целиком.

## PHP (обычный проект)

```bash
composer require sentry/sentry
```

Репозиторий: https://github.com/getsentry/sentry-php

```php
<?php
require __DIR__ . '/vendor/autoload.php';

\Sentry\init([
    'dsn' => '<ВАШ_DSN>',
    'environment' => getenv('APP_ENV') ?: 'production',
    'traces_sample_rate' => 0.2, // включает трейсинг производительности; см. раздел ниже
]);
```

Отправить тестовую ошибку — либо просто бросить необработанное исключение (SDK сам подхватит его через `set_exception_handler`), либо явно:

```php
try {
    throw new \RuntimeException('Тестовая ошибка Gotcha');
} catch (\Throwable $e) {
    \Sentry\captureException($e);
}
```

Событие появится в разделе **«Проблемы»** вашего проекта в течение нескольких секунд.

## PHP (Laravel)

```bash
composer require sentry/sentry-laravel
```

Репозиторий: https://github.com/getsentry/sentry-laravel

Опубликуйте конфиг и сразу пропишите DSN (команда добавит переменную в `.env` и создаст `config/sentry.php`):

```bash
php artisan sentry:publish --dsn=<ВАШ_DSN>
```

Либо вручную добавьте в `.env`:

```
SENTRY_LARAVEL_DSN=<ВАШ_DSN>
SENTRY_TRACES_SAMPLE_RATE=0.2
SENTRY_ENVIRONMENT=production
```

Пакет сам регистрирует обработчик исключений Laravel — ничего дополнительно инициализировать не нужно. Отправить тестовое событие:

```bash
php artisan sentry:test
```

Событие появится в **«Проблемах»** проекта, DSN которого вы указали.

## JavaScript / Node.js (сервер)

```bash
npm install @sentry/node
```

Репозиторий: https://github.com/getsentry/sentry-javascript

```js
const Sentry = require("@sentry/node");
// или: import * as Sentry from "@sentry/node";

Sentry.init({
  dsn: "<ВАШ_DSN>",
  environment: process.env.NODE_ENV || "production",
  tracesSampleRate: 0.2,
});
```

Тестовая ошибка:

```js
try {
  throw new Error("Тестовая ошибка Gotcha");
} catch (e) {
  Sentry.captureException(e);
}
```

Перед завершением короткоживущего процесса (скрипт, serverless-функция, воркер, который сразу выходит) обязательно дождитесь отправки буфера:

```js
await Sentry.close(2000); // ждём до 2с, чтобы событие успело уйти
```

## JavaScript (браузер)

```bash
npm install @sentry/browser
```

```js
import * as Sentry from "@sentry/browser";

Sentry.init({
  dsn: "<ВАШ_DSN>",
  environment: "production",
  tracesSampleRate: 0.2, // трейсинг + автосбор Web Vitals (LCP/INP/CLS/FCP/TTFB)
});
```

Тестовая ошибка — любое необработанное исключение в коде страницы поймается автоматически; явно:

```js
Sentry.captureException(new Error("Тестовая ошибка Gotcha"));
```

Если у сайта настроен Content-Security-Policy с директивой `connect-src`, добавьте туда адрес вашего инстанса Gotcha — иначе браузер молча заблокирует запрос к DSN.

## Python

```bash
pip install sentry-sdk
```

Репозиторий: https://github.com/getsentry/sentry-python

```python
import sentry_sdk

sentry_sdk.init(
    dsn="<ВАШ_DSN>",
    environment="production",
    traces_sample_rate=0.2,
)
```

Тестовая ошибка:

```python
try:
    raise RuntimeError("Тестовая ошибка Gotcha")
except Exception:
    sentry_sdk.capture_exception()
```

Для Django/Flask/FastAPI и других фреймворков `sentry-sdk` поднимает нужную интеграцию автоматически при её обнаружении в окружении — отдельно ничего включать не нужно, `sentry_sdk.init(...)` в точке входа приложения достаточно.

## Go

```bash
go get github.com/getsentry/sentry-go
```

Репозиторий: https://github.com/getsentry/sentry-go

```go
package main

import (
	"errors"
	"time"

	"github.com/getsentry/sentry-go"
)

func main() {
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              "<ВАШ_DSN>",
		Environment:      "production",
		TracesSampleRate: 0.2,
	})
	if err != nil {
		panic(err)
	}
	// ОБЯЗАТЕЛЬНО: без Flush процесс может завершиться раньше, чем
	// буфер событий уйдёт по сети.
	defer sentry.Flush(2 * time.Second)

	sentry.CaptureException(errors.New("тестовая ошибка Gotcha"))
}
```

## Окружение и релиз

`environment` (`environment`/`Environment` в зависимости от языка) и `release` — два поля, которые стоит проставлять с самого начала: они привязываются к каждому событию, транзакции и метрике и используются для фильтрации почти во всех разделах Gotcha.

```php
\Sentry\init([
    'dsn' => '<ВАШ_DSN>',
    'environment' => 'staging',
    'release' => 'my-app@' . trim(shell_exec('git rev-parse --short HEAD')),
]);
```

Так продовые ошибки не тонут среди тестового стенда, а по регрессии производительности видно, в каком именно релизе она началась.

## Производительность и трейсинг (транзакции)

Параметр `traces_sample_rate` (`tracesSampleRate` в JS) включает создание транзакций — единиц измерения производительности, из которых строится раздел [Производительность](/docs/performance). Значение — доля запросов, для которых создаётся трейс: `1.0` — все запросы, `0.2` — каждый пятый, `0` (по умолчанию) — трейсинг выключен, будут отправляться только ошибки.

Дополнительно на стороне сервера в **«Настройках проекта → Performance»** можно задать серверную долю семплирования (`sample_rate`, 0..1) — она применяется поверх того, что уже отправил SDK, и позволяет снизить объём хранимых транзакций без переразвёртывания приложения. В браузере с включённым трейсингом автоматически собираются и [Web Vitals](/docs/glossary) (LCP/INP/CLS/FCP/TTFB) — отдельно включать их не нужно.

## Метрики (OTLP)

Числовые метрики (counter/gauge/histogram) Gotcha принимает по протоколу OpenTelemetry (OTLP/HTTP), а не через Sentry SDK. Настройте OTLP-экспортёр вашего приложения (или OpenTelemetry Collector) на:

```
POST https://<адрес_gotcha>/v1/metrics
Authorization: Bearer <PUBLIC_KEY>
```

`<PUBLIC_KEY>` — это публичный ключ из DSN проекта (часть между `https://` и `@`), а не DSN целиком. У большинства OTel-экспортёров для заголовков есть штатная опция (`headers:` в конфиге коллектора, `OTEL_EXPORTER_OTLP_METRICS_HEADERS` в переменных окружения). Подробнее о типах метрик, агрегациях и оповещениях по порогам: [Метрики](/docs/metrics).

## Профилирование (pprof)

Помимо профилей, которые присылает Sentry SDK вместе с трейсами (`profiles_sample_rate` в Python/PHP/JS/Go SDK), Gotcha принимает и «сырые» pprof-профили напрямую:

```
POST https://<адрес_gotcha>/profiles/pprof?service=<имя_сервиса>&environment=<окружение>
Authorization: Bearer <PUBLIC_KEY>
Content-Type: application/octet-stream
```

Тело — обычный gzip'нутый pprof-профиль (например, результат `go tool pprof` или `runtime/pprof`). Подробнее о флеймграфах, in-app/system-кадрах и регрессиях: [Профилирование](/docs/profiling).

## Если событие не доходит

Если после отправки ошибки в «Проблемах» ничего не появилось за разумное время, по порядку проверьте:

| Причина | Как проверить |
|---|---|
| **Неверный или отозванный DSN** | Сверьте DSN с тем, что показан в «Настройках проекта → DSN-ключи»; ingest отвечает `401`/`403` на неизвестный или отозванный публичный ключ. Если ключ был перевыпущен — старый DSN перестаёт работать сразу. |
| **DSN от чужого проекта** | `project_id` в DSN должен совпадать с проектом самого ключа — иначе ingest вернёт `403 sentry_key does not match project`. |
| **Сеть/файрвол** | Приложение должно уметь достучаться по HTTPS/HTTP до адреса Gotcha (`GOTCHA_BASE_URL` инстанса) — проверьте `curl -i <адрес_gotcha>/healthz` с той же машины/контейнера, откуда работает приложение. Корпоративный прокси или egress-файрвол могут молча резать исходящие запросы. |
| **Событие/тело слишком большое** | По умолчанию ingest принимает тело не больше 1 МБ (`GOTCHA_MAX_EVENT_BYTES` на инстансе); превышение — `413`. Актуально для событий с очень длинными стектрейсами или большими breadcrumbs. |
| **Квота организации исчерпана** | Ingest отвечает `429` с `Retry-After`, если месячная квота исчерпана; проверьте «Настройки организации → Использование и лимиты». В `oss`-редакции по умолчанию квоты не ограничены (`0`), но админ инстанса мог их выставить. |
| **SDK не успел отправить данные** | В коротких процессах (CLI-скрипты, serverless, воркеры) вызовите `Flush`/`close` перед выходом — см. примеры для Go и Node выше; без этого буфер событий может просто не уйти по сети. |
| **CSP блокирует запрос в браузере** | Если на странице задан `Content-Security-Policy`, добавьте адрес Gotcha в `connect-src`. |

Если ни один пункт не помог — включите отладочный режим SDK (`debug: true` у большинства Sentry SDK) и посмотрите, что он логирует при попытке отправки: это почти всегда указывает точную причину.
