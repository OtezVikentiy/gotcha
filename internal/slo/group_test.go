package slo_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/depsuppress"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// реальный Grouper поверх реального Suppressor — без фейков резолвера корней.
func newSLOGrouper(pool *pgxpool.Pool) *incidentgroup.Grouper {
	return &incidentgroup.Grouper{
		Pool:  pool,
		Store: incidentgroup.NewStore(pool),
		Roots: depsuppress.NewSuppressor(pool),
	}
}

func seedGroupMonitor(t *testing.T, pool *pgxpool.Pool, projectID int64) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO monitors (project_id, name, kind, interval_seconds) VALUES ($1,'api','http',60) RETURNING id`,
		projectID).Scan(&id); err != nil {
		t.Fatalf("seed monitor: %v", err)
	}
	return id
}

// resolved_at NULL — монитор «упал» для depsuppress; notified — гейт «информирующего корня».
func seedOpenUptimeIncident(t *testing.T, pool *pgxpool.Pool, monitorID int64, notified bool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO incidents (monitor_id, notified_open) VALUES ($1,$2) RETURNING id`,
		monitorID, notified).Scan(&id); err != nil {
		t.Fatalf("seed uptime incident: %v", err)
	}
	return id
}

func readSLOGroupID(t *testing.T, pool *pgxpool.Pool, incidentID int64) *int64 {
	t.Helper()
	var gid *int64
	if err := pool.QueryRow(context.Background(),
		`SELECT group_id FROM slo_incidents WHERE id = $1`, incidentID).Scan(&gid); err != nil {
		t.Fatalf("read group_id: %v", err)
	}
	return gid
}

// каждая корзина 80/100 даёт burn 20 при target 0.99 — выше порога 14.4 в обоих окнах.
type burningProvider struct{}

func (burningProvider) Buckets(_ context.Context, _ slo.SLO, from, to time.Time, step time.Duration) ([]slo.Bucket, error) {
	var out []slo.Bucket
	for t := from; t.Before(to); t = t.Add(step) {
		out = append(out, slo.Bucket{T: t, Good: 80, Total: 100})
	}
	return out, nil
}

func (burningProvider) RetentionCap() time.Duration { return 0 }

func (p burningProvider) BucketsExcluding(ctx context.Context, s slo.SLO, from, to time.Time, step time.Duration, _ []uptime.Window) ([]slo.Bucket, error) {
	return p.Buckets(ctx, s, from, to, step)
}

func TestSLOUptimeMemberSilenced(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	pid := seedProject(t, pool)
	if _, err := alert.NewService(pool).CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	monID := seedGroupMonitor(t, pool, pid)
	rootInc := seedOpenUptimeIncident(t, pool, monID, true)

	st := slo.NewStore(pool)
	s, err := st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: "api uptime", Kind: slo.SLIUptime, MonitorID: &monID,
		Target: 0.99, WindowDays: 30, BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	notifier := &capturingNotifier{store: st}
	e := &slo.Evaluator{
		// от Interval считается бюджет тика — с дефолтом он упирается в пол 10s,
		// и на нагруженной машине запрос в CH может не уложиться.
		Interval:       time.Hour,
		Pool:           pool,
		Store:          st,
		Providers:      map[slo.SLIKind]slo.Provider{slo.SLIUptime: burningProvider{}},
		Notifier:       notifier,
		Policy:         escalation.NewPolicyStore(pool),
		IncidentGroups: newSLOGrouper(pool),
	}

	n, err := e.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 1 {
		t.Fatalf("переходов = %d, want 1 — группа глушит уведомление, не само открытие", n)
	}
	inc, open, err := st.OpenIncidentFor(ctx, s.ID)
	if err != nil || !open {
		t.Fatalf("OpenIncidentFor: open=%v err=%v", open, err)
	}
	gid := readSLOGroupID(t, pool, inc.ID)
	if gid == nil {
		t.Fatal("group_id IS NULL — SLO-инцидент не присоединён к группе uptime-корня")
	}
	var gotRootInc int64
	var gotSource string
	if err := pool.QueryRow(ctx,
		`SELECT root_source, root_incident_id FROM incident_groups WHERE id = $1`, *gid).
		Scan(&gotSource, &gotRootInc); err != nil {
		t.Fatalf("read group root: %v", err)
	}
	if gotSource != "uptime" || gotRootInc != rootInc {
		t.Errorf("группа якорится на %s/%d, want uptime-корень uptime/%d", gotSource, gotRootInc, rootInc)
	}
	if evs := notifier.snapshot(); len(evs) != 0 {
		t.Errorf("уведомлений = %d, want 0 (информирует uptime-корень, NotifyStep члена подавлен): %+v", len(evs), evs)
	}
	if inc.NotifiedOpen {
		t.Error("notified_open = true у члена информирующей группы, want false")
	}
}

func TestSLONonUptimeOutsideGroups(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	pid := seedProject(t, pool)
	if _, err := alert.NewService(pool).CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	st := slo.NewStore(pool)
	s, err := st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: "checkout", Kind: slo.SLIAvailability,
		Target: 0.99, WindowDays: 30, Transaction: "GET /checkout",
		BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	notifier := &capturingNotifier{store: st}
	e := &slo.Evaluator{
		// от Interval считается бюджет тика — с дефолтом он упирается в пол 10s,
		// и на нагруженной машине запрос в CH может не уложиться.
		Interval:       time.Hour,
		Pool:           pool,
		Store:          st,
		Providers:      map[slo.SLIKind]slo.Provider{slo.SLIAvailability: burningProvider{}},
		Notifier:       notifier,
		Policy:         escalation.NewPolicyStore(pool),
		IncidentGroups: newSLOGrouper(pool),
	}

	if n, err := e.Tick(ctx); err != nil || n != 1 {
		t.Fatalf("Tick: переходов %d err=%v, want 1", n, err)
	}
	inc, open, err := st.OpenIncidentFor(ctx, s.ID)
	if err != nil || !open {
		t.Fatalf("OpenIncidentFor: open=%v err=%v", open, err)
	}
	if readSLOGroupID(t, pool, inc.ID) != nil {
		t.Error("group_id проставлен у availability-SLO без монитора, want вне групп")
	}
	evs := notifier.snapshot()
	if len(evs) != 1 || !evs[0].Opened {
		t.Errorf("уведомлений = %d, want 1 open (нет узла — гейт групп не вмешивается): %+v", len(evs), evs)
	}
}

// член открытой группы исключён из OpenUnacked; после Resolve группы возвращается
// с StartedAt = GREATEST(started_at, resolved_at), а не старым started_at.
func TestSLOOpenUnackedGroupGating(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	pid := seedProject(t, pool)
	monID := seedGroupMonitor(t, pool, pid)
	rootInc := seedOpenUptimeIncident(t, pool, monID, true)

	st := slo.NewStore(pool)
	s, err := st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: "api uptime", Kind: slo.SLIUptime, MonitorID: &monID,
		Target: 0.99, WindowDays: 30, BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	member, created, err := st.OpenIncident(ctx, s.ID, pid, 20.0, nil, false)
	if err != nil || !created {
		t.Fatalf("OpenIncident: created=%v err=%v", created, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE slo_incidents SET started_at = now() - interval '3 hours' WHERE id = $1`, member.ID); err != nil {
		t.Fatalf("age member: %v", err)
	}

	store := incidentgroup.NewStore(pool)
	grp, err := store.EnsureGroup(ctx, pid, "uptime", rootInc, "monitor", monID)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := store.SetGroup(ctx, pid, "slo", member.ID, grp.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}

	contains := func(list []escalation.PendingIncident, id int64) *escalation.PendingIncident {
		for i := range list {
			if list[i].ID == id {
				return &list[i]
			}
		}
		return nil
	}

	list, err := st.OpenUnacked(ctx)
	if err != nil {
		t.Fatalf("OpenUnacked (группа открыта): %v", err)
	}
	if contains(list, member.ID) != nil {
		t.Error("член ОТКРЫТОЙ группы попал в OpenUnacked — планировщик эскалировал бы его в обход корня")
	}

	if ok, err := store.Resolve(ctx, "uptime", rootInc); err != nil || !ok {
		t.Fatalf("Resolve group: ok=%v err=%v", ok, err)
	}
	list, err = st.OpenUnacked(ctx)
	if err != nil {
		t.Fatalf("OpenUnacked (группа закрыта): %v", err)
	}
	got := contains(list, member.ID)
	if got == nil {
		t.Fatalf("член закрытой группы не вернулся в OpenUnacked — досылка step0 после распада группы потеряна: %+v", list)
	}
	if age := time.Since(got.StartedAt); age >= time.Minute {
		t.Errorf("StartedAt отстаёт на %v — база отсчёта лесенки не GREATEST(started_at, resolved_at): step1 с delay>0 на следующем тике стал бы дью и дал залп", age)
	}
}
