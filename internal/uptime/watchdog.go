package uptime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// доля Interval, не меньше пола — иначе повисший запрос держит self-метрику
// живости бесконечно. checkSSL сюда не входит — суточный бюджет свой, ниже.
const (
	tickBudgetShare = 0.8
	minTickBudget   = 10 * time.Second
)

// фиксированные 30с, не доля SSLEvery (сутки×0.8 бессмысленны как потолок) —
// без бюджета первый безусловный прогон в Run вешал бы Watchdog на старте.
const sslCheckBudget = 30 * time.Second

// не настоящая ошибка проверки (heartbeat не опрашивается активно) — нужна,
// чтобы текст причины/уведомления Detector.OnResult читался осмысленно.
const heartbeatMissedError = "no heartbeat within grace period"

type ReminderItem struct {
	Incident Incident
	Monitor  Monitor
}

// живая проверка перед напоминанием: снимок incidents.in_maintenance не видит
// окно, начавшееся после открытия инцидента.
type MaintenanceChecker interface {
	InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error)
}

type Watchdog struct {
	Svc      *Service
	Detector *Detector // heartbeat misses run through OnResult so incident thresholds/notifications behave like any other check
	Notifier Notifier  // used directly for ssl_expiring/reminder events, which don't go through Detector

	// nil допустим — «окон нет», напоминания идут как обычно.
	Maint MaintenanceChecker

	// обязателен в проде: без него пропущенный удар не пишется в check_results,
	// и доля OK/Total heartbeat-монитора держится 100% даже при открытом инциденте.
	Writer *ResultWriter

	// должен совпадать с регионом активных проверок монитора (cfg.LocalRegion);
	// пусто — DefaultRegion.
	Region string

	Interval time.Duration // heartbeat + reminder tick period, default 1 minute
	SSLEvery time.Duration // SSL check tick period, default 24 hours

	lastTickUnix    atomic.Int64  // unix-время последнего завершённого прохода heartbeat+reminder
	lastTickSeconds atomic.Uint64 // длительность последнего прохода, math.Float64bits
}

// 0, если ни одного прохода ещё не было. Мёртвый или отставший Watchdog
// снаружи выглядит как «пропущенных heartbeat и созревших напоминаний сейчас нет».
func (w *Watchdog) LastTickUnix() int64 { return w.lastTickUnix.Load() }

func (w *Watchdog) LastTickSeconds() float64 {
	return math.Float64frombits(w.lastTickSeconds.Load())
}

func (w *Watchdog) tickBudget() time.Duration {
	interval := w.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	budget := time.Duration(float64(interval) * tickBudgetShare)
	if budget < minTickBudget {
		return minTickBudget
	}
	return budget
}

func (w *Watchdog) region() string {
	if w.Region == "" {
		return DefaultRegion
	}
	return w.Region
}

// в отличие от Runner/ResultWriter отдельного Close нет — Watchdog не
// держит буферизованное состояние, достаточно ctx (см. drain() в main.go).
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

	// первый прогон checkSSL — сразу, иначе частый рестарт никогда не запускал бы
	// проверку сертификатов; безопасен для реплик — порог клеймится атомарно.
	w.checkSSL(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.tick(ctx)
		case <-sslTick.C:
			w.checkSSL(ctx)
		}
	}
}

func (w *Watchdog) tick(ctx context.Context) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, w.tickBudget())
	defer cancel()

	w.checkHeartbeats(ctx)
	w.checkReminders(ctx)

	w.lastTickSeconds.Store(math.Float64bits(time.Since(started).Seconds()))
	if ctx.Err() != nil {
		slog.Warn("uptime: watchdog: heartbeat/reminder tick did not finish within its budget",
			"budget", w.tickBudget())
		return
	}
	w.lastTickUnix.Store(time.Now().Unix())
}

// пропущенный удар проходит через Detector.OnResult как обычный отказ —
// fail_threshold/consensus/инциденты ведут себя одинаково для тишины и провала.
// Клейм перед ApplyResult: его предикат отсекает только СТАРШИЙ результат,
// не одновременный, иначе вторая реплика применит тот же пропуск повторно.
func (w *Watchdog) checkHeartbeats(ctx context.Context) {
	monitors, err := w.Svc.StaleHeartbeats(ctx)
	if err != nil {
		slog.Error("uptime: watchdog: stale heartbeats failed", "error", err)
		return
	}
	region := w.region()
	at := time.Now().UTC()
	debounce := w.Interval
	if debounce <= 0 {
		debounce = time.Minute
	}
	for _, m := range monitors {
		won, err := w.Svc.ClaimHeartbeatMiss(ctx, m.ID, region, debounce, at)
		if err != nil {
			slog.Error("uptime: watchdog: claim heartbeat miss failed", "monitor_id", m.ID, "error", err)
			continue
		}
		if !won {
			// Другая реплика уже применила пропуск этого heartbeat-тика.
			continue
		}
		st, err := w.Svc.ApplyResult(ctx, m.ID, region, false, heartbeatMissedError, at)
		if err != nil {
			slog.Error("uptime: watchdog: apply heartbeat miss failed", "monitor_id", m.ID, "error", err)
			continue
		}
		res := Result{OK: false, Error: heartbeatMissedError}
		if w.Writer != nil {
			w.Writer.Add(m.ProjectID, m.ID, region, at, res)
		}
		if w.Detector != nil {
			w.Detector.OnResult(ctx, m, region, res, st)
		}
	}
}

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

// округляет вверх — 5 дней и 1 час, и 5 дней и 23 часа, оба «осталось 5»;
// сертификат, истекающий через 30 минут, уже «остался 1 день», не 0.
func daysLeftUntil(expires, now time.Time) int {
	return int(math.Ceil(expires.Sub(now).Hours() / 24))
}

// один Notify покрывает все пересечённые пороги разом, иначе следующий тик не
// переалертил бы меньший; claim атомарен (RETURNING) — из гонки реплик побеждает одна.
func (w *Watchdog) checkSSL(ctx context.Context) {
	if w.Notifier == nil {
		// пропускаем claim тоже: клеймом без Notify порог навсегда пометился бы
		// алертнутым, и подключённый позже Notifier по нему уже не сработает.
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sslCheckBudget)
	defer cancel()

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
			slog.Warn("uptime: watchdog: ssl notify failed after claim", "monitor_id", m.ID, "days", due, "error", err)
		}
	}
}

// live-проверка InMaintenance ловит только окно, открывшееся после инцидента —
// более раннее не всплывает; ошибка проверки не блокирует напоминание умышленно.
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
	inMaint := map[int64]bool{}
	for _, it := range items {
		if w.Maint != nil {
			skip, cached := inMaint[it.Monitor.ProjectID]
			if !cached {
				skip, err = w.Maint.InMaintenance(ctx, it.Monitor.ProjectID, now)
				if err != nil {
					slog.Warn("uptime: watchdog: maintenance check failed, sending reminder anyway",
						"incident_id", it.Incident.ID, "project_id", it.Monitor.ProjectID, "error", err)
					skip = false
				}
				inMaint[it.Monitor.ProjectID] = skip
			}
			if skip {
				continue
			}
		}
		won, err := w.Svc.ClaimReminder(ctx, it.Incident.ID, it.Monitor.RemindEveryMinutes)
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
			// claim уже прошёл — на следующем тике не ретраится (цена анти-дублирования).
			slog.Warn("uptime: watchdog: reminder notify failed after claim", "incident_id", it.Incident.ID, "error", err)
		}
	}
}

// ChannelIDs не заполняются, их не читают Detector/Notifier; RegionCount нужен —
// aggregate() берёт его знаменателем consensus, без него all/majority ломается.
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
	var out []Monitor
	var ids []int64
	for rows.Next() {
		var m Monitor
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Name, &m.Kind, &m.Enabled, &m.IntervalSeconds,
			&m.TimeoutSeconds, &m.Config, &m.FailThreshold, &m.RecoveryThreshold, &m.Consensus,
			&m.RemindEveryMinutes, &m.SSLAlertDays, &m.SSLExpiresAt,
			&m.LastBeatAt, &m.CreatedAt, &m.Retries); err != nil {
			rows.Close()
			return nil, fmt.Errorf("uptime: stale heartbeats: %w", err)
		}
		out = append(out, m)
		ids = append(ids, m.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("uptime: stale heartbeats: %w", err)
	}

	regionsByMon, err := regionsOfBatch(ctx, s.pool, ids)
	if err != nil {
		return nil, err
	}
	for i, m := range out {
		regions := regionsByMon[m.ID]
		out[i].Regions = regions
		out[i].RegionCount = len(regions)
	}
	return out, nil
}

// в отличие от Get/List, дополнительно заполняет SSLAlertedDays.
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
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Name, &m.Kind, &m.Enabled, &m.IntervalSeconds,
			&m.TimeoutSeconds, &m.Config, &m.FailThreshold, &m.RecoveryThreshold, &m.Consensus,
			&m.RemindEveryMinutes, &m.SSLAlertDays, &m.SSLExpiresAt,
			&m.LastBeatAt, &m.CreatedAt, &m.Retries, &m.SSLAlertedDays); err != nil {
			return nil, fmt.Errorf("uptime: ssl candidates: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Клеймит только вызов, чей at не моложе debounce от предыдущего
// last_checked_at — won=false значит пропуск уже применён другим вызовом.
func (s *Service) ClaimHeartbeatMiss(ctx context.Context, monitorID int64, region string, debounce time.Duration, at time.Time) (bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO monitor_state (monitor_id, region, status, consecutive_fails, consecutive_oks, last_checked_at, last_error)
		VALUES ($1, $2, 'unknown', 0, 0, $4, '')
		ON CONFLICT (monitor_id, region) DO UPDATE
		SET last_checked_at = $4
		WHERE monitor_state.last_checked_at IS NULL
		   OR monitor_state.last_checked_at + make_interval(secs => $3) <= $4
		RETURNING monitor_id`,
		monitorID, region, debounce.Seconds(), at,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("uptime: claim heartbeat miss: %w", err)
	}
	return true, nil
}

// один UPDATE, не read-then-write — из гонки реплик побеждает ровно одна;
// won=false значит уже отмечено, Notify тогда вызывать нельзя.
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

// m.enabled: пауза глушит напоминания, но не закрывает инцидент — история не
// переписывается, закроет его живая проверка после снятия паузы.
func (s *Service) IncidentsDueForReminder(ctx context.Context) ([]ReminderItem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.monitor_id, i.started_at, i.resolved_at, i.cause, i.regions,
			i.in_maintenance, i.notified_open, i.notified_close, i.last_reminded_at, i.suppressed_by_dep,
			m.id, m.project_id, m.name, m.kind, m.enabled, m.interval_seconds, m.timeout_seconds,
			m.config, m.fail_threshold, m.recovery_threshold, m.consensus, m.remind_every_minutes,
			m.ssl_alert_days, m.ssl_expires_at, m.last_beat_at, m.created_at, m.retries
		FROM incidents i
		JOIN monitors m ON m.id = i.monitor_id
		WHERE i.resolved_at IS NULL
		  AND i.in_maintenance = false
		  AND i.suppressed_by_dep = false
		  AND i.notified_open = true
		  AND m.enabled
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
		if err := rows.Scan(&inc.ID, &inc.MonitorID, &inc.StartedAt, &inc.ResolvedAt, &inc.Cause, &inc.Regions,
			&inc.InMaintenance, &inc.NotifiedOpen, &inc.NotifiedClose, &inc.LastRemindedAt, &inc.SuppressedByDep,
			&m.ID, &m.ProjectID, &m.Name, &m.Kind, &m.Enabled, &m.IntervalSeconds, &m.TimeoutSeconds,
			&m.Config, &m.FailThreshold, &m.RecoveryThreshold, &m.Consensus, &m.RemindEveryMinutes,
			&m.SSLAlertDays, &m.SSLExpiresAt, &m.LastBeatAt, &m.CreatedAt, &m.Retries); err != nil {
			return nil, fmt.Errorf("uptime: incidents due for reminder: %w", err)
		}
		out = append(out, ReminderItem{Incident: inc, Monitor: m})
	}
	return out, rows.Err()
}

// cutoff считается внутри UPDATE от now() сервера — часы контейнера и БД
// расходятся на практике; побеждает ровно одна реплика, Notify только при won=true.
func (s *Service) ClaimReminder(ctx context.Context, incidentID int64, remindEveryMinutes int) (bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		UPDATE incidents
		SET last_reminded_at = now()
		WHERE id = $1 AND resolved_at IS NULL
		  AND coalesce(last_reminded_at, started_at) + make_interval(mins => $2) <= now()
		RETURNING id`,
		incidentID, remindEveryMinutes,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("uptime: claim reminder: %w", err)
	}
	return true, nil
}
