package web

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
)

func somePoints() []metric.Point {
	base := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	pts := make([]metric.Point, 6)
	for i := range pts {
		pts[i] = metric.Point{T: base.Add(time.Duration(i) * 10 * time.Minute), V: float64(i * 3)}
	}
	return pts
}

func TestMultiSeriesMarkupClassesAndCap(t *testing.T) {
	series := make([]NamedSeries, 10)
	for i := range series {
		series[i] = NamedSeries{Label: fmt.Sprintf("s%d", i), Points: somePoints()}
	}
	svg := multiSeriesMarkup(context.Background(), series, "", nil, nil, 720, 200)
	if !strings.Contains(svg, "series-m1") || !strings.Contains(svg, "series-m8") {
		t.Fatal("classes missing")
	}
	if strings.Contains(svg, "series-m9") {
		t.Fatal("more than 8 series rendered")
	}
}

func TestMultiSeriesMarkupEmpty(t *testing.T) {
	out := multiSeriesMarkup(context.Background(), nil, "", nil, nil, 720, 200)
	if !strings.Contains(out, "нет данных") {
		t.Errorf("пустой мульти-серийный график должен отмечать отсутствие данных: %s", out)
	}

	out = multiSeriesMarkup(context.Background(), []NamedSeries{{Label: "a", Points: nil}}, "", nil, nil, 720, 200)
	if !strings.Contains(out, "нет данных") {
		t.Errorf("серия без точек тоже должна давать «нет данных»: %s", out)
	}
}

func TestMultiSeriesMarkupNaNGap(t *testing.T) {
	base := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	points := []metric.Point{
		{T: base, V: 10},
		{T: base.Add(10 * time.Minute), V: math.NaN()},
		{T: base.Add(20 * time.Minute), V: 20},
		{T: base.Add(30 * time.Minute), V: 25},
	}
	series := []NamedSeries{{Label: "cpu", Points: points}}
	out := multiSeriesMarkup(context.Background(), series, "%", nil, nil, 720, 200)
	if !strings.Contains(out, "series-m1") {
		t.Fatal("класс первой серии отсутствует")
	}
	if !strings.Contains(out, `<polyline`) {
		t.Fatal("нет ни одной линии данных")
	}
}

func TestMultiSeriesMarkupThresholds(t *testing.T) {
	series := []NamedSeries{{Label: "cpu", Points: somePoints()}}
	out := multiSeriesMarkup(context.Background(), series, "", []metricThreshold{{Value: 6, Comparator: "gt"}}, nil, 720, 200)
	if !strings.Contains(out, "chart-threshold") || !strings.Contains(out, "stroke-dasharray") {
		t.Errorf("порог не нарисован: %s", out)
	}
}
