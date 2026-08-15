package host_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// entries — короткая сборка []host.TouchEntry из одних имён (без версии
// агента), чтобы не плодить составные литералы в каждом вызове Upsert
// тестов, которым версия не важна.
func entries(names ...string) []host.TouchEntry {
	out := make([]host.TouchEntry, len(names))
	for i, n := range names {
		out[i] = host.TouchEntry{Name: n}
	}
	return out
}

// setupProject поднимает мигрированную PG-базу и одну организацию/проект —
// заготовка, общая для всех тестов пакета.
func setupProject(t *testing.T) (*host.Store, int64) {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('host-test', 'Host Test', 0) RETURNING id").
		Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	var projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'host-test', 'Host Test') RETURNING id", orgID).
		Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return host.NewStore(pool), projectID
}

// TestStoreUpsertThenList — два новых имени в одном Upsert → List отдаёт обе,
// отсортированные по имени, first_seen==last_seen (только что вставлены).
func TestStoreUpsertThenList(t *testing.T) {
	s, projectID := setupProject(t)
	ctx := context.Background()

	if _, err := s.Upsert(ctx, projectID, entries("web-02", "web-01")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := s.List(ctx, projectID, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(List) = %d, want 2", len(got))
	}
	if got[0].Name != "web-01" || got[1].Name != "web-02" {
		t.Fatalf("List не отсортирован по имени: %+v", got)
	}
	for _, h := range got {
		if !h.FirstSeen.Equal(h.LastSeen) {
			t.Fatalf("host %q: FirstSeen=%v != LastSeen=%v сразу после первого Upsert", h.Name, h.FirstSeen, h.LastSeen)
		}
		if h.ProjectID != projectID {
			t.Fatalf("host %q: ProjectID = %d, want %d", h.Name, h.ProjectID, projectID)
		}
	}
}

// TestStoreUpsertAgainBumpsLastSeen — повторный Upsert одного и того же имени
// двигает last_seen вперёд, first_seen остаётся прежним.
func TestStoreUpsertAgainBumpsLastSeen(t *testing.T) {
	s, projectID := setupProject(t)
	ctx := context.Background()

	if _, err := s.Upsert(ctx, projectID, entries("web-01")); err != nil {
		t.Fatalf("Upsert #1: %v", err)
	}
	before, ok, err := s.Get(ctx, projectID, "web-01")
	if err != nil || !ok {
		t.Fatalf("Get после Upsert #1: ok=%v err=%v", ok, err)
	}

	// Гарантируем видимую разницу времени: now() у PostgreSQL имеет реальное
	// разрешение, но без задержки два запроса могут попасть в одну и ту же
	// точку часов на быстрой машине.
	time.Sleep(10 * time.Millisecond)

	if _, err := s.Upsert(ctx, projectID, entries("web-01")); err != nil {
		t.Fatalf("Upsert #2: %v", err)
	}
	after, ok, err := s.Get(ctx, projectID, "web-01")
	if err != nil || !ok {
		t.Fatalf("Get после Upsert #2: ok=%v err=%v", ok, err)
	}

	if !after.FirstSeen.Equal(before.FirstSeen) {
		t.Fatalf("FirstSeen изменился: было %v, стало %v", before.FirstSeen, after.FirstSeen)
	}
	if !after.LastSeen.After(before.LastSeen) {
		t.Fatalf("LastSeen не вырос: было %v, стало %v", before.LastSeen, after.LastSeen)
	}
	if after.ID != before.ID {
		t.Fatalf("ID изменился: было %d, стало %d (должна быть та же строка)", before.ID, after.ID)
	}
}

// TestStoreUpsertDeduplicatesNamesInBatch — дубли в одном срезе не должны
// ронять запрос: unnest+ON CONFLICT DO UPDATE падает («cannot affect row a
// second time»), если один INSERT конфликтует сам с собой, поэтому Upsert
// обязан дедуплицировать names внутри себя (контракт из брифа Task 4).
func TestStoreUpsertDeduplicatesNamesInBatch(t *testing.T) {
	s, projectID := setupProject(t)
	ctx := context.Background()

	if _, err := s.Upsert(ctx, projectID, entries("a", "a", "b", "a")); err != nil {
		t.Fatalf("Upsert с дублями: %v", err)
	}

	got, err := s.List(ctx, projectID, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(List) = %d, want 2 (дубли не должны плодить строки)", len(got))
	}
}

// TestStoreDeleteIdempotent — Delete существующего хоста → ok=true, повторный
// Delete того же имени → ok=false.
func TestStoreDeleteIdempotent(t *testing.T) {
	s, projectID := setupProject(t)
	ctx := context.Background()

	if _, err := s.Upsert(ctx, projectID, entries("web-01")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	ok, err := s.Delete(ctx, projectID, "web-01")
	if err != nil {
		t.Fatalf("Delete #1: %v", err)
	}
	if !ok {
		t.Fatal("Delete #1: ok = false, want true (хост существовал)")
	}

	ok, err = s.Delete(ctx, projectID, "web-01")
	if err != nil {
		t.Fatalf("Delete #2: %v", err)
	}
	if ok {
		t.Fatal("Delete #2: ok = true, want false (хост уже удалён)")
	}

	got, err := s.List(ctx, projectID, 0)
	if err != nil {
		t.Fatalf("List после Delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(List) = %d после Delete, want 0", len(got))
	}
}

// TestStoreGetNotFound — Get несуществующего имени → ok=false, без ошибки.
func TestStoreGetNotFound(t *testing.T) {
	s, projectID := setupProject(t)
	ctx := context.Background()

	_, ok, err := s.Get(ctx, projectID, "no-such-host")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("Get: ok = true для несуществующего хоста")
	}
}

// TestStoreListActiveWithProject — оценщик метрик по хосту читает все
// проекты сразу и фильтрует по свежести last_seen; свежий хост в один
// проект, устаревший (руками отодвинутый last_seen) в другой — попадает
// только первый.
func TestStoreListActiveWithProject(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('host-active', 'Host Active', 0) RETURNING id").
		Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	var freshProjectID, staleProjectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'host-fresh', 'Fresh') RETURNING id", orgID).
		Scan(&freshProjectID); err != nil {
		t.Fatalf("insert fresh project: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'host-stale', 'Stale') RETURNING id", orgID).
		Scan(&staleProjectID); err != nil {
		t.Fatalf("insert stale project: %v", err)
	}

	s := host.NewStore(pool)
	if _, err := s.Upsert(ctx, freshProjectID, entries("fresh-01")); err != nil {
		t.Fatalf("Upsert fresh: %v", err)
	}
	if _, err := s.Upsert(ctx, staleProjectID, entries("stale-01")); err != nil {
		t.Fatalf("Upsert stale: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE hosts SET last_seen = now() - interval '2 days' WHERE project_id = $1", staleProjectID); err != nil {
		t.Fatalf("age stale host: %v", err)
	}

	active, err := s.ListActiveWithProject(ctx, 24*time.Hour, 0)
	if err != nil {
		t.Fatalf("ListActiveWithProject: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("len(active) = %d, want 1: %+v", len(active), active)
	}
	if active[0].Name != "fresh-01" || active[0].ProjectID != freshProjectID {
		t.Fatalf("active[0] = %+v, want fresh-01/%d", active[0], freshProjectID)
	}
}

// TestStoreUpsertEnforcesProjectCeiling — потолок MaxHostsPerProject: новые
// имена сверх него не регистрируются и попадают в возвращаемое число
// отброшенных, а УЖЕ известные хосты продолжают обновлять last_seen. Иначе
// парк, доросший до границы, целиком провалился бы в ложную тишину.
func TestStoreUpsertEnforcesProjectCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	s, projectID := setupProject(t)
	ctx := context.Background()

	// Наливаем ровно потолок за один батч.
	full := make([]host.TouchEntry, 0, host.MaxHostsPerProject)
	for i := 0; i < host.MaxHostsPerProject; i++ {
		full = append(full, host.TouchEntry{Name: fmt.Sprintf("host-%04d", i)})
	}
	rejected, err := s.Upsert(ctx, projectID, full)
	if err != nil {
		t.Fatalf("Upsert потолка: %v", err)
	}
	if rejected != 0 {
		t.Fatalf("rejected = %d при заполнении ровно до потолка, want 0", rejected)
	}

	// Известное имя рядом с двумя новыми: новые отбрасываются, известное
	// обновляется.
	before, ok, err := s.Get(ctx, projectID, "host-0000")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	rejected, err = s.Upsert(ctx, projectID, entries("host-0000", "pod-xxxx", "pod-yyyy"))
	if err != nil {
		t.Fatalf("Upsert сверх потолка: %v", err)
	}
	if rejected != 2 {
		t.Errorf("rejected = %d, want 2 (два новых имени сверх потолка)", rejected)
	}
	if _, ok, err := s.Get(ctx, projectID, "pod-xxxx"); err != nil || ok {
		t.Errorf("новое имя зарегистрировано сверх потолка: ok=%v err=%v", ok, err)
	}
	after, ok, err := s.Get(ctx, projectID, "host-0000")
	if err != nil || !ok {
		t.Fatalf("Get после: ok=%v err=%v", ok, err)
	}
	if !after.LastSeen.After(before.LastSeen) {
		t.Errorf("last_seen известного хоста не обновился при упоре в потолок (%v → %v)",
			before.LastSeen, after.LastSeen)
	}
}

// TestStoreUpsertRejectsPathTraversalNames — "." и ".." не регистрируются:
// url.PathEscape точки не экранирует, и /hosts/.. нормализуется браузером в
// адрес проекта — открыть или удалить такой хост в интерфейсе было бы нечем.
func TestStoreUpsertRejectsPathTraversalNames(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	s, projectID := setupProject(t)
	ctx := context.Background()

	if _, err := s.Upsert(ctx, projectID, entries(".", "..", "", "web-01")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := s.List(ctx, projectID, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Name != "web-01" {
		t.Fatalf("зарегистрировано %+v, want только web-01", got)
	}
}

// TestStoreListRespectsLimit — потолок применяется в SQL: страница списка
// вычитывает свои строки, а не весь реестр проекта.
func TestStoreListRespectsLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	s, projectID := setupProject(t)
	ctx := context.Background()

	if _, err := s.Upsert(ctx, projectID, entries("a", "b", "c")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := s.List(ctx, projectID, 2)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("List(limit=2) = %+v, want первые два по имени", got)
	}
}

// TestStoreListActiveRespectsLimit — та же дисциплина у выборки оценщика:
// сорвавшаяся дисциплина регистрации не должна превращаться в неограниченную
// выборку в памяти узла оценки.
func TestStoreListActiveRespectsLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	s, projectID := setupProject(t)
	ctx := context.Background()

	if _, err := s.Upsert(ctx, projectID, entries("a", "b", "c")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := s.ListActiveWithProject(ctx, time.Hour, 2)
	if err != nil {
		t.Fatalf("ListActiveWithProject: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

// TestUpsertAgentVersion — версия агента переживает Upsert без версии
// (collector-путь): пустая AgentVersion в батче НЕ затирает уже известную
// (спека §3.2), а непустая — обновляет.
func TestUpsertAgentVersion(t *testing.T) {
	s, projectID := setupProject(t)
	ctx := context.Background()

	if _, err := s.Upsert(ctx, projectID, []host.TouchEntry{{Name: "web-1", AgentVersion: "0.6.0"}}); err != nil {
		t.Fatalf("Upsert с версией: %v", err)
	}
	got, ok, err := s.Get(ctx, projectID, "web-1")
	if err != nil || !ok {
		t.Fatalf("Get после первого Upsert: ok=%v err=%v", ok, err)
	}
	if got.AgentVersion != "0.6.0" {
		t.Fatalf("AgentVersion = %q, want 0.6.0", got.AgentVersion)
	}

	// Повторный Upsert без версии (собственно collector-путь: OTel-коллектор
	// не знает о версии агента) — версия обязана остаться прежней.
	if _, err := s.Upsert(ctx, projectID, []host.TouchEntry{{Name: "web-1"}}); err != nil {
		t.Fatalf("Upsert без версии: %v", err)
	}
	got, ok, err = s.Get(ctx, projectID, "web-1")
	if err != nil || !ok {
		t.Fatalf("Get после Upsert без версии: ok=%v err=%v", ok, err)
	}
	if got.AgentVersion != "0.6.0" {
		t.Fatalf("AgentVersion = %q после Upsert без версии, want сохранённые 0.6.0", got.AgentVersion)
	}

	// Новая версия — обновляет.
	if _, err := s.Upsert(ctx, projectID, []host.TouchEntry{{Name: "web-1", AgentVersion: "0.6.1"}}); err != nil {
		t.Fatalf("Upsert с новой версией: %v", err)
	}
	got, ok, err = s.Get(ctx, projectID, "web-1")
	if err != nil || !ok {
		t.Fatalf("Get после Upsert с новой версией: ok=%v err=%v", ok, err)
	}
	if got.AgentVersion != "0.6.1" {
		t.Fatalf("AgentVersion = %q, want 0.6.1", got.AgentVersion)
	}
}

// TestUpsertAgentVersionNewHost — новый хост сразу с версией: INSERT-ветка
// (не ON CONFLICT) тоже обязана сохранить AgentVersion, не только UPDATE.
func TestUpsertAgentVersionNewHost(t *testing.T) {
	s, projectID := setupProject(t)
	ctx := context.Background()

	if _, err := s.Upsert(ctx, projectID, []host.TouchEntry{{Name: "agent-new", AgentVersion: "0.6.0"}}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, ok, err := s.Get(ctx, projectID, "agent-new")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.AgentVersion != "0.6.0" {
		t.Fatalf("AgentVersion = %q, want 0.6.0 у нового хоста", got.AgentVersion)
	}
}
