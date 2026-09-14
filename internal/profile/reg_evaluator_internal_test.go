package profile

import (
	"context"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func seedFnRows(t *testing.T, ch driver.Conn, projectID int64, leaf string, n uint64, at time.Time) {
	t.Helper()
	if n == 0 {
		return
	}
	ctx := context.Background()
	batch, err := ch.PrepareBatch(ctx, `INSERT INTO profile_samples
		(project_id,profile_type,service,environment,transaction,platform,ts,stack,value,trace_id)`)
	if err != nil {
		t.Fatalf("seed prepare batch: %v", err)
	}
	for i := uint64(0); i < n; i++ {
		if err := batch.Append(uint64(projectID), "cpu", "api", "", "", "go", at, []string{"root", leaf}, uint64(1), ""); err != nil {
			t.Fatalf("seed append: %v", err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("seed send: %v", err)
	}
}

func seedShareDay(t *testing.T, ch driver.Conn, projectID int64, hot, other uint64, at time.Time) {
	t.Helper()
	seedFnRows(t, ch, projectID, "hot", hot, at)
	seedFnRows(t, ch, projectID, "other", other, at)
}

func seedInternalProject(t *testing.T, pool *pgxpool.Pool, slug string) int64 {
	t.Helper()
	ctx := context.Background()
	var uid, orgID, projID int64
	pool.QueryRow(ctx, "INSERT INTO users (email,password_hash) VALUES ($1,'x') RETURNING id", slug+"@e.com").Scan(&uid)
	pool.QueryRow(ctx, "INSERT INTO organizations (slug,name,event_quota) VALUES ($1,$1,1000000) RETURNING id", slug+"o").Scan(&orgID)
	pool.QueryRow(ctx, "INSERT INTO projects (org_id,slug,name,platform) VALUES ($1,$2,$2,'go') RETURNING id", orgID, slug+"p").Scan(&projID)
	return projID
}

// Воспроизводит K72: устойчивая (многодневная) регрессия не должна "рассасываться"
// сама — база не обязана расти поверх дней уже открытого инцидента.
func TestEvalServiceBaselineCutoffFreezesOnOpenIncident(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()
	pid := seedInternalProject(t, pool, "cutoff")

	now := time.Now().UTC()
	startedAt := now.Add(-3 * 24 * time.Hour)

	// Чистая база — 6% в течение 2 суток, ЗАДОЛГО до открытия инцидента.
	seedShareDay(t, ch, pid, 60, 940, now.Add(-6*24*time.Hour))
	seedShareDay(t, ch, pid, 60, 940, now.Add(-5*24*time.Hour))

	// Дни самого инцидента (после StartedAt) — держат повышенные 30%. Если база
	// впитает их, инцидент "рассосётся" сам собой на честно продолжающейся деградации.
	seedShareDay(t, ch, pid, 300, 700, now.Add(-3*24*time.Hour).Add(time.Hour))
	seedShareDay(t, ch, pid, 300, 700, now.Add(-2*24*time.Hour))
	seedShareDay(t, ch, pid, 300, 700, now.Add(-1*24*time.Hour))
	// "Сегодня" (recent-окно) — та же деградация, ещё не восстановилось.
	seedShareDay(t, ch, pid, 300, 700, now.Add(-5*time.Minute))

	regressions := NewRegressionService(pool)
	rec, created, err := regressions.Open(ctx, pid, "api", "cpu", "hot", 0.06, 0.06, false)
	if err != nil || !created {
		t.Fatalf("seed open regression: created=%v err=%v", created, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE profile_regressions SET started_at=$2 WHERE id=$1", rec.ID, startedAt); err != nil {
		t.Fatalf("backdate started_at: %v", err)
	}

	cfg := DefaultProfileRegressionConfig()
	cfg.BaselineDays = 4
	eval := &RegressionEvaluator{Query: NewQuery(ch), Regressions: regressions, Config: cfg}

	recentFrom := now.Add(-time.Duration(cfg.WindowMinutes) * time.Minute)
	eval.evalService(ctx, ProjectService{ProjectID: pid, Service: "api", Type: "cpu"}, recentFrom, now)

	_, open, err := regressions.OpenFor(ctx, pid, "api", "cpu", "hot")
	if err != nil {
		t.Fatalf("open for: %v", err)
	}
	if !open {
		t.Fatalf("регрессия закрылась сама на честно продолжающейся деградации — база впитала дни инцидента (K72)")
	}
}

// Воспроизводит K17: функция, вытесненная из top-K, но ПРОДОЛЖАЮЩАЯ регрессировать,
// обязана остаться открытой — закрытие по старому current выдало бы ложное "восстановлено".
func TestEvalServiceKeepsDisplacedFunctionOpenWhileStillRegressing(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()
	pid := seedInternalProject(t, pool, "topk-drop-still-bad")

	now := time.Now().UTC()
	seedFnRows(t, ch, pid, "warm", 60, now.Add(-24*time.Hour))
	seedFnRows(t, ch, pid, "other", 940, now.Add(-24*time.Hour))
	seedFnRows(t, ch, pid, "warm", 60, now.Add(-48*time.Hour))
	seedFnRows(t, ch, pid, "other", 940, now.Add(-48*time.Hour))
	// "hot" доминирует и займёт единственное место в TopK=1; "warm" всё ещё
	// держит ~33% (было ~6% на базе) — реальная, непрошедшая регрессия.
	seedFnRows(t, ch, pid, "hot", 300, now.Add(-5*time.Minute))
	seedFnRows(t, ch, pid, "warm", 150, now.Add(-5*time.Minute))

	regressions := NewRegressionService(pool)
	if _, created, err := regressions.Open(ctx, pid, "api", "cpu", "warm", 0.06, 0.33, false); err != nil || !created {
		t.Fatalf("seed open regression: created=%v err=%v", created, err)
	}

	cfg := DefaultProfileRegressionConfig()
	cfg.TopK = 1
	eval := &RegressionEvaluator{Query: NewQuery(ch), Regressions: regressions, Config: cfg}
	recentFrom := now.Add(-time.Duration(cfg.WindowMinutes) * time.Minute)
	eval.evalService(ctx, ProjectService{ProjectID: pid, Service: "api", Type: "cpu"}, recentFrom, now)

	_, open, err := regressions.OpenFor(ctx, pid, "api", "cpu", "warm")
	if err != nil {
		t.Fatalf("open for: %v", err)
	}
	if !open {
		t.Fatalf("регрессия 'warm' закрыта вслепую после вытеснения из top-K=1, хотя реально всё ещё регрессирует (K17)")
	}
}

// Вторая половина: функция вытеснена из top-K и ДЕЙСТВИТЕЛЬНО восстановилась —
// закрытие тут правильный исход, но должно опираться на свежие данные, не на факт выпадения.
func TestEvalServiceClosesDisplacedFunctionOnRealRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()
	pid := seedInternalProject(t, pool, "topk-drop-recovered")

	now := time.Now().UTC()
	seedFnRows(t, ch, pid, "warm", 60, now.Add(-24*time.Hour))
	seedFnRows(t, ch, pid, "other", 940, now.Add(-24*time.Hour))
	seedFnRows(t, ch, pid, "warm", 60, now.Add(-48*time.Hour))
	seedFnRows(t, ch, pid, "other", 940, now.Add(-48*time.Hour))
	// "hot" доминирует; "warm" вернулся к ~5% (база ~6%) — честное восстановление.
	seedFnRows(t, ch, pid, "hot", 2090, now.Add(-5*time.Minute))
	seedFnRows(t, ch, pid, "warm", 110, now.Add(-5*time.Minute))

	regressions := NewRegressionService(pool)
	if _, created, err := regressions.Open(ctx, pid, "api", "cpu", "warm", 0.06, 0.33, false); err != nil || !created {
		t.Fatalf("seed open regression: created=%v err=%v", created, err)
	}

	cfg := DefaultProfileRegressionConfig()
	cfg.TopK = 1
	eval := &RegressionEvaluator{Query: NewQuery(ch), Regressions: regressions, Config: cfg}
	recentFrom := now.Add(-time.Duration(cfg.WindowMinutes) * time.Minute)
	eval.evalService(ctx, ProjectService{ProjectID: pid, Service: "api", Type: "cpu"}, recentFrom, now)

	_, open, err := regressions.OpenFor(ctx, pid, "api", "cpu", "warm")
	if err != nil {
		t.Fatalf("open for: %v", err)
	}
	if open {
		t.Fatalf("регрессия 'warm' осталась открытой навсегда после вытеснения из top-K=1, хотя реально восстановилась (K17)")
	}
}

// Воспроизводит вторую половину K17: сервис, вовсе переставший слать профили, пропадает
// из ActiveServices — его открытые регрессии не должны застревать открытыми бессрочно.
func TestTickClosesRegressionsOfSilentService(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()
	pid := seedInternalProject(t, pool, "silent-service")

	now := time.Now().UTC()
	// Данные лежат за пределами скользящего окна (default WindowMinutes=60) —
	// сервис не появится в ActiveServices этого тика.
	seedShareDay(t, ch, pid, 100, 900, now.Add(-2*time.Hour))

	regressions := NewRegressionService(pool)
	if _, created, err := regressions.Open(ctx, pid, "api", "cpu", "hot", 0.1, 0.1, false); err != nil || !created {
		t.Fatalf("seed open regression: created=%v err=%v", created, err)
	}

	eval := &RegressionEvaluator{Query: NewQuery(ch), Regressions: regressions, Config: DefaultProfileRegressionConfig()}
	eval.Tick(ctx)

	_, open, err := regressions.OpenFor(ctx, pid, "api", "cpu", "hot")
	if err != nil {
		t.Fatalf("open for: %v", err)
	}
	if open {
		t.Fatalf("регрессия сервиса, замолчавшего целиком, осталась открытой навсегда (K17)")
	}
}
