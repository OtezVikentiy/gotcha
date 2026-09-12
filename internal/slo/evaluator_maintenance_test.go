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

type mockMaint func(ctx context.Context, projectID int64, at time.Time) (bool, error)

func (m mockMaint) InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error) {
	return m(ctx, projectID, at)
}

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

	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC(), goodBadSpecs(100, 20, "production"))

	notifier := &capturingNotifier{store: st}
	e := &slo.Evaluator{
		// от Interval считается бюджет тика — с дефолтом он упирается в пол 10s,
		// и на нагруженной машине запрос в CH может не уложиться.
		Interval:  time.Hour,
		Pool:      pool,
		Store:     st,
		Providers: slo.Providers(trace.NewQuery(conn), nil, nil, 90),
		Notifier:  notifier,
		Policy:    escalation.NewPolicyStore(pool),
		Maint:     mockMaint(func(context.Context, int64, time.Time) (bool, error) { return true, nil }),
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
		t.Error("InMaintenance = false, want true (открыт в окне)")
	}
	if evs := notifier.snapshot(); len(evs) != 0 {
		t.Errorf("notify events after open tick = %d, want 0 (suppressed by maintenance)", len(evs))
	}

	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC(), goodBadSpecs(100, 0, "production"))

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
		t.Errorf("notify events after resolve tick = %d, want still 0 (close-notify suppressed too)", len(evs))
	}
}

func TestSLOEvaluatorMaintenanceFalseStillNotifies(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres and clickhouse containers")
	}
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)
	// без хотя бы одного канала claim-before-notify бампит ступень напрямую, не
	// зовя notifyStep вовсе — тесту важно, что notifyStep реально позван.
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

	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC(), goodBadSpecs(100, 20, "production"))

	notifier := &capturingNotifier{store: st}
	e := &slo.Evaluator{
		// от Interval считается бюджет тика — с дефолтом он упирается в пол 10s,
		// и на нагруженной машине запрос в CH может не уложиться.
		Interval:  time.Hour,
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

	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC(), goodBadSpecs(100, 20, "production"))

	notifier := &capturingNotifier{store: st}
	inWindow := true
	e := &slo.Evaluator{
		// от Interval считается бюджет тика — с дефолтом он упирается в пол 10s,
		// и на нагруженной машине запрос в CH может не уложиться.
		Interval:  time.Hour,
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

	inWindow = false

	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC(), goodBadSpecs(100, 0, "production"))

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

// инцидент открыт в окне (заморожен InMaintenance=true), эскалация успела разбудить
// канал; после закрытия вне окна recovery обязан дойти, несмотря на замороженный флаг.
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

	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC(), goodBadSpecs(100, 20, "production"))

	notifier := &capturingNotifier{store: st}
	inWindow := true
	e := &slo.Evaluator{
		// от Interval считается бюджет тика — с дефолтом он упирается в пол 10s,
		// и на нагруженной машине запрос в CH может не уложиться.
		Interval:  time.Hour,
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

	// симулируем логом эскалации то, что реально делает планировщик после открытия.
	if err := escalation.LogStep(ctx, pool, "slo", incs[0].ID, chanID, 0); err != nil {
		t.Fatalf("log step: %v", err)
	}

	inWindow = false

	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC(), goodBadSpecs(100, 0, "production"))

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
