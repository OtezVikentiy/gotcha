package uptime_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestServiceName(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	if got := svc.Name(); got != "uptime" {
		t.Fatalf("Name() = %q, want %q", got, "uptime")
	}
}

func TestServiceOpenUnackedExcludesLevelZero(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx := context.Background()
	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	inc, created, err := svc.OpenIncident(ctx, mon.ID, "boom", []string{"local"}, false)
	if err != nil || !created {
		t.Fatalf("OpenIncident: (%+v,%v,%v)", inc, created, err)
	}

	list, err := svc.OpenUnacked(ctx)
	if err != nil {
		t.Fatalf("OpenUnacked: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("OpenUnacked на escalation_level=0 = %d записей, want 0 (Detector ещё не отдал первую доставку): %+v", len(list), list)
	}

	ok, err := svc.BumpEscalation(ctx, inc.ID, 0)
	if err != nil || !ok {
		t.Fatalf("BumpEscalation(0): (%v,%v), want (true,nil)", ok, err)
	}

	list, err = svc.OpenUnacked(ctx)
	if err != nil {
		t.Fatalf("OpenUnacked после bump: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("OpenUnacked после bump до level=1 = %d записей, want 1: %+v", len(list), list)
	}
	got := list[0]
	if got.ID != inc.ID || got.ProjectID != pid {
		t.Fatalf("OpenUnacked[0] = %+v, want ID=%d ProjectID=%d", got, inc.ID, pid)
	}
	if got.Severity != "critical" {
		t.Fatalf("OpenUnacked[0].Severity = %q, want %q (uptime-дефолт из 0084)", got.Severity, "critical")
	}
	if got.EscalationLevel != 1 {
		t.Fatalf("OpenUnacked[0].EscalationLevel = %d, want 1", got.EscalationLevel)
	}

	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", "uptime-esc-ack@e.com").
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if ok, err := svc.Acknowledge(ctx, inc.ID, pid, userID); err != nil || !ok {
		t.Fatalf("Acknowledge: (%v,%v), want (true,nil)", ok, err)
	}
	if list, err = svc.OpenUnacked(ctx); err != nil || len(list) != 0 {
		t.Fatalf("OpenUnacked после Acknowledge = %d/%v, want 0", len(list), err)
	}
}

func TestServiceOpenUnackedExcludesSuppressedByDep(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx := context.Background()
	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	inc, _, err := svc.OpenIncident(ctx, mon.ID, "boom", []string{"local"}, false)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	if _, err := svc.BumpEscalation(ctx, inc.ID, 0); err != nil {
		t.Fatalf("BumpEscalation: %v", err)
	}
	if err := svc.MarkSuppressedByDep(ctx, inc.ID); err != nil {
		t.Fatalf("MarkSuppressedByDep: %v", err)
	}

	list, err := svc.OpenUnacked(ctx)
	if err != nil {
		t.Fatalf("OpenUnacked: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("OpenUnacked с suppressed_by_dep=true = %d записей, want 0: %+v", len(list), list)
	}
}

func TestServiceOpenUnackedRestartsClockAfterClearSuppressedByDep(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx := context.Background()
	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	inc, _, err := svc.OpenIncident(ctx, mon.ID, "boom", []string{"local"}, false)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE incidents SET started_at = now() - interval '2 hours' WHERE id = $1", inc.ID); err != nil {
		t.Fatalf("backdate started_at: %v", err)
	}
	if _, err := svc.BumpEscalation(ctx, inc.ID, 0); err != nil {
		t.Fatalf("BumpEscalation: %v", err)
	}
	if err := svc.MarkSuppressedByDep(ctx, inc.ID); err != nil {
		t.Fatalf("MarkSuppressedByDep: %v", err)
	}

	before := timeNow(t, pool)
	if err := svc.ClearSuppressedByDep(ctx, inc.ID); err != nil {
		t.Fatalf("ClearSuppressedByDep: %v", err)
	}

	list, err := svc.OpenUnacked(ctx)
	if err != nil {
		t.Fatalf("OpenUnacked: %v", err)
	}
	if len(list) != 1 || list[0].ID != inc.ID {
		t.Fatalf("OpenUnacked = %+v, want [инцидент %d] (подавление снято)", list, inc.ID)
	}
	if list[0].StartedAt.Before(before) {
		t.Fatalf("OpenUnacked[0].StartedAt = %v, want не раньше момента ClearSuppressedByDep (%v) — часы должны перезапуститься от dep_released_at", list[0].StartedAt, before)
	}
}

// время сервера PG, не хоста теста — dep_released_at ставится через now() в БД.
func timeNow(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(context.Background(), "SELECT now()").Scan(&now); err != nil {
		t.Fatalf("select now(): %v", err)
	}
	return now
}

func TestServiceBumpEscalation(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx := context.Background()
	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	inc, _, err := svc.OpenIncident(ctx, mon.ID, "boom", []string{"local"}, false)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}

	ok, err := svc.BumpEscalation(ctx, inc.ID, 0)
	if err != nil || !ok {
		t.Fatalf("BumpEscalation(0) = (%v,%v), want (true,nil)", ok, err)
	}
	reloaded, found, err := svc.IncidentByID(ctx, inc.ID)
	if err != nil || !found {
		t.Fatalf("IncidentByID: (%+v,%v,%v)", reloaded, found, err)
	}
	if reloaded.EscalationLevel != 1 || reloaded.LastEscalatedAt == nil {
		t.Fatalf("after bump: EscalationLevel=%d LastEscalatedAt=%v, want 1/non-nil", reloaded.EscalationLevel, reloaded.LastEscalatedAt)
	}

	ok, err = svc.BumpEscalation(ctx, inc.ID, 0)
	if err != nil || ok {
		t.Fatalf("BumpEscalation(0) повторно = (%v,%v), want (false,nil): level уже 1", ok, err)
	}
}

func TestServiceAcknowledge(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx := context.Background()
	pid := newProject(t, pool)
	otherPid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	inc, _, err := svc.OpenIncident(ctx, mon.ID, "boom", []string{"local"}, false)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", "uptime-ack@e.com").
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	if ok, err := svc.Acknowledge(ctx, inc.ID, otherPid, userID); err != nil || ok {
		t.Fatalf("Acknowledge(wrong project) = (%v,%v), want (false,nil)", ok, err)
	}

	if ok, err := svc.Acknowledge(ctx, inc.ID, pid, userID); err != nil || !ok {
		t.Fatalf("Acknowledge = (%v,%v), want (true,nil)", ok, err)
	}
	reloaded, found, err := svc.IncidentByID(ctx, inc.ID)
	if err != nil || !found {
		t.Fatalf("IncidentByID: (%+v,%v,%v)", reloaded, found, err)
	}
	if reloaded.AcknowledgedAt == nil || reloaded.AcknowledgedBy == nil || *reloaded.AcknowledgedBy != userID {
		t.Fatalf("after ack: AcknowledgedAt=%v AcknowledgedBy=%v, want set/%d", reloaded.AcknowledgedAt, reloaded.AcknowledgedBy, userID)
	}

	if ok, err := svc.Acknowledge(ctx, inc.ID, pid, userID); err != nil || ok {
		t.Fatalf("Acknowledge повторно = (%v,%v), want (false,nil): уже подтверждён", ok, err)
	}
}

func TestIncidentDeliveryExhausted(t *testing.T) {
	cases := []struct {
		name     string
		failed   bool
		attempts int
		want     bool
	}{
		{"не пытались", false, 0, false},
		{"провалились, попыток меньше границы", true, 4, false},
		{"провалились, попыток на границе", true, 5, true},
		{"провалились, попыток больше границы", true, 6, true},
		{"доставлено, попыток накоплено, но флаг снят", false, 5, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inc := uptime.Incident{NotifyOpenFailed: c.failed, NotifyOpenAttempts: c.attempts}
			if got := inc.DeliveryExhausted(); got != c.want {
				t.Errorf("DeliveryExhausted() = %v, want %v (failed=%v attempts=%d)", got, c.want, c.failed, c.attempts)
			}
		})
	}
}

func TestOpenUnackedSkipsDisabledMonitor(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx := context.Background()
	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	inc, created, err := svc.OpenIncident(ctx, mon.ID, "boom", []string{"local"}, false)
	if err != nil || !created {
		t.Fatalf("OpenIncident: (%+v,%v,%v)", inc, created, err)
	}
	if ok, err := svc.BumpEscalation(ctx, inc.ID, 0); err != nil || !ok {
		t.Fatalf("BumpEscalation(0): (%v,%v), want (true,nil)", ok, err)
	}

	pending := func(step string) bool {
		t.Helper()
		list, err := svc.OpenUnacked(ctx)
		if err != nil {
			t.Fatalf("OpenUnacked %s: %v", step, err)
		}
		for _, p := range list {
			if p.ID == inc.ID {
				return true
			}
		}
		return false
	}

	if !pending("enabled") {
		t.Fatalf("OpenUnacked while monitor enabled: incident %d missing, want it pending", inc.ID)
	}
	if err := svc.SetEnabled(ctx, mon.ID, false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	if pending("disabled") {
		t.Fatalf("OpenUnacked while monitor disabled: incident %d listed, want it skipped", inc.ID)
	}
	if still := assertOpenIncident(t, ctx, svc, mon.ID); still.ID != inc.ID || still.ResolvedAt != nil {
		t.Fatalf("incident after pause = %+v, want the same one still open (pause must not resolve it)", still)
	}
	if err := svc.SetEnabled(ctx, mon.ID, true); err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}
	if !pending("re-enabled") {
		t.Fatalf("OpenUnacked after unpause: incident %d missing, want it pending again", inc.ID)
	}
}
