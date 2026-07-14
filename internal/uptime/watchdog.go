package uptime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// heartbeatMissedError — Result.Error/State.LastError recorded for a
// heartbeat monitor whose grace period expired without a ping. Not a real
// check error (heartbeat monitors are never actively probed) — it exists so
// Detector.OnResult's cause/notification text reads sensibly, same as any
// other checker's Result.Error.
const heartbeatMissedError = "no heartbeat within grace period"

// ReminderItem pairs an open incident with its monitor — the unit
// Service.IncidentsDueForReminder hands to the reminder watchdog, which
// needs both (the incident to notify/touch, the monitor for
// Event.Monitor/remind_every_minutes already having been applied by the
// query).
type ReminderItem struct {
	Incident Incident
	Monitor  Monitor
}

// Watchdog runs the three periodic jobs that active checks alone don't
// cover: heartbeat monitors don't get probed by Runner (a missed ping is a
// silence, not a failed request), SSL certificates need re-checking even
// between probes of the same monitor, and open incidents need periodic
// reminder notifications for as long as they stay open. Zero-value
// Interval/SSLEvery mean "use the default" (1 minute / 24 hours), matching
// Runner's ScheduleEvery/LeaseEvery convention.
type Watchdog struct {
	Svc      *Service
	Detector *Detector // heartbeat misses run through OnResult so incident thresholds/notifications behave like any other check
	Notifier Notifier  // used directly for ssl_expiring/reminder events, which don't go through Detector

	// Region — the local region a missed heartbeat is recorded under (must
	// match the region the monitor's active-check regions use, "local" by
	// default — see cmd/gotcha's cfg.LocalRegion). Empty means DefaultRegion.
	Region string

	Interval time.Duration // heartbeat + reminder tick period, default 1 minute
	SSLEvery time.Duration // SSL check tick period, default 24 hours
}

func (w *Watchdog) region() string {
	if w.Region == "" {
		return DefaultRegion
	}
	return w.Region
}

// Run ticks the heartbeat/reminder job every Interval and the SSL job every
// SSLEvery, until ctx is done. Meant to be started with "go w.Run(ctx)";
// there is no separate Close — unlike Runner/ResultWriter, Watchdog holds no
// buffered state that needs draining, so depending on ctx alone (as
// cmd/gotcha's drain() already assumes — see its comment on the ordering) is
// enough.
func (w *Watchdog) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	sslEvery := w.SSLEvery
	if sslEvery <= 0 {
		sslEvery = 24 * time.Hour
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()
	sslTick := time.NewTicker(sslEvery)
	defer sslTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.checkHeartbeats(ctx)
			w.checkReminders(ctx)
		case <-sslTick.C:
			w.checkSSL(ctx)
		}
	}
}

// checkHeartbeats applies a synthetic failed result for every heartbeat
// monitor whose grace period lapsed without a ping, then runs it through
// Detector.OnResult exactly like Runner does for an active check's result —
// so fail_threshold/consensus/incident-opening/notification all behave the
// same way for a silence as for an explicit failure. A successful ping is
// handled entirely by the public heartbeat endpoint (internal/web/heartbeat.go);
// this job only ever sees the failure side.
func (w *Watchdog) checkHeartbeats(ctx context.Context) {
	monitors, err := w.Svc.StaleHeartbeats(ctx)
	if err != nil {
		slog.Error("uptime: watchdog: stale heartbeats failed", "error", err)
		return
	}
	region := w.region()
	at := time.Now().UTC()
	for _, m := range monitors {
		st, err := w.Svc.ApplyResult(ctx, m.ID, region, false, heartbeatMissedError, at)
		if err != nil {
			slog.Error("uptime: watchdog: apply heartbeat miss failed", "monitor_id", m.ID, "error", err)
			continue
		}
		if w.Detector != nil {
			w.Detector.OnResult(ctx, m, region, Result{OK: false, Error: heartbeatMissedError}, st)
		}
	}
}

// sslThresholds returns the sorted-descending, deduplicated, positive-only
// alert thresholds for a monitor: its own ssl_alert_days plus the built-in
// {7,3,1} — see task brief.
func sslThresholds(alertDays int) []int {
	set := map[int]bool{7: true, 3: true, 1: true}
	if alertDays > 0 {
		set[alertDays] = true
	}
	out := make([]int, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out
}

// daysLeftUntil rounds up the whole+partial days between now and expires —
// "5 days and 1 hour left" and "5 days and 23 hours left" both count as 5
// days left having NOT yet elapsed, i.e. still within a 5-day threshold; a
// cert that expires 30 minutes from now already counts as "1 day left" for
// alerting purposes, not "0".
func daysLeftUntil(expires, now time.Time) int {
	return int(math.Ceil(expires.Sub(now).Hours() / 24))
}

// checkSSL notifies once per monitor per tick for the certificates that just
// crossed into a new, not-yet-alerted threshold. When daysLeft already
// satisfies more than one un-alerted threshold at once (e.g. a monitor whose
// SSL was only just observed close to expiry, jumping straight past a
// larger threshold to a smaller one) a single Notify call covers all of
// them, and all of them are recorded — not just the largest — so a later
// tick at the same (or a larger) daysLeft doesn't re-fire for the smaller
// one still sitting un-alerted. See task brief's example: ssl_alert_days=14,
// daysLeft=5 crosses both the 14 and the built-in 7 threshold in the same
// tick.
//
// Claim-before-notify: several `gotcha --mode=uptime` processes (one per
// region) run against the SAME database, each with its own Watchdog ticking
// independently. Svc.ClaimSSLAlert is a single atomic UPDATE ... RETURNING —
// of two replicas racing this method for the same monitor in the same
// window, only one gets won=true — so Notify only ever runs for the winner,
// never twice for the same threshold. This trades away the previous
// notify-then-mark ordering's retry-on-failure behavior: if Notify fails
// after a successful claim, the claim already landed, so the next tick will
// NOT retry it (see the Notify error handling below). That's the deliberate
// price of not double-sending across replicas.
func (w *Watchdog) checkSSL(ctx context.Context) {
	if w.Notifier == nil {
		// "Incidents only, no notifications" deployment mode: skip entirely,
		// including the claim. Claiming without notifying would permanently
		// mark the threshold as alerted, so a Notifier configured later
		// would never fire for it.
		return
	}
	monitors, err := w.Svc.SSLCandidates(ctx)
	if err != nil {
		slog.Error("uptime: watchdog: ssl candidates failed", "error", err)
		return
	}
	now := time.Now().UTC()
	for _, m := range monitors {
		if m.SSLExpiresAt == nil {
			continue
		}
		daysLeft := daysLeftUntil(*m.SSLExpiresAt, now)
		alerted := make(map[int]bool, len(m.SSLAlertedDays))
		for _, d := range m.SSLAlertedDays {
			alerted[d] = true
		}

		var due []int
		for _, t := range sslThresholds(m.SSLAlertDays) { // sorted desc
			if daysLeft <= t && !alerted[t] {
				due = append(due, t)
			}
		}
		if len(due) == 0 {
			continue
		}

		won, err := w.Svc.ClaimSSLAlert(ctx, m.ID, due)
		if err != nil {
			slog.Error("uptime: watchdog: claim ssl alert failed", "monitor_id", m.ID, "days", due, "error", err)
			continue
		}
		if !won {
			// Another replica's watchdog already claimed these thresholds
			// this tick (or an earlier one) — do not notify.
			continue
		}
		if err := w.Notifier.Notify(ctx, Event{Kind: "ssl_expiring", Monitor: m, DaysLeft: daysLeft}); err != nil {
			// See the claim-before-notify trade-off in the doc comment
			// above: this is NOT retried on the next tick.
			slog.Warn("uptime: watchdog: ssl notify failed after claim", "monitor_id", m.ID, "days", due, "error", err)
		}
	}
}

// checkReminders notifies once for every open, non-maintenance incident
// whose monitor wants reminders and is due for one, then claims the
// reminder (last_reminded_at = now()) so the next one waits a full
// remind_every_minutes again.
//
// Claim-before-notify, same rationale as checkSSL: Svc.ClaimReminder is a
// single atomic UPDATE ... RETURNING keyed on "hasn't been reminded (or
// opened) more recently than notBefore", so of several `--mode=uptime`
// replicas racing this method for the same incident, only one gets
// won=true. notBefore is computed here (now - remind_every_minutes) rather
// than inside ClaimReminder, so its notion of "due" stays driven by the same
// now() this tick already used to compute duration/pick items — passing the
// interval instead of a raw cutoff would just move that subtraction inside
// Service for no benefit, since the caller always has "now" in hand anyway.
// As with checkSSL, a Notify failure after a successful claim is NOT
// retried next tick — the price of not double-sending across replicas.
func (w *Watchdog) checkReminders(ctx context.Context) {
	if w.Notifier == nil {
		// "Incidents only, no notifications" mode — skip entirely, including
		// the claim (see checkSSL's identical guard for why).
		return
	}
	items, err := w.Svc.IncidentsDueForReminder(ctx)
	if err != nil {
		slog.Error("uptime: watchdog: incidents due for reminder failed", "error", err)
		return
	}
	now := time.Now().UTC()
	for _, it := range items {
		notBefore := now.Add(-time.Duration(it.Monitor.RemindEveryMinutes) * time.Minute)
		won, err := w.Svc.ClaimReminder(ctx, it.Incident.ID, notBefore)
		if err != nil {
			slog.Error("uptime: watchdog: claim reminder failed", "incident_id", it.Incident.ID, "error", err)
			continue
		}
		if !won {
			// Another replica's watchdog already claimed this reminder.
			continue
		}
		duration := int64(now.Sub(it.Incident.StartedAt).Seconds())
		ev := Event{
			Kind:            "reminder",
			Monitor:         it.Monitor,
			Incident:        it.Incident,
			Regions:         it.Incident.Regions,
			Cause:           it.Incident.Cause,
			DurationSeconds: duration,
		}
		if err := w.Notifier.Notify(ctx, ev); err != nil {
			// See the claim-before-notify trade-off in the doc comment
			// above: this is NOT retried on the next tick.
			slog.Warn("uptime: watchdog: reminder notify failed after claim", "incident_id", it.Incident.ID, "error", err)
		}
	}
}

// StaleHeartbeats returns enabled heartbeat monitors whose grace period has
// lapsed without a ping: last_beat_at (or, if it never pinged, created_at)
// plus the monitor's own grace_seconds (from its HeartbeatConfig) is in the
// past. Regions/ChannelIDs are not populated (unlike Get/List) — the
// heartbeat watchdog only needs id/project_id/consensus/fail_threshold and
// friends, all covered by monitorColumns.
func (s *Service) StaleHeartbeats(ctx context.Context) ([]Monitor, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, `+monitorColumns+`
		FROM monitors
		WHERE kind = 'heartbeat' AND enabled
		  AND COALESCE(last_beat_at, created_at)
		      + make_interval(secs => COALESCE((config->>'grace_seconds')::int, 60))
		      < now()
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("uptime: stale heartbeats: %w", err)
	}
	defer rows.Close()
	var out []Monitor
	for rows.Next() {
		var m Monitor
		var heartbeatToken *string
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Name, &m.Kind, &m.Enabled, &m.IntervalSeconds,
			&m.TimeoutSeconds, &m.Config, &m.FailThreshold, &m.RecoveryThreshold, &m.Consensus,
			&m.RemindEveryMinutes, &m.SSLAlertDays, &m.SSLExpiresAt, &heartbeatToken,
			&m.LastBeatAt, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("uptime: stale heartbeats: %w", err)
		}
		if heartbeatToken != nil {
			m.HeartbeatToken = *heartbeatToken
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SSLCandidates returns every monitor with a known certificate expiry
// (ssl_expires_at IS NOT NULL), regardless of kind — set by any https check
// via Detector.updateSSL/Service.SetSSLExpiry. Unlike Get/List, it also
// populates Monitor.SSLAlertedDays (see that field's doc comment).
func (s *Service) SSLCandidates(ctx context.Context) ([]Monitor, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, `+monitorColumns+`, ssl_alerted_days
		FROM monitors
		WHERE ssl_expires_at IS NOT NULL
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("uptime: ssl candidates: %w", err)
	}
	defer rows.Close()
	var out []Monitor
	for rows.Next() {
		var m Monitor
		var heartbeatToken *string
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Name, &m.Kind, &m.Enabled, &m.IntervalSeconds,
			&m.TimeoutSeconds, &m.Config, &m.FailThreshold, &m.RecoveryThreshold, &m.Consensus,
			&m.RemindEveryMinutes, &m.SSLAlertDays, &m.SSLExpiresAt, &heartbeatToken,
			&m.LastBeatAt, &m.CreatedAt, &m.SSLAlertedDays); err != nil {
			return nil, fmt.Errorf("uptime: ssl candidates: %w", err)
		}
		if heartbeatToken != nil {
			m.HeartbeatToken = *heartbeatToken
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ClaimSSLAlert atomically records thresholds as alerted for monitorID and
// reports whether THIS call won the claim: true when at least one of
// thresholds wasn't already present in ssl_alerted_days (so the row got
// updated and RETURNING produced a row), false when every one of them was
// already recorded (by this call or a concurrent one) — nothing to claim.
//
// Race-safety: the check (NOT ssl_alerted_days @> thresholds) and the write
// (array_agg(DISTINCT ...)) happen in one UPDATE statement, not a
// read-then-write pair — so of several `gotcha --mode=uptime` replicas
// racing ClaimSSLAlert for the same monitor/thresholds, exactly one observes
// won=true; the rest see the row the winner already updated and get no row
// back. Callers MUST NOT Notify before calling this, and MUST only Notify
// when won is true — see watchdog.go's checkSSL, this method's only caller.
func (s *Service) ClaimSSLAlert(ctx context.Context, monitorID int64, thresholds []int) (bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		UPDATE monitors
		SET ssl_alerted_days = (SELECT array_agg(DISTINCT x) FROM unnest(ssl_alerted_days || $2::int[]) x)
		WHERE id = $1 AND NOT (ssl_alerted_days @> $2::int[])
		RETURNING id`,
		monitorID, thresholds,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("uptime: claim ssl alert: %w", err)
	}
	return true, nil
}

// IncidentsDueForReminder returns (incident, monitor) pairs for every open,
// non-maintenance incident whose monitor wants periodic reminders
// (remind_every_minutes > 0) and hasn't had one recently enough:
// coalesce(last_reminded_at, started_at) + remind_every_minutes is in the
// past.
func (s *Service) IncidentsDueForReminder(ctx context.Context) ([]ReminderItem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.monitor_id, i.started_at, i.resolved_at, i.cause, i.regions,
			i.in_maintenance, i.notified_open, i.notified_close, i.last_reminded_at,
			m.id, m.project_id, m.name, m.kind, m.enabled, m.interval_seconds, m.timeout_seconds,
			m.config, m.fail_threshold, m.recovery_threshold, m.consensus, m.remind_every_minutes,
			m.ssl_alert_days, m.ssl_expires_at, m.heartbeat_token, m.last_beat_at, m.created_at
		FROM incidents i
		JOIN monitors m ON m.id = i.monitor_id
		WHERE i.resolved_at IS NULL
		  AND i.in_maintenance = false
		  AND m.remind_every_minutes > 0
		  AND COALESCE(i.last_reminded_at, i.started_at)
		      + make_interval(mins => m.remind_every_minutes)
		      < now()
		ORDER BY i.id`)
	if err != nil {
		return nil, fmt.Errorf("uptime: incidents due for reminder: %w", err)
	}
	defer rows.Close()
	var out []ReminderItem
	for rows.Next() {
		var inc Incident
		var m Monitor
		var heartbeatToken *string
		if err := rows.Scan(&inc.ID, &inc.MonitorID, &inc.StartedAt, &inc.ResolvedAt, &inc.Cause, &inc.Regions,
			&inc.InMaintenance, &inc.NotifiedOpen, &inc.NotifiedClose, &inc.LastRemindedAt,
			&m.ID, &m.ProjectID, &m.Name, &m.Kind, &m.Enabled, &m.IntervalSeconds, &m.TimeoutSeconds,
			&m.Config, &m.FailThreshold, &m.RecoveryThreshold, &m.Consensus, &m.RemindEveryMinutes,
			&m.SSLAlertDays, &m.SSLExpiresAt, &heartbeatToken, &m.LastBeatAt, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("uptime: incidents due for reminder: %w", err)
		}
		if heartbeatToken != nil {
			m.HeartbeatToken = *heartbeatToken
		}
		out = append(out, ReminderItem{Incident: inc, Monitor: m})
	}
	return out, rows.Err()
}

// ClaimReminder atomically records that a reminder was just sent for
// incidentID (last_reminded_at = now()) and reports whether THIS call won
// the claim: true only when the incident is still open (resolved_at IS
// NULL) and hasn't been reminded (or opened, via COALESCE) more recently
// than notBefore. The caller computes notBefore as now - remind_every —
// see watchdog.go's checkReminders, this method's only caller, for why that
// shape (an absolute cutoff, not a duration) is the cleaner fit: it already
// has "now" in hand from the same tick that picked this incident via
// IncidentsDueForReminder, so the two use the same clock reading.
//
// Race-safety: the check-and-set is one UPDATE statement, not a
// read-then-write pair — so of several `gotcha --mode=uptime` replicas
// racing ClaimReminder for the same incident, exactly one observes
// won=true. Callers MUST NOT Notify before calling this, and MUST only
// Notify when won is true.
func (s *Service) ClaimReminder(ctx context.Context, incidentID int64, notBefore time.Time) (bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		UPDATE incidents
		SET last_reminded_at = now()
		WHERE id = $1 AND resolved_at IS NULL AND coalesce(last_reminded_at, started_at) <= $2
		RETURNING id`,
		incidentID, notBefore,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("uptime: claim reminder: %w", err)
	}
	return true, nil
}
