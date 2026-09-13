package ingestsignal_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingestsignal"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

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

	const unknownProject = 999999
	if err := st.Bump(ctx, unknownProject, ingestsignal.KindKeyInvalid, 1, time.Now()); err != nil {
		t.Fatalf("bump на неизвестный проект вернул ошибку: %v", err)
	}
	if again, err := st.ForProject(ctx, unknownProject); err != nil || len(again) != 0 {
		t.Errorf("ForProject(неизвестный) = %+v, err=%v, want пусто без ошибки", again, err)
	}

	if got, err := st.ForProject(ctx, pid); err != nil || len(got) != 1 {
		t.Errorf("ForProject(pid) после bump на чужой проект = %+v, err=%v", got, err)
	}
}

func TestStoreBumpResetsHitsAfterResetWindow(t *testing.T) {
	st, pid := setupProject(t)
	ctx := context.Background()

	t1 := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
	if err := st.Bump(ctx, pid, ingestsignal.KindKeyInvalid, 10, t1); err != nil {
		t.Fatalf("bump 1: %v", err)
	}

	// KindKeyInvalid: окно 1ч. t1+2ч — далеко за окном, счётчик обязан начать
	// заново, а не прибавить к hits за прошлый эпизод.
	t2 := t1.Add(2 * time.Hour)
	if err := st.Bump(ctx, pid, ingestsignal.KindKeyInvalid, 3, t2); err != nil {
		t.Fatalf("bump 2: %v", err)
	}

	got, err := st.ForProject(ctx, pid)
	if err != nil {
		t.Fatalf("for project: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("сигналов %d, want 1: %+v", len(got), got)
	}
	if got[0].Hits != 3 {
		t.Errorf("hits = %d, want 3 (разрыв больше окна — старые 10 не в счёт)", got[0].Hits)
	}
	if !got[0].LastSeenAt.Equal(t2) {
		t.Errorf("last_seen_at = %v, want %v", got[0].LastSeenAt, t2)
	}
}

func TestStoreBumpSumsHitsWithinResetWindow(t *testing.T) {
	st, pid := setupProject(t)
	ctx := context.Background()

	t1 := time.Now().Add(-59 * time.Minute).UTC().Truncate(time.Microsecond)
	if err := st.Bump(ctx, pid, ingestsignal.KindKeyInvalid, 10, t1); err != nil {
		t.Fatalf("bump 1: %v", err)
	}

	// Разрыв меньше окна 1ч у KindKeyInvalid — тот же эпизод, hits суммируются.
	t2 := t1.Add(58 * time.Minute)
	if err := st.Bump(ctx, pid, ingestsignal.KindKeyInvalid, 3, t2); err != nil {
		t.Fatalf("bump 2: %v", err)
	}

	got, err := st.ForProject(ctx, pid)
	if err != nil {
		t.Fatalf("for project: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("сигналов %d, want 1: %+v", len(got), got)
	}
	if got[0].Hits != 13 {
		t.Errorf("hits = %d, want 13 (разрыв внутри окна — сумма)", got[0].Hits)
	}
}

func TestStoreBumpSumsHitsAtExactResetWindowBoundary(t *testing.T) {
	st, pid := setupProject(t)
	ctx := context.Background()

	t1 := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
	if err := st.Bump(ctx, pid, ingestsignal.KindKeyInvalid, 10, t1); err != nil {
		t.Fatalf("bump 1: %v", err)
	}

	// Разрыв РОВНО в окно (1ч у KindKeyInvalid) — SQL сравнивает строгим "<",
	// значит граница ещё не сброс, а сумма.
	t2 := t1.Add(time.Hour)
	if err := st.Bump(ctx, pid, ingestsignal.KindKeyInvalid, 3, t2); err != nil {
		t.Fatalf("bump 2: %v", err)
	}

	got, err := st.ForProject(ctx, pid)
	if err != nil {
		t.Fatalf("for project: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("сигналов %d, want 1: %+v", len(got), got)
	}
	if got[0].Hits != 13 {
		t.Errorf("hits = %d, want 13 (разрыв ровно в окно — ещё не сброс)", got[0].Hits)
	}
}

func TestStoreBumpResetsHitsJustPastResetWindowBoundary(t *testing.T) {
	st, pid := setupProject(t)
	ctx := context.Background()

	t1 := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Microsecond)
	if err := st.Bump(ctx, pid, ingestsignal.KindKeyInvalid, 10, t1); err != nil {
		t.Fatalf("bump 1: %v", err)
	}

	// Разрыв на 1с БОЛЬШЕ окна (1ч у KindKeyInvalid) — уже за границей, сброс.
	t2 := t1.Add(time.Hour + time.Second)
	if err := st.Bump(ctx, pid, ingestsignal.KindKeyInvalid, 3, t2); err != nil {
		t.Fatalf("bump 2: %v", err)
	}

	got, err := st.ForProject(ctx, pid)
	if err != nil {
		t.Fatalf("for project: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("сигналов %d, want 1: %+v", len(got), got)
	}
	if got[0].Hits != 3 {
		t.Errorf("hits = %d, want 3 (разрыв за окном — сброс, старые 10 не в счёт)", got[0].Hits)
	}
}

func TestStoreBumpSumsHitsForKindWithoutResetWindow(t *testing.T) {
	st, pid := setupProject(t)
	ctx := context.Background()

	const unknownKind = ingestsignal.Kind("no_such_kind")
	t1 := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Microsecond)
	if err := st.Bump(ctx, pid, unknownKind, 10, t1); err != nil {
		t.Fatalf("bump 1: %v", err)
	}

	// Вид без записи в resetWindow — Bump обязан падать в ttlIndefinite и
	// только накапливать, а не сбрасывать hits на первом же разрыве.
	t2 := t1.Add(24 * time.Hour)
	if err := st.Bump(ctx, pid, unknownKind, 3, t2); err != nil {
		t.Fatalf("bump 2: %v", err)
	}

	got, err := st.ForProject(ctx, pid)
	if err != nil {
		t.Fatalf("for project: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("сигналов %d, want 1: %+v", len(got), got)
	}
	if got[0].Hits != 13 {
		t.Errorf("hits = %d, want 13 (нет resetWindow — только накопление)", got[0].Hits)
	}
}

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
	if got[0].Kind != ingestsignal.KindDeprecatedLogs || got[1].Kind != ingestsignal.KindKeyScope {
		t.Errorf("порядок = [%s, %s], want [deprecated_logs, key_scope]", got[0].Kind, got[1].Kind)
	}
}

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
