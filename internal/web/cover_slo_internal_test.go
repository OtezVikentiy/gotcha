package web

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func TestSLOStatusBranches(t *testing.T) {
	for _, tc := range []struct {
		remaining float64
		want      string
	}{
		{-0.5, "exhausted"},
		{0, "exhausted"},
		{0.1, "burning"},
		{0.25, "burning"},
		{0.26, "healthy"},
		{1, "healthy"},
	} {
		if got := sloStatus(tc.remaining); got != tc.want {
			t.Errorf("sloStatus(%v) = %q, want %q", tc.remaining, got, tc.want)
		}
	}
}

type fakeSLOProvider struct {
	buckets      []slo.Bucket
	err          error
	retentionCap time.Duration

	calls            int
	lastFrom, lastTo time.Time
	lastStep         time.Duration
}

func (p *fakeSLOProvider) Buckets(ctx context.Context, s slo.SLO, from, to time.Time, step time.Duration) ([]slo.Bucket, error) {
	p.calls++
	p.lastFrom, p.lastTo, p.lastStep = from, to, step
	return p.buckets, p.err
}

func (p *fakeSLOProvider) RetentionCap() time.Duration { return p.retentionCap }

func (p *fakeSLOProvider) BucketsExcluding(ctx context.Context, s slo.SLO, from, to time.Time, step time.Duration, _ []uptime.Window) ([]slo.Bucket, error) {
	return p.Buckets(ctx, s, from, to, step)
}

func TestSLORowNoProvider(t *testing.T) {
	h := &Handler{}
	s := slo.SLO{ID: 1, Name: "checkout", Kind: slo.SLIAvailability, Target: 0.99}
	row := h.sloRow(context.Background(), s, nil, false)
	if row.HasData {
		t.Fatalf("HasData = true без провайдера, want false: %+v", row)
	}
	if row.ID != 1 || row.Name != "checkout" || row.Kind != "availability" || row.TargetPct != 99 {
		t.Errorf("базовые поля не заполнены: %+v", row)
	}
}

func TestSLORowProviderError(t *testing.T) {
	p := &fakeSLOProvider{buckets: []slo.Bucket{{Good: 1, Total: 1}}, err: errors.New("clickhouse: connection refused")}
	h := &Handler{SLOProviders: map[slo.SLIKind]slo.Provider{slo.SLIAvailability: p}}
	s := slo.SLO{ID: 2, Kind: slo.SLIAvailability, Target: 0.99, WindowDays: 30}
	row := h.sloRow(context.Background(), s, nil, false)
	if row.HasData {
		t.Fatalf("HasData = true при ошибке провайдера, want false: %+v", row)
	}
	if p.calls != 1 {
		t.Fatalf("Buckets вызван %d раз, want 1", p.calls)
	}
}

func TestSLORowNoEvents(t *testing.T) {
	p := &fakeSLOProvider{buckets: []slo.Bucket{}}
	h := &Handler{SLOProviders: map[slo.SLIKind]slo.Provider{slo.SLIAvailability: p}}
	s := slo.SLO{ID: 3, Kind: slo.SLIAvailability, Target: 0.99, WindowDays: 30}
	row := h.sloRow(context.Background(), s, nil, false)
	if row.HasData {
		t.Fatalf("HasData = true без событий, want false: %+v", row)
	}
}

func TestSLORowWithData(t *testing.T) {
	p := &fakeSLOProvider{buckets: []slo.Bucket{{Good: 970, Total: 1000}}}
	h := &Handler{SLOProviders: map[slo.SLIKind]slo.Provider{slo.SLIAvailability: p}}
	s := slo.SLO{ID: 4, Kind: slo.SLIAvailability, Target: 0.99, WindowDays: 30}
	row := h.sloRow(context.Background(), s, nil, false)
	if !row.HasData {
		t.Fatalf("HasData = false с данными, want true: %+v", row)
	}
	if row.AttainmentPct < 96.9 || row.AttainmentPct > 97.1 {
		t.Errorf("AttainmentPct = %v, want ~97", row.AttainmentPct)
	}
	if row.Status != "exhausted" {
		t.Errorf("Status = %q, want exhausted (remaining=%v)", row.Status, row.BudgetRemainingPct)
	}
}

func TestSLORowRetentionClip(t *testing.T) {
	p := &fakeSLOProvider{buckets: []slo.Bucket{{Good: 1, Total: 1}}, retentionCap: 24 * time.Hour}
	h := &Handler{SLOProviders: map[slo.SLIKind]slo.Provider{slo.SLIAvailability: p}}
	s := slo.SLO{ID: 5, Kind: slo.SLIAvailability, Target: 0.99, WindowDays: 90}
	h.sloRow(context.Background(), s, nil, false)
	if p.calls != 1 {
		t.Fatalf("Buckets вызван %d раз, want 1", p.calls)
	}
	age := p.lastTo.Sub(p.lastFrom)
	if age > 25*time.Hour {
		t.Errorf("окно не клипнуто к RetentionCap: from..to = %v, want ~24h", age)
	}
}

func TestSLORowNoClipWithoutCap(t *testing.T) {
	p := &fakeSLOProvider{buckets: []slo.Bucket{{Good: 1, Total: 1}}, retentionCap: 0}
	h := &Handler{SLOProviders: map[slo.SLIKind]slo.Provider{slo.SLIAvailability: p}}
	s := slo.SLO{ID: 6, Kind: slo.SLIAvailability, Target: 0.99, WindowDays: 5}
	h.sloRow(context.Background(), s, nil, false)
	age := p.lastTo.Sub(p.lastFrom)
	if age < 119*time.Hour || age > 121*time.Hour {
		t.Errorf("окно = %v, want ~120h (5 дней) без клипа", age)
	}
}

func TestFillSLODetailBudget(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	h := &Handler{}

	t.Run("BucketsError", func(t *testing.T) {
		p := &fakeSLOProvider{buckets: []slo.Bucket{{Good: 1, Total: 1}}, err: errors.New("boom")}
		vm := &templates.SLODetailVM{}
		s := slo.SLO{Target: 0.99, WindowDays: 30}
		h.fillSLODetailBudget(context.Background(), vm, p, s, now)
		if vm.HasData {
			t.Errorf("HasData = true при ошибке Buckets, want false")
		}
	})

	t.Run("EmptyWindow", func(t *testing.T) {
		p := &fakeSLOProvider{buckets: []slo.Bucket{}}
		vm := &templates.SLODetailVM{}
		s := slo.SLO{Target: 0.99, WindowDays: 30}
		h.fillSLODetailBudget(context.Background(), vm, p, s, now)
		if vm.HasData {
			t.Errorf("HasData = true за пустое окно, want false")
		}
	})

	t.Run("SuccessWithBurnDefaults", func(t *testing.T) {
		p := &fakeSLOProvider{buckets: []slo.Bucket{{Good: 995, Total: 1000}}}
		vm := &templates.SLODetailVM{}
		s := slo.SLO{Target: 0.99, WindowDays: 30}
		h.fillSLODetailBudget(context.Background(), vm, p, s, now)
		if !vm.HasData {
			t.Fatalf("HasData = false с данными, want true")
		}
		if vm.Chart == nil {
			t.Errorf("Chart не заполнен")
		}
		if !vm.HasBurn {
			t.Errorf("HasBurn = false, want true (burn посчитан по дефолтным окнам)")
		}
		if p.calls != 2 {
			t.Fatalf("Buckets вызван %d раз, want 2 (бюджет + burn)", p.calls)
		}
		if p.lastStep != 5*time.Minute {
			t.Errorf("шаг burn-запроса = %v, want 5m (дефолт BurnShortMin)", p.lastStep)
		}
		wantFrom := now.Add(-60 * time.Minute)
		if p.lastFrom.Sub(wantFrom) > time.Second || wantFrom.Sub(p.lastFrom) > time.Second {
			t.Errorf("from burn-запроса = %v, want ~%v (дефолт BurnLongMin=60)", p.lastFrom, wantFrom)
		}
	})

	t.Run("BurnBucketsError", func(t *testing.T) {
		p := &twoCallSLOProvider{
			firstBuckets:  []slo.Bucket{{Good: 99, Total: 100}},
			secondBuckets: []slo.Bucket{{Good: 1, Total: 100}},
			secondErr:     errors.New("burn boom"),
		}
		vm := &templates.SLODetailVM{}
		s := slo.SLO{Target: 0.99, WindowDays: 30, BurnLongMin: 60, BurnShortMin: 5}
		h.fillSLODetailBudget(context.Background(), vm, p, s, now)
		if !vm.HasData {
			t.Fatalf("HasData = false, want true (бюджет посчитан до ошибки burn)")
		}
		if vm.HasBurn {
			t.Errorf("HasBurn = true при ошибке burn-запроса, want false")
		}
	})
}

type twoCallSLOProvider struct {
	firstBuckets  []slo.Bucket
	secondBuckets []slo.Bucket
	secondErr     error
	calls         int
}

func (p *twoCallSLOProvider) Buckets(ctx context.Context, s slo.SLO, from, to time.Time, step time.Duration) ([]slo.Bucket, error) {
	p.calls++
	if p.calls == 1 {
		return p.firstBuckets, nil
	}
	return p.secondBuckets, p.secondErr
}

func (p *twoCallSLOProvider) RetentionCap() time.Duration { return 0 }

func (p *twoCallSLOProvider) BucketsExcluding(ctx context.Context, s slo.SLO, from, to time.Time, step time.Duration, _ []uptime.Window) ([]slo.Bucket, error) {
	return p.Buckets(ctx, s, from, to, step)
}

func TestMonitorInProject(t *testing.T) {
	t.Run("NoUptimeService", func(t *testing.T) {
		h := &Handler{}
		if h.monitorInProject(context.Background(), 1, 1) {
			t.Errorf("monitorInProject = true без h.Uptime, want false")
		}
	})

	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	h := &Handler{Uptime: svc}
	ctx := context.Background()

	projectID := mustSLOTestProject(t, pool, "slo-cov-a")
	otherProjectID := mustSLOTestProject(t, pool, "slo-cov-b")

	cfg, err := json.Marshal(uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	if err != nil {
		t.Fatalf("marshal http config: %v", err)
	}
	created, err := svc.Create(ctx, uptime.Monitor{
		ProjectID:         projectID,
		Name:              "health",
		Kind:              uptime.KindHTTP,
		Enabled:           true,
		IntervalSeconds:   60,
		TimeoutSeconds:    10,
		FailThreshold:     3,
		RecoveryThreshold: 2,
		Consensus:         uptime.ConsensusMajority,
		Config:            cfg,
	}, nil, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("Found", func(t *testing.T) {
		if !h.monitorInProject(ctx, projectID, created.ID) {
			t.Errorf("monitorInProject = false для своего монитора, want true")
		}
	})

	t.Run("WrongProject", func(t *testing.T) {
		if h.monitorInProject(ctx, otherProjectID, created.ID) {
			t.Errorf("monitorInProject = true для чужого проекта, want false")
		}
	})

	t.Run("ListError", func(t *testing.T) {
		cancelledCtx, cancel := context.WithCancel(ctx)
		cancel()
		if h.monitorInProject(cancelledCtx, projectID, created.ID) {
			t.Errorf("monitorInProject = true при ошибке List (отменённый контекст), want false")
		}
	})
}

var slotestProjectSeq int

func mustSLOTestProject(t *testing.T, pool *pgxpool.Pool, slugPrefix string) int64 {
	t.Helper()
	slotestProjectSeq++
	ctx := context.Background()
	var orgID, projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,'Org',1000000) RETURNING id",
		slugPrefix+"-org").Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,$2,'API') RETURNING id",
		orgID, slugPrefix+"-p").Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}
