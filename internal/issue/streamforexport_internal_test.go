package issue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Отдельный от newProject (query_test.go) helper: тот в package issue_test, а этот файл — package issue
// (белый ящик), неэкспортированные символы между ними не шарятся.
func newSnapshotTestProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var userID, orgID, projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ('snap@example.com','x') RETURNING id").Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('snap','Snap',1000000) RETURNING id").Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,'snap-api','Snap API') RETURNING id", orgID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

// Упор в потолок снимка обязан дать отказ, а не тихую усечённую выборку — неполная выгрузка хуже отказа.
// Тест бьёт в неэкспортированный streamForExport, минуя захардкоженный потолок 1_000_000 у StreamForExport.
func TestStreamForExportSnapshotOverflowFails(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	pid := newSnapshotTestProject(t, pool)

	now := time.Now().UTC()
	for i := 0; i < 4; i++ {
		if _, err := svc.Upsert(ctx, pid, "fp-"+string(rune('a'+i)), "t", "c", LevelError, "", now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	err := svc.streamForExport(ctx, pid, Filter{}, 3 /*snapshotLimit*/, func(Issue) error {
		return nil
	})
	if !errors.Is(err, ErrExportSnapshotTooLarge) {
		t.Fatalf("streamForExport вернул %v, want ErrExportSnapshotTooLarge (4 группы > потолка 3)", err)
	}
}

// Ровно snapshotLimit групп — не overflow: переполнение это len(ids) > snapshotLimit, строго больше.
func TestStreamForExportSnapshotWithinLimitSucceeds(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	pid := newSnapshotTestProject(t, pool)

	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if _, err := svc.Upsert(ctx, pid, "fp-"+string(rune('a'+i)), "t", "c", LevelError, "", now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	var got int
	err := svc.streamForExport(ctx, pid, Filter{}, 3 /*snapshotLimit*/, func(Issue) error {
		got++
		return nil
	})
	if err != nil {
		t.Fatalf("streamForExport: %v", err)
	}
	if got != 3 {
		t.Errorf("выгружено %d групп, want 3 (снимок ровно на потолке — не overflow)", got)
	}
}
