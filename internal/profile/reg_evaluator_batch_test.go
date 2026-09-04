package profile_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// pgQueryLog — pgx.QueryTracer, копящий SQL всех запросов пула: счётчик
// обращений к PostgreSQL для проверки «один запрос на сервис, а не на функцию».
type pgQueryLog struct {
	mu  sync.Mutex
	sql []string
}

func (l *pgQueryLog) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sql = append(l.sql, data.SQL)
	return ctx
}

func (l *pgQueryLog) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// openLookups — сколько SELECT'ов по profile_regressions ушло в базу
// (INSERT/UPDATE открытия и bump'а не в счёт: их число законно растёт с
// числом регрессий).
func (l *pgQueryLog) openLookups() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, q := range l.sql {
		q = strings.TrimSpace(q)
		if strings.HasPrefix(q, "SELECT") && strings.Contains(q, "profile_regressions") {
			n++
		}
	}
	return n
}

func (l *pgQueryLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sql = nil
}

// tracedPool — второй пул к той же базе, что pool, с трассировщиком запросов.
func tracedPool(t *testing.T, pool *pgxpool.Pool, log *pgQueryLog) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = log
	traced, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(traced.Close)
	return traced
}

// TestRegressionEvaluatorLooksUpOpenRegressionsOnce: PG-часть тика — один
// запрос открытых инцидентов на сервис, сколько бы функций ни проверялось.
// Раньше OpenFor звался в цикле по функциям: до TopK×services SELECT'ов за
// тик при уже батчированной CH-части.
func TestRegressionEvaluatorLooksUpOpenRegressionsOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()
	pid := seedProject(t, pool)

	log := &pgQueryLog{}
	regressions := profile.NewRegressionService(tracedPool(t, pool, log))
	eval := &profile.RegressionEvaluator{
		Query: profile.NewQuery(ch), Regressions: regressions,
		Interval: time.Hour, Config: profile.DefaultProfileRegressionConfig(),
	}

	// Свежее окно: три функции по 40/30/30 из 100. База (вчера, позавчера):
	// по 80 из 1000 каждая — объём функции за окно 190–200, с запасом над
	// MinSamples=100 (тест не про границу гейта) → у всех трёх рост в разы над
	// порогом → три Open.
	for _, ago := range []time.Duration{24 * time.Hour, 48 * time.Hour} {
		seedProfSample(t, ch, pid, "a", 80, ago)
		seedProfSample(t, ch, pid, "b", 80, ago)
		seedProfSample(t, ch, pid, "c", 80, ago)
		seedProfSample(t, ch, pid, "other", 760, ago)
	}
	seedProfSample(t, ch, pid, "a", 40, 5*time.Minute)
	seedProfSample(t, ch, pid, "b", 30, 5*time.Minute)
	seedProfSample(t, ch, pid, "c", 30, 5*time.Minute)

	eval.Tick(ctx)
	if n := log.openLookups(); n != 1 {
		t.Fatalf("open-regression SELECTs on the opening tick = %d, want 1 (one batched lookup per service)", n)
	}
	open, err := regressions.List(ctx, pid, "open", 10)
	if err != nil || len(open) != 3 {
		t.Fatalf("open regressions after tick = %d (%v), want 3", len(open), err)
	}

	// Повторный тик: все три открыты, все три — Bump; поиск открытых по-прежнему один.
	log.reset()
	eval.Tick(ctx)
	if n := log.openLookups(); n != 1 {
		t.Fatalf("open-regression SELECTs on the bump tick = %d, want 1", n)
	}
	open, err = regressions.List(ctx, pid, "open", 10)
	if err != nil || len(open) != 3 {
		t.Fatalf("open regressions after bump tick = %d (%v), want 3 still open", len(open), err)
	}
	for _, r := range open {
		if r.CurrentShare < 0.29 {
			t.Fatalf("regression %s current share = %v, want bumped to the recent share", r.Function, r.CurrentShare)
		}
	}
}
