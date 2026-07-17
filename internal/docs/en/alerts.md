# Alerts

The "Alerts" section links rules to delivery channels, so your team learns about issues, regressions, and spikes without having to watch dashboards constantly.

## Rules
A rule describes what event to react to: a new issue, a performance regression, an error spike, or a metric threshold being crossed. Each rule defines its trigger condition and, when relevant, a threshold.

## Delivery channels
Delivery can be configured via email, webhook, or Telegram. A single channel can be reused across multiple rules and across different projects in the organization.

## Throttling and failed deliveries
To avoid flooding the team with duplicate notifications, repeated triggers of the same rule are throttled — a repeat notification is sent no more often than a configured interval. Failed deliveries (for example, an unreachable webhook) show up in the alert history along with the error reason.
