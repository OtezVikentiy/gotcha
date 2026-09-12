package host_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func setupIncidentHost(t *testing.T) (*pgxpool.Pool, *host.IncidentService, int64, int64) {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('host-incident', 'Host Incident', 0) RETURNING id").
		Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	var projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'host-incident', 'Host Incident') RETURNING id", orgID).
		Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	store := host.NewStore(pool)
	if _, err := store.Upsert(ctx, projectID, entries("web-01")); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	h, ok, err := store.Get(ctx, projectID, "web-01")
	if err != nil || !ok {
		t.Fatalf("get host: ok=%v err=%v", ok, err)
	}

	return pool, host.NewIncidentService(pool), projectID, h.ID
}

func TestIncidentServiceOpenNewReturnsCreatedTrue(t *testing.T) {
	_, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	in, created, err := svc.Open(ctx, projectID, hostID, "disk", 0.95, "/var full", false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !created {
		t.Fatal("Open: created = false, want true")
	}
	if in.ID == 0 || in.ProjectID != projectID || in.HostID != hostID {
		t.Fatalf("Open: unexpected incident %+v", in)
	}
	if in.Kind != "disk" || in.Status != "open" {
		t.Fatalf("Open: Kind/Status = %q/%q, want disk/open", in.Kind, in.Status)
	}
	if in.CurrentValue != 0.95 || in.PeakValue != 0.95 {
		t.Fatalf("Open: CurrentValue/PeakValue = %v/%v, want 0.95/0.95 (peak=current на вставке)", in.CurrentValue, in.PeakValue)
	}
	if in.Detail != "/var full" {
		t.Fatalf("Open: Detail = %q, want %q", in.Detail, "/var full")
	}
	if in.ResolvedAt != nil {
		t.Fatalf("Open: ResolvedAt = %v, want nil", in.ResolvedAt)
	}
}

func TestIncidentServiceOpenConcurrentOnlyOneWins(t *testing.T) {
	pool, svc, projectID, hostID := setupIncidentHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	var createdCount int
	winnerIDs := make(map[int64]struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in, created, err := svc.Open(ctx, projectID, hostID, "disk", 0.9, "", false)
			if err != nil {
				errs[i] = err
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if created {
				createdCount++
			}
			winnerIDs[in.ID] = struct{}{}
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Open: %v", err)
		}
	}
	if createdCount != 1 {
		t.Fatalf("createdCount = %d, want exactly 1", createdCount)
	}
	if len(winnerIDs) != 1 {
		t.Fatalf("winnerIDs = %v, want ровно один общий id победителя", winnerIDs)
	}

	var openCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM host_incidents WHERE host_id = $1 AND status = 'open'", hostID).
		Scan(&openCount); err != nil {
		t.Fatalf("count open incidents: %v", err)
	}
	if openCount != 1 {
		t.Fatalf("openCount = %d, want 1 (частичный уникальный индекс должен предотвратить дубли)", openCount)
	}
}

func TestIncidentServiceOpenDifferentKindIsSeparateIncident(t *testing.T) {
	_, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	diskIn, created, err := svc.Open(ctx, projectID, hostID, "disk", 0.95, "", false)
	if err != nil || !created {
		t.Fatalf("Open disk: created=%v err=%v", created, err)
	}

	loadIn, created, err := svc.Open(ctx, projectID, hostID, "load", 3.5, "", false)
	if err != nil {
		t.Fatalf("Open load: %v", err)
	}
	if !created {
		t.Fatal("Open load: created = false, want true (другой kind — отдельный инцидент)")
	}
	if loadIn.ID == diskIn.ID {
		t.Fatal("Open load вернул тот же id, что disk — ключ конфликта должен включать kind")
	}

	open, err := svc.ListOpenByHost(ctx, hostID)
	if err != nil {
		t.Fatalf("ListOpenByHost: %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("len(ListOpenByHost) = %d, want 2 (disk и load одновременно)", len(open))
	}
}

func TestIncidentServiceBumpUpdatesCurrentAndPeak(t *testing.T) {
	_, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	in, _, err := svc.Open(ctx, projectID, hostID, "memory", 0.91, "", false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := svc.Bump(ctx, in.ID, 0.97, 0.99); err != nil {
		t.Fatalf("Bump: %v", err)
	}
	got, ok, err := svc.OpenFor(ctx, hostID, "memory")
	if err != nil || !ok {
		t.Fatalf("OpenFor после Bump: ok=%v err=%v", ok, err)
	}
	if got.CurrentValue != 0.97 || got.PeakValue != 0.99 {
		t.Fatalf("после Bump CurrentValue/PeakValue = %v/%v, want 0.97/0.99", got.CurrentValue, got.PeakValue)
	}

	if _, err := svc.Resolve(ctx, in.ID, 0.5); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := svc.Bump(ctx, in.ID, 0.6, 0.6); !errors.Is(err, host.ErrIncidentNotFound) {
		t.Fatalf("Bump закрытого инцидента: err = %v, want ErrIncidentNotFound", err)
	}
	if err := svc.Bump(ctx, 999999999, 0.6, 0.6); !errors.Is(err, host.ErrIncidentNotFound) {
		t.Fatalf("Bump несуществующего id: err = %v, want ErrIncidentNotFound", err)
	}
}

func TestIncidentServiceResolveTwiceSecondReturnsFalse(t *testing.T) {
	_, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	in, _, err := svc.Open(ctx, projectID, hostID, "silent", 0, "", false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ok, err := svc.Resolve(ctx, in.ID, 0)
	if err != nil {
		t.Fatalf("Resolve 1: %v", err)
	}
	if !ok {
		t.Fatal("Resolve 1: ok = false, want true")
	}

	got, found, err := svc.OpenFor(ctx, hostID, "silent")
	if err != nil {
		t.Fatalf("OpenFor после Resolve: %v", err)
	}
	if found {
		t.Fatalf("OpenFor после Resolve всё ещё нашёл открытый инцидент: %+v", got)
	}

	ok2, err := svc.Resolve(ctx, in.ID, 0)
	if err != nil {
		t.Fatalf("Resolve 2: %v", err)
	}
	if ok2 {
		t.Fatal("Resolve 2: ok = true, want false (уже закрыт)")
	}

	in2, created, err := svc.Open(ctx, projectID, hostID, "silent", 0, "", false)
	if err != nil {
		t.Fatalf("Open после Resolve: %v", err)
	}
	if !created {
		t.Fatal("Open после Resolve: created = false, want true")
	}
	if in2.ID == in.ID {
		t.Fatal("Open после Resolve переиспользовал id закрытого инцидента")
	}
}

func TestIncidentServiceMarkNotified(t *testing.T) {
	_, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	in, _, err := svc.Open(ctx, projectID, hostID, "load", 2.5, "", false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := svc.MarkNotified(ctx, in.ID, true); err != nil {
		t.Fatalf("MarkNotified open: %v", err)
	}
	got, ok, err := svc.OpenFor(ctx, hostID, "load")
	if err != nil || !ok {
		t.Fatalf("OpenFor: ok=%v err=%v", ok, err)
	}
	if !got.NotifiedOpen || got.NotifiedClose {
		t.Fatalf("после MarkNotified(open) = %+v, want NotifiedOpen=true NotifiedClose=false", got)
	}

	if err := svc.MarkNotified(ctx, in.ID, false); err != nil {
		t.Fatalf("MarkNotified close: %v", err)
	}
	got, ok, err = svc.OpenFor(ctx, hostID, "load")
	if err != nil || !ok {
		t.Fatalf("OpenFor: ok=%v err=%v", ok, err)
	}
	if !got.NotifiedOpen || !got.NotifiedClose {
		t.Fatalf("после MarkNotified(close) = %+v, want оба true", got)
	}

	if err := svc.MarkNotified(ctx, 999999999, true); !errors.Is(err, host.ErrIncidentNotFound) {
		t.Fatalf("MarkNotified неизвестного id: err = %v, want ErrIncidentNotFound", err)
	}
}

func TestIncidentServiceListByProjectFreshestFirst(t *testing.T) {
	_, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	first, _, err := svc.Open(ctx, projectID, hostID, "disk", 0.95, "", false)
	if err != nil {
		t.Fatalf("Open disk: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	second, _, err := svc.Open(ctx, projectID, hostID, "load", 3.0, "", false)
	if err != nil {
		t.Fatalf("Open load: %v", err)
	}

	got, err := svc.ListByProject(ctx, projectID, 10)
	if err != nil {
		t.Fatalf("ListByProject: %v", err)
	}
	if len(got) != 2 || got[0].ID != second.ID || got[1].ID != first.ID {
		t.Fatalf("ListByProject = %+v, want [%d %d] freshest first", got, second.ID, first.ID)
	}
}

func secondHost(t *testing.T, pool *pgxpool.Pool, projectID int64, name string) int64 {
	t.Helper()
	ctx := context.Background()
	store := host.NewStore(pool)
	if _, err := store.Upsert(ctx, projectID, entries(name)); err != nil {
		t.Fatalf("upsert host %s: %v", name, err)
	}
	h, ok, err := store.Get(ctx, projectID, name)
	if err != nil || !ok {
		t.Fatalf("get host %s: ok=%v err=%v", name, ok, err)
	}
	return h.ID
}

func TestIncidentServiceListOpenByProject(t *testing.T) {
	pool, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()
	otherID := secondHost(t, pool, projectID, "web-02")

	oldOpen, _, err := svc.Open(ctx, projectID, hostID, "disk", 0.95, "", false)
	if err != nil {
		t.Fatalf("Open disk: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	otherOpen, _, err := svc.Open(ctx, projectID, otherID, "memory", 0.99, "", false)
	if err != nil {
		t.Fatalf("Open memory на втором хосте: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	closed, _, err := svc.Open(ctx, projectID, hostID, "load", 3.0, "", false)
	if err != nil {
		t.Fatalf("Open load: %v", err)
	}
	if ok, err := svc.Resolve(ctx, closed.ID, 0.1); err != nil || !ok {
		t.Fatalf("Resolve load: ok=%v err=%v", ok, err)
	}

	got, err := svc.ListOpenByProject(ctx, projectID)
	if err != nil {
		t.Fatalf("ListOpenByProject: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListOpenByProject вернул %d инцидентов, want 2 (только открытые): %+v", len(got), got)
	}
	if got[0].ID != otherOpen.ID || got[1].ID != oldOpen.ID {
		t.Fatalf("порядок = [%d %d], want [%d %d] (свежайшие первыми)", got[0].ID, got[1].ID, otherOpen.ID, oldOpen.ID)
	}
	for _, in := range got {
		if in.Status != "open" {
			t.Errorf("в выборке инцидент со статусом %q: %+v", in.Status, in)
		}
	}
}

func TestIncidentServiceResolveOpenByProjectKind(t *testing.T) {
	pool, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()
	otherID := secondHost(t, pool, projectID, "web-02")

	diskA, _, err := svc.Open(ctx, projectID, hostID, "disk", 0.95, "/var", false)
	if err != nil {
		t.Fatalf("Open disk A: %v", err)
	}
	diskB, _, err := svc.Open(ctx, projectID, otherID, "disk", 0.99, "/", false)
	if err != nil {
		t.Fatalf("Open disk B: %v", err)
	}
	mem, _, err := svc.Open(ctx, projectID, hostID, "memory", 0.93, "", false)
	if err != nil {
		t.Fatalf("Open memory: %v", err)
	}

	// Соседний проект той же организации: настройки порогов — пер-проектные,
	// и выключение порога в одном проекте не должно гасить инциденты другого.
	var orgID int64
	if err := pool.QueryRow(ctx, "SELECT org_id FROM projects WHERE id = $1", projectID).Scan(&orgID); err != nil {
		t.Fatalf("read org: %v", err)
	}
	var otherProject int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'host-incident-2', 'Host Incident 2') RETURNING id", orgID).
		Scan(&otherProject); err != nil {
		t.Fatalf("insert second project: %v", err)
	}
	foreignHost := secondHost(t, pool, otherProject, "web-01")
	foreign, _, err := svc.Open(ctx, otherProject, foreignHost, "disk", 0.97, "", false)
	if err != nil {
		t.Fatalf("Open disk в соседнем проекте: %v", err)
	}

	n, err := svc.ResolveOpenByProjectKind(ctx, projectID, "disk")
	if err != nil {
		t.Fatalf("ResolveOpenByProjectKind: %v", err)
	}
	if n != 2 {
		t.Fatalf("закрыто %d инцидентов, want 2 (disk на обоих хостах)", n)
	}

	for _, id := range []int64{diskA.ID, diskB.ID} {
		var status string
		var resolvedAt *time.Time
		if err := pool.QueryRow(ctx, "SELECT status, resolved_at FROM host_incidents WHERE id = $1", id).
			Scan(&status, &resolvedAt); err != nil {
			t.Fatalf("read incident %d: %v", id, err)
		}
		if status != "resolved" || resolvedAt == nil {
			t.Errorf("инцидент %d: status=%q resolved_at=%v, want resolved + непустой момент", id, status, resolvedAt)
		}
	}
	if got, ok, err := svc.OpenFor(ctx, hostID, "memory"); err != nil || !ok || got.ID != mem.ID {
		t.Errorf("инцидент соседнего вида memory закрыт заодно: ok=%v err=%v", ok, err)
	}
	if got, ok, err := svc.OpenFor(ctx, foreignHost, "disk"); err != nil || !ok || got.ID != foreign.ID {
		t.Errorf("закрыт инцидент того же вида в СОСЕДНЕМ проекте: ok=%v err=%v", ok, err)
	}

	again, err := svc.ResolveOpenByProjectKind(ctx, projectID, "disk")
	if err != nil {
		t.Fatalf("повторный ResolveOpenByProjectKind: %v", err)
	}
	if again != 0 {
		t.Errorf("повторный вызов закрыл %d инцидентов, want 0 (идемпотентность)", again)
	}
}

func TestIncidentServiceAcknowledge(t *testing.T) {
	pool, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", "host-ack@e.com").
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	in, _, err := svc.Open(ctx, projectID, hostID, "disk", 0.95, "/var full", false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	list, err := svc.ListByProject(ctx, projectID, 10)
	if err != nil || len(list) != 1 || list[0].AcknowledgedAt != nil || list[0].AcknowledgedBy != nil {
		t.Fatalf("до Acknowledge: list=%+v err=%v, want AcknowledgedAt/By nil", list, err)
	}

	ok, err := svc.Acknowledge(ctx, in.ID, projectID, userID)
	if err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if !ok {
		t.Fatal("Acknowledge: ok = false, want true")
	}

	list, err = svc.ListByProject(ctx, projectID, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("после Acknowledge: list=%+v err=%v", list, err)
	}
	if list[0].AcknowledgedAt == nil {
		t.Fatal("после Acknowledge: AcknowledgedAt = nil, want заполнено")
	}
	if list[0].AcknowledgedBy == nil || *list[0].AcknowledgedBy != userID {
		t.Fatalf("после Acknowledge: AcknowledgedBy = %v, want %d", list[0].AcknowledgedBy, userID)
	}

	ok2, err := svc.Acknowledge(ctx, in.ID, projectID, userID)
	if err != nil {
		t.Fatalf("повторный Acknowledge: %v", err)
	}
	if ok2 {
		t.Fatal("повторный Acknowledge: ok = true, want false (идемпотентность)")
	}

	other, _, err := svc.Open(ctx, projectID, hostID, "memory", 0.9, "", false)
	if err != nil {
		t.Fatalf("Open memory: %v", err)
	}
	if _, err := svc.Resolve(ctx, other.ID, 0.1); err != nil {
		t.Fatalf("Resolve memory: %v", err)
	}
	okClosed, err := svc.Acknowledge(ctx, other.ID, projectID, userID)
	if err != nil {
		t.Fatalf("Acknowledge closed: %v", err)
	}
	if okClosed {
		t.Fatal("Acknowledge закрытого инцидента: ok = true, want false")
	}
}

func TestIncidentServiceAcknowledgeForeignProject(t *testing.T) {
	pool, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", "host-ack-foreign@e.com").
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	var otherProjectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) SELECT org_id, 'host-ack-foreign', 'other' FROM projects WHERE id = $1 RETURNING id",
		projectID).Scan(&otherProjectID); err != nil {
		t.Fatalf("insert other project: %v", err)
	}

	in, _, err := svc.Open(ctx, projectID, hostID, "disk", 0.95, "/var full", false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if ok, err := svc.Acknowledge(ctx, in.ID, otherProjectID, userID); err != nil || ok {
		t.Fatalf("Acknowledge с чужим project_id: (%v,%v), want (false,nil)", ok, err)
	}

	list, err := svc.ListByProject(ctx, projectID, 10)
	if err != nil || len(list) != 1 || list[0].AcknowledgedAt != nil {
		t.Fatalf("после чужого Acknowledge: list=%+v err=%v, want AcknowledgedAt nil", list, err)
	}
}

func TestIncidentsCascadeDeletedWithHost(t *testing.T) {
	pool, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	if _, _, err := svc.Open(ctx, projectID, hostID, "disk", 0.95, "", false); err != nil {
		t.Fatalf("Open disk: %v", err)
	}
	resolved, _, err := svc.Open(ctx, projectID, hostID, "memory", 0.91, "", false)
	if err != nil {
		t.Fatalf("Open memory: %v", err)
	}
	if _, err := svc.Resolve(ctx, resolved.ID, 0.5); err != nil {
		t.Fatalf("Resolve memory: %v", err)
	}

	if _, err := pool.Exec(ctx, "DELETE FROM hosts WHERE id = $1", hostID); err != nil {
		t.Fatalf("delete host: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM host_incidents WHERE host_id = $1", hostID).
		Scan(&count); err != nil {
		t.Fatalf("count incidents after host delete: %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d после удаления хоста, want 0 (ON DELETE CASCADE)", count)
	}
}

func TestOpenSuppressedAndClearSuppressed(t *testing.T) {
	pool, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	in, _, err := svc.Open(ctx, projectID, hostID, "disk", 0.95, "", false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if list, err := svc.OpenUnacked(ctx); err != nil || len(list) != 1 {
		t.Fatalf("до подавления OpenUnacked want 1, got %d (err=%v)", len(list), err)
	}
	if list, err := svc.OpenSuppressed(ctx); err != nil || len(list) != 0 {
		t.Fatalf("до подавления OpenSuppressed want 0, got %d (err=%v)", len(list), err)
	}

	// Флаг ставит внешний планировщик деп-подавления — здесь сырым SQL, в обход этого пакета.
	if _, err := pool.Exec(ctx, `UPDATE host_incidents SET suppressed_by_dep=true WHERE id=$1`, in.ID); err != nil {
		t.Fatalf("set suppressed flag: %v", err)
	}

	if list, err := svc.OpenUnacked(ctx); err != nil || len(list) != 0 {
		t.Fatalf("подавлен: OpenUnacked want 0, got %d (err=%v)", len(list), err)
	}
	suppressed, err := svc.OpenSuppressed(ctx)
	if err != nil {
		t.Fatalf("OpenSuppressed: %v", err)
	}
	if len(suppressed) != 1 || suppressed[0].ID != in.ID {
		t.Fatalf("OpenSuppressed = %+v, want [инцидент %d]", suppressed, in.ID)
	}

	beforeClear := time.Now()
	if err := svc.ClearSuppressed(ctx, in.ID); err != nil {
		t.Fatalf("ClearSuppressed: %v", err)
	}

	if list, err := svc.OpenSuppressed(ctx); err != nil || len(list) != 0 {
		t.Fatalf("после снятия: OpenSuppressed want 0, got %d (err=%v)", len(list), err)
	}
	unacked, err := svc.OpenUnacked(ctx)
	if err != nil {
		t.Fatalf("OpenUnacked после снятия: %v", err)
	}
	if len(unacked) != 1 || unacked[0].ID != in.ID {
		t.Fatalf("OpenUnacked после снятия = %+v, want [инцидент %d]", unacked, in.ID)
	}
	// Часы лесенки перезапущены: StartedAt не раньше момента снятия
	// подавления (GREATEST с dep_released_at).
	if unacked[0].StartedAt.Before(beforeClear) {
		t.Fatalf("StartedAt = %v, want не раньше момента снятия %v (часы должны были перезапуститься)",
			unacked[0].StartedAt, beforeClear)
	}

	var depReleasedAt *time.Time
	if err := pool.QueryRow(ctx, "SELECT dep_released_at FROM host_incidents WHERE id=$1", in.ID).Scan(&depReleasedAt); err != nil {
		t.Fatalf("select dep_released_at: %v", err)
	}
	if depReleasedAt == nil {
		t.Fatal("dep_released_at = NULL после ClearSuppressed, want проставленный момент снятия")
	}
}
