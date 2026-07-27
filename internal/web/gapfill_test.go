package web

import (
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
)

func TestTruncStepEpoch(t *testing.T) {
	step := time.Hour
	// 00:45 → 00:00 (выравнивание к эпохе, не к нулевому времени Go).
	got := truncStepEpoch(time.Date(2026, 7, 1, 0, 45, 0, 0, time.UTC), step)
	want := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("truncStepEpoch = %v, want %v", got, want)
	}
	// step<=0 → без изменений.
	if v := truncStepEpoch(want, 0); !v.Equal(want) {
		t.Errorf("step=0 should return input, got %v", v)
	}
}

func TestFillSeries(t *testing.T) {
	step := time.Hour
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(5 * time.Hour)
	// данные только в 2 из 5 корзин.
	src := []event.Point{
		{T: from.Add(1 * time.Hour), N: 3},
		{T: from.Add(3 * time.Hour), N: 7},
	}
	out := fillSeries(src, from, to, step,
		func(p event.Point) time.Time { return p.T },
		func(tt time.Time) event.Point { return event.Point{T: tt} })

	// Ожидаем сетку [0..5] по часу — 6 корзин, покрывающих всё окно.
	if len(out) != 6 {
		t.Fatalf("len(out) = %d, want 6\n%+v", len(out), out)
	}
	// Первая корзина — на границе окна, а не там, где начались данные.
	if !out[0].T.Equal(from) {
		t.Errorf("out[0].T = %v, want %v (окно, не данные)", out[0].T, from)
	}
	// Существующие значения сохранены на своих местах.
	if out[1].N != 3 || out[3].N != 7 {
		t.Errorf("данные не на местах: %+v", out)
	}
	// Пропуски — нулевые.
	if out[0].N != 0 || out[2].N != 0 || out[4].N != 0 {
		t.Errorf("пропуски должны быть нулевыми: %+v", out)
	}
}

func TestFillSeriesGuards(t *testing.T) {
	src := []event.Point{{T: time.Unix(100, 0), N: 1}}
	at := func(p event.Point) time.Time { return p.T }
	gap := func(tt time.Time) event.Point { return event.Point{T: tt} }
	from := time.Unix(0, 0)
	to := from.Add(time.Hour)

	// step<=0 → src без изменений.
	if out := fillSeries(src, from, to, 0, at, gap); len(out) != 1 {
		t.Errorf("step=0 should return src, got %d", len(out))
	}
	// from не раньше to → src.
	if out := fillSeries(src, to, from, time.Minute, at, gap); len(out) != 1 {
		t.Errorf("from>=to should return src, got %d", len(out))
	}
	// абсурдно мелкий шаг на большом окне → защита возвращает src (без OOM).
	huge := from.Add(1000 * time.Hour)
	if out := fillSeries(src, from, huge, time.Second, at, gap); len(out) != 1 {
		t.Errorf("pathological grid should return src, got %d", len(out))
	}
}
