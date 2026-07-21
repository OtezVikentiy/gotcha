package uptime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestApplyResultInvalidMonitor: для неизвестного monitorID CTE thresholds
// пуста → INSERT ... SELECT не даёт строк → RETURNING ничего не возвращает →
// ErrInvalidMonitor (а не сырое нарушение FK). Отменённый ctx покрывает общую
// ветку ошибки запроса.
func TestApplyResultInvalidMonitor(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx := context.Background()

	if _, err := svc.ApplyResult(ctx, 999999, "local", false, "boom", time.Now().UTC()); !errors.Is(err, uptime.ErrInvalidMonitor) {
		t.Fatalf("ApplyResult(unknown monitor) = %v, want ErrInvalidMonitor", err)
	}

	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.ApplyResult(cctx, 1, "local", false, "boom", time.Now().UTC()); err == nil {
		t.Fatal("ApplyResult on cancelled ctx: got nil, want error")
	} else if errors.Is(err, uptime.ErrInvalidMonitor) {
		t.Fatalf("ApplyResult on cancelled ctx = %v, want DB error not ErrInvalidMonitor", err)
	}
}

// TestStatusPageByID покрывает StatusPageByID (0%): успешное чтение по id,
// ErrNotFound на неизвестном id и ошибку запроса на отменённом ctx.
func TestStatusPageByID(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx := context.Background()
	pid := newProject(t, pool)

	sp, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid, Slug: "byid", Title: "By ID", Enabled: true,
	}, nil)
	if err != nil {
		t.Fatalf("CreateStatusPage: %v", err)
	}

	got, err := svc.StatusPageByID(ctx, sp.ID)
	if err != nil {
		t.Fatalf("StatusPageByID: %v", err)
	}
	if got.ID != sp.ID || got.Slug != "byid" || got.ProjectID != pid || got.Title != "By ID" {
		t.Fatalf("StatusPageByID = %+v, want id=%d slug=byid", got, sp.ID)
	}

	if _, err := svc.StatusPageByID(ctx, 999999999); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("StatusPageByID(missing) = %v, want ErrNotFound", err)
	}

	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.StatusPageByID(cctx, sp.ID); err == nil {
		t.Fatal("StatusPageByID on cancelled ctx: got nil, want error")
	} else if errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("StatusPageByID on cancelled ctx = %v, want DB error not ErrNotFound", err)
	}
}

// TestCreateStatusPageBogusMonitorFKError: несуществующий monitor_id нарушает
// FK status_page_monitors→monitors, поэтому вставка в replaceStatusPageMonitors
// падает — покрывает ветку ошибки INSERT внутри цикла. Страница при этом не
// должна остаться (транзакция откатилась).
func TestCreateStatusPageBogusMonitorFKError(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx := context.Background()
	pid := newProject(t, pool)

	_, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid, Slug: "fk", Title: "FK", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: 999999999, DisplayName: "Ghost", Position: 0}})
	if err == nil {
		t.Fatal("CreateStatusPage with bogus monitor_id: got nil error, want FK error")
	}

	// Откат: страница не создана.
	if _, _, err := svc.StatusPageBySlug(ctx, "fk"); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("status page must not persist after rollback: err = %v, want ErrNotFound", err)
	}
}

// TestTouchHeartbeat покрывает TouchHeartbeat (0%): успешный UPDATE ставит
// last_beat_at, неизвестный monitorID → ErrNotFound, отменённый ctx → ошибка.
func TestTouchHeartbeat(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx := context.Background()
	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 3, 2)

	if err := svc.TouchHeartbeat(ctx, mon.ID); err != nil {
		t.Fatalf("TouchHeartbeat: %v", err)
	}
	var beat *time.Time
	if err := pool.QueryRow(ctx, "SELECT last_beat_at FROM monitors WHERE id = $1", mon.ID).Scan(&beat); err != nil {
		t.Fatalf("read last_beat_at: %v", err)
	}
	if beat == nil {
		t.Fatal("last_beat_at is nil after TouchHeartbeat, want set")
	}

	if err := svc.TouchHeartbeat(ctx, 999999999); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("TouchHeartbeat(missing) = %v, want ErrNotFound", err)
	}

	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.TouchHeartbeat(cctx, mon.ID); err == nil {
		t.Fatal("TouchHeartbeat on cancelled ctx: got nil, want error")
	} else if errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("TouchHeartbeat on cancelled ctx = %v, want DB error not ErrNotFound", err)
	}
}
