package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ≥3× троттлинга Toucher (60с) — иначе живой хост ловит ложный silent из-за устаревшего last_seen.
const MinSilentAfter = 180 * time.Second

// Меньше суток (freshWithin оценщика) с запасом — иначе хост выпадет из выборки раньше,
// чем накопит порог тишины; заодно ограничивает переполнение Duration/int4 в форме.
const MaxSilentAfter = 12 * time.Hour

// Диск и память хранятся долями (0..1], конверсия в проценты — на границе веба.
type Settings struct {
	DiskEnabled     bool
	DiskThreshold   float64
	MemoryEnabled   bool
	MemoryThreshold float64
	LoadEnabled     bool
	LoadThreshold   float64
	SilentEnabled   bool
	SilentAfter     time.Duration
}

func DefaultSettings() Settings {
	return Settings{
		DiskEnabled:     true,
		DiskThreshold:   0.90,
		MemoryEnabled:   true,
		MemoryThreshold: 0.90,
		LoadEnabled:     true,
		LoadThreshold:   2.0,
		SilentEnabled:   true,
		SilentAfter:     5 * time.Minute,
	}
}

// ok=false для незнакомого вида: вызывающий не должен принимать «неизвестно» за «выключено» и
// действовать над его инцидентами (например, закрывать их при выключении в hostSettingsSave).
func (s Settings) KindEnabled(kind string) (enabled, ok bool) {
	switch kind {
	case "disk":
		return s.DiskEnabled, true
	case "memory":
		return s.MemoryEnabled, true
	case "load":
		return s.LoadEnabled, true
	case "silent":
		return s.SilentEnabled, true
	default:
		return false, false
	}
}

// Различимые ошибки — чтобы вызывающий (FormState) знал, какое поле подсветить.
var (
	ErrInvalidDiskThreshold   = errors.New("host: disk threshold must be in (0, 1)")
	ErrInvalidMemoryThreshold = errors.New("host: memory threshold must be in (0, 1)")
	ErrInvalidLoadThreshold   = errors.New("host: load threshold must be > 0")
	ErrInvalidSilentAfter     = errors.New("host: silent after must be between 180s and 12h")
)

// Диск/память строго (0,1): applyDecision сравнивает через строгое «>», и 1.0 было бы мёртвым порогом.
// Значения выключенных порогов проверяются наравне — иначе включение обратно тихо активирует мусор.
func Validate(s Settings) error {
	if s.DiskThreshold <= 0 || s.DiskThreshold >= 1 {
		return fmt.Errorf("%w: got %v", ErrInvalidDiskThreshold, s.DiskThreshold)
	}
	if s.MemoryThreshold <= 0 || s.MemoryThreshold >= 1 {
		return fmt.Errorf("%w: got %v", ErrInvalidMemoryThreshold, s.MemoryThreshold)
	}
	if s.LoadThreshold <= 0 {
		return fmt.Errorf("%w: got %v", ErrInvalidLoadThreshold, s.LoadThreshold)
	}
	if s.SilentAfter < MinSilentAfter || s.SilentAfter > MaxSilentAfter {
		return fmt.Errorf("%w: got %v", ErrInvalidSilentAfter, s.SilentAfter)
	}
	return nil
}

type SettingsService struct {
	pool *pgxpool.Pool
}

func NewSettingsService(pool *pgxpool.Pool) *SettingsService {
	return &SettingsService{pool: pool}
}

// exists различает «не настроено» от «совпало с дефолтом» — важно каскаду override/group/project.
// Строки нет — не ошибка: DefaultSettings() и exists=false; строка создаётся лениво при первом Save.
func (s *SettingsService) GetWithExists(ctx context.Context, projectID int64) (Settings, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT disk_enabled, disk_threshold, memory_enabled, memory_threshold,
		       load_enabled, load_threshold, silent_enabled, silent_after_seconds
		FROM host_threshold_settings
		WHERE project_id = $1`, projectID)

	var out Settings
	var silentSeconds int
	err := row.Scan(
		&out.DiskEnabled, &out.DiskThreshold,
		&out.MemoryEnabled, &out.MemoryThreshold,
		&out.LoadEnabled, &out.LoadThreshold,
		&out.SilentEnabled, &silentSeconds,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultSettings(), false, nil
	}
	if err != nil {
		return Settings{}, false, fmt.Errorf("host: get settings: %w", err)
	}
	out.SilentAfter = time.Duration(silentSeconds) * time.Second
	return out, true, nil
}

func (s *SettingsService) Get(ctx context.Context, projectID int64) (Settings, error) {
	out, _, err := s.GetWithExists(ctx, projectID)
	return out, err
}

func (s *SettingsService) Save(ctx context.Context, projectID int64, settings Settings) error {
	if err := Validate(settings); err != nil {
		return err
	}
	silentSeconds := int(settings.SilentAfter / time.Second)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO host_threshold_settings (
			project_id, disk_enabled, disk_threshold, memory_enabled, memory_threshold,
			load_enabled, load_threshold, silent_enabled, silent_after_seconds, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
		ON CONFLICT (project_id) DO UPDATE SET
			disk_enabled = $2, disk_threshold = $3,
			memory_enabled = $4, memory_threshold = $5,
			load_enabled = $6, load_threshold = $7,
			silent_enabled = $8, silent_after_seconds = $9,
			updated_at = now()`,
		projectID,
		settings.DiskEnabled, settings.DiskThreshold,
		settings.MemoryEnabled, settings.MemoryThreshold,
		settings.LoadEnabled, settings.LoadThreshold,
		settings.SilentEnabled, silentSeconds,
	)
	if err != nil {
		return fmt.Errorf("host: save settings: %w", err)
	}
	return nil
}
