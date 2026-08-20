package trace

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Значения Decision.Kind — что делать оценщику (план 4) с целью.
const (
	DecisionOpen    = "open"    // порог пробит, открытого инцидента нет → открыть
	DecisionResolve = "resolve" // метрика восстановилась → закрыть открытый
	DecisionNone    = "none"    // ничего не делать (нет базы/статистики/в норме)
)

// RegressionSample — замер метрики в окне: Value — p95 эндпойнта или p75
// web-vital'а, в миллисекундах (CLS безразмерный), Samples — число замеров в
// окне (по нему решается достаточность статистики). Единица держится
// единственностью точки конвертации: все пути duration проходят через
// msSample (query.go), которая переводит us→ms; Decide сам конвертацией не
// занимается и полагается на то, что Value уже в мс — иначе абсолютный пол
// (cfg.Floor) сравнивается не в своих единицах и не работает (было так, пока
// пакетные запросы эндпойнтов звали valueSample вместо msSample).
type RegressionSample struct {
	Value   float64
	Samples int
}

// Decision — решение детектора по одной цели за один тик оценщика.
type Decision struct {
	Kind string // DecisionOpen | DecisionResolve | DecisionNone
}

// Decide — чистая логика детекции регрессии (§6): БЕЗ БД/IO/времени.
// base — скользящая база (медиана дневных значений), recent — свежее окно,
// metric — для выбора абсолютного пола (cfg.Floor), open — есть ли уже открытый
// инцидент по этой цели.
//
// Порядок проверок важен: сначала отсекаем недостаток статистики и отсутствие
// базы (решать не на чем), затем — гистерезисное закрытие для открытых и
// открытие по двойному условию (относительный порог И абсолютный пол) для
// закрытых.
func Decide(base, recent RegressionSample, cfg RegressionConfig, metric string, open bool) Decision {
	// Мало сэмплов в любом из окон → статистики нет, решения не принимаем.
	if recent.Samples < cfg.MinSamples || base.Samples < cfg.MinSamples {
		return Decision{Kind: DecisionNone}
	}
	// Нет базы (нулевая/отрицательная) → не с чем сравнивать.
	if base.Value <= 0 {
		return Decision{Kind: DecisionNone}
	}

	if open {
		// Гистерезис: закрываем, только когда вернулись под recovery-порог
		// (recovery_pct < threshold_pct — иначе инцидент мигал бы на границе).
		if recent.Value <= base.Value*(1+cfg.RecoveryPct) {
			return Decision{Kind: DecisionResolve}
		}
		return Decision{Kind: DecisionNone}
	}

	// Открытие только при одновременном выполнении относительного порога И
	// абсолютного пола: пол режет ложную тревогу на «+100% с 20 на 40 мс».
	if recent.Value > base.Value*(1+cfg.ThresholdPct) && recent.Value > base.Value+cfg.Floor(metric) {
		return Decision{Kind: DecisionOpen}
	}
	return Decision{Kind: DecisionNone}
}

// Regression — строка perf_regressions: инцидент регрессии производительности
// (рост p95 эндпойнта или p75 web-vital'а над скользящей базой), моделируемый как
// open/close по образцу uptime-инцидентов (см. internal/uptime/incident.go). На
// цель (project_id, target, metric) — не более одного открытого одновременно,
// это держит частичный уникальный индекс perf_regressions_one_open_idx.
type Regression struct {
	ID         int64
	ProjectID  int64
	TargetKind string // 'endpoint_p95' | 'webvital_p75'
	Target     string
	Metric     string // 'duration' | 'lcp' | 'inp' | 'cls' | 'fcp' | 'ttfb'
	Status     string // 'open' | 'resolved'

	BaselineValue float64
	PeakValue     float64
	CurrentValue  float64

	StartedAt  time.Time
	ResolvedAt *time.Time

	InMaintenance bool
	NotifiedOpen  bool
	NotifiedClose bool
}

const regressionColumns = `id, project_id, target_kind, target, metric, status, baseline_value, peak_value, current_value, started_at, resolved_at, in_maintenance, notified_open, notified_close`

func scanRegression(row pgx.Row) (Regression, error) {
	var r Regression
	if err := row.Scan(&r.ID, &r.ProjectID, &r.TargetKind, &r.Target, &r.Metric, &r.Status,
		&r.BaselineValue, &r.PeakValue, &r.CurrentValue, &r.StartedAt, &r.ResolvedAt,
		&r.InMaintenance, &r.NotifiedOpen, &r.NotifiedClose); err != nil {
		return Regression{}, err
	}
	return r, nil
}

// RegressionService — CRUD инцидентов регрессий поверх perf_regressions.
type RegressionService struct {
	pool *pgxpool.Pool
}

// NewRegressionService создаёт сервис поверх пула PG.
func NewRegressionService(pool *pgxpool.Pool) *RegressionService {
	return &RegressionService{pool: pool}
}

// Open открывает новый инцидент по цели (project_id, target, metric), если по ней
// ещё нет открытого. Гонко-безопасность держится на частичном уникальном индексе
// perf_regressions_one_open_idx (project_id, target, metric) WHERE status='open':
// INSERT целится прямо в этот индекс как arbiter, поэтому из двух параллельных
// вызовов ровно один INSERT проходит, а второй ловит конфликт (DO NOTHING →
// RETURNING не отдаёт строки) — окна read-then-write нет. Проигравший дочитывает
// строку победителя и возвращает created=false. baseline_value=base,
// peak_value=current, current_value=current на вставке. inMaintenance (B3)
// фиксируется на инциденте на всё его время: вызывающий решает по
// MaintenanceChecker в момент открытия, гейт notify — на нём же, а не на
// состоянии окна в момент закрытия (см. host.IncidentService.Open).
func (s *RegressionService) Open(ctx context.Context, projectID int64, targetKind, target, metric string, base, current float64, inMaintenance bool) (Regression, bool, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO perf_regressions (project_id, target_kind, target, metric, baseline_value, peak_value, current_value, in_maintenance)
		VALUES ($1, $2, $3, $4, $5, $6, $6, $7)
		ON CONFLICT (project_id, target, metric) WHERE status = 'open' DO NOTHING
		RETURNING `+regressionColumns,
		projectID, targetKind, target, metric, base, current, inMaintenance)
	r, err := scanRegression(row)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, found, err := s.OpenFor(ctx, projectID, target, metric)
		if err != nil {
			return Regression{}, false, err
		}
		if !found {
			return Regression{}, false, fmt.Errorf("trace: open regression: conflicted but no open incident found")
		}
		return existing, false, nil
	}
	if err != nil {
		return Regression{}, false, fmt.Errorf("trace: open regression: %w", err)
	}
	return r, true, nil
}

// OpenFor возвращает открытый инцидент по (project_id, target, metric), если он
// есть.
func (s *RegressionService) OpenFor(ctx context.Context, projectID int64, target, metric string) (Regression, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+regressionColumns+`
		FROM perf_regressions WHERE project_id = $1 AND target = $2 AND metric = $3 AND status = 'open'`,
		projectID, target, metric)
	r, err := scanRegression(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Regression{}, false, nil
	}
	if err != nil {
		return Regression{}, false, fmt.Errorf("trace: open regression for: %w", err)
	}
	return r, true, nil
}

// RegressionKey — цель и метрика, ключ карты OpenForProject. Поля названы как
// Regression.Target/Regression.Metric — не переиспользует query.VitalKey
// (Transaction/Metric): тот ключ живёт на стороне ClickHouse-агрегатов и
// назван их терминологией, этот — на стороне хранилища регрессий в PG.
type RegressionKey struct {
	Target string
	Metric string
}

// OpenForProject возвращает ВСЕ открытые инциденты проекта одним запросом,
// картой по (target, metric) — находка №43: evalTarget звал OpenFor на
// КАЖДУЮ цель по отдельности, и сто проектов при тике в 5 минут давали
// двадцать тысяч последовательных запросов в PostgreSQL за проход, в одной
// горутине.
//
// Фильтр только по project_id и status='open' — без цели/метрики: у проекта
// открыто немного инцидентов одновременно (не больше одного на цель — держит
// perf_regressions_one_open_idx), поэтому дешевле забрать их все один раз,
// чем передавать список целей в IN/ANY. Запрос ложится на
// perf_regressions_one_open_idx (project_id, target, metric) WHERE
// status = 'open' (0011_perf_regressions.up.sql:23): WHERE-условие совпадает
// дословно, project_id — ведущая колонка индекса.
//
// В отличие от StatesBatch (internal/uptime/state.go) карта НЕ предзаполняется
// нулевыми записями на заранее известный список ключей: здесь нет входного
// списка целей (тик ещё не решил, какие цели будет оценивать — тот список
// приходит из CH позже, TopEndpointsByTraffic/TopVitalPages), поэтому
// единица чтения — «весь проект», а не «эти конкретные пары». Отсутствие
// ключа в карте и есть ответ «не открыт» — тем же способом, каким
// OpenFor раньше отдавал found=false, только через comma-ok на карте, а не
// через отдельный булев результат.
func (s *RegressionService) OpenForProject(ctx context.Context, projectID int64) (map[RegressionKey]Regression, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+regressionColumns+`
		FROM perf_regressions WHERE project_id = $1 AND status = 'open'`,
		projectID)
	if err != nil {
		return nil, fmt.Errorf("trace: open regressions for project: %w", err)
	}
	defer rows.Close()
	out := make(map[RegressionKey]Regression)
	for rows.Next() {
		r, err := scanRegression(rows)
		if err != nil {
			return nil, fmt.Errorf("trace: open regressions for project: %w", err)
		}
		out[RegressionKey{Target: r.Target, Metric: r.Metric}] = r
	}
	return out, rows.Err()
}

// Bump обновляет метрику открытого инцидента: current_value=$2,
// peak_value=max(peak_value,$2). По закрытому/несуществующему id → ErrNotFound.
func (s *RegressionService) Bump(ctx context.Context, id int64, current float64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE perf_regressions
		SET current_value = $2, peak_value = GREATEST(peak_value, $2)
		WHERE id = $1 AND status = 'open'`, id, current)
	if err != nil {
		return fmt.Errorf("trace: bump regression: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Resolve закрывает открытый инцидент (status='resolved', resolved_at=now,
// current_value=$2). ok=false, если открытого не было (идемпотентно: повторный
// вызов после закрытия отдаёт ok=false, а не ошибку).
func (s *RegressionService) Resolve(ctx context.Context, id int64, current float64) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE perf_regressions
		SET status = 'resolved', resolved_at = now(), current_value = $2
		WHERE id = $1 AND status = 'open'
		RETURNING id`, id, current)
	var closedID int64
	err := row.Scan(&closedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("trace: resolve regression: %w", err)
	}
	return true, nil
}

// MarkNotified фиксирует отправку уведомления об открытии/закрытии инцидента:
// open=true выставляет notified_open, иначе notified_close. Неизвестный id →
// ErrNotFound.
func (s *RegressionService) MarkNotified(ctx context.Context, id int64, open bool) error {
	column := "notified_close"
	if open {
		column = "notified_open"
	}
	tag, err := s.pool.Exec(ctx, "UPDATE perf_regressions SET "+column+" = true WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("trace: mark regression notified: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// List возвращает регрессии проекта (открытые и закрытые), свежайшие первыми —
// для UI плана 5.
func (s *RegressionService) List(ctx context.Context, projectID int64, limit int) ([]Regression, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+regressionColumns+`
		FROM perf_regressions WHERE project_id = $1
		ORDER BY started_at DESC
		LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("trace: list regressions: %w", err)
	}
	defer rows.Close()
	var out []Regression
	for rows.Next() {
		r, err := scanRegression(rows)
		if err != nil {
			return nil, fmt.Errorf("trace: list regressions: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
