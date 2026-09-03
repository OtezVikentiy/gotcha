package depsuppress_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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

// silentSeconds — длительность тишины (секунды), с которой оценщик открыл бы
// silent-инцидент: заведомо больше порога по умолчанию.
const silentSeconds = 900.0

// seedSilentIncident открывает инцидент тишины хоста — так же, как это
// делает host.IncidentService.Open (единственный продовый писатель
// host_incidents): current_value/peak_value для kind='silent' — длительность
// тишины в секундах, обе колонки NOT NULL с миграции 0087.
func seedSilentIncident(t *testing.T, pool *pgxpool.Pool, projectID, hostID int64) int64 {
	t.Helper()
	var incidentID int64
	mustScan(t, pool, &incidentID,
		`INSERT INTO host_incidents (project_id, host_id, kind, status, current_value, peak_value, started_at)
		 VALUES ($1,$2,'silent','open',$3,$3,now()) RETURNING id`,
		projectID, hostID, silentSeconds)
	return incidentID
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

// TestStoreUpdate — правка ребра сохраняет id, проходит ту же цепочку
// валидации, что Create, но не спотыкается о собственную старую версию
// (no-op сохранение — не дубликат, разворот единственного ребра — не цикл).
func TestStoreUpdate(t *testing.T) {
	pool := testenv.MigratedPG(t)
	pid, hostID, monID := seedProjectHostMonitor(t, pool)
	st := depsuppress.NewStore(pool)
	ctx := context.Background()

	h2 := seedHost(t, pool, pid, "h2", "db", "prod")
	id, err := st.Create(ctx, depsuppress.Edge{ProjectID: pid, ParentHostID: &hostID, ChildHostID: &h2})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Ребро-свидетель того же проекта: успешные Update ниже не должны его
	// задевать (UPDATE скоупится по id, а не по всему project_id).
	bystander, err := st.Create(ctx, depsuppress.Edge{ProjectID: pid, ParentHostID: &hostID, ChildMonitorID: &monID})
	if err != nil {
		t.Fatalf("Create bystander: %v", err)
	}

	// no-op: сохранение той же формы не должно падать дубликатом самого себя.
	if err := st.Update(ctx, depsuppress.Edge{ID: id, ProjectID: pid, ParentHostID: &hostID, ChildHostID: &h2}); err != nil {
		t.Fatalf("no-op Update: %v", err)
	}

	// разворот ребра A→B в B→A: старая версия исключена из графа
	// цикл-проверки, иначе был бы ложный ErrCycle.
	if err := st.Update(ctx, depsuppress.Edge{ID: id, ProjectID: pid, ParentHostID: &h2, ChildHostID: &hostID}); err != nil {
		t.Fatalf("reverse Update: %v", err)
	}

	// смена ребёнка на монитор; id обязан остаться прежним.
	if err := st.Update(ctx, depsuppress.Edge{ID: id, ProjectID: pid, ParentHostID: &h2, ChildMonitorID: &monID}); err != nil {
		t.Fatalf("Update child to monitor: %v", err)
	}
	list, err := st.List(ctx, pid)
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %+v / %v, want 2 ребра", list, err)
	}
	byID := map[int64]depsuppress.Edge{list[0].ID: list[0], list[1].ID: list[1]}
	got := byID[id]
	if got.ID != id || got.ParentHostID == nil || *got.ParentHostID != h2 ||
		got.ChildMonitorID == nil || *got.ChildMonitorID != monID || got.ChildHostID != nil {
		t.Fatalf("после Update ребро = %+v, want id=%d host(%d)->monitor(%d)", got, id, h2, monID)
	}
	// Свидетель нетронут: правка одного ребра не переписывает соседей проекта.
	bst := byID[bystander]
	if bst.ID != bystander || bst.ParentHostID == nil || *bst.ParentHostID != hostID ||
		bst.ChildMonitorID == nil || *bst.ChildMonitorID != monID {
		t.Fatalf("ребро-свидетель после Update = %+v, want нетронутым host(%d)->monitor(%d)", bst, hostID, monID)
	}
}

// TestStoreUpdateValidation — Update отвергает то же, что Create: дубликат
// ЧУЖОГО ребра, цикл через другое ребро, чужой узел, битую форму; ребро
// чужого проекта или несуществующее — ErrNotFound.
func TestStoreUpdateValidation(t *testing.T) {
	pool := testenv.MigratedPG(t)
	pid, hostID, monID := seedProjectHostMonitor(t, pool)
	st := depsuppress.NewStore(pool)
	ctx := context.Background()

	h2 := seedHost(t, pool, pid, "h2", "db", "prod")
	h3 := seedHost(t, pool, pid, "h3", "cache", "prod")
	e1, err := st.Create(ctx, depsuppress.Edge{ProjectID: pid, ParentHostID: &hostID, ChildHostID: &h2}) // A→B
	if err != nil {
		t.Fatalf("Create e1: %v", err)
	}
	e2, err := st.Create(ctx, depsuppress.Edge{ProjectID: pid, ParentHostID: &h2, ChildHostID: &h3}) // B→C
	if err != nil {
		t.Fatalf("Create e2: %v", err)
	}

	// дубликат другого ребра: правим e2 в форму e1.
	err = st.Update(ctx, depsuppress.Edge{ID: e2, ProjectID: pid, ParentHostID: &hostID, ChildHostID: &h2})
	if !errors.Is(err, depsuppress.ErrDuplicate) {
		t.Fatalf("Update в дубликат e1: err = %v, want ErrDuplicate", err)
	}
	// цикл через ОСТАВШЕЕСЯ ребро: e1 (A→B) правим в C→B при живом B→C.
	err = st.Update(ctx, depsuppress.Edge{ID: e1, ProjectID: pid, ParentHostID: &h3, ChildHostID: &h2})
	if !errors.Is(err, depsuppress.ErrCycle) {
		t.Fatalf("Update с циклом B→C→B: err = %v, want ErrCycle", err)
	}
	// чужой узел в новой форме.
	_, foreignHost, _ := seedProjectHostMonitor(t, pool)
	err = st.Update(ctx, depsuppress.Edge{ID: e1, ProjectID: pid, ParentHostID: &hostID, ChildHostID: &foreignHost})
	if !errors.Is(err, depsuppress.ErrForeignNode) {
		t.Fatalf("Update с чужим узлом: err = %v, want ErrForeignNode", err)
	}
	// битая форма: два родителя.
	err = st.Update(ctx, depsuppress.Edge{ID: e1, ProjectID: pid, ParentHostID: &hostID, ParentMonitorID: &monID, ChildHostID: &h2})
	if !errors.Is(err, depsuppress.ErrInvalidEdge) {
		t.Fatalf("Update с двумя родителями: err = %v, want ErrInvalidEdge", err)
	}
	// ребро чужого проекта — ErrNotFound, содержимое не меняется.
	otherPid, otherHost, otherMon := seedProjectHostMonitor(t, pool)
	foreignEdge, err := st.Create(ctx, depsuppress.Edge{ProjectID: otherPid, ParentMonitorID: &otherMon, ChildHostID: &otherHost})
	if err != nil {
		t.Fatalf("Create foreign edge: %v", err)
	}
	err = st.Update(ctx, depsuppress.Edge{ID: foreignEdge, ProjectID: pid, ParentHostID: &hostID, ChildHostID: &h2})
	if !errors.Is(err, depsuppress.ErrNotFound) {
		t.Fatalf("Update чужого ребра: err = %v, want ErrNotFound", err)
	}
	// несуществующий id — тоже ErrNotFound.
	err = st.Update(ctx, depsuppress.Edge{ID: foreignEdge + 1_000_000, ProjectID: pid, ParentHostID: &hostID, ChildHostID: &h2})
	if !errors.Is(err, depsuppress.ErrNotFound) {
		t.Fatalf("Update несуществующего ребра: err = %v, want ErrNotFound", err)
	}

	list, err := st.List(ctx, otherPid)
	if err != nil || len(list) != 1 || list[0].ParentMonitorID == nil || *list[0].ParentMonitorID != otherMon {
		t.Fatalf("чужое ребро после отвергнутых Update = %+v / %v, want нетронутым", list, err)
	}
}
