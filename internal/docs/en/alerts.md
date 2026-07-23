# Alerts

The "Alerts" section links **rules** to **delivery channels**, so your team learns about new issues, regressions, and spikes without having to watch dashboards constantly. Open it via the bell icon in the left icon rail, or directly at `/projects/{id}/alerts`.

This page covers alerting on **issues**. Threshold alerts on numeric metrics are configured separately — see [Metric Alerts](/docs/metric-alerts); their notifications go out through the same channels described here.

## Delivery channels

A channel is a specific address/recipient a notification is sent to. A single channel is reused across every rule in the project (including metric alert rules).

| Type | Recipient (the "Recipient" field) | Secret (the "Secret" field) |
|---|---|---|
| **Email** | Email address | Not needed |
| **Webhook** | URL (`http://` or `https://`) | Optional — if set, the request body is signed with HMAC-SHA256 in the `X-Gotcha-Signature: sha256=<hex>` header |
| **Telegram** | Recipient/group `chat_id` | Required — the bot token (`123456789:AA...`) |

### Adding a channel

1. On the Alerts page, click **"+"** ("Add channel") — a modal opens.
2. Pick the **Type**: Email, Webhook, or Telegram.
   - Email is disabled in the dropdown ("Email (SMTP not configured)") until the instance operator configures SMTP — see [Configuration](/docs/configuration). This is an instance-wide switch, not a per-project setting.
3. Fill in the **Recipient**:
   - Email — just the address, e.g. `team@example.com`;
   - Webhook — the full endpoint URL that will accept a `POST` with a JSON body, e.g. `https://example.com/hooks/gotcha`;
   - Telegram — the `chat_id` (a number, usually negative for groups) — you can find it, for example, via `@userinfobot` after adding your bot to the target chat.
4. Fill in the **Secret**, if applicable:
   - Webhook — an arbitrary string you'll use to verify the `X-Gotcha-Signature` on your side;
   - Telegram — the bot token issued by `@BotFather` (`123456789:AA...`).
5. Leave "Enabled" checked (default) and click **"Add channel"**.

Server-side validation: email must be a syntactically valid address, webhook must be a valid `http`/`https` URL with a host, Telegram requires a non-empty recipient and secret. Invalid input returns `422` with an error message and the channel is not created.

Webhooks pointing at a private/local address (e.g. `http://localhost:...`) are blocked by default (SSRF protection), unless the operator has explicitly allowed private addresses instance-wide (for single-tenant installs).

Deleting a channel — the "Delete" button on its row in the channel table; it takes effect immediately.

## Issue rules

Three rule kinds, always present on the form (just "disabled" until configured):

| Rule | Fires when | Extra fields |
|---|---|---|
| **New issue** | A new issue (new fingerprint) appears | — |
| **Regression** | A resolved issue reopens (the same underlying error occurs again) | — |
| **Spike** | One issue's event count within a window reaches a threshold | Event threshold, window (minutes) |

For every rule: an "Enabled" checkbox, and "Throttle (minutes)" — the minimum interval between repeat notifications for the same issue and rule (guards against flooding with duplicates; `0` means no throttling). Spike additionally has "Event threshold" (e.g. `10`) and "Window (minutes)" (e.g. `5`): the rule fires once an issue accumulates N events within the last M minutes.

All three rules are saved with a single form — the **"Save rules"** button at the bottom of the "Rules" section submits all three cards' state at once.

A notification is queued to every enabled channel in the project whenever a rule of the matching kind fires; repeat firings for the same issue and rule are throttled per the configured interval.

## How channels attach to rules

There's no separate "attach channel to rule" step: an enabled rule automatically notifies **every enabled channel in the project**. If you need different rules to go to different channels, the only lever available today is toggling channels on/off. The same applies to metric alerts ([Metric Alerts](/docs/metric-alerts)) — they use the same project channel list.

If an enabled channel is email but SMTP isn't configured on the instance, delivery through that channel is skipped (with a warning in the server log); it doesn't block the other channels.

## Delivery log

A separate page at `/projects/{id}/alerts/deliveries` (labeled "Delivery log" in the sub-navigation) shows notifications that **failed to deliver**: channel type, recipient, attempt count, and the last error text (e.g. an SMTP rejection or a non-2xx HTTP status from a webhook), plus the timestamp. Useful when a webhook returns a non-2xx status, a Telegram bot token has expired, or your mail server is having intermittent issues — you can see the reason here without digging through server logs.

While there are no failed deliveries, the page shows an empty state: "No failed deliveries".

### Telegram: events don't reach the bot

If Telegram delivery consistently fails (the delivery log shows a timeout/network error) while webhook/email work, check **how the instance resolves `api.telegram.org`**:

```bash
docker compose exec gotcha getent hosts api.telegram.org
```

If the name resolves to an **IPv6** address while the server has no global IPv6 (common on a VPS), the connection to the Bot API will time out — even though Telegram's IPv4 is reachable. It looks like "blocking", but the fix is to pin `api.telegram.org` to a working IPv4 in the gotcha container — add to `docker-compose.override.yml`:

```yaml
services:
  gotcha:
    extra_hosts:
      - "api.telegram.org:149.154.167.220"
```

After `docker compose up -d gotcha` delivery recovers. Telegram's IP is stable, but update the pin if it fails again.

## Privacy: what external channels see

Webhook and Telegram are external services outside your infrastructure; email is treated as internal (delivered through your own SMTP). The instance environment variable

```
GOTCHA_EXTERNAL_CHANNEL_DETAILS=true|false
```

controls what goes out to webhook/Telegram when an alert fires (both for issues and for metrics):

- **`true`** (default) — the full text: issue title, culprit, level, notification body, metric values, and so on;
- **`false`** — an anonymized payload: only routing fields (project/issue/rule id, counters, alert kind) plus a link back to the card in Gotcha — no error text, transaction/function names, or values that could be personal data.

Email is not affected by this switch — SMTP is treated as a trusted channel inside the organization. It's set at the instance level by the operator; see [Configuration](/docs/configuration).

## See also

- [Metric Alerts](/docs/metric-alerts) — threshold rules on numeric metrics, using the same channel set.
- [Issues](/docs/issues) — what an issue is, what a regression is, statuses.
- [Configuration](/docs/configuration) — the SMTP variables and `GOTCHA_EXTERNAL_CHANNEL_DETAILS`.
