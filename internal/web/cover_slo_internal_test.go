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

// TestSLOStatusBranches покрывает три ветки sloStatus: исчерпан, горит,
// здоров — и обе границы (0 и 0.25 включительно попадают в «плохие» ветки).
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

// fakeSLOProvider — тестовый slo.Provider: отдаёт заранее заданные корзины
// либо ошибку, запоминает аргументы последнего вызова Buckets (нужно, чтобы
// проверить клип окна к RetentionCap в sloRow/fillSLODetailBudget).
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
	// buckets возвращаются ДАЖЕ при ошибке (как реальный клиент ClickHouse на
	// частичном ответе может отдать и строки, и err) — иначе тест ошибки не
	// отличил бы «err проверен и отброшен» от «err не проверен вовсе»: в обоих
	// случаях nil-срез даёт Attainment(ok=false) и тот же итог HasData=false.
	return p.buckets, p.err
}

func (p *fakeSLOProvider) RetentionCap() time.Duration { return p.retentionCap }

// TestSLORowNoProvider — SLO с типом, для которого в h.SLOProviders нет
// провайдера (карта пуста или nil): sloRow обязана вернуть базовую строку без
// данных, а не паниковать на нулевом провайдере.
func TestSLORowNoProvider(t *testing.T) {
	h := &Handler{}
	s := slo.SLO{ID: 1, Name: "checkout", Kind: slo.SLIAvailability, Target: 0.99}
	row := h.sloRow(context.Background(), s)
	if row.HasData {
		t.Fatalf("HasData = true без провайдера, want false: %+v", row)
	}
	if row.ID != 1 || row.Name != "checkout" || row.Kind != "availability" || row.TargetPct != 99 {
		t.Errorf("базовые поля не заполнены: %+v", row)
	}
}

// TestSLORowProviderError — ошибка Buckets трактуется как «нет данных», а не
// падение строки целиком (список SLO не должен рушиться из-за одного
// провайдера без телеметрии).
func TestSLORowProviderError(t *testing.T) {
	// Buckets отдаёт и данные, и ошибку разом: если бы код игнорировал err,
	// непустой bs дал бы Attainment(ok=true) и HasData=true — тест ловит именно
	// пропуск проверки err, а не просто пустой ответ.
	p := &fakeSLOProvider{buckets: []slo.Bucket{{Good: 1, Total: 1}}, err: errors.New("clickhouse: connection refused")}
	h := &Handler{SLOProviders: map[slo.SLIKind]slo.Provider{slo.SLIAvailability: p}}
	s := slo.SLO{ID: 2, Kind: slo.SLIAvailability, Target: 0.99, WindowDays: 30}
	row := h.sloRow(context.Background(), s)
	if row.HasData {
		t.Fatalf("HasData = true при ошибке провайдера, want false: %+v", row)
	}
	if p.calls != 1 {
		t.Fatalf("Buckets вызван %d раз, want 1", p.calls)
	}
}

// TestSLORowNoEvents — провайдер отвечает пустым рядом (total==0 за окно):
// slo.Attainment вернёт ok=false, строка остаётся без данных (прочерк, а не
// мнимые 0%).
func TestSLORowNoEvents(t *testing.T) {
	p := &fakeSLOProvider{buckets: []slo.Bucket{}}
	h := &Handler{SLOProviders: map[slo.SLIKind]slo.Provider{slo.SLIAvailability: p}}
	s := slo.SLO{ID: 3, Kind: slo.SLIAvailability, Target: 0.99, WindowDays: 30}
	row := h.sloRow(context.Background(), s)
	if row.HasData {
		t.Fatalf("HasData = true без событий, want false: %+v", row)
	}
}

// TestSLORowWithData — провайдер отдаёт корзины с данными: строка получает
// HasData=true и достижение/остаток бюджета, посчитанные slo.Attainment /
// slo.BudgetRemainingFraction, а Status выставлен sloStatus от остатка.
func TestSLORowWithData(t *testing.T) {
	p := &fakeSLOProvider{buckets: []slo.Bucket{{Good: 970, Total: 1000}}}
	h := &Handler{SLOProviders: map[slo.SLIKind]slo.Provider{slo.SLIAvailability: p}}
	s := slo.SLO{ID: 4, Kind: slo.SLIAvailability, Target: 0.99, WindowDays: 30}
	row := h.sloRow(context.Background(), s)
	if !row.HasData {
		t.Fatalf("HasData = false с данными, want true: %+v", row)
	}
	// attainment = 970/1000 = 0.97 → 97%.
	if row.AttainmentPct < 96.9 || row.AttainmentPct > 97.1 {
		t.Errorf("AttainmentPct = %v, want ~97", row.AttainmentPct)
	}
	// consumed = (1-0.97)/(1-0.99) = 3 → remaining = 1-3 = -2 → перерасход.
	if row.Status != "exhausted" {
		t.Errorf("Status = %q, want exhausted (remaining=%v)", row.Status, row.BudgetRemainingPct)
	}
}

// TestSLORowRetentionClip — RetentionCap провайдера короче запрошенного окна
// (WindowDays) обязан клипать from к границе хранения, а не просить данные за
// пределами TTL источника.
func TestSLORowRetentionClip(t *testing.T) {
	p := &fakeSLOProvider{buckets: []slo.Bucket{{Good: 1, Total: 1}}, retentionCap: 24 * time.Hour}
	h := &Handler{SLOProviders: map[slo.SLIKind]slo.Provider{slo.SLIAvailability: p}}
	s := slo.SLO{ID: 5, Kind: slo.SLIAvailability, Target: 0.99, WindowDays: 90}
	h.sloRow(context.Background(), s)
	if p.calls != 1 {
		t.Fatalf("Buckets вызван %d раз, want 1", p.calls)
	}
	// Без клипа from был бы ~90 дней назад; с клипом — ~24ч назад.
	age := p.lastTo.Sub(p.lastFrom)
	if age > 25*time.Hour {
		t.Errorf("окно не клипнуто к RetentionCap: from..to = %v, want ~24h", age)
	}
}

// TestSLORowNoClipWithoutCap — RetentionCap()==0 («хранить вечно») не клипает
// окно вовсе: from остаётся на полные WindowDays назад.
func TestSLORowNoClipWithoutCap(t *testing.T) {
	p := &fakeSLOProvider{buckets: []slo.Bucket{{Good: 1, Total: 1}}, retentionCap: 0}
	h := &Handler{SLOProviders: map[slo.SLIKind]slo.Provider{slo.SLIAvailability: p}}
	s := slo.SLO{ID: 6, Kind: slo.SLIAvailability, Target: 0.99, WindowDays: 5}
	h.sloRow(context.Background(), s)
	age := p.lastTo.Sub(p.lastFrom)
	if age < 119*time.Hour || age > 121*time.Hour {
		t.Errorf("окно = %v, want ~120h (5 дней) без клипа", age)
	}
}

// TestFillSLODetailBudget покрывает fillSLODetailBudget: ошибка провайдера,
// пустое окно (ok=false), успешный путь (бюджет+график+burn) и дефолтинг
// BurnLongMin/BurnShortMin, когда SLO их не задаёт (0 в БД у старых записей),
// и отдельно ошибку burn-запроса (второй Buckets).
func TestFillSLODetailBudget(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	h := &Handler{}

	t.Run("BucketsError", func(t *testing.T) {
		// buckets непусты вместе с ошибкой — если бы код не проверял err,
		// непустой bs дал бы HasData=true по данным, которые не должны были
		// использоваться.
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
		// BurnLongMin/BurnShortMin оставлены нулевыми — код обязан
		// подставить дефолты 60/5, а не запросить нулевое окно.
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
		// Второй вызов Buckets — burn-окно, шаг должен быть defaultSLOBurnShortMin.
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
		// Первый Buckets (бюджет) успешен, второй (burn) — с ошибкой, но всё
		// же непустыми данными: если бы код не проверял err burn-запроса,
		// BurnRate по этим данным дал бы HasBurn=true.
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

// twoCallSLOProvider отдаёт firstBuckets на первый вызов Buckets и ошибку на
// второй — нужен, чтобы отдельно проверить путь burn-запроса в
// fillSLODetailBudget (первый вызов — полное окно бюджета, второй — узкое
// окно burn rate).
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

// TestMonitorInProject покрывает все ветки monitorInProject: h.Uptime==nil,
// монитор найден, монитор из другого проекта (не найден), ошибка List
// (контекст отменён до вызова).
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

// mustSLOTestProject заводит организацию+проект прямыми INSERT (как
// internal/uptime тесты) — минимум, достаточный для FK monitors.project_id.
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
