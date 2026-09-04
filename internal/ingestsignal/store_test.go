package ingestsignal_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingestsignal"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// setupProject поднимает мигрированную PG-базу и одну организацию/проект —
// та же заготовка, что deploy_test.go/host/store_test.go.
func setupProject(t *testing.T) (*ingestsignal.Store, int64) {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('is-test', 'IS Test', 0) RETURNING id").
		Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	var projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'is-test', 'IS Test') RETURNING id", orgID).
		Scan(&projectID); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return ingestsignal.NewStore(pool), projectID
}

// TestStoreBumpUpsertsAndIgnoresUnknownProject — два Bump одной пары
// суммируют hits и продвигают last_seen_at до максимума; Bump на
// несуществующий project_id — no-op без ошибки и без строки (K7-5/K7-6:
// project_id при KindKeyInvalid берётся из URL ДО проверки, что проект вообще
// существует).
func TestStoreBumpUpsertsAndIgnoresUnknownProject(t *testing.T) {
	st, pid := setupProject(t)
	ctx := context.Background()

	t1 := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	t2 := time.Now().UTC().Truncate(time.Microsecond)

	if err := st.Bump(ctx, pid, ingestsignal.KindKeyInvalid, 3, t1); err != nil {
		t.Fatalf("bump 1: %v", err)
	}
	if err := st.Bump(ctx, pid, ingestsignal.KindKeyInvalid, 2, t2); err != nil {
		t.Fatalf("bump 2: %v", err)
	}

	got, err := st.ForProject(ctx, pid)
	if err != nil {
		t.Fatalf("for project: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("сигналов %d, want 1: %+v", len(got), got)
	}
	if got[0].Hits != 5 {
		t.Errorf("hits = %d, want 5 (сумма двух Bump)", got[0].Hits)
	}
	if !got[0].LastSeenAt.Equal(t2) {
		t.Errorf("last_seen_at = %v, want %v (максимум, а не последний по порядку записи)", got[0].LastSeenAt, t2)
	}

	// Bump на несуществующий проект — no-op, без ошибки и без строки где бы
	// то ни было.
	const unknownProject = 999999
	if err := st.Bump(ctx, unknownProject, ingestsignal.KindKeyInvalid, 1, time.Now()); err != nil {
		t.Fatalf("bump на неизвестный проект вернул ошибку: %v", err)
	}
	if again, err := st.ForProject(ctx, unknownProject); err != nil || len(again) != 0 {
		t.Errorf("ForProject(неизвестный) = %+v, err=%v, want пусто без ошибки", again, err)
	}

	// Сигнал неизвестного проекта не просочился и в чужую строку.
	if got, err := st.ForProject(ctx, pid); err != nil || len(got) != 1 {
		t.Errorf("ForProject(pid) после bump на чужой проект = %+v, err=%v", got, err)
	}
}

// TestStoreForProjectOrdersByKind и второй kind на том же проекте — ForProject
// отдаёт строки в порядке kind, а не порядке вставки.
func TestStoreForProjectOrdersByKind(t *testing.T) {
	st, pid := setupProject(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	if err := st.Bump(ctx, pid, ingestsignal.KindKeyScope, 1, now); err != nil {
		t.Fatalf("bump key_scope: %v", err)
	}
	if err := st.Bump(ctx, pid, ingestsignal.KindDeprecatedLogs, 1, now); err != nil {
		t.Fatalf("bump deprecated_logs: %v", err)
	}

	got, err := st.ForProject(ctx, pid)
	if err != nil {
		t.Fatalf("for project: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("сигналов %d, want 2: %+v", len(got), got)
	}
	// "deprecated_logs" < "key_scope" лексикографически.
	if got[0].Kind != ingestsignal.KindDeprecatedLogs || got[1].Kind != ingestsignal.KindKeyScope {
		t.Errorf("порядок = [%s, %s], want [deprecated_logs, key_scope]", got[0].Kind, got[1].Kind)
	}
}

// TestStoreClosedPoolReturnsWrappedErrors — minor m1: ветки ошибок Bump/
// ForProject не были покрыты ничем (мутация «убрать fmt.Errorf-обёртку»
// выживала бы молча). Закрытый пул — самый дешёвый способ гарантированно
// получить ошибку от pool.Exec/pool.Query без порчи данных других тестов.
func TestStoreClosedPoolReturnsWrappedErrors(t *testing.T) {
	pool := testenv.MigratedPG(t)
	st := ingestsignal.NewStore(pool)
	pool.Close()
	ctx := context.Background()

	if err := st.Bump(ctx, 1, ingestsignal.KindKeyInvalid, 1, time.Now()); err == nil {
		t.Fatal("Bump на закрытом пуле не вернул ошибку")
	} else if !strings.Contains(err.Error(), "ingestsignal: bump:") {
		t.Errorf("Bump error = %q, want содержит обёртку %q", err, "ingestsignal: bump:")
	}

	if _, err := st.ForProject(ctx, 1); err == nil {
		t.Fatal("ForProject на закрытом пуле не вернул ошибку")
	} else if !strings.Contains(err.Error(), "ingestsignal: for project:") {
		t.Errorf("ForProject error = %q, want содержит обёртку %q", err, "ingestsignal: for project:")
	}
}
