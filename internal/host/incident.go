package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Kinds — виды встроенных инцидентов хоста (host_incidents.kind, CHECK
// миграции 0066). Источник истины для сторожа i18n (Task 12): любой новый
// вид добавляется здесь и одновременно получает переводы.
var Kinds = []string{"disk", "memory", "load", "silent"}

var ErrIncidentNotFound = errors.New("host: incident not found")

// Incident — открытый или закрытый инцидент встроенного порога хоста
// (host_incidents): диск/память/нагрузка/тишина.
type Incident struct {
	ID            int64
	ProjectID     int64
	HostID        int64
	Kind          string
	Status        string
	CurrentValue  float64
	PeakValue     float64
	Detail        string
	StartedAt     time.Time
	ResolvedAt    *time.Time
	InMaintenance bool
	NotifiedOpen  bool
	NotifiedClose bool
}

const incidentColumns = `id, project_id, host_id, kind, status, current_value, peak_value,
	detail, started_at, resolved_at, in_maintenance, notified_open, notified_close`

func scanIncident(row pgx.Row) (Incident, error) {
	var in Incident
	err := row.Scan(&in.ID, &in.ProjectID, &in.HostID, &in.Kind, &in.Status,
		&in.CurrentValue, &in.PeakValue, &in.Detail,
		&in.StartedAt, &in.ResolvedAt, &in.InMaintenance, &in.NotifiedOpen, &in.NotifiedClose)
	return in, err
}

// IncidentService — атомарные open/close встроенных инцидентов хоста
// (калька metric.IncidentService, ключ конфликта — (host_id, kind): на
// одном хосте disk и load могут быть открыты одновременно, но не два disk).
type IncidentService struct {
	pool *pgxpool.Pool
}

func NewIncidentService(pool *pgxpool.Pool) *IncidentService {
	return &IncidentService{pool: pool}
}

// Open открывает инцидент по (host_id, kind), если открытого такого ещё
// нет. inMaintenance фиксируется на инциденте на всё его время (B3): вызывающий
// решает по MaintenanceChecker в момент открытия, гейт notify — на нём же, а не
// на состоянии окна в момент закрытия. Гонко-безопасно через частичный уникальный
// индекс host_incidents_one_open_idx (host_id, kind) WHERE status='open': из
// параллельных вызовов ровно один INSERT проходит, остальные ловят
// конфликт (DO NOTHING → нет RETURNING) и дочитывают победителя через
// OpenFor. peak=current на вставке.
func (s *IncidentService) Open(ctx context.Context, projectID, hostID int64, kind string, current float64, detail string, inMaintenance bool) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, in_maintenance)
		VALUES ($1, $2, $3, 'open', $4, $4, $5, $6)
		ON CONFLICT (host_id, kind) WHERE status = 'open' DO NOTHING
		RETURNING `+incidentColumns,
		projectID, hostID, kind, current, detail, inMaintenance)
	in, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, found, err := s.OpenFor(ctx, hostID, kind)
		if err != nil {
			return Incident{}, false, err
		}
		if !found {
			return Incident{}, false, fmt.Errorf("host: open incident: conflicted but no open incident found")
		}
		return existing, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("host: open incident: %w", err)
	}
	return in, true, nil
}

// OpenFor возвращает открытый инцидент хоста данного вида, если он есть.
func (s *IncidentService) OpenFor(ctx context.Context, hostID int64, kind string) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+incidentColumns+" FROM host_incidents WHERE host_id = $1 AND kind = $2 AND status = 'open'",
		hostID, kind)
	in, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("host: open incident for: %w", err)
	}
	return in, true, nil
}

// Bump обновляет открытый инцидент: current_value=$2, peak_value=$3 (peak
// вычисляет вызывающий — экстремум в сторону нарушения). Закрытый/нет →
// ErrIncidentNotFound.
func (s *IncidentService) Bump(ctx context.Context, id int64, current, peak float64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE host_incidents SET current_value = $2, peak_value = $3
		WHERE id = $1 AND status = 'open'`, id, current, peak)
	if err != nil {
		return fmt.Errorf("host: bump incident: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrIncidentNotFound
	}
	return nil
}

// Resolve закрывает открытый инцидент. ok=false, если открытого не было
// (идемпотентно).
func (s *IncidentService) Resolve(ctx context.Context, id int64, current float64) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE host_incidents SET status = 'resolved', resolved_at = now(), current_value = $2
		WHERE id = $1 AND status = 'open'
		RETURNING id`, id, current)
	var closedID int64
	err := row.Scan(&closedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("host: resolve incident: %w", err)
	}
	return true, nil
}

// ResolveOpenByProjectKind закрывает ВСЕ открытые инциденты проекта данного
// вида и возвращает их число.
//
// Нужен ровно одному сценарию: оператор выключил шумный порог на странице
// настроек. Evaluator.Tick выключенные виды пропускает целиком (disk/memory/
// load — по s.*Enabled, silent — первой строкой evalSilent), а значит закрыть
// уже открытый инцидент выключенного вида некому — красный бейдж на списке
// хостов остался бы навсегда, и снять его было бы нечем: ручного закрытия
// инцидента хоста в интерфейсе нет.
//
// Уведомление о закрытии здесь НЕ ставится в очередь (notified_close остаётся
// false): порог выключил сам оператор, и «инцидент закрыт» в канал — это шум о
// его же собственном действии, а не новость. Досылки по notified_close в
// подсистеме хостов нет — флаг читает только тот, кто уведомление отправил.
func (s *IncidentService) ResolveOpenByProjectKind(ctx context.Context, projectID int64, kind string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE host_incidents SET status = 'resolved', resolved_at = now()
		WHERE project_id = $1 AND kind = $2 AND status = 'open'`, projectID, kind)
	if err != nil {
		return 0, fmt.Errorf("host: resolve open incidents by project kind: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ResolveOpenByHostKind закрывает открытый инцидент КОНКРЕТНОГО хоста
// данного вида, если он есть, и возвращает 1 (или 0, если открытого не
// было).
//
// Зеркало ResolveOpenByProjectKind, но по одному хосту: каскад порогов
// (Task 4/5) может выключить вид ТОЧЕЧНО на одном хосте (host-override) или
// группе (role/env-override), а не на всём проекте, и в этом случае закрывать
// инциденты всех хостов проекта разом было бы неверно — соседей с включённым
// видом это задело бы напрасно. Evaluator.Tick зовёт этот метод для хоста,
// чей эффективный порог (Task 4) выключен, а открытый инцидент есть — иначе
// он висел бы открытым вечно: ручного закрытия инцидента хоста в интерфейсе
// нет.
//
// Уведомление о закрытии здесь тоже НЕ ставится в очередь — по той же
// причине, что и у ResolveOpenByProjectKind: порог выключил сам оператор
// (или его собственная настройка каскада), и «инцидент закрыт» в канал — это
// шум о его же действии, а не новость.
func (s *IncidentService) ResolveOpenByHostKind(ctx context.Context, hostID int64, kind string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE host_incidents SET status = 'resolved', resolved_at = now()
		WHERE host_id = $1 AND kind = $2 AND status = 'open'`, hostID, kind)
	if err != nil {
		return 0, fmt.Errorf("host: resolve open incidents by host kind: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ListOpenKindsForHosts — батч-версия «какие виды инцидентов сейчас открыты»
// для оценщика (evaluator.go): один запрос на ВСЕ хосты тика вместо отдельного
// UPDATE на каждый (host, kind) с выключенным видом (M-A ремедиации Task 5,
// см. Evaluator.evalOrCloseKind). Хосты без открытых инцидентов в карте
// отсутствуют — как GetForHosts у HostOverrideService.
func (s *IncidentService) ListOpenKindsForHosts(ctx context.Context, hostIDs []int64) (map[int64]map[string]bool, error) {
	out := make(map[int64]map[string]bool, len(hostIDs))
	if len(hostIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT host_id, kind FROM host_incidents WHERE host_id = ANY($1) AND status = 'open'`,
		hostIDs)
	if err != nil {
		return nil, fmt.Errorf("host: list open incident kinds for hosts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var hostID int64
		var kind string
		if err := rows.Scan(&hostID, &kind); err != nil {
			return nil, fmt.Errorf("host: scan open incident kind row: %w", err)
		}
		if out[hostID] == nil {
			out[hostID] = make(map[string]bool)
		}
		out[hostID][kind] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("host: list open incident kinds for hosts: %w", err)
	}
	return out, nil
}

// MarkNotified фиксирует отправку уведомления (open → notified_open, иначе
// notified_close).
func (s *IncidentService) MarkNotified(ctx context.Context, id int64, open bool) error {
	column := "notified_close"
	if open {
		column = "notified_open"
	}
	tag, err := s.pool.Exec(ctx, "UPDATE host_incidents SET "+column+" = true WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("host: mark incident notified: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrIncidentNotFound
	}
	return nil
}

// ListByProject возвращает инциденты проекта, свежайшие первыми (для UI).
func (s *IncidentService) ListByProject(ctx context.Context, projectID int64, limit int) ([]Incident, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+incidentColumns+" FROM host_incidents WHERE project_id = $1 ORDER BY started_at DESC LIMIT $2",
		projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("host: list incidents by project: %w", err)
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("host: list incidents by project scan: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// ListOpenByProject возвращает ВСЕ открытые инциденты проекта, свежайшие
// первыми.
//
// Без лимита намеренно, в отличие от ListByProject: список хостов сворачивает
// открытые инциденты по host_id, и «последние N инцидентов проекта любого
// статуса» для этого не годится — в проекте, где закрытых инцидентов больше
// лимита, открытый инцидент старого хоста не попал бы в выборку вовсе, и хост
// показался бы спокойным при живой проблеме. Открытых инцидентов по построению
// немного: частичный уникальный индекс host_incidents_one_open_idx допускает
// не больше одного на пару (host_id, kind), то есть потолок — хосты × 4 вида.
func (s *IncidentService) ListOpenByProject(ctx context.Context, projectID int64) ([]Incident, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+incidentColumns+" FROM host_incidents WHERE project_id = $1 AND status = 'open' ORDER BY started_at DESC",
		projectID)
	if err != nil {
		return nil, fmt.Errorf("host: list open incidents by project: %w", err)
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("host: list open incidents by project scan: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// ListOpenByHost возвращает все открытые инциденты хоста (разных видов).
func (s *IncidentService) ListOpenByHost(ctx context.Context, hostID int64) ([]Incident, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+incidentColumns+" FROM host_incidents WHERE host_id = $1 AND status = 'open' ORDER BY started_at DESC",
		hostID)
	if err != nil {
		return nil, fmt.Errorf("host: list open incidents by host: %w", err)
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("host: list open incidents by host scan: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
