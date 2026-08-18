package slo_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// txSpec — одна засеваемая транзакция: смещение от base, длительность и статус.
type txSpec struct {
	offset time.Duration
	dur    time.Duration
	status string
	env    string
}

// seedTransactions пишет транзакции в CH через SpanWriter (как writer_test.go):
// MV transactions_5m наполняется автоматически при вставке в raw transactions.
func seedTransactions(t *testing.T, conn driver.Conn, projectID int64, name string, base time.Time, specs []txSpec) {
	t.Helper()
	w := trace.NewSpanWriter(conn)
	go w.Run()
	for i, s := range specs {
		start := base.Add(s.offset)
		w.Add(projectID, projectID, trace.Transaction{
			TraceID:     fmt.Sprintf("%032x", i+1),
			SpanID:      fmt.Sprintf("%016x", i+1),
			Name:        name,
			Op:          "http.server",
			Status:      s.status,
			Start:       start,
			End:         start.Add(s.dur),
			Environment: s.env,
			Source:      "test",
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("seed transactions close: %v", err)
	}
}

// seedChecks пишет проверки в check_results через ResultWriter.
func seedChecks(t *testing.T, conn driver.Conn, projectID, monitorID int64, base time.Time, oks []bool) {
	t.Helper()
	w := uptime.NewResultWriter(conn)
	go w.Run()
	for i, ok := range oks {
		at := base.Add(time.Duration(i) * time.Second)
		w.Add(projectID, monitorID, "eu", at, uptime.Result{OK: ok, StatusCode: 200, TotalMs: 100})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("seed checks close: %v", err)
	}
}

func sumBuckets(bs []slo.Bucket) (good, total uint64) {
	for _, b := range bs {
		good += b.Good
		total += b.Total
	}
	return good, total
}

func TestAvailabilityProvider(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx := context.Background()

	const pid = int64(9001)
	base := time.Now().UTC().Truncate(5 * time.Minute).Add(-30 * time.Minute)

	specs := make([]txSpec, 0, 100)
	for i := 0; i < 100; i++ {
		status := "ok"
		if i < 5 {
			status = "internal_error"
		}
		specs = append(specs, txSpec{offset: time.Duration(i) * time.Millisecond, dur: 50 * time.Millisecond, status: status, env: "production"})
	}
	seedTransactions(t, conn, pid, "GET /checkout", base, specs)

	p := slo.NewAvailabilityProvider(trace.NewQuery(conn), nil, 90)
	from, to := base, base.Add(5*time.Minute)
	bs, err := p.Buckets(ctx, slo.SLO{ProjectID: pid, Kind: slo.SLIAvailability, Transaction: "GET /checkout"}, from, to, 5*time.Minute)
	if err != nil {
		t.Fatalf("Buckets: %v", err)
	}
	good, total := sumBuckets(bs)
	if total != 100 || good != 95 {
		t.Fatalf("good/total = %d/%d, want 95/100", good, total)
	}
}

func TestLatencyProvider(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx := context.Background()

	const pid = int64(9002)
	base := time.Now().UTC().Truncate(5 * time.Minute).Add(-30 * time.Minute)

	specs := make([]txSpec, 0, 100)
	for i := 0; i < 100; i++ {
		dur := 100 * time.Millisecond // быстрее порога 500 мс
		if i < 10 {
			dur = 1000 * time.Millisecond // медленнее порога
		}
		specs = append(specs, txSpec{offset: time.Duration(i) * time.Millisecond, dur: dur, status: "ok", env: "production"})
	}
	seedTransactions(t, conn, pid, "GET /slow", base, specs)

	p := slo.NewLatencyProvider(trace.NewQuery(conn), nil, 90)
	from, to := base, base.Add(5*time.Minute)
	bs, err := p.Buckets(ctx, slo.SLO{ProjectID: pid, Kind: slo.SLILatency, Transaction: "GET /slow", ThresholdMS: 500}, from, to, 5*time.Minute)
	if err != nil {
		t.Fatalf("Buckets: %v", err)
	}
	good, total := sumBuckets(bs)
	if total != 100 || good != 90 {
		t.Fatalf("good/total = %d/%d, want 90/100", good, total)
	}
}

func TestUptimeProvider(t *testing.T) {
	conn := testenv.MigratedCH(t)
	ctx := context.Background()

	const pid = int64(9003)
	const mid = int64(9103)
	base := time.Now().UTC().Truncate(5 * time.Minute).Add(-30 * time.Minute)

	oks := make([]bool, 10)
	for i := range oks {
		oks[i] = i != 0 // 9 из 10 успешны (первая — сбой)
	}
	seedChecks(t, conn, pid, mid, base, oks)

	monID := mid
	p := slo.NewUptimeProvider(uptime.NewQuery(conn), nil, 90)
	from, to := base, base.Add(5*time.Minute)
	bs, err := p.Buckets(ctx, slo.SLO{ProjectID: pid, Kind: slo.SLIUptime, MonitorID: &monID}, from, to, 5*time.Minute)
	if err != nil {
		t.Fatalf("Buckets: %v", err)
	}
	good, total := sumBuckets(bs)
	if total != 10 || good != 9 {
		t.Fatalf("good/total = %d/%d, want 9/10", good, total)
	}
}

// TestUptimeProviderNoMonitor — uptime-SLO без монитора → ошибка, не паника.
func TestUptimeProviderNoMonitor(t *testing.T) {
	conn := testenv.MigratedCH(t)
	p := slo.NewUptimeProvider(uptime.NewQuery(conn), nil, 90)
	if _, err := p.Buckets(context.Background(), slo.SLO{ProjectID: 1, Kind: slo.SLIUptime}, time.Now(), time.Now().Add(time.Hour), 5*time.Minute); err == nil {
		t.Fatalf("ожидалась ошибка для SLO без monitor_id")
	}
}

// TestProviderExcludesMaintenance — корзина, чей центр попал в окно
// обслуживания, отброшена; корзина вне окна остаётся.
func TestProviderExcludesMaintenance(t *testing.T) {
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedProject(t, pool)
	maint := uptime.NewService(pool)

	// Два соседних 5-минутных бакета: B1 (вне окна), B2 (внутри окна).
	b1 := time.Now().UTC().Truncate(5 * time.Minute).Add(-30 * time.Minute)
	b2 := b1.Add(5 * time.Minute)

	// Окно обслуживания накрывает B2 целиком.
	winStart := b2
	winEnd := b2.Add(5 * time.Minute)
	if _, err := maint.CreateWindow(ctx, uptime.Window{
		ProjectID: pid, Name: "maint", Weekly: false,
		StartsAt: &winStart, EndsAt: &winEnd, Timezone: "UTC",
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	seedTransactions(t, conn, pid, "GET /m", b1, []txSpec{
		{offset: 0, dur: 50 * time.Millisecond, status: "ok", env: "production"},
		{offset: time.Millisecond, dur: 50 * time.Millisecond, status: "ok", env: "production"},
	})
	seedTransactions(t, conn, pid, "GET /m", b2, []txSpec{
		{offset: 0, dur: 50 * time.Millisecond, status: "ok", env: "production"},
		{offset: time.Millisecond, dur: 50 * time.Millisecond, status: "ok", env: "production"},
	})

	q := trace.NewQuery(conn)
	from, to := b1, b2.Add(5*time.Minute)

	// Без maint — оба бакета видны.
	pNo := slo.NewAvailabilityProvider(q, nil, 90)
	bsNo, err := pNo.Buckets(ctx, slo.SLO{ProjectID: pid, Kind: slo.SLIAvailability, Transaction: "GET /m"}, from, to, 5*time.Minute)
	if err != nil {
		t.Fatalf("Buckets(no maint): %v", err)
	}
	if len(bsNo) != 2 {
		t.Fatalf("без обслуживания бакетов = %d, want 2", len(bsNo))
	}

	// С maint — B2 отброшен.
	pMaint := slo.NewAvailabilityProvider(q, maint, 90)
	bs, err := pMaint.Buckets(ctx, slo.SLO{ProjectID: pid, Kind: slo.SLIAvailability, Transaction: "GET /m"}, from, to, 5*time.Minute)
	if err != nil {
		t.Fatalf("Buckets(maint): %v", err)
	}
	if len(bs) != 1 {
		t.Fatalf("с обслуживанием бакетов = %d, want 1 (B2 отброшен)", len(bs))
	}
	if !bs[0].T.Equal(b1) {
		t.Fatalf("оставшийся бакет T = %s, want %s (B1)", bs[0].T, b1)
	}
}

// TestRetentionCap — cfg.RetentionDays → длительность клипа; 0 = без клипа.
func TestRetentionCap(t *testing.T) {
	conn := testenv.MigratedCH(t)
	q := trace.NewQuery(conn)
	if got := slo.NewAvailabilityProvider(q, nil, 90).RetentionCap(); got != 90*24*time.Hour {
		t.Fatalf("RetentionCap(90) = %s, want 2160h", got)
	}
	if got := slo.NewAvailabilityProvider(q, nil, 0).RetentionCap(); got != 0 {
		t.Fatalf("RetentionCap(0) = %s, want 0 (вечно)", got)
	}
}
