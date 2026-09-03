[English](SECURITY.md) · [Русский](SECURITY.ru.md)

# Security Policy

## Reporting a vulnerability

Please **do not** open a public GitHub/GitFlic issue for security
vulnerabilities. Instead, report privately using one of:

- Open a private security advisory on the repository host (if the hosting
  platform you're viewing this on supports private vulnerability reporting,
  e.g. GitHub Security Advisories), or
- Email the maintainers at: otezvikentiy@gmail.com

Please include:

- A description of the vulnerability and its potential impact.
- Steps to reproduce (a minimal repro is very helpful).
- The affected version/commit.

We'll acknowledge reports as soon as possible and work with you on a fix and
disclosure timeline. Please give us a reasonable amount of time to address
the issue before any public disclosure.

## Supported versions

Security fixes land in the `main` branch and ship with the next tagged
release. The latest release line is supported; older releases do not
receive backported fixes — upgrade to the current release to get them.

## Security posture

A few defaults are worth knowing about when running Gotcha:

- **PII scrubbing is on by default.** Reporter IP and email
  addresses are zeroed server-side (`GOTCHA_SCRUB_IP`, `GOTCHA_SCRUB_EMAIL`,
  both default `true`), and a denylist of sensitive key names (passwords,
  tokens, cookies, API keys, etc.) is redacted from tags/contexts/stack
  traces/span data by default (`GOTCHA_SCRUB_DENY_KEYS`).
- **SSRF protection is on by default.** Outbound requests made on your
  behalf — uptime checks and webhook alert deliveries — refuse to target
  private/loopback/link-local addresses unless you explicitly opt in with
  `GOTCHA_SSRF_ALLOW_PRIVATE=true`. Leave this off on any multi-tenant or
  internet-facing instance.
- **The default `GOTCHA_SECRET_KEY` is public** (it ships in source) and the
  process refuses to start in every mode except `probe` against a non-local
  `GOTCHA_BASE_URL` unless a real secret is configured — see the README's
  Configuration section.

If you find a gap in any of the above, please report it as described above
rather than filing a public issue.
