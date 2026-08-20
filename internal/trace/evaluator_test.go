package trace

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestEvaluatorLifecycle прогоняет полный жизненный цикл регрессии через
// tick оценщика на живых PG+CH: стабильная база ~800 мс за неделю, свежий скачок
// до 1200 мс → открытие инцидента ровно один раз и ровно одна задача в outbox
// (notified_open); повторный tick при той же нагрузке → без нового алерта (Bump);
// свежее окно вернулось к ~800 → закрытие и ровно одна задача close
// (notified_close). Плюс проверки: enabled=false не оценивается; топ-K отсекает
// низкотрафичную цель; web-vital открывается по своей ветке. Внутренний тест
// (package trace) — чтобы звать неэкспортированный tick напрямую.
func TestEvaluatorLifecycle(t *testing.T) {
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	notifier := &RegressionNotifier{
		Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
		Regressions: NewRegressionService(pool), Pool: pool,
	}
	ev := &Evaluator{
		Pool:         pool,
		Query:        NewQuery(conn),
		Regressions:  NewRegressionService(pool),
		Notifier:     notifier,
		Policy:       escalation.NewPolicyStore(pool),
		TopK:         50,
		BaselineDays: 7,
	}

	// --- projMain: полный цикл open → re-tick → resolve --------------------
	pid := createEvalProject(t, pool, "eval-main")
	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	const target = "GET /reg-main"

	// База: 6 прошлых дней по 800 мс (по 20 замеров/день). Скачок: 120 замеров
	// по 1200 мс в свежем окне (последние минуты).
	now := time.Now().UTC()
	w := NewSpanWriter(conn)
	go w.Run()
	for d := 1; d <= 6; d++ {
		addEndpointTx(w, pid, target, now.Add(-time.Duration(d)*24*time.Hour), 800, 20, fmt.Sprintf("base-%d", d))
	}
	addEndpointTx(w, pid, target, now.Add(-2*time.Minute), 1200, 120, "spikeA")
	if err := w.Close(ctx); err != nil {
		t.Fatalf("seed phase A close: %v", err)
	}

	// Tick 1: открытие инцидента + одна задача open.
	ev.tick(ctx)
	if got := countIncidents(t, ctx, pool, pid); got != 1 {
		t.Fatalf("after open tick: incidents = %d, want 1", got)
	}
	status, no, nc := incidentState(t, ctx, pool, pid, target, "duration")
	if status != "open" || !no || nc {
		t.Fatalf("after open tick: status=%q notified_open=%v notified_close=%v, want open/true/false", status, no, nc)
	}
	if got := outboxCount(t, ctx, pool); got != 1 {
		t.Fatalf("after open tick: outbox rows = %d, want 1", got)
	}

	// Tick 2: та же нагрузка → без нового инцидента и без нового алерта (Bump).
	ev.tick(ctx)
	if got := countIncidents(t, ctx, pool, pid); got != 1 {
		t.Fatalf("after re-tick: incidents = %d, want still 1", got)
	}
	if got := outboxCount(t, ctx, pool); got != 1 {
		t.Fatalf("after re-tick: outbox rows = %d, want still 1 (Bump, no new alert)", got)
	}

	// Восстановление: заливаем много замеров по 800 мс в свежее окно, чтобы p95
	// окна опустился под recovery-порог (скачок 1200 становится < 5% выборки).
	now2 := time.Now().UTC()
	w2 := NewSpanWriter(conn)
	go w2.Run()
	addEndpointTx(w2, pid, target, now2.Add(-1*time.Minute), 800, 4000, "recoverA")
	if err := w2.Close(ctx); err != nil {
		t.Fatalf("seed recovery close: %v", err)
	}

	// Tick 3: закрытие инцидента + одна задача close.
	ev.tick(ctx)
	status, no, nc = incidentState(t, ctx, pool, pid, target, "duration")
	if status != "resolved" || !no || !nc {
		t.Fatalf("after resolve tick: status=%q notified_open=%v notified_close=%v, want resolved/true/true", status, no, nc)
	}
	if got := outboxCount(t, ctx, pool); got != 2 {
		t.Fatalf("after resolve tick: outbox rows = %d, want 2 (open + close)", got)
	}

	// --- web-vital: открытие по ветке webvital_p75 -------------------------
	pidV := createEvalProject(t, pool, "eval-vital")
	const vpage = "GET /vp"
	wv := NewSpanWriter(conn)
	go wv.Run()
	for d := 1; d <= 6; d++ {
		addVitalTx(wv, pidV, vpage, now.Add(-time.Duration(d)*24*time.Hour), 200, 20, fmt.Sprintf("vbase-%d", d))
	}
	addVitalTx(wv, pidV, vpage, now.Add(-2*time.Minute), 600, 120, "vspike")
	if err := wv.Close(ctx); err != nil {
		t.Fatalf("seed vital close: %v", err)
	}
	ev.tick(ctx)
	status, no, nc = incidentState(t, ctx, pool, pidV, vpage, "lcp")
	if status != "open" || !no {
		t.Fatalf("vital: status=%q notified_open=%v, want open/true", status, no)
	}
	var kind string
	if err := pool.QueryRow(ctx,
		"SELECT target_kind FROM perf_regressions WHERE project_id=$1 AND target=$2 AND metric='lcp'",
		pidV, vpage).Scan(&kind); err != nil {
		t.Fatalf("vital target_kind: %v", err)
	}
	if kind != "webvital_p75" {
		t.Fatalf("vital target_kind = %q, want webvital_p75", kind)
	}

	// --- enabled=false: проект не оценивается ------------------------------
	pidD := createEvalProject(t, pool, "eval-disabled")
	setRegConfig(t, ctx, pool, pidD, `{"enabled":false}`)
	const dtarget = "GET /disabled"
	wd := NewSpanWriter(conn)
	go wd.Run()
	for d := 1; d <= 6; d++ {
		addEndpointTx(wd, pidD, dtarget, now.Add(-time.Duration(d)*24*time.Hour), 800, 20, fmt.Sprintf("dbase-%d", d))
	}
	addEndpointTx(wd, pidD, dtarget, now.Add(-2*time.Minute), 1200, 120, "dspike")
	if err := wd.Close(ctx); err != nil {
		t.Fatalf("seed disabled close: %v", err)
	}
	ev.tick(ctx)
	if got := countIncidents(t, ctx, pool, pidD); got != 0 {
		t.Fatalf("disabled project: incidents = %d, want 0 (not evaluated)", got)
	}

	// --- топ-K: низкотрафичная цель отсекается -----------------------------
	pidK := createEvalProject(t, pool, "eval-topk")
	const hiTarget = "GET /hi" // высокий трафик, стабильный
	const loTarget = "GET /lo" // низкий трафик, но со скачком
	wk := NewSpanWriter(conn)
	go wk.Run()
	for d := 1; d <= 6; d++ {
		addEndpointTx(wk, pidK, hiTarget, now.Add(-time.Duration(d)*24*time.Hour), 800, 200, fmt.Sprintf("hibase-%d", d))
		addEndpointTx(wk, pidK, loTarget, now.Add(-time.Duration(d)*24*time.Hour), 800, 20, fmt.Sprintf("lobase-%d", d))
	}
	addEndpointTx(wk, pidK, hiTarget, now.Add(-2*time.Minute), 800, 300, "hirec")    // стабильный
	addEndpointTx(wk, pidK, loTarget, now.Add(-2*time.Minute), 1200, 120, "lospike") // скачок, но мало трафика
	if err := wk.Close(ctx); err != nil {
		t.Fatalf("seed topk close: %v", err)
	}
	// TopK=1: TopEndpointsByTraffic вернёт только самый нагруженный (hi, 300 >
	// 120), низкотрафичный lo даже не оценивается.
	evK := &Evaluator{
		Pool: pool, Query: NewQuery(conn), Regressions: NewRegressionService(pool),
		Notifier: notifier, TopK: 1, BaselineDays: 7,
	}
	evK.tick(ctx)
	if got := countIncidents(t, ctx, pool, pidK); got != 0 {
		t.Fatalf("topk project: incidents = %d, want 0 (lo excluded by TopK, hi stable)", got)
	}
}

// TestEvaluatorSeasonalOpensAndFallback проверяет сезонный режим оценщика на
// живых PG+CH. Проект с cfg.SeasonalEnabled=true оценивается по сезонному base
// (то же окно того же дня недели за прошлые недели), а не по скользящему:
//
//   - «GET /seasonal» имеет сезонный слот ~200мс за 3 прошлые недели (в окне
//     [now−60м, now), сдвинутом на 7/14/21 сут) и свежий скачок ~400мс →
//     открывается по СЕЗОННОМУ коридору (400 > 200×1.25 и > 200+floor). У этой
//     цели НЕТ дневной истории внутри скользящего окна [now−7д, now) (слоты
//     лежат ≥7 сут назад), поэтому при скользящем base она бы не открылась —
//     открытие доказывает, что взят именно сезонный base (baseline_value ≈ 200).
//   - «GET /nohist» сезонной истории не имеет (слот < min_samples) → fallback на
//     скользящий: дневная база ~200мс за 6 прошлых суток + свежий скачок ~400мс →
//     открывается по скользящему коридору, без паники.
//
// Внутренний тест (package trace) — чтобы звать evalProject с управляемым now
// (сезонный якорь должен быть детерминированным, а не time.Now в tick).
func TestEvaluatorSeasonalOpensAndFallback(t *testing.T) {
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	ev := &Evaluator{
		Pool:         pool,
		Query:        NewQuery(conn),
		Regressions:  NewRegressionService(pool),
		TopK:         50,
		BaselineDays: 7,
	}

	pid := createEvalProject(t, pool, "eval-seasonal")
	// Сезонный режим включён; порог/пол/восстановление — дефолтные (0.25/100/0.10),
	// min_samples снижен до 40, чтобы сеять слот дешевле (60 сэмплов слота > 40).
	setRegConfig(t, ctx, pool, pid, `{"seasonal_enabled":true,"seasonal_weeks":3,"min_samples":40}`)
	cfg, err := RegressionConfigFromJSON(regConfigRaw(t, ctx, pool, pid))
	if err != nil {
		t.Fatalf("parse cfg: %v", err)
	}

	// Якорь now — полдень позавчера (UTC), кратный 5 минутам: окна слота и смещения
	// −30 мин ложатся ровно в 5-минутные бакеты MV без пограничного дребезга.
	now := time.Now().UTC().Truncate(24 * time.Hour).Add(-36 * time.Hour)
	const weekD = 7 * 24 * time.Hour

	w := NewSpanWriter(conn)
	go w.Run()

	// «GET /seasonal»: сезонный слот ~200мс за 3 недели (сдвиг 7/14/21 сут в том же
	// окне) + свежий скачок 400мс. Слоты лежат ≥7 сут назад — вне скользящего окна.
	const seasonalTarget = "GET /seasonal"
	addEndpointTx(w, pid, seasonalTarget, now.Add(-1*weekD).Add(-30*time.Minute), 200, 20, "se-w1")
	addEndpointTx(w, pid, seasonalTarget, now.Add(-2*weekD).Add(-30*time.Minute), 200, 20, "se-w2")
	addEndpointTx(w, pid, seasonalTarget, now.Add(-3*weekD).Add(-30*time.Minute), 200, 20, "se-w3")
	addEndpointTx(w, pid, seasonalTarget, now.Add(-30*time.Minute), 400, 120, "se-rec")

	// «GET /nohist»: сезонной истории нет; дневная база ~200мс за 6 прошлых суток
	// (внутри скользящего окна, но не в недельном слоте) + свежий скачок 400мс.
	const fallbackTarget = "GET /nohist"
	for d := 1; d <= 6; d++ {
		addEndpointTx(w, pid, fallbackTarget, now.Add(-time.Duration(d)*24*time.Hour).Add(-30*time.Minute), 200, 20, fmt.Sprintf("no-base-%d", d))
	}
	addEndpointTx(w, pid, fallbackTarget, now.Add(-30*time.Minute), 400, 120, "no-rec")

	if err := w.Close(ctx); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	ev.evalProject(ctx, pid, cfg, 50, 7, now)

	// Обе цели открылись: сезонная — по сезонному коридору, fallback — по скользящему.
	if got := countIncidents(t, ctx, pool, pid); got != 2 {
		t.Fatalf("после сезонного прохода: инцидентов = %d, want 2 (сезонный OPEN + fallback OPEN)", got)
	}
	if status, _, _ := incidentState(t, ctx, pool, pid, seasonalTarget, "duration"); status != "open" {
		t.Fatalf("сезонная цель: status=%q, want open", status)
	}
	if status, _, _ := incidentState(t, ctx, pool, pid, fallbackTarget, "duration"); status != "open" {
		t.Fatalf("fallback-цель: status=%q, want open (скользящий base)", status)
	}
	// baseline_value сезонной цели ≈ 200мс доказывает, что взят сезонный слот
	// (медиана недельных p95), а не скользящее окно (где базы у цели нет вовсе).
	var seasonalBase float64
	if err := pool.QueryRow(ctx,
		"SELECT baseline_value FROM perf_regressions WHERE project_id=$1 AND target=$2 AND metric='duration'",
		pid, seasonalTarget).Scan(&seasonalBase); err != nil {
		t.Fatalf("seasonal baseline_value: %v", err)
	}
	if seasonalBase < 180 || seasonalBase > 220 {
		t.Fatalf("сезонный baseline_value = %.0f, want ~200 (сезонный слот, не скользящий)", seasonalBase)
	}
}

// TestEvaluatorSeasonalVitalPartialFallback покрывает vital-ветку сезонного
// оценщика e2e (SeasonalBaselineVitalP75s + пер-ключевой merge fallback), которую
// TestEvaluatorSeasonalOpensAndFallback не исполняет (там только эндпойнты). Самое
// нетривиальное — merge «переопределить только недобравшие ключи (страница,
// метрика), набравшие оставить сезонными» — до этого теста e2e не проверялось.
//
// Одна страница, две метрики с разной судьбой:
//   - lcp: сезонный слот ~200 за 3 недели (в окне [now−60м, now), сдвинутом на
//     7/14/21 сут) + свежий скачок 600 → открывается по СЕЗОННОМУ коридору
//     (600 > 200×1.25 и > 200+floorLCP(200)); baseline_value ≈ 200. Слоты лежат
//     ≥7 сут назад — скользящей истории у lcp нет, при скользящем base не открылся бы.
//   - inp: сезонной истории НЕТ (слот < min_samples) → страница попадает в добор,
//     но переопределяется ТОЛЬКО ключ inp: дневная база ~150 за 6 суток внутри
//     скользящего окна + свежий скачок 500 → открывается по СКОЛЬЗЯЩЕМУ коридору
//     (500 > 150×1.25 и > 150+floorINP(50)); baseline_value ≈ 150.
//
// Совпадение ОБЕИХ баз (lcp≈200 сезонный, inp≈150 скользящий) на одной странице
// доказывает частичный merge: затри он набравший ключ или не добери недобравший —
// одна из баз оказалась бы не той или инцидент не открылся.
func TestEvaluatorSeasonalVitalPartialFallback(t *testing.T) {
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	ev := &Evaluator{
		Pool:         pool,
		Query:        NewQuery(conn),
		Regressions:  NewRegressionService(pool),
		TopK:         50,
		BaselineDays: 7,
	}

	pid := createEvalProject(t, pool, "eval-seasonal-vital")
	setRegConfig(t, ctx, pool, pid, `{"seasonal_enabled":true,"seasonal_weeks":3,"min_samples":40}`)
	cfg, err := RegressionConfigFromJSON(regConfigRaw(t, ctx, pool, pid))
	if err != nil {
		t.Fatalf("parse cfg: %v", err)
	}

	now := time.Now().UTC().Truncate(24 * time.Hour).Add(-36 * time.Hour)
	const weekD = 7 * 24 * time.Hour
	const page = "/vp"

	w := NewSpanWriter(conn)
	go w.Run()

	// lcp: сезонный слот ~200 за 3 недели (сдвиг 7/14/21 сут) + свежий скачок 600.
	addVitalMetricTx(w, pid, page, now.Add(-1*weekD).Add(-30*time.Minute), "lcp", 200, 60, "vl-w1")
	addVitalMetricTx(w, pid, page, now.Add(-2*weekD).Add(-30*time.Minute), "lcp", 200, 60, "vl-w2")
	addVitalMetricTx(w, pid, page, now.Add(-3*weekD).Add(-30*time.Minute), "lcp", 200, 60, "vl-w3")
	addVitalMetricTx(w, pid, page, now.Add(-30*time.Minute), "lcp", 600, 120, "vl-rec")

	// inp: сезонной истории нет; дневная база ~150 за 6 суток (внутри скользящего
	// окна, но не в недельном слоте) + свежий скачок 500.
	for d := 1; d <= 6; d++ {
		addVitalMetricTx(w, pid, page, now.Add(-time.Duration(d)*24*time.Hour).Add(-30*time.Minute), "inp", 150, 60, fmt.Sprintf("vi-d%d", d))
	}
	addVitalMetricTx(w, pid, page, now.Add(-30*time.Minute), "inp", 500, 120, "vi-rec")

	if err := w.Close(ctx); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	ev.evalProject(ctx, pid, cfg, 50, 7, now)

	// Обе метрики страницы открылись; duration остался плоским (1 с) — лишнего
	// инцидента нет, поэтому ровно 2.
	if got := countIncidents(t, ctx, pool, pid); got != 2 {
		t.Fatalf("vital-проход: инцидентов = %d, want 2 (lcp сезонный + inp fallback)", got)
	}
	if status, _, _ := incidentState(t, ctx, pool, pid, page, "lcp"); status != "open" {
		t.Fatalf("lcp: status=%q, want open (сезонный)", status)
	}
	if status, _, _ := incidentState(t, ctx, pool, pid, page, "inp"); status != "open" {
		t.Fatalf("inp: status=%q, want open (fallback скользящий)", status)
	}
	// Ключевое: базы разошлись — lcp остался сезонным (~200), inp ушёл в скользящий
	// (~150). Совпадение обеих на одной странице доказывает пер-ключевой merge.
	if lcpBase := vitalBaseline(t, ctx, pool, pid, page, "lcp"); lcpBase < 180 || lcpBase > 220 {
		t.Fatalf("lcp baseline_value = %.0f, want ~200 (сезонный слот)", lcpBase)
	}
	if inpBase := vitalBaseline(t, ctx, pool, pid, page, "inp"); inpBase < 130 || inpBase > 170 {
		t.Fatalf("inp baseline_value = %.0f, want ~150 (скользящий, не сезонный)", inpBase)
	}
}

// mockMaint — trace.MaintenanceChecker для тестов: func-обёртка вместо
// полноценного uptime.Service (интерфейс здесь в один метод — реальный сервис
// с окнами обслуживания и своей БД тестам этого пакета не нужен). Калька
// host.mockMaint (Task 3).
type mockMaint func(ctx context.Context, projectID int64, at time.Time) (bool, error)

func (m mockMaint) InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error) {
	return m(ctx, projectID, at)
}

// regressionInMaintenance читает in_maintenance инцидента регрессии по
// (target, metric) — что записал Evaluator.Regressions.Open в момент открытия.
func regressionInMaintenance(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid int64, target, metric string) bool {
	t.Helper()
	var v bool
	if err := pool.QueryRow(ctx,
		"SELECT in_maintenance FROM perf_regressions WHERE project_id=$1 AND target=$2 AND metric=$3",
		pid, target, metric).Scan(&v); err != nil {
		t.Fatalf("read in_maintenance: %v", err)
	}
	return v
}

// TestEvaluatorMaintenanceSuppressesRegressionNotify — B3 Task 5, Путь A:
// открытие регрессии в окне обслуживания (Maint→true) пишет инцидент с
// in_maintenance=true, но НЕ уведомляет; закрытие того же инцидента (ещё
// внутри окна) тоже не уведомляет. Зеркало
// host.TestEvaluatorMaintenanceSuppressesThresholdNotify (Task 3), но по пути
// регрессий perf: open/close идут через один evalTarget, а не через
// applyDecision.
func TestEvaluatorMaintenanceSuppressesRegressionNotify(t *testing.T) {
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	asvc := alert.NewService(pool)
	notifier := &RegressionNotifier{
		Alerts: asvc, Outbox: notify.NewOutbox(pool), BaseURL: "https://gotcha.example",
		Regressions: NewRegressionService(pool), Pool: pool,
	}
	ev := &Evaluator{
		Pool: pool, Query: NewQuery(conn), Regressions: NewRegressionService(pool),
		Notifier: notifier, Policy: escalation.NewPolicyStore(pool), TopK: 50, BaselineDays: 7,
		Maint: mockMaint(func(context.Context, int64, time.Time) (bool, error) { return true, nil }),
	}

	pid := createEvalProject(t, pool, "eval-maint-open")
	// Канал ОБЯЗАТЕЛЕН: без него Notify не пишет outbox независимо от гейта
	// (Notifier.Notify: "проект без включённых каналов — задач не будет"), и
	// проверка outboxCount()==0 ниже доказывала бы только отсутствие канала, а
	// не работу гейта maintenance.
	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	const target = "GET /maint"

	now := time.Now().UTC()
	w := NewSpanWriter(conn)
	go w.Run()
	for d := 1; d <= 6; d++ {
		addEndpointTx(w, pid, target, now.Add(-time.Duration(d)*24*time.Hour), 800, 20, fmt.Sprintf("mbase-%d", d))
	}
	addEndpointTx(w, pid, target, now.Add(-2*time.Minute), 1200, 120, "mspikeA")
	if err := w.Close(ctx); err != nil {
		t.Fatalf("seed spike close: %v", err)
	}

	ev.tick(ctx)
	status, _, _ := incidentState(t, ctx, pool, pid, target, "duration")
	if status != "open" {
		t.Fatalf("after open tick: status=%q, want open (окно обслуживания не отменяет открытие)", status)
	}
	if !regressionInMaintenance(t, ctx, pool, pid, target, "duration") {
		t.Error("in_maintenance = false, want true (открыто в окне)")
	}
	if got := outboxCount(t, ctx, pool); got != 0 {
		t.Errorf("outbox rows after open tick = %d, want 0 (suppressed by maintenance)", got)
	}

	// Восстановление: заливаем много замеров по 800 мс в свежее окно, чтобы p95
	// окна опустился под recovery-порог. Окно обслуживания всё ещё активно
	// (mockMaint не менялся) — закрытие тоже не должно уведомлять.
	now2 := time.Now().UTC()
	w2 := NewSpanWriter(conn)
	go w2.Run()
	addEndpointTx(w2, pid, target, now2.Add(-1*time.Minute), 800, 4000, "mrecoverA")
	if err := w2.Close(ctx); err != nil {
		t.Fatalf("seed recovery close: %v", err)
	}

	ev.tick(ctx)
	status, _, _ = incidentState(t, ctx, pool, pid, target, "duration")
	if status != "resolved" {
		t.Fatalf("after resolve tick: status=%q, want resolved", status)
	}
	if got := outboxCount(t, ctx, pool); got != 0 {
		t.Errorf("outbox rows after resolve tick = %d, want still 0 (close-notify suppressed too)", got)
	}
}

// TestEvaluatorMaintenanceFalseStillNotifies — Maint заполнен (не nil), но вне
// окна (InMaintenance→false): поведение обычное, уведомление уходит. Отличает
// «MaintenanceChecker сконфигурирован и говорит false» от «MaintenanceChecker
// ==nil» (последнее уже покрыто TestEvaluatorLifecycle back-compat'ом).
func TestEvaluatorMaintenanceFalseStillNotifies(t *testing.T) {
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	asvc := alert.NewService(pool)
	notifier := &RegressionNotifier{
		Alerts: asvc, Outbox: notify.NewOutbox(pool), BaseURL: "https://gotcha.example",
		Regressions: NewRegressionService(pool), Pool: pool,
	}
	ev := &Evaluator{
		Pool: pool, Query: NewQuery(conn), Regressions: NewRegressionService(pool),
		Notifier: notifier, Policy: escalation.NewPolicyStore(pool), TopK: 50, BaselineDays: 7,
		Maint: mockMaint(func(context.Context, int64, time.Time) (bool, error) { return false, nil }),
	}

	pid := createEvalProject(t, pool, "eval-maint-false")
	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	const target = "GET /nomaint"

	now := time.Now().UTC()
	w := NewSpanWriter(conn)
	go w.Run()
	for d := 1; d <= 6; d++ {
		addEndpointTx(w, pid, target, now.Add(-time.Duration(d)*24*time.Hour), 800, 20, fmt.Sprintf("nmbase-%d", d))
	}
	addEndpointTx(w, pid, target, now.Add(-2*time.Minute), 1200, 120, "nmspikeA")
	if err := w.Close(ctx); err != nil {
		t.Fatalf("seed spike close: %v", err)
	}

	ev.tick(ctx)
	status, no, _ := incidentState(t, ctx, pool, pid, target, "duration")
	if status != "open" || !no {
		t.Fatalf("after open tick: status=%q notified_open=%v, want open/true", status, no)
	}
	if regressionInMaintenance(t, ctx, pool, pid, target, "duration") {
		t.Error("in_maintenance = true, want false (outside window)")
	}
	if got := outboxCount(t, ctx, pool); got != 1 {
		t.Errorf("outbox rows after open tick = %d, want 1 (not suppressed outside maintenance)", got)
	}
}

// TestEvaluatorMaintenanceCloseSuppressedByFlagAfterWindowEnds — дискриминирует
// close-гейт «по сохранённому флагу» (!open.InMaintenance) от ошибочного «по
// текущему окну» (!e.inMaintenance(now)): открываем регрессию В окне, затем
// окно ЗАКАНЧИВАЕТСЯ (mock→false) — close всё равно должен быть подавлен, т.к.
// читается сохранённый флаг инцидента. Зеркало
// host.TestEvaluatorMaintenanceCloseSuppressedByFlagAfterWindowEnds.
func TestEvaluatorMaintenanceCloseSuppressedByFlagAfterWindowEnds(t *testing.T) {
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	asvc := alert.NewService(pool)
	notifier := &RegressionNotifier{
		Alerts: asvc, Outbox: notify.NewOutbox(pool), BaseURL: "https://gotcha.example",
		Regressions: NewRegressionService(pool), Pool: pool,
	}
	inWindow := true
	ev := &Evaluator{
		Pool: pool, Query: NewQuery(conn), Regressions: NewRegressionService(pool),
		Notifier: notifier, Policy: escalation.NewPolicyStore(pool), TopK: 50, BaselineDays: 7,
		Maint: mockMaint(func(context.Context, int64, time.Time) (bool, error) { return inWindow, nil }),
	}

	pid := createEvalProject(t, pool, "eval-maint-flag")
	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	const target = "GET /maintflag"

	now := time.Now().UTC()
	w := NewSpanWriter(conn)
	go w.Run()
	for d := 1; d <= 6; d++ {
		addEndpointTx(w, pid, target, now.Add(-time.Duration(d)*24*time.Hour), 800, 20, fmt.Sprintf("fbase-%d", d))
	}
	addEndpointTx(w, pid, target, now.Add(-2*time.Minute), 1200, 120, "fspikeA")
	if err := w.Close(ctx); err != nil {
		t.Fatalf("seed spike close: %v", err)
	}

	ev.tick(ctx)
	status, _, _ := incidentState(t, ctx, pool, pid, target, "duration")
	if status != "open" {
		t.Fatalf("after open tick: status=%q, want open", status)
	}
	if !regressionInMaintenance(t, ctx, pool, pid, target, "duration") {
		t.Fatal("in_maintenance = false, want true (открыто в окне)")
	}
	if got := outboxCount(t, ctx, pool); got != 0 {
		t.Fatalf("outbox rows after open tick = %d, want 0 (suppressed by maintenance)", got)
	}

	// Окно обслуживания закончилось — close-гейт должен смотреть на
	// сохранённый флаг, а не на текущее состояние окна.
	inWindow = false

	now2 := time.Now().UTC()
	w2 := NewSpanWriter(conn)
	go w2.Run()
	addEndpointTx(w2, pid, target, now2.Add(-1*time.Minute), 800, 4000, "frecoverA")
	if err := w2.Close(ctx); err != nil {
		t.Fatalf("seed recovery close: %v", err)
	}

	ev.tick(ctx)
	status, _, _ = incidentState(t, ctx, pool, pid, target, "duration")
	if status != "resolved" {
		t.Fatalf("after resolve tick: status=%q, want resolved", status)
	}
	if got := outboxCount(t, ctx, pool); got != 0 {
		t.Errorf("outbox rows after resolve tick = %d, want still 0 (close by saved flag, not by current window)", got)
	}
}

// regConfigRaw читает сырой perf_regression_config проекта — чтобы тест разобрал
// его тем же RegressionConfigFromJSON, что и оценщик в проде.
func regConfigRaw(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid int64) []byte {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(ctx,
		"SELECT perf_regression_config FROM projects WHERE id=$1", pid).Scan(&raw); err != nil {
		t.Fatalf("read reg config: %v", err)
	}
	return raw
}

// addEndpointTx добавляет n одинаковых http.server-транзакций (все durMs мс) с
// уникальными id — так перцентиль окна равен ровно durMs.
func addEndpointTx(w *SpanWriter, pid int64, name string, at time.Time, durMs, n int, prefix string) {
	for i := 0; i < n; i++ {
		w.Add(pid, pid, Transaction{
			TraceID:     fmt.Sprintf("%s-%06d", prefix, i),
			SpanID:      fmt.Sprintf("%s-s-%06d", prefix, i),
			Name:        name,
			Op:          "http.server",
			Status:      "ok",
			Start:       at,
			End:         at.Add(time.Duration(durMs) * time.Millisecond),
			Environment: "production",
		})
	}
}

// addVitalTx добавляет n одинаковых pageload-транзакций с фиксированным lcp
// (мс), длительность самой транзакции постоянна (1 с) — чтобы её эндпойнтный
// p95 не дрейфовал и не открыл лишний duration-инцидент.
func addVitalTx(w *SpanWriter, pid int64, name string, at time.Time, lcp float64, n int, prefix string) {
	for i := 0; i < n; i++ {
		w.Add(pid, pid, Transaction{
			TraceID:      fmt.Sprintf("%s-%06d", prefix, i),
			SpanID:       fmt.Sprintf("%s-s-%06d", prefix, i),
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

// addVitalMetricTx добавляет n pageload-транзакций страницы name с фиксированным
// значением одной web-vital-метрики; длительность транзакции постоянна (1 с),
// чтобы её эндпойнтный p95 не дрейфовал и не открыл лишний duration-инцидент.
func addVitalMetricTx(w *SpanWriter, pid int64, name string, at time.Time, metric string, val float64, n int, prefix string) {
	for i := 0; i < n; i++ {
		w.Add(pid, pid, Transaction{
			TraceID:      fmt.Sprintf("%s-%06d", prefix, i),
			SpanID:       fmt.Sprintf("%s-s-%06d", prefix, i),
			Name:         name,
			Op:           "pageload",
			Status:       "ok",
			Start:        at,
			End:          at.Add(time.Second),
			Environment:  "production",
			Measurements: map[string]float64{metric: val},
		})
	}
}

// vitalBaseline читает baseline_value инцидента vital-метрики (страница, метрика).
func vitalBaseline(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid int64, target, metric string) float64 {
	t.Helper()
	var v float64
	if err := pool.QueryRow(ctx,
		"SELECT baseline_value FROM perf_regressions WHERE project_id=$1 AND target=$2 AND metric=$3",
		pid, target, metric).Scan(&v); err != nil {
		t.Fatalf("vital baseline_value (%s): %v", metric, err)
	}
	return v
}

// createEvalProject заводит проект прямыми вставками (пакет trace не зависит от
// org), возвращает project_id. Конфиг регрессий — дефолтный '{}' (enabled=true).
func createEvalProject(t *testing.T, pool *pgxpool.Pool, slug string) int64 {
	t.Helper()
	ctx := context.Background()
	var orgID, projectID int64
	if _, err := pool.Exec(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x')", slug+"@example.com"); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,$1,1000000) RETURNING id",
		slug).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,$2,$2) RETURNING id",
		orgID, slug).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

func setRegConfig(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid int64, cfg string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		"UPDATE projects SET perf_regression_config = $1::jsonb WHERE id = $2", cfg, pid); err != nil {
		t.Fatalf("set reg config: %v", err)
	}
}

func countIncidents(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid int64) int {
	t.Helper()
	var c int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM perf_regressions WHERE project_id = $1", pid).Scan(&c); err != nil {
		t.Fatalf("count incidents: %v", err)
	}
	return c
}

func incidentState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid int64, target, metric string) (status string, notifiedOpen, notifiedClose bool) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		"SELECT status, notified_open, notified_close FROM perf_regressions WHERE project_id=$1 AND target=$2 AND metric=$3",
		pid, target, metric).Scan(&status, &notifiedOpen, &notifiedClose); err != nil {
		t.Fatalf("incident state: %v", err)
	}
	return status, notifiedOpen, notifiedClose
}

func outboxCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var c int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM notification_outbox").Scan(&c); err != nil {
		t.Fatalf("outbox count: %v", err)
	}
	return c
}

// countingRegressions оборачивает настоящий RegressionService, считая заходы
// в PostgreSQL за снимком открытых регрессий — то единственное, что интересует
// находку №43. Обёртка, а не полностью in-memory подделка: это позволяет
// прогнать реальный evalProject через настоящую БД (тот же стенд, что и у
// TestEvaluatorLifecycle) и при этом точно, не по логам, посчитать число
// обращений — способ посчитать запросы взят из соседнего fakeCHConn
// (writer_unit_test.go): там тоже считающая обёртка над интерфейсом, а не
// разбор журнала.
type countingRegressions struct {
	*RegressionService
	reads atomic.Int64
}

func (c *countingRegressions) OpenForProject(ctx context.Context, projectID int64) (map[RegressionKey]Regression, error) {
	c.reads.Add(1)
	return c.RegressionService.OpenForProject(ctx, projectID)
}

// TestEvaluatorReadsOpenRegressionsOnce: решение по каждой цели начиналось с
// отдельного запроса в PostgreSQL. При полусотне целей на проект это двести
// round-trip'ов на проект за тик, последовательно, в одной горутине (находка
// №43). Три цели здесь — не полусотня, но принцип виден: число обращений не
// должно расти вместе с числом целей.
//
// Раунд правок 1 (ревью): чтение снимка стоит ДО циклов по целям и
// безусловно — «один запрос» было бы верно даже при НУЛЕ обработанных целей
// (например, если правка сломает подбор целей и тик перестанет их видеть
// вовсе). Поэтому одного `reads == 1` недостаточно: тест обязан НЕЗАВИСИМО
// от этого счётчика доказать, что целей было заведомо больше одной. Для
// этого все три цели дают настоящий скачок (как в TestEvaluatorLifecycle) —
// после тика в perf_regressions обязано появиться РОВНО len(targets) строк,
// и это проверяется прямым запросом к базе (countIncidents), а не через
// countingRegressions: два разных источника истины для двух разных
// утверждений, ни один не выводится из другого.
func TestEvaluatorReadsOpenRegressionsOnce(t *testing.T) {
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	counting := &countingRegressions{RegressionService: NewRegressionService(pool)}
	ev := &Evaluator{
		Pool: pool, Query: NewQuery(conn), Regressions: counting,
		TopK: 10, BaselineDays: 7,
	}

	pid := createEvalProject(t, pool, "eval-batch-count")
	now := time.Now().UTC()
	w := NewSpanWriter(conn)
	go w.Run()
	targets := []string{"GET /batch-a", "GET /batch-b", "GET /batch-c"}
	for _, target := range targets {
		for d := 1; d <= 6; d++ {
			addEndpointTx(w, pid, target, now.Add(-time.Duration(d)*24*time.Hour), 800, 20, fmt.Sprintf("%s-base-%d", target, d))
		}
		// Настоящий скачок в свежем окне — не стабильная нагрузка: нужен
		// независимый от countingRegressions сигнал, что цель дошла до
		// конца обработки, а не просто была прочитана из CH и отброшена.
		addEndpointTx(w, pid, target, now.Add(-2*time.Minute), 1200, 120, target+"-spike")
	}
	if err := w.Close(ctx); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	ev.tick(ctx)

	// Независимая проверка: целей было действительно len(targets), а не
	// ноль и не одна — прямым запросом к perf_regressions, в обход
	// countingRegressions. Если бы чтение снимка "случайно" проходило один
	// раз просто потому, что целей не было вовсе, эта проверка упала бы
	// первой и не позволила бы утверждению ниже создать ложное чувство
	// доказанности (см. мутацию в task-5-report.md, раунд правок 1).
	if got := countIncidents(t, ctx, pool, pid); got != len(targets) {
		t.Fatalf("подготовка сценария сломана: %d целей дошли до открытия инцидента, ожидалось %d — тест не доказывает то, что заявляет его докблок", got, len(targets))
	}
	if got := counting.reads.Load(); got != 1 {
		t.Fatalf("запросов открытых регрессий за тик = %d, хотим 1 (независимо от числа целей — их %d)", got, len(targets))
	}
}
