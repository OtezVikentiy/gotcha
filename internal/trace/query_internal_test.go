package trace

import "testing"

// TestMsSampleConvertsMicrosecondsToMilliseconds закрепляет единственную точку
// конвертации единиц: msSample делит на 1000 (dur в MV — микросекунды, детектор
// регрессий и его полы — миллисекунды). Без этого теста подмену msSample на
// valueSample в RecentEndpointP95s/BaselineEndpointP95s не ловит ничто на
// уровне пакета — см. интеграционный TestRecentEndpointP95sConvertsToMs рядом.
func TestMsSampleConvertsMicrosecondsToMilliseconds(t *testing.T) {
	got := msSample(1_000_000, 5)
	want := RegressionSample{Value: 1000, Samples: 5}
	if got != want {
		t.Fatalf("msSample(1_000_000, 5) = %+v, want %+v", got, want)
	}
}

// TestMsSampleZeroSamples — нулевые сэмплы дают Value 0, а не NaN: пустой
// quantilesMerge отдаёт NaN, а Decide/detектор с ним обращаться не должен.
func TestMsSampleZeroSamples(t *testing.T) {
	got := msSample(12345, 0)
	want := RegressionSample{Value: 0, Samples: 0}
	if got != want {
		t.Fatalf("msSample(12345, 0) = %+v, want %+v (NaN не должен просочиться)", got, want)
	}
}

// TestValueSampleNoConversion — valueSample для web-vital'ов не делит на 1000:
// они уже в миллисекундах (CLS безразмерный).
func TestValueSampleNoConversion(t *testing.T) {
	got := valueSample(1_000_000, 5)
	want := RegressionSample{Value: 1_000_000, Samples: 5}
	if got != want {
		t.Fatalf("valueSample(1_000_000, 5) = %+v, want %+v", got, want)
	}
}

// TestValueSampleZeroSamples — симметрично msSample: 0 сэмплов → Value 0.
func TestValueSampleZeroSamples(t *testing.T) {
	got := valueSample(12345, 0)
	want := RegressionSample{Value: 0, Samples: 0}
	if got != want {
		t.Fatalf("valueSample(12345, 0) = %+v, want %+v", got, want)
	}
}
