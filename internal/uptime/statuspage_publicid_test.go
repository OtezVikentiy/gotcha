package uptime_test

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestCreateStatusPageGeneratesPublicID: Create заполняет PublicID —
// непрозрачный ключ "p_" + 24 hex (12 случайных байт), не зависящий от slug.
func TestCreateStatusPageGeneratesPublicID(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	sp, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid, Title: "Any", Enabled: true,
	}, nil)
	if err != nil {
		t.Fatalf("CreateStatusPage: %v", err)
	}
	if !strings.HasPrefix(sp.PublicID, "p_") {
		t.Fatalf("CreateStatusPage: PublicID = %q, want prefix \"p_\"", sp.PublicID)
	}
	if len(sp.PublicID) != len("p_")+24 {
		t.Fatalf("CreateStatusPage: PublicID = %q, len = %d, want 26", sp.PublicID, len(sp.PublicID))
	}
	if _, err := hex.DecodeString(sp.PublicID[2:]); err != nil {
		t.Fatalf("CreateStatusPage: PublicID suffix not hex: %q: %v", sp.PublicID, err)
	}
}

// TestCreateStatusPageDuplicateMonitorFailsImmediately: PK-нарушение в
// status_page_monitors (дубль monitor_id в списке — POST /statuspages не
// дедуплицирует, см. parseStatusPageForm в internal/web) тоже даёт SQLSTATE
// 23505, но НЕ должно ложно трактоваться как коллизия public_id: retry-цикл
// в CreateStatusPage обязан вернуть эту ошибку СРАЗУ, не сжигая все 3
// попытки на детерминированно повторяющейся ошибке (см.
// statusPagePublicIDConstraint в statuspage.go).
func TestCreateStatusPageDuplicateMonitorFailsImmediately(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 3, 2)

	_, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid, Title: "Dup Monitor", Enabled: true,
	}, []uptime.StatusPageMonitor{
		{MonitorID: mon.ID, DisplayName: "One", Position: 0},
		{MonitorID: mon.ID, DisplayName: "Two", Position: 1},
	})
	if err == nil {
		t.Fatal("CreateStatusPage with duplicate monitor_id: got nil error, want PK violation")
	}
	if strings.Contains(err.Error(), "public id collision") {
		t.Fatalf("CreateStatusPage misdiagnosed duplicate monitor_id as public_id collision: %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "status_page_monitors_pkey" {
		t.Fatalf("CreateStatusPage error = %v, want pg error on status_page_monitors_pkey", err)
	}
}

// TestStatusPageByPublicID: enabled-страница резолвится по своему PublicID;
// disabled — ErrNotFound (единообразно со старым поведением по slug: скрытая
// страница неотличима от несуществующей).
func TestStatusPageByPublicID(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 3, 2)

	enabled, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid, Title: "On", Enabled: true,
	}, []uptime.StatusPageMonitor{{MonitorID: mon.ID, DisplayName: "Mon", Position: 0}})
	if err != nil {
		t.Fatalf("CreateStatusPage enabled: %v", err)
	}
	disabled, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid, Title: "Off", Enabled: false,
	}, nil)
	if err != nil {
		t.Fatalf("CreateStatusPage disabled: %v", err)
	}

	got, monitors, err := svc.StatusPageByPublicID(ctx, enabled.PublicID)
	if err != nil {
		t.Fatalf("StatusPageByPublicID(enabled): %v", err)
	}
	if got.ID != enabled.ID || got.Title != "On" {
		t.Fatalf("StatusPageByPublicID(enabled) = %+v", got)
	}
	if len(monitors) != 1 || monitors[0].MonitorID != mon.ID {
		t.Fatalf("StatusPageByPublicID(enabled) monitors = %+v", monitors)
	}

	if _, _, err := svc.StatusPageByPublicID(ctx, disabled.PublicID); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("StatusPageByPublicID(disabled) = %v, want ErrNotFound", err)
	}
}

// TestStatusPageForRedirect: legacy_slug → public_id для 301, только для
// enabled-страницы; неизвестный slug и slug выключенной страницы — (?, false).
func TestStatusPageForRedirect(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	enabled, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid, Title: "On", Enabled: true,
	}, nil)
	if err != nil {
		t.Fatalf("CreateStatusPage enabled: %v", err)
	}
	disabled, err := svc.CreateStatusPage(ctx, uptime.StatusPage{
		ProjectID: pid, Title: "Off", Enabled: false,
	}, nil)
	if err != nil {
		t.Fatalf("CreateStatusPage disabled: %v", err)
	}

	if _, err := pool.Exec(ctx,
		"INSERT INTO status_page_redirects (legacy_slug, status_page_id) VALUES ($1,$2)",
		"oldname", enabled.ID); err != nil {
		t.Fatalf("insert redirect (enabled): %v", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO status_page_redirects (legacy_slug, status_page_id) VALUES ($1,$2)",
		"oldoff", disabled.ID); err != nil {
		t.Fatalf("insert redirect (disabled): %v", err)
	}

	publicID, ok, err := svc.StatusPageForRedirect(ctx, "oldname")
	if err != nil {
		t.Fatalf("StatusPageForRedirect(oldname): %v", err)
	}
	if !ok || publicID != enabled.PublicID {
		t.Fatalf("StatusPageForRedirect(oldname) = (%q,%v), want (%q,true)", publicID, ok, enabled.PublicID)
	}

	publicID, ok, err = svc.StatusPageForRedirect(ctx, "does-not-exist")
	if err != nil {
		t.Fatalf("StatusPageForRedirect(unknown): %v", err)
	}
	if ok || publicID != "" {
		t.Fatalf("StatusPageForRedirect(unknown) = (%q,%v), want (\"\",false)", publicID, ok)
	}

	publicID, ok, err = svc.StatusPageForRedirect(ctx, "oldoff")
	if err != nil {
		t.Fatalf("StatusPageForRedirect(disabled): %v", err)
	}
	if ok || publicID != "" {
		t.Fatalf("StatusPageForRedirect(disabled) = (%q,%v), want (\"\",false)", publicID, ok)
	}
}
