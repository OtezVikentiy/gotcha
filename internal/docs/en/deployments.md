# Deployments

A record of your releases: each deployment your CI reports shows up as a
vertical marker on the project's charts, in a dedicated list, and next to the
regressions it precedes — so "what changed right before this went wrong"
becomes a link instead of a guess.

## What you get

- **Markers on the charts.** Every deployment draws a dashed vertical line at
  its timestamp on the performance, metrics, host, and uptime charts, labelled
  with the version. A latency spike that lines up with a release is visible at
  a glance.
- **A deployments list.** The **"Deployments"** subsection of the Performance
  area lists every reported release newest-first: version, environment, when,
  the changelog, and a link back to the CI run or release page.
- **Regression attribution.** On the regressions list, a regression that
  started within 7 days after a deployment is annotated with "after deploy
  vX" — the most likely change to have introduced it, one click from the
  deployments list.

Nothing is required for the rest of the product to keep working: if no CI
reports deployments, there are simply no markers and the list is empty.

## Pushing a deployment from CI

Report a deployment with a single HTTP request at the end of your deploy job.

```
POST https://<your-gotcha-host>/api/<PROJECT_ID>/deployments/
```

### Authentication

Like the event ingest endpoint, this path is authorized with the project's
DSN key — the **public key** (`<PUBLIC_KEY>`, the segment of the DSN between
`https://` and `@`) plus the **project id** (`<PROJECT_ID>`, the last segment
of the DSN). See [SDK & integrations](/docs/sdk) for where to find your DSN.

Pass the key either as a query parameter:

```
POST https://<your-gotcha-host>/api/<PROJECT_ID>/deployments/?sentry_key=<PUBLIC_KEY>
```

or in the header used by the Sentry protocol:

```
X-Sentry-Auth: Sentry sentry_key=<PUBLIC_KEY>
```

A missing key returns `401`; a key that doesn't belong to `<PROJECT_ID>`
returns `403`.

The request body is capped by the same variable used for the rest of ingest,
`GOTCHA_MAX_EVENT_BYTES` (see [Configuration](/docs/configuration)) — a body
larger than the cap is rejected with `413`. A body that doesn't parse as JSON
returns `400`.

### Request body

A single JSON object describing one deployment:

| Field | Required | Description |
|---|---|---|
| `version` | required | The release identifier (`v1.4.2`, a git SHA, a build number). An empty or missing value returns `400`. |
| `environment` | optional | The environment the release went to (`prod`, `staging`, …). Used to tell releases of different environments apart. |
| `deployed_at` | optional | When the release happened — an RFC3339 string (`"2026-08-18T12:00:00Z"`) or unix time in seconds as a number. Missing, empty, or unparsable — the server's receive time is used instead. |
| `url` | optional | A link back to the CI run or the release page. Rendered as an outbound link in the list (only `http`/`https` become a link; anything else stays plain text). |
| `changelog` | optional | Free-form text of what changed. Shown in the list with line breaks preserved. |

On success the response is `200 OK` with the body `{"id": <n>}`, the id of the
stored record.

### GitHub Actions

A step at the end of the deploy job — the DSN parts live in repository secrets:

```yaml
- name: Notify Gotcha of the deployment
  run: |
    curl -sf -X POST \
      "https://gotcha.example.com/api/${{ secrets.GOTCHA_PROJECT_ID }}/deployments/?sentry_key=${{ secrets.GOTCHA_PUBLIC_KEY }}" \
      -H "Content-Type: application/json" \
      -d "$(jq -n \
        --arg v "${{ github.ref_name }}" \
        --arg url "${{ github.server_url }}/${{ github.repository }}/actions/runs/${{ github.run_id }}" \
        --arg log "${{ github.event.head_commit.message }}" \
        '{version:$v, environment:"prod", url:$url, changelog:$log}')"
```

### GitLab CI

A job in the `.gitlab-ci.yml` deploy stage — `GOTCHA_PROJECT_ID` and
`GOTCHA_PUBLIC_KEY` are CI/CD variables:

```yaml
notify-gotcha:
  stage: deploy
  needs: [deploy]
  script:
    - |
      curl -sf -X POST \
        "https://gotcha.example.com/api/${GOTCHA_PROJECT_ID}/deployments/?sentry_key=${GOTCHA_PUBLIC_KEY}" \
        -H "Content-Type: application/json" \
        -d "{\"version\":\"${CI_COMMIT_TAG:-$CI_COMMIT_SHORT_SHA}\",\"environment\":\"prod\",\"url\":\"${CI_PIPELINE_URL}\",\"changelog\":\"${CI_COMMIT_TITLE}\"}"
```

## Where the markers appear

The version-labelled markers are drawn on every time-series chart of a
project: the [Performance](/docs/performance) endpoint charts, custom
[metric](/docs/metrics) charts, [host](/docs/hosts) resource charts, and
[uptime](/docs/uptime) latency charts. A marker is placed at the deployment's
timestamp along the chart's own time axis; deployments outside the chart's
visible window aren't drawn.

## The deployments screen

`/projects/{id}/deployments` ("Deployments" in the Performance navigation)
lists the project's reported deployments, newest first. Each row shows the
version, environment, when it happened (relative time), the changelog with its
line breaks, and — if a `url` was sent — a link out to the CI run or release
page. An empty project shows an empty state with these instructions.

## Regression attribution

The [regressions](/docs/performance) list ties each detected regression to the
deployment most likely to have caused it: the nearest deployment that happened
**before** the regression started, within a 7-day window. That row gains an
"after deploy vX" note linking to the deployments list. A regression with no
deployment in the window is left unannotated — the attribution is additive and
never changes how regressions themselves are detected.
