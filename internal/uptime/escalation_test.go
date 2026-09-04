package uptime_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestServiceName проверяет, что ключ источника совпадает с
// incident_source='uptime', зафиксированным в incident_escalations
// (0077/0084) — W2-C находка 2.
func TestServiceName(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	if got := svc.Name(); got != "uptime" {
		t.Fatalf("Name() = %q, want %q", got, "uptime")
	}
}

// TestServiceOpenUnackedExcludesLevelZero — W2-C находка 2: в отличие от
// host/metric/trace/profile/slo, uptime.Service.OpenUnacked НЕ отдаёт
// планировщику эскалации инцидент на escalation_level=0 — эту ступень
// целиком владеет Detector (см. докблок OpenUnacked про то, зачем это
// разделение). Сразу после OpenIncident (level=0) инцидент невидим
// планировщику; после BumpEscalation(0->1), как это делает MarkNotified,
// становится видимым — с корректными ProjectID/Severity/EscalationLevel.
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

	// Acknowledge гасит инцидент из выборки.
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

// TestServiceOpenUnackedExcludesSuppressedByDep — подавленный B5-инцидент
// (даже если бы у него оказался ненулевой уровень) не должен попадать в
// эскалацию: тот же гейт, что у остальных 5 источников.
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

// TestServiceOpenUnackedRestartsClockAfterClearSuppressedByDep — F2 (аудит
// перед 1.0): выжившая мутация «убрать третий аргумент GREATEST
// (dep_released_at) в uptime OpenUnacked» — инцидент, освобождённый из-под
// подавления зависимостью, обязан вернуться в OpenUnacked с StartedAt не
// раньше момента освобождения (dep_released_at), а не с исходным
// started_at, отставшим на часы, — иначе он получил бы просроченные ступени
// лесенки каскадом на первом же тике (тот же приём, что уже есть у host,
// см. TestOpenSuppressedAndClearSuppressed).
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
	// started_at — далеко в прошлом (симулирует старый инцидент), чтобы
	// GREATEST без dep_released_at дал бы StartedAt значительно РАНЬШЕ
	// момента освобождения.
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

// timeNow — время сервера PG (не хоста теста), которым проставляется
// dep_released_at = now() внутри ClearSuppressedByDep: сравнивать со
// StartedAt нужно в одних часах.
func timeNow(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(context.Background(), "SELECT now()").Scan(&now); err != nil {
		t.Fatalf("select now(): %v", err)
	}
	return now
}

// TestServiceBumpEscalation проверяет атомарность продвижения
// escalation_level: успешный бамп двигает level и last_escalated_at,
// повторный бамп с устаревшим from — идемпотентный no-op (ok=false).
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

	// Устаревший from (гонка с другим тиком) — no-op.
	ok, err = svc.BumpEscalation(ctx, inc.ID, 0)
	if err != nil || ok {
		t.Fatalf("BumpEscalation(0) повторно = (%v,%v), want (false,nil): level уже 1", ok, err)
	}
}

// TestServiceAcknowledge — паритет с host.IncidentService.Acknowledge:
// успех, идемпотентный повтор (ok=false), кросс-тенант (ok=false, project_id
// не совпадает).
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

	// Кросс-тенант: incident принадлежит pid, не otherPid.
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

	// Повтор — идемпотентно, не ошибка.
	if ok, err := svc.Acknowledge(ctx, inc.ID, pid, userID); err != nil || ok {
		t.Fatalf("Acknowledge повторно = (%v,%v), want (false,nil): уже подтверждён", ok, err)
	}
}

// TestIncidentDeliveryExhausted — ревью аудита 2026-08-27 (усиление находки
// 1): DeliveryExhausted true ТОЛЬКО когда попытка провалилась И граница
// исчерпана — не раньше (ретрай ещё идёт, показывать тревогу рано) и не при
// успешной доставке (NotifyOpenFailed=false, сколько бы попыток ни было
// накоплено раньше — MarkNotified сбрасывает флаг, но не обязан обнулять
// счётчик атомарно с ним в этом юнит-тесте, важно поведение самого метода).
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
