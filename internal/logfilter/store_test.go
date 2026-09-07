package logfilter_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/logfilter"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// randSlug — короткий случайный суффикс для уникальных slug/email между
// тестами общей БД (testenv поднимает один контейнер на пакет). По образцу
// internal/export/store_test.go.
func randSlug(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b)
}

// seedFilterFixtures заводит организацию, проект и двух пользователей —
// минимальный набор внешних ссылок для log_saved_filters (те же
// project_id/users, что и у export_jobs, набор колонок взят из
// internal/export/store_test.go).
func seedFilterFixtures(t *testing.T, pool *pgxpool.Pool) (projectID, alice, bob int64) {
	t.Helper()
	ctx := context.Background()
	slug := "lf-" + randSlug(t)

	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,$1,1000000) RETURNING id",
		slug).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,$2,$2) RETURNING id",
		orgID, slug).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}

	seedUser := func(name string) int64 {
		var id int64
		email := "lf-" + name + "-" + randSlug(t) + "@e.com"
		if err := pool.QueryRow(ctx,
			"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", email).Scan(&id); err != nil {
			t.Fatalf("user %s: %v", name, err)
		}
		return id
	}
	alice = seedUser("alice")
	bob = seedUser("bob")
	return projectID, alice, bob
}

func TestStoreCreateAndVisibility(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	s := logfilter.NewStore(pool)
	projectID, alice, bob := seedFilterFixtures(t, pool)

	preds := []log.Predicate{{Field: log.FieldBody, Op: log.OpNotContains, Value: "buffered"}}

	personal, err := s.Create(ctx, projectID, &alice, alice, "мой шум", preds)
	if err != nil {
		t.Fatalf("create personal: %v", err)
	}
	if personal.Shared() {
		t.Fatalf("фильтр с владельцем не может быть общим")
	}
	if _, err := s.Create(ctx, projectID, nil, alice, "общий шум", preds); err != nil {
		t.Fatalf("create shared: %v", err)
	}

	forBob, err := s.Visible(ctx, projectID, bob)
	if err != nil {
		t.Fatalf("visible: %v", err)
	}
	if len(forBob) != 1 || forBob[0].Name != "общий шум" {
		t.Fatalf("боб видит чужой личный фильтр: %#v", forBob)
	}
}

func TestStoreLimits(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	s := logfilter.NewStore(pool)
	projectID, alice, _ := seedFilterFixtures(t, pool)
	preds := []log.Predicate{{Field: log.FieldBody, Op: log.OpNotContains, Value: "шум"}}

	for i := 0; i < 30; i++ {
		if _, err := s.Create(ctx, projectID, &alice, alice, fmt.Sprintf("личный-%d", i), preds); err != nil {
			t.Fatalf("создание %d-го личного: %v", i, err)
		}
	}
	if _, err := s.Create(ctx, projectID, &alice, alice, "лишний", preds); !errors.Is(err, logfilter.ErrLimitReached) {
		t.Fatalf("31-й личный принят, ожидался ErrLimitReached, получено %v", err)
	}
	// Общие считаются отдельно и на личный лимит не влияют.
	if _, err := s.Create(ctx, projectID, nil, alice, "общий", preds); err != nil {
		t.Fatalf("общий фильтр отклонён личным лимитом: %v", err)
	}
}

func TestStoreNameTaken(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	s := logfilter.NewStore(pool)
	projectID, alice, bob := seedFilterFixtures(t, pool)
	preds := []log.Predicate{{Field: log.FieldBody, Op: log.OpNotContains, Value: "шум"}}

	if _, err := s.Create(ctx, projectID, &alice, alice, "без шума", preds); err != nil {
		t.Fatalf("первый: %v", err)
	}
	if _, err := s.Create(ctx, projectID, &alice, alice, "Без Шума", preds); !errors.Is(err, logfilter.ErrNameTaken) {
		t.Fatalf("имя различается только регистром, ожидался ErrNameTaken, получено %v", err)
	}
	if _, err := s.Create(ctx, projectID, &bob, bob, "без шума", preds); err != nil {
		t.Fatalf("одноимённый личный фильтр другого пользователя отклонён: %v", err)
	}
	if _, err := s.Create(ctx, projectID, nil, alice, "без шума", preds); err != nil {
		t.Fatalf("общий фильтр с именем личного отклонён: %v", err)
	}
}

func TestStoreUserDeletionKeepsSharedDropsPersonal(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	s := logfilter.NewStore(pool)
	projectID, alice, bob := seedFilterFixtures(t, pool)
	preds := []log.Predicate{{Field: log.FieldBody, Op: log.OpNotContains, Value: "шум"}}

	if _, err := s.Create(ctx, projectID, &alice, alice, "личный алисы", preds); err != nil {
		t.Fatalf("личный: %v", err)
	}
	shared, err := s.Create(ctx, projectID, nil, alice, "общий алисы", preds)
	if err != nil {
		t.Fatalf("общий: %v", err)
	}

	if _, err := pool.Exec(ctx, "DELETE FROM users WHERE id = $1", alice); err != nil {
		t.Fatalf("удаление пользователя: %v", err)
	}

	got, err := s.Visible(ctx, projectID, bob)
	if err != nil {
		t.Fatalf("visible: %v", err)
	}
	if len(got) != 1 || got[0].ID != shared.ID {
		t.Fatalf("после удаления автора ожидался один общий фильтр, получено %#v", got)
	}
	if got[0].AuthorUserID != nil {
		t.Fatalf("автор общего фильтра должен обнулиться, а не удалить фильтр")
	}
}

func TestStoreDeleteFilterClearsDefaults(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	s := logfilter.NewStore(pool)
	projectID, alice, bob := seedFilterFixtures(t, pool)
	preds := []log.Predicate{{Field: log.FieldBody, Op: log.OpNotContains, Value: "шум"}}

	shared, err := s.Create(ctx, projectID, nil, alice, "общий", preds)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, uid := range []int64{alice, bob} {
		if err := s.SetDefault(ctx, projectID, uid, shared.ID); err != nil {
			t.Fatalf("умолчание для %d: %v", uid, err)
		}
	}
	if err := s.Delete(ctx, shared.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, uid := range []int64{alice, bob} {
		if _, ok, err := s.Default(ctx, projectID, uid); err != nil || ok {
			t.Errorf("у пользователя %d осталось умолчание на удалённый фильтр (ok=%v, err=%v)", uid, ok, err)
		}
	}
}

func TestStoreDemoteSharedClearsOtherDefaults(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	s := logfilter.NewStore(pool)
	projectID, alice, bob := seedFilterFixtures(t, pool)
	preds := []log.Predicate{{Field: log.FieldBody, Op: log.OpNotContains, Value: "шум"}}

	shared, err := s.Create(ctx, projectID, nil, alice, "общий", preds)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, uid := range []int64{alice, bob} {
		if err := s.SetDefault(ctx, projectID, uid, shared.ID); err != nil {
			t.Fatalf("умолчание для %d: %v", uid, err)
		}
	}

	// Понижение общего до личного: владельцем становится тот, кто понижает.
	if err := s.Update(ctx, shared.ID, "общий", preds, &alice); err != nil {
		t.Fatalf("update: %v", err)
	}

	if _, ok, err := s.Default(ctx, projectID, alice); err != nil || !ok {
		t.Errorf("у нового владельца умолчание должно остаться (ok=%v, err=%v)", ok, err)
	}
	if _, ok, err := s.Default(ctx, projectID, bob); err != nil || ok {
		t.Errorf("у бобa осталось умолчание на ставший невидимым фильтр (ok=%v, err=%v)", ok, err)
	}
}

func TestStoreUnknownPayloadVersion(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	s := logfilter.NewStore(pool)
	projectID, alice, _ := seedFilterFixtures(t, pool)

	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO log_saved_filters (project_id, owner_user_id, author_user_id, name, payload)
		VALUES ($1, $2, $2, 'из будущего', '{"v":99,"predicates":[]}'::jsonb)
		RETURNING id`, projectID, alice).Scan(&id)
	if err != nil {
		t.Fatalf("вставка: %v", err)
	}

	f, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("неизвестная версия payload уронила чтение: %v", err)
	}
	if f.Applicable {
		t.Errorf("фильтр неизвестной версии не должен быть применимым")
	}
	if len(f.Predicates) != 0 {
		t.Errorf("из неизвестной версии не должно разбираться ни одного условия: %#v", f.Predicates)
	}
}
