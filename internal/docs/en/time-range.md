# Time range

Every page with a chart — Performance and the endpoint page, Web Vitals, Metrics, Profiles, a monitor's latency chart, and an issue's frequency chart — shares one control for the time window the chart and its numbers cover. It sits in the filter row, usually labeled **"Period"**. (The issues *list* has a simpler period filter — all-time / 24h / 7d / 30d, no custom range; see [Issues](/docs/issues).)

## Presets and a custom range

The control offers four quick presets — **1h / 24h / 7d / 30d** — and a **custom range** for anything else. A preset is relative to "now"; a custom range is two fixed points in time. Applying a window re-scopes the whole page to it and keeps the page's other filters (environment, sorting, and so on).

## Picking a custom range

**With JavaScript**, the control is a single button showing the current window. Clicking it opens a popup: the quick presets on the left and a **two-month calendar** on the right. Click the first day to set the start, click a second day to set the end — the selected range highlights as a band across both months, and **"Apply"** loads it. Days in the future are disabled, and a custom range wider than the data-retention window is trimmed to it (asking for a period older than retention returns nothing anyway).

**Without JavaScript** (or if the script is blocked), the control falls back to the preset dropdown plus a **"Set dates"** disclosure with two native date fields — filling both and applying selects the custom range just the same. Everything works without JavaScript; the popup calendar is a convenience layered on top.

## Charts cover the whole selected window

A chart's axis spans the **window you selected**, not just the range where data happens to exist. A 30-day window draws all 30 days, with empty gaps where there was no data, so a burst of activity at the very end of the window isn't stretched to fill the chart. Time labels thin out automatically, so a long window stays readable instead of overlapping into a solid strip.

If there is no data at all in the window, the chart shows "no data for the period" rather than an empty frame.
