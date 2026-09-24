# Hardening your install

The baseline production checklist lives in [Installation](/docs/installation); the table of
HSTS variables and how they constrain each other lives in [Configuration](/docs/configuration)
(Security section). This page is the roundup: what perimeter defense the reverse proxy in
front of the app closes, what the app itself closes, and how to check both halves after a
deploy.

## Where the boundary sits

The reverse proxy closes what the app can never reach from below: it terminates TLS, replaces
the web server's/hosting panel's default error pages (which typically leak the nginx or
Traefik version), restricts the set of HTTP methods, and decides which paths are visible from
outside at all. The app closes what lives inside its own response: security headers on every
page, `Strict-Transport-Security` when `GOTCHA_BASE_URL` is HTTPS, never serving TRACE, and
error pages that carry no build version or stack trace. Neither half substitutes for the
other — a proxy without these settings leaves holes the app cannot close in principle (the
nginx version is gone before the request ever reaches the Go process), and a bare app with no
proxy in front of it means plain HTTP with no TLS and service paths exposed to the internet.

## The reverse proxy

Three settings worth setting in any proxy in front of Gotcha: `server_tokens off`, so the
proxy itself doesn't leak its version in headers and default error pages; your own
`error_page` instead of nginx's/Traefik's/the hosting panel's default; and restricting methods
down to what's actually used — `GET`, `POST`, `HEAD`.

```nginx
server_tokens off;

location / {
    limit_except GET POST HEAD { deny all; }
    proxy_intercept_errors on;
    error_page 404 500 502 503 504 /error.html;
    proxy_pass http://127.0.0.1:59080;
}
```

`proxy_intercept_errors on` is required alongside `error_page` — without it nginx proxies the
backend's own error page as-is instead of replacing it.

## What to keep off the public internet

`/metrics`, `/version`, `/healthz`, `/readyz` are service endpoints meant to be reached from
the inside (an orchestrator, a monitoring system, yourself over an SSH tunnel), not from the
public. None of them require authentication. `/version`, `/healthz`, and `/readyz` all reveal
the exact build version anonymously (`version`/`commit` fields in the response body) — and an
exact version narrows an attacker's search to the vulnerabilities fixed in exactly that
release, instead of a blind guess. Full detail on these endpoints and what they expose —
[Monitoring gotcha itself](/docs/self-monitoring).

The app has no way to tell whether its port is published externally (`GOTCHA_COMPOSE_BIND`
is a Docker Compose variable that never reaches the process), so it logs a warning about
these four endpoints on every startup unconditionally, even once you've locked them down.
It's not a diagnostic of a problem — it's a nudge to check the configuration below.

```nginx
location ~ ^/(metrics|version|healthz|readyz)$ {
    allow 10.0.0.0/8;
    allow 127.0.0.1;
    deny all;
    proxy_pass http://127.0.0.1:59080;
}
```

```caddy
@internal path /metrics /version /healthz /readyz
handle @internal {
    @allowed remote_ip 10.0.0.0/8 127.0.0.1
    handle @allowed { reverse_proxy localhost:59080 }
    respond 403
}
```

Replace `10.0.0.0/8` with the range your probes actually come from (orchestrator, Prometheus,
your own network) — the default-open range is meaningless as a restriction.

On bare metal, close `/metrics` and `/version` in your own proxy — the examples in
"External access and TLS" in [Installation without Docker](/docs/installation-bare-metal)
already do this: both endpoints answer 403 from the outside, open only to loopback
(`127.0.0.1`/`::1`). `/healthz` and `/readyz` are deliberately left open — they're the only two endpoints that need
to answer from the outside, for external checks of the instance's own availability. The price
is exactly the one named above: both probes hand out a `version` field anonymously, that is,
the exact build version. If that's unacceptable for your install, add `healthz|readyz` to the
same `location` block of your nginx site (`location ~ ^/(metrics|version|healthz|readyz)$`) and point
your external availability check at a regular page instead of a probe (for Apache/Caddy —
the equivalent rule in your proxy). To let
your own metrics collector reach `/metrics`, add its subnet as an `allow ...;` line before
`deny all;` in the `location ~ ^/(metrics|version)$` block of
your nginx site and reload the config (`nginx -t && systemctl reload nginx`).

The same goes for the databases. The stock `docker-compose.yml` doesn't publish the PostgreSQL
and ClickHouse ports on the host — only containers on the same docker network can reach them
— but both ship with the default password `gotcha` / `gotcha`, identical on every install in
the world. Change it via `GOTCHA_COMPOSE_PG_PASSWORD` / `GOTCHA_COMPOSE_CH_PASSWORD` **before
the first start** (once the volume is initialized, the variable alone changes nothing), and on
a running install run `ALTER USER` inside the database first, then set the variable; the
commands are in [Configuration](/docs/configuration#compose-only-variables-database-containers).
And don't add `ports:` to the databases "for convenience": with the default password that is
an open database on a public address. Bare metal has no such hole by construction:
`install-bare-metal.sh` generates a random password for each database on first install
(`openssl rand -hex 24`), so there's no password shared across every install in the world
— and PostgreSQL and ClickHouse only listen on `127.0.0.1` to begin with, with no port
published externally unless you set one up by hand.

## TLS and HSTS

TLS 1.2 or newer, with a redirect from plain HTTP to HTTPS. Set HSTS in exactly ONE place —
either the proxy or the app, never both: two sources of the header on one response don't add
up, they just mask each other. If the proxy already sends HSTS, set
`GOTCHA_HSTS_ENABLED=false` in the app.

The app assembles the header from four variables:

| Variable | Default | Meaning |
|---|---|---|
| `GOTCHA_HSTS_ENABLED` | `true` | Whether to send `Strict-Transport-Security` at all (https responses only). |
| `GOTCHA_HSTS_MAX_AGE_SECONDS` | `31536000` | How many seconds a browser should remember the HTTPS requirement (default: one year); `0` is not "off" but a deliberate emergency rollback, see below. |
| `GOTCHA_HSTS_INCLUDE_SUBDOMAINS` | `false` | Extend the HTTPS requirement to every subdomain of the host in `GOTCHA_BASE_URL`. |
| `GOTCHA_HSTS_PRELOAD` | `false` | Mark the instance as a candidate for browser HSTS preload lists. |

The exact startup-refusal rules, what `MAX_AGE_SECONDS=0` does, and why turning HSTS off does
not un-pin a browser that already cached it — in
[Configuration](/docs/configuration#security).

Only enable `includeSubDomains` if you control (or have already verified HTTPS on) the entire
parent domain: with this flag, `gotcha.example.com` requires HTTPS not just for itself but for
every service on `example.com`, including ones you don't administer and that may not be
HTTPS-ready.

Preload is a one-way ticket: once a domain lands on a browser's preload list, it's baked into
browser releases for months, and getting it removed is a matter of months, not minutes. The
way out during an emergency is, strictly in this order — otherwise the app refuses to start,
because config validation requires at least a year of max-age while `PRELOAD=true` (see
[Configuration](/docs/configuration#security)):

1. `GOTCHA_HSTS_PRELOAD=false` — lifts the year-long max-age requirement that would otherwise
   block step 2.
2. `GOTCHA_HSTS_MAX_AGE_SECONDS=0`, keeping `GOTCHA_HSTS_ENABLED=true` — a header with a zero
   max-age is actually sent to clients and un-pins them.
3. Wait for the pin to expire on clients that already visited the instance.
4. Only now `GOTCHA_HSTS_ENABLED=false`, if the header isn't needed at all anymore.

Turning HSTS off by itself does **not** un-pin — it just stops renewing the pin, so step 4
without steps 1-3 doesn't end the emergency, it freezes it for the duration of the previously
sent max-age.

## Bare metal: a systemd unit instead of the container runtime

On a Docker-free install ([Installation without Docker](/docs/installation-bare-metal)),
the same process hardening the container runtime provides on the Docker path comes from
the systemd unit `/etc/systemd/system/gotcha.service`, written by
`install-bare-metal.sh`. Line-by-line parity:

| Docker Compose | systemd unit |
|---|---|
| `read_only: true` | `ProtectSystem=strict` (writes allowed only into `StateDirectory=`) |
| `tmpfs: [/tmp]` | `PrivateTmp=yes` |
| `cap_drop: [ALL]` | `CapabilityBoundingSet=` and `AmbientCapabilities=` (both empty) |
| `no-new-privileges` | `NoNewPrivileges=yes` |
| `pids_limit: 512` | `TasksMax=512` |
| `mem_limit: 1g` | `MemoryMax=` + `MemoryAccounting=yes` + an explicit `GOMEMLIMIT` in `gotcha.env` |
| `stop_grace_period: 90s` | `TimeoutStopSec=90` |
| `restart: unless-stopped` | `Restart=always`, `RestartSec=5` |
| the exports volume | `StateDirectory=gotcha`, `StateDirectoryMode=0700` |
| `logging: json-file` | journald (`journalctl -u gotcha`) |
| `depends_on: service_healthy` | `After=postgresql.service clickhouse-server.service network-online.target` |

The unit goes beyond parity: `ProtectHome`, `PrivateDevices`, `ProtectKernelTunables`,
`ProtectKernelModules`, `ProtectKernelLogs`, `ProtectControlGroups`, `ProtectClock`,
`ProtectHostname`, `ProtectProc=invisible`, `RestrictNamespaces`, `RestrictRealtime`,
`RestrictSUIDSGID`, `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`,
`LockPersonality`, `SystemCallFilter=@system-service`, `SystemCallArchitectures=native`,
`UMask=0077`, and `MemoryDenyWriteExecute=yes` — the container runtime simply has no
equivalent for these, they're specific to systemd.

`After=` without `Requires=` is deliberate: restarting PostgreSQL or ClickHouse shouldn't
drag the app down with it — it survives the database being briefly unreachable and
recovers on its own via `Restart=always` once the database is back, the same behavior as
on the Docker path.

Check what actually got applied after installing:

```bash
systemctl show gotcha.service -p MemoryDenyWriteExecute,ProtectSystem,NoNewPrivileges
systemd-analyze security gotcha.service
```

`systemd-analyze security` prints what each directive buys you and an overall score — the
same kind of tool as `docker inspect` for the container runtime, just for a unit.

## security.txt

The app deliberately does not serve `/.well-known/security.txt` itself — this is a choice, not
an oversight. A security contact is a property of the DOMAIN, not of a particular app running
on it: a large share of Gotcha installs live on a subdomain of someone else's domain (shared
hosting, a corporate portal), and the security contact belongs to whoever owns that domain,
not to Gotcha. Put the file on the proxy instead:

```nginx
location = /.well-known/security.txt {
    default_type text/plain;
    return 200 "Contact: mailto:security@example.com\nExpires: 2027-01-01T00:00:00.000Z\n";
}
```

## Self-check

After a deploy, check both halves of the perimeter — proxy and app — with a handful of `curl`
commands:

```bash
# app security headers on the login page
curl -sI https://gotcha.example/login \
  | grep -Ei 'content-security-policy|x-frame-options|strict-transport'

# HSTS is present on the https instance...
curl -sI https://gotcha.example/login | grep -i strict-transport
# ...and absent on a plain-http deploy, regardless of config
# (empty unless your proxy itself adds HSTS on the http->https redirect —
# the recommended topology above allows that; then the header on the 301 is expected)
curl -sI http://gotcha.example/login | grep -i strict-transport # expected: empty

# service paths are closed at the proxy
curl -s -o /dev/null -w '%{http_code}\n' https://gotcha.example/metrics # expected 403
curl -s -o /dev/null -w '%{http_code}\n' https://gotcha.example/version # expected 403

# TRACE is never served
curl -s -o /dev/null -w '%{http_code}\n' -X TRACE https://gotcha.example/login # expected 404
```

A note on TRACE specifically: expect exactly **404, not 405**. The web layer intercepts every
method on this path with a single catch-all and always answers with the styled 404 page — the
absence of 405 here isn't a sign that TRACE is served in any way, it's a sign the route never
distinguishes methods at the mux level in the first place.
