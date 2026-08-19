package trace

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRegressionConfigFromJSON(t *testing.T) {
	def := DefaultRegressionConfig()

	cases := []struct {
		name    string
		raw     string
		want    RegressionConfig
		wantErr bool
	}{
		{name: "nil -> дефолты", raw: "", want: def},
		{name: "пустой объект -> дефолты", raw: `{}`, want: def},
		{name: "null -> дефолты", raw: `null`, want: def},
		{
			name: "все поля",
			raw: `{"threshold_pct":0.5,"recovery_pct":0.2,"window_minutes":30,` +
				`"min_samples":50,"duration_floor_ms":250,` +
				`"vital_floor":{"lcp":300,"fcp":300,"ttfb":300,"inp":80,"cls":0.1},"enabled":false}`,
			want: RegressionConfig{
				ThresholdPct: 0.5, RecoveryPct: 0.2, WindowMinutes: 30, MinSamples: 50,
				DurationFloorMs: 250,
				VitalFloor:      map[string]float64{"lcp": 300, "fcp": 300, "ttfb": 300, "inp": 80, "cls": 0.1},
				Enabled:         false,
				SeasonalEnabled: def.SeasonalEnabled, SeasonalWeeks: def.SeasonalWeeks,
			},
		},
		{
			// Отсутствующие ключи остаются дефолтными, заданные — перекрывают.
			name: "частичный конфиг: только threshold_pct",
			raw:  `{"threshold_pct":0.4}`,
			want: RegressionConfig{
				ThresholdPct: 0.4, RecoveryPct: def.RecoveryPct, WindowMinutes: def.WindowMinutes,
				MinSamples: def.MinSamples, DurationFloorMs: def.DurationFloorMs,
				VitalFloor: def.VitalFloor, Enabled: def.Enabled,
				SeasonalEnabled: def.SeasonalEnabled, SeasonalWeeks: def.SeasonalWeeks,
			},
		},
		{
			// enabled=false задан явно — не перезаписывается дефолтом true.
			name: "enabled=false явно",
			raw:  `{"enabled":false}`,
			want: RegressionConfig{
				ThresholdPct: def.ThresholdPct, RecoveryPct: def.RecoveryPct,
				WindowMinutes: def.WindowMinutes, MinSamples: def.MinSamples,
				DurationFloorMs: def.DurationFloorMs, VitalFloor: def.VitalFloor, Enabled: false,
				SeasonalEnabled: def.SeasonalEnabled, SeasonalWeeks: def.SeasonalWeeks,
			},
		},
		{
			// vital_floor перекрывает только заданные метрики, прочие — дефолт.
			name: "частичный vital_floor",
			raw:  `{"vital_floor":{"lcp":500}}`,
			want: RegressionConfig{
				ThresholdPct: def.ThresholdPct, RecoveryPct: def.RecoveryPct,
				WindowMinutes: def.WindowMinutes, MinSamples: def.MinSamples,
				DurationFloorMs: def.DurationFloorMs,
				VitalFloor:      map[string]float64{"lcp": 500, "fcp": 200, "ttfb": 200, "inp": 50, "cls": 0.05},
				Enabled:         def.Enabled,
				SeasonalEnabled: def.SeasonalEnabled, SeasonalWeeks: def.SeasonalWeeks,
			},
		},
		{name: "мусор -> ошибка", raw: `{`, want: def, wantErr: true},
		{name: "не объект -> ошибка", raw: `[1,2]`, want: def, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw []byte
			if tc.raw != "" {
				raw = []byte(tc.raw)
			}
			got, err := RegressionConfigFromJSON(raw)
			if tc.wantErr && err == nil {
				t.Fatal("ошибка ожидалась, получен nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("RegressionConfigFromJSON(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestSeasonalConfigDefaultsAndParse(t *testing.T) {
	d := DefaultRegressionConfig()
	if d.SeasonalEnabled != false || d.SeasonalWeeks != 4 {
		t.Fatalf("дефолты сезонности = %v/%d, want false/4", d.SeasonalEnabled, d.SeasonalWeeks)
	}
	// старый конфиг без полей → seasonal off, weeks=дефолт 4 (back-compat)
	c, err := RegressionConfigFromJSON([]byte(`{"threshold_pct":0.3}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.SeasonalEnabled != false || c.SeasonalWeeks != 4 {
		t.Fatalf("back-compat: seasonal=%v weeks=%d, want false/4", c.SeasonalEnabled, c.SeasonalWeeks)
	}
	// явные значения
	c2, _ := RegressionConfigFromJSON([]byte(`{"seasonal_enabled":true,"seasonal_weeks":6}`))
	if !c2.SeasonalEnabled || c2.SeasonalWeeks != 6 {
		t.Fatalf("explicit: seasonal=%v weeks=%d, want true/6", c2.SeasonalEnabled, c2.SeasonalWeeks)
	}
	// симметрия Marshal→Unmarshal
	b, _ := json.Marshal(c2)
	c3, _ := RegressionConfigFromJSON(b)
	if c3.SeasonalWeeks != 6 || !c3.SeasonalEnabled {
		t.Fatalf("roundtrip потерял сезонность: %+v", c3)
	}
	// защитный клампинг: битый jsonb вне [2,12] → дефолт 4
	c4, _ := RegressionConfigFromJSON([]byte(`{"seasonal_weeks":1}`))
	if c4.SeasonalWeeks != 4 {
		t.Fatalf("clamp <2: weeks=%d, want 4", c4.SeasonalWeeks)
	}
	c5, _ := RegressionConfigFromJSON([]byte(`{"seasonal_weeks":99}`))
	if c5.SeasonalWeeks != 4 {
		t.Fatalf("clamp >12: weeks=%d, want 4", c5.SeasonalWeeks)
	}
}

func TestRegressionConfigFloor(t *testing.T) {
	cfg := DefaultRegressionConfig()

	if got := cfg.Floor("duration"); got != 100 {
		t.Errorf(`Floor("duration") = %v, want 100`, got)
	}
	if got := cfg.Floor("lcp"); got != 200 {
		t.Errorf(`Floor("lcp") = %v, want 200`, got)
	}
	if got := cfg.Floor("inp"); got != 50 {
		t.Errorf(`Floor("inp") = %v, want 50`, got)
	}
	if got := cfg.Floor("cls"); got != 0.05 {
		t.Errorf(`Floor("cls") = %v, want 0.05`, got)
	}
	// Неизвестная метрика получает разумный дефолт, а не 0 (пол 0 = «срабатывать
	// всегда»).
	if got := cfg.Floor("unknown"); got <= 0 {
		t.Errorf(`Floor("unknown") = %v, want > 0`, got)
	}
}
