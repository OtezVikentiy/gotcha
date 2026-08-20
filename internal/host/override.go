package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ThresholdOverride — частичное переопределение порогов host-инцидентов:
// nil-поле = наследовать со следующего уровня каскада (группа/проект/дефолт),
// non-nil = пришпилено на этом уровне. Общий тип для двух хранилищ —
// per-host (host_threshold_overrides) и группового по метке
// (host_group_thresholds), их колонки идентичны по смыслу и границам.
type ThresholdOverride struct {
	DiskEnabled     *bool
	DiskThreshold   *float64
	MemoryEnabled   *bool
	MemoryThreshold *float64
	LoadEnabled     *bool
	LoadThreshold   *float64
	SilentEnabled   *bool
	SilentAfter     *time.Duration
}

// ValidateOverride проверяет ТОЛЬКО заданные (non-nil) поля. Для каждого
// вида:
//   - value без enabled (enabled==nil && value!=nil) — ошибка (M-3):
//     резолвер каскада не смог бы решить, откуда брать включённость для
//     пришпиленного значения — «наследовать вкл/выкл, но взять чужое число»
//     не имеет разумного смысла;
//   - enabled=true без value — тоже ошибка: нечего сравнивать с метрикой;
//   - value (заданное, при любом enabled, в т.ч. отсутствующем) обязано быть
//     в тех же границах, что и Validate для полных Settings — сохранённое,
//     но временно выключенное значение не должно быть мусором.
func ValidateOverride(ov ThresholdOverride) error {
	if err := validateKindOverride(ov.DiskEnabled, ov.DiskThreshold,
		func(v float64) bool { return v > 0 && v <= 1 }, ErrInvalidDiskThreshold); err != nil {
		return err
	}
	if err := validateKindOverride(ov.MemoryEnabled, ov.MemoryThreshold,
		func(v float64) bool { return v > 0 && v <= 1 }, ErrInvalidMemoryThreshold); err != nil {
		return err
	}
	if err := validateKindOverride(ov.LoadEnabled, ov.LoadThreshold,
		func(v float64) bool { return v > 0 }, ErrInvalidLoadThreshold); err != nil {
		return err
	}
	if err := validateSilentOverride(ov.SilentEnabled, ov.SilentAfter); err != nil {
		return err
	}
	return nil
}

func validateKindOverride(enabled *bool, value *float64, inBounds func(float64) bool, sentinel error) error {
	if enabled == nil && value != nil {
		return fmt.Errorf("%w: value without enabled flag", sentinel)
	}
	if enabled != nil && *enabled && value == nil {
		return fmt.Errorf("%w: enabled without value", sentinel)
	}
	if value != nil && !inBounds(*value) {
		return fmt.Errorf("%w: got %v", sentinel, *value)
	}
	return nil
}

func validateSilentOverride(enabled *bool, after *time.Duration) error {
	if enabled == nil && after != nil {
		return fmt.Errorf("%w: value without enabled flag", ErrInvalidSilentAfter)
	}
	if enabled != nil && *enabled && after == nil {
		return fmt.Errorf("%w: enabled without value", ErrInvalidSilentAfter)
	}
	if after != nil && (*after < MinSilentAfter || *after > MaxSilentAfter) {
		return fmt.Errorf("%w: got %v", ErrInvalidSilentAfter, *after)
	}
	return nil
}

// durationPtrFromSeconds конвертирует nullable-колонку silent_after_seconds
// (INTEGER) в *time.Duration. pgx не умеет сканить INTEGER NULL напрямую в
// *time.Duration — колонка всегда сканится в промежуточный *int, а
// конвертация выполняется здесь.
func durationPtrFromSeconds(secs *int) *time.Duration {
	if secs == nil {
		return nil
	}
	d := time.Duration(*secs) * time.Second
	return &d
}

// secondsPtrFromDuration — обратная конвертация для записи в БД.
func secondsPtrFromDuration(d *time.Duration) *int {
	if d == nil {
		return nil
	}
	s := int(*d / time.Second)
	return &s
}

// HostOverrideService — Get/Save/GetForHosts переопределений порогов
// конкретного хоста поверх host_threshold_overrides.
type HostOverrideService struct {
	pool *pgxpool.Pool
}

func NewHostOverrideService(pool *pgxpool.Pool) *HostOverrideService {
	return &HostOverrideService{pool: pool}
}

// Get возвращает override хоста. Строки нет (переопределений ещё не
// сохраняли) — не ошибка, а пустой ThresholdOverride (все поля nil, то есть
// «наследовать всё»).
func (s *HostOverrideService) Get(ctx context.Context, hostID int64) (ThresholdOverride, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT disk_enabled, disk_threshold, memory_enabled, memory_threshold,
		       load_enabled, load_threshold, silent_enabled, silent_after_seconds
		FROM host_threshold_overrides
		WHERE host_id = $1`, hostID)

	var ov ThresholdOverride
	var silentSeconds *int
	err := row.Scan(
		&ov.DiskEnabled, &ov.DiskThreshold,
		&ov.MemoryEnabled, &ov.MemoryThreshold,
		&ov.LoadEnabled, &ov.LoadThreshold,
		&ov.SilentEnabled, &silentSeconds,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ThresholdOverride{}, nil
	}
	if err != nil {
		return ThresholdOverride{}, fmt.Errorf("host: get override: %w", err)
	}
	ov.SilentAfter = durationPtrFromSeconds(silentSeconds)
	return ov, nil
}

// Save валидирует и сохраняет override хоста (upsert — первый Save хоста
// создаёт строку, последующие обновляют её и updated_at).
func (s *HostOverrideService) Save(ctx context.Context, hostID int64, ov ThresholdOverride) error {
	if err := ValidateOverride(ov); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO host_threshold_overrides (
			host_id, disk_enabled, disk_threshold, memory_enabled, memory_threshold,
			load_enabled, load_threshold, silent_enabled, silent_after_seconds, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
		ON CONFLICT (host_id) DO UPDATE SET
			disk_enabled = $2, disk_threshold = $3,
			memory_enabled = $4, memory_threshold = $5,
			load_enabled = $6, load_threshold = $7,
			silent_enabled = $8, silent_after_seconds = $9,
			updated_at = now()`,
		hostID,
		ov.DiskEnabled, ov.DiskThreshold,
		ov.MemoryEnabled, ov.MemoryThreshold,
		ov.LoadEnabled, ov.LoadThreshold,
		ov.SilentEnabled, secondsPtrFromDuration(ov.SilentAfter),
	)
	if err != nil {
		return fmt.Errorf("host: save override: %w", err)
	}
	return nil
}

// GetForHosts — батч-версия Get для оценщика (evaluator.go): один запрос на
// все хосты тика вместо N+1. Хосты без строки просто отсутствуют в карте —
// вызывающий трактует это как пустой override (см. Get).
func (s *HostOverrideService) GetForHosts(ctx context.Context, hostIDs []int64) (map[int64]ThresholdOverride, error) {
	out := make(map[int64]ThresholdOverride, len(hostIDs))
	if len(hostIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT host_id, disk_enabled, disk_threshold, memory_enabled, memory_threshold,
		       load_enabled, load_threshold, silent_enabled, silent_after_seconds
		FROM host_threshold_overrides
		WHERE host_id = ANY($1)`, hostIDs)
	if err != nil {
		return nil, fmt.Errorf("host: get overrides for hosts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var hostID int64
		var ov ThresholdOverride
		var silentSeconds *int
		if err := rows.Scan(
			&hostID,
			&ov.DiskEnabled, &ov.DiskThreshold,
			&ov.MemoryEnabled, &ov.MemoryThreshold,
			&ov.LoadEnabled, &ov.LoadThreshold,
			&ov.SilentEnabled, &silentSeconds,
		); err != nil {
			return nil, fmt.Errorf("host: scan override row: %w", err)
		}
		ov.SilentAfter = durationPtrFromSeconds(silentSeconds)
		out[hostID] = ov
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("host: get overrides for hosts: %w", err)
	}
	return out, nil
}

// GroupThreshold — override, привязанный к группе хостов проекта по метке
// (scope "env"/"role" из B1, точное совпадение значения телеметрии).
type GroupThreshold struct {
	Scope, Label string
	ThresholdOverride
}

// GroupThresholdService — List/Upsert/Delete групповых порогов поверх
// host_group_thresholds.
type GroupThresholdService struct {
	pool *pgxpool.Pool
}

func NewGroupThresholdService(pool *pgxpool.Pool) *GroupThresholdService {
	return &GroupThresholdService{pool: pool}
}

// List возвращает все групповые пороги проекта, отсортированные по
// scope/label (стабильный порядок для UI и тестов).
func (s *GroupThresholdService) List(ctx context.Context, projectID int64) ([]GroupThreshold, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT scope, label, disk_enabled, disk_threshold, memory_enabled, memory_threshold,
		       load_enabled, load_threshold, silent_enabled, silent_after_seconds
		FROM host_group_thresholds
		WHERE project_id = $1
		ORDER BY scope, label`, projectID)
	if err != nil {
		return nil, fmt.Errorf("host: list group thresholds: %w", err)
	}
	defer rows.Close()

	var out []GroupThreshold
	for rows.Next() {
		var gt GroupThreshold
		var silentSeconds *int
		if err := rows.Scan(
			&gt.Scope, &gt.Label,
			&gt.DiskEnabled, &gt.DiskThreshold,
			&gt.MemoryEnabled, &gt.MemoryThreshold,
			&gt.LoadEnabled, &gt.LoadThreshold,
			&gt.SilentEnabled, &silentSeconds,
		); err != nil {
			return nil, fmt.Errorf("host: scan group threshold row: %w", err)
		}
		gt.SilentAfter = durationPtrFromSeconds(silentSeconds)
		out = append(out, gt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("host: list group thresholds: %w", err)
	}
	return out, nil
}

// Upsert валидирует и сохраняет override группы (project_id, scope, label) —
// первый Upsert группы создаёт строку, последующие обновляют её и updated_at.
func (s *GroupThresholdService) Upsert(ctx context.Context, projectID int64, scope, label string, ov ThresholdOverride) error {
	if err := ValidateOverride(ov); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO host_group_thresholds (
			project_id, scope, label, disk_enabled, disk_threshold, memory_enabled, memory_threshold,
			load_enabled, load_threshold, silent_enabled, silent_after_seconds, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now())
		ON CONFLICT (project_id, scope, label) DO UPDATE SET
			disk_enabled = $4, disk_threshold = $5,
			memory_enabled = $6, memory_threshold = $7,
			load_enabled = $8, load_threshold = $9,
			silent_enabled = $10, silent_after_seconds = $11,
			updated_at = now()`,
		projectID, scope, label,
		ov.DiskEnabled, ov.DiskThreshold,
		ov.MemoryEnabled, ov.MemoryThreshold,
		ov.LoadEnabled, ov.LoadThreshold,
		ov.SilentEnabled, secondsPtrFromDuration(ov.SilentAfter),
	)
	if err != nil {
		return fmt.Errorf("host: upsert group threshold: %w", err)
	}
	return nil
}

// Delete удаляет override группы. Отсутствие строки — не ошибка (Delete
// идемпотентен, как принято в остальных стораджах продукта).
func (s *GroupThresholdService) Delete(ctx context.Context, projectID int64, scope, label string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM host_group_thresholds WHERE project_id = $1 AND scope = $2 AND label = $3`,
		projectID, scope, label)
	if err != nil {
		return fmt.Errorf("host: delete group threshold: %w", err)
	}
	return nil
}
