package slo_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// mockMaint — slo.MaintenanceChecker для тестов: func-обёртка вместо
// полноценного uptime.Service (интерфейс здесь в один метод — реальный сервис
// с окнами обслуживания и своей БД тестам этого пакета не нужен). Калька
// host.mockMaint / trace.mockMaint / profile.mockMaint (Task 3/5/6).
type mockMaint func(ctx context.Context, projectID int64, at time.Time) (bool, error)

func (m mockMaint) InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error) {
	return m(ctx, projectID, at)
}

// TestSLOEvaluatorMaintenanceSuppressesNotify — B3 Task 6: открытие инцидента
// сжигания бюджета в окне обслуживания (Maint→true) пишет slo_incidents с
// InMaintenance=true, но НЕ уведомляет; закрытие того же инцидента (ещё
// внутри окна, после флап-защиты) тоже не уведомляет. Зеркало
// trace.TestEvaluatorMaintenanceSuppressesRegressionNotify (Task 5).
func TestSLOEvaluatorMaintenanceSuppressesNotify(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres and clickhouse containers")
	}
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)

	s, err := st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: "checkout maint", Kind: slo.SLIAvailability,
		Target: 0.99, WindowDays: 30, Transaction: "GET /checkout",
		BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Прожог: 100 транзакций в свежем бакете, 20 плохих → badRate 0.2 →
	// burn 0.2/(1-0.99)=20× > порог 14.4.
	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC().Add(-10*time.Minute), goodBadSpecs(100, 20, "production"))

	notifier := &capturingNotifier{store: st}
	e := &slo.Evaluator{
		Pool:      pool,
		Store:     st,
		Providers: slo.Providers(trace.NewQuery(conn), nil, nil, 90),
		Notifier:  notifier,
		Policy:    escalation.NewPolicyStore(pool),
		Maint:     mockMaint(func(context.Context, int64, time.Time) (bool, error) { return true, nil }),
	}

	// Открытие: инцидент должен быть создан (окно обслуживания не отменяет
	// открытие), но уведомление подавлено.
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
	if !incs[0].InMaintenance {
		t.Error("InMaintenance = false, want true (открыт в окне)")
	}
	if evs := notifier.snapshot(); len(evs) != 0 {
		t.Errorf("notify events after open tick = %d, want 0 (suppressed by maintenance)", len(evs))
	}

	// Остывание: свежий бакет полностью хороший → короткое (последнее) окно < порога.
	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC().Add(-1*time.Minute), goodBadSpecs(100, 0, "production"))

	// Флап-защита: два тика остывания подряд НЕ закрывают инцидент.
	for i := 0; i < 2; i++ {
		if n3, err := e.Tick(ctx); err != nil || n3 != 0 {
			t.Fatalf("тик %d остывания: переходов %d err=%v, want 0 (рано закрывать)", i+1, n3, err)
		}
	}

	// Третий тик остывания подряд — закрывает. Окно обслуживания всё ещё
	// активно (mockMaint не менялся) — закрытие тоже не должно уведомлять.
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
	if evs := notifier.snapshot(); len(evs) != 0 {
		t.Errorf("notify events after resolve tick = %d, want still 0 (close-notify suppressed too)", len(evs))
	}
}

// TestSLOEvaluatorMaintenanceFalseStillNotifies — Maint сконфигурирован (не
// nil), но вне окна (InMaintenance→false): поведение обычное, уведомление
// уходит. Отличает «MaintenanceChecker сконфигурирован и говорит false» от
// «MaintenanceChecker==nil» (последнее уже покрыто TestSLOEvaluatorOpensAndCloses).
func TestSLOEvaluatorMaintenanceFalseStillNotifies(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres and clickhouse containers")
	}
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)
	// Дефолт-лесенка эскалации резолвится из РЕАЛЬНЫХ enabled-каналов проекта
	// (escalation.PolicyStore.defaultLadder) — без единого канала её
	// ChannelIDs пуст, и claim-before-notify (аудит K1-1) бампит ступень
	// НАПРЯМУЮ, не зовя notifyStep вовсе (уже нечего занимать/слать). Этому
	// тесту важно, что notifyStep реально позван при открытии вне окна
	// обслуживания — нужен хотя бы один канал, как в TestSLOEvaluatorOpensAndCloses.
	if _, err := alert.NewService(pool).CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	s, err := st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: "checkout nomaint", Kind: slo.SLIAvailability,
		Target: 0.99, WindowDays: 30, Transaction: "GET /checkout",
		BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC().Add(-10*time.Minute), goodBadSpecs(100, 20, "production"))

	notifier := &capturingNotifier{store: st}
	e := &slo.Evaluator{
		Pool:      pool,
		Store:     st,
		Providers: slo.Providers(trace.NewQuery(conn), nil, nil, 90),
		Notifier:  notifier,
		Policy:    escalation.NewPolicyStore(pool),
		Maint:     mockMaint(func(context.Context, int64, time.Time) (bool, error) { return false, nil }),
	}

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
	if incs[0].InMaintenance {
		t.Error("InMaintenance = true, want false (outside window)")
	}
	if evs := notifier.snapshot(); len(evs) != 1 || !evs[0].Opened {
		t.Errorf("notify events after open tick = %+v, want 1 open event (not suppressed outside maintenance)", evs)
	}
}

// TestSLOEvaluatorMaintenanceCloseSuppressedByFlagAfterWindowEnds —
// дискриминирует close-гейт «по сохранённому флагу» (!inc.InMaintenance) от
// ошибочного «по текущему окну» (!e.inMaintenance(now)): открываем инцидент
// сжигания бюджета В окне, затем окно ЗАКАНЧИВАЕТСЯ (mock→false) — close всё
// равно должен быть подавлен, т.к. читается сохранённый флаг инцидента.
// Зеркало trace/profile-аналогов.
func TestSLOEvaluatorMaintenanceCloseSuppressedByFlagAfterWindowEnds(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres and clickhouse containers")
	}
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)

	s, err := st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: "checkout maint flag", Kind: slo.SLIAvailability,
		Target: 0.99, WindowDays: 30, Transaction: "GET /checkout",
		BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC().Add(-10*time.Minute), goodBadSpecs(100, 20, "production"))

	notifier := &capturingNotifier{store: st}
	inWindow := true
	e := &slo.Evaluator{
		Pool:      pool,
		Store:     st,
		Providers: slo.Providers(trace.NewQuery(conn), nil, nil, 90),
		Notifier:  notifier,
		Policy:    escalation.NewPolicyStore(pool),
		Maint:     mockMaint(func(context.Context, int64, time.Time) (bool, error) { return inWindow, nil }),
	}

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
	if !incs[0].InMaintenance {
		t.Fatal("InMaintenance = false, want true (открыт в окне)")
	}
	if evs := notifier.snapshot(); len(evs) != 0 {
		t.Fatalf("notify events after open tick = %d, want 0 (suppressed by maintenance)", len(evs))
	}

	// Окно обслуживания закончилось — close-гейт должен смотреть на
	// сохранённый флаг инцидента, а не на текущее состояние окна.
	inWindow = false

	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC().Add(-1*time.Minute), goodBadSpecs(100, 0, "production"))

	for i := 0; i < 2; i++ {
		if n3, err := e.Tick(ctx); err != nil || n3 != 0 {
			t.Fatalf("тик %d остывания: переходов %d err=%v, want 0 (рано закрывать)", i+1, n3, err)
		}
	}

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
	if evs := notifier.snapshot(); len(evs) != 0 {
		t.Errorf("notify events after resolve tick = %d, want still 0 (close by saved flag, not by current window)", len(evs))
	}
}

// TestSLOEvaluatorRecoveryReachesWokenChannelAfterMaintenanceWindowEnds — M-7
// (аудит B4, remediation A): инцидент сжигания бюджета открыт В окне
// обслуживания (in_maintenance заморожен=true, open-гейт не тронут — открытие
// молчит), но за время жизни инцидента эскалация реально разбудила канал
// (планировщик T8, здесь симулируем логом эскалации напрямую, как советует
// бриф — вне этого пакета). Окно кончается, инцидент закрывается ВНЕ окна:
// close ОБЯЗАН прислать recovery разбуженному каналу, несмотря на замороженный
// InMaintenance=true — старый гейт `!inc.InMaintenance` на close-пути гасил
// именно этот случай (M-7). Дискриминирует: падает, если гейт вернуть.
func TestSLOEvaluatorRecoveryReachesWokenChannelAfterMaintenanceWindowEnds(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres and clickhouse containers")
	}
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)

	asvc := alert.NewService(pool)
	chanID, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}

	s, err := st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: "checkout woken recovery", Kind: slo.SLIAvailability,
		Target: 0.99, WindowDays: 30, Transaction: "GET /checkout",
		BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC().Add(-10*time.Minute), goodBadSpecs(100, 20, "production"))

	notifier := &capturingNotifier{store: st}
	inWindow := true
	e := &slo.Evaluator{
		Pool:      pool,
		Store:     st,
		Providers: slo.Providers(trace.NewQuery(conn), nil, nil, 90),
		Notifier:  notifier,
		Policy:    escalation.NewPolicyStore(pool),
		Maint:     mockMaint(func(context.Context, int64, time.Time) (bool, error) { return inWindow, nil }),
	}

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
	if !incs[0].InMaintenance {
		t.Fatal("InMaintenance = false, want true (открыт в окне)")
	}
	if evs := notifier.snapshot(); len(evs) != 0 {
		t.Fatalf("notify events after open tick = %d, want 0 (open suppressed by maintenance, open-гейт не тронут)", len(evs))
	}

	// Планировщик (T8, вне этого пакета) реально эскалировал инцидент после
	// открытия — разбудил канал. Симулируем логом эскалации напрямую.
	if err := escalation.LogStep(ctx, pool, "slo", incs[0].ID, chanID, 0); err != nil {
		t.Fatalf("log step: %v", err)
	}

	// Окно обслуживания закончилось.
	inWindow = false

	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC().Add(-1*time.Minute), goodBadSpecs(100, 0, "production"))

	for i := 0; i < 2; i++ {
		if n3, err := e.Tick(ctx); err != nil || n3 != 0 {
			t.Fatalf("тик %d остывания: переходов %d err=%v, want 0 (рано закрывать)", i+1, n3, err)
		}
	}

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

	evs := notifier.snapshot()
	if len(evs) != 1 || evs[0].Opened {
		t.Fatalf("notify events after resolve tick = %+v, want 1 recovery event (M-7: recovery must reach woken channel despite frozen InMaintenance=true)", evs)
	}
}
