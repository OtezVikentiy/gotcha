package uptime

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// LeaseUntil — токен оптимистической блокировки: если его перевыдали другой
// реплике/пробе, значение сменилось, и исходный ClaimJob не пройдёт.
type Job struct {
	QueueID    int64
	MonitorID  int64
	Region     string
	LeaseUntil time.Time
	Monitor    Monitor
}

// безопасен при нескольких репликах благодаря уникальному индексу
// (monitor_id, region) на check_queue — без него дубли неизбежны.
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

// общий для всех запросов, отдающих Job; рассинхрон со scanLeasedJobs ловится
// только в рантайме, не на компиляции. Требует алиасов q и m в запросе.
const leasedJobColumns = `q.id, q.monitor_id, q.region, q.lease_until, ` + monitorColumns + `,
	(SELECT count(*) FROM monitor_regions mr WHERE mr.monitor_id = m.id)`

// порядок полей Scan должен совпадать с порядком leasedJobColumns.
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
			&m.SSLAlertDays, &m.SSLExpiresAt, &m.LastBeatAt, &m.CreatedAt, &m.Retries,
			&m.RegionCount,
		); err != nil {
			return nil, err
		}
		m.ID = j.MonitorID
		out = append(out, j)
	}
	return out, rows.Err()
}

// orgID обязателен вместе с probeID: без скоупа по организации проба одной
// компании получит мониторы другой с тем же именем региона.
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
			SET lease_until = now() + make_interval(secs =>
					m.interval_seconds * 2 + m.retries * (m.timeout_seconds + 1)),
				leased_by = $3
			FROM picked, monitors m
			WHERE q.id = picked.id AND m.id = picked.monitor_id
			RETURNING q.id, q.monitor_id, q.region, q.lease_until
		)
		SELECT `+leasedJobColumns+`
		FROM leased q
		JOIN monitors m ON m.id = q.monitor_id
		ORDER BY q.id`,
		region, limit, probeID, orgID)
	if err != nil {
		return nil, fmt.Errorf("uptime: lease: %w", err)
	}
	defer rows.Close()

	out, err := scanLeasedJobs(rows)
	if err != nil {
		return nil, fmt.Errorf("uptime: lease: %w", err)
	}
	s.decryptJobs(out)
	return out, nil
}

// ошибку расшифровки одного монитора не размножаем на партию: без мастер-ключа
// decryptMonitorConfig обнуляет шифротекст-заголовки, а не пропускает их как есть.
func (s *Service) decryptJobs(jobs []Job) {
	for i := range jobs {
		if err := s.decryptMonitorConfig(&jobs[i].Monitor); err != nil {
			slog.Error("uptime: cannot decrypt monitor headers for check",
				"monitor_id", jobs[i].MonitorID, "error", err)
		}
	}
}

func (s *Service) LeaseLocal(ctx context.Context, region string, limit int) ([]Job, error) {
	return s.lease(ctx, region, limit, nil, nil)
}

// принимает целиком Probe, а не (id, region), чтобы вызывающий не мог
// забыть передать организацию, к которой должен быть скоуп.
func (s *Service) LeaseForProbe(ctx context.Context, probe Probe, limit int) ([]Job, error) {
	return s.lease(ctx, probe.Region, limit, &probe.ID, &probe.OrgID)
}

// предварительная проверка: арбитр — ClaimJob по Job.LeaseUntil отсюда; org
// дублирует скоуп LeaseForProbe, т.к. проект монитора мог сменить организацию.
func (s *Service) LeasedJob(ctx context.Context, queueID, probeID int64) (Job, error) {
	jobs, err := s.LeasedJobs(ctx, []int64{queueID}, probeID)
	if err != nil {
		return Job{}, err
	}
	j, ok := jobs[queueID]
	if !ok {
		return Job{}, ErrNotFound
	}
	return j, nil
}

// чего нет в карте — то же самое, что ErrNotFound у LeasedJob (чужое,
// протухшее или уже выполненное задание).
func (s *Service) LeasedJobs(ctx context.Context, queueIDs []int64, probeID int64) (map[int64]Job, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+leasedJobColumns+`
		FROM check_queue q
		JOIN monitors m ON m.id = q.monitor_id
		WHERE q.id = ANY($1) AND q.leased_by = $2 AND q.lease_until > now()
		  AND EXISTS (
				SELECT 1 FROM projects p
				JOIN probes pr ON pr.org_id = p.org_id
				WHERE p.id = m.project_id AND pr.id = $2 AND pr.revoked_at IS NULL)`,
		queueIDs, probeID)
	if err != nil {
		return nil, fmt.Errorf("uptime: leased job: %w", err)
	}
	defer rows.Close()

	jobs, err := scanLeasedJobs(rows)
	if err != nil {
		return nil, fmt.Errorf("uptime: leased job: %w", err)
	}
	s.decryptJobs(jobs)
	out := make(map[int64]Job, len(jobs))
	for _, j := range jobs {
		out[j.QueueID] = j
	}
	return out, nil
}

type JobClaim struct {
	QueueID    int64
	LeaseUntil time.Time
}

// чего нет в ответе — задание уже забрано или перевыдано с новым
// lease_until; результат отбрасывается, как и при ClaimJob=false.
func (s *Service) ClaimJobs(ctx context.Context, claims []JobClaim) (map[int64]bool, error) {
	ids := make([]int64, len(claims))
	leases := make([]time.Time, len(claims))
	for i, c := range claims {
		ids[i], leases[i] = c.QueueID, c.LeaseUntil
	}
	rows, err := s.pool.Query(ctx, `
		DELETE FROM check_queue q
		USING unnest($1::bigint[], $2::timestamptz[]) AS c(id, lease_until)
		WHERE q.id = c.id AND q.lease_until = c.lease_until
		RETURNING q.id`, ids, leases)
	if err != nil {
		return nil, fmt.Errorf("uptime: claim jobs: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]bool, len(claims))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("uptime: claim jobs: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("uptime: claim jobs: %w", err)
	}
	return out, nil
}

// единственный exactly-once гейт перед применением результата: ApplyResult
// не идемпотентен, повторное применение задвоит consecutive_fails.
func (s *Service) ClaimJob(ctx context.Context, queueID int64, leaseUntil time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM check_queue WHERE id = $1 AND lease_until = $2", queueID, leaseUntil)
	if err != nil {
		return false, fmt.Errorf("uptime: claim job: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Service) PendingCount(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM check_queue").Scan(&n); err != nil {
		return 0, fmt.Errorf("uptime: pending count: %w", err)
	}
	return n, nil
}
