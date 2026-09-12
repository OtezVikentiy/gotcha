package uptime

// package uptime, не uptime_test: тест зовёт неэкспортируемый notifyOpen
// напрямую (второй регион того же монитора, или вторая реплика).

import (
	"context"
	"sync"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// 20 конкурентных вызовов на один инцидент — ровно одна реальная отправка.
func TestNotifyOpenConcurrentCallsSendExactlyOnce(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newConcurrencyTestProject(t, pool)
	ch, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	mon, err := svc.Create(ctx, concurrencyTestHTTPMonitor(pid), []string{"local"}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	inc, createdNow, err := svc.OpenIncident(ctx, mon.ID, "boom", []string{"local"}, false)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	if !createdNow {
		t.Fatalf("OpenIncident: created = false, want true")
	}

	notifier := &OutboxNotifier{
		Alerts: asvc, Uptime: svc, Outbox: ob, BaseURL: "https://gotcha.example",
		Details: alert.NewDetailPolicy("", nil, true), Locale: i18n.Locale{Code: "en"},
	}
	d := &Detector{Svc: svc, Notifier: notifier, Pool: pool}
	ev := downEvent(mon, inc, []string{"local"}, "boom")

	const n = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			d.notifyOpen(ctx, inc.ID, ev)
		}()
	}
	close(start)
	wg.Wait()

	jobs, err := ob.Claim(ctx, 100)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("down jobs = %d, want exactly 1 (20 конкурентных notifyOpen на один инцидент)", len(jobs))
	}
	if jobs[0].ChannelID != ch {
		t.Fatalf("job channel = %d, want %d", jobs[0].ChannelID, ch)
	}

	fresh, found, err := svc.IncidentByID(ctx, inc.ID)
	if err != nil || !found {
		t.Fatalf("IncidentByID: found=%v err=%v", found, err)
	}
	if !fresh.NotifiedOpen {
		t.Fatalf("NotifiedOpen = false, want true после конкурентных notifyOpen")
	}

	var loggedCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='uptime' AND incident_id=$1 AND step=0",
		inc.ID).Scan(&loggedCount); err != nil {
		t.Fatalf("select incident_escalations: %v", err)
	}
	if loggedCount != 1 {
		t.Fatalf("incident_escalations rows = %d, want ровно 1", loggedCount)
	}
}
