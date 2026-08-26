package export

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
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
	return mustEnqueueKind(t, st, projectID, userID, KindIssues, FormatCSV)
}

// mustEnqueueKind — как mustEnqueue, но с явным видом и форматом выгрузки.
func mustEnqueueKind(t *testing.T, st *Store, projectID, userID int64, kind Kind, format Format) int64 {
	t.Helper()
	now := time.Now().UTC()
	id, err := st.Enqueue(context.Background(), Job{
		ProjectID: projectID, CreatedBy: userID,
		Kind: kind, Format: format,
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

func TestClaimTakesOldestQueuedOnce(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	first := mustEnqueue(t, st, projectID, userID)
	mustEnqueue(t, st, projectID, userID)

	got, ok, err := st.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim: %v ok=%v", err, ok)
	}
	if got.ID != first {
		t.Errorf("Claim взял заявку %d, ожидали самую старую %d", got.ID, first)
	}
	if got.Status != StatusRunning || got.Attempts != 1 {
		t.Errorf("после клейма статус=%q attempts=%d", got.Status, got.Attempts)
	}
	again, ok, err := st.Claim(ctx)
	if err != nil {
		t.Fatalf("второй Claim: %v", err)
	}
	if ok && again.ID == first {
		t.Error("одна и та же заявка выдана дважды")
	}
}

func TestClaimReclaimsExpiredLease(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID)
	if _, _, err := st.Claim(ctx); err != nil {
		t.Fatalf("первый Claim: %v", err)
	}
	// Инстанс «упал»: лиза протухла, заявка обязана вернуться в работу.
	if _, err := pool.Exec(ctx,
		`UPDATE export_jobs SET claimed_at = now() - interval '21 minutes' WHERE id=$1`, id); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	got, ok, err := st.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("переклейм: %v ok=%v", err, ok)
	}
	if got.ID != id || got.Attempts != 2 {
		t.Errorf("переклейм вернул id=%d attempts=%d, ожидали %d и 2", got.ID, got.Attempts, id)
	}
}

// TestClaimDoesNotReclaimExhaustedRunningJob — граница attempts == maxAttempts:
// заявка с протухшей лизой, но без оставшихся попыток, не должна снова уйти в
// работу. Её судьба — SweepStale, а не ещё один клейм.
func TestClaimDoesNotReclaimExhaustedRunningJob(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID)
	if _, err := pool.Exec(ctx, `UPDATE export_jobs
		SET status='running', attempts=$2, claimed_at = now() - interval '21 minutes'
		WHERE id=$1`, id, maxAttempts); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	if _, ok, err := st.Claim(ctx); err != nil {
		t.Fatalf("Claim: %v", err)
	} else if ok {
		t.Error("Claim переклеймил заявку с исчерпанными попытками")
	}
}

// TestClaimReclaimsJustBelowMaxAttempts — симметричная граница: одной попытки
// в запасе достаточно, чтобы протухшая лиза вернула заявку в работу.
func TestClaimReclaimsJustBelowMaxAttempts(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID)
	if _, err := pool.Exec(ctx, `UPDATE export_jobs
		SET status='running', attempts=$2, claimed_at = now() - interval '21 minutes'
		WHERE id=$1`, id, maxAttempts-1); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	got, ok, err := st.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("переклейм на границе: %v ok=%v", err, ok)
	}
	if got.ID != id || got.Attempts != maxAttempts {
		t.Errorf("переклейм на границе вернул id=%d attempts=%d, ожидали %d и %d",
			got.ID, got.Attempts, id, maxAttempts)
	}
}

func TestSweepStaleFailsExhaustedJob(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID)
	if _, err := pool.Exec(ctx, `UPDATE export_jobs
		SET status='running', attempts=3, claimed_at = now() - interval '21 minutes'
		WHERE id=$1`, id); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	n, err := st.SweepStale(ctx)
	if err != nil {
		t.Fatalf("SweepStale: %v", err)
	}
	if n != 1 {
		t.Fatalf("SweepStale обработал %d заявок, ожидали 1", n)
	}
	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusFailed || got.LastError == "" {
		t.Errorf("зависшая заявка осталась в статусе %q, причина %q", got.Status, got.LastError)
	}
	// Заявка с исчерпанными попытками не должна больше выдаваться в работу.
	if _, ok, _ := st.Claim(ctx); ok {
		t.Error("Claim выдал заявку с исчерпанными попытками")
	}
}

func TestDoneSetsExpiryFromFinish(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID)
	claim, _, err := st.Claim(ctx)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := st.Done(ctx, id, claim.Attempts, 42, 4096, true, 48*time.Hour); err != nil {
		t.Fatalf("Done: %v", err)
	}
	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusDone || got.RowsWritten != 42 || got.Bytes != 4096 || !got.Truncated {
		t.Errorf("итог заявки: %+v", got)
	}
	if got.FinishedAt == nil || got.ExpiresAt == nil {
		t.Fatal("Done не проставил finished_at/expires_at")
	}
	if d := got.ExpiresAt.Sub(*got.FinishedAt); d < 47*time.Hour || d > 49*time.Hour {
		t.Errorf("срок хранения отсчитан не от завершения: разница %v", d)
	}
}

func TestFailRetriesThenGivesUp(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID)

	for i := 1; i <= maxAttempts; i++ {
		claim, _, err := st.Claim(ctx)
		if err != nil {
			t.Fatalf("Claim %d: %v", i, err)
		}
		if err := st.Fail(ctx, id, claim.Attempts, "ClickHouse недоступен"); err != nil {
			t.Fatalf("Fail %d: %v", i, err)
		}
		got, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
		want := StatusQueued
		if i == maxAttempts {
			want = StatusFailed
		}
		if got.Status != want {
			t.Errorf("после %d-й неудачи статус %q, ожидали %q", i, got.Status, want)
		}
	}
}

// TestDoneIgnoresAlreadyFinalizedJob — Done не должен воскрешать заявку,
// которую тем временем уже закрыл SweepStale (протухшая лиза, попытки
// исчерпаны): «застрявший» вызов Done из зомби-воркера получает
// ErrStaleClaim и не имеет права откатить финальный статус обратно на
// 'done', даже если номер попытки совпадает — status='running' в связке
// с attempts защищает и от этого случая.
func TestDoneIgnoresAlreadyFinalizedJob(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID)
	if _, err := pool.Exec(ctx, `UPDATE export_jobs
		SET status='running', attempts=3, claimed_at = now() - interval '21 minutes'
		WHERE id=$1`, id); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	if n, err := st.SweepStale(ctx); err != nil || n != 1 {
		t.Fatalf("SweepStale: n=%d err=%v", n, err)
	}
	if err := st.Done(ctx, id, 3, 1, 1, false, time.Hour); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("Done: err=%v, ожидали ErrStaleClaim", err)
	}
	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("Done воскресил заявку из failed в %q", got.Status)
	}
}

// TestFailIgnoresAlreadyDoneJob — симметричный случай: Fail из зомби-вызова
// получает ErrStaleClaim и не откатывает уже успешно досчитанную заявку в
// очередь.
func TestFailIgnoresAlreadyDoneJob(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID)
	claim, _, err := st.Claim(ctx)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := st.Done(ctx, id, claim.Attempts, 1, 1, false, time.Hour); err != nil {
		t.Fatalf("Done: %v", err)
	}
	if err := st.Fail(ctx, id, claim.Attempts, "запоздавшая ошибка зомби-воркера"); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("Fail: err=%v, ожидали ErrStaleClaim", err)
	}
	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusDone {
		t.Errorf("Fail откатил уже завершённую заявку в %q", got.Status)
	}
}

// TestDoneRejectsStaleClaimAfterReclaim — подтверждённый сценарий гонки:
// A клеймит заявку, лиза протухает, её подбирает B (attempts вырос), и
// запоздавший Done от A обязан получить ErrStaleClaim и не тронуть строку —
// иначе заявка финализировалась бы устаревшими данными A, а Done от B, у
// которого строка увести уже некому, тихо потерял бы результат.
func TestDoneRejectsStaleClaimAfterReclaim(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID)

	claimA, ok, err := st.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim A: %v ok=%v", err, ok)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE export_jobs SET claimed_at = now() - interval '21 minutes' WHERE id=$1`, id); err != nil {
		t.Fatalf("протухание лизы A: %v", err)
	}
	claimB, ok, err := st.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim B (переклейм): %v ok=%v", err, ok)
	}
	if claimB.Attempts != claimA.Attempts+1 {
		t.Fatalf("B перехватил заявку с attempts=%d, ожидали %d", claimB.Attempts, claimA.Attempts+1)
	}

	// A не знает о перехвате и дописывает СВОИ (устаревшие) результаты.
	if err := st.Done(ctx, id, claimA.Attempts, 111, 222, false, time.Hour); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("Done от A: err=%v, ожидали ErrStaleClaim", err)
	}
	afterA, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get после Done A: %v", err)
	}
	if afterA.Status != StatusRunning || afterA.RowsWritten != 0 {
		t.Fatalf("устаревший Done от A изменил заявку: %+v", afterA)
	}

	// B ведёт актуальную попытку — его Done обязан пройти и записать его данные.
	if err := st.Done(ctx, id, claimB.Attempts, 999, 888, true, time.Hour); err != nil {
		t.Fatalf("Done от B: %v", err)
	}
	final, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get после Done B: %v", err)
	}
	if final.Status != StatusDone || final.RowsWritten != 999 || final.Bytes != 888 || !final.Truncated {
		t.Fatalf("итог заявки не соответствует данным B: %+v", final)
	}
}

// TestFailRejectsStaleClaimAfterReclaim — та же гонка со стороны Fail:
// запоздавшая неудача от A не должна откатывать попытку, которую уже
// ведёт B.
func TestFailRejectsStaleClaimAfterReclaim(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID)

	claimA, ok, err := st.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim A: %v ok=%v", err, ok)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE export_jobs SET claimed_at = now() - interval '21 minutes' WHERE id=$1`, id); err != nil {
		t.Fatalf("протухание лизы A: %v", err)
	}
	claimB, ok, err := st.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim B (переклейм): %v ok=%v", err, ok)
	}

	if err := st.Fail(ctx, id, claimA.Attempts, "устаревшая ошибка A"); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("Fail от A: err=%v, ожидали ErrStaleClaim", err)
	}
	afterA, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get после Fail A: %v", err)
	}
	if afterA.Status != StatusRunning || afterA.Attempts != claimB.Attempts {
		t.Fatalf("устаревший Fail от A изменил заявку: %+v", afterA)
	}

	if err := st.Fail(ctx, id, claimB.Attempts, "реальная ошибка B"); err != nil {
		t.Fatalf("Fail от B: %v", err)
	}
	final, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get после Fail B: %v", err)
	}
	if final.Status != StatusQueued || final.LastError != "реальная ошибка B" {
		t.Fatalf("итог заявки не соответствует Fail от B: %+v", final)
	}
}

// TestClaimConcurrentDoesNotDoubleAssign — настоящая конкурентность: несколько
// горутин со своими соединениями одновременно бьются за пул заявок. Без
// корректной блокировки строки (FOR UPDATE SKIP LOCKED) два клейма могли бы
// увидеть одну и ту же «самую старую» заявку в своих снапшотах и оба её
// забрать.
// Раунды и высокое соотношение воркеров к заявкам — не украшение: без
// FOR UPDATE SKIP LOCKED окно гонки между чтением «самой старой заявки» и
// её захватом узкое, и один раунд с малым числом участников ловит поломку
// не всегда (наблюдалось ~3 из 10 прогонов при 12 воркерах на 6 заявок).
// Барьер запускает все горутины раунда одновременно, а несколько раундов
// подряд убирают зависимость результата от разового везения планировщика.
func TestClaimConcurrentDoesNotDoubleAssign(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	const rounds = 8
	const jobsPerRound = 3
	const workersPerRound = 20

	for round := 0; round < rounds; round++ {
		ids := make(map[int64]bool, jobsPerRound)
		for i := 0; i < jobsPerRound; i++ {
			ids[mustEnqueue(t, st, projectID, userID)] = true
		}

		start := make(chan struct{})
		var mu sync.Mutex
		claimed := make(map[int64]int)
		var wg sync.WaitGroup
		wg.Add(workersPerRound)
		for i := 0; i < workersPerRound; i++ {
			go func() {
				defer wg.Done()
				<-start
				got, ok, err := st.Claim(ctx)
				if err != nil {
					t.Errorf("раунд %d: Claim: %v", round, err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				claimed[got.ID]++
				mu.Unlock()
			}()
		}
		close(start) // отпускает все горутины раунда одним махом
		wg.Wait()

		if len(claimed) != jobsPerRound {
			t.Fatalf("раунд %d: забрано %d уникальных заявок из %d, ожидали все",
				round, len(claimed), jobsPerRound)
		}
		for id, n := range claimed {
			if n != 1 {
				t.Errorf("раунд %d: заявка %d выдана %d раз(а), ожидали ровно один", round, id, n)
			}
			if !ids[id] {
				t.Errorf("раунд %d: забрана неизвестная заявка %d", round, id)
			}
		}
	}
}

// TestClaimConcurrentReclaimsExpiredLeaseExactlyOnce — та же гарантия для
// переклейма: параллельные попытки подобрать одну просроченную лизу обязаны
// отдать её ровно одному вызову, остальные — уйти с ok=false.
func TestClaimConcurrentReclaimsExpiredLeaseExactlyOnce(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID)
	if _, err := pool.Exec(ctx, `UPDATE export_jobs
		SET status='running', attempts=1, claimed_at = now() - interval '21 minutes'
		WHERE id=$1`, id); err != nil {
		t.Fatalf("подготовка: %v", err)
	}

	const workers = 10
	var mu sync.Mutex
	wins := 0
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			got, ok, err := st.Claim(ctx)
			if err != nil {
				t.Errorf("Claim: %v", err)
				return
			}
			if ok && got.ID == id {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("протухшую лизу забрали %d раз(а), ожидали ровно один", wins)
	}
	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Attempts != 2 {
		t.Errorf("после единственного переклейма attempts=%d, ожидали 2", got.Attempts)
	}
}

// TestFailPermanentIgnoresRetryBudgetButRespectsAttemptFence — в отличие от
// Fail, FailPermanent обязан закрыть заявку сразу при СОВПАДАЮЩЕМ attempt, а
// не вернуть её в очередь: первая попытка (attempts=1, меньше maxAttempts=3)
// через обычный Fail ушла бы обратно в 'queued', и именно поэтому воркер
// зовёт FailPermanent для причин, которые повтор не исправит. Игнорируется
// только потолок попыток — не сам номер попытки (см.
// TestFailPermanentRejectsStaleClaimAfterReclaim).
func TestFailPermanentIgnoresRetryBudgetButRespectsAttemptFence(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID)
	claim, ok, err := st.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}

	if err := st.FailPermanent(ctx, id, claim.Attempts, "на диске не осталось места"); err != nil {
		t.Fatalf("FailPermanent: %v", err)
	}

	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("статус %q, ожидали failed невзирая на attempts=%d < maxAttempts", got.Status, got.Attempts)
	}
	if got.Attempts != 1 {
		t.Errorf("FailPermanent не должен трогать attempts: %d", got.Attempts)
	}
	if got.LastError != "на диске не осталось места" {
		t.Errorf("last_error = %q", got.LastError)
	}
	if got.FinishedAt == nil {
		t.Error("finished_at не выставлен")
	}
}

// TestFailPermanentRejectsStaleClaimAfterReclaim — тот же сценарий гонки, что
// у Done/Fail: A клеймит заявку, лиза протухает, её подбирает B (attempts
// вырос и B активно работает), и запоздавший постоянный отказ от A обязан
// получить ErrStaleClaim и не тронуть строку. Без фенсинга по attempt
// FailPermanent закрыл бы заявку как failed поверх активной попытки B — дыра
// шире обычного зомби-Done, потому что FailPermanent не ждёт даже исчерпания
// попыток.
func TestFailPermanentRejectsStaleClaimAfterReclaim(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID)

	claimA, ok, err := st.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim A: %v ok=%v", err, ok)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE export_jobs SET claimed_at = now() - interval '21 minutes' WHERE id=$1`, id); err != nil {
		t.Fatalf("протухание лизы A: %v", err)
	}
	claimB, ok, err := st.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim B (переклейм): %v ok=%v", err, ok)
	}
	if claimB.Attempts != claimA.Attempts+1 {
		t.Fatalf("B перехватил заявку с attempts=%d, ожидали %d", claimB.Attempts, claimA.Attempts+1)
	}

	// A не знает о перехвате и зовёт постоянный отказ по СВОЕЙ (устаревшей) попытке.
	if err := st.FailPermanent(ctx, id, claimA.Attempts, "диск переполнен"); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("FailPermanent от A: err=%v, ожидали ErrStaleClaim", err)
	}

	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusRunning || got.Attempts != claimB.Attempts {
		t.Fatalf("постоянный отказ A закрыл активную попытку B: %+v", got)
	}
}

// TestFailPermanentUnknownIDReturnsStaleClaim — постоянный отказ заявки,
// которую успели удалить, неотличим по фенсингу от переклейма (0 затронутых
// строк в обоих случаях) — тот же ErrStaleClaim, что и у Fail/Done для того
// же входа.
func TestFailPermanentUnknownIDReturnsStaleClaim(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	if err := st.FailPermanent(ctx, 0, 1, "не важно"); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("FailPermanent по несуществующему id: err=%v, ожидали ErrStaleClaim", err)
	}
}

// TestDueForExpiryReturnsOnlyExpiredDone — выборка обязана видеть только
// done-заявки с просроченным expires_at: живой done (срок ещё не наступил) и
// queued (даже с NULL expires_at) не должны попасть в проход джанитора.
func TestDueForExpiryReturnsOnlyExpiredDone(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	expiredID := mustEnqueue(t, st, projectID, userID)
	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status='done', finished_at = now(),
		expires_at = now() - interval '1 hour' WHERE id = $1`, expiredID); err != nil {
		t.Fatalf("подготовка истёкшей заявки: %v", err)
	}

	liveID := mustEnqueue(t, st, projectID, userID)
	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status='done', finished_at = now(),
		expires_at = now() + interval '1 hour' WHERE id = $1`, liveID); err != nil {
		t.Fatalf("подготовка живой заявки: %v", err)
	}

	_ = mustEnqueue(t, st, projectID, userID) // queued, expires_at NULL — не наш случай

	jobs, err := st.DueForExpiry(ctx)
	if err != nil {
		t.Fatalf("DueForExpiry: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != expiredID {
		t.Fatalf("DueForExpiry вернул %+v, ожидали только заявку %d", jobs, expiredID)
	}
}

// TestMarkExpiredGuardsStatus — MarkExpired обязан трогать только заявки в
// статусе done: заявку, ещё стоящую в очереди (queued), пометить expired
// нельзя — файл по ней ещё не создавался и удалять нечего.
func TestMarkExpiredGuardsStatus(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)
	id := mustEnqueue(t, st, projectID, userID) // остаётся queued

	if err := st.MarkExpired(ctx, []int64{id}); err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}
	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusQueued {
		t.Fatalf("MarkExpired тронул queued-заявку: статус стал %q", got.Status)
	}

	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status='done' WHERE id=$1`, id); err != nil {
		t.Fatalf("подготовка done: %v", err)
	}
	if err := st.MarkExpired(ctx, []int64{id}); err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}
	got, err = st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusExpired {
		t.Fatalf("MarkExpired не перевёл done-заявку в expired: статус %q", got.Status)
	}
}

// TestMarkExpiredEmptyIDsIsNoop — пустой список id не должен бить по базе
// SQL-запросом с пустым ANY($1): вызывающая сторона (Janitor) собирает список
// из циклa, который на пустых входных данных может дать nil-срез.
func TestMarkExpiredEmptyIDsIsNoop(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	if err := st.MarkExpired(ctx, nil); err != nil {
		t.Fatalf("MarkExpired(nil): %v", err)
	}
}

// TestPurgeRowsRemovesOnlyOldTerminal — история чистится по finished_at и
// только у терминальных статусов: свежая терминальная заявка и активная
// (queued/running), сколько бы она ни висела, остаются нетронутыми.
func TestPurgeRowsRemovesOnlyOldTerminal(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	oldID := mustEnqueue(t, st, projectID, userID)
	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status='failed',
		finished_at = now() - interval '40 days' WHERE id = $1`, oldID); err != nil {
		t.Fatalf("подготовка старой заявки: %v", err)
	}

	freshID := mustEnqueue(t, st, projectID, userID)
	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status='done',
		finished_at = now() - interval '1 hour' WHERE id = $1`, freshID); err != nil {
		t.Fatalf("подготовка свежей заявки: %v", err)
	}

	// Активная заявка без finished_at, "состаренная" по created_at — Purge
	// не должен цепляться за created_at вовсе, только за finished_at
	// терминальных статусов.
	activeID := mustEnqueue(t, st, projectID, userID)
	if _, err := pool.Exec(ctx, `UPDATE export_jobs SET created_at = now() - interval '40 days'
		WHERE id = $1`, activeID); err != nil {
		t.Fatalf("подготовка активной заявки: %v", err)
	}

	n, err := st.PurgeRows(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeRows: %v", err)
	}
	if n != 1 {
		t.Fatalf("PurgeRows удалил %d строк, ожидали 1", n)
	}
	if _, err := st.Get(ctx, oldID); err != ErrNotFound {
		t.Errorf("старая терминальная строка не вычищена: err=%v", err)
	}
	if _, err := st.Get(ctx, freshID); err != nil {
		t.Errorf("свежая терминальная строка ошибочно удалена: %v", err)
	}
	if _, err := st.Get(ctx, activeID); err != nil {
		t.Errorf("активная заявка ошибочно удалена: %v", err)
	}
}

// TestPurgeRowsContinuesBeyondBatch — цикл обязан пройти больше одного
// батча: janitorBatchSize временно занижается до 2, строк — пять, значит без
// продолжения цикла после первого батча часть строк осталась бы жить.
func TestPurgeRowsContinuesBeyondBatch(t *testing.T) {
	origBatch := janitorBatchSize
	janitorBatchSize = 2
	defer func() { janitorBatchSize = origBatch }()

	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	const total = 5
	ids := make([]int64, total)
	for i := range ids {
		id := mustEnqueue(t, st, projectID, userID)
		if _, err := pool.Exec(ctx, `UPDATE export_jobs SET status='failed',
			finished_at = now() - interval '40 days' WHERE id = $1`, id); err != nil {
			t.Fatalf("подготовка заявки %d: %v", i, err)
		}
		ids[i] = id
	}

	n, err := st.PurgeRows(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeRows: %v", err)
	}
	if n != total {
		t.Fatalf("PurgeRows удалил %d из %d — цикл остановился на первом батче (лимит %d)", n, total, janitorBatchSize)
	}
	for _, id := range ids {
		if _, err := st.Get(ctx, id); err != ErrNotFound {
			t.Errorf("строка %d не вычищена: err=%v", id, err)
		}
	}
}

// TestExistingIDsReturnsSubset — джанитор сверяет файлы каталога со строками
// именно так: из произвольного набора id возвращаются только реально
// существующие, несуществующие тихо выпадают, а не превращаются в ошибку.
func TestExistingIDsReturnsSubset(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	projectID, userID := seedProjectAndUser(t, pool)

	id1 := mustEnqueue(t, st, projectID, userID)
	id2 := mustEnqueue(t, st, projectID, userID)

	got, err := st.ExistingIDs(ctx, []int64{id1, id2, 999999999})
	if err != nil {
		t.Fatalf("ExistingIDs: %v", err)
	}
	if !got[id1] || !got[id2] || got[999999999] {
		t.Fatalf("ExistingIDs вернул %v, ожидали {%d:true, %d:true}", got, id1, id2)
	}
}

// TestExistingIDsEmptyInput — пустой список на входе не должен бить по базе
// SQL-запросом с пустым ANY($1) — тот же случай, что и MarkExpired.
func TestExistingIDsEmptyInput(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	st := NewStore(pool)
	got, err := st.ExistingIDs(ctx, nil)
	if err != nil {
		t.Fatalf("ExistingIDs(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ExistingIDs(nil) = %v, ожидали пустую карту", got)
	}
}
