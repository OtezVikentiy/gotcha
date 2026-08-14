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

// setupIncidentHost — своя заготовка (не переиспользует setupProject/
// setupSettingsProject: slug должен отличаться, иначе UNIQUE(slug)
// организаций столкнёт тесты, случайно запущенные в одном пакете). Заводит
// организацию, проект и один хост — возвращает пул, IncidentService,
// projectID и hostID.
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
	if _, err := store.Upsert(ctx, projectID, []string{"web-01"}); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	h, ok, err := store.Get(ctx, projectID, "web-01")
	if err != nil || !ok {
		t.Fatalf("get host: ok=%v err=%v", ok, err)
	}

	return pool, host.NewIncidentService(pool), projectID, h.ID
}

// TestIncidentServiceOpenNewReturnsCreatedTrue — открытие инцидента на
// хосте, где такого (kind) ещё не было — created=true, поля вставки
// (peak=current, detail, status='open') сохранены как переданы.
func TestIncidentServiceOpenNewReturnsCreatedTrue(t *testing.T) {
	_, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	in, created, err := svc.Open(ctx, projectID, hostID, "disk", 0.95, "/var full")
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

// TestIncidentServiceOpenConcurrentOnlyOneWins — параллельные Open той же
// пары (host_id, kind) — ровно один создаёт запись, остальные получают
// created=false и того же победителя (частичный уникальный индекс 0066).
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
			in, created, err := svc.Open(ctx, projectID, hostID, "disk", 0.9, "")
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

// TestIncidentServiceOpenDifferentKindIsSeparateIncident — открытие другого
// вида (kind) на том же хосте не блокируется уже открытым disk-инцидентом:
// ключ конфликта — (host_id, kind), а не только host_id.
func TestIncidentServiceOpenDifferentKindIsSeparateIncident(t *testing.T) {
	_, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	diskIn, created, err := svc.Open(ctx, projectID, hostID, "disk", 0.95, "")
	if err != nil || !created {
		t.Fatalf("Open disk: created=%v err=%v", created, err)
	}

	loadIn, created, err := svc.Open(ctx, projectID, hostID, "load", 3.5, "")
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

// TestIncidentServiceBumpUpdatesCurrentAndPeak — Bump на открытом инциденте
// обновляет current/peak; на закрытом/несуществующем — ErrIncidentNotFound.
func TestIncidentServiceBumpUpdatesCurrentAndPeak(t *testing.T) {
	_, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	in, _, err := svc.Open(ctx, projectID, hostID, "memory", 0.91, "")
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

// TestIncidentServiceResolveTwiceSecondReturnsFalse — Resolve дважды: первый
// закрывает (ok=true), второй — идемпотентно ok=false, без ошибки.
func TestIncidentServiceResolveTwiceSecondReturnsFalse(t *testing.T) {
	_, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	in, _, err := svc.Open(ctx, projectID, hostID, "silent", 0, "")
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

	// Открытие того же (host, kind) после Resolve — новая запись, не переиспользование id.
	in2, created, err := svc.Open(ctx, projectID, hostID, "silent", 0, "")
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

// TestIncidentServiceMarkNotified — MarkNotified(open=true) ставит
// notified_open, MarkNotified(open=false) — notified_close, независимо друг
// от друга; неизвестный id — ErrIncidentNotFound.
func TestIncidentServiceMarkNotified(t *testing.T) {
	_, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	in, _, err := svc.Open(ctx, projectID, hostID, "load", 2.5, "")
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

// TestIncidentServiceListByProjectFreshestFirst — ListByProject отдаёт
// инциденты проекта по убыванию started_at.
func TestIncidentServiceListByProjectFreshestFirst(t *testing.T) {
	_, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	first, _, err := svc.Open(ctx, projectID, hostID, "disk", 0.95, "")
	if err != nil {
		t.Fatalf("Open disk: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	second, _, err := svc.Open(ctx, projectID, hostID, "load", 3.0, "")
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

// secondHost добавляет проекту ещё один хост и возвращает его id — нужен
// тестам «по проекту», где важно, что метод собирает инциденты РАЗНЫХ хостов
// (свернуть их по host_id — работа вызывающего).
func secondHost(t *testing.T, pool *pgxpool.Pool, projectID int64, name string) int64 {
	t.Helper()
	ctx := context.Background()
	store := host.NewStore(pool)
	if _, err := store.Upsert(ctx, projectID, []string{name}); err != nil {
		t.Fatalf("upsert host %s: %v", name, err)
	}
	h, ok, err := store.Get(ctx, projectID, name)
	if err != nil || !ok {
		t.Fatalf("get host %s: ok=%v err=%v", name, ok, err)
	}
	return h.ID
}

// TestIncidentServiceListOpenByProject — ListOpenByProject отдаёт ТОЛЬКО
// открытые инциденты проекта, по всем его хостам, свежайшие первыми.
//
// Ревью I3: список хостов раньше сворачивал открытые виды из ListByProject с
// лимитом, то есть из «последних N любого статуса» — при закрытых инцидентах
// сверх лимита открытый в выборку не попадал вовсе, и хост с живой проблемой
// показывался спокойным. Здесь это воспроизведено буквально: закрытый инцидент
// СВЕЖЕЕ открытого, и выборка обязана вернуть именно открытый.
func TestIncidentServiceListOpenByProject(t *testing.T) {
	pool, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()
	otherID := secondHost(t, pool, projectID, "web-02")

	oldOpen, _, err := svc.Open(ctx, projectID, hostID, "disk", 0.95, "")
	if err != nil {
		t.Fatalf("Open disk: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	otherOpen, _, err := svc.Open(ctx, projectID, otherID, "memory", 0.99, "")
	if err != nil {
		t.Fatalf("Open memory на втором хосте: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	closed, _, err := svc.Open(ctx, projectID, hostID, "load", 3.0, "")
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

// TestIncidentServiceResolveOpenByProjectKind — ревью I2: выключение порога
// закрывает открытые инциденты ИМЕННО этого вида на всех хостах проекта,
// соседние виды не трогает, повторный вызов идемпотентен (0 строк).
func TestIncidentServiceResolveOpenByProjectKind(t *testing.T) {
	pool, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()
	otherID := secondHost(t, pool, projectID, "web-02")

	diskA, _, err := svc.Open(ctx, projectID, hostID, "disk", 0.95, "/var")
	if err != nil {
		t.Fatalf("Open disk A: %v", err)
	}
	diskB, _, err := svc.Open(ctx, projectID, otherID, "disk", 0.99, "/")
	if err != nil {
		t.Fatalf("Open disk B: %v", err)
	}
	mem, _, err := svc.Open(ctx, projectID, hostID, "memory", 0.93, "")
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
	foreign, _, err := svc.Open(ctx, otherProject, foreignHost, "disk", 0.97, "")
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

// TestIncidentsCascadeDeletedWithHost — удаление хоста (ON DELETE CASCADE
// host_incidents.host_id) уносит за собой все его инциденты, открытые и
// закрытые.
func TestIncidentsCascadeDeletedWithHost(t *testing.T) {
	pool, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	if _, _, err := svc.Open(ctx, projectID, hostID, "disk", 0.95, ""); err != nil {
		t.Fatalf("Open disk: %v", err)
	}
	resolved, _, err := svc.Open(ctx, projectID, hostID, "memory", 0.91, "")
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
