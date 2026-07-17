# Security Policy

## Reporting a vulnerability

Please **do not** open a public GitHub/GitFlic issue for security
vulnerabilities. Instead, report privately using one of:

- Open a private security advisory on the repository host (if the hosting
  platform you're viewing this on supports private vulnerability reporting,
  e.g. GitHub Security Advisories), or
- Email the maintainers at: `<security contact>`

*(Maintainers: replace `<security contact>` above with a real, monitored
security contact address before publishing this repository.)*

Please include:

- A description of the vulnerability and its potential impact.
- Steps to reproduce (a minimal repro is very helpful).
- The affected version/commit.

We'll acknowledge reports as soon as possible and work with you on a fix and
disclosure timeline. Please give us a reasonable amount of time to address
the issue before any public disclosure.

## Supported versions

Gotcha does not yet have numbered releases; security fixes are applied to
the `main` branch. Once tagged releases exist, this section will be updated
to state which release lines receive security fixes.

## Security posture

A few defaults are worth knowing about when running Gotcha:

- **Privacy scrubbing is on by default.** Reporter IP addresses and email
  addresses are zeroed server-side (`GOTCHA_SCRUB_IP`, `GOTCHA_SCRUB_EMAIL`,
  both default `true`), and a denylist of sensitive key names (passwords,
  tokens, cookies, API keys, etc.) is redacted from tags/contexts/stack
  traces/span data by default (`GOTCHA_SCRUB_KEYS`).
- **SSRF protection is on by default.** Outbound requests made on your
  behalf — uptime checks and webhook alert deliveries — refuse to target
  private/loopback/link-local addresses unless you explicitly opt in with
  `GOTCHA_SSRF_ALLOW_PRIVATE=true`. Leave this off on any multi-tenant or
  internet-facing instance.
- **The default `GOTCHA_SECRET_KEY` is public** (it ships in source) and the
  process refuses to start in `web`/`all` mode against a non-local
  `GOTCHA_BASE_URL` unless a real secret is configured — see the README's
  Configuration section.

If you find a gap in any of the above, please report it as described above
rather than filing a public issue.
