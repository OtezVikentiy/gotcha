# SSO and social login

Besides passwords, Gotcha can sign users in through external providers: a generic **OIDC** provider (any compatible IdP — Keycloak, Authentik, Auth0, etc.), **Yandex ID**, and **VK ID**. Each provider is enabled independently through server environment variables — this is an instance-level setting; there's no UI for it.

Secrets live only in the process's memory (env), never in the database.

## How it works

A provider is turned on with a `*_ENABLED` boolean; if it's enabled but its required variables (client id/secret, etc.) are missing, **the server refuses to start**, with a clear configuration error. Enabled providers show up as login buttons on the login page.

The callback (redirect URI) you need to register in the provider's application settings always has this shape:

```
{GOTCHA_BASE_URL}/auth/oauth/{provider}/callback
```

where `{provider}` is `oidc`, `yandex`, or `vk` depending on the provider, and `{GOTCHA_BASE_URL}` is the same address configured in the server's `GOTCHA_BASE_URL` (e.g. `https://gotcha.example.com`). For generic OIDC, that's `https://gotcha.example.com/auth/oauth/oidc/callback`. The URI isn't separately configurable — it's always built this way, so make sure you register the exact same address with the provider.

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
GOTCHA_OIDC_NAME=Corp SSO                 # optional — the button label on /login (defaults to "OIDC")
```

5. Restart the server. The `/login` page will show a "Sign in with {GOTCHA_OIDC_NAME or OIDC}" button.

Gotcha fetches `{issuer}/.well-known/openid-configuration` itself to discover the authorization/token endpoints and the JWKS — you don't need to set those manually.

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
- If no account exists for that email yet, the provider provisions a new user only if there's a pending invite for that email (see [Inviting members](/docs/teams)); without an invite, sign-in with an email unknown to the system is rejected.
- From `/profile`, a signed-in user can additionally link a provider to their existing account through the same flow (`?link=1`).

## How this differs from per-org enterprise SSO

Separately from these instance-level providers, each organization has its own optional **SSO** section on `/orgs/{id}/settings` (owner only): its own OIDC provider for signing in members with a specific email domain, with an "enforced" option (mandatory SSO for that domain — passwords and the general providers above stop working for those emails). That's a separate, organization-level feature not covered in detail here — it's configured directly in the UI, not via env vars.

## What's next

- [Teams and roles](/docs/teams) — the invites a new OAuth user needs to be provisioned.
- [Configuration](/docs/configuration) — the rest of the server's environment variables.
