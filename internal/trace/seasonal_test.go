package trace_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

func TestSeasonalBaselines(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	const (
		pEndpoint   = int64(71)
		pSmall      = int64(72)
		pVital      = int64(73)
		pClamp      = int64(74)
		pClampVital = int64(75)
	)

	w := trace.NewSpanWriter(conn)
	go w.Run()

	now := time.Now().UTC().Truncate(24 * time.Hour).Add(-36 * time.Hour)
	const weekD = 7 * 24 * time.Hour

	addEndpoint := func(pid int64, name string, at time.Time, durMs, n int, tag string) {
		for i := 0; i < n; i++ {
			w.Add(pid, pid, trace.Transaction{
				TraceID:     fmt.Sprintf("%s-%03d", tag, i),
				SpanID:      fmt.Sprintf("%s-span-%03d", tag, i),
				Name:        name,
				Op:          "http.server",
				Status:      "ok",
				Start:       at,
				End:         at.Add(time.Duration(durMs) * time.Millisecond),
				Environment: "production",
			})
		}
	}
	addVital := func(pid int64, name string, at time.Time, lcp float64, n int, tag string) {
		for i := 0; i < n; i++ {
			w.Add(pid, pid, trace.Transaction{
				TraceID:      fmt.Sprintf("%s-%03d", tag, i),
				SpanID:       fmt.Sprintf("%s-span-%03d", tag, i),
				Name:         name,
				Op:           "pageload",
				Status:       "ok",
				Start:        at,
				End:          at.Add(time.Second),
				Environment:  "production",
				Measurements: map[string]float64{"lcp": lcp},
			})
		}
	}

	addEndpoint(pEndpoint, "GET /s", now.Add(-1*weekD).Add(-30*time.Minute), 200, 20, "s-w1")
	addEndpoint(pEndpoint, "GET /s", now.Add(-2*weekD).Add(-30*time.Minute), 210, 20, "s-w2")
	addEndpoint(pEndpoint, "GET /s", now.Add(-3*weekD).Add(-50*time.Minute), 190, 20, "s-w3")
	addEndpoint(pEndpoint, "GET /s", now.Add(-30*time.Minute), 999, 20, "s-cur")
	addEndpoint(pEndpoint, "GET /s", now.Add(-1*weekD).Add(-3*time.Hour), 999, 20, "s-hour")
	addEndpoint(pEndpoint, "GET /s", now.Add(-2*24*time.Hour).Add(-30*time.Minute), 999, 20, "s-day")

	addEndpoint(pSmall, "GET /s1", now.Add(-1*weekD).Add(-30*time.Minute), 200, 15, "s1-w1")

	addVital(pVital, "GET /vs", now.Add(-1*weekD).Add(-30*time.Minute), 1000, 20, "vs-w1")
	addVital(pVital, "GET /vs", now.Add(-2*weekD).Add(-30*time.Minute), 1100, 20, "vs-w2")
	addVital(pVital, "GET /vs", now.Add(-3*weekD).Add(-50*time.Minute), 900, 20, "vs-w3")
	addVital(pVital, "GET /vs", now.Add(-30*time.Minute), 5000, 20, "vs-cur")

	addEndpoint(pClamp, "GET /c", now.Add(-1*weekD).Add(-30*time.Minute), 200, 20, "c-w1")
	addEndpoint(pClamp, "GET /c", now.Add(-2*weekD).Add(-30*time.Minute), 200, 20, "c-w2")
	addEndpoint(pClamp, "GET /c", now.Add(-3*weekD).Add(-30*time.Minute), 200, 20, "c-w3")
	addEndpoint(pClamp, "GET /c", now.Add(-1*weekD).Add(-2*24*time.Hour), 900, 20, "c-wide1")
	addEndpoint(pClamp, "GET /c", now.Add(-2*weekD).Add(-2*24*time.Hour), 900, 20, "c-wide2")

	addVital(pClampVital, "GET /cv", now.Add(-1*weekD).Add(-30*time.Minute), 1000, 20, "cv-w1")
	addVital(pClampVital, "GET /cv", now.Add(-2*weekD).Add(-30*time.Minute), 1000, 20, "cv-w2")
	addVital(pClampVital, "GET /cv", now.Add(-3*weekD).Add(-30*time.Minute), 1000, 20, "cv-w3")
	addVital(pClampVital, "GET /cv", now.Add(-1*weekD).Add(-2*24*time.Hour), 3000, 20, "cv-wide1")
	addVital(pClampVital, "GET /cv", now.Add(-2*weekD).Add(-2*24*time.Hour), 3000, 20, "cv-wide2")

	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := w.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}

	q := trace.NewQuery(conn)

	t.Run("EndpointSlotMedian", func(t *testing.T) {
		out, err := q.SeasonalBaselineEndpointP95s(ctx, pEndpoint, []string{"GET /s"}, 60, 3, now)
		if err != nil {
			t.Fatalf("SeasonalBaselineEndpointP95s: %v", err)
		}
		s, ok := out["GET /s"]
		if !ok {
			t.Fatalf("нет ключа %q в %v", "GET /s", out)
		}
		if s.Samples != 60 {
			t.Fatalf("Samples = %d, want 60 (шум исключён, старейшая неделя включена)", s.Samples)
		}
		assertNearF(t, "сезонный base p95 ms", s.Value, 200, 1)
	})

	t.Run("EndpointSmallSlot", func(t *testing.T) {
		out, err := q.SeasonalBaselineEndpointP95s(ctx, pSmall, []string{"GET /s1"}, 60, 3, now)
		if err != nil {
			t.Fatalf("SeasonalBaselineEndpointP95s small: %v", err)
		}
		s := out["GET /s1"]
		if s.Samples != 15 {
			t.Fatalf("малый слот Samples = %d, want 15", s.Samples)
		}
		assertNearF(t, "малый слот base p95 ms", s.Value, 200, 1)
	})

	t.Run("EndpointEmptyList", func(t *testing.T) {
		out, err := q.SeasonalBaselineEndpointP95s(ctx, pEndpoint, nil, 60, 3, now)
		if err != nil {
			t.Fatalf("SeasonalBaselineEndpointP95s empty: %v", err)
		}
		if len(out) != 0 {
			t.Fatalf("пустой список целей → пустая карта, got %v", out)
		}
	})

	t.Run("VitalSlotMedian", func(t *testing.T) {
		out, err := q.SeasonalBaselineVitalP75s(ctx, pVital, []string{"GET /vs"}, []string{"lcp"}, 60, 3, now)
		if err != nil {
			t.Fatalf("SeasonalBaselineVitalP75s: %v", err)
		}
		s, ok := out[trace.VitalKey{Transaction: "GET /vs", Metric: "lcp"}]
		if !ok {
			t.Fatalf("нет ключа {GET /vs, lcp} в %v", out)
		}
		if s.Samples != 60 {
			t.Fatalf("vital Samples = %d, want 60", s.Samples)
		}
		assertNearF(t, "сезонный vital base p75", s.Value, 1000, 1)
	})

	t.Run("VitalUnknownMetric", func(t *testing.T) {
		if _, err := q.SeasonalBaselineVitalP75s(ctx, pVital, []string{"GET /vs"}, []string{"bogus"}, 60, 3, now); err == nil {
			t.Fatalf("неизвестная метрика: want error, got nil")
		}
	})

	t.Run("WindowClampEndpoint", func(t *testing.T) {
		atMax, err := q.SeasonalBaselineEndpointP95s(ctx, pClamp, []string{"GET /c"}, 1440, 3, now)
		if err != nil {
			t.Fatalf("endpoint at max window: %v", err)
		}
		huge, err := q.SeasonalBaselineEndpointP95s(ctx, pClamp, []string{"GET /c"}, 20000, 3, now)
		if err != nil {
			t.Fatalf("endpoint huge window: %v", err)
		}
		a, h := atMax["GET /c"], huge["GET /c"]
		if a.Samples != h.Samples || a.Value != h.Value {
			t.Fatalf("окно 20000 != окно 1440: {%d, %.1f} vs {%d, %.1f} — кламп не сработал",
				h.Samples, h.Value, a.Samples, a.Value)
		}
		if a.Samples != 60 {
			t.Fatalf("клампнутый Samples = %d, want 60 (широкие бакеты вне слота)", a.Samples)
		}
		assertNearF(t, "клампнутый base p95 ms", a.Value, 200, 1)
	})

	t.Run("WindowClampVital", func(t *testing.T) {
		key := trace.VitalKey{Transaction: "GET /cv", Metric: "lcp"}
		atMax, err := q.SeasonalBaselineVitalP75s(ctx, pClampVital, []string{"GET /cv"}, []string{"lcp"}, 1440, 3, now)
		if err != nil {
			t.Fatalf("vital at max window: %v", err)
		}
		huge, err := q.SeasonalBaselineVitalP75s(ctx, pClampVital, []string{"GET /cv"}, []string{"lcp"}, 20000, 3, now)
		if err != nil {
			t.Fatalf("vital huge window: %v", err)
		}
		a, h := atMax[key], huge[key]
		if a.Samples != h.Samples || a.Value != h.Value {
			t.Fatalf("vital окно 20000 != окно 1440: {%d, %.1f} vs {%d, %.1f} — кламп не сработал",
				h.Samples, h.Value, a.Samples, a.Value)
		}
		if a.Samples != 60 {
			t.Fatalf("vital клампнутый Samples = %d, want 60 (широкие бакеты вне слота)", a.Samples)
		}
		assertNearF(t, "vital клампнутый base p75", a.Value, 1000, 1)
	})
}
