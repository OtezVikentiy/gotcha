package trace

import (
	"math"
	"testing"
)

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

// TestApdexBoundsUSNormalThreshold — обычный порог (пример: 300мс) даёт
// satUS/tolUS без клампа, ровно T·1000 и 4·T·1000.
func TestApdexBoundsUSNormalThreshold(t *testing.T) {
	satUS, tolUS := apdexBoundsUS(300)
	if satUS != 300_000 || tolUS != 1_200_000 {
		t.Fatalf("apdexBoundsUS(300) = (%d, %d), want (300000, 1200000)", satUS, tolUS)
	}
}

// TestApdexBoundsUSClampsOnOverflow — apdex_threshold_ms без верхней границы
// в валидации (projsettings.go: только >0) мог довести uint32-арифметику до
// переполнения и дать МУСОРНОЕ (обёрнутое) значение вместо клампа (P2-6 из
// аудита 2026-08-12). Проверяем: результат либо ровно T·1000/4·T·1000 (если
// влезает), либо math.MaxUint32 — но никогда не обёрнутый в 0/маленькое
// число мусор.
func TestApdexBoundsUSClampsOnOverflow(t *testing.T) {
	cases := []int{
		4_294_967,     // чуть ниже точки переполнения satUS
		4_294_968,     // сразу за точкой переполнения satUS (~4.29M мс из находки)
		10_000_000,    // с большим запасом
		math.MaxInt32, // максимум, который вообще проходит ParseInt(..., 32)
	}
	for _, apdexT := range cases {
		satUS, tolUS := apdexBoundsUS(apdexT)
		if satUS > math.MaxUint32 {
			t.Fatalf("apdexBoundsUS(%d): satUS=%d превышает uint32", apdexT, satUS)
		}
		if tolUS > math.MaxUint32 {
			t.Fatalf("apdexBoundsUS(%d): tolUS=%d превышает uint32", apdexT, tolUS)
		}
		wantSat := uint64(apdexT) * 1000
		if wantSat > math.MaxUint32 {
			wantSat = math.MaxUint32
		}
		if uint64(satUS) != wantSat {
			t.Fatalf("apdexBoundsUS(%d): satUS = %d, want %d (клампленное, не обёрнутое)", apdexT, satUS, wantSat)
		}
		if tolUS < satUS {
			t.Fatalf("apdexBoundsUS(%d): tolUS=%d < satUS=%d — похоже на переполнение", apdexT, tolUS, satUS)
		}
	}
}
