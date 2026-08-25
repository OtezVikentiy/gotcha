# Incident groups

When a node other things depend on goes down — a gateway, a hypervisor, a
database — signals fire from every direction: hosts behind the gateway go
silent, metric thresholds trip, an SLO monitor turns red. Incident groups
fold that fan-out into a single card: a **root availability incident**
(a silent host or a down uptime monitor) plus its **composition** — incidents
of nodes chained to the root through declared dependencies (see "Alert storm
suppression").

## How a group is built

- The root of a group is an open availability incident of a node: a silent
  host or a down monitor. Threshold incidents of a host (disk/memory/load)
  never become roots — degradation is not unavailability.
- A member is an incident whose node is chained to the root through downed
  nodes: host incidents (by host), uptime incidents (by monitor), metric
  thresholds labeled `host`, and uptime-type SLOs (by target monitor).
- While the root is open and its notification has gone out, members hold
  back their own open notifications and don't escalate — the root informs
  on their behalf. The root's notification gains a "Dependent nodes: N"
  line.
- Signals that fired before the root went down join retroactively —
  notifications they already sent are not recalled.
- When the root closes, the group closes; members still open (disk still
  full) notify and escalate again — the ladder restarts from the moment the
  group closed.

## Incident feed

The "Alerts → Incident feed" section shows open groups with their
composition (expand in place), open out-of-group incidents across all six
sources, and what closed in the last day. A root shows in the header of its
own group card and is not repeated under "Out of groups". Incidents
suppressed by
dependencies carry a "suppressed by dependency" badge — they show up in a
group's composition even though they never sent notifications.

## Why transactions and profiling stay out of groups

Transaction and profile regressions have no node (their keys are a
transaction and a function), so a causal link to a specific host going down
can't be asserted. Their incidents appear in the feed's "Out of groups"
section, and their notifications are unchanged.
