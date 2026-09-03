# Hosts

The "Hosts" section shows system metrics for the servers your application runs on: CPU, memory, disk, network, load average, and process count — kept separate from your application's own metrics. It uses the same OTLP ingest as [Metrics](/docs/metrics), tagged with a `host.name` resource attribute, plus its own subsystem of built-in thresholds and incidents.

Open it via the server icon in the left icon rail ("Hosts") or directly at `/projects/{id}/hosts`. The section is only visible when metric ingest is enabled on the instance — the same gate as "Metrics".

## Connecting a host

Both methods below — the agent and the collector — register a host, and both
use a project key of type `agent`: that's the only key type that can register
a host (see [Ingest keys](/docs/keys) for details). The command/config shown
in the UI already has such a key filled in; reissue one in the project
settings if you need to.

### Gotcha agent (recommended)

The simplest way to collect host metrics is the native `gotcha-agent`: a single static, dependency-free binary installed with one command run straight from your instance. A ready-made command with the instance's address and the project's key already filled in is shown right in the UI: as the onboarding step on an empty hosts list, and under "How to install the agent" on a non-empty list and on the threshold settings page, with a "Copy command" button next to it. In shape it looks like this:

```bash
GOTCHA_AGENT_ENDPOINT=https://gotcha.example.com GOTCHA_AGENT_INGEST_KEY=a1b2c3d4e5f6 sh -c "$(curl -fsSL https://gotcha.example.com/install.sh)"
```

Run the command as a regular user with `sudo` rights, or as root. **Don't prefix it with `sudo` yourself** — the script invokes it itself where needed: prefixing the whole line with `sudo sh -c "..."` strips the `GOTCHA_AGENT_*` variables (sudo's default `env_reset`), and the script silently falls into the update path instead of a first install, while `sudo KEY=... sh -c ...` puts the project key into `sudo`'s process arguments, visible to other local users via `ps`.

What the command does:

- downloads the agent binary and verifies its SHA-256 against `SHA256SUMS` — both files are served by the instance itself (`GOTCHA_DIST_DIR`, see [Configuration](/docs/configuration)), so the agent needs no outbound internet access to install — this works in closed networks too; if the instance was built without a Docker image (e.g. a local `make go-build` without the image build), `/install.sh` answers `404` — the Docker build is what puts the agent binaries into the image (`GOTCHA_DIST_DIR`);
- creates a system user `gotcha-agent` (no home directory, no shell) and places the binary at `/usr/local/bin/gotcha-agent`;
- writes the config to `/etc/gotcha-agent/gotcha-agent.env` (mode `0600`, readable only by root) and a systemd unit `gotcha-agent` (`User=gotcha-agent` — the process doesn't run as root);
- enables and starts the service: `systemctl enable --now gotcha-agent`.

**Trust boundary.** Installing the agent means the host trusts your Gotcha instance (and the network path to it) as a source of code that runs as root — the same arrangement Datadog's, Zabbix's, and Netdata's agents rely on. The SHA-256 check above catches a broken download or a proxy cache serving something stale, but it **doesn't protect against a compromised instance**: the checksums travel over the same channel as the binary, so a forged source simply serves forged checksums to match. That's why the delivery channel matters: if the instance's address isn't `https://` (and isn't localhost), the UI won't show the install command at all — only a hint to enable HTTPS or connect the host through the collector instead — otherwise the command, carrying the project's key and running as root, would go out in the clear, and any on-path attacker on that network would get the same root access across your whole fleet. And the usual care that comes with root access applies to who you give administrator rights to the instance itself.

The agent collects the same set of `system.*` metrics as the shipped collector config (see the table below), plus `system.uptime`, over the same OTLP protocol — the page's contract (metric names, the `host.name` promotion) is identical for the agent and the collector, so switching between them requires no changes on the Gotcha side. It exports every 30 seconds by default. If the instance is temporarily unreachable (a restart, a network blip), the agent buffers undelivered batches in memory — at the default interval that's roughly an **hour** of history (120 batches **or 8 MiB, whichever comes first**) — and sends them once connectivity is back, losing nothing to a short outage.

The installed agent's version shows on the host's card; if it's behind the instance's version, the card shows an "Update available" badge. You can also check the installed version on the host itself: `gotcha-agent --version`; the card catches up with the next export — within one collection interval (30 seconds by default).

**Update** — the same command, but **without** the environment variables: a script that's already run on a host remembers the endpoint and key itself (they're in the config it already wrote) and just drops a fresh binary in place of the old one:

```bash
sh -c "$(curl -fsSL https://gotcha.example.com/install.sh)"
```

**Uninstall:**

```bash
sudo systemctl disable --now gotcha-agent
sudo rm /usr/local/bin/gotcha-agent /etc/systemd/system/gotcha-agent.service
sudo rm -r /etc/gotcha-agent
sudo systemctl daemon-reload
sudo userdel gotcha-agent
```

**Customizing the systemd unit.** The unit file is an installer artifact: both the install and every update overwrite it wholesale, so editing `/etc/systemd/system/gotcha-agent.service` directly won't survive the next `sh -c "$(curl ...)"`. For changes that need to stick (a custom `RestartSec`, extra systemd sandboxing, and so on), use a drop-in: `sudo systemctl edit gotcha-agent` creates a separate file under `/etc/systemd/system/gotcha-agent.service.d/`, which the installer never touches.

**The command with the key stays in shell history.** `GOTCHA_AGENT_ENDPOINT`/`GOTCHA_AGENT_INGEST_KEY` at the start of the install command land in `~/.bash_history` (or your shell's equivalent) on the server where you ran it — the same as any command carrying a secret in an environment variable on the same line. If that doesn't fit your threat model, clear the line from history afterward, or run the install from a script or CI secret store instead of interactively.

**Closed networks: a separate endpoint for the agent.** The install command shown in the UI fills `GOTCHA_AGENT_ENDPOINT` with the instance's `GOTCHA_BASE_URL` — the address browsers use to reach it. If hosts running the agent reach the instance by a different path (an internal DNS name/IP, a separate internal domain, a reverse proxy dedicated to telemetry), edit `GOTCHA_AGENT_ENDPOINT` to that address before running the command — the agent itself doesn't need to be reachable from outside, and doesn't need to reach the instance the same way a browser does.

Supported platforms are Linux amd64/arm64 with systemd; for another OS, or without systemd, use the collector below. Besides systemd the host needs `curl`, `useradd` (shadow-utils) and `sha256sum`/`install`/`mktemp` (coreutils) — present on any normal distribution; on a minimal image the installer says what's missing up front, without changing anything.

The `gotcha-agent` unit runs with `MemoryMax=128M`, `Nice=10`, and `CPUWeight=20` — on a shared server the agent doesn't compete for CPU with your main workload and can't grow past that memory ceiling.

**Migrating from the collector to the agent.** If `otelcol-contrib` is already running on the host from the earlier instructions, stop it before installing the agent — otherwise both send metrics under the same `host.name`: points are duplicated, the rate charts (network, disk I/O) are computed over an interleaved series, and the "Silence" threshold stops catching an agent failure for as long as the collector is alive.

```bash
sudo systemctl disable --now otelcol-contrib
```

This doesn't touch metric history: the host name is the same, so the card continues without a gap.

#### If the host doesn't show up

The first data point usually reaches ingest within a minute of the install — open `/projects/{id}/hosts` and refresh. If the host is still missing after a couple of minutes:

```bash
systemctl status gotcha-agent
journalctl -u gotcha-agent -n 50
```

- a `401` in the log means the key in `/etc/gotcha-agent/gotcha-agent.env` is wrong (a revoked key, for instance): fix `GOTCHA_AGENT_INGEST_KEY` and run `sudo systemctl restart gotcha-agent`;
- connection or TLS errors mean the instance isn't reachable from this host at `GOTCHA_AGENT_ENDPOINT` (see "Closed networks" above), or its certificate is self-signed (`GOTCHA_AGENT_CA_CERT`);
- if the unit doesn't start at all, `journalctl -u gotcha-agent` says why: the agent validates its config at startup and names the offending variable;
- the fastest way to check the config itself without touching the live process: `sudo systemd-run --quiet --wait --pipe -p EnvironmentFile=/etc/gotcha-agent/gotcha-agent.env /usr/local/bin/gotcha-agent --check` (the installer runs this same check itself).

#### Agent environment variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `GOTCHA_AGENT_ENDPOINT` | yes | — | The instance's base URL, without a path (same meaning as `endpoint` in the collector config). Must be an absolute `http(s)` URL with no query or fragment; a trailing slash is stripped automatically. |
| `GOTCHA_AGENT_INGEST_KEY` | yes | — | The project's public key — the same one used in the DSN and in the collector config's `Authorization` header. |
| `GOTCHA_AGENT_INTERVAL_SECONDS` | no | `30` | Collection and export interval, in whole seconds. Valid range **10–300**: lower risks self-DoS'ing ingest with your own key, higher causes false "Silence" threshold trips. |
| `GOTCHA_AGENT_HOSTNAME` | no | the server's `os.Hostname()` | Overrides `host.name`, for when the system hostname isn't what you want on the card. |
| `GOTCHA_AGENT_CA_CERT` | no | *(empty)* | Path to a PEM CA file — for instances with a self-signed TLS certificate. The recommended way to trust such an instance. |
| `GOTCHA_AGENT_TLS_INSECURE_SKIP_VERIFY` | no | `false` | Disables TLS certificate verification for the instance entirely (`1`/`0`/`true`/`false`/`yes`/`no`/`on`/`off`, case-insensitive). A last resort — prefer `GOTCHA_AGENT_CA_CERT`; this removes MITM protection on the metric delivery channel. |
| `GOTCHA_AGENT_ENVIRONMENT` | no | *(empty)* | Host environment label (`prod`, `staging`, …). Lands in the `deployment.environment` resource attribute; an empty value isn't emitted. |
| `GOTCHA_AGENT_ROLE` | no | *(empty)* | Host role label (`web`, `db`, …). Lands in the `host.role` resource attribute; an empty value isn't emitted. |

Except for the required ones on first install, these variables are applied once — at the moment the install command runs — and land in `/etc/gotcha-agent/gotcha-agent.env`. To change them later, edit the file and run `sudo systemctl restart gotcha-agent` (re-running the installer without the variables won't pick them up, and it explicitly fails if only optional ones are left in the command — on purpose, so a value never gets silently dropped).

### OpenTelemetry Collector (alternative)

The `otelcol-contrib` collector is a third-party process reaching the same result, with its own set of dependencies rather than the Gotcha binary. It isn't the default path anymore, but it remains a fully supported alternative — for example, if you already run `otelcol-contrib` for other purposes, or on a platform/architecture outside Linux amd64/arm64+systemd that the agent supports.

#### 1. Install the collector

Host metrics are sent by the [OpenTelemetry Collector Contrib](https://github.com/open-telemetry/opentelemetry-collector-releases) distribution — a small standalone process on the server itself, not your application. The official `.deb`/`.rpm` packages install a systemd unit (`otelcol-contrib`) and a default config at `/etc/otelcol-contrib/config.yaml`:

```bash
curl -L -o otelcol-contrib.deb \
  https://github.com/open-telemetry/opentelemetry-collector-releases/releases/latest/download/otelcol-contrib_<version>_linux_amd64.deb
sudo dpkg -i otelcol-contrib.deb
```

(on rpm-based distributions, use the matching `.rpm` with `rpm -i`/`dnf install`).

#### 2. Replace the config

A ready-made YAML with the instance's `BaseURL` and the project's active public key already filled in is shown right in the UI: on the empty state of the hosts list, under "Alternative: OpenTelemetry Collector", and on a non-empty list and the threshold settings page, under "Show collector config", with a copy-to-clipboard button next to it. In shape it's the same config as below:

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
      system:
        metrics:
          system.uptime: {enabled: true}
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

`system.cpu.logical.count` is enabled explicitly: without it there's nothing to divide load average by for the "per core" chart or threshold. `system.uptime` (the `system` scraper) is enabled explicitly for the uptime shown on the host's card — on its own it feeds no chart or threshold. The rest of each scraper's metrics are enabled by whatever default set ships with your `otelcol-contrib` version — only the ones the charts, thresholds, and card need directly are listed explicitly.

The exclusion lists on the `filesystem` scraper are not cosmetic — the disk threshold depends on them. Without them the collector reports **every** mounted filesystem, and the threshold takes the maximum across mount points per host: on a stock Ubuntu box every installed snap is mounted as a `squashfs` image that is 100% full **by design** (the image is sized exactly to its contents), so a ">90%" threshold would open an incident on the evaluator's very first tick while the disk is in fact free, and the usage chart's top 8 would be nothing but `/snap/*` instead of real partitions. The same noise comes from `tmpfs`/`devtmpfs` (that's RAM, not disk), `overlay` (container layers on top of the root you already counted) and the kernel's pseudo-filesystems. If you have something else mounted that you don't want in the charts, add it to the same lists: `fs_types`/`mount_points` with `match_type: strict` (exact match) or `regexp` (a regular expression; it matches a substring, which is why the patterns here are anchored with `^`).

Replace the contents of `/etc/otelcol-contrib/config.yaml` with this YAML.

#### 3. Start it and enable on boot

```bash
sudo systemctl enable --now otelcol-contrib
sudo systemctl status otelcol-contrib
```

#### 4. Verify the host showed up

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
| Uptime (host card) | `system.uptime` | `system` |
| "Silence" threshold | not a metric — the last time ingest accepted an export (see below) | — |

The ready-made config from step 2 already enables everything needed for the full set of charts and thresholds, out of the box. The Gotcha agent has no config of its own — it always sends this same full set.

## Host labels: environment, role, filter, grouping

Hosts in the list can carry an environment and a role label — both come from the telemetry itself; the UI doesn't set or edit them:

- **Environment** — the `deployment.environment` resource attribute (the same one metrics/traces/logs use) or the agent's `GOTCHA_AGENT_ENVIRONMENT` variable (see the table above).
- **Role** — the `host.role` resource attribute or the agent's `GOTCHA_AGENT_ROLE` variable (see the table above).

A host with no label shows up empty in the list's "Environment"/"Role" columns, and under a shared "(no label)" heading in the filter and when grouping.

**Filter.** Above the list there are facets for the environment and role values seen in the project (the same mechanism as the [logs](/docs/logs) facets), plus a separate "New" chip (see below). Facets are always computed over the project's whole host registry, not the already-filtered list — picking one value doesn't remove the others from the facets you could switch to. The "Reset filter" link returns the list to its unfiltered state.

**Grouping.** The "Group by" toggle next to the filter splits the list into sections by environment or by role; hosts with no label form their own "(no label)" section. The order of hosts within each section is the same as in the plain list (problem hosts first, then silent, then ok, all then by name) — grouping only slices the already-computed order into sections, it doesn't re-sort it. With no grouping selected, the list stays a plain flat table.

**"New" badge.** A host younger than 24 hours since it was first seen (`first_seen`) is marked with a "new" badge — in the list and on the host's card. The "New" filter chip selects the same hosts using the same 24-hour window.

The list page shows at most **500** hosts at a time; if a project has more, narrow the selection with the environment/role/"new" filter — otherwise some hosts on the page won't be visible.

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

Incident evaluation (open/escalate/resolve) runs on a background loop every `GOTCHA_HOST_EVAL_INTERVAL_SECONDS` (60 seconds by default, minimum 1 second, see [Configuration](/docs/configuration)); notifications go out through the project's channels via the same shared mechanism as [Alerts](/docs/alerts), and further step-by-step escalation is configured on the [Escalations](/docs/escalations) page.

A disabled threshold is not evaluated at all, so saving the settings also resolves its **already open** incidents right away: otherwise nothing could clear the red badge off the host — host incidents cannot be closed by hand. No notification is sent for such a resolve: the operator disabled the threshold themselves, and there is no news in reporting the consequence of their own action back to them.

## Thresholds: cascade and overrides

The project-wide thresholds above aren't the only level. Each of the 4 kinds (disk/memory/load/silence) can be overridden more narrowly, at the level of a single host or a group of hosts sharing an environment or role label (see [Host labels](#host-labels-environment-role-filter-grouping)). The cascade's priority, from most specific to most general:

**host → role → environment → project → default value**

The resolver works out each threshold kind **independently** of the other three, and within a kind, **enabled and value are resolved separately**: the on/off state and the number can come from different cascade levels — a host can inherit its number from the role level while deciding for itself whether the threshold is on.

At each level (host, role, environment), every kind has three states (tri-state):

- **Inherit** — take the value from the next cascade level;
- **Override** — set your own value (a percentage threshold / a per-core multiplier / minutes of silence);
- **Turn off** — don't evaluate the threshold at this level, regardless of what's set above it; a turned-off threshold still holds the inherited number underneath — so there's nothing stale to show if it's turned back on later.

Role outranks environment: if a host carries both labels and both have a group rule with an explicit value for the same kind, the role's rule wins. A host missing the corresponding label (an empty environment or role) simply skips that cascade level, as if no group rule existed for it.

**Where to set it:**

- **Thresholds for this host** — a block on the host's card (`/projects/{id}/hosts/{name}`), one tri-state per kind, for a project operator. Next to each kind is what's in effect right now and where it came from: "In effect on this host: 85%", "Inherited from role \"db\": 90%", "Inherited from environment \"prod\": 2.0", "Inherited from project settings: 90%", or "Default value: 90%". A member without operator rights sees the same values and sources, but read-only — no form.
- **Thresholds by environment/role** — a block on `/projects/{id}/hosts/settings` (for operators). One rule is one pair (an "Environment"/"Role" scope plus a label). The label is picked from values already seen in the project — the same ones the host-list filter's facets use — freeform entry isn't supported: until some host in the project carries the environment or role you want, there's nothing to attach a rule to. Saving the same (scope, label) pair again edits the existing rule instead of creating a second one.

Changing the scope (environment/role) or the label on save creates a separate rule — the original stays in place; delete it from the table to remove it.

The effective value is the same one the background incident evaluator uses (`GOTCHA_HOST_EVAL_INTERVAL_SECONDS`) and the one shown in the UI with its source. An override that turns a kind off resolves the host's already-open incidents for that kind right away — the same rule, and the same lack of notification, as changing the project-wide settings (see above).

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
- Status-aware host auto-registration isn't part of this version.
- Environment and role labels are read-only and come only from telemetry (see [Host labels](#host-labels-environment-role-filter-grouping) above) — the UI doesn't offer freeform labels of its own, unmoored from a resource attribute.
- Per-process metrics aren't collected — only the `system.processes.count` aggregate broken down by status.

## What's next

- [Metrics](/docs/metrics) — the general OTLP metric ingest this section is built on.
- [Cardinality](/docs/cardinality) — what happens when the distinct-value limit overflows.
- [Alerts](/docs/alerts) — delivery channels for host incident notifications.
- [Configuration](/docs/configuration) — instance environment variables, including `GOTCHA_HOST_EVAL_INTERVAL_SECONDS` and `GOTCHA_METRIC_RETENTION_DAYS`.
