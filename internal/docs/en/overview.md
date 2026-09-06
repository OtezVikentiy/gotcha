# Overview

The "Overview" screen is the way into a project: a single timeline of what is
broken right now and what was recently fixed, gathered from every incident
source at once — hosts, uptime monitors, metrics, SLOs, and transaction and
profile regressions. Instead of six separate lists, each aware only of its own
source, there is one screen answering "how is the project doing".

## Where it lives

The first item in the left rail — **"Overview"**, URL
`/projects/<id>/overview`. The site root and the project switcher in the
header lead here too: pick a project and you land on its Overview. The
address `/projects/<id>/incident-feed`, where the screen lived before version
0.29.0, answers with a permanent redirect (301) to the new one.

## Status line

Under the heading are three tiles; each one is clickable as a whole and leads
to the matching section.

| Tile | What the number is | Window | Leads to |
|---|---|---|---|
| Uptime for window | share of successful checks across the project, in percent | the switcher's window (24h / 7d) | monitor list |
| Hosts over threshold | how many hosts have at least one open incident | state "right now" | host list |
| New issues in 24h | how many issues appeared for the first time | always 24 hours | issue list |

Three clarifications, without which the numbers are easy to misread:

- **Uptime** is a single pool: successful and total checks of every monitor in
  the project are summed and divided, not averaged per monitor. A monitor
  checked more often weighs more in that figure. Paused monitors are not
  excluded, and [maintenance windows](/docs/maintenance) are not subtracted
  from the denominator — the precision here matches the uptime column in the
  monitor list, not the single-monitor page, where windows are accounted for.
  If there were no checks in the window at all, the tile shows "no data"; if
  the check storage did not answer, it shows "unavailable", and the rest of
  the page still renders as usual.
- **Hosts over threshold** counts *distinct hosts*, not incidents: three open
  incidents on one machine make it one. The window switcher does not affect
  this tile; it is always about what is open right now.
- **New issues in 24h** counts by the time an issue was first seen. An old
  issue that got noisy again does not land here — the [Issues](/docs/issues)
  section, sorted by last event, is for that. The tile's window is always 24
  hours and deliberately ignores the switcher: the number has to stay
  comparable day over day.

A zero on the second and third tiles means "quiet". A subsystem you have not
set up yet (no monitors, no host agent installed) yields zero or "no data",
not an error — the screen opens either way.

## Window: 24 hours or 7 days

The **"24h" / "7d"** switcher sits by the "Recently resolved" heading, and on
an empty screen right below the empty state, so a quiet day can be widened to
a week. These are plain links, no JS: "24h" is the screen's canonical
address, "7d" is the same address with `?range=7d`. The window defaults to 24
hours; an unknown parameter value is silently read as 24 hours. The choice is
stored nowhere and lives only in the address — a link to the weekly window can
be bookmarked.

The window affects three things: the "Recently resolved" section, the uptime
tile, and the deploy list. It does **not** affect the "New issues in 24h" and
"Hosts over threshold" tiles, or either section of open items — those are
about "now" by definition.

## Deploys

If the selected window contains [deploy markers](/docs/deployments), a table
of the latest ones (up to 20) appears under the status line — version,
environment, and when, newest first — with an "All deploys" link to the full
section. No deploys in the window means no section at all: not an empty
table, an absent block. The reason for the adjacency is simple: most of "what
happened" starts with "what we shipped".

## The timeline

Below are three sections. There are deliberately no filters and no pagination
on this screen: "Overview" answers "what is going on", while the dense
history of monitor downtime lives on the "Availability incidents" page.

**Open groups** — up to 50 [incident groups](/docs/incident-groups), newest
first. A group is a root failure (a host went silent, a monitor went down)
plus everything that failed because of it; the card expands in place and
shows its members. What the badges on members mean and how the grouping works
is on the incident groups page.

**Ungrouped** — up to 50 open incidents that belong to no open group, across
all six sources: hosts, uptime monitors, metrics, SLOs, transaction
regressions, profile regressions. The root of an open group is not repeated
here — it is already shown in its own card's header. The window does not
apply to this section: it is about what is open right now.

**Recently resolved** — what closed within the selected window: up to 50
groups and up to 50 standalone incidents. Those are two independent caps, and
the caption under the heading says so plainly — the section can show up to a
hundred cards and rows combined.

Rows are laid out the same way in every section: source, name, status, start
time, and resolution time.

## When the screen is empty

If all four queries come back empty at once — no open groups, no open
ungrouped incidents, and nothing resolved within the window — the sections
are replaced by a **"Getting started"** prompt leading to the project setup
page. The status line, deploys, and the window switcher stay where they are:
an empty timeline means "nothing had a chance to break", not "the screen is
broken".

## Access

The screen is available to any member of the organization the project belongs
to — there is no separate permission for it. A project in someone else's
organization answers 404, not 403: the status code must not reveal whether
such a project exists.
