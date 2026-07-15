package trace

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
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
	notifier := &RegressionNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example"}
	ev := &Evaluator{
		Pool:        pool,
		Query:       NewQuery(conn),
		Regressions: NewRegressionService(pool),
		Notifier:    notifier,
		TopK:        50,
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
	const hiTarget = "GET /hi"   // высокий трафик, стабильный
	const loTarget = "GET /lo"   // низкий трафик, но со скачком
	wk := NewSpanWriter(conn)
	go wk.Run()
	for d := 1; d <= 6; d++ {
		addEndpointTx(wk, pidK, hiTarget, now.Add(-time.Duration(d)*24*time.Hour), 800, 200, fmt.Sprintf("hibase-%d", d))
		addEndpointTx(wk, pidK, loTarget, now.Add(-time.Duration(d)*24*time.Hour), 800, 20, fmt.Sprintf("lobase-%d", d))
	}
	addEndpointTx(wk, pidK, hiTarget, now.Add(-2*time.Minute), 800, 300, "hirec")   // стабильный
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

// addEndpointTx добавляет n одинаковых http.server-транзакций (все durMs мс) с
// уникальными id — так перцентиль окна равен ровно durMs.
func addEndpointTx(w *SpanWriter, pid int64, name string, at time.Time, durMs, n int, prefix string) {
	for i := 0; i < n; i++ {
		w.Add(pid, Transaction{
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
		w.Add(pid, Transaction{
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
