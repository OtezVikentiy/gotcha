# Escalations

Escalation is a ladder of notification steps: the longer an incident stays
open and unacknowledged, the wider the set of channels a notification goes
to. The screen configures two independent ladders — "Critical" and
"Warning" — and applies to all six incident sources: hosts, metrics,
transaction regressions, profile regressions, SLOs, and uptime. It's reached
through "Alerts → Escalations" in the left rail — `/projects/{id}/escalations`.

## How a ladder is built

An incident's severity decides which of the two ladders fires. Inside a
ladder there are up to five steps, numbered sequentially starting at zero (a
gap in numbering — say, step 0 filled and step 1 empty — is rejected on
save). Each step has a delay in minutes from when the incident opened (`0` =
immediately) and a set of checked channels; later steps typically widen the
recipient list. A delay on step zero also postpones the very first
notification — that's deliberate: it avoids paging someone over a problem
that resolves itself within a couple of minutes. A step with no channel
checked counts as unused; unchecking every channel on a step is how you
remove it.

A channel that stopped being deliverable after it was picked for a step
(the operator disabled it, or a webhook/bot secret broke) doesn't silently
disappear from the form — it stays listed, marked with a badge explaining
why ("disabled" / "broken secret"), and survives the form being saved
as-is. Only a channel that's deliverable right now can be added to a new
step.

Both ladders save through separate forms, each with its own preview
underneath.

## When no ladder is configured

An empty ladder isn't an error — it means "not configured yet", and the
product's old, pre-escalation behavior kicks in instead: every enabled
channel of the project, immediately. The fallback applies per severity
independently — you can configure a ladder only for "Critical" and leave
"Warning" on the default.

## Dry-run preview

Under each ladder's form is a side-effect-free preview: what the ladder
actually in effect right now (the saved one, or the default fallback if
unconfigured) would send, step by step — "immediately" or "after N min",
with the channel list. The note underneath explains that a recovery
notification goes out to every channel that received at least one step of
that ladder over the incident's lifetime — not necessarily every channel of
the project. That holds for five of the six sources (hosts, metrics,
transaction regressions, profile regressions, SLOs). **Uptime** recovery
works differently: it goes out to every channel currently deliverable to
the monitor (the monitor's own channels if it has any, otherwise every
enabled channel of the project) — not only the ones that actually received
an escalation step for that incident.

## Stopping escalation: acknowledgment (ack)

An open incident's card (the incident feed, the hosts list, uptime, and so
on) carries an operator-only "Acknowledge" button. Acknowledging stops
further escalation — no more steps are sent — but doesn't close the
incident: it stays open until the problem resolves on its own (its source's
usual auto-close). Who acknowledged it and when is shown on the card.

Uptime monitors gained acknowledgment and escalation later than the other
five sources: before, a site going down couldn't be escalated further over
time, nor acknowledged to stop the paging.

## The uptime special case

Uptime escalates the same way as the other five sources, with one
difference: the very first "site is down" notification goes out right away
(or after the settling grace, if the monitor has a declared parent — see
[Storm suppression](/docs/alert-suppression)) and ignores step zero's delay
entirely. Only step 1 onward follows the configured ladder's schedule.

## Interaction with maintenance windows and dependencies

Whether to escalate is decided fresh every time a step is about to be sent,
not frozen at the moment the incident opened: if the project's maintenance
window started after the incident was already open, the next step still
won't go out while the window is active (see
[Maintenance windows](/docs/maintenance)). The same goes for an incident
whose node depends on an already-down parent (see
[Storm suppression](/docs/alert-suppression)) — it doesn't advance up the
ladder while suppressed.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_ESCALATION_INTERVAL_SECONDS` | `60` | How often (in seconds) the centralized scheduler sweeps every open, unacknowledged incident across all six sources and advances the ladder to its next step once that step's delay from opening has elapsed. Minimum 1 second. |

Like the metric/host/SLO evaluators, the escalation scheduler itself only
runs in `uptime`/`all` modes, or when explicitly enabled via
`GOTCHA_RUN_EVALUATORS` — see [Configuration](/docs/configuration). The log
of delivered steps (`incident_escalations`) is kept for the same period as
incidents themselves — `GOTCHA_INCIDENT_RETENTION_DAYS`.

## See also

- [Alerts](/docs/alerts) — the delivery channels every ladder step draws from.
- [Storm suppression](/docs/alert-suppression) — the settling grace and suppression of dependent nodes' alerts.
- [Incident groups](/docs/incident-groups) — how escalation and acknowledgment show up on the feed once incidents fold into a single card.
- [Hosts](/docs/hosts) — one of the six incident sources escalated by these ladders.
- [SLOs](/docs/slo) — another source: error-budget burn incidents.
