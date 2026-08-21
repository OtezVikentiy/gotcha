package web

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/recipes"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// seedRecipePoint вставляет точку метрики рецепта напрямую в metric_points —
// с ПУСТЫМ host: метрики рецептов приходят без resourcedetection (реестр B6),
// и билдер обязан находить их через пустой-байпас host в query-слое (T2).
func seedRecipePoint(t *testing.T, ch driver.Conn, projectID int64, name, typ string, monotonic uint8, temporality string, ts time.Time, val float64, attrs map[string]string) {
	t.Helper()
	if attrs == nil {
		attrs = map[string]string{}
	}
	if err := ch.Exec(context.Background(), `
		INSERT INTO metric_points (project_id, name, type, unit, service, environment, host, attributes, ts, value, count, bucket_counts, explicit_bounds, monotonic, temporality)
		VALUES (?, ?, ?, '1', 'svc', 'prod', '', ?, ?, ?, 0, [], [], ?, ?)`,
		projectID, name, typ, attrs, ts, val, monotonic, temporality); err != nil {
		t.Fatalf("seed recipe point %s: %v", name, err)
	}
}

// chartByKey достаёт VM графика по ключу реестра — тесты не должны зависеть от
// порядка Charts в рецепте.
func chartByKey(t *testing.T, vms []templates.RecipeChartVM, key string) templates.RecipeChartVM {
	t.Helper()
	for _, vm := range vms {
		if vm.Key == key {
			return vm
		}
	}
	t.Fatalf("chart %q not found", key)
	panic("unreachable")
}

// TestRecipeChartsRedis — билдер по рецепту redis: VM на КАЖДЫЙ Chart реестра,
// скалярные и парные rate-ряды с данными не Empty (Legend пары — из i18n),
// незасеянные метрики — Empty; TitleKey/ExplorerURL собраны по контракту T4.
func TestRecipeChartsRedis(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	ch := testenv.MigratedCH(t)
	h := &Handler{Metrics: metric.NewQuery(ch)}
	ctx := context.Background()
	const pid = int64(880061)

	now := time.Now().UTC().Truncate(time.Minute)
	from, to := now.Add(-10*time.Minute), now.Add(time.Minute)

	// Скалярные: gauge память + не-monotonic sum клиентов.
	seedRecipePoint(t, ch, pid, "redis.memory.used", "gauge", 0, "", now.Add(-2*time.Minute), 1024, nil)
	seedRecipePoint(t, ch, pid, "redis.memory.used", "gauge", 0, "", now.Add(-time.Minute), 2048, nil)
	seedRecipePoint(t, ch, pid, "redis.clients.connected", "sum", 0, "cumulative", now.Add(-time.Minute), 5, nil)
	// Парный rate-график keyspace: monotonic cumulative, >=2 корзины на rate.
	for i, v := range []float64{100, 160, 220} {
		ts := now.Add(-time.Duration(3-i) * time.Minute)
		seedRecipePoint(t, ch, pid, "redis.keyspace.hits", "sum", 1, "cumulative", ts, v, nil)
		seedRecipePoint(t, ch, pid, "redis.keyspace.misses", "sum", 1, "cumulative", ts, v/2, nil)
	}

	rec, ok := recipes.ByID("redis")
	if !ok {
		t.Fatal("redis recipe not found")
	}
	vms := h.recipeCharts(ctx, pid, rec, from, to, time.Minute)
	if len(vms) != len(rec.Charts) {
		t.Fatalf("recipeCharts len = %d, want %d", len(vms), len(rec.Charts))
	}

	mem := chartByKey(t, vms, "memory")
	if mem.Empty {
		t.Error("memory chart Empty при засеянных точках")
	}
	if mem.TitleKey != "recipes.redis.chart.memory" {
		t.Errorf("memory TitleKey = %q, want recipes.redis.chart.memory", mem.TitleKey)
	}
	wantURL := "/projects/" + strconv.FormatInt(pid, 10) + "/metrics/redis.memory.used?agg=avg"
	if mem.ExplorerURL != wantURL {
		t.Errorf("memory ExplorerURL = %q, want %q", mem.ExplorerURL, wantURL)
	}

	if vm := chartByKey(t, vms, "clients"); vm.Empty {
		t.Error("clients chart Empty при засеянной точке")
	}
	keyspace := chartByKey(t, vms, "keyspace")
	if keyspace.Empty {
		t.Error("keyspace chart Empty при засеянных rate-точках")
	}
	if len(keyspace.Legend) != 2 {
		t.Fatalf("keyspace legend len = %d, want 2 (hits+misses)", len(keyspace.Legend))
	}

	// Незасеянные метрики рецепта — честный Empty, не паника и не мусор.
	for _, key := range []string{"commands", "fragmentation"} {
		if vm := chartByKey(t, vms, key); !vm.Empty {
			t.Errorf("%s chart not Empty без данных", key)
		}
	}
}

// TestRecipeChartsGrouped — обе групповые ветки билдера: SeriesGrouped
// (postgres backends по postgresql.database.name, скаляр) и SeriesGroupedRate
// (синтетический Chart c GroupKey+Rate; в реестре так устроены deadlocks/
// blocks_read postgres и network_rx/tx docker — синтетика оставлена, чтобы
// тест ветки не зависел от состава реестра). Легенда групповых рядов — сырые
// ключи групп.
func TestRecipeChartsGrouped(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	ch := testenv.MigratedCH(t)
	h := &Handler{Metrics: metric.NewQuery(ch)}
	ctx := context.Background()
	const pid = int64(880062)

	now := time.Now().UTC().Truncate(time.Minute)
	from, to := now.Add(-10*time.Minute), now.Add(time.Minute)

	for _, db := range []string{"app", "auth"} {
		seedRecipePoint(t, ch, pid, "postgresql.backends", "sum", 0, "cumulative",
			now.Add(-time.Minute), 7, map[string]string{"postgresql.database.name": db})
	}
	rec, ok := recipes.ByID("postgres")
	if !ok {
		t.Fatal("postgres recipe not found")
	}
	vms := h.recipeCharts(ctx, pid, rec, from, to, time.Minute)
	backends := chartByKey(t, vms, "backends")
	if backends.Empty {
		t.Fatal("backends chart Empty при засеянных группах")
	}
	labels := make([]string, 0, len(backends.Legend))
	for _, li := range backends.Legend {
		labels = append(labels, li.Label)
	}
	joined := strings.Join(labels, ",")
	if !strings.Contains(joined, "app") || !strings.Contains(joined, "auth") {
		t.Errorf("backends legend = %q, want raw group keys app+auth", joined)
	}

	// Групповой rate: monotonic счётчик двух контейнеров.
	for i, v := range []float64{100, 200, 300} {
		ts := now.Add(-time.Duration(3-i) * time.Minute)
		seedRecipePoint(t, ch, pid, "test.recipe.net", "sum", 1, "cumulative", ts, v, map[string]string{"container.name": "c1"})
		seedRecipePoint(t, ch, pid, "test.recipe.net", "sum", 1, "cumulative", ts, v*2, map[string]string{"container.name": "c2"})
	}
	synth := recipes.Recipe{ID: "docker", Charts: []recipes.Chart{{
		Key:      "network",
		Series:   []recipes.ChartSeries{{Metric: "test.recipe.net", Rate: true}},
		GroupKey: "container.name",
		Agg:      "avg",
	}}}
	vms = h.recipeCharts(ctx, pid, synth, from, to, time.Minute)
	if len(vms) != 1 {
		t.Fatalf("synthetic recipeCharts len = %d, want 1", len(vms))
	}
	if vms[0].Empty {
		t.Fatal("grouped-rate chart Empty при засеянных счётчиках")
	}
	if len(vms[0].Legend) != 2 {
		t.Errorf("grouped-rate legend len = %d, want 2 (c1+c2)", len(vms[0].Legend))
	}
}

// TestRecipeChartsNoData — рецепт без единой точки: столько же VM, все Empty.
func TestRecipeChartsNoData(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	ch := testenv.MigratedCH(t)
	h := &Handler{Metrics: metric.NewQuery(ch)}
	now := time.Now().UTC().Truncate(time.Minute)

	rec, ok := recipes.ByID("nginx")
	if !ok {
		t.Fatal("nginx recipe not found")
	}
	vms := h.recipeCharts(context.Background(), 880063, rec, now.Add(-10*time.Minute), now, time.Minute)
	if len(vms) != len(rec.Charts) {
		t.Fatalf("recipeCharts len = %d, want %d", len(vms), len(rec.Charts))
	}
	for _, vm := range vms {
		if !vm.Empty {
			t.Errorf("chart %s not Empty без данных", vm.Key)
		}
	}
}

// TestRecipeChartsQueryError — ошибка query (отменённый контекст) роняет
// ТОЛЬКО график в Empty, не страницу: билдер возвращает полный набор VM.
func TestRecipeChartsQueryError(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	ch := testenv.MigratedCH(t)
	h := &Handler{Metrics: metric.NewQuery(ch)}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now := time.Now().UTC().Truncate(time.Minute)
	rec, ok := recipes.ByID("redis")
	if !ok {
		t.Fatal("redis recipe not found")
	}
	vms := h.recipeCharts(ctx, 880064, rec, now.Add(-10*time.Minute), now, time.Minute)
	if len(vms) != len(rec.Charts) {
		t.Fatalf("recipeCharts len = %d, want %d (страница не падает)", len(vms), len(rec.Charts))
	}
	for _, vm := range vms {
		if !vm.Empty {
			t.Errorf("chart %s not Empty при ошибке query", vm.Key)
		}
	}
}

// TestRecipeChartVMEmptySeriesGuard — гвард синтетического Chart без Series:
// честный Empty до первого обращения к h.Metrics (без него Series[0] в
// recipeExplorerURL паниковала бы). Чистый unit — контейнер не нужен.
func TestRecipeChartVMEmptySeriesGuard(t *testing.T) {
	rec := recipes.Recipe{ID: "synth"}
	now := time.Now()
	vm := (&Handler{}).recipeChartVM(context.Background(), 1, rec,
		recipes.Chart{Key: "hollow"}, nil, now.Add(-time.Hour), now, time.Minute)
	if !vm.Empty {
		t.Error("Chart без Series должен давать Empty VM, а не панику/запрос")
	}
	if vm.Key != "hollow" || vm.TitleKey != "recipes.synth.chart.hollow" {
		t.Errorf("Empty VM должен сохранять Key/TitleKey, got %q/%q", vm.Key, vm.TitleKey)
	}
	if vm.ExplorerURL != "" {
		t.Errorf("у Chart без Series нет первой метрики — ExplorerURL должен быть пуст, got %q", vm.ExplorerURL)
	}
}

// TestRecipeExplorerURL — ссылка «открыть в метриках»: с агрегацией — хвост
// ?agg=, без неё (в реестре таких сегодня нет — ветка для синтетики) — чистый
// адрес метрики.
func TestRecipeExplorerURL(t *testing.T) {
	chart := recipes.Chart{Series: []recipes.ChartSeries{{Metric: "redis.memory.used"}}, Agg: "avg"}
	if got, want := recipeExplorerURL(7, chart), "/projects/7/metrics/redis.memory.used?agg=avg"; got != want {
		t.Errorf("recipeExplorerURL c Agg = %q, want %q", got, want)
	}
	chart.Agg = ""
	if got, want := recipeExplorerURL(7, chart), "/projects/7/metrics/redis.memory.used"; got != want {
		t.Errorf("recipeExplorerURL без Agg = %q, want %q", got, want)
	}
}

// TestRecipeChartsDataArrives — детекция «данные приходят» (§7.3): true при
// свежей сигнатурной метрике, false без неё и false вовсе без h.Metrics.
func TestRecipeChartsDataArrives(t *testing.T) {
	if testing.Short() {
		t.Skip("requires clickhouse container")
	}
	ch := testenv.MigratedCH(t)
	h := &Handler{Metrics: metric.NewQuery(ch)}
	ctx := context.Background()
	const pid = int64(880065)

	rec, ok := recipes.ByID("redis")
	if !ok {
		t.Fatal("redis recipe not found")
	}
	if h.recipeDataArrives(ctx, pid, rec) {
		t.Error("dataArrives = true без единой точки")
	}
	seedRecipePoint(t, ch, pid, rec.Signature, "sum", 0, "cumulative", time.Now().UTC().Add(-time.Minute), 3, nil)
	if !h.recipeDataArrives(ctx, pid, rec) {
		t.Error("dataArrives = false при свежей сигнатурной метрике")
	}
	if h.recipeDataArrives(ctx, pid+1, rec) {
		t.Error("dataArrives = true для чужого проекта")
	}
	if (&Handler{}).recipeDataArrives(ctx, pid, rec) {
		t.Error("dataArrives = true без h.Metrics")
	}
}
