# Maintenance windows

A maintenance window is a pre-announced interval of planned work for a project. While it's active, incidents opened by that project's [monitors](/docs/uptime) are marked "in maintenance" and **do not send notifications** through [alerts](/docs/alerts) — so a planned reboot or deploy doesn't flood your channel with false alarms. The incident itself is still recorded (visible in the history), just without a delivery.

The page lives at `/projects/{id}/maintenance`, available to a member of a team attached to the project (an operator — see [Roles and access](/docs/teams)), as well as to the organization's owner and admins.

## Creating a window

1. On the "Maintenance" page, click "New window" — a create modal opens.
2. Enter a **Name** — a short description of the work (e.g. "Database upgrade").
3. Pick the type — **One-off** or **Weekly** (a radio pair, one-off selected by default):
   - **One-off** — set **Start** and **End** (date and time, `datetime-local` fields);
   - **Weekly** — a recurring weekly window: set the **Weekday** and a **Start**/**End** time in HH:MM, which repeats every week on that day.
4. Set the **Timezone** — pick one from the list (UTC, Europe/Moscow, Europe/Berlin, Asia/Yekaterinburg), or, if the one you need isn't listed, pick "Other" and type an IANA name into the adjacent field (e.g. `America/New_York`).
5. Save with "Create window".

Created windows are listed with their name, type, and schedule, each with an "Edit" and a "Delete" button.

## Editing a window

"Edit" opens the same form, already filled in with the window's values: name, type, schedule, and time zone. Everything is editable, the type included — a one-off window can become weekly and back, and the columns of the previous schedule are cleared. A one-off window is shown in its own time zone rather than in UTC, so shifting it by an hour is a one-field edit.

## Effect on monitors and alerts

A maintenance window applies to the **whole project**, not a single monitor: while the current time falls inside any active window, every incident that opens at that moment is flagged "in maintenance" and does not notify. Checks keep running as usual — a maintenance window doesn't pause monitoring, it only suppresses notification noise.

On the monitor detail page and in the incident list, such incidents are marked with a separate "Maintenance" column (yes/no), so you can always tell a real alert apart from expected downtime during planned work.

On a [public status page](/docs/status-pages), upcoming maintenance windows are shown to visitors as a separate list — a way to warn users about planned downtime ahead of time.

## FAQ

### Do checks stop while a maintenance window is active?

No. Monitoring keeps running as usual: checks execute, incidents open and land in the history. The window only affects notifications — incidents that open inside the window are flagged "in maintenance" and are not delivered to alert channels.

### Does planned work hurt uptime numbers?

No. Maintenance-window intervals are excluded from the availability computation — both on the monitor detail page and on the public status page. Downtime inside an announced window does not lower the uptime %.

### What happens to an incident that started before the window?

The "in maintenance" flag is set when the incident opens. An incident opened before the window is a regular one: the open notification has already gone out, and the close notification will be delivered too, even if the service recovers inside the window. That's why it pays to create the window ahead of the actual work.

### Can a window apply to a single monitor instead of the whole project?

No, a window covers the whole project. If the work affects one service while the project's other monitors must keep alerting, pause that monitor for the duration, or move the service into a separate project with its own windows.

### Will users learn about planned work in advance?

Yes, if the project has a [public status page](/docs/status-pages): upcoming maintenance windows are shown there as a separate list before the work starts.

### How does a window survive daylight-saving changes?

The schedule is stored together with the window's time zone (an IANA name, e.g. `Europe/Berlin`), and interval matching is computed in that zone. A weekly window at "every Saturday 03:00" keeps firing at local 03:00 after the clocks change in that zone.

## What's next

- [Uptime and monitors](/docs/uptime) — thresholds, consensus, incidents.
- [Alerts](/docs/alerts) — where notifications go outside maintenance windows.
- [Public status pages](/docs/status-pages) — where visitors see upcoming work.
