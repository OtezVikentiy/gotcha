package profile

import (
	"bytes"
	"testing"
	"time"

	pp "github.com/google/pprof/profile"
)

// TestParsePprofKeepsUnit — единица измерения значения выборки сохраняется из
// pprof SampleType.Unit. Раньше она отбрасывалась, и UI угадывал её по имени
// типа профиля: для нестандартных типов такая догадка не работает, а для
// alloc-профилей выбор типа меняет и единицу (объекты — count, объём — bytes).
func TestParsePprofKeepsUnit(t *testing.T) {
	fn := &pp.Function{ID: 1, Name: "main", Filename: "m.go"}
	loc := &pp.Location{ID: 1, Line: []pp.Line{{Function: fn, Line: 10}}}

	cases := []struct {
		name     string
		types    []*pp.ValueType
		selected string
		wantType string
		wantUnit string
	}{
		{
			name:     "cpu в наносекундах",
			types:    []*pp.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
			selected: "cpu", wantType: "cpu", wantUnit: "nanoseconds",
		},
		{
			name:     "alloc: выбран объём — единица bytes",
			types:    []*pp.ValueType{{Type: "alloc_objects", Unit: "count"}, {Type: "alloc_space", Unit: "bytes"}},
			selected: "alloc_space", wantType: "alloc_space", wantUnit: "bytes",
		},
		{
			name:     "alloc: выбраны объекты — единица count",
			types:    []*pp.ValueType{{Type: "alloc_objects", Unit: "count"}, {Type: "alloc_space", Unit: "bytes"}},
			selected: "alloc_objects", wantType: "alloc_objects", wantUnit: "count",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			values := make([]int64, len(c.types))
			for i := range values {
				values[i] = 7
			}
			prof := &pp.Profile{
				SampleType: c.types,
				Function:   []*pp.Function{fn},
				Location:   []*pp.Location{loc},
				Sample:     []*pp.Sample{{Location: []*pp.Location{loc}, Value: values}},
			}
			var buf bytes.Buffer
			if err := prof.Write(&buf); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := ParsePprof(buf.Bytes(), c.selected, time.Now())
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got.Type != c.wantType || got.Unit != c.wantUnit {
				t.Errorf("тип/единица = %q/%q, want %q/%q", got.Type, got.Unit, c.wantType, c.wantUnit)
			}
		})
	}
}

// TestPctIncrease закрывает обе ветки доли роста: при base<=0 сравнивать не с
// чем — возвращается 0 (иначе было бы деление на ноль); при base>0 — доля
// относительного прироста recent над base.
func TestPctIncrease(t *testing.T) {
	cases := []struct {
		base, recent, want float64
	}{
		{0, 0.8, 0},     // base<=0 → 0
		{-1, 0.8, 0},    // отрицательная база тоже 0
		{0.1, 0.2, 1.0}, // +100%
		{0.5, 0.5, 0},   // без изменений
		{0.4, 0.2, -0.5},
	}
	for _, c := range cases {
		if got := pctIncrease(c.base, c.recent); got != c.want {
			t.Errorf("pctIncrease(%v,%v) = %v, want %v", c.base, c.recent, got, c.want)
		}
	}
}
