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

Server-side validation: email must be a syntactically valid address, webhook must be a valid `http`/`https` URL with a host, Telegram requires a `chat_id` that parses as an integer (negative for groups and supergroups) and a non-empty secret. Invalid input returns `422` with an error message and the channel is not created.

Webhooks pointing at a private/local address (e.g. `http://localhost:...`) are blocked by default (SSRF protection), unless the operator has explicitly allowed private addresses instance-wide (for single-tenant installs).

Editing a channel — the "Edit" button on its row. The recipient, the secret, and whether the channel is enabled can all change: a disabled channel can be switched back on, and a typo in the address fixed, without losing the delivery history. Leave the secret field empty to keep the current one — it is entered blind and never rendered back into the form. The channel type does not change: address and secret mean different things for email, webhook, and Telegram, so "switch the type" is a separate channel.

Deleting a channel — the "Delete" button on its row in the channel table; it asks for confirmation and then takes effect immediately.

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

The symptom: Telegram delivery consistently fails — the delivery log shows a timeout or a network error — while webhook and email work. The cause is almost always on the way to `api.telegram.org`, not in the bot or the token.

**Step 1 — how the instance resolves the name.**

```bash
docker compose exec gotcha getent hosts api.telegram.org
```

If the name resolves to an **IPv6** address while the server has no global IPv6 (common on a VPS), the connection will time out even though Telegram's IPv4 is perfectly reachable.

**Step 2 — whether traffic reaches the address at all.** From the server:

```bash
curl -sS -m 5 -o /dev/null -w '%{http_code}\n' https://api.telegram.org/
```

Silence until the timeout expires means traffic isn't reaching the Bot API: filtering by a carrier along the path, a closed egress, a missing route. You can tell it apart by the pattern — some addresses behind that name answer and others don't, and which ones depends on the location and the direction. Telegram is healthy; it's unreachable *from here*.

**What to do about it** — three remedies, from the most durable to the most temporary.

1. **Your own Bot API address.** `GOTCHA_TELEGRAM_API_BASE` points delivery at your own [`telegram-bot-api`](https://github.com/tdlib/telegram-bot-api) server or at a plain reverse proxy on a network where Telegram is reachable. The only option that doesn't depend on which Telegram addresses happen to pass today.
2. **An outbound proxy.** The standard `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY` variables in the container's environment apply to Telegram delivery. They don't apply to webhook channels or uptime checks: those dial their targets directly on purpose, otherwise the SSRF filter would stop deciding on the address actually connected to.
3. **Pinning the name to a working IP** — a quick measure while you investigate. In `docker-compose.override.yml`:

   ```yaml
   services:
     gotcha:
       extra_hosts:
         - "api.telegram.org:149.154.167.220"
   ```

   Delivery recovers after `docker compose up -d gotcha`. Treat it as a stopgap: the address holds only until Telegram's addresses or the filtering rules change, and when it stops working the symptom returns with nothing changed on your side. Drop the pin once option 1 or 2 works.

### A TLS timeout on a connection that exists

When the connection is clearly established and the first large exchange is what fails — the email error shows the `220` greeting arrived and the failure came at STARTTLS — then small packets are passing while large ones vanish. Traffic filtering does that, and so does the MTU of the container network. Telling them apart is easy: **test from inside the container**, not from the host.

```bash
docker compose exec gotcha wget -q -O /dev/null https://api.github.com/ && echo ok || echo fail
```

If it hangs there too, go to `GOTCHA_NET_MTU` in [Configuration](/docs/configuration), which explains why the host works while the container doesn't, why some destinations are fine and others aren't, and why the failure can disappear for ten minutes and come back.

## Privacy: what external channels see

Event details — issue title, culprit, level, notification body, metric values — reach **trusted recipients only**. Everyone else gets an anonymized notification: routing fields (project, issue, and rule ids, counters, alert kind) plus a link back to the card in Gotcha.

The decision is made **per recipient, not per channel type**:

| Recipient | Trusted when |
|---|---|
| Email | the address domain is the instance host (or a subdomain of it), or is listed in `GOTCHA_TRUSTED_RECIPIENTS` |
| Webhook | the URL host is the same, or any internal address (`localhost`, private ranges, the `.local`, `.internal`, `.lan`, `.home.arpa` zones) |
| Telegram | never by address: the recipient is a `chat_id` and has no domain |
| Any channel | the **"This recipient is inside my perimeter"** box on the channel |

The box is for the cases where the address proves nothing: a Telegram chat that belongs to you cannot be recognized as yours from a `chat_id`. It is ticked by hand, one channel at a time, off by default, and such channels carry a "With details" badge in the table. See [Privacy](/docs/privacy) for the reasoning.

A mailbox on a public mail service (`@gmail.com`, `@yandex.ru`) is someone else's infrastructure exactly as Telegram is — details do not go there. A webhook pointed at your own server on an internal network, conversely, always receives them.

If your organization's mail lives on a domain other than the instance host, list it:

```
GOTCHA_TRUSTED_RECIPIENTS=corp.example
```

```
GOTCHA_EXTERNAL_CHANNEL_DETAILS=true
```

lifts the restriction entirely — details then go to every recipient, Telegram included. Enable it only if you have a lawful basis for cross-border transfer. Both are instance-level settings; see [Privacy and 152-FZ](/docs/privacy) for the reasoning.

## See also

- [Metric Alerts](/docs/metric-alerts) — threshold rules on numeric metrics, using the same channel set.
- [Issues](/docs/issues) — what an issue is, what a regression is, statuses.
- [Configuration](/docs/configuration) — the SMTP variables and `GOTCHA_EXTERNAL_CHANNEL_DETAILS`.
