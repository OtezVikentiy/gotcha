# Privacy and personal data (152-FZ)

Gotcha is a self-hosted platform: you run it on your own infrastructure and control all of the data. Under Russia's Federal Law No. 152-FZ "On Personal Data" (and comparable regimes), that makes **you the personal-data operator (controller)**, not the Gotcha developers. This page explains what personal data the system processes, what is already enabled to minimize it, and which obligations stay with you.

> This is technical guidance, not legal advice. Consult a qualified lawyer for a full assessment of your obligations.

## A dev secret key means no encryption at rest

`GOTCHA_SECRET_KEY` is what encrypts SSO client secrets and channel tokens in
PostgreSQL. While it is left at the built-in development default, encryption is
**not enabled at all** — those secrets sit in the database as plain text.
Encrypting them with a key that is published in the source would be theatre, so
the product does not pretend otherwise.

Two consequences worth knowing:

- Setting a real key later does **not** encrypt what is already stored. Existing
  rows stay readable as legacy plain text; only values written afterwards are
  encrypted. If an instance ever ran on the dev key with real credentials,
  re-enter those credentials after setting a proper key.
- On a non-local `GOTCHA_BASE_URL` the app refuses to start with the default key
  in the `web`, `all`, `ingest`, and `uptime` modes (everywhere except `probe`), so this normally only affects local instances — unless
  the refusal was explicitly overridden.

## What personal data is processed

| Category | Where it lives | Examples |
|---|---|---|
| Gotcha account holders | PostgreSQL: `users`, `user_identities` | email at registration, email and subject from an SSO/OAuth provider |
| End users of observed applications | ClickHouse: `events`, `transactions`, `metric_points` | `user_id`, `user_ip`, `user_email`, `user.*` attributes in tags/attributes |
| Free text with possible personal data | ClickHouse: `events.message`/`exception_value`/`stacktrace`/`contexts`, `spans.description`/`data`, `profile_samples.stack`, `tags` | anything an SDK or developer placed into an error message, transaction name, SQL/URL |
| Notification delivery addresses | PostgreSQL: `org_invites.email`, channel configuration | invitee emails, email/Telegram/webhook recipient addresses |

Sessions (`sessions`) store only a token hash and `user_id` — no IP, no user agent — so minimization is enforced at the schema level.

## What is enabled by default

- **IP and email scrubbing also matches FIELD NAMES.** With `GOTCHA_SCRUB_IP`/`GOTCHA_SCRUB_EMAIL` on, the value of any field whose name (case- and separator-insensitive) contains `email` — or one of `user_ip`, `ip_address`, `client_address`, `net_peer_ip`, `net_sock_peer_addr`, `network_peer_address`, `network_local_address`, `client_ip`, `x_forwarded_for`, `x_real_ip`, `remote_addr`, `cf_connecting_ip`, `Forwarded` — is redacted, in tags, OTLP attributes, `span.data`, headers and query strings. In practice a `customer_email` tag becomes `[scrubbed]` even though it is not in `GOTCHA_SCRUB_KEYS`. Use `GOTCHA_SCRUB_ALLOW_KEYS` to get a specific name back — it is checked BEFORE the email and IP rules, so it overrides them for an exactly matching name. The match is exact: `GOTCHA_SCRUB_ALLOW_KEYS=email` only unmasks a field whose normalized name is exactly `email`; it affects neither `customer_email` nor `events.user_email` (that one is nulled by the separate `GOTCHA_SCRUB_EMAIL` rule).
- **IP and email scrubbing on ingest.** `GOTCHA_SCRUB_IP=true` and `GOTCHA_SCRUB_EMAIL=true` by default: `events.user_ip` and `events.user_email` are nulled before they reach ClickHouse. Denylisted keys (`GOTCHA_SCRUB_KEYS`) are stripped from tags, contexts, headers, query strings and request bodies. Matching is **fail-closed**: a name is redacted when it *contains* a denylist word, so `x_api_key`, `clientSecret` and `mytoken` are all caught — and so are `author` (contains `auth`) and `tokenizer` (contains `token`). Under-redacting leaks personal data; over-redacting costs a debugging field and is reversible with `GOTCHA_SCRUB_ALLOW_KEYS`.
- **Retention.** TTL is enforced and configurable: events and more via `GOTCHA_RETENTION_DAYS`, with spans, metrics, and profiles having their own settings (see [Configuration](/docs/configuration)). Data is deleted from ClickHouse automatically once its retention expires (a retention of `0` disables deletion for that class).
- **Retention covers summary records too — each by the retention of its own telemetry.** Error and performance issues (title, culprit) are deleted once their last event is older than `GOTCHA_RETENTION_DAYS`, and performance regressions follow the same period. Profile regressions follow `GOTCHA_PROFILE_RETENTION_DAYS`, metric incidents `GOTCHA_METRIC_RETENTION_DAYS`, and resolved uptime incidents `GOTCHA_INCIDENT_RETENTION_DAYS`: a summary record must not outlive the data it describes, or its card opens onto nothing. An issue title is free text from your application, and it must not outlive the retention you declared. Open incidents and regressions are never deleted — they describe what is happening right now. A retention of zero disables deletion within its own class.
- **Anonymized external notifications.** By default, error text reaches trusted recipients only — those on your own domains or internal network; everyone else, Telegram included, gets an anonymized link back to the instance. See the external-recipients section below.
- **No phone-home.** Gotcha sends no analytics or telemetry back to its developers. The only external recipients are the ones you configure (alert channels, SSO providers).
- **SSRF protection.** Outbound requests (webhook alerts, uptime checks) do not target private/loopback addresses by default (`GOTCHA_SSRF_ALLOW_PRIVATE=false`).

## Data subject rights: access and deletion

152-FZ (art. 14) and comparable rules give the subject a right to access and delete their personal data. In Gotcha this is available at the organization level (to the **owner** role):

- **Export subject data** — exports an end user's data (by `user_id` or email), including events, transactions (including by identifiers in tags), and metrics.
- **Delete subject data** — removes the same data from ClickHouse.

**Deleting a project or an organization.** The PostgreSQL rows are removed by
cascade immediately, while the ClickHouse telemetry is queued for deletion in the
same transaction and carried out by a background worker: eight mutations over
months of data take minutes, and running them inside the HTTP request meant the
deletion was cut short by a timeout, leaving part of the data behind forever. The
page therefore says the cleanup is **queued**, not done. You can evidence that it
ran with the `gotcha_purge_queue_depth` and `gotcha_purge_queue_oldest_seconds`
metrics (see [Self-monitoring](/docs/self-monitoring)): a growing age of the
oldest request means the deletion has not happened, and the reason for the last
attempt is recorded in the request itself.

> **Important: with the default scrubbing on, lookups by email and IP match nothing.** `GOTCHA_SCRUB_IP` and `GOTCHA_SCRUB_EMAIL` are on by default, so `events.user_email` and `events.user_ip` are nulled at ingest — there is nothing left to search by. Use **`user_id`**: it is deliberately excluded from scrubbing for exactly this right. The export and deletion forms warn about this inline, and deletion reports a count ("deleted N records" or "no records matched") so you can evidence that the request was carried out.

Free text (`spans.data`/`description`, `profile_samples.stack`) is not deleted per-subject programmatically — a subject cannot be reliably identified inside arbitrary JSON/stack frames. Those fields are cleared by TTL expiry (spans 30 days, transactions 90, metrics 30, profiles 7 by default). If such data is sensitive for you, enable free-text scrubbing (below) and configure SDK-side scrubbing.

**Account self-deletion.** A Gotcha account holder can delete their own account from the profile page — linked sign-in methods, organization memberships, and sessions are removed by cascade. If they are the sole owner of an organization, they must transfer ownership or delete the organization first.

## Free-text scrubbing

By default `GOTCHA_SCRUB_FREETEXT=false`: error text, stack traces, and span descriptions are stored verbatim — except for secrets inside URLs: denylisted query parameters, the fragment and basic-auth credentials are stripped from URLs regardless of this flag (naive masking would break SQL/URLs and reduce usefulness). If your developers might put personal data directly into error text, enable `GOTCHA_SCRUB_FREETEXT=true` (masks email in free text) and additionally configure scrubbing on the SDK side.

## External recipients and cross-border transfer

When you connect an alert channel or SSO, personal data can leave your perimeter:

| Recipient | What is sent | Jurisdiction |
|---|---|---|
| Telegram | alert link and the recipient `chat_id`; the text only if the channel is marked as your own (or with `GOTCHA_EXTERNAL_CHANNEL_DETAILS=true`) | servers outside Russia |
| Email (SMTP) | the recipient address and the alert link; the text only for trusted recipients (below) | your SMTP server and the recipient's mail server |
| Webhook | the alert payload; details only for trusted hosts (below) | the address you configured |
| OAuth/SSO (Yandex ID, VK ID, generic OIDC) | email/subject at sign-in | the provider (Yandex/VK — Russia) |

Sending error text (with possible personal data) outside your perimeter is a potential **cross-border transfer** (152-FZ art. 12). By default, event details — title, culprit, level, message body — therefore reach **trusted recipients only**; everyone else gets an anonymized notification with a link back to the instance.

A recipient is trusted when its address belongs to your infrastructure:

| Recipient | Trusted when |
|---|---|
| Email | the address domain is the instance host (or a subdomain of it), or is listed in `GOTCHA_TRUSTED_RECIPIENTS` |
| Webhook | the URL host is the same, or any internal address: `localhost`, private ranges (`10.0.0.0/8`, `192.168.0.0/16`, …), the `.local`, `.internal`, `.lan`, `.home.arpa` zones |
| Telegram | never by address: the recipient is a `chat_id` with no domain, and nothing about it can confirm it belongs to your perimeter |
| Any channel | the **"This recipient is inside my perimeter"** box on the channel — the operator states what the address cannot show |

The decision is made per **recipient**, not per channel type. A mailbox on a public mail service is someone else's infrastructure exactly as Telegram is; a webhook pointed at your own server on an internal network never leaves your perimeter at all.

The box exists for the cases where the address proves nothing. Telegram is the usual one: the instance is self-hosted, the chat belongs to the operator, and no amount of parsing a `chat_id` will ever show that. Before it, the choice was between "nowhere" and `GOTCHA_EXTERNAL_CHANNEL_DETAILS=true`, meaning "everywhere, to everyone". The box is ticked one channel at a time, by hand, and is off by default; channels carrying it are marked in the table with a **"With details"** badge, so who receives personal data is visible at a glance instead of by opening every channel in turn.

The statement is the operator's responsibility: the product cannot verify it — which is precisely why the box is ticked by hand.

If your organization's domain differs from the instance host, list it explicitly:

```
GOTCHA_TRUSTED_RECIPIENTS=corp.example,ops.corp.example
```

Matching happens on label boundaries: `corp.example` covers `mail.corp.example` but not `evilcorp.example`. The instance's parent domain is **not** trusted automatically — walking one level up from `gotcha.github.io` would extend trust to all of `github.io`, meaning every unrelated project on that host.

`GOTCHA_EXTERNAL_CHANNEL_DETAILS=true` lifts the restriction entirely: details then go to every recipient of every project, Telegram included. When the goal is to open up one or two channels of your own, ticking the box on those channels is the better instrument — same result, and every other recipient stays protected.

The active policy is printed at startup: the line `alert details: sent only to trusted recipients` lists the instance host and the configured list.

## Your obligations as an operator (152-FZ)

The following is a pointer, not an exhaustive list; check the current text of the law and a lawyer:

- **Database localization (art. 18.5).** Personal data of Russian citizens must be stored in databases located in Russia. Gotcha is not tied to any foreign hosting — deploy PostgreSQL and ClickHouse in the required jurisdiction.
- **Notify Roskomnadzor** of the intent to process personal data (except cases under art. 22).
- **Publish a personal-data processing policy (art. 18.1 §2(2)).** Use the inventory above as a starting point.
- **Legal basis and consent** of subjects where required.
- **Protection measures** for personal data: instance access, channel encryption (TLS), backups, role separation.

The technical means for compliance (scrubbing, retention, export/deletion, anonymized external notifications) are built in — but configuring them and the legal side remain the operator's responsibility.
