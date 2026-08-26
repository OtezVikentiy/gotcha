package export

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// randSlug — короткий случайный суффикс для уникальных slug/email между
// тестами общей БД (testenv поднимает один контейнер на пакет).
func randSlug(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b)
}

// seedUser заводит пользователя без привязки к проекту — второй участник
// теста изоляции по пользователю (см. TestActiveCountsSeparatesUserAndProject).
func seedUser(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	email := "export-" + randSlug(t) + "@e.com"
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("seedUser: %v", err)
	}
	return id
}

// seedProjectAndUser заводит организацию, проект и пользователя-автора —
// минимальный набор для постановки заявки на выгрузку.
func seedProjectAndUser(t *testing.T, pool *pgxpool.Pool) (projectID, userID int64) {
	t.Helper()
	ctx := context.Background()
	slug := "exp-" + randSlug(t)
	var orgID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (slug, name, event_quota) VALUES ($1,$1,1000000) RETURNING id`,
		slug).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (org_id, slug, name) VALUES ($1,$2,$2) RETURNING id`,
		orgID, slug).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	userID = seedUser(t, pool)
	return projectID, userID
}

// mustEnqueue ставит заявку с параметрами по умолчанию (issues/csv) — для
// тестов, которым важен только факт наличия заявки, а не её содержимое.
func mustEnqueue(t *testing.T, st *Store, projectID, userID int64) int64 {
	t.Helper()
	return mustEnqueueKind(t, st, projectID, userID, KindIssues)
}

// mustEnqueueKind — как mustEnqueue, но с явным видом выгрузки.
func mustEnqueueKind(t *testing.T, st *Store, projectID, userID int64, kind Kind) int64 {
	t.Helper()
	now := time.Now().UTC()
	id, err := st.Enqueue(context.Background(), Job{
		ProjectID: projectID, CreatedBy: userID,
		Kind: kind, Format: FormatCSV,
		Params: Params{Since: now.Add(-time.Hour), Until: now},
	})
	if err != nil {
		t.Fatalf("mustEnqueueKind: %v", err)
	}
	return id
}

func TestEnqueueGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	since := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	id, err := st.Enqueue(ctx, Job{
		ProjectID: projectID, CreatedBy: userID,
		Kind: KindIssues, Format: FormatCSV,
		Params: Params{Status: "unresolved", Environment: "prod", Since: since, Until: since.Add(time.Hour)},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusQueued {
		t.Errorf("статус новой заявки = %q, ожидали queued", got.Status)
	}
	if got.Params.Environment != "prod" || !got.Params.Since.Equal(since) {
		t.Errorf("params не пережили round-trip: %+v", got.Params)
	}
	if got.Kind != KindIssues || got.Format != FormatCSV {
		t.Errorf("вид/формат искажены: %v %v", got.Kind, got.Format)
	}
}

// TestEnqueueDefaultsAndScope — расширяет round-trip на поля, которые первый
// тест не трогает: ScopeIssueID, IncludePII, остальные фильтры Params и
// значения по умолчанию, которые заявка получает от схемы (attempts,
// last_error, claimed_at и т.п.). Ловит перепутанные позиции в scanJob —
// TestEnqueueGetRoundTrip такую перестановку не заметил бы, если бы она
// случайно совпала по типам соседних колонок.
func TestEnqueueDefaultsAndScope(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	since := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	id, err := st.Enqueue(ctx, Job{
		ProjectID: projectID, CreatedBy: userID,
		Kind: KindEvents, Format: FormatNDJSON, ScopeIssueID: 42,
		IncludePII: true,
		Params: Params{
			Status: "resolved", Level: "error", Query: "npe",
			Environment: "staging", Sort: "last_seen",
			Since: since, Until: since.Add(24 * time.Hour),
		},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ScopeIssueID != 42 {
		t.Errorf("ScopeIssueID = %d, ожидали 42", got.ScopeIssueID)
	}
	if !got.IncludePII {
		t.Error("IncludePII не пережил round-trip")
	}
	if got.FileExt != "ndjson" {
		t.Errorf("FileExt = %q, ожидали ndjson (по формату)", got.FileExt)
	}
	if got.Params.Level != "error" || got.Params.Query != "npe" || got.Params.Sort != "last_seen" ||
		got.Params.Status != "resolved" || !got.Params.Until.Equal(since.Add(24*time.Hour)) {
		t.Errorf("params не пережили round-trip целиком: %+v", got.Params)
	}
	if got.Attempts != 0 || got.LastError != "" || got.RowsWritten != 0 || got.Bytes != 0 || got.Truncated {
		t.Errorf("новая заявка не в исходном состоянии: %+v", got)
	}
	if got.ClaimedAt != nil || got.FinishedAt != nil || got.ExpiresAt != nil {
		t.Errorf("временные метки новой заявки должны быть nil: %+v", got)
	}
}

// TestEnqueueWithoutScopeStoresNull — заявка без привязки к группе (обычный
// массовый экспорт) обязана вернуть ScopeIssueID=0, а не паниковать на NULL
// в scope_issue_id при сканировании.
func TestEnqueueWithoutScopeStoresNull(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID)

	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ScopeIssueID != 0 {
		t.Errorf("ScopeIssueID = %d, ожидали 0 (без области)", got.ScopeIssueID)
	}
}

// TestEnqueueScopeColumnNullVsZero — проверяет саму колонку scope_issue_id
// напрямую SQL-запросом, а не через Get: jobColumns читает
// coalesce(scope_issue_id, 0), поэтому round-trip через Get не отличает NULL
// от буквального нуля и не заметил бы потерю guard'а на j.ScopeIssueID == 0
// в Enqueue.
func TestEnqueueScopeColumnNullVsZero(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	withoutScope := mustEnqueue(t, st, projectID, userID)
	withScope, err := st.Enqueue(ctx, Job{
		ProjectID: projectID, CreatedBy: userID,
		Kind: KindIssues, Format: FormatCSV, ScopeIssueID: 7,
		Params: Params{Since: time.Now(), Until: time.Now()},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	var isNull bool
	if err := pool.QueryRow(ctx,
		`SELECT scope_issue_id IS NULL FROM export_jobs WHERE id = $1`, withoutScope).Scan(&isNull); err != nil {
		t.Fatalf("проверка колонки (без области): %v", err)
	}
	if !isNull {
		t.Error("scope_issue_id без области должен быть NULL, а не 0")
	}

	var scope int64
	if err := pool.QueryRow(ctx,
		`SELECT scope_issue_id FROM export_jobs WHERE id = $1`, withScope).Scan(&scope); err != nil {
		t.Fatalf("проверка колонки (с областью): %v", err)
	}
	if scope != 7 {
		t.Errorf("scope_issue_id = %d, ожидали 7", scope)
	}
}

func TestGetUnknownIDReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)

	if _, err := st.Get(ctx, 999999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(несуществующий id): err = %v, ожидали ErrNotFound", err)
	}
}

// TestByProjectOrdersNewestFirstAndLimits — сортировка и limit одновременно:
// если ORDER BY потеряется, третий по счёту (случайно попавший в LIMIT
// первым при вставке) не окажется отрезан, и тест это заметит.
func TestByProjectOrdersNewestFirstAndLimits(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	oldID := mustEnqueue(t, st, projectID, userID)
	midID := mustEnqueue(t, st, projectID, userID)
	newID := mustEnqueue(t, st, projectID, userID)

	// created_at выставляется вручную: default now() у трёх вставок подряд
	// может совпасть до микросекунды и сделать порядок недетерминированным.
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	for i, id := range []int64{oldID, midID, newID} {
		if _, err := pool.Exec(ctx, `UPDATE export_jobs SET created_at = $2 WHERE id = $1`,
			id, base.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("подготовка created_at для %d: %v", id, err)
		}
	}

	// export_jobs_list_idx уже отсортирован по (project_id, created_at DESC),
	// и планировщик способен отдать верный порядок его сканированием даже без
	// ORDER BY в запросе — потерю сортировки такое совпадение маскирует. База
	// теста изолированная (своя на тест, testenv.PostgresDSN), поэтому индекс
	// можно снести здесь без риска для остальных тестов пакета.
	if _, err := pool.Exec(ctx, `DROP INDEX export_jobs_list_idx`); err != nil {
		t.Fatalf("снятие индекса: %v", err)
	}

	all, err := st.ByProject(ctx, projectID, 10)
	if err != nil {
		t.Fatalf("ByProject: %v", err)
	}
	if len(all) != 3 || all[0].ID != newID || all[1].ID != midID || all[2].ID != oldID {
		t.Fatalf("ByProject без ограничения = %+v, ожидали [new,mid,old]", ids(all))
	}

	limited, err := st.ByProject(ctx, projectID, 2)
	if err != nil {
		t.Fatalf("ByProject с limit: %v", err)
	}
	if len(limited) != 2 || limited[0].ID != newID || limited[1].ID != midID {
		t.Fatalf("ByProject(limit=2) = %+v, ожидали [new,mid]", ids(limited))
	}
}

// TestByProjectNonPositiveLimit фиксирует контракт ByProject на границах,
// которые сама сигнатура не запрещает: limit — забота вызывающей стороны
// (страница «Выгрузки» задаёт его константой пакета web, как issueEventsLimit
// у EventsForIssue), стор её не подменяет. limit=0 — валидный SQL LIMIT 0,
// пустой список без ошибки; отрицательный — ошибка PostgreSQL ("LIMIT must
// not be negative"), которую ByProject не глотает молча.
func TestByProjectNonPositiveLimit(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	mustEnqueue(t, st, projectID, userID)

	zero, err := st.ByProject(ctx, projectID, 0)
	if err != nil {
		t.Fatalf("ByProject(limit=0): err = %v, ожидали nil", err)
	}
	if len(zero) != 0 {
		t.Errorf("ByProject(limit=0) = %+v, ожидали пустой список", ids(zero))
	}

	if _, err := st.ByProject(ctx, projectID, -1); err == nil {
		t.Fatal("ByProject(limit=-1) вернул nil, ожидали ошибку")
	}
}

func ids(js []Job) []int64 {
	out := make([]int64, len(js))
	for i, j := range js {
		out[i] = j.ID
	}
	return out
}

// TestByProjectIsolatesOtherProjects — заявка чужого проекта не должна
// попасть в выборку: без фильтра по project_id ByProject превратился бы в
// глобальный список заявок всех арендаторов.
func TestByProjectIsolatesOtherProjects(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectA, userA := seedProjectAndUser(t, pool)
	projectB, userB := seedProjectAndUser(t, pool)

	idA := mustEnqueue(t, st, projectA, userA)
	mustEnqueue(t, st, projectB, userB)

	got, err := st.ByProject(ctx, projectA, 10)
	if err != nil {
		t.Fatalf("ByProject: %v", err)
	}
	if len(got) != 1 || got[0].ID != idA {
		t.Fatalf("ByProject(projectA) = %+v, ожидали только заявку %d", ids(got), idA)
	}
}

func TestActiveCountsSeparatesUserAndProject(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userA := seedProjectAndUser(t, pool)
	userB := seedUser(t, pool)

	for i := 0; i < 2; i++ {
		mustEnqueue(t, st, projectID, userA)
	}
	mustEnqueue(t, st, projectID, userB)

	proj, user, err := st.ActiveCounts(ctx, projectID, userA)
	if err != nil {
		t.Fatalf("ActiveCounts: %v", err)
	}
	if proj != 3 {
		t.Errorf("активных на проект = %d, ожидали 3", proj)
	}
	if user != 2 {
		t.Errorf("активных на пользователя = %d, ожидали 2", user)
	}
}

// TestActiveCountsExcludesTerminalStatuses — досчитанная заявка не должна
// занимать место в лимите одновременных выгрузок: иначе автор упрётся в
// потолок из-за заявок, которые давно отработали.
func TestActiveCountsExcludesTerminalStatuses(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	activeID := mustEnqueue(t, st, projectID, userID)
	doneID := mustEnqueue(t, st, projectID, userID)
	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status='done' WHERE id=$1`, doneID); err != nil {
		t.Fatalf("подготовка: %v", err)
	}

	proj, user, err := st.ActiveCounts(ctx, projectID, userID)
	if err != nil {
		t.Fatalf("ActiveCounts: %v", err)
	}
	if proj != 1 || user != 1 {
		t.Fatalf("ActiveCounts после завершения одной заявки = (%d,%d), ожидали (1,1); активная = %d", proj, user, activeID)
	}
}

// TestActiveCountsEmptyProject — на проекте без единой заявки нет строки, по
// которой можно было бы неудачно сматчить агрегат: count(*) обязан дать
// (0,0), а не ошибку/ноль строк.
func TestActiveCountsEmptyProject(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	proj, user, err := st.ActiveCounts(ctx, projectID, userID)
	if err != nil {
		t.Fatalf("ActiveCounts: %v", err)
	}
	if proj != 0 || user != 0 {
		t.Fatalf("ActiveCounts на пустом проекте = (%d,%d), ожидали (0,0)", proj, user)
	}
}

func TestDeleteRefusesNonTerminal(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID)

	if err := st.Delete(ctx, id); !errors.Is(err, ErrNotDeletable) {
		t.Fatalf("удаление queued-заявки: err = %v, ожидали ErrNotDeletable", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status='done' WHERE id=$1`, id); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	if err := st.Delete(ctx, id); err != nil {
		t.Fatalf("удаление done-заявки: %v", err)
	}
	if _, err := st.Get(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("после удаления Get вернул %v, ожидали ErrNotFound", err)
	}
}

// TestDeleteUnknownIDReturnsNotDeletable — несуществующий id ведёт себя как
// незавершённая заявка (нуль затронутых строк), а не как отдельная ошибка:
// со стороны вызывающего это тот же «удалить нельзя».
func TestDeleteUnknownIDReturnsNotDeletable(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)

	if err := st.Delete(ctx, 999999999); !errors.Is(err, ErrNotDeletable) {
		t.Fatalf("Delete(несуществующий id): err = %v, ожидали ErrNotDeletable", err)
	}
}
