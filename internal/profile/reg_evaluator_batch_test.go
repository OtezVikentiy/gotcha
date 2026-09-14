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

	for _, ago := range []time.Duration{24 * time.Hour, 48 * time.Hour} {
		seedProfSample(t, ch, pid, "a", 80, ago)
		seedProfSample(t, ch, pid, "b", 80, ago)
		seedProfSample(t, ch, pid, "c", 80, ago)
		seedProfSample(t, ch, pid, "other", 760, ago)
	}
	// Те же доли 0.4/0.3/0.3, что и раньше, но масштаб поднят так, чтобы recentSamples
	// каждой функции по отдельности проходил MinSamples (100), не только их сумма.
	seedProfSample(t, ch, pid, "a", 160, 5*time.Minute)
	seedProfSample(t, ch, pid, "b", 120, 5*time.Minute)
	seedProfSample(t, ch, pid, "c", 120, 5*time.Minute)

	eval.Tick(ctx)
	// 2 = 1 тиковый OpenServices (сервисы с открытыми регрессиями, добор к ActiveServices)
	// + 1 батч OpenForService на единственный сервис проекта — не по одному на функцию.
	if n := log.openLookups(); n != 2 {
		t.Fatalf("open-regression SELECTs on the opening tick = %d, want 2 (one Tick-level + one batched per service)", n)
	}
	open, err := regressions.List(ctx, pid, "open", 10)
	if err != nil || len(open) != 3 {
		t.Fatalf("open regressions after tick = %d (%v), want 3", len(open), err)
	}

	log.reset()
	eval.Tick(ctx)
	if n := log.openLookups(); n != 2 {
		t.Fatalf("open-regression SELECTs on the bump tick = %d, want 2", n)
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
