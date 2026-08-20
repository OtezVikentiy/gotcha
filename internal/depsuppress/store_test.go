package depsuppress_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/depsuppress"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// randSlug возвращает короткий случайный идентификатор для уникальных
// slug'ов организации/проекта между тестами общей БД.
func randSlug(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b)
}

// mustScan выполняет запрос и сканирует единственную колонку результата в dst.
func mustScan(t *testing.T, pool *pgxpool.Pool, dst *int64, sql string, args ...any) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(dst); err != nil {
		t.Fatalf("scan %q: %v", sql, err)
	}
}

// mustExec выполняет запрос без ожидания результата (проверяет только ошибку).
func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// seedHost создаёт хост в существующем проекте с заданными именем/ролью/окружением.
func seedHost(t *testing.T, pool *pgxpool.Pool, projectID int64, name, role, env string) int64 {
	t.Helper()
	var hostID int64
	mustScan(t, pool, &hostID,
		`INSERT INTO hosts (project_id, name, environment, role) VALUES ($1,$2,$3,$4) RETURNING id`,
		projectID, name, env, role)
	return hostID
}

// seedProjectHostMonitor создаёт организацию, проект, хост (role='web',
// env='prod') и монитор — минимальный набор для тестов store.go.
func seedProjectHostMonitor(t *testing.T, pool *pgxpool.Pool) (projectID, hostID, monitorID int64) {
	t.Helper()
	var orgID int64
	slug := "ds-" + randSlug(t)
	mustScan(t, pool, &orgID,
		`INSERT INTO organizations (slug,name,event_quota) VALUES ($1,$1,0) RETURNING id`, slug)
	mustScan(t, pool, &projectID,
		`INSERT INTO projects (org_id,slug,name) VALUES ($1,$2,$2) RETURNING id`, orgID, slug)
	hostID = seedHost(t, pool, projectID, "h1", "web", "prod")
	mustScan(t, pool, &monitorID,
		`INSERT INTO monitors (project_id, name, kind, interval_seconds)
		 VALUES ($1,'m1','http',60) RETURNING id`, projectID)
	return projectID, hostID, monitorID
}

func TestStoreCreateAndList(t *testing.T) {
	pool := testenv.MigratedPG(t)
	pid, hostID, monID := seedProjectHostMonitor(t, pool)
	st := depsuppress.NewStore(pool)
	id, err := st.Create(context.Background(), depsuppress.Edge{
		ProjectID: pid, ParentMonitorID: &monID, ChildHostID: &hostID,
	})
	if err != nil || id == 0 {
		t.Fatalf("Create: id=%d err=%v", id, err)
	}
	list, err := st.List(context.Background(), pid)
	if err != nil || len(list) != 1 || list[0].ID != id {
		t.Fatalf("List = %+v / %v, want 1 ребро id=%d", list, err, id)
	}
}

func TestStoreValidation(t *testing.T) {
	pool := testenv.MigratedPG(t)
	pid, hostID, monID := seedProjectHostMonitor(t, pool) // host role='web'
	st := depsuppress.NewStore(pool)
	ctx := context.Background()
	// self-loop: parent host == child host
	if _, err := st.Create(ctx, depsuppress.Edge{ProjectID: pid, ParentHostID: &hostID, ChildHostID: &hostID}); err == nil {
		t.Fatal("self-loop: want error")
	}
	// self-match label: parent host gw, child_label role='web', сам host role='web' → self-match
	scope, val := "role", "web"
	if _, err := st.Create(ctx, depsuppress.Edge{ProjectID: pid, ParentHostID: &hostID,
		ChildLabelScope: &scope, ChildLabelValue: &val}); err == nil {
		t.Fatal("self-match label: want ErrSelfMatch")
	}
	// foreign node: чужой проект
	otherPid, otherHost, _ := seedProjectHostMonitor(t, pool)
	_ = otherPid
	if _, err := st.Create(ctx, depsuppress.Edge{ProjectID: pid, ParentMonitorID: &monID, ChildHostID: &otherHost}); err == nil {
		t.Fatal("foreign child node: want ErrForeignNode")
	}
	// cycle среди явных узлов: A(host)→B(monitor уже нельзя, разные типы); используем два хоста
	h2 := seedHost(t, pool, pid, "h2", "db", "prod")
	if _, err := st.Create(ctx, depsuppress.Edge{ProjectID: pid, ParentHostID: &hostID, ChildHostID: &h2}); err != nil {
		t.Fatalf("edge A->B: %v", err)
	}
	if _, err := st.Create(ctx, depsuppress.Edge{ProjectID: pid, ParentHostID: &h2, ChildHostID: &hostID}); err == nil {
		t.Fatal("cycle B->A: want ErrCycle")
	}
	// дубликат
	if _, err := st.Create(ctx, depsuppress.Edge{ProjectID: pid, ParentHostID: &hostID, ChildHostID: &h2}); err == nil {
		t.Fatal("duplicate edge: want ErrDuplicate")
	}
}
