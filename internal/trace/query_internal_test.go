package trace

import (
	"math"
	"testing"
)

func TestMsSampleConvertsMicrosecondsToMilliseconds(t *testing.T) {
	got := msSample(1_000_000, 5)
	want := RegressionSample{Value: 1000, Samples: 5}
	if got != want {
		t.Fatalf("msSample(1_000_000, 5) = %+v, want %+v", got, want)
	}
}

func TestMsSampleZeroSamples(t *testing.T) {
	got := msSample(12345, 0)
	want := RegressionSample{Value: 0, Samples: 0}
	if got != want {
		t.Fatalf("msSample(12345, 0) = %+v, want %+v (NaN не должен просочиться)", got, want)
	}
}

func TestValueSampleNoConversion(t *testing.T) {
	got := valueSample(1_000_000, 5)
	want := RegressionSample{Value: 1_000_000, Samples: 5}
	if got != want {
		t.Fatalf("valueSample(1_000_000, 5) = %+v, want %+v", got, want)
	}
}

func TestValueSampleZeroSamples(t *testing.T) {
	got := valueSample(12345, 0)
	want := RegressionSample{Value: 0, Samples: 0}
	if got != want {
		t.Fatalf("valueSample(12345, 0) = %+v, want %+v", got, want)
	}
}

func TestApdexBoundsUSNormalThreshold(t *testing.T) {
	satUS, tolUS := apdexBoundsUS(300)
	if satUS != 300_000 || tolUS != 1_200_000 {
		t.Fatalf("apdexBoundsUS(300) = (%d, %d), want (300000, 1200000)", satUS, tolUS)
	}
}

func TestApdexBoundsUSClampsOnOverflow(t *testing.T) {
	cases := []int{
		4_294_967,     // чуть ниже точки переполнения satUS
		4_294_968,     // сразу за точкой переполнения satUS (~4.29M мс)
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
