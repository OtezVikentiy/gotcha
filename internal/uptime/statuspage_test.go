package uptime_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestStatusPageCreateAndFindByPublicID(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 3, 2)

	sp, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid,
		Title:     "API Status",
		Enabled:   true,
	}, []uptime.StatusPageMonitor{{MonitorID: mon.ID, DisplayName: "API", Position: 0}})
	if err != nil {
		t.Fatalf("CreateStatusPage: %v", err)
	}
	if sp.ID == 0 {
		t.Fatalf("CreateStatusPage: id = 0")
	}

	if len(sp.PublicID) != 26 || !strings.HasPrefix(sp.PublicID, "p_") {
		t.Fatalf("CreateStatusPage: PublicID = %q, want \"p_\"+24hex", sp.PublicID)
	}

	got, monitors, err := svc.StatusPageByPublicID(ctx, sp.PublicID)
	if err != nil {
		t.Fatalf("StatusPageByPublicID: %v", err)
	}
	if got.ID != sp.ID || got.Title != "API Status" {
		t.Fatalf("StatusPageByPublicID: %+v", got)
	}
	if len(monitors) != 1 || monitors[0].MonitorID != mon.ID || monitors[0].DisplayName != "API" {
		t.Fatalf("StatusPageByPublicID monitors: %+v", monitors)
	}

	list, err := svc.StatusPagesOf(ctx, pid)
	if err != nil {
		t.Fatalf("StatusPagesOf: %v", err)
	}
	if len(list) != 1 || list[0].ID != sp.ID {
		t.Fatalf("StatusPagesOf: %+v", list)
	}
}

func TestStatusPageInvalidTitle(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	if _, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid, Title: "", Enabled: true,
	}, nil); !errors.Is(err, uptime.ErrInvalidStatusPage) {
		t.Fatalf("CreateStatusPage empty title: err = %v, want ErrInvalidStatusPage", err)
	}
}

func TestStatusPageDisabledOrUnknownPublicIDNotFound(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	sp, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid, Title: "Hidden", Enabled: false,
	}, nil)
	if err != nil {
		t.Fatalf("CreateStatusPage disabled: %v", err)
	}

	if _, _, err := svc.StatusPageByPublicID(ctx, sp.PublicID); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("StatusPageByPublicID disabled: err = %v, want ErrNotFound", err)
	}
	if _, _, err := svc.StatusPageByPublicID(ctx, "p_does000not000exist00000"); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("StatusPageByPublicID unknown key: err = %v, want ErrNotFound", err)
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
		ProjectID: pid, Title: "Up", Enabled: true,
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

	got, monitors, err := svc.StatusPageByPublicID(ctx, sp.PublicID)
	if err != nil {
		t.Fatalf("StatusPageByPublicID after update: %v", err)
	}
	if got.Title != "Up v2" {
		t.Fatalf("UpdateStatusPage: title = %q, want %q", got.Title, "Up v2")
	}
	if len(monitors) != 1 || monitors[0].MonitorID != mon2.ID || monitors[0].DisplayName != "Two" {
		t.Fatalf("UpdateStatusPage monitors not replaced: %+v", monitors)
	}

	if err := svc.UpdateStatusPage(ctx, uptime.StatusPage{ID: 999999999, Title: "Ghost"}, nil); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("UpdateStatusPage unknown id: err = %v, want ErrNotFound", err)
	}

	if err := svc.DeleteStatusPage(ctx, sp.ID); err != nil {
		t.Fatalf("DeleteStatusPage: %v", err)
	}
	if err := svc.DeleteStatusPage(ctx, sp.ID); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("DeleteStatusPage again: err = %v, want ErrNotFound", err)
	}
	if _, _, err := svc.StatusPageByPublicID(ctx, sp.PublicID); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("StatusPageByPublicID after delete: err = %v, want ErrNotFound", err)
	}
}
