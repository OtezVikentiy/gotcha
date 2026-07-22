# Privacy and personal data (152-FZ)

Gotcha is a self-hosted platform: you run it on your own infrastructure and control all of the data. Under Russia's Federal Law No. 152-FZ "On Personal Data" (and comparable regimes), that makes **you the personal-data operator (controller)**, not the Gotcha developers. This page explains what personal data the system processes, what is already enabled to minimize it, and which obligations stay with you.

> This is technical guidance, not legal advice. Consult a qualified lawyer for a full assessment of your obligations.

## What personal data is processed

| Category | Where it lives | Examples |
|---|---|---|
| Gotcha account holders | PostgreSQL: `users`, `user_identities` | email at registration, email and subject from an SSO/OAuth provider |
| End users of observed applications | ClickHouse: `events`, `transactions`, `metric_points` | `user_id`, `user_ip`, `user_email`, `user.*` attributes in tags/attributes |
| Free text with possible personal data | ClickHouse: `events.message`/`exception_value`/`stacktrace`/`contexts`, `spans.description`/`data`, `profile_samples.stack`, `tags` | anything an SDK or developer placed into an error message, transaction name, SQL/URL |
| Notification delivery addresses | PostgreSQL: `org_invites.email`, channel configuration | invitee emails, email/Telegram/webhook recipient addresses |

Sessions (`sessions`) store only a token hash and `user_id` — no IP, no user agent — so minimization is enforced at the schema level.

## What is enabled by default

- **IP and email scrubbing on ingest.** `GOTCHA_SCRUB_IP=true` and `GOTCHA_SCRUB_EMAIL=true` by default: `events.user_ip` and `events.user_email` are nulled before they reach ClickHouse. Denylisted keys (`GOTCHA_SCRUB_KEYS`) are stripped from tags/contexts.
- **Retention.** TTL is enforced and configurable: events and more via `GOTCHA_RETENTION_DAYS`, with spans, metrics, and profiles having their own settings (see [Configuration](/docs/configuration)). Data is deleted from ClickHouse automatically once its retention expires.
- **Anonymized external notifications.** `GOTCHA_EXTERNAL_CHANNEL_DETAILS=false` by default (privacy-by-default): Telegram/webhook receive only an anonymized link back to the instance, not the error text. See the external-recipients section below.
- **No phone-home.** Gotcha sends no analytics or telemetry back to its developers. The only external recipients are the ones you configure (alert channels, SSO providers).
- **SSRF protection.** Outbound requests (webhook alerts, uptime checks) do not target private/loopback addresses by default (`GOTCHA_SSRF_ALLOW_PRIVATE=false`).

## Data subject rights: access and deletion

152-FZ (art. 14) and comparable rules give the subject a right to access and delete their personal data. In Gotcha this is available at the organization level (to the **owner** role):

- **Export subject data** — exports an end user's data (by `user_id` or email), including events, transactions (including by identifiers in tags), and metrics.
- **Delete subject data** — removes the same data from ClickHouse.

Free text (`spans.data`/`description`, `profile_samples.stack`) is not deleted per-subject programmatically — a subject cannot be reliably identified inside arbitrary JSON/stack frames. Those fields are cleared by TTL expiry (spans 30 days, transactions 90, metrics 30, profiles 7 by default). If such data is sensitive for you, enable free-text scrubbing (below) and configure SDK-side scrubbing.

**Account self-deletion.** A Gotcha account holder can delete their own account from the profile page — linked sign-in methods, organization memberships, and sessions are removed by cascade. If they are the sole owner of an organization, they must transfer ownership or delete the organization first.

## Free-text scrubbing

By default `GOTCHA_SCRUB_FREETEXT=false`: error text, stack traces, and span descriptions are stored verbatim (naive masking would break SQL/URLs and reduce usefulness). If your developers might put personal data directly into error text, enable `GOTCHA_SCRUB_FREETEXT=true` (masks email in free text) and additionally configure scrubbing on the SDK side.

## External recipients and cross-border transfer

When you connect an alert channel or SSO, personal data can leave your perimeter:

| Recipient | What is sent | Jurisdiction |
|---|---|---|
| Telegram | alert text/link (recipient `chat_id`) | servers outside Russia |
| Email (SMTP) | alert text/link, recipient email | your SMTP server |
| Webhook | alert payload | the address you configured |
| OAuth/SSO (Yandex ID, VK ID, generic OIDC) | email/subject at sign-in | the provider (Yandex/VK — Russia) |

Sending error text (with possible personal data) to Telegram is a potential **cross-border transfer** (152-FZ art. 12). That is why details are off by default (`GOTCHA_EXTERNAL_CHANNEL_DETAILS=false`). If you enable details, make sure you have a lawful basis for cross-border transfer. `TelegramSender` lets you override the API base URL, so you can proxy through infrastructure in the required jurisdiction.

## Your obligations as an operator (152-FZ)

The following is a pointer, not an exhaustive list; check the current text of the law and a lawyer:

- **Database localization (art. 18.5).** Personal data of Russian citizens must be stored in databases located in Russia. Gotcha is not tied to any foreign hosting — deploy PostgreSQL and ClickHouse in the required jurisdiction.
- **Notify Roskomnadzor** of the intent to process personal data (except cases under art. 22).
- **Publish a personal-data processing policy (art. 18.1 §2(2)).** Use the inventory above as a starting point.
- **Legal basis and consent** of subjects where required.
- **Protection measures** for personal data: instance access, channel encryption (TLS), backups, role separation.

The technical means for compliance (scrubbing, retention, export/deletion, anonymized external notifications) are built in — but configuring them and the legal side remain the operator's responsibility.
