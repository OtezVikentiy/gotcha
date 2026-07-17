# Maintenance windows

A maintenance window is a pre-announced interval of planned work for a project. While it's active, incidents opened by that project's [monitors](/docs/uptime) are marked "in maintenance" and **do not send notifications** through [alerts](/docs/alerts) — so a planned reboot or deploy doesn't flood your channel with false alarms. The incident itself is still recorded (visible in the history), just without a delivery.

The page lives at `/projects/{id}/maintenance`, available to an org owner/admin of the project.

## Creating a window

1. On the "Maintenance" page, click "New maintenance window" — a create modal opens.
2. Enter a **Name** — a short description of the work (e.g. "Database upgrade").
3. Pick the type with the **Weekly** checkbox:
   - **unchecked** — a one-off window: set **Start** and **End** (date and time, `datetime-local` fields);
   - **checked** — a recurring weekly window: set the **Weekday** and a **Start**/**End** time in HH:MM, which repeats every week on that day.
4. Set the **Timezone** — pick one from the list (UTC, Europe/Moscow, Europe/Berlin, Asia/Yekaterinburg), or, if the one you need isn't listed, pick "Other" and type an IANA name into the adjacent field (e.g. `America/New_York`).
5. Save with "Create".

Created windows are listed with their name, type, and schedule, each with a delete button.

## Effect on monitors and alerts

A maintenance window applies to the **whole project**, not a single monitor: while the current time falls inside any active window, every incident that opens at that moment is flagged "in maintenance" and does not notify. Checks keep running as usual — a maintenance window doesn't pause monitoring, it only suppresses notification noise.

On the monitor detail page and in the incident list, such incidents are marked with a separate "Maintenance" column (yes/no), so you can always tell a real alert apart from expected downtime during planned work.

On a [public status page](/docs/status-pages), upcoming maintenance windows are shown to visitors as a separate list — a way to warn users about planned downtime ahead of time.

## What's next

- [Uptime and monitors](/docs/uptime) — thresholds, consensus, incidents.
- [Alerts](/docs/alerts) — where notifications go outside maintenance windows.
- [Public status pages](/docs/status-pages) — where visitors see upcoming work.
