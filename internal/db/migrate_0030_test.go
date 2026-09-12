package db_test

import (
	"context"
	"math"
	"os"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

const migration0030Path = "migrations/pg/0030_regression_values_ms.up.sql"

// 0030 меняет смысл данных (мкс → мс) без изменения схемы — destructiveSQL не считает UPDATE
// разрушительным, поэтому маркер здесь проверяется точечно, по точному совпадению первой строки.
func TestMigration0030IsMarkedBreaking(t *testing.T) {
	b, err := os.ReadFile(migration0030Path)
	if err != nil {
		t.Fatalf("прочитать миграцию: %v", err)
	}
	first, _, _ := strings.Cut(string(b), "\n")
	first = strings.TrimSpace(first)
	if first != "-- backward-compatible: no" {
		t.Fatalf("первая строка %s = %q, want %q", migration0030Path, first, "-- backward-compatible: no")
	}
}

// Раунд-трип умножения/деления на 1000 в double не обязан быть побитово точным (~1e-10 на ~1e6) —
// это свойство арифметики с плавающей точкой, а не небрежность; кратные 1000 сравниваются точно.
const durationRoundTripTolerance = 1e-6

func approxEqual(a, b, tolerance float64) bool {
	return math.Abs(a-b) <= tolerance
}

// Правка обязана задеть только metric='duration' (записан в мкс из-за дефекта p95-запросов) — web-
// vital'ы (уже в мс) и безразмерный cls трогать не должна.
func TestMigrate0030RecomputesDurationValues(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 29); err != nil {
		t.Fatalf("migrate to 29: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	_, projID := seedProject(t, ctx, pool)

	mustExec(t, pool, `INSERT INTO perf_regressions
		(project_id, target_kind, target, metric, baseline_value, peak_value, current_value)
		VALUES ($1, 'endpoint_p95', 'GET /orders', 'duration', 150000, 300000, 200000)`, projID)
	// Некратный duration — реалистичное сырое значение ClickHouse-квантиля, не только простой случай.
	mustExec(t, pool, `INSERT INTO perf_regressions
		(project_id, target_kind, target, metric, baseline_value, peak_value, current_value)
		VALUES ($1, 'endpoint_p95', 'GET /search', 'duration', 643271.4, 910000.7, 712345.6)`, projID)
	// lcp: web-vital, уже в миллисекундах — не должен делиться.
	mustExec(t, pool, `INSERT INTO perf_regressions
		(project_id, target_kind, target, metric, baseline_value, peak_value, current_value)
		VALUES ($1, 'webvital_p75', '/checkout', 'lcp', 2500, 4000, 3000)`, projID)
	// cls: безразмерный — не должен делиться.
	mustExec(t, pool, `INSERT INTO perf_regressions
		(project_id, target_kind, target, metric, baseline_value, peak_value, current_value)
		VALUES ($1, 'webvital_p75', '/checkout', 'cls', 0.1, 0.25, 0.15)`, projID)

	if err := db.MigratePGTo(dsn, 30); err != nil {
		t.Fatalf("migrate to 30: %v", err)
	}

	var durBase, durPeak, durCur float64
	if err := pool.QueryRow(ctx,
		"SELECT baseline_value, peak_value, current_value FROM perf_regressions WHERE metric = 'duration' AND target = 'GET /orders' AND project_id = $1",
		projID).Scan(&durBase, &durPeak, &durCur); err != nil {
		t.Fatalf("select duration row: %v", err)
	}
	if durBase != 150 || durPeak != 300 || durCur != 200 {
		t.Fatalf("duration после 0030 = (%v,%v,%v), want (150,300,200): пересчёт микросекунд в миллисекунды не сработал",
			durBase, durPeak, durCur)
	}

	var rawBase, rawPeak, rawCur float64
	if err := pool.QueryRow(ctx,
		"SELECT baseline_value, peak_value, current_value FROM perf_regressions WHERE metric = 'duration' AND target = 'GET /search' AND project_id = $1",
		projID).Scan(&rawBase, &rawPeak, &rawCur); err != nil {
		t.Fatalf("select non-round duration row: %v", err)
	}
	if !approxEqual(rawBase, 643.2714, durationRoundTripTolerance) ||
		!approxEqual(rawPeak, 910.0007, durationRoundTripTolerance) ||
		!approxEqual(rawCur, 712.3456, durationRoundTripTolerance) {
		t.Fatalf("некратный duration после 0030 = (%v,%v,%v), want ~(643.2714,910.0007,712.3456)",
			rawBase, rawPeak, rawCur)
	}

	var lcpBase, lcpPeak, lcpCur float64
	if err := pool.QueryRow(ctx,
		"SELECT baseline_value, peak_value, current_value FROM perf_regressions WHERE metric = 'lcp' AND project_id = $1",
		projID).Scan(&lcpBase, &lcpPeak, &lcpCur); err != nil {
		t.Fatalf("select lcp row: %v", err)
	}
	if lcpBase != 2500 || lcpPeak != 4000 || lcpCur != 3000 {
		t.Fatalf("lcp после 0030 = (%v,%v,%v), want (2500,4000,3000): web-vital затронут ошибочно",
			lcpBase, lcpPeak, lcpCur)
	}

	var clsBase, clsPeak, clsCur float64
	if err := pool.QueryRow(ctx,
		"SELECT baseline_value, peak_value, current_value FROM perf_regressions WHERE metric = 'cls' AND project_id = $1",
		projID).Scan(&clsBase, &clsPeak, &clsCur); err != nil {
		t.Fatalf("select cls row: %v", err)
	}
	if clsBase != 0.1 || clsPeak != 0.25 || clsCur != 0.15 {
		t.Fatalf("cls после 0030 = (%v,%v,%v), want (0.1,0.25,0.15): безразмерная метрика затронута ошибочно",
			clsBase, clsPeak, clsCur)
	}

	if err := db.MigratePGTo(dsn, 29); err != nil {
		t.Fatalf("migrate down to 29: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT baseline_value, peak_value, current_value FROM perf_regressions WHERE metric = 'duration' AND target = 'GET /orders' AND project_id = $1",
		projID).Scan(&durBase, &durPeak, &durCur); err != nil {
		t.Fatalf("select duration row after down: %v", err)
	}
	if durBase != 150000 || durPeak != 300000 || durCur != 200000 {
		t.Fatalf("duration после отката = (%v,%v,%v), want (150000,300000,200000)", durBase, durPeak, durCur)
	}

	if err := pool.QueryRow(ctx,
		"SELECT baseline_value, peak_value, current_value FROM perf_regressions WHERE metric = 'duration' AND target = 'GET /search' AND project_id = $1",
		projID).Scan(&rawBase, &rawPeak, &rawCur); err != nil {
		t.Fatalf("select non-round duration row after down: %v", err)
	}
	if !approxEqual(rawBase, 643271.4, durationRoundTripTolerance*1000) ||
		!approxEqual(rawPeak, 910000.7, durationRoundTripTolerance*1000) ||
		!approxEqual(rawCur, 712345.6, durationRoundTripTolerance*1000) {
		t.Fatalf("некратный duration после отката = (%v,%v,%v), want ~(643271.4,910000.7,712345.6): "+
			"откат — двоичная арифметика, а не точное равенство",
			rawBase, rawPeak, rawCur)
	}

	// lcp/cls после отката должны остаться исходными — down-миграция делит только metric='duration',
	// но проверяем отдельно, чтобы будущая правка условия не осталась незамеченной.
	if err := pool.QueryRow(ctx,
		"SELECT baseline_value, peak_value, current_value FROM perf_regressions WHERE metric = 'lcp' AND project_id = $1",
		projID).Scan(&lcpBase, &lcpPeak, &lcpCur); err != nil {
		t.Fatalf("select lcp row after down: %v", err)
	}
	if lcpBase != 2500 || lcpPeak != 4000 || lcpCur != 3000 {
		t.Fatalf("lcp после отката = (%v,%v,%v), want (2500,4000,3000): "+
			"down-миграция потеряла условие по метрике и задела web-vital",
			lcpBase, lcpPeak, lcpCur)
	}

	if err := pool.QueryRow(ctx,
		"SELECT baseline_value, peak_value, current_value FROM perf_regressions WHERE metric = 'cls' AND project_id = $1",
		projID).Scan(&clsBase, &clsPeak, &clsCur); err != nil {
		t.Fatalf("select cls row after down: %v", err)
	}
	if clsBase != 0.1 || clsPeak != 0.25 || clsCur != 0.15 {
		t.Fatalf("cls после отката = (%v,%v,%v), want (0.1,0.25,0.15): "+
			"down-миграция потеряла условие по метрике и задела безразмерную метрику",
			clsBase, clsPeak, clsCur)
	}
}
