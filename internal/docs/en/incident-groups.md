# Incident groups

When a node other things depend on goes down — a gateway, a hypervisor, a
database — signals fire from every direction: hosts behind the gateway go
silent, metric thresholds trip, an SLO monitor turns red. Incident groups
fold that fan-out into a single card: a **root availability incident**
(a silent host or a down uptime monitor) plus its **composition** — incidents
of nodes chained to the root through declared dependencies (dependency
edges are set on the "Alerts → Storm suppression" page).

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
  line. The feed marks such a member with a "silent — root notifies"
  badge.
- Signals that fired before the root went down join retroactively —
  notifications they already sent are not recalled.
- When the root closes, the group closes; members still open (disk still
  full) notify and escalate again — the ladder restarts from the moment the
  group closed. A former member like this shows up in "Out of groups"
  immediately: the incident's link to the closed group (`group_id`) isn't
  cleared, but the feed filters on whether the group has resolved, not on
  an empty `group_id` — a problem that outlived its neighbor doesn't drop
  out of sight.

## Incident feed

The "Alerts → Incident feed" section shows open groups with their
composition (expand in place), open out-of-group incidents across all six
sources, and what closed in the last day.

A group card's header shows the root's type and name (linked to its own
page; a node that has since been deleted is labeled "deleted node"), the
root's severity, and a translated composition line with a total count. An
open group shows how long ago it started; a closed one shows when it
closed and carries a "resolved" badge instead. A root shows in the header
of its own group card and is not repeated under "Out of groups". A member
that closed before the group itself did (say, disk space freed up while
the root is still down) is marked resolved in the composition, with its
own close time.

An open or recently closed incident outside any group, that used to
belong to one whose group has since closed (or whose card has aged out
under retention), carries a "previously grouped — <root>" badge — the
link to its former group isn't lost even once the group itself is gone
from the feed (the badge links to that group's card when it's still
listed among the recently resolved).

Two different badges in the feed explain why an incident may not have
sent a notification, and they're not interchangeable — both can appear on
the same card at once, each for its own reason:

- "silent — root notifies" — an incident that's a member of an open
  group, notified on its behalf by the root: it holds back its own open
  notifications as long as the group and its own incident stay open.
- "suppressed — parent down" — suppression driven by the dependency graph
  (edges set on the "Alerts → Storm suppression" page), a mechanism
  independent of grouping: the incident's escalation is suppressed
  because its node depends on one that's already down. The absence of
  this badge doesn't guarantee a notification actually went out — it may
  have been held back for the other reason instead.

## Why transactions and profiling stay out of groups

Transaction and profile regressions have no node (their keys are a
transaction and a function), so a causal link to a specific host going down
can't be asserted. Their incidents appear in the feed's "Out of groups"
section, and their notifications are unchanged.
