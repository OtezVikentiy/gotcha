# Probes (remote regions)

A probe is a separate `gotcha` process, started with `--mode=probe`, that runs [monitor](/docs/uptime) checks from its own point in the network (a different city, data center, or cloud) and reports results back to the central server. Probes let you tell a local network hiccup near the Gotcha server apart from a real service outage, and let monitors use regional consensus (see [Uptime](/docs/uptime)).

An installation always has a built-in local region (checks run by the server itself); probes add extra regions on top of it.

Manage probes on `/orgs/{id}/probes`, available only to an org owner/admin.

## Registering a probe

1. Open "Organization" → "Probes".
2. In the form at the bottom of the page, fill in:
   - **Name** — a human-readable name for the probe (e.g. "Moscow probe"), up to 40 characters;
   - **Region** — the region identifier monitors will see when picking regions (e.g. `ru-msk`), up to 40 characters. The same region name can be reused by several probes — that's how you scale one region across multiple probe instances.
3. Click "Create probe".

The probe's token is shown **once**, right after creation, on the page itself — save it now, it will not be shown again (only its hash is stored in the database). The same block also gives you a ready-to-run command with your server address and this token already filled in.

## Running a probe

A probe needs no access to PostgreSQL or ClickHouse — only outbound HTTP(S) to the central server. Two environment variables are required:

- `GOTCHA_SERVER_URL` — the central Gotcha server's base URL (the same value as `GOTCHA_BASE_URL` on the server), e.g. `https://gotcha.example.com`;
- `GOTCHA_PROBE_TOKEN` — the token you got when registering the probe.

Example run with Docker (this exact command, with your values filled in, is what the "Probes" page shows you after creating a probe):

```bash
docker run -e GOTCHA_SERVER_URL=https://gotcha.example.com \
  -e GOTCHA_PROBE_TOKEN=6e1f2a...af92 \
  gotcha --mode=probe
```

The same process can run without Docker, from a built `gotcha` binary:

```bash
GOTCHA_SERVER_URL=https://gotcha.example.com \
GOTCHA_PROBE_TOKEN=6e1f2a...af92 \
./gotcha --mode=probe
```

The probe reports in to the center as soon as it starts — the "Probes" page will show status **online** and the time of its last check-in. If a probe stops reporting, its status flips to **offline**; a revoked probe (the "Revoke" button) is marked **revoked** and its token stops being accepted immediately.

## How regions show up in monitors

Once a probe has checked in at least once, its `Region` appears in the list of available regions in the monitor form (`/projects/{id}/monitors/new` and Edit), alongside the built-in local region. Check the regions you want and set a consensus rule (see [Uptime](/docs/uptime)) — the monitor will start being checked from all selected points in parallel.

## What's next

- [Uptime and monitors](/docs/uptime) — how status is computed across multiple regions.
- [Teams and roles](/docs/teams) — who can manage probes.
