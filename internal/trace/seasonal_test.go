package trace_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// TestSeasonalBaselines поднимает один CH-контейнер и проверяет сезонные
// baseline-запросы: база — медиана p95/p75 по ТОМУ ЖЕ окну того же дня недели за
// прошлые недели (сдвиг на k·7 суток). Контейнер дорогой — один на все подтесты.
func TestSeasonalBaselines(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	const (
		pEndpoint   = int64(71) // сезонный endpoint: 3 недели + шум
		pSmall      = int64(72) // одна неделя истории (малый слот, для fallback T3)
		pVital      = int64(73) // сезонный vital (lcp)
		pClamp      = int64(74) // дискриминатор клампа окна (endpoint)
		pClampVital = int64(75) // дискриминатор клампа окна (vital)
	)

	w := trace.NewSpanWriter(conn)
	go w.Run()

	// Якорь now — полдень позавчера (UTC): заведомо в прошлом, секунды/минуты
	// кратны 5 (12:00), поэтому окна [now−k·7д−W, now−k·7д) и смещения −30/−50
	// мин ложатся ровно в 5-минутные бакеты MV без пограничного дребезга.
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

	// pEndpoint «GET /s»: то же окно [now−60м, now), сдвинутое на 1/2/3 недели.
	// Недельные p95 = [200, 210, 190] → медиана 200мс; всего 60 замеров слота.
	// Неделя-3 намеренно у ДАЛЬНЕЙ границы окна (−50 мин): проверяет, что нижняя
	// граница from включает −W (иначе самая старая неделя целиком отсекается).
	addEndpoint(pEndpoint, "GET /s", now.Add(-1*weekD).Add(-30*time.Minute), 200, 20, "s-w1")
	addEndpoint(pEndpoint, "GET /s", now.Add(-2*weekD).Add(-30*time.Minute), 210, 20, "s-w2")
	addEndpoint(pEndpoint, "GET /s", now.Add(-3*weekD).Add(-50*time.Minute), 190, 20, "s-w3")
	// Шум, который НЕ должен попасть в сезонную базу:
	//  - текущее окно k=0 (исключается WHERE k>=1);
	//  - тот же день недели, но другой час (−3ч → modulo вне окна);
	//  - другой день недели (−2 суток → modulo вне окна).
	addEndpoint(pEndpoint, "GET /s", now.Add(-30*time.Minute), 999, 20, "s-cur")
	addEndpoint(pEndpoint, "GET /s", now.Add(-1*weekD).Add(-3*time.Hour), 999, 20, "s-hour")
	addEndpoint(pEndpoint, "GET /s", now.Add(-2*24*time.Hour).Add(-30*time.Minute), 999, 20, "s-day")

	// pSmall «GET /s1»: только одна прошлая неделя → мало сэмплов слота.
	addEndpoint(pSmall, "GET /s1", now.Add(-1*weekD).Add(-30*time.Minute), 200, 15, "s1-w1")

	// pVital «GET /vs» (lcp): недельные p75 = [1000, 1100, 900] → медиана 1000;
	// 60 замеров слота. Плюс шум k=0 (lcp 5000) — не должен войти в базу.
	addVital(pVital, "GET /vs", now.Add(-1*weekD).Add(-30*time.Minute), 1000, 20, "vs-w1")
	addVital(pVital, "GET /vs", now.Add(-2*weekD).Add(-30*time.Minute), 1100, 20, "vs-w2")
	addVital(pVital, "GET /vs", now.Add(-3*weekD).Add(-50*time.Minute), 900, 20, "vs-w3")
	addVital(pVital, "GET /vs", now.Add(-30*time.Minute), 5000, 20, "vs-cur")

	// pClamp «GET /c»: слот 3 недель (200мс, 60 замеров) ПЛЮС «широкие» бакеты на
	// k=1 и k=2 с позицией-в-неделе 2 сут (>24ч, но та же неделя). При окне ≤1440
	// (кламп) они вне слота и в базу не входят → {60, 200}. Без клампа окно 20000
	// вырождает modulo(...)<winSec в always-true и втягивает их в недельный агрегат
	// → {100, ~900}. Дискриминатор: снятый кламп заставляет WindowClampEndpoint упасть.
	addEndpoint(pClamp, "GET /c", now.Add(-1*weekD).Add(-30*time.Minute), 200, 20, "c-w1")
	addEndpoint(pClamp, "GET /c", now.Add(-2*weekD).Add(-30*time.Minute), 200, 20, "c-w2")
	addEndpoint(pClamp, "GET /c", now.Add(-3*weekD).Add(-30*time.Minute), 200, 20, "c-w3")
	addEndpoint(pClamp, "GET /c", now.Add(-1*weekD).Add(-2*24*time.Hour), 900, 20, "c-wide1")
	addEndpoint(pClamp, "GET /c", now.Add(-2*weekD).Add(-2*24*time.Hour), 900, 20, "c-wide2")

	// pClampVital «GET /cv» (lcp): то же для vital — слот 1000мс + широкие 3000мс на
	// k=1/k=2. Кламп → {60, 1000}; без клампа → {100, ~3000}.
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
		// Samples = только слот трёх недель (20·3=60): шум k=0/другой час/другой
		// день не попал; старейшая неделя (k=3) не отсечена нижней границей.
		if s.Samples != 60 {
			t.Fatalf("Samples = %d, want 60 (шум исключён, старейшая неделя включена)", s.Samples)
		}
		// Медиана недельных p95 [200,210,190] = 200мс; шум 999 базу не сдвинул.
		assertNearF(t, "сезонный base p95 ms", s.Value, 200, 1)
	})

	t.Run("EndpointSmallSlot", func(t *testing.T) {
		out, err := q.SeasonalBaselineEndpointP95s(ctx, pSmall, []string{"GET /s1"}, 60, 3, now)
		if err != nil {
			t.Fatalf("SeasonalBaselineEndpointP95s small: %v", err)
		}
		s := out["GET /s1"]
		// Одна неделя истории → мало сэмплов (для fallback в T3): не паника, есть
		// значение, но Samples заведомо меньше «полного» слота.
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
		// Медиана недельных p75 [1000,1100,900] = 1000; шум k=0 (5000) не вошёл.
		assertNearF(t, "сезонный vital base p75", s.Value, 1000, 1)
	})

	t.Run("VitalUnknownMetric", func(t *testing.T) {
		if _, err := q.SeasonalBaselineVitalP75s(ctx, pVital, []string{"GET /vs"}, []string{"bogus"}, 60, 3, now); err == nil {
			t.Fatalf("неизвестная метрика: want error, got nil")
		}
	})

	// WindowClamp: window_minutes сверх максимума (слот шире суток) вырождал бы
	// фильтр modulo(...) < winSec в always-true — слот перестал бы сужать выборку и
	// запрос сканировал бы весь ретеншен. Кламп в запросе держит окно у́же недели
	// (≤1440 мин), поэтому абсурдно большое окно обязано давать РОВНО тот же
	// результат, что и максимум. Сид pClamp/pClampVital содержит «широкие» бакеты
	// (k≥1, позиция-в-неделе 2 сут), которые попадают в выборку ТОЛЬКО без клампа —
	// поэтому при снятом клампе окно 20000 расходится с 1440 (Samples 100 vs 60,
	// медиана ~900 vs 200) и тест падает. Дополнительно фиксируем ожидаемый
	// «слотовый» результат, чтобы тест не был замкнут сам на себя.
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
		// Клампнутый результат = только слот (широкие бакеты исключены): 60 замеров, медиана 200.
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
