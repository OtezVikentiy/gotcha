package host_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func entries(names ...string) []host.TouchEntry {
	out := make([]host.TouchEntry, len(names))
	for i, n := range names {
		out[i] = host.TouchEntry{Name: n}
	}
	return out
}

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

func TestStoreUpsertEnforcesProjectCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	s, projectID := setupProject(t)
	ctx := context.Background()

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

func TestStoreUpsertCeilingAdmitsInArrivalOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	s, projectID := setupProject(t)
	ctx := context.Background()

	almost := make([]host.TouchEntry, 0, host.MaxHostsPerProject-1)
	for i := 0; i < host.MaxHostsPerProject-1; i++ {
		almost = append(almost, host.TouchEntry{Name: fmt.Sprintf("host-%04d", i)})
	}
	if rejected, err := s.Upsert(ctx, projectID, almost); err != nil || rejected != 0 {
		t.Fatalf("Upsert до потолка минус один: rejected=%d err=%v", rejected, err)
	}

	rejected, err := s.Upsert(ctx, projectID, entries("zeta", "mid", "alpha"))
	if err != nil {
		t.Fatalf("Upsert батча сверх потолка: %v", err)
	}
	if rejected != 2 {
		t.Errorf("rejected = %d, want 2 (одно свободное место на три новых имени)", rejected)
	}
	if _, ok, err := s.Get(ctx, projectID, "zeta"); err != nil || !ok {
		t.Errorf("первое приехавшее имя zeta не зарегистрировано в единственное свободное место: ok=%v err=%v", ok, err)
	}
	for _, name := range []string{"mid", "alpha"} {
		if _, ok, err := s.Get(ctx, projectID, name); err != nil || ok {
			t.Errorf("%s зарегистрирован в обход порядка прихода (алфавитный отбор): ok=%v err=%v", name, ok, err)
		}
	}
}

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

func names(hosts []host.Host) string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = h.Name
	}
	return strings.Join(out, ",")
}

func TestListFiltered(t *testing.T) {
	s, projectID := setupProject(t)
	ctx := context.Background()

	if _, err := s.Upsert(ctx, projectID, []host.TouchEntry{{Name: "a", Environment: "prod", Role: "web"}}); err != nil {
		t.Fatalf("upsert a: %v", err)
	}
	if _, err := s.Upsert(ctx, projectID, []host.TouchEntry{{Name: "b", Environment: "prod", Role: "db"}}); err != nil {
		t.Fatalf("upsert b: %v", err)
	}
	if _, err := s.Upsert(ctx, projectID, []host.TouchEntry{{Name: "c"}}); err != nil {
		t.Fatalf("upsert c: %v", err)
	}

	got, err := s.ListFiltered(ctx, projectID, host.HostFilter{Environment: "prod"}, 0)
	if err != nil {
		t.Fatalf("filter env: %v", err)
	}
	if names(got) != "a,b" {
		t.Fatalf("env=prod → %s, want a,b", names(got))
	}

	none, err := s.ListFiltered(ctx, projectID, host.HostFilter{Role: host.HostLabelNone}, 0)
	if err != nil {
		t.Fatalf("filter role=none: %v", err)
	}
	if names(none) != "c" {
		t.Fatalf("role=__none__ → %s, want c", names(none))
	}

	byRole, err := s.ListFiltered(ctx, projectID, host.HostFilter{Role: "db"}, 0)
	if err != nil {
		t.Fatalf("filter role=db: %v", err)
	}
	if names(byRole) != "b" {
		t.Fatalf("role=db → %s, want b", names(byRole))
	}
}

func TestListFilteredNewOnly(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('host-newonly', 'Host NewOnly', 0) RETURNING id").
		Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	var projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'host-newonly', 'Host NewOnly') RETURNING id", orgID).
		Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	s := host.NewStore(pool)

	if _, err := s.Upsert(ctx, projectID, entries("fresh")); err != nil {
		t.Fatalf("upsert fresh: %v", err)
	}
	if _, err := s.Upsert(ctx, projectID, entries("old")); err != nil {
		t.Fatalf("upsert old: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE hosts SET first_seen = now() - interval '2 days' WHERE project_id = $1 AND name = 'old'", projectID); err != nil {
		t.Fatalf("age old host: %v", err)
	}

	got, err := s.ListFiltered(ctx, projectID, host.HostFilter{NewOnly: true}, 0)
	if err != nil {
		t.Fatalf("filter new: %v", err)
	}
	if names(got) != "fresh" {
		t.Fatalf("new=1 → %s, want fresh", names(got))
	}
}

func TestUpsertLabelsNonEmptyWins(t *testing.T) {
	s, projectID := setupProject(t)
	ctx := context.Background()

	if _, err := s.Upsert(ctx, projectID, []host.TouchEntry{{Name: "h1", Environment: "prod", Role: "web"}}); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	if _, err := s.Upsert(ctx, projectID, []host.TouchEntry{{Name: "h1"}}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	h, ok, err := s.Get(ctx, projectID, "h1")
	if err != nil || !ok {
		t.Fatalf("get: %v ok=%v", err, ok)
	}
	if h.Environment != "prod" || h.Role != "web" {
		t.Fatalf("labels=(%q,%q), want (prod,web) — пустой тик затёр метку", h.Environment, h.Role)
	}

	if _, err := s.Upsert(ctx, projectID, []host.TouchEntry{{Name: "h2"}, {Name: "h2", Environment: "stg", Role: "db"}}); err != nil {
		t.Fatalf("upsert 3: %v", err)
	}
	h2, ok, err := s.Get(ctx, projectID, "h2")
	if err != nil || !ok {
		t.Fatalf("get h2: %v ok=%v", err, ok)
	}
	if h2.Environment != "stg" || h2.Role != "db" {
		t.Fatalf("in-batch dedup labels=(%q,%q), want (stg,db)", h2.Environment, h2.Role)
	}
}
