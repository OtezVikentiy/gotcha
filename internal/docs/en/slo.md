# SLOs & error budgets

A **Service Level Objective (SLO)** turns "the service should be reliable" into a
number you can hold yourself to: *99% of requests succeed over the last 30 days*.
Gotcha measures that number continuously from the telemetry you already collect,
tells you how much of your **error budget** is left, and raises an alert the
moment you start burning that budget fast enough to run out.

This page is for both audiences: managers who want a single honest health number
per service, and SREs who need to know exactly when to page.

## The three ideas

- **SLI — Service Level Indicator.** The measured ratio of good outcomes to total
  outcomes: successful requests over all requests, fast requests over all
  requests, successful checks over all checks. Always a fraction between 0 and 1.
- **SLO — the target.** The line the SLI must stay above, e.g. `0.99`. You also
  pick the **window** it's measured over (1–90 days, a rolling window).
- **Error budget.** The slack the target allows: `1 − target`. A 99% SLO over 30
  days permits 1% of outcomes to be bad — that 1% is your budget. Spend it slowly
  and you never notice; spend it in an afternoon and something is on fire.

**Attainment** is the SLI over the whole window (e.g. 99.4%). **Budget remaining**
is how much of that 1% you have left (100% = untouched, 0% = spent exactly, below
0% = you've overshot the target). Both are shown on every SLO's detail screen.

## The three SLI types

You choose the SLI type when you create an SLO. Each answers "what counts as
good?" differently and reads from a different signal.

| Type | Good / total | Configure with |
|---|---|---|
| **Availability** | successful requests ÷ all requests to a transaction | transaction filter, environment filter |
| **Latency** | requests faster than *T* ms ÷ all requests | transaction filter, environment filter, threshold **T** (ms) |
| **Uptime** | successful monitor checks ÷ all checks | the uptime monitor to attach |

- **Availability** and **latency** are *request-based*: they read the same
  transaction data as the [Performance](/docs/performance) screen. Leave the
  transaction filter empty to cover every transaction; leave environment empty to
  cover every environment.
- **Uptime** is *probe-based*: it reads the pass/fail history of one
  [uptime monitor](/docs/uptime). Use it when an active check ("is the endpoint
  answering?") is the right definition of healthy.

## Burn rate and the two-window alert

**Burn rate** is how fast you're spending the budget *right now*, relative to
spending it evenly across the whole window. Burn rate 1 means you'll use the
budget up exactly at the end of the window; burn rate 14.4 means you'd exhaust a
30-day budget in about two days.

A single fast-burn threshold is a dilemma: check it over a short window and a
brief blip pages you; check it over a long window and you don't hear about a real
outage for hours. Gotcha resolves this the standard way — a **two-window** test:

- An incident **opens** only when **both** a long window (default 60 min) **and**
  a short window (default 5 min) are burning at or above the threshold (default
  14.4). The long window confirms the burn is sustained; the short window
  confirms it's still happening now. Requiring both filters out momentary spikes.
- The incident **closes** when the short window cools back below the threshold —
  the acute burn is over.

When an incident opens or closes, Gotcha notifies through the same
[alert channels](/docs/alerts) as your other alerts. The SLO list shows each
objective's current attainment and remaining budget at a glance; the detail
screen adds a budget-burn chart and the incident history.

## Burn rate assumes steady traffic

Burn rate is calibrated for a service under **stable, continuous load** — the case
where "how fast am I spending the budget right now" is a meaningful question. On
**sparse or intermittent** traffic the signal gets weaker: the short window may
contain only a handful of requests (or none), so a single failure swings the rate
sharply, and when the stream falls quiet the last non-empty bucket keeps standing
in as "now" until fresh traffic arrives. The result is a burn-rate reading that can
lag reality — it self-corrects within the burn window (about an hour for the
default long window) once traffic resumes, but until then it may under- or
over-state how fast you're really spending.

For a service that legitimately receives only occasional requests, prefer an
**uptime SLO** on a monitor that probes it on a fixed schedule: a steady stream of
checks gives the burn-rate math the continuous signal it needs, regardless of how
rarely real users call the service.

## Two things that don't burn budget

**Maintenance windows are excluded.** Any period covered by a project
[maintenance window](/docs/maintenance) is removed from every SLO's calculation —
availability, latency, and uptime alike. Planned work doesn't cost you budget.

**A total outage of a request-based SLI is a blind spot — read this.** Availability
and latency are ratios of *requests*. If an outage is severe enough that traffic
drops to **zero** — nothing reaches the service at all — there are no requests to
score, and "no requests" is indistinguishable from "no bad requests". The budget
simply stops moving; it does not burn. This is inherent to request-based SLIs, not
a Gotcha limitation.

For any service where a full, traffic-killing outage is exactly the failure you
care about most, pair the request-based SLO with an **uptime SLO** on a monitor
that actively probes the service. The monitor keeps checking whether the endpoint
answers, so a total outage produces failed checks and burns the uptime budget even
when the request stream has gone silent.

## Window and retention

The SLO window can be set up to 90 days, but it can only see as far back as your
data actually goes. Each evaluation clips the window to your telemetry retention
(`GOTCHA_EVENT_RETENTION_DAYS` for request-based SLIs, the check-result retention for
uptime): a 90-day window on an instance that keeps 30 days of data is evaluated
over 30 days. Keep the SLO window at or below your retention if you want the full
window to count.

## Evaluation cadence

The evaluator re-scores every enabled SLO on a fixed interval —
`GOTCHA_SLO_EVAL_INTERVAL_SECONDS`, 120 seconds by default. Lowering it makes burn alerts
react faster at the cost of more frequent queries; raising it does the opposite.

Important: like the metric/host/regression evaluators, the SLO evaluator by
default only runs in `uptime` and `all` modes — in a `web`+`ingest` split, an
SLO looks configured but budget burn is never computed. Enable the
evaluators explicitly with `GOTCHA_EVALUATORS_ENABLED=true`, see
[Configuration](/docs/configuration).
