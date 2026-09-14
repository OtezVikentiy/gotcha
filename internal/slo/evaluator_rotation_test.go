package slo_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

var sloRotationSeq atomic.Int64

func seedSLOProjectN(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	n := sloRotationSeq.Add(1)
	var orgID, projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1, 'SLO Rotation', 0) RETURNING id",
		fmt.Sprintf("slo-rot-%d", n)).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, $2, 'SLO Rotation') RETURNING id",
		orgID, fmt.Sprintf("slo-rot-%d", n)).Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return projectID
}

// stuckSLOProvider виснет на каждом запросе бакетов и пишет project_id — так
// видно, какой SLO реально дошёл до провайдера за тик.
type stuckSLOProvider struct {
	mu      sync.Mutex
	touched []int64
}

func (p *stuckSLOProvider) Buckets(ctx context.Context, s slo.SLO, _, _ time.Time, _ time.Duration) ([]slo.Bucket, error) {
	p.mu.Lock()
	p.touched = append(p.touched, s.ProjectID)
	p.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (p *stuckSLOProvider) BucketsExcluding(ctx context.Context, s slo.SLO, from, to time.Time, step time.Duration, _ []uptime.Window) ([]slo.Bucket, error) {
	return p.Buckets(ctx, s, from, to, step)
}

func (p *stuckSLOProvider) RetentionCap() time.Duration { return 0 }

func (p *stuckSLOProvider) touchedCopy() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.touched...)
}

// Без ротации бюджет тика (пол 10с) всегда обрывается на одном и том же первом SLO.
func TestSLOEvaluatorRotatesAcrossTicks(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	store := slo.NewStore(pool)

	var projectIDs []int64
	for i := 0; i < 3; i++ {
		pid := seedSLOProjectN(t, pool)
		projectIDs = append(projectIDs, pid)
		if _, err := store.Create(context.Background(), slo.SLO{
			ProjectID: pid, Name: "rot", Kind: slo.SLIAvailability,
			Target: 0.99, WindowDays: 30, Enabled: true,
		}); err != nil {
			t.Fatalf("create slo for project %d: %v", pid, err)
		}
	}

	provider := &stuckSLOProvider{}
	eval := &slo.Evaluator{
		Pool:      pool,
		Store:     store,
		Providers: map[slo.SLIKind]slo.Provider{slo.SLIAvailability: provider},
		Interval:  time.Second, // бюджет: пол minTickBudget = 10с
	}

	tickAndFirstTouched := func(tickNo int) int64 {
		before := len(provider.touchedCopy())
		if _, err := eval.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", tickNo, err)
		}
		got := provider.touchedCopy()[before:]
		if len(got) == 0 {
			t.Fatalf("тик %d: провайдер ни разу не запрошен — тест не проверяет то, что должен", tickNo)
		}
		first := got[0]
		for _, pid := range got {
			if pid != first {
				t.Fatalf("тик %d: за один тик задет не один SLO: %v", tickNo, got)
			}
		}
		return first
	}

	// За 4 тика курсор обязан пройти все три SLO по кругу и вернуться к первому.
	want := []int64{projectIDs[0], projectIDs[1], projectIDs[2], projectIDs[0]}
	for i, w := range want {
		got := tickAndFirstTouched(i + 1)
		if got != w {
			t.Fatalf("тик %d: обработан SLO проекта %d, want %d (порядок %v)", i+1, got, w, projectIDs)
		}
		if skipped := eval.LastTickSkippedSLOs(); skipped != 2 {
			t.Errorf("тик %d: LastTickSkippedSLOs() = %d, want 2 (два SLO из трёх не влезли в бюджет)", i+1, skipped)
		}
	}
}
