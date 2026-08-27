package issue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// newSnapshotTestProject — вставка проекта напрямую SQL, тот же приём, что
// у newProject в query_test.go (package issue_test). Тот helper отсюда
// недоступен: этот файл — package issue (белый ящик, нужен доступ к
// неэкспортированному streamForExport), а неэкспортированные символы не
// шарятся между файлами package issue и package issue_test в одной
// директории.
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

// TestStreamForExportSnapshotOverflowFails — упор в потолок снимка обязан
// дать отказ (ErrExportSnapshotTooLarge), а не тихо обрезанный по НЕПРАВИЛЬНОЙ
// границе снимок (см. докблок ErrExportSnapshotTooLarge в query.go): молча
// неполная выгрузка хуже отказа, тот же принцип, что и у ErrTooManyIssues в
// internal/export.
//
// Тест бьёт в неэкспортированный streamForExport с потолком-параметром
// (streamForExport(..., snapshotLimit, ...)), а не в публичный
// StreamForExport (issueExportSnapshotSafetyLimit = 1_000_000 захардкожен) —
// иначе для срабатывания пришлось бы вставить больше миллиона строк на
// каждый прогон теста.
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

// TestStreamForExportSnapshotWithinLimitSucceeds — регресс-гарантия рядом с
// overflow-тестом: РОВНО snapshotLimit подходящих групп — не overflow (снимок
// запрашивает snapshotLimit+1, переполнение — это len(ids) > snapshotLimit,
// СТРОГО больше). Мутация "> заменить на >=" на соседней строке иначе прошла
// бы этот файл незамеченной.
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
