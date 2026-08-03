package org_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// queued — стоит ли заявка на очистку телеметрии проекта.
func queued(t *testing.T, pool *pgxpool.Pool, projectID int64) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM project_purge_queue WHERE project_id = $1)",
		projectID).Scan(&ok); err != nil {
		t.Fatalf("чтение очереди: %v", err)
	}
	return ok
}

// TestDeleteProjectEnqueuesPurge — заявка на очистку ClickHouse обязана
// появиться той же транзакцией, что удаляет проект: без неё телеметрия
// становится неадресуемой (идентификатора проекта после каскада нет нигде).
func TestDeleteProjectEnqueuesPurge(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "purge-owner@example.com")
	o, err := svc.CreateOrg(ctx, "purge-proj", "Purge", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	p, err := svc.CreateProject(ctx, o.ID, "api", "API", "go")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	if err := svc.DeleteProject(ctx, p.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if !queued(t, pool, p.ID) {
		t.Errorf("проект удалён, а заявки на очистку телеметрии нет — данные стали неадресуемы")
	}
}

// TestDeleteProjectMissingLeavesNoRequest — удаление несуществующего проекта
// не должно оставлять заявку: откат транзакции снимает и вставку. Иначе
// исполнитель гонял бы восемь мутаций по проекту, которого не было.
func TestDeleteProjectMissingLeavesNoRequest(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ghost = int64(999_000_001)
	if err := svc.DeleteProject(ctx, ghost); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("DeleteProject(несуществующий) = %v, ожидалась ErrNotFound", err)
	}
	if queued(t, pool, ghost) {
		t.Errorf("осталась заявка на несуществующий проект")
	}
}

// TestDeleteOrgEnqueuesPurgeForEveryProject — заявки ставятся ДО удаления,
// выборкой по org_id: каскад уничтожает строки projects, и после него
// идентификаторы узнать неоткуда.
func TestDeleteOrgEnqueuesPurgeForEveryProject(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "purge-org-owner@example.com")
	o, err := svc.CreateOrg(ctx, "purge-org", "Purge Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	var ids []int64
	for _, slug := range []string{"api", "site", "worker"} {
		p, err := svc.CreateProject(ctx, o.ID, slug, slug, "go")
		if err != nil {
			t.Fatalf("CreateProject(%s): %v", slug, err)
		}
		ids = append(ids, p.ID)
	}

	if err := svc.DeleteOrg(ctx, o.ID); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM project_purge_queue WHERE project_id = ANY($1)", ids).Scan(&n); err != nil {
		t.Fatalf("чтение очереди: %v", err)
	}
	if n != len(ids) {
		t.Errorf("заявок %d при %d проектах организации: телеметрия части проектов останется в ClickHouse навсегда", n, len(ids))
	}
}

// TestDeleteOrgMissingLeavesNoRequest — удаление несуществующей организации
// заявок не оставляет (проектов у неё нет, но проверка фиксирует, что откат
// работает и на этом пути).
func TestDeleteOrgMissingLeavesNoRequest(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := svc.DeleteOrg(ctx, 999_000_002); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("DeleteOrg(несуществующая) = %v, ожидалась ErrNotFound", err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM project_purge_queue").Scan(&n); err != nil {
		t.Fatalf("чтение очереди: %v", err)
	}
	if n != 0 {
		t.Errorf("в очереди %d заявок после удаления несуществующей организации", n)
	}
}
