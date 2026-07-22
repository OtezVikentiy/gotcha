[English](CONTRIBUTING.md) · [Русский](CONTRIBUTING.ru.md)

# Contributing to Gotcha

Thanks for your interest in improving Gotcha. This document covers dev
environment setup, testing, and the project's coding conventions.

## Requirements

- Go 1.26+
- Docker + Docker Compose (for PostgreSQL, ClickHouse, and integration tests)

## Dev setup

```bash
# gitflic (main, guaranteed anonymous HTTPS)
git clone https://gitflic.ru/project/otezvikentiy/gotcha.git
# GitHub (mirror, if published)
git clone https://github.com/OtezVikentiy/gotcha.git
# Contributors with SSH access can use:
# git clone git@gitflic.ru:otezvikentiy/gotcha.git
cd gotcha
docker compose up -d
```

This builds and runs the app plus PostgreSQL and ClickHouse. See the
[README's Quick start](README.md#quick-start) for first-run steps (creating
the bootstrap admin user, connecting an SDK).

Useful `make` targets (run `make help` for the full, current list):

| Target | Does |
|---|---|
| `make up` / `make down` | Start/stop the compose stack. |
| `make up-rebuild` | Rebuild the image and force-recreate containers. |
| `make logs` / `make logs-all` | Follow app logs / all container logs. |
| `make health` | Curl `/healthz` on the running app. |
| `make app-connect` | Shell into the running app container. |
| `make db-connect` | `psql` into PostgreSQL. |
| `make ch-connect` | `clickhouse-client` into ClickHouse. |
| `make run` | `go run ./cmd/gotcha` (needs `make up` running for PG/CH — see the README's "Build from source" section, since the compose file doesn't publish PG/CH ports to the host by default). |
| `make go-build` | Build the `gotcha` binary. |
| `make templ` | Regenerate `*_templ.go` from `.templ` sources. |
| `make fmt` | `gofmt` all Go sources. |
| `make vet` | `go vet ./...`. |
| `make tidy` | `go mod tidy`. |
| `make check` | `fmt` + `vet` + `test-short` — run before committing. |

## Running tests

**Run tests gently**: this repo's test suite uses `testcontainers-go` to spin
up real PostgreSQL/ClickHouse containers for integration tests. Running the
full suite with unrestricted parallelism can spawn many containers at once
and exhaust the machine's memory/CPU (or the Docker daemon). Always run:

```bash
nice -n 19 go test -p 2 ./...
```

— `nice -n 19` deprioritizes the run relative to other work, and `-p 2` caps
how many test binaries (i.e. how many packages, each potentially bringing up
its own containers) run in parallel. Do not run a bare `go test ./...` or
`go test -p <N>` with a large `N` in this repo.

For fast, container-free iteration, unit tests only:

```bash
go test ./... -short -count=1
```

(this is what `make test-short` and `make check` run). The Makefile's plain
`make test` target runs the full suite via `go test ./... -count=1 -timeout
1800s` without the `nice -p 2` guard — prefer the `nice -n 19 go test -p 2
./...` form above when running the full suite yourself.

## Working with templ templates

The web UI is server-rendered with [templ](https://templ.guide). `.templ`
files compile to `*_templ.go`. **Never hand-edit a `*_templ.go` file** — it
is generated and will be overwritten. After changing a `.templ` file, run:

```bash
make templ
```

and commit both the `.templ` source and the regenerated `*_templ.go` output.

## Content Security Policy / no inline JS or CSS

Gotcha serves a strict CSP (`default-src 'self'; base-uri 'none';
form-action 'self'; frame-ancestors 'none'`) with no `unsafe-inline` and no
inline `<script>`/`<style>` or `style="..."` attributes. All interactivity
must be achieved with native HTML/CSS mechanisms — primarily `<details>`/
`<summary>` for disclosure widgets and CSS `:target` for state that needs to
survive without JavaScript — plus htmx for anything that genuinely needs a
server round-trip. Do not introduce inline event handlers, inline styles, or
new script sources; if a change seems to require one, look for a CSS-only or
htmx-based way to achieve it first.

## i18n (RU/EN catalog parity)

UI strings are looked up from JSON catalogs under `internal/i18n/locales/`
for `ru` and `en`. Every key added to one catalog must be added to the other
— the catalogs are expected to stay in parity (a message present only in one
locale will silently fall back to the raw key for the other). When adding or
changing user-facing strings, update both `ru.json` and `en.json` in the same
change.

## Commit / PR conventions

- Keep commits focused; write commit messages that explain *why*, not just
  *what*.
- Prefer conventional prefixes where they fit (`feat:`, `fix:`, `docs:`,
  `refactor:`, `test:`) — see `git log` for existing style.
- Run `make check` (or at minimum `gofmt`, `go vet ./...`, and the tests
  relevant to your change) before opening a PR.
- If your change touches `.templ` files, make sure the regenerated
  `*_templ.go` files are included in the same commit.
- If your change adds or changes user-facing text, update both i18n
  catalogs (see above).
- Describe the "why" in the PR description; link any relevant issue.

## Reporting security issues

Do not open a public issue for a security vulnerability — see
[SECURITY.md](SECURITY.md).
