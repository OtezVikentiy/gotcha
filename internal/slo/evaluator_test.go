package slo_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// capturingNotifier копит SLOEvent, чтобы тест проверил, что уведомление ушло
// ровно один раз на открытие и один раз на закрытие.
type capturingNotifier struct {
	mu     sync.Mutex
	events []slo.SLOEvent
}

func (c *capturingNotifier) Notify(_ context.Context, ev slo.SLOEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *capturingNotifier) snapshot() []slo.SLOEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]slo.SLOEvent, len(c.events))
	copy(out, c.events)
	return out
}

// goodBadSpecs — n транзакций, первые bad помечены сбоем (badRate = bad/n).
func goodBadSpecs(n, bad int, env string) []txSpec {
	specs := make([]txSpec, 0, n)
	for i := 0; i < n; i++ {
		status := "ok"
		if i < bad {
			status = "internal_error"
		}
		specs = append(specs, txSpec{offset: time.Duration(i) * time.Millisecond, dur: 50 * time.Millisecond, status: status, env: env})
	}
	return specs
}

// TestSLOEvaluatorOpensAndCloses — прожог бюджета открывает инцидент и шлёт
// уведомление; повторный тик при том же прожоге идемпотентен; после остывания
// короткого окна инцидент закрывается только на defaultCloseStreak-й тик подряд
// (флап-защита), не раньше.
func TestSLOEvaluatorOpensAndCloses(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres and clickhouse containers")
	}
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)

	s, err := st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: "checkout", Kind: slo.SLIAvailability,
		Target: 0.99, WindowDays: 30, Transaction: "GET /checkout",
		BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Прожог: 100 транзакций в свежем бакете, 20 плохих → badRate 0.2 →
	// burn 0.2/(1-0.99)=20× > порог 14.4.
	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC().Add(-10*time.Minute), goodBadSpecs(100, 20, "production"))

	notifier := &capturingNotifier{}
	e := &slo.Evaluator{
		Pool:      pool,
		Store:     st,
		Providers: slo.Providers(trace.NewQuery(conn), nil, nil, 90),
		Notifier:  notifier,
	}

	// Открытие.
	n, err := e.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick(open): %v", err)
	}
	if n != 1 {
		t.Fatalf("переходов при открытии = %d, want 1", n)
	}
	incs, err := st.Incidents(ctx, pid, s.ID, 10)
	if err != nil || len(incs) != 1 || incs[0].Status != "open" {
		t.Fatalf("инцидент не открыт: %+v err=%v", incs, err)
	}
	evs := notifier.snapshot()
	if len(evs) != 1 || !evs[0].Opened {
		t.Fatalf("нет notify открытия: %+v", evs)
	}
	if evs[0].BurnRate < 14.4 {
		t.Fatalf("burn при открытии = %v, want >= 14.4", evs[0].BurnRate)
	}

	// Повторный тик при том же прожоге — второй инцидент не создаётся.
	if n2, err := e.Tick(ctx); err != nil || n2 != 0 {
		t.Fatalf("повторный тик при открытом инциденте: переходов %d err=%v, want 0", n2, err)
	}

	// Остывание: свежий бакет полностью хороший → короткое (последнее) окно < порога.
	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC().Add(-1*time.Minute), goodBadSpecs(100, 0, "production"))

	// Флап-защита: два тика остывания подряд НЕ закрывают инцидент.
	for i := 0; i < 2; i++ {
		n3, err := e.Tick(ctx)
		if err != nil || n3 != 0 {
			t.Fatalf("тик %d остывания: переходов %d err=%v, want 0 (рано закрывать)", i+1, n3, err)
		}
		cur, _ := st.Incidents(ctx, pid, s.ID, 10)
		if len(cur) == 0 || cur[0].Status != "open" {
			t.Fatalf("инцидент закрыт раньше defaultCloseStreak тиков: %+v", cur)
		}
	}

	// Третий тик остывания подряд — закрывает.
	n4, err := e.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick(close): %v", err)
	}
	if n4 != 1 {
		t.Fatalf("переходов при закрытии = %d, want 1", n4)
	}
	closed, _ := st.Incidents(ctx, pid, s.ID, 10)
	if len(closed) == 0 || closed[0].Status != "resolved" {
		t.Fatalf("инцидент не закрыт после 3 тиков остывания: %+v", closed)
	}
	evs = notifier.snapshot()
	if len(evs) < 2 {
		t.Fatalf("ожидались уведомления открытия и закрытия, есть %d", len(evs))
	}
	if evs[len(evs)-1].Opened {
		t.Fatalf("последнее уведомление должно быть закрытием: %+v", evs[len(evs)-1])
	}
}
