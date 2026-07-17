# Profiling

A **profile** is a snapshot of where CPU time or memory (alloc/heap) goes inside your application, captured from real call stacks during actual requests — not a synthetic benchmark, but what really happened in production. Profiles are visualized as a **flame graph**: the wider a function's block, the more time/memory it accounted for relative to the whole profile.

## How to send profiles

Gotcha accepts profiles through two different paths.

### 1. Through the regular SDK (alongside tracing)

If your Sentry SDK supports profiling, the profile is sent as part of the regular envelope next to the transaction — nothing extra to wire up besides the same tracing setup used for [Performance](/docs/performance). Whether profiling is available and how to enable it depends on the specific SDK — see [SDK and integrations](/docs/sdk) and your language's SDK docs.

### 2. Through the direct pprof endpoint

For Go applications (e.g. via the built-in `net/http/pprof`) or any continuous-profiling tool that can emit the standard Go pprof format, there's a dedicated ingestion endpoint:

```
POST /profiles/pprof?service=<service_name>&type=<sample_type>&environment=<environment>&transaction=<transaction>
Authorization: Bearer <DSN public key>
```

- **Authentication** — an `Authorization: Bearer <key>` header, where `<key>` is the public part of the project's DSN (the same string that sits before the `@` in a DSN like `https://<public_key>@<host>/<project_id>`).
- **Request body** — a pprof profile (protobuf), **which the client must gzip itself**. That's a convention of the pprof format itself (compression inside the body, not an HTTP `Content-Encoding`); the server decompresses it with a size limit.
- **`service`** — an arbitrary service/application name the profile is grouped under in the list.
- **`type`** — the name of the pprof sample type to record (e.g. `cpu`/`samples` for a CPU profile, `alloc_space`/`inuse_space` for a memory profile). If omitted or not found in the profile, the profile's last sample type is used.
- **`transaction`**, **`environment`**, **`trace_id`** — optional metadata: the transaction and environment the profile was captured for, and a `trace_id` if the profile should be linked to a specific trace (see below).

Example — capture a 10-second CPU profile with Go's standard tooling and send it to Gotcha:

```bash
curl -s "http://localhost:6060/debug/pprof/profile?seconds=10" -o cpu.pprof
gzip -c cpu.pprof | curl -X POST \
  "https://<gotcha_host>/profiles/pprof?service=my-service&type=cpu&environment=production" \
  -H "Authorization: Bearer <DSN_public_key>" \
  --data-binary @-
```

A successful submission returns `202 Accepted`. If profiling is disabled on the instance, the write is silently skipped but the response is still `202`. If the organization's profile quota is exhausted, ingestion returns `429`.

## Profile list

The section opens from the **"Profiles"** link in the "Performance" subsection menu — `/projects/<id>/profiles`. Period tabs are 1 hour / 24 hours / 7 days. The table groups profiles by (service, type, transaction) and shows the sample count for each group; clicking a row opens that group's flame graph for the selected period. A link to **"Profile regressions"** (see below) sits at the top.

## Flame graph

Gotcha's flame graph is drawn as an **icicle** diagram (top to bottom): the root ("all") is the top bar spanning the full width, its immediate callees sit below it, and so on deeper with each stack level. How to read it:

- **Block width** — the function's share of time/samples relative to the profile's total sample count (the root is 100%).
- **Depth (row number from the top)** — call depth: the lower a block sits, the deeper it is in the call stack.
- **Hovering** over a block shows a tooltip with the function name and the exact percentage.
- Block color is deterministic based on the function name (for visually telling adjacent calls apart) — it doesn't encode "good/bad" and doesn't distinguish your code from library code.
- The flame graph is static: there's no click-to-zoom into depth — the whole picture is in front of you at once, and the widest branch at any level is the heaviest execution path, which is where to start digging.

## Relation to transactions (profiling in trace context)

If a profile was captured with a `trace_id` tied to a specific trace (either a pprof profile with an explicit `trace_id` parameter, or an SDK profile sent alongside a transaction), the waterfall page for that trace (`/traces/<trace_id>`, see [Performance](/docs/performance)) shows a **"View flame graph"** link — going straight from "which span was slow" to "what was actually executing inside it, function by function."

## Profile regressions

A profile regression is a significant increase in a specific function's **self share** (the share of samples where that exact function sits at the top of the stack — its own time, not counting what it calls) relative to a baseline. It uses the same "threshold + hysteresis" principle as [performance regressions](/docs/performance), but is computed separately, from profile data:

- A background evaluator checks the top functions by self share (20 per service/type by default) over a recent window (60 minutes by default) for every (service, profile type) that has profiles in that window.
- The **baseline** is the median of the function's daily self shares over the last 7 days (by default).
- **Opening**: the recent-window share grew by more than 50% over the baseline (`recent > base × 1.5`) **and** stayed above a 5% noise floor — functions under a 5% share aren't considered at all, it's too noisy.
- **Closing** (hysteresis): the share dropped back to baseline + 20% or below (`recent ≤ base × 1.2`) — a looser threshold than opening, so the regression doesn't flicker at the boundary.
- A decision is only made with enough samples in the window (100 by default); otherwise the evaluator does nothing that tick.

The list of open/resolved regressions is in the **"Profile regressions"** section (`/projects/<id>/profile-regressions`), with "Open / Resolved / All" tabs. Table columns: function, service · type, percentage increase, "baseline → peak" share range, status, when it started, duration.
