# Organizations, projects and teams

## Hierarchy

- **Organization (org)** — the top level: billing/quotas, members, teams, probes, SSO. Everything else lives inside it.
- **Project** — a specific application or service: it has its own DSN, its own issues, its own monitors, and so on.
- **Team** — a group of an organization's members that can be granted access to a subset of projects, without handing out an organization-wide role.

## Roles

Roles are assigned at the organization level and apply across all its projects:

| Role | Can do |
|---|---|
| **owner** | Everything admin can, plus: grant/revoke the owner role for others, remove owners, configure the organization's [SSO](/docs/sso), export/delete subjects' personal data, delete the organization entirely |
| **admin** | Invite and remove members, change admin/member roles (not owner), manage ingest quotas, create/delete teams, manage probes and project settings (monitors, status pages, maintenance windows) |
| **member** | Works inside the projects they have access to (issues, performance, metrics, uptime, etc.), with no access to the organization's admin pages |

An organization's last owner cannot be demoted or removed — the system always protects against an organization ending up with no owner.

## Inviting members

The "Organization" → "Members" page (`/orgs/{id}/settings`, owner/admin only) has a table of current members (email, role, change-role, remove) and an invite form:

1. Enter the invitee's email.
2. Pick a role — **member** or **admin** (the **owner** role cannot be granted through an invite — it can only be assigned to an existing member afterward).
3. Click "Send invite".

If the server has no SMTP configured, the invite is still created but no email is sent — instead, the page shows a direct invite link once, which you forward to the person manually (it's not shown again).

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

Each team gets its own card on the page, with two lists and forms underneath:

- **Members** — a table of current members, an add form with a dropdown (listing only organization members not yet in this team), and a remove button on each row.
- **Projects** — a table of attached projects, an attach form with a dropdown (listing only organization projects not yet attached), and a detach button on each row.

Only someone already in the organization can be added to a team; attaching a project to a team doesn't change what organization owners/admins can see — they already see every project.

## What's next

- [SSO and social login](/docs/sso) — single sign-on instead of a password.
- [Probes](/docs/probes) — another owner/admin organization page.
- [Configuration](/docs/configuration) — registration modes and other server settings.
