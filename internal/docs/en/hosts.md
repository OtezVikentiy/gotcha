# Hosts

The "Hosts" section shows system metrics for the servers your application runs on: CPU, memory, disk, network, load average, and process count — kept separate from your application's own metrics. It uses the same OTLP ingest as [Metrics](/docs/metrics), tagged with a `host.name` resource attribute, plus its own subsystem of built-in thresholds and incidents.

Open it via the server icon in the left icon rail ("Hosts") or directly at `/projects/{id}/hosts`. The section is only visible when metric ingest is enabled on the instance — the same gate as "Metrics".

## Connecting a host

### 1. Install the collector

Host metrics are sent by the [OpenTelemetry Collector Contrib](https://github.com/open-telemetry/opentelemetry-collector-releases) distribution — a small standalone process on the server itself, not your application. The official `.deb`/`.rpm` packages install a systemd unit (`otelcol-contrib`) and a default config at `/etc/otelcol-contrib/config.yaml`:

```bash
curl -L -o otelcol-contrib.deb \
  https://github.com/open-telemetry/opentelemetry-collector-releases/releases/latest/download/otelcol-contrib_<version>_linux_amd64.deb
sudo dpkg -i otelcol-contrib.deb
```

(on rpm-based distributions, use the matching `.rpm` with `rpm -i`/`dnf install`).

### 2. Replace the config

A ready-made YAML with the instance's `BaseURL` and the project's active public key already filled in is shown right in the UI — on the empty state of the hosts list and on the threshold settings page ("Show collector config"), with a copy-to-clipboard button. In shape it's the same config as below:

```yaml
receivers:
  hostmetrics:
    collection_interval: 30s
    scrapers:
      cpu:
        metrics:
          system.cpu.utilization: {enabled: true}
          system.cpu.logical.count: {enabled: true}
      memory:
        metrics:
          system.memory.utilization: {enabled: true}
      filesystem:
        exclude_fs_types:
          match_type: strict
          fs_types: [autofs, binfmt_misc, bpf, cgroup, cgroup2, configfs, debugfs,
            devpts, devtmpfs, efivarfs, fusectl, hugetlbfs, iso9660, mqueue, nsfs,
            overlay, proc, pstore, ramfs, securityfs, squashfs, sysfs, tmpfs, tracefs]
        exclude_mount_points:
          match_type: regexp
          mount_points: [^/snap/.*, ^/var/lib/docker/.*, ^/var/lib/kubelet/.*,
            ^/run/.*, ^/dev/.*, ^/proc/.*, ^/sys/.*]
        metrics:
          system.filesystem.utilization: {enabled: true}
      disk: {}
      network: {}
      load: {}
      processes: {}
processors:
  resourcedetection:
    detectors: [env, system]
  batch: {}
exporters:
  otlphttp:
    endpoint: https://gotcha.example.com
    headers:
      Authorization: "Bearer a1b2c3d4e5f6"
service:
  pipelines:
    metrics:
      receivers: [hostmetrics]
      processors: [resourcedetection, batch]
      exporters: [otlphttp]
```

`endpoint` is the instance's **base** URL, without `/v1/metrics` — the `otlphttp` exporter appends the path itself. The key in the `Authorization` header is the same project public key used in the project's DSN (see [SDK & integrations](/docs/sdk)); the config carries the same visibility boundary as the DSN — available to anyone with access to the project.

`system.cpu.logical.count` is enabled explicitly: without it there's nothing to divide load average by for the "per core" chart or threshold. The rest of each scraper's metrics are enabled by whatever default set ships with your `otelcol-contrib` version — only the ones the charts and thresholds need directly are listed explicitly.

The exclusion lists on the `filesystem` scraper are not cosmetic — the disk threshold depends on them. Without them the collector reports **every** mounted filesystem, and the threshold takes the maximum across mount points per host: on a stock Ubuntu box every installed snap is mounted as a `squashfs` image that is 100% full **by design** (the image is sized exactly to its contents), so a ">90%" threshold would open an incident on the evaluator's very first tick while the disk is in fact free, and the usage chart's top 8 would be nothing but `/snap/*` instead of real partitions. The same noise comes from `tmpfs`/`devtmpfs` (that's RAM, not disk), `overlay` (container layers on top of the root you already counted) and the kernel's pseudo-filesystems. If you have something else mounted that you don't want in the charts, add it to the same lists: `fs_types`/`mount_points` with `match_type: strict` (exact match) or `regexp` (a regular expression; it matches a substring, which is why the patterns here are anchored with `^`).

Replace the contents of `/etc/otelcol-contrib/config.yaml` with this YAML.

### 3. Start it and enable on boot

```bash
sudo systemctl enable --now otelcol-contrib
sudo systemctl status otelcol-contrib
```

### 4. Verify the host showed up

With a 30-second collection interval plus network delivery, the first point usually reaches ingest within a minute of the collector starting — open `/projects/{id}/hosts` and refresh. If the host doesn't appear after a couple of minutes, `systemctl status otelcol-contrib` and `journalctl -u otelcol-contrib` will show whether export is happening at all (a wrong key makes ingest respond `401`, which the collector logs).

## Charts and thresholds: which metrics they need

Every chart on a host's card and every built-in threshold needs specific metrics. If a metric isn't arriving (the scraper is disabled in the collector config, or the `otelcol-contrib` version doesn't support it), the corresponding chart shows an empty state naming the scraper to enable, and the threshold simply isn't evaluated — **no data is not the same as an incident**.

| Chart / threshold | Required metrics | Collector scraper |
|---|---|---|
| CPU busy % (chart) | `system.cpu.utilization` (`state` attribute) | `cpu` |
| RAM % (chart), "Memory" threshold | `system.memory.utilization` (`state` attribute) | `memory` |
| Disk: usage (chart), "Disk" threshold | `system.filesystem.utilization` (`mountpoint` attribute) | `filesystem` |
| Disk: I/O (chart) | `system.disk.io` (`device`, `direction` attributes) | `disk` |
| Network (chart) | `system.network.io` (`device`, `direction` attributes) | `network` |
| Load average (chart), "Load" threshold | `system.cpu.load_average.1m`/`.5m`/`.15m` **and** `system.cpu.logical.count` (the "per core" divisor) | `load` + `cpu` |
| Processes (chart) | `system.processes.count` (`status` attribute) | `processes` |
| "Silence" threshold | not a metric — the last time ingest accepted an export (see below) | — |

The ready-made config from step 2 already enables everything needed for the full set of charts and thresholds, out of the box.

## Built-in thresholds and their settings

Thresholds are a fixed built-in set (not created by hand like metric alert rules), tunable on `/projects/{id}/hosts/settings` (available to project operators):

| Threshold | Default condition | Window | What's configurable |
|---|---|---|---|
| Disk | worst mountpoint > **90%** | 5 min | on/off, threshold in % |
| Memory | average used > **90%** | 5 min | on/off, threshold in % |
| Load | average `load average (5m) ÷ cores` > **2.0** | 5 min | on/off, per-core multiplier |
| Silence | no accepted export for longer than **5 minutes** | — | on/off, threshold in minutes, **minimum 3 minutes** |

CPU utilization is deliberately left out of the built-in set: a brief spike to 100% is normal, not an incident, and a noisy default CPU threshold wouldn't earn its keep. When a "Disk" incident opens, its detail field shows the worst mountpoint at the moment it opened.

The minimum for the silence threshold (3 minutes) isn't an arbitrary number — it's the registration throttling interval (60 seconds, see below) tripled with margin, so that a live host with infrequent exports doesn't trip a false incident just because `last_seen` naturally lags a little.

Incident evaluation (open/escalate/resolve) runs on a background loop every `GOTCHA_HOST_EVAL_INTERVAL` (60 seconds by default, minimum 1 second, see [Configuration](/docs/configuration)); notifications go out through the project's channels via the same shared mechanism as [Alerts](/docs/alerts).

A disabled threshold is not evaluated at all, so saving the settings also resolves its **already open** incidents right away: otherwise nothing could clear the red badge off the host — host incidents cannot be closed by hand. No notification is sent for such a resolve: the operator disabled the threshold themselves, and there is no news in reporting the consequence of their own action back to them.

## Notification privacy

Host incident notifications follow the product's general [privacy](/docs/privacy) rule: a recipient outside your perimeter gets no metric values, no `detail` (the worst mount point, for instance) and no free text — just a redacted message with a link.

The machine name does not leak inside the link either: a host card's address looks like `/projects/{id}/hosts/{name}` and a host has no separate id-based route, so a redacted message **links to the project's host list** (`/projects/{id}/hosts`) instead of the card. A recipient inside your perimeter — a channel allowed to see details — still gets the direct link to the card.

## What "last seen" means, and when a host counts as silent

A host's `last_seen` in the list and card means **"ingest accepted an export from this host"**, not "the data was actually written to storage": the ClickHouse write is asynchronous, and tying a host's liveness to it would be dishonest toward the silence threshold.

A "Silence" incident isn't opened every time `last_seen` is older than the threshold — there are two exceptions, both of them guards against false alarms:

- **We have only just come back up.** While the product was down (a restart, an unavailable PostgreSQL, a version upgrade), nobody was refreshing `last_seen`. The evaluator counts silence only from its own start: for the first `silence threshold` minutes after startup, no new "Silence" incidents are opened. Incidents already open stay open — a restart neither "fixes" genuinely silent hosts nor sends false "incident resolved" notifications.
- **The host was observed for less than the threshold.** A machine that showed up for the first time and went quiet faster than the silence threshold (less than the threshold between its `first_seen` and `last_seen`) does not open an incident. That's the normal life of ephemeral instances — pods, autoscaling machines — not a server going silent; otherwise every pod that shut down would turn into an email in every channel of the project.

While a "Silence" incident is open, its row isn't rewritten on every pass: the recorded duration is updated only once the silence has grown noticeably — and the exact "how long has it been quiet" is always visible from the incident's start time.

An important consequence: `last_seen` is updated **even when ingest responds `429`** because the organization's monthly quota is exhausted (see [Metrics → Settings and quotas](/docs/metrics)) — the fact that the collector sent an export still counts, even though the points weren't stored. Otherwise a quota running out would look exactly like "the host went silent", even though the server is fully alive and still sending data — it's just not being accepted right now.

## Deletion and automatic cleanup

The "Delete host" button on a host's card is visible only to a project operator, and the action requires two-step confirmation. Deleting a host cascades to all of its incidents, open and closed. If the host keeps sending metrics, it reappears with a new `first_seen` — the "monitored since…" history starts over.

Hosts that have stopped sending data for good don't need manual cleanup: a background janitor deletes a host once its `last_seen` is older than the metric retention period (`GOTCHA_METRIC_RETENTION_DAYS`, 30 days by default; `0` keeps data forever, and the janitor leaves such hosts alone). Its incidents go with the same cascade.

If such a host still has **open** incidents (for a server gone silent for good that is at least "Silence"), it is retired rather than dropped in silence: those incidents are resolved first, a separate "host retired" notification listing the closed thresholds goes out to the project's channels, and only then is the host itself deleted. That way a dead server's disappearance leaves an event you can see in email or Telegram — and the registry still does not grow without bound. A host with no open incidents has nothing to announce and is deleted quietly.

The retirement notice is its own kind, not the usual "incident resolved": resolution says a threshold came back to normal, whereas here the opposite happened — the machine is gone for good. If the notification could not be sent (the channel is unavailable), the host is not deleted on that pass: the janitor comes back to it on the next hourly one.

Incidents of a live host are cleaned up on the same schedule: a **resolved** incident is deleted once more than `GOTCHA_METRIC_RETENTION_DAYS` has passed since it closed — by then there are no metric points left to look at it through anyway. An open incident is never deleted by retention: it describes what is happening to the host right now. It can only go together with the host — on manual deletion, or on retirement, where it is resolved first (see above).

## Registration: throttling and reappearance

Ingest doesn't write `last_seen` to PostgreSQL on every batch of points — that's throttled: no more than once every 60 seconds per (project, host) pair, tracked in the ingest process's memory, capped at 65,536 entries with the oldest evicted on overflow. A brand-new host name is registered immediately — throttling only limits how often an *already-known* host's `last_seen` gets refreshed.

There's a subtlety when ingest (`--mode=ingest`) and web (`--mode=web`) run as separate processes/replicas — a documented topology for busier instances. Deleting a host on the web replica clears only that replica's throttle map; the ingest replica's own throttle map knows nothing about the deletion and keeps treating the host as "recently touched" until its 60-second window expires. If the host keeps exporting during that window, it won't be recreated in PostgreSQL until the window closes — up to **60 seconds** after deletion. This is accepted behavior rather than a bug: the price of a rare edge case, in exchange for throttling that needs no coordination between replicas.

## Cardinality of host names

Host names (`host.name`) sit under the same cardinality guard as metric names, transaction names, services, and environments: a cap of 10,000 distinct values per project per hour (`GOTCHA_CARDINALITY_LIMIT`), beyond which new names collapse into `<cardinality-limit>` instead of creating their own aggregate rows. In practice this only matters for auto-generated or ephemeral host names (for example, short-lived containers with a random hostname on every restart) — a fleet of ordinary servers stays well under 10,000. See [Cardinality](/docs/cardinality) for the mechanism and how to raise the cap.

## Per-project host ceiling

A project holds **1000 hosts**. A name arriving beyond that number is not registered: already-known hosts keep refreshing their `last_seen` (a fleet that grew into the ceiling doesn't fall into false silence), while new machines simply stop appearing in the section. Dropped names show up in the log and in the `gotcha_host_registrations_rejected_total` counter (see [Self-monitoring](/docs/self-monitoring)) — this never happens quietly.

The ceiling exists because a host is more than a row in a table: each one carries its own thresholds, and each one that dies opens a "Silence" incident with a notification. Without a limit, a fleet with an identifier in the host name (pods, autoscaling) would turn the section into an avalanche of incidents. If you hit the ceiling with a real fleet, drop the variable part from `host.name` or split the machines across projects.

Slots under this ceiling are freed by the janitor alone — by `last_seen` older than the metric retention period (see [Deletion and automatic cleanup](#deletion-and-automatic-cleanup)). Open incidents do not hold a slot: an expired host is retired together with them (see above). With `GOTCHA_METRIC_RETENTION_DAYS=0` the janitor never touches hosts at all, which makes the ceiling permanent: a thousand ephemeral names that arrived once hold the registry forever, and new — this time real — machines will not register until the ephemeral ones are deleted by hand. So if your fleet has ephemeral names, set `GOTCHA_METRIC_RETENTION_DAYS` above zero: unlimited retention and the host ceiling only coexist on stable names.

The names `.` and `..` are never registered: a host card's address is `/projects/{id}/hosts/{name}`, and such names would lead nowhere.

## Limitations

- A host literally named `settings` isn't reachable through its card: `/projects/{id}/hosts/settings` is the threshold settings page, and that literal route segment wins over the `{name}` wildcard. The same accepted limitation already exists for a metric literally named `alerts` under "Metrics".
- There's no per-host threshold override — the settings on `/hosts/settings` apply to every host in the project at once. Per-host thresholds, tags, and status-aware auto-registration aren't part of this version.
- Per-process metrics aren't collected — only the `system.processes.count` aggregate broken down by status.

## A dedicated agent — coming in a future version

Right now, the only supported way to collect host metrics is the third-party `otelcol-contrib`. A future version will add a lightweight Gotcha-native agent as a simpler alternative to install — this page's contract (metric names, the `host.name` promotion) is already designed to stay compatible with it: the agent will send the same metric names over the same protocol.

## What's next

- [Metrics](/docs/metrics) — the general OTLP metric ingest this section is built on.
- [Cardinality](/docs/cardinality) — what happens when the distinct-value limit overflows.
- [Alerts](/docs/alerts) — delivery channels for host incident notifications.
- [Configuration](/docs/configuration) — instance environment variables, including `GOTCHA_HOST_EVAL_INTERVAL` and `GOTCHA_METRIC_RETENTION_DAYS`.
