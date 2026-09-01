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
curl -sI https://gotcha.example/login | grep -Ei 'content-security-policy|x-frame-options|strict-transport'

# HSTS is present on the https instance...
curl -sI https://gotcha.example/login | grep -i strict-transport
# ...and absent on a plain-http deploy, regardless of config
# (empty unless your proxy itself adds HSTS on the http->https redirect —
# the recommended topology above allows that; then the header on the 301 is expected)
curl -sI http://gotcha.example/login | grep -i strict-transport   # expected: empty

# service paths are closed at the proxy
curl -s -o /dev/null -w '%{http_code}\n' https://gotcha.example/metrics   # expected 403
curl -s -o /dev/null -w '%{http_code}\n' https://gotcha.example/version  # expected 403

# TRACE is never served
curl -s -o /dev/null -w '%{http_code}\n' -X TRACE https://gotcha.example/login  # expected 404
```

A note on TRACE specifically: expect exactly **404, not 405**. The web layer intercepts every
method on this path with a single catch-all and always answers with the styled 404 page — the
absence of 405 here isn't a sign that TRACE is served in any way, it's a sign the route never
distinguishes methods at the mux level in the first place.
