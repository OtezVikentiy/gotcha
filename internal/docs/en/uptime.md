# Uptime

The "Uptime" section watches the availability of external addresses and services through periodic checks — monitors.

## Monitors and statuses
A monitor can be of type HTTP, TCP, DNS, or heartbeat (the application itself reports that it's alive). Each monitor shows its current status, check history, and uptime percentage over a period.

## Incidents
When a check fails several times in a row, an incident opens — it tracks the start time, duration, and recovery, and can notify through "Alerts". Scheduled maintenance windows suppress incidents so they don't create false noise.

## Public status pages and probes
A status page is a public view of the state of selected monitors for users and partners. Probes are geographically distributed points from which checks run, which helps distinguish a local network issue from an actual service outage.
