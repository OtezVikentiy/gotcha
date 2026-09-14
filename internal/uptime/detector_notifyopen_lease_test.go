package uptime

// package uptime, не uptime_test: тесты зовут неэкспортируемые notifyOpen и
// settleHeldIncident напрямую, чтобы проверить лизинг клейма шага 0 в изоляции.

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func setupLeaseTest(t *testing.T) (svc *Service, notifier *OutboxNotifier, ob *notify.Outbox, ch int64, mon Monitor) {
	t.Helper()
	pool := testenv.MigratedPG(t)
	svc = NewService(pool)
	asvc := alert.NewService(pool)
	ob = notify.NewOutbox(pool)
	ctx := context.Background()

	pid := newConcurrencyTestProject(t, pool)
	var err error
	ch, err = asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	mon, err = svc.Create(ctx, concurrencyTestHTTPMonitor(pid), []string{"local"}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	notifier = &OutboxNotifier{
		Alerts: asvc, Uptime: svc, Outbox: ob, BaseURL: "https://gotcha.example",
		Details: alert.NewDetailPolicy("", nil, true), Locale: i18n.Locale{Code: "en"},
	}
	return svc, notifier, ob, ch, mon
}

// Возраст клейма не отличает "процесс умер до отправки" от "отправил, но не
// отметил" — переклейм после notifyOpenLease обязан идти через EnqueueIdempotent.
func TestNotifyOpenReclaimAfterDeliveryDoesNotDuplicateJob(t *testing.T) {
	svc, notifier, ob, ch, mon := setupLeaseTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	inc, createdNow, err := svc.OpenIncident(ctx, mon.ID, "boom", []string{"local"}, false)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	if !createdNow {
		t.Fatalf("OpenIncident: created = false, want true")
	}
	d := &Detector{Svc: svc, Notifier: notifier, Pool: svc.pool}
	ev := downEvent(mon, inc, []string{"local"}, "boom")

	d.notifyOpen(ctx, inc.ID, ev)

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs после первой отправки = %d, want 1", len(jobs))
	}

	// Состариваем клейм за notifyOpenLease, как будто с отправки прошла минута.
	if _, err := svc.pool.Exec(ctx,
		"UPDATE incident_escalations SET sent_at = sent_at - interval '2 minutes' WHERE incident_source='uptime' AND incident_id=$1",
		inc.ID); err != nil {
		t.Fatalf("age claim: %v", err)
	}

	d.notifyOpen(ctx, inc.ID, ev)

	jobs2, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim after reclaim: %v", err)
	}
	if len(jobs2) != 0 {
		t.Fatalf("jobs после переклейма уже доставленного шага = %d, want 0 (дубль пейджа)", len(jobs2))
	}

	fresh, found, err := svc.IncidentByID(ctx, inc.ID)
	if err != nil || !found {
		t.Fatalf("IncidentByID: found=%v err=%v", found, err)
	}
	if !fresh.NotifiedOpen {
		t.Fatalf("NotifiedOpen = false, want true")
	}
	_ = ch
}

// Клейм старше notifyOpenLease без соответствующего job в Outbox — брошенный
// процесс; обязан быть переклеймлен и реально отправлен, а не молчать вечно.
func TestNotifyOpenReclaimsTrulyAbandonedClaimAndSends(t *testing.T) {
	svc, notifier, ob, ch, mon := setupLeaseTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	inc, createdNow, err := svc.OpenIncident(ctx, mon.ID, "boom", []string{"local"}, false)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	if !createdNow {
		t.Fatalf("OpenIncident: created = false, want true")
	}

	// Симулируем клейм, оставленный умершим процессом ДО вызова NotifyOpenStep0:
	// строка в incident_escalations есть, но никакого job в Outbox никогда не было.
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO incident_escalations (incident_source, incident_id, step, channel_id, sent_at)
		 VALUES ('uptime', $1, 0, $2, now() - interval '2 minutes')`, inc.ID, ch); err != nil {
		t.Fatalf("seed abandoned claim: %v", err)
	}

	d := &Detector{Svc: svc, Notifier: notifier, Pool: svc.pool}
	ev := downEvent(mon, inc, []string{"local"}, "boom")
	d.notifyOpen(ctx, inc.ID, ev)

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want ровно 1 (переклейм брошенного клейма обязан реально отправить)", len(jobs))
	}
	if jobs[0].ChannelID != ch {
		t.Fatalf("job channel = %d, want %d", jobs[0].ChannelID, ch)
	}

	fresh, found, err := svc.IncidentByID(ctx, inc.ID)
	if err != nil || !found {
		t.Fatalf("IncidentByID: found=%v err=%v", found, err)
	}
	if !fresh.NotifiedOpen {
		t.Fatalf("NotifiedOpen = false, want true: тишина по немому клейму не должна быть вечной")
	}
}

// OpenIncident зовётся напрямую, минуя Detector.openIncident — notifyOpen
// ещё не звался, NotifiedOpen=false, а Dep == nil.
func TestSettleHeldIncidentWithoutDepRetriesNotifyOpen(t *testing.T) {
	svc, notifier, ob, ch, mon := setupLeaseTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	inc, createdNow, err := svc.OpenIncident(ctx, mon.ID, "boom", []string{"local"}, false)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	if !createdNow {
		t.Fatalf("OpenIncident: created = false, want true")
	}
	if inc.NotifiedOpen {
		t.Fatalf("test setup: NotifiedOpen = true, want false (notifyOpen ещё не звался)")
	}

	d := &Detector{Svc: svc, Notifier: notifier, Pool: svc.pool} // Dep == nil
	st := State{MonitorID: mon.ID, Region: "local", Status: "down"}
	states := []State{st}

	d.settleHeldIncident(ctx, mon, inc, states, st, time.Now().UTC())

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want ровно 1: settleHeldIncident без Dep обязан позвать notifyOpen", len(jobs))
	}
	if jobs[0].ChannelID != ch {
		t.Fatalf("job channel = %d, want %d", jobs[0].ChannelID, ch)
	}

	fresh, found, err := svc.IncidentByID(ctx, inc.ID)
	if err != nil || !found {
		t.Fatalf("IncidentByID: found=%v err=%v", found, err)
	}
	if !fresh.NotifiedOpen {
		t.Fatalf("NotifiedOpen = false, want true")
	}
}
