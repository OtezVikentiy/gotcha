package trace

import (
	"encoding/json"
	"fmt"
)

// metricDuration — метрика эндпойнтной регрессии (p95 длительности транзакции).
// Прочие метрики — это web-vital'ы, у каждого свой абсолютный пол (см. Floor).
const metricDuration = "duration"

// defaultVitalFloor — пол для web-vital'а, которого нет в VitalFloor. Пол 0
// означал бы «срабатывать всегда» (любой рост над базой), поэтому для
// незнакомой метрики берём консервативные 200 (мс у ms-метрик) вместо нуля.
const defaultVitalFloor = 200

// Границы числа недель сезонной истории. Клампинг [2,12] строго валидирует UI,
// но при чтении битого jsonb значение вне диапазона заменяется дефолтом, чтобы
// оценщик не получил мусор.
const (
	defaultSeasonalWeeks = 4
	minSeasonalWeeks     = 2
	maxSeasonalWeeks     = 12
)

// RegressionConfig — пороги детектора регрессий, приезжают из
// projects.perf_regression_config. Это ДРУГОЙ механизм, чем DetectorConfig
// этапа 3 (N+1/медленные запросы): здесь отслеживается рост p95/p75 над
// скользящей базой.
// json-теги РОВНО совпадают с regressionConfigJSON: настройки проекта
// сохраняются через json.Marshal(RegressionConfig{...}) (см.
// projectSettingsRegressions в web), и Marshal обязан выдать те же ключи, что
// читает RegressionConfigFromJSON, — иначе опечатка молча перекрылась бы
// дефолтом (как json-теги DetectorConfig этапа 3).
type RegressionConfig struct {
	ThresholdPct    float64            `json:"threshold_pct"`     // открытие: recent > base×(1+ThresholdPct)
	RecoveryPct     float64            `json:"recovery_pct"`      // закрытие: recent ≤ base×(1+RecoveryPct); RecoveryPct < ThresholdPct (гистерезис)
	WindowMinutes   int                `json:"window_minutes"`    // размер свежего окна
	MinSamples      int                `json:"min_samples"`       // минимум сэмплов в окне, иначе решения нет
	DurationFloorMs float64            `json:"duration_floor_ms"` // абсолютный пол для метрики duration
	VitalFloor      map[string]float64 `json:"vital_floor"`       // абсолютные полы web-vital'ов: lcp/fcp/ttfb/inp/cls
	Enabled         bool               `json:"enabled"`           // выключенный проект не оценивается
	SeasonalEnabled bool               `json:"seasonal_enabled"`  // сезонный baseline: сравнивать с тем же окном того же дня недели за прошлые недели
	SeasonalWeeks   int                `json:"seasonal_weeks"`    // сколько прошлых недель берётся в сезонный слот (медиана); [2,12]
}

// DefaultRegressionConfig — дефолты из спеки (§6). Пол обязателен: +100% на
// 20→40 мс без него поднял бы ложную тревогу.
func DefaultRegressionConfig() RegressionConfig {
	return RegressionConfig{
		ThresholdPct:    0.25,
		RecoveryPct:     0.10,
		WindowMinutes:   60,
		MinSamples:      100,
		DurationFloorMs: 100,
		VitalFloor: map[string]float64{
			"lcp":  200,
			"fcp":  200,
			"ttfb": 200,
			"inp":  50,
			"cls":  0.05,
		},
		Enabled:         true,
		SeasonalEnabled: false,
		SeasonalWeeks:   defaultSeasonalWeeks,
	}
}

// regressionConfigJSON — промежуточное представление для разбора. Поля —
// указатели, чтобы отличить «ключ отсутствует» от «задан ноль»: у Enabled
// дефолт true, и обычный bool не дал бы отличить missing (должен стать true) от
// явного false.
type regressionConfigJSON struct {
	ThresholdPct    *float64           `json:"threshold_pct"`
	RecoveryPct     *float64           `json:"recovery_pct"`
	WindowMinutes   *int               `json:"window_minutes"`
	MinSamples      *int               `json:"min_samples"`
	DurationFloorMs *float64           `json:"duration_floor_ms"`
	VitalFloor      map[string]float64 `json:"vital_floor"`
	Enabled         *bool              `json:"enabled"`
	SeasonalEnabled *bool              `json:"seasonal_enabled"`
	SeasonalWeeks   *int               `json:"seasonal_weeks"`
}

// RegressionConfigFromJSON парсит projects.perf_regression_config. Пустой/nil
// вход — не ошибка (колонка по умолчанию '{}'), отсутствующий ключ заменяется
// дефолтом (как ConfigFromJSON этапа 3). vital_floor перекрывает только
// заданные метрики, остальные полы остаются дефолтными. При ошибке разбора
// возвращаются дефолты вместе с ошибкой: вызывающий может продолжить на них.
func RegressionConfigFromJSON(raw []byte) (RegressionConfig, error) {
	cfg := DefaultRegressionConfig()
	if len(raw) == 0 {
		return cfg, nil
	}
	var j regressionConfigJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return DefaultRegressionConfig(), fmt.Errorf("perf regression config: %w", err)
	}
	if j.ThresholdPct != nil {
		cfg.ThresholdPct = *j.ThresholdPct
	}
	if j.RecoveryPct != nil {
		cfg.RecoveryPct = *j.RecoveryPct
	}
	if j.WindowMinutes != nil {
		cfg.WindowMinutes = *j.WindowMinutes
	}
	if j.MinSamples != nil {
		cfg.MinSamples = *j.MinSamples
	}
	if j.DurationFloorMs != nil {
		cfg.DurationFloorMs = *j.DurationFloorMs
	}
	if j.Enabled != nil {
		cfg.Enabled = *j.Enabled
	}
	if j.SeasonalEnabled != nil {
		cfg.SeasonalEnabled = *j.SeasonalEnabled
	}
	// Защитно: из битого jsonb значение вне [2,12] → дефолт (строгую валидацию
	// диапазона делает UI при сохранении).
	if j.SeasonalWeeks != nil && *j.SeasonalWeeks >= minSeasonalWeeks && *j.SeasonalWeeks <= maxSeasonalWeeks {
		cfg.SeasonalWeeks = *j.SeasonalWeeks
	}
	// Перекрываем только заданные метрики: cfg.VitalFloor — свежая карта из
	// DefaultRegressionConfig, мутировать её безопасно.
	for k, v := range j.VitalFloor {
		cfg.VitalFloor[k] = v
	}
	return cfg, nil
}

// Floor — абсолютный пол для метрики: duration → DurationFloorMs; иначе
// VitalFloor[metric], а для незнакомой метрики — defaultVitalFloor (не 0).
func (c RegressionConfig) Floor(metric string) float64 {
	if metric == metricDuration {
		return c.DurationFloorMs
	}
	if f, ok := c.VitalFloor[metric]; ok {
		return f
	}
	return defaultVitalFloor
}
