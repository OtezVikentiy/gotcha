package uptime

import (
	"context"
	"fmt"
	"time"
)

// Job — одно задание из очереди check_queue вместе с монитором, чтобы
// исполнитель мог сразу выполнить проверку без похода в БД за монитором.
//
// LeaseUntil — момент истечения lease, выданного ИМЕННО ЭТОМУ исполнителю.
// Это токен оптимистичной блокировки: он же — единственное доказательство
// того, что задание всё ещё «наше», и по нему ClaimJob снимает задание с
// очереди ровно один раз (см. ClaimJob и Ingestor.Accept). Если задание
// успели перевыдать (lease протух, его взяла другая реплика/проба),
// lease_until в строке уже другой — и наш claim не пройдёт.
type Job struct {
	QueueID    int64
	MonitorID  int64
	Region     string
	LeaseUntil time.Time
	Monitor    Monitor
}

// Schedule — планировщик (см. спеку §6): один CTE-стейтмент, идемпотентный
// и безопасный при нескольких репликах благодаря уникальному индексу
// (monitor_id, region) на check_queue. Ставит задание каждому включённому
// не-heartbeat монитору в каждом его регионе, для которого пришло время
// (now() >= last_scheduled_at + interval), и обновляет last_scheduled_at
// всем таким мониторам. Если задание для (monitor_id, region) уже стоит в
// очереди (предыдущая проверка ещё не завершена — ClaimJob не вызван),
// дубль не создаётся: ON CONFLICT DO NOTHING на уникальном индексе. Возвращает
// число реально поставленных заданий (без учёта пропущенных дублей).
func (s *Service) Schedule(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		WITH due AS (
			SELECT m.id, r.region
			FROM monitors m
			JOIN monitor_regions r ON r.monitor_id = m.id
			WHERE m.enabled AND m.kind <> 'heartbeat'
			  AND now() >= coalesce(m.last_scheduled_at, '-infinity')
			               + make_interval(secs => m.interval_seconds)
			FOR UPDATE OF m SKIP LOCKED
		), ins AS (
			INSERT INTO check_queue (monitor_id, region, due_at)
			SELECT id, region, now() FROM due
			ON CONFLICT (monitor_id, region) DO NOTHING
			RETURNING monitor_id
		), upd AS (
			-- Only advance last_scheduled_at for monitors whose job was actually
			-- inserted. If the INSERT was skipped (ON CONFLICT DO NOTHING) because a
			-- previous job is still pending, we must not update last_scheduled_at —
			-- otherwise each scheduler tick advances the next due time further into
			-- the future, stretching the effective check cadence exactly when it matters most.
			UPDATE monitors SET last_scheduled_at = now()
			WHERE id IN (SELECT monitor_id FROM ins)
			RETURNING id
		)
		SELECT count(*) FROM ins`,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("uptime: schedule: %w", err)
	}
	return n, nil
}

// scanLeasedJobs consumes rows produced by the lease queries below: queue
// id, monitor id, region, lease_until, followed by monitorColumns.
func scanLeasedJobs(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]Job, error) {
	var out []Job
	for rows.Next() {
		var j Job
		m := &j.Monitor
		if err := rows.Scan(
			&j.QueueID, &j.MonitorID, &j.Region, &j.LeaseUntil,
			&m.ProjectID, &m.Name, &m.Kind, &m.Enabled, &m.IntervalSeconds, &m.TimeoutSeconds,
			&m.Config, &m.FailThreshold, &m.RecoveryThreshold, &m.Consensus, &m.RemindEveryMinutes,
			&m.SSLAlertDays, &m.SSLExpiresAt, &m.LastBeatAt, &m.CreatedAt,
		); err != nil {
			return nil, err
		}
		m.ID = j.MonitorID
		out = append(out, j)
	}
	return out, rows.Err()
}

// lease is the shared implementation behind LeaseLocal/LeaseForProbe: picks
// up to limit due jobs of region (not currently leased, or whose lease has
// expired), marks them leased for 2x the monitor's interval (comfortably
// longer than a single check, so a slow-but-alive prober isn't raced by a
// second lease before it reports back), and returns them together with
// their monitor. probeID is stored in leased_by when non-nil (LeaseForProbe);
// LeaseLocal passes nil, leaving leased_by NULL.
//
// orgID scopes the pick to one tenant and MUST be non-nil whenever probeID is
// (remote probes belong to an organization): region names are free-form, so two
// unrelated orgs routinely use the same one ("eu-west"), and a region-only pick
// would hand org B's monitors — config and all, HTTP headers with its secrets
// included — to org A's probe, letting it drive org B's monitors up and down.
// The in-process prober (LeaseLocal) passes nil: it serves every org by design.
func (s *Service) lease(ctx context.Context, region string, limit int, probeID, orgID *int64) ([]Job, error) {
	rows, err := s.pool.Query(ctx, `
		WITH picked AS (
			SELECT q.id, q.monitor_id
			FROM check_queue q
			WHERE q.region = $1 AND (q.lease_until IS NULL OR q.lease_until < now())
			  AND ($4::bigint IS NULL OR EXISTS (
					SELECT 1 FROM monitors qm
					JOIN projects p ON p.id = qm.project_id
					WHERE qm.id = q.monitor_id AND p.org_id = $4))
			ORDER BY q.due_at
			FOR UPDATE OF q SKIP LOCKED
			LIMIT $2
		), leased AS (
			UPDATE check_queue q
			SET lease_until = now() + make_interval(secs => m.interval_seconds * 2),
				leased_by = $3
			FROM picked, monitors m
			WHERE q.id = picked.id AND m.id = picked.monitor_id
			RETURNING q.id, q.monitor_id, q.region, q.lease_until
		)
		SELECT l.id, l.monitor_id, l.region, l.lease_until, `+monitorColumns+`
		FROM leased l
		JOIN monitors m ON m.id = l.monitor_id
		ORDER BY l.id`,
		region, limit, probeID, orgID)
	if err != nil {
		return nil, fmt.Errorf("uptime: lease: %w", err)
	}
	defer rows.Close()

	out, err := scanLeasedJobs(rows)
	if err != nil {
		return nil, fmt.Errorf("uptime: lease: %w", err)
	}
	return out, nil
}

// LeaseLocal leases up to limit due jobs of region for the in-process local
// prober. leased_by stays NULL — the local prober isn't a registered Probe.
func (s *Service) LeaseLocal(ctx context.Context, region string, limit int) ([]Job, error) {
	return s.lease(ctx, region, limit, nil, nil)
}

// LeaseForProbe leases up to limit due jobs of the probe's region on behalf of
// a registered remote probe, recording it in leased_by (used by /probe/lease).
// Only jobs of monitors belonging to the probe's own organization are handed
// out — the whole Probe is taken (rather than an id + region pair) precisely so
// that the caller cannot forget the tenant it must be scoped to.
func (s *Service) LeaseForProbe(ctx context.Context, probe Probe, limit int) ([]Job, error) {
	return s.lease(ctx, probe.Region, limit, &probe.ID, &probe.OrgID)
}

// LeasedJob возвращает задание queueID вместе с его монитором, ТОЛЬКО если
// оно выдано пробе probeID, её lease ещё не истёк И монитор принадлежит
// организации этой пробы; иначе ErrNotFound. Это проверка доверия для
// /probe/results: центр принимает результат, лишь пока задание действительно
// числится за приславшей его пробой — чужое, протухшее или уже выполненное
// (снятое с очереди ClaimJob) задание неотличимы и все дают ErrNotFound.
//
// ВНИМАНИЕ: это только предварительная проверка «есть ли смысл продолжать»;
// правом применить результат она не является и от гонки не защищает — два
// одновременных запроса с одним queue_id оба её проходят. Единственный
// арбитр — ClaimJob (см. Ingestor.Accept), которому и передаётся
// Job.LeaseUntil из возвращённого здесь задания.
//
// Org-проверка здесь дублирует org-скоуп LeaseForProbe (вторая линия обороны):
// строка очереди могла быть выдана до того, как проект монитора переехал в
// другую организацию. Организация берётся из самой пробы (JOIN probes), так что
// вызывающему нечего забыть передать.
func (s *Service) LeasedJob(ctx context.Context, queueID, probeID int64) (Job, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT q.id, q.monitor_id, q.region, q.lease_until, `+monitorColumns+`
		FROM check_queue q
		JOIN monitors m ON m.id = q.monitor_id
		WHERE q.id = $1 AND q.leased_by = $2 AND q.lease_until > now()
		  AND EXISTS (
				SELECT 1 FROM projects p
				JOIN probes pr ON pr.org_id = p.org_id
				WHERE p.id = m.project_id AND pr.id = $2 AND pr.revoked_at IS NULL)`,
		queueID, probeID)
	if err != nil {
		return Job{}, fmt.Errorf("uptime: leased job: %w", err)
	}
	defer rows.Close()

	jobs, err := scanLeasedJobs(rows)
	if err != nil {
		return Job{}, fmt.Errorf("uptime: leased job: %w", err)
	}
	if len(jobs) == 0 {
		return Job{}, ErrNotFound
	}
	return jobs[0], nil
}

// ClaimJob atomically claims job queueID for the holder of the lease that
// expires at leaseUntil (Job.LeaseUntil, as handed out by lease): the row is
// deleted from the queue — freeing the (monitor_id, region) slot for the next
// Schedule — and claimed is true ONLY for the caller whose DELETE actually hit
// the row. Everyone else gets (false, nil).
//
// This is the exactly-once gate for applying a check result, and it exists
// because ApplyResult is atomic but NOT idempotent (it increments
// consecutive_fails): applying one real check twice would double-count the
// streak, write two ClickHouse rows and fire the detector twice — with
// fail_threshold=2, a single failed check would take the monitor down.
// Merely checking that the row is still leased to us (LeasedJob) does not
// prevent that: two concurrent POST /probe/results carrying the same queue_id
// both see the live lease and both pass. So the claim, not the check, is what
// authorizes the write — and it must happen BEFORE any side effect.
//
// lease_until is the optimistic-concurrency token: if the lease expired and
// somebody else (another replica's local runner, another process running the
// same probe token) re-leased the row, its lease_until has moved and the stale
// holder's claim no longer matches — it loses, and its result is dropped
// instead of being applied on top of the new holder's.
func (s *Service) ClaimJob(ctx context.Context, queueID int64, leaseUntil time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM check_queue WHERE id = $1 AND lease_until = $2", queueID, leaseUntil)
	if err != nil {
		return false, fmt.Errorf("uptime: claim job: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// PendingCount returns the number of jobs currently sitting in check_queue
// (leased or not) — used by tests and metrics.
func (s *Service) PendingCount(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM check_queue").Scan(&n); err != nil {
		return 0, fmt.Errorf("uptime: pending count: %w", err)
	}
	return n, nil
}
