package metric

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
)

var ErrIncidentNotFound = errors.New("metric: incident not found")

// Incident — открытый или закрытый инцидент пробоя порога (metric_incidents).
type Incident struct {
	ID             int64
	RuleID         int64
	ProjectID      int64
	Status         string
	PeakValue      float64
	CurrentValue   float64
	StartedAt      time.Time
	ResolvedAt     *time.Time
	InMaintenance  bool
	NotifiedOpen   bool
	NotifiedClose  bool
	AcknowledgedAt *time.Time
	AcknowledgedBy *int64
	Severity       string
}

const incidentColumns = `id, rule_id, project_id, status, peak_value, current_value,
	started_at, resolved_at, in_maintenance, notified_open, notified_close,
	acknowledged_at, acknowledged_by, severity`

func scanIncident(row pgx.Row) (Incident, error) {
	var in Incident
	err := row.Scan(&in.ID, &in.RuleID, &in.ProjectID, &in.Status, &in.PeakValue, &in.CurrentValue,
		&in.StartedAt, &in.ResolvedAt, &in.InMaintenance, &in.NotifiedOpen, &in.NotifiedClose,
		&in.AcknowledgedAt, &in.AcknowledgedBy, &in.Severity)
	return in, err
}

// IncidentService — атомарные open/close инцидентов (калька RegressionService).
type IncidentService struct {
	pool *pgxpool.Pool
}

func NewIncidentService(pool *pgxpool.Pool) *IncidentService {
	return &IncidentService{pool: pool}
}

// Open открывает инцидент по правилу, если открытого ещё нет. inMaintenance
// фиксируется на инциденте на всё его время (B3): вызывающий решает по
// MaintenanceChecker в момент открытия, гейт notify — на нём же, а не на
// состоянии окна в момент закрытия. Гонко-безопасно через частичный уникальный
// индекс metric_incidents_one_open_idx (rule_id) WHERE status='open': из
// параллельных вызовов ровно один INSERT проходит, остальные ловят конфликт
// (DO NOTHING → нет RETURNING) и дочитывают победителя. peak=current на
// вставке. severity — override из правила (B4, T5): "" (нет override) даёт
// table-DEFAULT 'warning' через COALESCE в INSERT, непустое значение идёт как
// есть.
func (s *IncidentService) Open(ctx context.Context, ruleID, projectID int64, current float64, inMaintenance bool, severity string) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO metric_incidents (rule_id, project_id, peak_value, current_value, in_maintenance, severity)
		VALUES ($1, $2, $3, $3, $4, COALESCE(NULLIF($5,''), 'warning'))
		ON CONFLICT (rule_id) WHERE status = 'open' DO NOTHING
		RETURNING `+incidentColumns,
		ruleID, projectID, current, inMaintenance, severity)
	in, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, found, err := s.OpenFor(ctx, ruleID)
		if err != nil {
			return Incident{}, false, err
		}
		if !found {
			return Incident{}, false, fmt.Errorf("metric: open incident: conflicted but no open incident found")
		}
		return existing, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("metric: open incident: %w", err)
	}
	return in, true, nil
}

// OpenFor возвращает открытый инцидент правила, если он есть.
func (s *IncidentService) OpenFor(ctx context.Context, ruleID int64) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+incidentColumns+" FROM metric_incidents WHERE rule_id = $1 AND status = 'open'", ruleID)
	in, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("metric: open incident for: %w", err)
	}
	return in, true, nil
}

// GetByID возвращает инцидент по id (любого статуса). Нужен эскалации (B4,
// T6): планировщик и StepNotifier знают только incidentID, объект инцидента
// приходится перегружать заново.
func (s *IncidentService) GetByID(ctx context.Context, id int64) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+incidentColumns+" FROM metric_incidents WHERE id = $1", id)
	in, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("metric: get incident by id: %w", err)
	}
	return in, true, nil
}

// Bump обновляет открытый инцидент: current_value=$2, peak_value=$3 (peak
// вычисляет вызывающий — экстремум в сторону нарушения). Закрытый/нет → ErrIncidentNotFound.
func (s *IncidentService) Bump(ctx context.Context, id int64, current, peak float64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE metric_incidents SET current_value = $2, peak_value = $3
		WHERE id = $1 AND status = 'open'`, id, current, peak)
	if err != nil {
		return fmt.Errorf("metric: bump incident: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrIncidentNotFound
	}
	return nil
}

// resolveIncidentSQL — единственный UPDATE, которым инцидент закрывается:
// им пользуются и штатный Resolve (оценщик), и закрытие при выключении
// правила (resolveOpenIncidentForRule) — чтобы поля закрытия не могли
// разъехаться между двумя путями.
const resolveIncidentSQL = `
		UPDATE metric_incidents SET status = 'resolved', resolved_at = now(), current_value = $2
		WHERE id = $1 AND status = 'open'
		RETURNING id`

// Resolve закрывает открытый инцидент. ok=false, если открытого не было
// (идемпотентно).
func (s *IncidentService) Resolve(ctx context.Context, id int64, current float64) (bool, error) {
	row := s.pool.QueryRow(ctx, resolveIncidentSQL, id, current)
	var closedID int64
	err := row.Scan(&closedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("metric: resolve incident: %w", err)
	}
	return true, nil
}

// resolveOpenIncidentForRule закрывает открытый инцидент правила при его
// выключении — тем же resolveIncidentSQL, что и штатный Resolve, поэтому
// resolved_at и семантика статуса совпадают со штатным закрытием;
// current_value остаётся последним измеренным (свежего агрегата в момент
// выключения нет, значение передаётся его же собственным). Уведомление о
// восстановлении не шлётся намеренно: восстановления не было, правило
// выключил оператор; notified_close=false — то же штатное состояние, что при
// пустом наборе recovery-каналов (см. evaluator.notifyClose). Открытого
// инцидента может не быть — это не ошибка. Вызывается только из транзакции
// RuleService.Update: выключение правила и закрытие его инцидента атомарны.
func resolveOpenIncidentForRule(ctx context.Context, tx pgx.Tx, ruleID int64) error {
	var id int64
	var current float64
	err := tx.QueryRow(ctx,
		"SELECT id, current_value FROM metric_incidents WHERE rule_id = $1 AND status = 'open'",
		ruleID).Scan(&id, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("metric: open incident of disabled rule: %w", err)
	}
	if err := tx.QueryRow(ctx, resolveIncidentSQL, id, current).Scan(&id); err != nil {
		return fmt.Errorf("metric: resolve incident of disabled rule: %w", err)
	}
	return nil
}

// MarkNotified фиксирует отправку уведомления (open → notified_open, иначе
// notified_close).
func (s *IncidentService) MarkNotified(ctx context.Context, id int64, open bool) error {
	column := "notified_close"
	if open {
		column = "notified_open"
	}
	tag, err := s.pool.Exec(ctx, "UPDATE metric_incidents SET "+column+" = true WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("metric: mark incident notified: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrIncidentNotFound
	}
	return nil
}

// Acknowledge подтверждает открытый инцидент (B4: эскалации) — фиксирует
// acknowledged_at/acknowledged_by, чем гасит дальнейшую эскалацию. ok=false,
// если инцидент уже подтверждён или закрыт (идемпотентно). project_id в
// WHERE — defense-in-depth (зеркало uptime.DeleteWindow, B3).
func (s *IncidentService) Acknowledge(ctx context.Context, incidentID, projectID, userID int64) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE metric_incidents SET acknowledged_at = now(), acknowledged_by = $3
		WHERE id = $1 AND project_id = $2 AND status = 'open' AND acknowledged_at IS NULL
		RETURNING id`, incidentID, projectID, userID)
	var ackedID int64
	err := row.Scan(&ackedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("metric: acknowledge incident: %w", err)
	}
	return true, nil
}

// Name — ключ источника для эскалации (B4, T4): совпадает с incident_source
// 'metric' в incident_escalations (0077).
func (s *IncidentService) Name() string { return "metric" }

// OpenUnacked возвращает открытые неподтверждённые инциденты — кандидаты
// планировщика эскалации (T7). Члены ОТКРЫТЫХ групп исключаются (D3 Р5):
// информирование берёт на себя корень; удалённая группа (висячий group_id,
// LEFT JOIN даёт NULL) ≡ закрытая. Для бывшего члена закрытой группы база
// отсчёта лесенки — момент освобождения: StartedAt = GREATEST(started_at,
// g.resolved_at) (анти-залп BLOCKER-1: elapsed планировщика считается от
// StartedAt, и член, просидевший в группе часы, иначе получил бы всю
// лесенку очередью за 2-3 тика).
// Осознанно (фикс ревью плана m-1): фильтр не различает informing/немой
// корень — член НЕМОГО корня уведомил сам (step0 из оценщика), но step1+
// через планировщик пойдут только после закрытия группы. Не баг — буква
// спеки §4.2.
func (s *IncidentService) OpenUnacked(ctx context.Context) ([]escalation.PendingIncident, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.project_id,
		       GREATEST(i.started_at, COALESCE(g.resolved_at, i.started_at)) AS started_at,
		       i.severity, i.escalation_level
		FROM metric_incidents i
		LEFT JOIN incident_groups g ON g.id = i.group_id
		WHERE i.status = 'open' AND i.acknowledged_at IS NULL
		  AND (i.group_id IS NULL OR g.id IS NULL OR g.resolved_at IS NOT NULL)
		ORDER BY i.id`)
	if err != nil {
		return nil, fmt.Errorf("metric: open unacked incidents: %w", err)
	}
	defer rows.Close()
	var out []escalation.PendingIncident
	for rows.Next() {
		var p escalation.PendingIncident
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.StartedAt, &p.Severity, &p.EscalationLevel); err != nil {
			return nil, fmt.Errorf("metric: open unacked incidents scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// BumpEscalation атомарно продвигает уровень эскалации инцидента с from на
// from+1 и фиксирует last_escalated_at (B4, T4). ok=false, если level уже не
// равен from — планировщик проиграл гонку другому тику (идемпотентно).
func (s *IncidentService) BumpEscalation(ctx context.Context, id int64, from int) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE metric_incidents SET escalation_level = $2 + 1, last_escalated_at = now()
		WHERE id = $1 AND escalation_level = $2
		RETURNING id`, id, from)
	var bumpedID int64
	err := row.Scan(&bumpedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("metric: bump escalation: %w", err)
	}
	return true, nil
}

// List возвращает инциденты проекта, свежайшие первыми (для UI).
func (s *IncidentService) List(ctx context.Context, projectID int64, limit int) ([]Incident, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+incidentColumns+" FROM metric_incidents WHERE project_id = $1 ORDER BY started_at DESC LIMIT $2",
		projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("metric: list incidents: %w", err)
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("metric: list incidents scan: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
