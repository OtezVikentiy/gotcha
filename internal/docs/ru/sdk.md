# SDK и интеграции

Gotcha принимает данные по протоколу приёма Sentry и по OTLP (метрики). Чтобы отправлять события, используйте официальный Sentry SDK вашего языка и укажите DSN проекта Gotcha (см. «Настройки проекта → Подключение»). Ниже — установка и минимальная инициализация. Замените `<ВАШ_DSN>` на DSN проекта.

## Go
```
go get github.com/getsentry/sentry-go
```
Репозиторий: https://github.com/getsentry/sentry-go
```go
sentry.Init(sentry.ClientOptions{ Dsn: "<ВАШ_DSN>" })
```

## PHP
```
composer require sentry/sentry
```
Репозиторий: https://github.com/getsentry/sentry-php
```php
\Sentry\init(['dsn' => '<ВАШ_DSN>']);
```

## JavaScript (браузер)
```
npm i @sentry/browser
```
Репозиторий: https://github.com/getsentry/sentry-javascript
```js
Sentry.init({ dsn: "<ВАШ_DSN>" });
```

## Python
```
pip install sentry-sdk
```
Репозиторий: https://github.com/getsentry/sentry-python
```python
import sentry_sdk
sentry_sdk.init(dsn="<ВАШ_DSN>")
```

## Метрики (OTLP)
Метрики можно отправлять по протоколу OpenTelemetry (OTLP) — настройте OTLP-экспортёр на эндпойнт приёма метрик Gotcha. Точный адрес — в конфигурации вашей инсталляции.

> DSN проекта: возьмите на странице **Настройки проекта → Подключение**.
