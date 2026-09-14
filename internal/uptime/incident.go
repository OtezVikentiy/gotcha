package uptime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
)

type Incident struct {
	ID              int64
	MonitorID       int64
	StartedAt       time.Time
	ResolvedAt      *time.Time
	Cause           string
	Regions         []string
	InMaintenance   bool
	NotifiedOpen    bool
	NotifiedClose   bool
	LastRemindedAt  *time.Time
	SuppressedByDep bool
	AcknowledgedAt  *time.Time
	AcknowledgedBy  *int64
	Severity        string
	EscalationLevel int
	LastEscalatedAt *time.Time
	// NotifyOpenFailed отличает "попытка провалилась" от "сознательно не
	// уведомляли" (suppressed_by_dep/in_maintenance).
	NotifyOpenFailed   bool
	NotifyOpenAttempts int
	// nil — ретраить логирование шага 0 нечего: либо уже завершено, либо
	// инцидент старше этой колонки.
	NotifyOpenChannels []int64
}

// NotifyOpenFailed=true само по себе значит, что ретрай ещё идёт; true
// здесь — попытки исчерпаны, канал доставки считается мёртвым.
func (inc Incident) DeliveryExhausted() bool {
	return inc.NotifyOpenFailed && inc.NotifyOpenAttempts >= maxNotifyOpenAttempts
}

const incidentColumns = `id, monitor_id, started_at, resolved_at, cause, regions, in_maintenance, notified_open, notified_close, last_reminded_at, suppressed_by_dep, acknowledged_at, acknowledged_by, severity, escalation_level, last_escalated_at, notify_open_failed, notify_open_attempts, notify_open_channels`

// единственный список приёмников под incidentColumns — второй независимый
// Scan-список на ту же строку колонок уже расходился с ней на практике.
func incidentScanDest(inc *Incident) []any {
	return []any{&inc.ID, &inc.MonitorID, &inc.StartedAt, &inc.ResolvedAt, &inc.Cause,
		&inc.Regions, &inc.InMaintenance, &inc.NotifiedOpen, &inc.NotifiedClose, &inc.LastRemindedAt,
		&inc.SuppressedByDep, &inc.AcknowledgedAt, &inc.AcknowledgedBy, &inc.Severity,
		&inc.EscalationLevel, &inc.LastEscalatedAt, &inc.NotifyOpenFailed, &inc.NotifyOpenAttempts,
		&inc.NotifyOpenChannels}
}

func scanIncident(row pgx.Row) (Incident, error) {
	var inc Incident
	if err := row.Scan(incidentScanDest(&inc)...); err != nil {
		return Incident{}, err
	}
	return inc, nil
}

// race-safe за счёт партиального уникального индекса incidents_one_open_idx:
// конфликтующий INSERT возвращает ноль строк, проигравший читает уже открытый инцидент.
func (s *Service) OpenIncident(ctx context.Context, monitorID int64, cause string, regions []string, inMaintenance bool) (Incident, bool, error) {
	if regions == nil {
		regions = []string{} // regions NOT NULL — pgx кодирует nil-слайс как SQL NULL
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO incidents (monitor_id, cause, regions, in_maintenance)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (monitor_id) WHERE resolved_at IS NULL DO NOTHING
		RETURNING `+incidentColumns,
		monitorID, cause, regions, inMaintenance)
	inc, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, found, err := s.OpenIncidentFor(ctx, monitorID)
		if err != nil {
			return Incident{}, false, err
		}
		if !found {
			return Incident{}, false, fmt.Errorf("uptime: open incident: conflicted but no open incident found")
		}
		return existing, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("uptime: open incident: %w", err)
	}
	return inc, true, nil
}

// ok=false, если открытого инцидента не было — идемпотентно, не ошибка.
func (s *Service) ResolveIncident(ctx context.Context, monitorID int64, at time.Time) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE incidents SET resolved_at = $2
		WHERE monitor_id = $1 AND resolved_at IS NULL
		RETURNING `+incidentColumns,
		monitorID, at)
	inc, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("uptime: resolve incident: %w", err)
	}
	return inc, true, nil
}

func (s *Service) IncidentByID(ctx context.Context, id int64) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+incidentColumns+" FROM incidents WHERE id = $1", id)
	inc, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("uptime: incident by id: %w", err)
	}
	return inc, true, nil
}

func (s *Service) OpenIncidentFor(ctx context.Context, monitorID int64) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+incidentColumns+`
		FROM incidents WHERE monitor_id = $1 AND resolved_at IS NULL`, monitorID)
	inc, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("uptime: open incident for: %w", err)
	}
	return inc, true, nil
}

func queryIncidents(ctx context.Context, pool *pgxpool.Pool, query string, args ...any) ([]Incident, error) {
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("uptime: incidents: %w", err)
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("uptime: incidents: %w", err)
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

func (s *Service) Incidents(ctx context.Context, projectID int64, limit int) ([]Incident, error) {
	return queryIncidents(ctx, s.pool, `
		SELECT i.id, i.monitor_id, i.started_at, i.resolved_at, i.cause, i.regions,
			i.in_maintenance, i.notified_open, i.notified_close, i.last_reminded_at, i.suppressed_by_dep,
			i.acknowledged_at, i.acknowledged_by, i.severity, i.escalation_level, i.last_escalated_at,
			i.notify_open_failed, i.notify_open_attempts, i.notify_open_channels
		FROM incidents i
		JOIN monitors m ON m.id = i.monitor_id
		WHERE m.project_id = $1
		ORDER BY i.started_at DESC
		LIMIT $2`, projectID, limit)
}

func (s *Service) IncidentsForMonitor(ctx context.Context, monitorID int64, limit int) ([]Incident, error) {
	return queryIncidents(ctx, s.pool, `
		SELECT `+incidentColumns+`
		FROM incidents WHERE monitor_id = $1
		ORDER BY started_at DESC
		LIMIT $2`, monitorID, limit)
}

// per-monitor топ-N через row_number() OVER (PARTITION BY monitor_id) — обычный
// LIMIT срезал бы топ по всему набору сразу, а не по каждому монитору отдельно.
func (s *Service) IncidentsForMonitorsBatch(ctx context.Context, monitorIDs []int64, limit int) (map[int64][]Incident, error) {
	out := make(map[int64][]Incident, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}
	for _, id := range monitorIDs {
		out[id] = nil // монитор без инцидентов остаётся в карте, не отсутствует
	}
	if limit <= 0 {
		return out, nil
	}
	rows, err := queryIncidents(ctx, s.pool, `
		SELECT `+incidentColumns+`
		FROM (
			SELECT `+incidentColumns+`,
			       row_number() OVER (PARTITION BY monitor_id ORDER BY started_at DESC) AS rn
			FROM incidents
			WHERE monitor_id = ANY($1)
		) ranked
		WHERE rn <= $2
		ORDER BY monitor_id, started_at DESC`, monitorIDs, limit)
	if err != nil {
		return nil, err
	}
	for _, inc := range rows {
		out[inc.MonitorID] = append(out[inc.MonitorID], inc)
	}
	return out, nil
}

// count(*) отдельным запросом, не OVER() — OVER() материализует и считает
// всю выборку до LIMIT, и первая страница стоила бы как вся история.
func queryIncidentsPaged(ctx context.Context, pool *pgxpool.Pool, countFrom, pageQuery string, key int64, limit, offset int) ([]Incident, int64, error) {
	var total int64
	if err := pool.QueryRow(ctx, "SELECT count(*)"+countFrom, key).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("uptime: incidents count: %w", err)
	}
	if int64(offset) >= total {
		return nil, 0, nil
	}
	out, err := queryIncidents(ctx, pool, pageQuery, key, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	if len(out) == 0 {
		return nil, 0, nil
	}
	return out, total, nil
}

func (s *Service) IncidentsPaged(ctx context.Context, projectID int64, limit, offset int) ([]Incident, int64, error) {
	const from = `
		FROM incidents i
		JOIN monitors m ON m.id = i.monitor_id
		WHERE m.project_id = $1`
	return queryIncidentsPaged(ctx, s.pool, from, `
		SELECT i.id, i.monitor_id, i.started_at, i.resolved_at, i.cause, i.regions,
			i.in_maintenance, i.notified_open, i.notified_close, i.last_reminded_at, i.suppressed_by_dep,
			i.acknowledged_at, i.acknowledged_by, i.severity, i.escalation_level, i.last_escalated_at,
			i.notify_open_failed, i.notify_open_attempts, i.notify_open_channels`+from+`
		ORDER BY i.started_at DESC
		LIMIT $2 OFFSET $3`, projectID, limit, offset)
}

func (s *Service) IncidentsForMonitorPaged(ctx context.Context, monitorID int64, limit, offset int) ([]Incident, int64, error) {
	const from = ` FROM incidents WHERE monitor_id = $1`
	return queryIncidentsPaged(ctx, s.pool, from, `
		SELECT `+incidentColumns+from+`
		ORDER BY started_at DESC
		LIMIT $2 OFFSET $3`, monitorID, limit, offset)
}

// open=true также сбрасывает notify_open_failed и поднимает escalation_level
// минимум до 1 (GREATEST — идемпотентно на повторный вызов).
func (s *Service) MarkNotified(ctx context.Context, incidentID int64, open bool) error {
	column := "notified_close"
	extra := ""
	if open {
		column = "notified_open"
		extra = ", notify_open_failed = false, escalation_level = GREATEST(escalation_level, 1)"
	}
	tag, err := s.pool.Exec(ctx, "UPDATE incidents SET "+column+" = true"+extra+" WHERE id = $1", incidentID)
	if err != nil {
		return fmt.Errorf("uptime: mark notified: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) MarkNotifyOpenFailed(ctx context.Context, incidentID int64) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE incidents SET notify_open_failed = true, notify_open_attempts = notify_open_attempts + 1 WHERE id = $1",
		incidentID)
	if err != nil {
		return fmt.Errorf("uptime: mark notify open failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// пишет снимок ДО попытки логирования шага 0 — крах между доставкой и записью
// иначе терял бы список безвозвратно, и ретраить лог было бы нечем.
func (s *Service) SetNotifyOpenChannels(ctx context.Context, incidentID int64, channelIDs []int64) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE incidents SET notify_open_channels = $2 WHERE id = $1", incidentID, channelIDs)
	if err != nil {
		return fmt.Errorf("uptime: set notify open channels: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) ClearNotifyOpenChannels(ctx context.Context, incidentID int64) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE incidents SET notify_open_channels = NULL WHERE id = $1", incidentID)
	if err != nil {
		return fmt.Errorf("uptime: clear notify open channels: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// project_id проверяется через JOIN на monitors — у incidents нет своей
// колонки; ok=false — уже подтверждён/закрыт или чужой проект, не ошибка.
func (s *Service) Acknowledge(ctx context.Context, incidentID, projectID, userID int64) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE incidents SET acknowledged_at = now(), acknowledged_by = $3
		WHERE id = $1 AND resolved_at IS NULL AND acknowledged_at IS NULL
		  AND monitor_id IN (SELECT id FROM monitors WHERE project_id = $2)
		RETURNING id`, incidentID, projectID, userID)
	var ackedID int64
	err := row.Scan(&ackedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("uptime: acknowledge incident: %w", err)
	}
	return true, nil
}

func (s *Service) Name() string { return "uptime" }

// escalation_level > 0: Detector сам владеет первой доставкой "down" (свой
// hold/grace/retry автомат) — без фильтра Scheduler продублировал бы её.
func (s *Service) OpenUnacked(ctx context.Context) ([]escalation.PendingIncident, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, m.project_id,
		       GREATEST(i.started_at, COALESCE(g.resolved_at, i.started_at), COALESCE(i.dep_released_at, i.started_at)) AS started_at,
		       i.severity, i.escalation_level
		FROM incidents i
		JOIN monitors m ON m.id = i.monitor_id
		LEFT JOIN incident_groups g ON g.id = i.group_id
		WHERE i.resolved_at IS NULL AND i.acknowledged_at IS NULL AND i.suppressed_by_dep = false
		  AND m.enabled
		  AND i.escalation_level > 0
		  AND (i.group_id IS NULL OR g.id IS NULL OR g.resolved_at IS NOT NULL)
		ORDER BY i.id`)
	if err != nil {
		return nil, fmt.Errorf("uptime: open unacked incidents: %w", err)
	}
	defer rows.Close()
	var out []escalation.PendingIncident
	for rows.Next() {
		var p escalation.PendingIncident
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.StartedAt, &p.Severity, &p.EscalationLevel); err != nil {
			return nil, fmt.Errorf("uptime: open unacked incidents scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ok=false — level уже не равен from: гонку с другим тиком планировщика
// проиграли, идемпотентно.
func (s *Service) BumpEscalation(ctx context.Context, id int64, from int) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE incidents SET escalation_level = $2 + 1, last_escalated_at = now()
		WHERE id = $1 AND escalation_level = $2
		RETURNING id`, id, from)
	var bumpedID int64
	err := row.Scan(&bumpedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("uptime: bump escalation: %w", err)
	}
	return true, nil
}

func (s *Service) MarkSuppressedByDep(ctx context.Context, incidentID int64) error {
	tag, err := s.pool.Exec(ctx, "UPDATE incidents SET suppressed_by_dep = true WHERE id = $1", incidentID)
	if err != nil {
		return fmt.Errorf("uptime: mark suppressed by dep: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// "AND suppressed_by_dep" делает повторный вызов идемпотентным; RowsAffected==0
// поэтому не считается ошибкой — не отличить "не было" от "уже снято".
func (s *Service) ClearSuppressedByDep(ctx context.Context, incidentID int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE incidents SET suppressed_by_dep = false, dep_released_at = now()
		WHERE id = $1 AND suppressed_by_dep`, incidentID)
	if err != nil {
		return fmt.Errorf("uptime: clear suppressed by dep: %w", err)
	}
	return nil
}

// escalation.SuppressedSource: снятие подавления не должно ждать нового
// результата пробы — Detector.settleHeldIncident вызывается только из
// OnResult, а регион мог замолчать навсегда (монитор на паузе, регион
// удалён), и тогда реактивный путь не перезапустится никогда.
func (s *Service) OpenSuppressed(ctx context.Context) ([]escalation.PendingIncident, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, m.project_id, i.started_at, i.severity, i.escalation_level
		FROM incidents i
		JOIN monitors m ON m.id = i.monitor_id
		WHERE i.resolved_at IS NULL AND i.acknowledged_at IS NULL AND i.suppressed_by_dep = true
		ORDER BY i.id`)
	if err != nil {
		return nil, fmt.Errorf("uptime: open suppressed incidents: %w", err)
	}
	defer rows.Close()
	var out []escalation.PendingIncident
	for rows.Next() {
		var p escalation.PendingIncident
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.StartedAt, &p.Severity, &p.EscalationLevel); err != nil {
			return nil, fmt.Errorf("uptime: open suppressed incidents scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// escalation.SuppressedSource: тот же эффект, что и ClearSuppressedByDep
// (используется и реактивно, из Detector), просто под именем, которого
// ждёт интерфейс Scheduler'а.
func (s *Service) ClearSuppressed(ctx context.Context, incidentID int64) error {
	return s.ClearSuppressedByDep(ctx, incidentID)
}
