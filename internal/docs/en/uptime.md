# Uptime

The "Uptime" section watches the availability of external addresses and services through periodic checks — **monitors**. Open it from the activity icon in the left rail, or at `/projects/{id}/monitors`.

## Creating a monitor

The monitor list lives at `/projects/{id}/monitors`. The "New monitor" button (visible only to an org owner/admin) opens the form at `/projects/{id}/monitors/new`.

The form starts with a **check type** picker — HTTP, TCP, DNS, or Heartbeat. Each type has its own fields:

| Type | What it checks | Fields |
|---|---|---|
| **HTTP** | An HTTP(S) request to a URL; success is the response code and, optionally, body content | Method (GET/POST/HEAD), URL, Headers, Body, Expected status (comma-separated codes; empty = any 200–299), Body contains / Body not contains, Follow redirects, SSL alert days |
| **TCP** | Whether a TCP connection to host:port succeeds | Host, Port (1–65535) |
| **DNS** | Whether a name resolves and, optionally, matches an expected value | Hostname, Record type (A/AAAA/CNAME/MX/TXT), Expected value (optional) |
| **Heartbeat** | The reverse of the others: instead of Gotcha reaching out, your own application "checks in" periodically by pinging a dedicated URL. If it hasn't checked in within the *grace* period, the monitor is considered down | Grace seconds (60 or more) |

The type is fixed once a monitor is created — editing lets you change its settings, not its type.

For a Heartbeat monitor, the monitor's detail page shows a personal URL of the form `{base_url}/uptime/hb/{token}` plus a ready-made cron line that pings it with `curl` at the configured interval:

```
*/5 * * * * curl -fsS https://gotcha.example.com/uptime/hb/6e1f...af92 >/dev/null
```

Add that to your application's cron/systemd timer — every successful call resets the grace timer.

## Interval, timeout, thresholds

- **Interval** — how often the check runs, in seconds (minimum 30).
- **Timeout** — how long to wait for a response, in seconds (1–120, and must be less than the interval).
- **Fail threshold** — how many consecutive failed checks flip the monitor to down and open an incident.
- **Recovery threshold** — how many consecutive successful checks flip it back to up and close the incident.
- **Remind every (minutes)** — how often to resend a notification for a still-open incident (0 = never remind).

Thresholds are tracked independently per region — the monitor's overall status is decided by the consensus rule below.

## Regions and consensus

A monitor can be checked from more than one **region** — the built-in local region (running alongside the server) and any remote [probes](/docs/probes) the organization has registered. The list of available regions and their checkboxes live in the same monitor form.

When a monitor has more than one region, its overall status is computed with a **consensus** rule:

| Consensus | Rule | When to use it |
|---|---|---|
| **any** | Down if at least one region is down | Strict: any single point of unavailability is already a problem |
| **majority** | Down if more than half of the decided regions are down | A compromise: tolerates a single regional blip |
| **all** | Down only if every region is down | Lenient: alert only on a total outage |

**Important note about majority with an even number of regions**: if exactly half the regions are down (e.g. 2 of 4), that also counts as **down**, not up — a deliberate fail-safe so the monitor is never shown green when half the fleet is reporting an outage.

A region only counts toward consensus once it has crossed its own fail or recovery threshold — before that it's excluded from the tally.

## Incidents

When a monitor's region-aggregated status flips to down, an **incident** opens: it records the start time, a cause (the last error text), and the list of regions that are down. It stays open while any region remains down, and closes with a recorded duration once consensus returns to up.

An incident that opens during an active [maintenance window](/docs/maintenance) is marked "in maintenance" and does not send a notification — so planned work doesn't create false noise. Ordinary incidents notify through the channels attached to the monitor (see [Alerts](/docs/alerts)).

A monitor's incident list appears both at `/projects/{id}/incidents` and as a timeline at the bottom of the monitor's detail page.

## Monitor detail page

Clicking a monitor's name opens `/monitors/{id}`: current status, uptime% over 24h/7d/30d, a latency chart, a table of recent checks per region, an incident timeline, and — for HTTPS monitors — the SSL certificate's expiry. Owners/admins also get Pause/Resume, Edit, and Delete actions here.

## What's next

- [Probes](/docs/probes) — running a check from another region.
- [Public status pages](/docs/status-pages) — a public view of service state.
- [Maintenance windows](/docs/maintenance) — suppressing noise during planned work.
- [Alerts](/docs/alerts) — where and how incident notifications are delivered.
