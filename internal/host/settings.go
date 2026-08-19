package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MinSilentAfter — минимальный порог «тишины» хоста: инвариант ≥3×
// троттлинга регистрации хостов на приёме (Toucher, every=60с, см. touch.go).
// На меньших значениях живой хост с редким экспортом метрик ловил бы ложный
// silent-инцидент из-за устаревшего last_seen — Toucher обновляет его не
// чаще раза в 60с, поэтому у порога должен быть запас в несколько таких
// интервалов, а не строго больше одного.
const MinSilentAfter = 180 * time.Second

// MaxSilentAfter — верхняя граница порога «тишины». Смысловая: оценщик
// рассматривает только хосты, чей last_seen свежее суток (freshWithin в
// evaluator.go), поэтому порог, сравнимый с этим окном, не сработал бы никогда
// — молчащий хост выпал бы из выборки раньше, чем накопил бы столько тишины.
// Граница нужна и технически: без неё введённые в форму 10^12 минут
// переполняли и time.Duration, и колонку int4, превращая опечатку в 500-ю
// вместо честного «поле заполнено не так».
const MaxSilentAfter = 12 * time.Hour

// Settings — пороги встроенных инцидентов хоста одного проекта (диск/память/
// нагрузка/тишина). Диск и память хранятся долями (0..1] — конверсия в
// проценты на границе веба.
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

// DefaultSettings — встроенный набор по умолчанию (§4.1 дизайна): диск/
// память >90%, load >2.0 на ядро, тишина >5 минут.
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

// KindEnabled — оценивается ли порог вида kind (Kinds) при этих настройках:
// тот же предикат, по которому Evaluator.Tick решает, звать ли evalDisk/
// evalMemory/evalLoad, а evalSilent — выходить ли первой строкой.
//
// ok=false для незнакомого вида (как thresholdFor в notify.go): вызывающий,
// который на «выключено» совершает действие над инцидентами (web:
// hostSettingsSave закрывает открытые инциденты выключенных видов), не должен
// принимать неизвестный вид за выключенный и трогать его. Сторож —
// TestKindEnabledKnowsEveryKind: новый вид в Kinds обязан появиться и здесь.
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

// Границы валидации — различимые ошибки, чтобы вызывающий (FormState) знал,
// какое конкретно поле подсветить.
var (
	ErrInvalidDiskThreshold   = errors.New("host: disk threshold must be in (0, 1]")
	ErrInvalidMemoryThreshold = errors.New("host: memory threshold must be in (0, 1]")
	ErrInvalidLoadThreshold   = errors.New("host: load threshold must be > 0")
	ErrInvalidSilentAfter     = errors.New("host: silent after must be between 180s and 12h")
)

// Validate проверяет Settings на те же границы, что и CHECK'и миграции
// 0065 (плюс MinSilentAfter — семантический инвариант, не выразимый в
// CHECK на секундах без потери читаемости). Значения ВЫКЛЮЧЕННЫХ порогов
// (Enabled=false) проверяются наравне с включёнными: сохранённое, но
// временно выключенное значение должно быть валидным само по себе — иначе
// включение порога назад без повторного ввода тихо активирует мусор.
func Validate(s Settings) error {
	if s.DiskThreshold <= 0 || s.DiskThreshold > 1 {
		return fmt.Errorf("%w: got %v", ErrInvalidDiskThreshold, s.DiskThreshold)
	}
	if s.MemoryThreshold <= 0 || s.MemoryThreshold > 1 {
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

// SettingsService — Get/Save порогов хоста поверх host_threshold_settings.
type SettingsService struct {
	pool *pgxpool.Pool
}

func NewSettingsService(pool *pgxpool.Pool) *SettingsService {
	return &SettingsService{pool: pool}
}

// GetWithExists возвращает пороги проекта и признак, есть ли для проекта
// сохранённая строка (M2: нужен вызывающим, различающим «явно не настроено»
// от «настроено и совпало с дефолтом» — например, каскаду override/group/
// project/default, которому важно, останавливаться ли на уровне проекта).
// Строки нет (порог ещё не сохранялся) — не ошибка: DefaultSettings() и
// exists=false, строка создаётся лениво, только при первом Save.
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

// Get возвращает пороги проекта — обёртка над GetWithExists, отбрасывающая
// признак наличия строки для вызывающих, которым он не нужен.
func (s *SettingsService) Get(ctx context.Context, projectID int64) (Settings, error) {
	out, _, err := s.GetWithExists(ctx, projectID)
	return out, err
}

// Save валидирует и сохраняет пороги проекта (upsert — первый Save проекта
// создаёт строку, последующие обновляют её и updated_at).
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
