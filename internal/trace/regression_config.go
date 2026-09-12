package trace

import (
	"encoding/json"
	"fmt"
)

const metricDuration = "duration"

// 0 означало бы «срабатывать всегда»; для незнакомой метрики берём
// консервативные 200 вместо нуля.
const defaultVitalFloor = 200

// клампинг [2,12] здесь — защита от битого jsonb; строгую валидацию диапазона
// делает UI при сохранении.
const (
	defaultSeasonalWeeks = 4
	minSeasonalWeeks     = 2
	maxSeasonalWeeks     = 12
)

// json-теги должны РОВНО совпадать с regressionConfigJSON — иначе опечатка в
// Marshal молча перекроется дефолтом при чтении.
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

// пол обязателен: +100% на 20→40 мс без него поднял бы ложную тревогу.
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

// поля — указатели, чтобы отличить «ключ отсутствует» от «явный ноль/false»
// (у Enabled дефолт true).
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

// пустой/nil вход — не ошибка; при ошибке разбора возвращаются дефолты вместе
// с ошибкой, вызывающий может продолжить на них.
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

func (c RegressionConfig) Floor(metric string) float64 {
	if metric == metricDuration {
		return c.DurationFloorMs
	}
	if f, ok := c.VitalFloor[metric]; ok {
		return f
	}
	return defaultVitalFloor
}
