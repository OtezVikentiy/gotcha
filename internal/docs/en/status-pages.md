# Public status pages

A public status page is a public view of the state of selected [monitors](/docs/uptime) in a project: a standalone page that requires no login, which you can link to for users and partners. Manage its content at `/projects/{id}/statuspages` — available to a project team member (an operator) as well as to the organization's owner and admins. Publishing a page is admin/owner-only, though — see [Roles and access](/docs/teams).

## Creating a status page

1. Open the "Uptime" sidebar section → "Status Pages".
2. Click "New status page" — a create modal opens.
3. Fill in:
   - **Title** — the heading visitors will see; it's also the page's public name, so pick something visitors will recognize.
   - **Description** — a short blurb under the title (optional).
   - **Published** — turns the page on; a disabled page still shows in the dashboard but is not publicly reachable.
4. In the monitor list, check the ones that should appear on the page, and set a **Public name** for each — the public-facing name. It doesn't have to match the monitor's internal name: the real name is shown only inside this management form (to whoever can manage the page — operators, admins, and owners), and only the public name is exposed publicly. Tiles are ordered alphabetically by monitor name; the order you check them in doesn't change that.
5. Save with "Create page".

You can later edit the page with the same form or remove it with "Delete". Once saved, the ready-made public link is shown right under the form.

## Public URL

A status page is reachable at:

```
{base_url}/status/{public_id}
```

`{public_id}` (`p_<hex>`) is an opaque key the system generates when the page is created — there's nothing to type or edit, and it never changes for the life of the page. Earlier versions used a human-chosen slug instead; a slug could be guessed at, which let anyone probe whether a given name was already taken by another organization on the same instance, and let short, desirable names get squatted on. Links of the form `/status/{slug}` still work — they answer with a 301 redirect to the page's `p_...` address, so bookmarks and links shared before the change keep working. A custom domain for a status page is planned but not available yet.

You'll find the current public link right under the edit form on `/projects/{id}/statuspages` — copy it and drop it, for example, into your site's footer or a support channel.

## What visitors see

The public page (`/status/{public_id}`) requires no login and has no dashboard navigation — just:

- overall status ("All systems operational" / "Partial outage" / "Major outage");
- service tiles with their public name, current status (up/down/paused/maintenance/unknown), an availability bar, and 90-day uptime % (or the `GOTCHA_RETENTION_DAYS` window, whichever is shorter — the page never shows cells beyond what is actually stored);
- an incident feed for the last 90 days (service, start time, duration — no cause text or region detail);
- a list of upcoming scheduled [maintenance windows](/docs/maintenance).

Real monitor URLs, hosts, ports, and error text never reach the public page — only what you explicitly selected and labeled is exposed.

## FAQ

### Do visitors need an account to open a status page?

No. The `/status/{public_id}` page opens without logging in — link to it from anywhere: for customers, partners, or a support channel. It carries no dashboard navigation.

### What data is exposed publicly?

Only what you explicitly selected and labeled: public service names, their status, uptime %, and an incident feed without details. Real monitor URLs, hosts, ports, and error text never reach the public page.

### Can one project have several status pages?

Yes. Pages are managed as a list on `/projects/{id}/statuspages`, each with its own public link and its own set of monitors — e.g. one page for customers and a more detailed one for partners.

### I have an old `/status/<slug>` link — will it still work?

Yes. It answers with a 301 redirect to the page's current `/status/p_...` address, so bookmarks, footers, and status-page links shared before the change don't break.

### How do I hide a page temporarily without deleting it?

Uncheck **Published** in the edit form. The page stays in the dashboard with all its settings, but the public URL stops resolving until you re-enable it.

### Does planned maintenance count against the uptime %?

It counts in the service's favor: [maintenance window](/docs/maintenance) intervals are excluded from the computation, so pre-announced planned downtime does not lower the uptime shown on the page. Upcoming scheduled windows are also shown to visitors as a separate list.

### How much history does the page show?

The availability bar and uptime % cover 90 days, and so does the incident feed. If the `GOTCHA_RETENTION_DAYS` retention is shorter, the page shows exactly what is stored — it never renders cells out of thin air.

## What's next

- [Uptime and monitors](/docs/uptime) — what gets checked and how status is computed.
- [Maintenance windows](/docs/maintenance) — why planned work doesn't skew the numbers.
