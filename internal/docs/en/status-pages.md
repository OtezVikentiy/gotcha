# Public status pages

A public status page is a public view of the state of selected [monitors](/docs/uptime) in a project: a standalone page that requires no login, which you can link to for users and partners. Manage it at `/projects/{id}/statuspages` (available to an org owner/admin of the project).

## Creating a status page

1. Open the "Uptime" sidebar section → "Status Pages".
2. Click "New status page" — a create modal opens.
3. Fill in:
   - **Slug** — the short identifier used in the page URL (e.g. `main`). Same format as project/team slugs; must be unique across the instance.
   - **Title** — the heading visitors will see.
   - **Description** — a short blurb under the title (optional).
   - **Enabled** — turns the page on; a disabled page still shows in the dashboard but is not publicly reachable.
4. In the monitor list, check the ones that should appear on the page, and set a **Display name** for each — the public-facing name. It doesn't have to match the monitor's internal name: the real name is shown only to admins in this form, and only the display name is exposed publicly. The order you check monitors in is the order the tiles appear in.
5. Save with "Create".

You can later edit the page with the same form or remove it with "Delete". Once saved, the ready-made public link is shown right under the form.

## Public URL

A status page is reachable at:

```
{base_url}/status/{slug}
```

You'll find that link right under the edit form on `/projects/{id}/statuspages` — copy it and drop it, for example, into your site's footer or a support channel.

## What visitors see

The public page (`/status/{slug}`) requires no login and has no dashboard navigation — just:

- overall status ("All systems operational" / "Partial outage" / "Major outage");
- service tiles with their display name, current status (up/down/paused/maintenance/unknown), an availability bar, and 90-day uptime %;
- an incident feed for the last 90 days (service, start time, duration — no cause text or region detail);
- a list of upcoming scheduled [maintenance windows](/docs/maintenance).

Real monitor URLs, hosts, ports, and error text never reach the public page — only what you explicitly selected and labeled is exposed.

## What's next

- [Uptime and monitors](/docs/uptime) — what gets checked and how status is computed.
- [Maintenance windows](/docs/maintenance) — why planned work doesn't skew the numbers.
