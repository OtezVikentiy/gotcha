# Getting Started

Gotcha is an observability platform: errors, performance, metrics, uptime, and alerts in one place.

## 1. Create a project
A project represents a single application or service. Errors, transactions, and metrics are all tied to a project. An organization groups projects and members together.

## 2. Connect an SDK
The "Project Settings → Connect" page has your project's DSN and code snippets. Gotcha accepts data via the Sentry ingestion protocol — use the official Sentry SDK for your language and point it at your Gotcha DSN. More details: [SDK & Integrations](/docs/sdk).

## 3. Get your first event
After connecting, trigger an error in your application — it will show up under "Issues". Identical errors are grouped into a single "issue".

## 4. Set up alerts
In your project's "Alerts", add a delivery channel (email/webhook/Telegram) and enable rules — you'll be notified about new issues and spikes.

## What's next
- [Glossary](/docs/glossary) — a short dictionary of terms.
- Section guides: [Issues](/docs/issues), [Performance](/docs/performance), [Metrics](/docs/metrics), [Uptime](/docs/uptime), [Alerts](/docs/alerts), [Profiling](/docs/profiling).
