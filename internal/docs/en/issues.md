# Issues

An **issue** is not a single error — it's a group of identical errors collapsed by fingerprint. Instead of a thousand rows of the same exception, the "Issues" section shows you one card with a "how many times this happened" counter and a chart of when.

## How errors get into Gotcha

Issues are created automatically from events sent by your application's SDK — Gotcha accepts them over the Sentry ingestion protocol (envelope/store), so any official Sentry SDK for your language works out of the box. Installing the SDK, finding your project's DSN, and minimal initialization for Go/PHP/JavaScript/Python are covered in [SDK and integrations](/docs/sdk). There's no manual step to "create" an issue in the UI: as soon as the SDK sends an unhandled exception (or you explicitly call `captureException`), the first issue shows up within a few seconds.

## List and filters

The section opens from the **"Issues"** entry in the left rail (bug icon) — a URL like `/projects/<id>/issues`. At the top is a filter bar (a plain form, works without JS):

| Filter | Values |
|---|---|
| Status | all / unresolved / resolved / ignored |
| Level | all / debug / info / warning / error / fatal |
| Search | over title or culprit (code location) |
| Sort | by last seen / by first seen / by frequency |
| Environment | environments actually seen in the project |
| Period | all time / 24h / 7 days / 30 days |

The table shows, for each issue: level (badge), title and culprit, a 24-hour trend sparkline, event count ("Events"), when it was last seen, status, and the assignee. The list is paginated (25 issues per page), with "Prev / N of M / Next" navigation at the bottom.

Checking issues via the checkboxes on the left lets you apply a bulk action — **"Resolve"**, **"Ignore"**, or **"Unresolve"** — to several rows at once.

## How grouping works

Every incoming event gets a **fingerprint** — a key that either places it into an existing issue or creates a new one. Computation priority (highest first):

1. **Custom fingerprint from the SDK** — if the application explicitly set a `fingerprint` on the event (including the special value `{{ default }}`, which substitutes the automatically computed part).
2. **Normalized stack trace** — if there's an exception with a stack trace, the key is built from the frames' modules/functions.
3. **Exception type + normalized message** — if there's no stack trace but there is an exception (type + message with variable parts like numbers and IDs stripped out).
4. **Normalized message** — a plain message with no exception (e.g. `captureMessage`).

An event whose fingerprint has been seen before joins the existing issue: the "Count" goes up and "Last seen" is updated. A fingerprint that hasn't been seen before creates a new issue with that event as the first one.

Because messages and stack traces are normalized, errors that are essentially the same but differ by numbers/IDs in the text (`user 42 not found`, `user 43 not found`) usually group into one issue, while exceptions of a different type or code location end up in different ones.

## Issue detail

Clicking a row's title opens `/issues/<id>`:

- Title, culprit, a level badge, and a status badge.
- Metadata: **First seen** / **Last seen** / **Times seen**.
- Action buttons (see below) and an assignee form.
- **Frequency chart** — a bar chart of events over the last 7 days in 3-hour steps (56 bars).
- **Recent events** — a table of the 20 most recent events for this issue: when, message, environment, release. Clicking the timestamp opens that event's detail (`?event=<id>` in the URL, the row is highlighted).

### Reading the frequency chart

Each bar is the number of events in a 3-hour window. A flat, low background with occasional single bars usually means the error is one-off or very rare. A single sharp spike points to a short-lived incident (an outage in a third-party service, or one bad deploy that got rolled back). A steadily climbing "staircase" of bars is a sign of an ongoing degradation that won't fix itself — worth acting on right away, not just waiting for an alert.

### Event detail

With an event selected, the page shows a block with:

- **Stack trace** — frames from your application's own code (in-app) are shown in full right away: file path with line number, function, module. Frames from frameworks/runtime/dependencies (not in-app) are collapsed into a `<details>` element — by default only the `function (file:line)` summary line is visible, and the full frame expands on click. This separates your code from library noise right in the stack trace.
- **Trace link** — if the event has an associated `trace_id` (the request was part of a traced transaction), a "View trace" link to the waterfall in [Performance](/docs/performance) is shown.
- **Tags** — arbitrary key-value pairs the SDK attached to the event.
- **User and SDK** — the user's id/email/IP (if the SDK sent them) and SDK information.
- **Contexts** — raw structured data from the event (runtime environment, device, etc. — whatever the SDK sent).

## Actions

An issue can be **resolved** (marked fixed), **ignored** (hidden without marking it fixed — useful for known noise), or **unresolved** again. A separate form assigns the issue to any project member; assigning doesn't change the status, it's just "who's on it."

## Alerting on new issues

Instead of checking the list by hand, set up a "new issue" alert rule in [Alerts](/docs/alerts) — the team gets notified on the channel of your choice (email/webhook/Telegram) as soon as an issue that didn't exist before shows up.
