package incidentgroup_test

import (
	"context"
	"errors"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/depsuppress"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

type fakeB5Checker struct {
	markErr error
}

func (f *fakeB5Checker) CheckIncident(ctx context.Context, source string, incidentID int64) (bool, bool, error) {
	return false, false, nil
}

func (f *fakeB5Checker) MarkSuppressed(ctx context.Context, source string, incidentID int64) error {
	return f.markErr
}

type erroringRootResolver struct {
	err error
}

func (e *erroringRootResolver) DownRoot(ctx context.Context, kind string, nodeID int64) (string, int64, bool, error) {
	return "", 0, false, e.err
}

func (e *erroringRootResolver) Invalidate() {}

func TestDepGateMarkSuppressedAttachesToGroup(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	childHost := seedHost(t, pool, projectID, "child-"+randSlug(t))
	seedEdgeHH(t, pool, projectID, rootHost, childHost)
	rootInc := seedSilent(t, pool, projectID, rootHost, true)

	var childInc int64
	mustScan(t, pool, &childInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, childHost)

	sup := depsuppress.NewSuppressor(pool)
	grouper, _ := newGrouper(pool)
	gate := &incidentgroup.DepGate{Dep: sup, Grouper: grouper}

	if err := gate.MarkSuppressed(ctx, "host", childInc); err != nil {
		t.Fatalf("MarkSuppressed: %v", err)
	}

	var suppressed bool
	if err := pool.QueryRow(ctx, `SELECT suppressed_by_dep FROM host_incidents WHERE id = $1`, childInc).Scan(&suppressed); err != nil {
		t.Fatalf("read suppressed_by_dep: %v", err)
	}
	if !suppressed {
		t.Fatal("suppressed_by_dep want true after DepGate.MarkSuppressed")
	}

	var groupID *int64
	if err := pool.QueryRow(ctx, `SELECT group_id FROM host_incidents WHERE id = $1`, childInc).Scan(&groupID); err != nil {
		t.Fatalf("read group_id: %v", err)
	}
	if groupID == nil {
		t.Fatal("DepGate.MarkSuppressed must attach the B5-suppressed host incident to its down-root's group")
	}
	var gotRootInc int64
	mustScan(t, pool, &gotRootInc, `SELECT root_incident_id FROM incident_groups WHERE id = $1`, *groupID)
	if gotRootInc != rootInc {
		t.Fatalf("group must anchor to root incident %d, got %d", rootInc, gotRootInc)
	}
}

func TestDepGateMarkSuppressedNonHostSourceSkipsAttach(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	childHost := seedHost(t, pool, projectID, "child-"+randSlug(t))
	seedEdgeHH(t, pool, projectID, rootHost, childHost)
	seedSilent(t, pool, projectID, rootHost, true)

	var childInc int64
	mustScan(t, pool, &childInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, childHost)

	sup := depsuppress.NewSuppressor(pool)
	grouper, _ := newGrouper(pool)
	gate := &incidentgroup.DepGate{Dep: sup, Grouper: grouper}

	// incidentID совпадает с реальным host-инцидентом — проверяем, что гейт по source сработал раньше.
	if err := gate.MarkSuppressed(ctx, "uptime", childInc); err != nil {
		t.Fatalf("MarkSuppressed(uptime): %v", err)
	}

	var groupID *int64
	if err := pool.QueryRow(ctx, `SELECT group_id FROM host_incidents WHERE id = $1`, childInc).Scan(&groupID); err != nil {
		t.Fatalf("read group_id: %v", err)
	}
	if groupID != nil {
		t.Fatalf("MarkSuppressed(uptime) must not attach host_incidents row via the D3 hook, got group_id=%d", *groupID)
	}
}

func TestDepGateMarkSuppressedNilGrouperNoPanic(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	hostID := seedHost(t, pool, projectID, "h-"+randSlug(t))
	var incID int64
	mustScan(t, pool, &incID, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, hostID)

	sup := depsuppress.NewSuppressor(pool)
	gate := &incidentgroup.DepGate{Dep: sup, Grouper: nil}

	if err := gate.MarkSuppressed(ctx, "host", incID); err != nil {
		t.Fatalf("MarkSuppressed with nil Grouper: %v", err)
	}
	var suppressed bool
	if err := pool.QueryRow(ctx, `SELECT suppressed_by_dep FROM host_incidents WHERE id = $1`, incID).Scan(&suppressed); err != nil {
		t.Fatalf("read suppressed_by_dep: %v", err)
	}
	if !suppressed {
		t.Fatal("underlying B5 suppression must still happen with nil Grouper")
	}
}

func TestDepGateMarkSuppressedHostVanished(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	sup := depsuppress.NewSuppressor(pool)
	grouper, _ := newGrouper(pool)
	gate := &incidentgroup.DepGate{Dep: sup, Grouper: grouper}

	if err := gate.MarkSuppressed(ctx, "host", 987654321); err != nil {
		t.Fatalf("MarkSuppressed for vanished incident: want nil, got %v", err)
	}
}

func TestDepGateCheckIncidentDelegates(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	childHost := seedHost(t, pool, projectID, "child-"+randSlug(t))
	seedEdgeHH(t, pool, projectID, rootHost, childHost)
	seedSilent(t, pool, projectID, rootHost, true)

	var childInc int64
	mustScan(t, pool, &childInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, childHost)

	sup := depsuppress.NewSuppressor(pool)
	grouper, _ := newGrouper(pool)
	gate := &incidentgroup.DepGate{Dep: sup, Grouper: grouper}

	wantHasParent, wantParentDown, err := sup.CheckIncident(ctx, "host", childInc)
	if err != nil {
		t.Fatalf("Suppressor.CheckIncident: %v", err)
	}
	if !wantHasParent || !wantParentDown {
		t.Fatalf("setup: want hasParent/parentDown = true/true, got %v/%v", wantHasParent, wantParentDown)
	}

	gotHasParent, gotParentDown, err := gate.CheckIncident(ctx, "host", childInc)
	if err != nil {
		t.Fatalf("DepGate.CheckIncident: %v", err)
	}
	if gotHasParent != wantHasParent || gotParentDown != wantParentDown {
		t.Fatalf("DepGate.CheckIncident = %v/%v, want passthrough %v/%v",
			gotHasParent, gotParentDown, wantHasParent, wantParentDown)
	}
}

func TestDepGateMarkSuppressedPropagatesDepError(t *testing.T) {
	wantErr := errors.New("dep marksuppressed boom")
	gate := &incidentgroup.DepGate{Dep: &fakeB5Checker{markErr: wantErr}}
	if err := gate.MarkSuppressed(context.Background(), "host", 1); !errors.Is(err, wantErr) {
		t.Fatalf("MarkSuppressed = %v, want propagated %v", err, wantErr)
	}
}

func TestDepGateMarkSuppressedHostIDQueryErrorSwallowed(t *testing.T) {
	pool := testenv.MigratedPG(t)
	projectID := seedProject(t, pool)
	hostID := seedHost(t, pool, projectID, "h-"+randSlug(t))
	var incID int64
	mustScan(t, pool, &incID, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, hostID)

	grouper := &incidentgroup.Grouper{Pool: pool, Store: incidentgroup.NewStore(pool)}
	pool.Close() // рвём соединение ПОСЛЕ сидирования — следующий запрос упадёт не ErrNoRows'ом

	gate := &incidentgroup.DepGate{Dep: &fakeB5Checker{}, Grouper: grouper}
	if err := gate.MarkSuppressed(context.Background(), "host", incID); err != nil {
		t.Fatalf("MarkSuppressed must swallow non-ErrNoRows query errors on the best-effort membership hook, got %v", err)
	}
}

func TestDepGateMarkSuppressedAttachErrorSwallowed(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	hostID := seedHost(t, pool, projectID, "h-"+randSlug(t))
	var incID int64
	mustScan(t, pool, &incID, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, hostID)

	grouper := &incidentgroup.Grouper{
		Pool:  pool,
		Store: incidentgroup.NewStore(pool),
		Roots: &erroringRootResolver{err: errors.New("downroot boom")},
	}
	gate := &incidentgroup.DepGate{Dep: &fakeB5Checker{}, Grouper: grouper}

	if err := gate.MarkSuppressed(ctx, "host", incID); err != nil {
		t.Fatalf("MarkSuppressed must swallow best-effort Attach errors, got %v", err)
	}
	var groupID *int64
	if err := pool.QueryRow(ctx, `SELECT group_id FROM host_incidents WHERE id = $1`, incID).Scan(&groupID); err != nil {
		t.Fatalf("read group_id: %v", err)
	}
	if groupID != nil {
		t.Fatalf("Attach failed (DownRoot error) — membership must not have been set, got group_id=%d", *groupID)
	}
}
