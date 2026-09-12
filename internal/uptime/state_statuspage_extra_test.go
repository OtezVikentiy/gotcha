package uptime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

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

func TestStatusPageByID(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx := context.Background()
	pid := newProject(t, pool)

	sp, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid, Title: "By ID", Enabled: true,
	}, nil)
	if err != nil {
		t.Fatalf("CreateStatusPage: %v", err)
	}

	got, err := svc.StatusPageByID(ctx, sp.ID)
	if err != nil {
		t.Fatalf("StatusPageByID: %v", err)
	}
	// slug nullable, не годится как источник проверки — сверяем PublicID.
	if got.ID != sp.ID || got.PublicID != sp.PublicID || got.ProjectID != pid || got.Title != "By ID" {
		t.Fatalf("StatusPageByID = %+v, want id=%d public_id=%s", got, sp.ID, sp.PublicID)
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

func TestCreateStatusPageBogusMonitorFKError(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx := context.Background()
	pid := newProject(t, pool)

	_, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid, Title: "FK", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: 999999999, DisplayName: "Ghost", Position: 0}})
	if err == nil {
		t.Fatal("CreateStatusPage with bogus monitor_id: got nil error, want FK error")
	}

	// PublicID взять нечем — Create упал до коммита; проверяем список проекта, он пуст.
	list, err := svc.StatusPagesOf(ctx, pid)
	if err != nil {
		t.Fatalf("StatusPagesOf: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("status page must not persist after rollback: %+v", list)
	}
}

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
