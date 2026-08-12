# Organizations, projects and teams

## Hierarchy

- **Organization (org)** — the top level: billing/quotas, members, teams, probes, SSO. Everything else lives inside it.
- **Project** — a specific application or service: it has its own DSN, its own issues, its own monitors, and so on.
- **Team** — a group of an organization's members that can be granted access to a subset of projects, without handing out an organization-wide role.

## Roles

Roles are assigned at the organization level and apply across all its projects:

| Role | Can do |
|---|---|
| **owner** | Everything admin can, plus: grant/revoke the owner role for others, remove owners, see the organization's [SSO](/docs/sso) section (configuring it isn't gated by the owner role at all, but by a separate "instance administrator" flag — the section is visible to whoever carries that flag too, even for organizations they don't own), export/delete subjects' personal data, delete the organization entirely |
| **admin** | Invite and remove members, change admin/member roles (not owner), manage ingest quotas, create/delete teams, manage probes and project settings (monitors, status pages, maintenance windows) |
| **member** | Works inside the projects they have access to (issues, performance, metrics, uptime, etc.), with no access to the organization's admin pages |

An organization's last owner cannot be demoted or removed — the system always protects against an organization ending up with no owner.

## Inviting members

The "Organization" → "Members" page (`/orgs/{id}/settings`, owner/admin only) has a table of current members (email, role, change-role, remove) and an invite form:

1. Enter the invitee's email.
2. Pick a role — **member** or **admin** (the **owner** role cannot be granted through an invite — it can only be assigned to an existing member afterward).
3. Click "Invite".

If the server has no SMTP configured, the invite is still created but no email is sent — instead, the page shows a direct invite link once, which you forward to the person manually (it's not shown again).

Until it is accepted, an invitation is listed under "Awaiting acceptance" on the same page and can be revoked there — mistype an address, and revoking kills the link before a stranger can use it. Accepting removes the invitation from the list and adds the member.

What happens next depends on the instance's **registration mode** (`GOTCHA_REGISTRATION`, see [Configuration](/docs/configuration)):

- **open** — self-registration is always open; an invite is just a way to add someone with a specific role right away;
- **invite** (default) — self-registration closes once the instance has its first user; joining afterward requires a valid invite link (or OAuth/SSO, if a pending invite exists for that email);
- **closed** — self-registration is closed entirely, except for the instance's very first user.

## Teams

The "Organization" → "Teams" page (`/orgs/{id}/teams`, owner/admin only) lets you group members and grant them access to specific projects without changing their organization-wide role.

Creating a team:

1. Click the "+" next to the page heading — a "Create team" modal opens.
2. Enter a **Slug** (a short identifier, e.g. `backend`) and a **Name** (the display name, e.g. "Backend").
3. Save.

Each team gets its own card on the page, with two lists and forms underneath. The card heading carries a "Rename" button: it changes the display name without touching members or attached projects. The slug stays as it is — it is used in URLs and in access grants.

Inside the card:

- **Members** — a table of current members, an add form with a dropdown (listing only organization members not yet in this team), and a remove button on each row.
- **Projects** — a table of attached projects, an attach form with a dropdown (listing only organization projects not yet attached), and a detach button on each row.

Only someone already in the organization can be added to a team; attaching a project to a team doesn't change what organization owners/admins can see — they already see every project.

## Roles and access

Being on a team attached to a project — an **operator**, in the table below — is what lets a `member` actually run the monitoring day to day, without promoting them to `admin` and handing them every other project in the organization along with it. Org owners and admins automatically qualify as operators on every project too, since they already have full access.

| Action | Viewer (access) | Operator (team member) | Admin | Owner |
|---|:---:|:---:|:---:|:---:|
| View issues, performance, uptime, alerts, etc. | ✓ | ✓ | ✓ | ✓ |
| Change an issue's or a performance issue's status | ✓ | ✓ | ✓ | ✓ |
| Monitors: create, edit, pause/resume, delete | — | ✓ | ✓ | ✓ |
| Heartbeat monitor: regenerate the ping token | — | ✓ | ✓ | ✓ |
| Maintenance windows: create, edit, delete | — | ✓ | ✓ | ✓ |
| Status page content: create/edit/delete a page, pick monitors and titles | — | ✓ (a page an operator creates starts unpublished) | ✓ | ✓ |
| Status page publication: the "Published" toggle | — | — | ✓ | ✓ |
| Alert rules (new issue / regression / spike) | — | ✓ | ✓ | ✓ |
| Alert channels: create, edit, delete, "Test" | — | — (sees each channel's kind and a masked target only, enough to tell channels apart when picking one in a rule) | ✓ | ✓ |
| Delivery log | — | ✓ (targets masked) | ✓ (full) | ✓ (full) |
| Metric alerts: create, delete | — | ✓ | ✓ | ✓ |
| Project settings (rename, DSN keys, quotas, sample rate) | — | — | ✓ | ✓ |
| Create a new project | — | — | ✓ | ✓ |
| Organization management (members, roles, invites, teams, probes) | — | — | ✓ | ✓ |
| Delete a project or the organization | — | — | — | ✓ |

"Viewer" here isn't a role of its own — it's what `CanAccessProject` grants to anyone who can already see the project (an org owner/admin, or a plain `member` on an attached team), before touching the operator predicate at all. In other words, every operator is also a viewer, and every admin and owner is also an operator: the columns are cumulative, not exclusive tiers.

What team membership buys a `member`, in short: everything about running monitoring day to day on that project's monitors, maintenance windows, status pages, alert rules, and metric alerts — the operational surface, without an organization-wide role. What still requires `admin` (or `owner`): alert channels, because their recipient and secret (a bot token, an SMTP address, a webhook URL) are credentials and personal data, not operational settings; whether a status page is actually public, because that's a decision about what the organization shows the world, not how monitoring is run; and project settings and everything at the organization level, unchanged from before.

## What's next

- [SSO and social login](/docs/sso) — single sign-on instead of a password.
- [Probes](/docs/probes) — another owner/admin organization page.
- [Configuration](/docs/configuration) — registration modes and other server settings.
