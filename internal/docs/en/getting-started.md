# Getting Started

Gotcha is an observability platform: errors, performance, metrics, uptime, and alerts in one place. It speaks the Sentry ingestion protocol, so connecting your app comes down to installing the regular Sentry SDK for your language and pointing it at your Gotcha project's DSN. This page is the end-to-end path from registration to your first event and your first alert; each step is short and links to a deeper page.

If you haven't stood up Gotcha itself yet (it's a separate self-hosted service — a Docker image plus PostgreSQL and ClickHouse), start with the installation page: [Installation & Operations](/docs/installation). Everything below assumes you already have a running instance open in your browser.

## 1. Register

Open `/register` on your instance and create a user (email + password). On a fresh instance, the **first** registered user always succeeds — regardless of the registration mode — and is automatically granted instance-admin rights. Every user after that follows whatever registration policy the administrator configured (open self-registration, invite-only, or fully closed).

## 2. Create an organization and your first project

Right after your first login you're redirected to `/onboarding` — a single form that creates both the organization and the first project at once:

- **Organization** — a slug (a short URL identifier, e.g. `my-team`) and a display name. An organization groups projects and members together; this is what you'll invite your team into.
- **Project** — a slug, a name, and a platform (Go / PHP / JavaScript / Python / other — this is just a hint for the snippets on the next step, it doesn't affect ingestion). A project represents a single application or service: events, transactions, and metrics are always tied to a specific project, not to the organization as a whole.

For definitions of these terms, see [Glossary](/docs/glossary).

## 3. Find your project's DSN

After the project is created you're automatically redirected to that project's **"Setup"** page — a URL like `/projects/<id>/setup`. It shows the DSN right away, plus ready-made Go/PHP/JavaScript snippets. You can get back to this page later two ways:

- from the **"Projects"** list — every project row has a **"Setup"** button;
- from **"Project settings"** (the "Project settings" link in the project menu) — its **"DSN keys"** section shows the DSN of the current live key, plus a table of all keys with options to issue a new one or revoke an old one.

The DSN looks like a regular URL: `https://<public_key>@<your-gotcha-host>/<project_id>` — an open-key DSN with no secret component, exactly the format official Sentry SDKs expect.

## 4. Connect an SDK

Take the DSN from step 3, install the official Sentry SDK for your language, and pass the DSN at init time — nothing else Gotcha-specific is needed in your code. The full list of languages, install commands, minimal init code, sending a test error, setting `environment`/`release`, enabling performance tracing, and pointers to OTLP metrics and pprof profiling ingest all live on the [SDK & Integrations](/docs/sdk) page. If you're on PHP (including Laravel), there are dedicated snippets for your case.

## 5. Get your first event

Trigger an unhandled error in your application (or send a test event explicitly, using the method described in [SDK & Integrations](/docs/sdk) for your language). Within a few seconds it will show up under the project's **"Issues"**. Identical errors (matched by type, message, and code location) are grouped into a single "issue" — the counter goes up instead of creating duplicates. More on the list, filters, and grouping: [Issues](/docs/issues).

If nothing shows up, don't panic — the end of the [SDK & Integrations](/docs/sdk) page has a "Not seeing an event?" section covering the most common causes (wrong DSN, network/firewall, quota, or the SDK not flushing before the process exits).

## 6. Set up alerts

To avoid checking "Issues" manually, open the project's **"Alerts"** section:

1. In **"Delivery channels"**, click **"Add channel"** and pick a type (email, webhook, or Telegram) plus a target.
2. In **"Rules"**, enable the conditions you want — a new issue, a regression (an issue reopening), or a spike in errors — and attach a channel to them.

From then on your team hears about new issues and spikes without watching a dashboard. More on throttling repeated notifications and the delivery history: [Alerts](/docs/alerts).

## What's next

- [Glossary](/docs/glossary) — a short dictionary of terms (project, DSN, event, issue, transaction, and more).
- [SDK & Integrations](/docs/sdk) — the flagship connection guide: PHP, Laravel, JavaScript (Node and browser), Python, Go, plus OTLP metrics and pprof profiling.
- Section guides: [Issues](/docs/issues), [Performance](/docs/performance), [Metrics](/docs/metrics), [Uptime](/docs/uptime), [Alerts](/docs/alerts), [Profiling](/docs/profiling).
- [Installation & Operations](/docs/installation) — if you're the one responsible for the Gotcha instance itself (Docker, environment variables, backups, upgrades).
