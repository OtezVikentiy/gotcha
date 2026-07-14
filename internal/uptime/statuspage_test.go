package uptime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestStatusPageCreateAndFindBySlug(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 3, 2)

	sp, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid,
		Slug:      "status",
		Title:     "API Status",
		Enabled:   true,
	}, []uptime.StatusPageMonitor{{MonitorID: mon.ID, DisplayName: "API", Position: 0}})
	if err != nil {
		t.Fatalf("CreateStatusPage: %v", err)
	}
	if sp.ID == 0 {
		t.Fatalf("CreateStatusPage: id = 0")
	}

	got, monitors, err := svc.StatusPageBySlug(ctx, "status")
	if err != nil {
		t.Fatalf("StatusPageBySlug: %v", err)
	}
	if got.ID != sp.ID || got.Title != "API Status" {
		t.Fatalf("StatusPageBySlug: %+v", got)
	}
	if len(monitors) != 1 || monitors[0].MonitorID != mon.ID || monitors[0].DisplayName != "API" {
		t.Fatalf("StatusPageBySlug monitors: %+v", monitors)
	}

	list, err := svc.StatusPagesOf(ctx, pid)
	if err != nil {
		t.Fatalf("StatusPagesOf: %v", err)
	}
	if len(list) != 1 || list[0].ID != sp.ID {
		t.Fatalf("StatusPagesOf: %+v", list)
	}
}

func TestStatusPageDuplicateSlugTaken(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	if _, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid, Slug: "dup", Title: "First", Enabled: true,
	}, nil); err != nil {
		t.Fatalf("CreateStatusPage first: %v", err)
	}

	other := newProject(t, pool)
	if _, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: other, Slug: "dup", Title: "Second", Enabled: true,
	}, nil); !errors.Is(err, uptime.ErrSlugTaken) {
		t.Fatalf("CreateStatusPage dup slug: err = %v, want ErrSlugTaken", err)
	}
}

func TestStatusPageInvalidSlugOrTitle(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	if _, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid, Slug: "Bad Slug!", Title: "X", Enabled: true,
	}, nil); !errors.Is(err, uptime.ErrInvalidStatusPage) {
		t.Fatalf("CreateStatusPage bad slug: err = %v, want ErrInvalidStatusPage", err)
	}

	if _, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid, Slug: "no-title", Title: "", Enabled: true,
	}, nil); !errors.Is(err, uptime.ErrInvalidStatusPage) {
		t.Fatalf("CreateStatusPage empty title: err = %v, want ErrInvalidStatusPage", err)
	}
}

func TestStatusPageDisabledOrUnknownSlugNotFound(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	if _, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid, Slug: "hidden", Title: "Hidden", Enabled: false,
	}, nil); err != nil {
		t.Fatalf("CreateStatusPage disabled: %v", err)
	}

	if _, _, err := svc.StatusPageBySlug(ctx, "hidden"); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("StatusPageBySlug disabled: err = %v, want ErrNotFound", err)
	}
	if _, _, err := svc.StatusPageBySlug(ctx, "does-not-exist"); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("StatusPageBySlug unknown slug: err = %v, want ErrNotFound", err)
	}
}

func TestUpdateAndDeleteStatusPage(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)
	mon1 := createMonitor(t, svc, pid, 3, 2)
	mon2 := createMonitor(t, svc, pid, 3, 2)

	sp, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid, Slug: "up", Title: "Up", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: mon1.ID, DisplayName: "One", Position: 0}})
	if err != nil {
		t.Fatalf("CreateStatusPage: %v", err)
	}

	sp.Title = "Up v2"
	sp.Enabled = true
	if err := svc.UpdateStatusPage(ctx, sp, []uptime.StatusPageMonitor{
		{MonitorID: mon2.ID, DisplayName: "Two", Position: 0},
	}); err != nil {
		t.Fatalf("UpdateStatusPage: %v", err)
	}

	got, monitors, err := svc.StatusPageBySlug(ctx, "up")
	if err != nil {
		t.Fatalf("StatusPageBySlug after update: %v", err)
	}
	if got.Title != "Up v2" {
		t.Fatalf("UpdateStatusPage: title = %q, want %q", got.Title, "Up v2")
	}
	if len(monitors) != 1 || monitors[0].MonitorID != mon2.ID || monitors[0].DisplayName != "Two" {
		t.Fatalf("UpdateStatusPage monitors not replaced: %+v", monitors)
	}

	if err := svc.UpdateStatusPage(ctx, uptime.StatusPage{ID: 999999999, Slug: "ghost", Title: "Ghost"}, nil); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("UpdateStatusPage unknown id: err = %v, want ErrNotFound", err)
	}

	if err := svc.DeleteStatusPage(ctx, sp.ID); err != nil {
		t.Fatalf("DeleteStatusPage: %v", err)
	}
	if err := svc.DeleteStatusPage(ctx, sp.ID); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("DeleteStatusPage again: err = %v, want ErrNotFound", err)
	}
	if _, _, err := svc.StatusPageBySlug(ctx, "up"); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("StatusPageBySlug after delete: err = %v, want ErrNotFound", err)
	}
}
