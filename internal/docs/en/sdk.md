# SDK & Integrations

Gotcha doesn't have its own wire protocol for sending data — it accepts events and transactions via the Sentry ingestion protocol, and metrics and profiles via OTLP and pprof respectively. That means connecting your application doesn't require any special "Gotcha SDK": you install the **official Sentry SDK** for your language and point it at your Gotcha project's DSN. From there the SDK behaves exactly as documented upstream — Gotcha just happens to be on the other end of the DSN instead of `sentry.io`.

Below: install and minimal init for PHP (including Laravel), JavaScript/Node, JavaScript in the browser, Python, and Go, followed by environment & release, sending performance data (tracing), pointers to metrics and profiling ingest, and a troubleshooting section for connection issues.

## Where to find your DSN

Your project's DSN lives on the **"Setup"** page: right after creating a project you're automatically redirected there (a URL like `/projects/<id>/setup`), and you can get back to it later via the **"Setup"** button in the projects list or the **"DSN keys"** section on the "Project settings" page. A DSN looks like this:

```
https://<PUBLIC_KEY>@<your-gotcha-host>/<PROJECT_ID>
```

In every example below, replace `<YOUR_PROJECT_DSN>` with this full string.

## PHP (plain project)

```bash
composer require sentry/sentry
```

Repository: https://github.com/getsentry/sentry-php

```php
<?php
require __DIR__ . '/vendor/autoload.php';

\Sentry\init([
    'dsn' => '<YOUR_PROJECT_DSN>',
    'environment' => getenv('APP_ENV') ?: 'production',
    'traces_sample_rate' => 0.2, // enables performance tracing; see the section below
]);
```

Send a test error — either just let an unhandled exception happen (the SDK hooks `set_exception_handler` automatically), or capture one explicitly:

```php
try {
    throw new \RuntimeException('Gotcha test error');
} catch (\Throwable $e) {
    \Sentry\captureException($e);
}
```

The event shows up under your project's **"Issues"** within a few seconds.

## PHP (Laravel)

```bash
composer require sentry/sentry-laravel
```

Repository: https://github.com/getsentry/sentry-laravel

Publish the config and set the DSN in one go (this adds the variable to `.env` and creates `config/sentry.php`):

```bash
php artisan sentry:publish --dsn=<YOUR_PROJECT_DSN>
```

Or add it to `.env` manually:

```
SENTRY_LARAVEL_DSN=<YOUR_PROJECT_DSN>
SENTRY_TRACES_SAMPLE_RATE=0.2
SENTRY_ENVIRONMENT=production
```

The package registers itself into Laravel's exception handler automatically — no extra init call is needed. Send a test event:

```bash
php artisan sentry:test
```

It shows up under **"Issues"** in the project whose DSN you configured.

## PHP (Symfony)

```bash
composer require sentry/sentry-symfony
```

Repository: https://github.com/getsentry/sentry-symfony

The default Flex recipe enables the bundle only in `prod` and without tracing. For a self-hosted setup it's handier to keep it active in every environment but gated on the DSN (empty = SDK off), and enable tracing via env — `config/packages/sentry.yaml`:

```yaml
sentry:
    dsn: '%env(SENTRY_DSN)%'
    options:
        traces_sample_rate: '%env(float:SENTRY_TRACES_SAMPLE_RATE)%'
        environment: '%kernel.environment%'
        # 404/405 are ordinary web noise (scanners, dead links), not app errors
        ignore_exceptions:
            - 'Symfony\Component\HttpKernel\Exception\NotFoundHttpException'
            - 'Symfony\Component\HttpKernel\Exception\MethodNotAllowedHttpException'
```

Register the bundle in every environment (`config/bundles.php`):

```php
Sentry\SentryBundle\SentryBundle::class => ['all' => true],
```

Variables in `.env`:

```
SENTRY_DSN=<YOUR_DSN>
SENTRY_TRACES_SAMPLE_RATE=0.2
```

The bundle captures unhandled exceptions on its own. To test — `\Sentry\captureMessage('check')` or a temporary route that throws; the event shows up under **Issues**.

> **`ignore_exceptions` matters:** without it, `NotFoundHttpException` flows into Gotcha as an error, and every scan or dead link clutters your issues. Ignoring client 404/405 leaves only real application errors.

## JavaScript / Node.js (server)

```bash
npm install @sentry/node
```

Repository: https://github.com/getsentry/sentry-javascript

```js
const Sentry = require("@sentry/node");
// or: import * as Sentry from "@sentry/node";

Sentry.init({
  dsn: "<YOUR_PROJECT_DSN>",
  environment: process.env.NODE_ENV || "production",
  tracesSampleRate: 0.2,
});
```

Test error:

```js
try {
  throw new Error("Gotcha test error");
} catch (e) {
  Sentry.captureException(e);
}
```

Before a short-lived process exits (a CLI script, a serverless function, a worker that returns immediately), make sure to flush the buffer:

```js
await Sentry.close(2000); // wait up to 2s for the event to be sent
```

## JavaScript (browser)

```bash
npm install @sentry/browser
```

```js
import * as Sentry from "@sentry/browser";

Sentry.init({
  dsn: "<YOUR_PROJECT_DSN>",
  environment: "production",
  tracesSampleRate: 0.2, // tracing + automatic Web Vitals collection (LCP/INP/CLS/FCP/TTFB)
});
```

Test error — any unhandled exception on the page is captured automatically; explicitly:

```js
Sentry.captureException(new Error("Gotcha test error"));
```

If your site sets a Content-Security-Policy with `connect-src`, add your Gotcha instance's address to it — otherwise the browser will silently block the request to your DSN.

The browser sends events to the address in the DSN — often a **different domain** than the site itself (site on `app.example.com`, Gotcha on `gotcha.example.com`). Gotcha's ingest replies with CORS headers and handles the preflight (`OPTIONS`), so the browser SDK sends **directly**, with no proxy or tunnel. The public key in the DSN is public by design — the receiver allows any origin.

## Python

```bash
pip install sentry-sdk
```

Repository: https://github.com/getsentry/sentry-python

```python
import sentry_sdk

sentry_sdk.init(
    dsn="<YOUR_PROJECT_DSN>",
    environment="production",
    traces_sample_rate=0.2,
)
```

Test error:

```python
try:
    raise RuntimeError("Gotcha test error")
except Exception:
    sentry_sdk.capture_exception()
```

For Django/Flask/FastAPI and other frameworks, `sentry-sdk` auto-enables the matching integration when it detects the framework is installed — no separate opt-in is needed beyond calling `sentry_sdk.init(...)` at your app's entry point.

## Go

```bash
go get github.com/getsentry/sentry-go
```

Repository: https://github.com/getsentry/sentry-go

```go
package main

import (
	"errors"
	"time"

	"github.com/getsentry/sentry-go"
)

func main() {
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              "<YOUR_PROJECT_DSN>",
		Environment:      "production",
		TracesSampleRate: 0.2,
	})
	if err != nil {
		panic(err)
	}
	// REQUIRED: without Flush the process can exit before the event
	// buffer has actually been sent over the network.
	defer sentry.Flush(2 * time.Second)

	sentry.CaptureException(errors.New("Gotcha test error"))
}
```

## Environment & release

`environment` and `release` are two fields worth setting from day one: they get attached to every event, transaction, and metric, and are used for filtering across nearly every section of Gotcha.

```php
\Sentry\init([
    'dsn' => '<YOUR_PROJECT_DSN>',
    'environment' => 'staging',
    'release' => 'my-app@' . trim(shell_exec('git rev-parse --short HEAD')),
]);
```

That way production errors don't drown among your staging environment's noise, and a performance regression can be traced back to the exact release it started in.

## Performance & tracing (transactions)

The `traces_sample_rate` option (`tracesSampleRate` in JS) turns on transactions — the unit of performance data that powers the [Performance](/docs/performance) section. Its value is the fraction of requests to trace: `1.0` traces everything, `0.2` traces one in five, and `0` (the default) disables tracing entirely, sending only errors.

On top of that, **"Project Settings → Performance"** has a server-side sampling knob (`sample_rate`, 0..1) that's applied on top of whatever the SDK already sent — useful for trimming stored transaction volume without redeploying your app. In the browser, enabling tracing also automatically collects [Web Vitals](/docs/glossary) (LCP/INP/CLS/FCP/TTFB) — no separate opt-in needed.

## Metrics (OTLP)

Numeric metrics (counter/gauge/histogram) are accepted by Gotcha over the OpenTelemetry protocol (OTLP/HTTP), not through the Sentry SDK. Point your application's OTLP exporter (or an OpenTelemetry Collector) at:

```
POST https://<your-gotcha-host>/v1/metrics
Authorization: Bearer <PUBLIC_KEY>
```

`<PUBLIC_KEY>` is the public key part of your project's DSN (the segment between `https://` and `@`), not the full DSN. Most OTel exporters have a built-in way to set headers (`headers:` in a Collector config, `OTEL_EXPORTER_OTLP_METRICS_HEADERS` as an env var). More on metric types, aggregations, and threshold alerts: [Metrics](/docs/metrics).

## Profiling (pprof)

Besides the profiles a Sentry SDK sends alongside traces (`profiles_sample_rate` in the Python/PHP/JS/Go SDKs), Gotcha also accepts raw pprof profiles directly:

```
POST https://<your-gotcha-host>/profiles/pprof?service=<service-name>&environment=<environment>
Authorization: Bearer <PUBLIC_KEY>
Content-Type: application/octet-stream
```

The body is a regular gzip-compressed pprof profile (e.g. the output of `go tool pprof` or `runtime/pprof`). More on flame graphs, in-app vs. system frames, and regressions: [Profiling](/docs/profiling).

## Not seeing an event?

If an error you sent hasn't shown up under "Issues" after a reasonable wait, check these in order:

| Cause | How to check |
|---|---|
| **Wrong or revoked DSN** | Compare it against what's shown under "Project Settings → DSN keys"; ingest returns `401`/`403` for an unknown or revoked public key. Reissuing a key immediately invalidates the old DSN. |
| **DSN from a different project** | The `project_id` in the DSN must match the project the key actually belongs to — otherwise ingest returns `403 sentry_key does not match project`. |
| **Network/firewall** | Your application needs HTTPS/HTTP reachability to your Gotcha instance's address (`GOTCHA_BASE_URL`) — try `curl -i <your-gotcha-host>/healthz` from the same machine/container the app runs on. A corporate proxy or egress firewall can silently drop outbound requests. |
| **Event/body too large** | By default ingest caps request bodies at 1 MB (`GOTCHA_MAX_EVENT_BYTES` on the instance); exceeding it returns `413`. This can bite events with very long stack traces or large breadcrumb trails. |
| **Organization quota exhausted** | Ingest returns `429` with `Retry-After` once the monthly quota is used up; check "Organization settings → Usage & rate limits". The `oss` edition defaults to unlimited (`0`), but the instance admin may have set a cap. |
| **SDK didn't flush in time** | In short-lived processes (CLI scripts, serverless functions, workers that exit immediately), call `Flush`/`close` before exiting — see the Go and Node examples above; without it, the event buffer may never actually be sent. |
| **CSP blocking the request in the browser** | If the page sets a `Content-Security-Policy`, add your Gotcha host to `connect-src`. |

If none of these explain it, turn on the SDK's debug mode (`debug: true` on most Sentry SDKs) and see what it logs when it tries to send — that almost always pinpoints the exact cause.
