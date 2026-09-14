# SSO and social login

Besides passwords, Gotcha can sign users in through external providers: a generic **OIDC** provider (any compatible IdP — Keycloak, Authentik, Auth0, etc.), **Yandex ID**, and **VK ID**. Each provider is enabled independently through server environment variables — this is an instance-level setting; there's no UI for it.

Secrets for providers configured through instance environment variables live only in the process's memory — they are never in the database.

Separately, there is **per-org SSO**: an instance admin configures OIDC for one organization through the UI, and there the `client_secret` is stored in the database (`org_sso`), encrypted with the `GOTCHA_SECRET_KEY` master key. These are two different mechanisms, and "secrets are never in the database" applies only to the first.

## How it works

A provider is turned on with a `*_ENABLED` boolean; if it's enabled but its required variables (client id/secret, etc.) are missing, **the server refuses to start**, with a clear configuration error. Enabled providers show up as login buttons on the login page.

The callback (redirect URI) you need to register in the provider's application settings always has this shape:

```
{GOTCHA_BASE_URL}/auth/oauth/{provider}/callback
```

where `{provider}` is `oidc`, `yandex`, or `vk` depending on the provider, and `{GOTCHA_BASE_URL}` is the same address configured in the server's `GOTCHA_BASE_URL` (e.g. `https://gotcha.example.com`). For generic OIDC, that's `https://gotcha.example.com/auth/oauth/oidc/callback`. The URI isn't separately configurable — it's always built this way, so make sure you register the exact same address with the provider.

Yandex ID and VK ID confirm the user's address themselves, so both auto-provisioning a new account and auto-linking a login to an existing account by email are allowed for them. A generic OIDC provider's claimed `email`/`email_verified` can be forged by the IdP itself, so by default (`GOTCHA_OIDC_TRUST_EMAIL=false`) logging in through it only works for accounts already linked — self-registration and email-based auto-linking need to be turned on explicitly, see below.

## Generic OIDC — step by step

1. In your IdP's console (Keycloak, Authentik, Auth0, Zitadel, etc.), create a new OAuth/OIDC application (client) of type "confidential"/"web".
2. Set its **redirect URI** to `{GOTCHA_BASE_URL}/auth/oauth/oidc/callback` — exactly that, with your real `GOTCHA_BASE_URL`.
3. Copy the **Issuer** (typically something like `https://idp.example.com/realms/myrealm` — the base address where `.well-known/openid-configuration` is served), the **Client ID**, and the **Client secret** from the application's settings in the IdP.
4. Set the server's environment variables:

```bash
GOTCHA_OIDC_ENABLED=true
GOTCHA_OIDC_ISSUER=https://idp.example.com/realms/myrealm
GOTCHA_OIDC_CLIENT_ID=<client id from the IdP>
GOTCHA_OIDC_CLIENT_SECRET=<client secret from the IdP>
GOTCHA_OIDC_SCOPES=openid email profile   # optional — this is already the default
GOTCHA_OIDC_DISPLAY_NAME=Corp SSO                 # optional — the button label on /login (defaults to "OIDC")
```

5. Restart the server. The `/login` page will show a "Sign in with {GOTCHA_OIDC_DISPLAY_NAME or OIDC}" button.

Gotcha fetches `{issuer}/.well-known/openid-configuration` itself to discover the authorization/token endpoints and the JWKS — you don't need to set those manually.

By default, this is only enough for signing in people whose account is already linked to this provider. To let people SELF-REGISTER through this same OIDC provider (open registration or an invite) or have a login auto-linked to an existing account by matching email, add:

```bash
GOTCHA_OIDC_TRUST_EMAIL=true
```

Only turn this on if the IdP is your own, single-tenant one (your own Keycloak/Authentik/Zitadel, etc.) where you control who can create an account. **Do not enable it** for a public multi-tenant IdP (a shared Google tenant, a general-purpose Auth0 tenant, etc.) — there, anyone can sign up and claim someone else's email address, and `GOTCHA_OIDC_TRUST_EMAIL=true` would make Gotcha take that claim at face value and hand over access to the account with that address.

## Yandex ID

1. Register an application in [Yandex OAuth](https://oauth.yandex.ru) (or the Yandex ID developer console).
2. Redirect URI: `{GOTCHA_BASE_URL}/auth/oauth/yandex/callback`.
3. Copy the application's **ID** and **secret (password)**.
4. Environment variables:

```bash
GOTCHA_YANDEX_ENABLED=true
GOTCHA_YANDEX_CLIENT_ID=<application ID>
GOTCHA_YANDEX_CLIENT_SECRET=<application secret>
```

The `/login` button reads "Sign in with Yandex".

## VK ID

1. Register an application in the VK ID developer console.
2. Redirect URI: `{GOTCHA_BASE_URL}/auth/oauth/vk/callback`.
3. Copy the **application ID** and the **secure key (client secret)**.
4. Environment variables:

```bash
GOTCHA_VK_ENABLED=true
GOTCHA_VK_CLIENT_ID=<application ID>
GOTCHA_VK_CLIENT_SECRET=<secure key>
```

The `/login` button reads "Sign in with VK".

## What happens at sign-in

- If the provider's email is already linked to an existing account (or matches an existing user's verified email), sign-in issues a session right away.
- If no account exists for that email yet, what happens depends on `GOTCHA_REGISTRATION_MODE`: under `open`, an account is created on the first sign-in through a provider (a pending invite for that email, if any, is accepted along the way); under `invite`, a new user is provisioned only if there's a pending invite for that email (see [Inviting members](/docs/teams)) — otherwise sign-in is rejected; under `closed`, no new accounts appear at all.
- From `/profile`, a signed-in user can additionally link a provider to their existing account through the same flow (`?link=1`).

## How this differs from per-org enterprise SSO

Separately from these instance-level providers, each organization has its own optional **SSO** section on `/orgs/{id}/settings`: its own OIDC provider for signing in members with a specific email domain, with an "enforced" option (mandatory SSO for that domain — passwords and the general providers above stop working for those emails). The settings page itself is reachable by the org's owner or admin, but the SSO section within it renders only for the org's owner, or separately for anyone marked an [**instance administrator**](/docs/teams#instance-admin) — a flag independent of any org role, so an instance admin sees the section even without owning that org, while an org admin who isn't also an instance admin doesn't see it at all. Seeing the section still isn't enough to configure it: setting up or removing the federation requires instance-administrator status specifically — an org's owner who isn't an instance admin can see the current SSO status but not the configuration form, and org ownership plays no part in the configure/delete gate. This split exists because a self-service org owner claiming a domain they don't control would be an account-takeover vector; only someone the instance operator trusts with that domain can wire it up.

The setup form takes an **issuer** (must be `https://`), **client id**, **client secret**, a **domain** (the member email domain this applies to), and a **default role** (`member` or `admin`) — the role a person gets the first time they sign in through this SSO without already being a member of the organization. The redirect URI to register with your IdP is `{GOTCHA_BASE_URL}/auth/oauth/sso-{org ID}/callback` (the org ID is visible in the settings page's URL). The IdP must confirm the email (`email_verified`) — unlike the instance-level generic OIDC above, there is no separate switch here to relax that: an unverified email is always rejected, and a verified email outside the configured domain is rejected too (the IdP could have forged it).

**The `/sso` page** is a sign-in entry point separate from `/login`, built specifically for this mechanism: a "Sign in with SSO" link appears at the bottom of both `/login` and `/register`. A visitor enters their work email, the server looks up an organization by that email's domain and, if one is found, redirects to that organization's OIDC provider; if no org has SSO configured for the domain, the form answers "SSO is not configured for this domain" on the same page. Like `/login`/`/register`, this is a separate flow from those two — attempting a password sign-in on `/login` with an email from a domain where `enforced` is on is rejected with "Your organization requires SSO — use 'Sign in with SSO'", and the same message greets a `/register` attempt with such an email.

**What `enforced` actually breaks.** From the moment it's turned on, everyone whose email ends in that domain loses password sign-in and the general providers (Yandex/VK/generic OIDC) entirely — whether or not they're already a member of this organization, and regardless of their role in it: `enforced` is keyed to the email domain, not to membership. The only way in becomes `/sso` with that domain. An existing account with a verified email on that domain is added to the organization automatically, with the default role, the first time it signs in through `/sso` — if it was already a member, its role is left unchanged. Only an instance administrator can turn `enforced` off or remove the organization's SSO configuration entirely — the org's own owner has no path to that (see above), so if an organization loses access to its IdP, its owner cannot roll this switch back alone and has to ask an instance administrator.

## What's next

- [Teams and roles](/docs/teams) — the invites a new OAuth user needs to be provisioned.
- [Configuration](/docs/configuration) — the rest of the server's environment variables.
