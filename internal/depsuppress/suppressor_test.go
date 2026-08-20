package depsuppress_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/depsuppress"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestParentDownExplicit проверяет прямое подавление через явное ребро
// монитор→хост: пока родитель-монитор жив, ParentDown лжив; как только у
// него открывается uptime-инцидент, ParentDown становится true, а HasParent
// подтверждает, что ребро вообще задекларировано.
func TestParentDownExplicit(t *testing.T) {
	pool := testenv.MigratedPG(t)
	pid, hostID, monID := seedProjectHostMonitor(t, pool)
	if _, err := depsuppress.NewStore(pool).Create(context.Background(), depsuppress.Edge{
		ProjectID: pid, ParentMonitorID: &monID, ChildHostID: &hostID}); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	sup := depsuppress.NewSuppressor(pool)
	ctx := context.Background()
	// родитель жив → ложь
	if down, err := sup.ParentDown(ctx, "host", hostID); err != nil || down {
		t.Fatalf("parent up → ParentDown = %v/%v, want false", down, err)
	}
	// открыть uptime-инцидент родителя-монитора
	mustExec(t, pool, `INSERT INTO incidents (monitor_id, started_at) VALUES ($1, now())`, monID)
	sup2 := depsuppress.NewSuppressor(pool) // свежий (без кеша прошлого)
	if down, err := sup2.ParentDown(ctx, "host", hostID); err != nil || !down {
		t.Fatalf("parent down → ParentDown = %v/%v, want true", down, err)
	}
	if has, err := sup2.HasParent(ctx, "host", hostID); err != nil || !has {
		t.Fatalf("HasParent = %v/%v, want true", has, err)
	}
}

// TestParentDownLabelAndTransitive проверяет резолвинг label-селектора
// (родитель-хост → дети по role=web) и self-match исключение: ребро на
// собственную группу не должно подавлять самого родителя.
//
// Ребро parent=gwHost(role=web) → child_label role=web вставляется НАПРЯМУЮ
// в alert_dependencies, а не через Store.Create: gwHost из
// seedProjectHostMonitor уже имеет role='web', и Store.checkSelfMatch (T2)
// отверг бы именно такое ребро на этапе создания (ErrSelfMatch) — здесь же
// проверяется defense-in-depth самого Suppressor на случай, если такая
// строка всё же оказалась в таблице (смена роли хоста после создания ребра
// и т.п.): резолвинг обязан исключать self-match независимо от того, как
// ребро туда попало.
func TestParentDownLabelAndTransitive(t *testing.T) {
	pool := testenv.MigratedPG(t)
	pid, gwHost, monID := seedProjectHostMonitor(t, pool) // gwHost role='web' env='prod'
	_ = monID
	web := seedHost(t, pool, pid, "web1", "web", "prod")
	mustExec(t, pool, `INSERT INTO alert_dependencies (project_id, parent_host_id, child_label_scope, child_label_value)
		VALUES ($1,$2,'role','web')`, pid, gwHost)
	mustExec(t, pool, `INSERT INTO host_incidents (project_id, host_id, kind, status, started_at)
		VALUES ($1,$2,'silent','open',now())`, pid, gwHost)
	sup := depsuppress.NewSuppressor(pool)
	ctx := context.Background()
	if down, err := sup.ParentDown(ctx, "host", web); err != nil || !down {
		t.Fatalf("web-host под упавшим gw (по метке role=web) → ParentDown = %v/%v, want true", down, err)
	}
	// self-match: сам gwHost (role=web) НЕ должен подавляться собственным ребром
	if down, err := sup.ParentDown(ctx, "host", gwHost); err != nil || down {
		t.Fatalf("gw подавляет сам себя через self-match (баг MAJOR-5): ParentDown = %v/%v", down, err)
	}
}

// TestTransitiveChainViaOpenIntermediate фиксирует инвариант MINOR-7:
// ParentDown смотрит только на ПРЯМЫХ родителей узла и не фильтрует их
// инциденты по suppressed_by_dep. B — промежуточный узел цепочки A→B→C,
// у него открытый silent-инцидент, уже помеченный как подавленный
// (suppressed_by_dep=true), но это не мешает B продолжать подавлять C.
func TestTransitiveChainViaOpenIntermediate(t *testing.T) {
	pool := testenv.MigratedPG(t)
	pid, b, monID := seedProjectHostMonitor(t, pool)
	_ = monID
	c := seedHost(t, pool, pid, "c", "db", "prod")
	if _, err := depsuppress.NewStore(pool).Create(context.Background(), depsuppress.Edge{
		ProjectID: pid, ParentHostID: &b, ChildHostID: &c}); err != nil {
		t.Fatalf("create edge B->C: %v", err)
	}

	var incID int64
	mustScan(t, pool, &incID, `INSERT INTO host_incidents (project_id, host_id, kind, status, started_at)
		VALUES ($1,$2,'silent','open',now()) RETURNING id`, pid, b)
	mustExec(t, pool, `UPDATE host_incidents SET suppressed_by_dep = true WHERE id = $1`, incID)

	sup := depsuppress.NewSuppressor(pool)
	if down, err := sup.ParentDown(context.Background(), "host", c); err != nil || !down {
		t.Fatalf("ParentDown(C) через открытый-но-подавленный B = %v/%v, want true", down, err)
	}
}

// TestMarkSuppressed проверяет единственного писателя флага для host-
// инцидентов: source="host" ставит suppressed_by_dep, прочие source — no-op.
func TestMarkSuppressed(t *testing.T) {
	pool := testenv.MigratedPG(t)
	pid, hostID, _ := seedProjectHostMonitor(t, pool)
	var incID int64
	mustScan(t, pool, &incID, `INSERT INTO host_incidents (project_id, host_id, kind, status, started_at)
		VALUES ($1,$2,'silent','open',now()) RETURNING id`, pid, hostID)

	sup := depsuppress.NewSuppressor(pool)
	ctx := context.Background()
	if err := sup.MarkSuppressed(ctx, "host", incID); err != nil {
		t.Fatalf("MarkSuppressed(host): %v", err)
	}
	var flag bool
	if err := pool.QueryRow(ctx, `SELECT suppressed_by_dep FROM host_incidents WHERE id=$1`, incID).Scan(&flag); err != nil {
		t.Fatalf("select suppressed_by_dep: %v", err)
	}
	if !flag {
		t.Fatal("suppressed_by_dep want true after MarkSuppressed")
	}

	if err := sup.MarkSuppressed(ctx, "monitor", incID); err != nil {
		t.Fatalf("MarkSuppressed(monitor): want nil (no-op), got %v", err)
	}
}

// TestParentDownLabelDoesNotCrossProjectBoundary — находка ревью (CRITICAL):
// label-ребро одного проекта (родитель gw role=web, проект P1) не должно
// матчить одноимённую метку role=web у хоста ДРУГОГО проекта (P2) — метки
// типовые (web/prod/db) и коллизируют между тенантами. Без сверки project_id
// упавший gw в P1 подавлял бы реальный инцидент чужого хоста в P2 (тихая
// потеря алертов). Контрольная проверка: хост с той же меткой В ТОМ ЖЕ
// проекте P1 обязан подавляться как обычно.
func TestParentDownLabelDoesNotCrossProjectBoundary(t *testing.T) {
	pool := testenv.MigratedPG(t)
	p1, gw, mon1 := seedProjectHostMonitor(t, pool) // gw role='web' env='prod', проект P1
	_ = mon1
	p1Web := seedHost(t, pool, p1, "p1-web", "web", "prod") // контроль: тот же проект P1
	p2, p2Web, _ := seedProjectHostMonitor(t, pool)         // независимый проект P2
	_ = p2

	mustExec(t, pool, `INSERT INTO alert_dependencies (project_id, parent_host_id, child_label_scope, child_label_value)
		VALUES ($1,$2,'role','web')`, p1, gw)
	mustExec(t, pool, `INSERT INTO host_incidents (project_id, host_id, kind, status, started_at)
		VALUES ($1,$2,'silent','open',now())`, p1, gw)

	sup := depsuppress.NewSuppressor(pool)
	ctx := context.Background()

	if down, err := sup.ParentDown(ctx, "host", p2Web); err != nil || down {
		t.Fatalf("хост ЧУЖОГО проекта P2 не должен подавляться label-ребром P1: ParentDown = %v/%v, want false", down, err)
	}
	if down, err := sup.ParentDown(ctx, "host", p1Web); err != nil || !down {
		t.Fatalf("контроль: хост СВОЕГО проекта P1 обязан подавляться: ParentDown = %v/%v, want true", down, err)
	}
}

// TestCheckIncidentHostSource проверяет резолвинг по инциденту (не по
// узлу), как его вызывает escalation-scheduler: source="host" join'ит
// host_id и резолвит HasParent/ParentDown; прочие source — сразу
// (false,false,nil), без запроса к БД.
func TestCheckIncidentHostSource(t *testing.T) {
	pool := testenv.MigratedPG(t)
	pid, hostID, monID := seedProjectHostMonitor(t, pool)
	if _, err := depsuppress.NewStore(pool).Create(context.Background(), depsuppress.Edge{
		ProjectID: pid, ParentMonitorID: &monID, ChildHostID: &hostID}); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	mustExec(t, pool, `INSERT INTO incidents (monitor_id, started_at) VALUES ($1, now())`, monID)

	var incID int64
	mustScan(t, pool, &incID, `INSERT INTO host_incidents (project_id, host_id, kind, status, started_at)
		VALUES ($1,$2,'silent','open',now()) RETURNING id`, pid, hostID)

	sup := depsuppress.NewSuppressor(pool)
	ctx := context.Background()
	if hasParent, parentDown, err := sup.CheckIncident(ctx, "host", incID); err != nil || !hasParent || !parentDown {
		t.Fatalf("CheckIncident(host,%d) = %v/%v/%v, want true/true/nil", incID, hasParent, parentDown, err)
	}
	if hasParent, parentDown, err := sup.CheckIncident(ctx, "monitor", 999999); err != nil || hasParent || parentDown {
		t.Fatalf("CheckIncident(monitor,...) = %v/%v/%v, want false/false/nil", hasParent, parentDown, err)
	}
}
