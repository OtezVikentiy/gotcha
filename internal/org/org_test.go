package org_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// newUser вставляет пользователя напрямую: org-пакет не зависит от auth.
func newUser(t *testing.T, pool *pgxpool.Pool, email string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		"INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id", email).Scan(&id)
	if err != nil {
		t.Fatalf("newUser: %v", err)
	}
	return id
}

func TestCreateOrgAndRoles(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "owner@example.com")
	o, err := svc.CreateOrg(ctx, "acme", "Acme Inc", owner)
	if err != nil || o.ID == 0 {
		t.Fatalf("CreateOrg: %+v err=%v", o, err)
	}
	if o.EventQuota != 1_000_000 {
		t.Errorf("EventQuota = %d, want default 1000000", o.EventQuota)
	}
	if _, err := svc.CreateOrg(ctx, "acme", "Other", owner); !errors.Is(err, org.ErrSlugTaken) {
		t.Fatalf("duplicate slug: got %v, want ErrSlugTaken", err)
	}

	if r, err := svc.Role(ctx, o.ID, owner); err != nil || r != org.RoleOwner {
		t.Fatalf("owner role: r=%q err=%v", r, err)
	}
	stranger := newUser(t, pool, "stranger@example.com")
	if _, err := svc.Role(ctx, o.ID, stranger); !errors.Is(err, org.ErrNotMember) {
		t.Fatalf("stranger: got %v, want ErrNotMember", err)
	}

	dev := newUser(t, pool, "dev@example.com")
	if err := svc.AddMember(ctx, o.ID, dev, org.RoleMember); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := svc.SetRole(ctx, o.ID, dev, org.RoleAdmin); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	if r, _ := svc.Role(ctx, o.ID, dev); r != org.RoleAdmin {
		t.Fatalf("dev role = %q, want admin", r)
	}
}

func TestInvalidRole(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "roleowner@example.com")
	o, err := svc.CreateOrg(ctx, "roleorg", "Role Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	dev := newUser(t, pool, "roledev@example.com")
	if err := svc.AddMember(ctx, o.ID, dev, "superuser"); !errors.Is(err, org.ErrInvalidRole) {
		t.Fatalf("AddMember bad role: got %v, want ErrInvalidRole", err)
	}
	if err := svc.SetRole(ctx, o.ID, owner, "root"); !errors.Is(err, org.ErrInvalidRole) {
		t.Fatalf("SetRole bad role: got %v, want ErrInvalidRole", err)
	}
}

func TestCreateOrgInvalidSlug(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "slugowner@example.com")
	for _, slug := range []string{"Bad Slug!", ""} {
		if _, err := svc.CreateOrg(ctx, slug, "Name", owner); !errors.Is(err, org.ErrInvalidSlug) {
			t.Errorf("CreateOrg(%q, ...): got %v, want ErrInvalidSlug", slug, err)
		}
	}
	if _, err := svc.CreateOrg(ctx, "my-org-1", "Name", owner); err != nil {
		t.Errorf("CreateOrg(%q, ...): unexpected err %v", "my-org-1", err)
	}
}

func TestLastOwnerProtected(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "solo@example.com")
	o, err := svc.CreateOrg(ctx, "solo", "Solo", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if err := svc.SetRole(ctx, o.ID, owner, org.RoleMember); !errors.Is(err, org.ErrLastOwner) {
		t.Fatalf("demote last owner: got %v, want ErrLastOwner", err)
	}
	if err := svc.RemoveMember(ctx, o.ID, owner); !errors.Is(err, org.ErrLastOwner) {
		t.Fatalf("remove last owner: got %v, want ErrLastOwner", err)
	}
	// Второй owner снимает защиту.
	second := newUser(t, pool, "second@example.com")
	if err := svc.AddMember(ctx, o.ID, second, org.RoleOwner); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := svc.SetRole(ctx, o.ID, owner, org.RoleMember); err != nil {
		t.Fatalf("demote with two owners: %v", err)
	}
}

func TestValidSlug(t *testing.T) {
	cases := []struct {
		slug string
		want bool
	}{
		{"ok-1", true},
		{"Bad!", false},
	}
	for _, c := range cases {
		if got := org.ValidSlug(c.slug); got != c.want {
			t.Errorf("ValidSlug(%q) = %v, want %v", c.slug, got, c.want)
		}
	}
}

func TestDeleteOrg(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "delowner@example.com")
	o, err := svc.CreateOrg(ctx, "del-org", "Del Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	if err := svc.DeleteOrg(ctx, o.ID); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}

	// Каскадом должно удалиться и членство.
	if _, err := svc.Role(ctx, o.ID, owner); !errors.Is(err, org.ErrNotMember) {
		t.Fatalf("Role after DeleteOrg: got %v, want ErrNotMember", err)
	}

	if err := svc.DeleteOrg(ctx, o.ID); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("DeleteOrg (already deleted): got %v, want ErrNotFound", err)
	}
}

func TestMembersOf(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "membersof-owner@example.com")
	o, err := svc.CreateOrg(ctx, "membersof-org", "Members Of", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	dev := newUser(t, pool, "membersof-dev@example.com")
	if err := svc.AddMember(ctx, o.ID, dev, org.RoleMember); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	// Посторонний из другой организации не должен попасть в выборку.
	otherOwner := newUser(t, pool, "membersof-other@example.com")
	if _, err := svc.CreateOrg(ctx, "membersof-other-org", "Other", otherOwner); err != nil {
		t.Fatalf("CreateOrg (other): %v", err)
	}

	members, err := svc.MembersOf(ctx, o.ID)
	if err != nil {
		t.Fatalf("MembersOf: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("MembersOf = %+v, want 2 members", members)
	}
	// Отсортированы по email: dev раньше owner.
	if members[0].Email != "membersof-dev@example.com" || members[0].UserID != dev || members[0].Role != org.RoleMember {
		t.Errorf("members[0] = %+v, want dev/member", members[0])
	}
	if members[1].Email != "membersof-owner@example.com" || members[1].UserID != owner || members[1].Role != org.RoleOwner {
		t.Errorf("members[1] = %+v, want owner/owner", members[1])
	}
}

func TestOrgsOf(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uid := newUser(t, pool, "orgsof-user@example.com")

	zOrg, err := svc.CreateOrg(ctx, "orgsof-zeta", "Zeta", uid)
	if err != nil {
		t.Fatalf("CreateOrg (zeta): %v", err)
	}
	other := newUser(t, pool, "orgsof-alpha-owner@example.com")
	aOrg, err := svc.CreateOrg(ctx, "orgsof-alpha", "Alpha", other)
	if err != nil {
		t.Fatalf("CreateOrg (alpha): %v", err)
	}
	if err := svc.AddMember(ctx, aOrg.ID, uid, org.RoleMember); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	// Организация без uid не должна протекать в выборку.
	stranger := newUser(t, pool, "orgsof-stranger@example.com")
	if _, err := svc.CreateOrg(ctx, "orgsof-stranger-org", "Stranger", stranger); err != nil {
		t.Fatalf("CreateOrg (stranger): %v", err)
	}

	orgs, err := svc.OrgsOf(ctx, uid)
	if err != nil {
		t.Fatalf("OrgsOf: %v", err)
	}
	if len(orgs) != 2 {
		t.Fatalf("OrgsOf = %+v, want 2 orgs", orgs)
	}
	if orgs[0].ID != aOrg.ID || orgs[1].ID != zOrg.ID {
		t.Errorf("OrgsOf order = %+v, want alpha before zeta", orgs)
	}

	// Юзер без организаций получает пустой (не nil-паникующий) срез.
	lonely := newUser(t, pool, "orgsof-lonely@example.com")
	orgs, err = svc.OrgsOf(ctx, lonely)
	if err != nil {
		t.Fatalf("OrgsOf (lonely): %v", err)
	}
	if len(orgs) != 0 {
		t.Fatalf("OrgsOf (lonely) = %+v, want empty", orgs)
	}
}

func TestUsageAndQuota(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "usage-owner@example.com")
	o, err := svc.CreateOrg(ctx, "usage-org", "Usage Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	jan := time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)
	if n, err := svc.Usage(ctx, o.ID, jan); err != nil || n != 0 {
		t.Fatalf("Usage (empty): n=%d err=%v, want 0", n, err)
	}

	if n, err := svc.IncUsage(ctx, o.ID, jan); err != nil || n != 1 {
		t.Fatalf("IncUsage (1st): n=%d err=%v, want 1", n, err)
	}
	if n, err := svc.IncUsage(ctx, o.ID, jan); err != nil || n != 2 {
		t.Fatalf("IncUsage (2nd): n=%d err=%v, want 2", n, err)
	}
	// Другой день того же месяца бьёт в тот же счётчик (period_month
	// нормализуется к 1-му числу).
	janLater := time.Date(2026, time.January, 28, 3, 0, 0, 0, time.UTC)
	if n, err := svc.IncUsage(ctx, o.ID, janLater); err != nil || n != 3 {
		t.Fatalf("IncUsage (same month, later day): n=%d err=%v, want 3", n, err)
	}
	if n, err := svc.Usage(ctx, o.ID, jan); err != nil || n != 3 {
		t.Fatalf("Usage (january): n=%d err=%v, want 3", n, err)
	}

	// Другой месяц независим.
	feb := time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)
	if n, err := svc.Usage(ctx, o.ID, feb); err != nil || n != 0 {
		t.Fatalf("Usage (february, before inc): n=%d err=%v, want 0", n, err)
	}
	if n, err := svc.IncUsage(ctx, o.ID, feb); err != nil || n != 1 {
		t.Fatalf("IncUsage (february): n=%d err=%v, want 1", n, err)
	}
	if n, err := svc.Usage(ctx, o.ID, jan); err != nil || n != 3 {
		t.Fatalf("Usage (january, unaffected by february): n=%d err=%v, want 3", n, err)
	}

	if err := svc.SetQuota(ctx, o.ID, 42); err != nil {
		t.Fatalf("SetQuota: %v", err)
	}
	got, err := svc.Get(ctx, o.ID)
	if err != nil || got.EventQuota != 42 {
		t.Fatalf("Get after SetQuota: %+v err=%v, want EventQuota=42", got, err)
	}
	if err := svc.SetQuota(ctx, 999999, 1); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("SetQuota (missing org): got %v, want ErrNotFound", err)
	}
}

// TestCreateOrgQuotas — CreateOrg проставляет все 5 квот из дефолтов сервиса:
// event = defaultQuota (из NewService), tx/metric/profile/log — из SetQuotaDefaults.
// В OSS-конфиге все дефолты = 0 (безлимит), и созданная орга наследует их.
func TestCreateOrgQuotas(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 0)
	svc.SetQuotaDefaults(0, 0, 0, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "quotas-owner@example.com")
	o, err := svc.CreateOrg(ctx, "quotas-org", "Quotas Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	// Возврат CreateOrg тоже несёт все квоты.
	if o.EventQuota != 0 || o.TransactionQuota != 0 || o.MetricQuota != 0 || o.ProfileQuota != 0 || o.LogQuota != 0 {
		t.Fatalf("CreateOrg returned %+v, want all quotas 0", o)
	}

	got, err := svc.Get(ctx, o.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.EventQuota != 0 {
		t.Errorf("EventQuota = %d, want 0", got.EventQuota)
	}
	if got.TransactionQuota != 0 {
		t.Errorf("TransactionQuota = %d, want 0", got.TransactionQuota)
	}
	if got.MetricQuota != 0 {
		t.Errorf("MetricQuota = %d, want 0", got.MetricQuota)
	}
	if got.ProfileQuota != 0 {
		t.Errorf("ProfileQuota = %d, want 0", got.ProfileQuota)
	}
	if got.LogQuota != 0 {
		t.Errorf("LogQuota = %d, want 0", got.LogQuota)
	}
}

// TestCreateOrgQuotasFromConfig — все 5 дефолтов берутся из конфига независимо:
// event из NewService, остальные из SetQuotaDefaults.
func TestCreateOrgQuotasFromConfig(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 500)
	svc.SetQuotaDefaults(100, 200, 300, 400)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "quotas-cfg-owner@example.com")
	o, err := svc.CreateOrg(ctx, "quotas-cfg-org", "Quotas Cfg Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	got, err := svc.Get(ctx, o.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.EventQuota != 500 || got.TransactionQuota != 100 || got.MetricQuota != 200 || got.ProfileQuota != 300 || got.LogQuota != 400 {
		t.Fatalf("Get = %+v, want event=500 tx=100 metric=200 profile=300 log=400", got)
	}
}

func TestSetQuotaNegative(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "quota-owner@example.com")
	o, err := svc.CreateOrg(ctx, "quota-org", "Quota Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	// Initial quota is default (1_000_000).
	initialOrg, err := svc.Get(ctx, o.ID)
	if err != nil || initialOrg.EventQuota != 1_000_000 {
		t.Fatalf("Get initial: %+v err=%v", initialOrg, err)
	}

	// SetQuota(-5) should reject and leave quota unchanged.
	if err := svc.SetQuota(ctx, o.ID, -5); !errors.Is(err, org.ErrInvalidQuota) {
		t.Fatalf("SetQuota(-5): got %v, want ErrInvalidQuota", err)
	}
	afterReject, err := svc.Get(ctx, o.ID)
	if err != nil || afterReject.EventQuota != 1_000_000 {
		t.Fatalf("Get after SetQuota(-5): %+v err=%v, want quota unchanged", afterReject, err)
	}

	// SetQuota(0) should succeed (0 = unlimited).
	if err := svc.SetQuota(ctx, o.ID, 0); err != nil {
		t.Fatalf("SetQuota(0): %v", err)
	}
	afterZero, err := svc.Get(ctx, o.ID)
	if err != nil || afterZero.EventQuota != 0 {
		t.Fatalf("Get after SetQuota(0): %+v err=%v, want quota=0", afterZero, err)
	}
}

// TestSetQuotas — P2-8 (+ волна 3 задача H): SetQuotas применяет любое
// подмножество из пяти квот (событий/транзакций/метрик/профилей/логов)
// одним UPDATE. Непереданные (nil) поля не трогает — COALESCE сохраняет
// прежнее значение, а не сбрасывает его в 0/NULL, — а на невалидном значении
// не применяет ничего (в отличие от цикла отдельных Set*Quota, который мог
// оставить квоты частично изменёнными на сбое посреди цикла).
func TestSetQuotas(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "quotas-owner@example.com")
	o, err := svc.CreateOrg(ctx, "quotas-org", "Quotas Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	// Меняем три поля из пяти (включая log) — остальные два должны остаться
	// дефолтными (event=1_000_000 задан CreateOrg, tx/profile=0).
	event, metric, log := int64(700), int64(200), int64(300)
	if err := svc.SetQuotas(ctx, o.ID, &event, nil, &metric, nil, &log); err != nil {
		t.Fatalf("SetQuotas(event, nil, metric, nil, log): %v", err)
	}
	got, err := svc.Get(ctx, o.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.EventQuota != 700 || got.MetricQuota != 200 || got.LogQuota != 300 || got.TransactionQuota != 0 || got.ProfileQuota != 0 {
		t.Fatalf("Get after partial SetQuotas = %+v, want event=700 metric=200 log=300 tx=0(unchanged) profile=0(unchanged)", got)
	}

	// Ещё один частичный вызов, log_quota в нём nil: уже заданное значение
	// (300) обязано сохраниться через COALESCE, а не сброситься нулём —
	// та же проверка, что выше для tx/profile, но теперь на поле, которое
	// уже было ненулевым перед вызовом (иначе "не тронуто" и "сброшено в 0"
	// неотличимы).
	profile := int64(150)
	if err := svc.SetQuotas(ctx, o.ID, nil, nil, nil, &profile, nil); err != nil {
		t.Fatalf("SetQuotas(nil, nil, nil, profile, nil): %v", err)
	}
	afterNilLog, err := svc.Get(ctx, o.ID)
	if err != nil {
		t.Fatalf("Get after nil-log SetQuotas: %v", err)
	}
	if afterNilLog.LogQuota != 300 {
		t.Fatalf("LogQuota after nil-log SetQuotas = %d, want 300 (COALESCE must keep prior value)", afterNilLog.LogQuota)
	}
	if afterNilLog.ProfileQuota != 150 {
		t.Fatalf("ProfileQuota after SetQuotas = %d, want 150", afterNilLog.ProfileQuota)
	}

	// Невалидное значение (отрицательное) в одном из пяти полей → ничего не
	// применяется, даже прочие валидные поля того же вызова — включая log.
	badTx := int64(-1)
	newEvent := int64(999)
	newLog := int64(9999)
	if err := svc.SetQuotas(ctx, o.ID, &newEvent, &badTx, nil, nil, &newLog); !errors.Is(err, org.ErrInvalidQuota) {
		t.Fatalf("SetQuotas with negative tx: got %v, want ErrInvalidQuota", err)
	}
	afterReject, err := svc.Get(ctx, o.ID)
	if err != nil {
		t.Fatalf("Get after rejected SetQuotas: %v", err)
	}
	if afterReject.EventQuota != 700 || afterReject.LogQuota != 300 {
		t.Fatalf("Get after rejected SetQuotas = %+v, want event still 700 and log still 300 (nothing applied)", afterReject)
	}

	// Неизвестная организация → ErrNotFound.
	if err := svc.SetQuotas(ctx, 999999, &event, nil, nil, nil, nil); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("SetQuotas(unknown org): got %v, want ErrNotFound", err)
	}
}

func TestLastOwnerRace(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for round := 0; round < 20; round++ {
		a := newUser(t, pool, fmt.Sprintf("race-a-%d@example.com", round))
		b := newUser(t, pool, fmt.Sprintf("race-b-%d@example.com", round))
		o, err := svc.CreateOrg(ctx, fmt.Sprintf("race-%d", round), "Race", a)
		if err != nil {
			t.Fatalf("CreateOrg: %v", err)
		}
		if err := svc.AddMember(ctx, o.ID, b, org.RoleOwner); err != nil {
			t.Fatalf("AddMember: %v", err)
		}

		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, uid := range []int64{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = svc.SetRole(ctx, o.ID, uid, org.RoleMember)
			}()
		}
		wg.Wait()

		var owners int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM org_members WHERE org_id = $1 AND role = 'owner'", o.ID).Scan(&owners); err != nil {
			t.Fatalf("count owners: %v", err)
		}
		if owners < 1 {
			t.Fatalf("round %d: org left with zero owners (errs: %v, %v)", round, errs[0], errs[1])
		}
	}
}

// TestSetRoleAsOwnerOnly — security fix (задача 5/1, TOCTOU в owner-guard):
// SetRoleAs/RemoveMemberAs проверяют актёра и цель в той же транзакции, что и
// саму мутацию. admin не может ни выдать owner, ни тронуть существующего
// owner'а (ErrOwnerOnly), а owner может и то, и другое; last-owner защита
// по-прежнему работает через *As-варианты.
func TestSetRoleAsOwnerOnly(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner := newUser(t, pool, "asowner@example.com")
	o, err := svc.CreateOrg(ctx, "as-org", "As Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	admin := newUser(t, pool, "asadmin@example.com")
	if err := svc.AddMember(ctx, o.ID, admin, org.RoleAdmin); err != nil {
		t.Fatalf("AddMember admin: %v", err)
	}
	member := newUser(t, pool, "asmember@example.com")
	if err := svc.AddMember(ctx, o.ID, member, org.RoleMember); err != nil {
		t.Fatalf("AddMember member: %v", err)
	}

	// admin → member получает owner: ErrOwnerOnly, роль не меняется.
	if err := svc.SetRoleAs(ctx, o.ID, admin, member, org.RoleOwner); !errors.Is(err, org.ErrOwnerOnly) {
		t.Fatalf("admin promotes member to owner: got %v, want ErrOwnerOnly", err)
	}
	if r, err := svc.Role(ctx, o.ID, member); err != nil || r != org.RoleMember {
		t.Fatalf("member role after blocked promotion = %v, %v, want member, nil", r, err)
	}

	// admin понижает owner'а: ErrOwnerOnly, роль не меняется.
	if err := svc.SetRoleAs(ctx, o.ID, admin, owner, org.RoleAdmin); !errors.Is(err, org.ErrOwnerOnly) {
		t.Fatalf("admin demotes owner: got %v, want ErrOwnerOnly", err)
	}
	if r, err := svc.Role(ctx, o.ID, owner); err != nil || r != org.RoleOwner {
		t.Fatalf("owner role after blocked demotion = %v, %v, want owner, nil", r, err)
	}

	// admin удаляет owner'а: ErrOwnerOnly, участник остаётся.
	if err := svc.RemoveMemberAs(ctx, o.ID, admin, owner); !errors.Is(err, org.ErrOwnerOnly) {
		t.Fatalf("admin removes owner: got %v, want ErrOwnerOnly", err)
	}
	if r, err := svc.Role(ctx, o.ID, owner); err != nil || r != org.RoleOwner {
		t.Fatalf("owner role after blocked removal = %v, %v, want owner, nil", r, err)
	}

	// admin по-прежнему управляет member/admin-уровнем без ограничений.
	if err := svc.SetRoleAs(ctx, o.ID, admin, member, org.RoleAdmin); err != nil {
		t.Fatalf("admin promotes member to admin: %v", err)
	}
	if r, err := svc.Role(ctx, o.ID, member); err != nil || r != org.RoleAdmin {
		t.Fatalf("member role after admin->admin promotion = %v, %v, want admin, nil", r, err)
	}

	// owner может и выдать owner, и понизить owner'а (второй owner в наличии
	// после промоции — last-owner защита не срабатывает).
	if err := svc.SetRoleAs(ctx, o.ID, owner, member, org.RoleOwner); err != nil {
		t.Fatalf("owner promotes member to owner: %v", err)
	}
	if r, err := svc.Role(ctx, o.ID, member); err != nil || r != org.RoleOwner {
		t.Fatalf("member role after owner promotion = %v, %v, want owner, nil", r, err)
	}
	if err := svc.SetRoleAs(ctx, o.ID, owner, member, org.RoleAdmin); err != nil {
		t.Fatalf("owner demotes member back: %v", err)
	}
	if r, err := svc.Role(ctx, o.ID, member); err != nil || r != org.RoleAdmin {
		t.Fatalf("member role after owner demotion = %v, %v, want admin, nil", r, err)
	}

	// last-owner защита всё ещё работает через *As-варианты: единственный
	// owner не может ни понизить, ни удалить сам себя.
	if err := svc.SetRoleAs(ctx, o.ID, owner, owner, org.RoleAdmin); !errors.Is(err, org.ErrLastOwner) {
		t.Fatalf("last owner demotes self via SetRoleAs: got %v, want ErrLastOwner", err)
	}
	if err := svc.RemoveMemberAs(ctx, o.ID, owner, owner); !errors.Is(err, org.ErrLastOwner) {
		t.Fatalf("last owner removes self via RemoveMemberAs: got %v, want ErrLastOwner", err)
	}
}
