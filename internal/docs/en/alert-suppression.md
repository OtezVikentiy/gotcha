# Storm suppression

When a node other things depend on goes down — a gateway, a hypervisor, a
database — every dependent node either goes silent or starts firing its own
alerts, and the on-call person gets a storm of notifications instead of one
clear root cause. Storm suppression works on top of a graph of explicit
dependency edges that you declare yourself: while a parent has an open
availability incident, its child's first notification is held back instead
of going out independently. It's reached through "Alerts → Silence → Storm
suppression" — `/projects/{id}/alert-suppression`.

This is **not** the same thing as the [Dependencies](/docs/dependencies)
screen: that page automatically builds a map of a service's outbound calls
(databases, caches, HTTP) from traces. Here it's the opposite — you manually
declare that one infrastructure node (a host or a monitor) depends on
another, and the edge is used only to suppress alerts, not to visualize
topology.

## Dependency edges

An edge is a parent → child pair:

- **Parent** — a specific host or a specific monitor.
- **Child** — can likewise be a specific host or monitor, or expressed as a
  label match: "every host with environment X" or "every host with role Y"
  (labels — see [Host labels](/docs/hosts)). A single parent can cover a
  whole group of hosts with one label edge instead of listing them one by
  one.

Saving the form validates that: the parent and child belong to the same
project; the edge doesn't loop back on itself; a label edge doesn't match
the parent host's own label; the exact same edge doesn't already exist; and
the new edge doesn't close a cycle among the already-declared nodes of the
graph.

An edge can be **edited**, not only deleted and recreated: the "Edit" button
on its row opens the "Edit dependency" modal with the same form and the same
validation as on creation (only the fields for the selected parent and child
types are shown). Editing, like creating and deleting, is a project operator
action. It applies to **new** suppression decisions: the service re-reads the
graph within a few seconds. Incidents already suppressed stay suppressed
until they close — an edit doesn't replay anything retroactively.

## What actually gets suppressed

Only the child's **availability incidents** are suppressed — a host going
silent or a monitor going down. Metric alerts on the same host
(disk/memory/load) and SLO alerts on its uptime keep opening as usual and
join the same [incident group](/docs/incident-groups): if the root has
already sent its notification, theirs is held back for as long as the group
stays open — the on-call person sees one message about the root cause
instead of a storm; if the root hasn't notified yet at that point, the
child's first notification goes out as usual, and further
[escalation](/docs/escalations) waits for the group to close. Trace and
profile regressions are outside this mechanism entirely and notify as
usual — they have no node that could be set as a dependency.

**A limitation of the current release.** The dependency gate in the
[escalation scheduler](/docs/escalations) — hold the ladder while the parent
is down, release it once the parent recovers — is implemented for **host**
incidents only. A monitor incident is checked against its parent once, when
it opens (the uptime detector decides on its own: suppress for good, or send
late after the settling grace), and is not re-evaluated against the parent
afterwards. Incidents from metric rules, trace and profile regressions, and
SLOs don't consult dependency edges at all — for them only the
[incident groups](/docs/incident-groups) mechanism described above applies.

## Settling grace

A child's first notification doesn't fire instantly — it's held for
`GOTCHA_DEPENDENCY_SETTLE_SECONDS` (300 seconds / 5 minutes by default).
That gives the parent time to either go down too (in which case suppression
kicks in before the child's first notification would have gone out), or
stay up — in which case, once the grace elapses, the child's notification
goes out as usual, with no further delay. The grace value in effect is
shown right on the page above the edge list, so you don't have to go look
it up in the instance's configuration.

The grace should be at least as long as the slowest silence threshold
(`silent_after`) among parent hosts — otherwise, when a parent host goes
down, part of the storm still slips through before suppression kicks in.
The page warns about this mismatch directly and points at the silence
thresholds on the [Hosts](/docs/hosts) page.

The same grace — the same `GOTCHA_DEPENDENCY_SETTLE_SECONDS` value — is
also used by the [escalation](/docs/escalations) scheduler and the uptime
detector for a monitor with a declared parent: step zero of a dependent
node's escalation ladder is held for the same period.

## Preview

Below the edge list and the add form is a side-effect-free calculation: for
every parent that has at least one edge, what would be suppressed right
now if that parent went down this instant. Useful for checking the graph
before it's needed during a real incident.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `GOTCHA_DEPENDENCY_SETTLE_SECONDS` | `300` | The settling grace when a parent goes down — see above. Used as the same value by storm suppression, the escalation scheduler, and the uptime detector. |

## See also

- [Incident groups](/docs/incident-groups) — how suppressed and "quiet — root notifies" incidents show up on the shared feed.
- [Escalations](/docs/escalations) — the notification ladder that storm suppression pauses for dependent nodes.
- [Hosts](/docs/hosts) — environment/role labels for label edges, and the silence thresholds the grace needs to stay in step with.
- [Dependencies](/docs/dependencies) — the automatic map of a service's outbound calls; not to be confused with this page.
