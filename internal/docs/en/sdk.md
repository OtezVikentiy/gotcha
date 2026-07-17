# SDK & Integrations

Gotcha accepts data via the Sentry ingestion protocol and via OTLP (metrics). To send events, use the official Sentry SDK for your language and point it at your Gotcha project's DSN (see "Project Settings → Connect"). Below is the install and minimal init for each. Replace `<YOUR_DSN>` with your project's DSN.

## Go
```
go get github.com/getsentry/sentry-go
```
Repository: https://github.com/getsentry/sentry-go
```go
sentry.Init(sentry.ClientOptions{ Dsn: "<YOUR_DSN>" })
```

## PHP
```
composer require sentry/sentry
```
Repository: https://github.com/getsentry/sentry-php
```php
\Sentry\init(['dsn' => '<YOUR_DSN>']);
```

## JavaScript (browser)
```
npm i @sentry/browser
```
Repository: https://github.com/getsentry/sentry-javascript
```js
Sentry.init({ dsn: "<YOUR_DSN>" });
```

## Python
```
pip install sentry-sdk
```
Repository: https://github.com/getsentry/sentry-python
```python
import sentry_sdk
sentry_sdk.init(dsn="<YOUR_DSN>")
```

## Metrics (OTLP)
Metrics can be sent using the OpenTelemetry protocol (OTLP) — point your OTLP exporter at Gotcha's metrics ingest endpoint. The exact address depends on your installation's configuration.

> Project DSN: find it on the **Project Settings → Connect** page.
