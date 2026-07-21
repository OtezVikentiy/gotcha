package org_test

import (
	"context"
	"errors"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestRemoveMemberSuccessAndNotMember покрывает happy-path RemoveMember (успешно
// удаляем не-последнего участника) и ветку ErrNotMember (DELETE 0 строк для
// того, кто участником не был). Ветка ErrLastOwner уже покрыта в org_test.go.
func TestRemoveMemberSuccessAndNotMember(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()

	owner := newUser(t, pool, "rm-owner@example.com")
	o, err := svc.CreateOrg(ctx, "rm-org", "RM Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	dev := newUser(t, pool, "rm-dev@example.com")
	if err := svc.AddMember(ctx, o.ID, dev, org.RoleMember); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	// Успешное удаление обычного участника (owner остаётся — не last-owner).
	if err := svc.RemoveMember(ctx, o.ID, dev); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if _, err := svc.Role(ctx, o.ID, dev); !errors.Is(err, org.ErrNotMember) {
		t.Fatalf("Role after remove: got %v, want ErrNotMember", err)
	}

	// Удаление того, кто вообще не участник: ensureNotLastOwner пропускает
	// (не owner), DELETE затрагивает 0 строк → ErrNotMember.
	stranger := newUser(t, pool, "rm-stranger@example.com")
	if err := svc.RemoveMember(ctx, o.ID, stranger); !errors.Is(err, org.ErrNotMember) {
		t.Fatalf("RemoveMember(non-member): got %v, want ErrNotMember", err)
	}
}

// TestRemoveMemberCancelledCtx: отменённый ctx роняет pool.Begin — покрывает
// самую раннюю ветку ошибки RemoveMember.
func TestRemoveMemberCancelledCtx(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.RemoveMember(ctx, 1, 1); err == nil {
		t.Fatal("RemoveMember on cancelled ctx: got nil, want error")
	}
}

// TestRemoveMemberAsSuccessAndGuards покрывает happy-path RemoveMemberAs (admin
// удаляет member) и обе ErrNotMember-ветки owner-guard'а: актёр не участник и
// цель не участник. ErrOwnerOnly/ErrLastOwner уже покрыты в org_test.go.
func TestRemoveMemberAsSuccessAndGuards(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()

	owner := newUser(t, pool, "rma-owner@example.com")
	o, err := svc.CreateOrg(ctx, "rma-org", "RMA Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	admin := newUser(t, pool, "rma-admin@example.com")
	if err := svc.AddMember(ctx, o.ID, admin, org.RoleAdmin); err != nil {
		t.Fatalf("AddMember admin: %v", err)
	}
	member := newUser(t, pool, "rma-member@example.com")
	if err := svc.AddMember(ctx, o.ID, member, org.RoleMember); err != nil {
		t.Fatalf("AddMember member: %v", err)
	}

	// admin удаляет member — успех.
	if err := svc.RemoveMemberAs(ctx, o.ID, admin, member); err != nil {
		t.Fatalf("RemoveMemberAs(admin→member): %v", err)
	}
	if _, err := svc.Role(ctx, o.ID, member); !errors.Is(err, org.ErrNotMember) {
		t.Fatalf("Role after RemoveMemberAs: got %v, want ErrNotMember", err)
	}

	// Актёр не участник организации → ErrNotMember из owner-guard'а.
	stranger := newUser(t, pool, "rma-stranger@example.com")
	if err := svc.RemoveMemberAs(ctx, o.ID, stranger, admin); !errors.Is(err, org.ErrNotMember) {
		t.Fatalf("RemoveMemberAs(stranger actor): got %v, want ErrNotMember", err)
	}

	// Цель не участник (member уже удалён) → ErrNotMember из owner-guard'а
	// (targetRole не найден).
	if err := svc.RemoveMemberAs(ctx, o.ID, owner, member); !errors.Is(err, org.ErrNotMember) {
		t.Fatalf("RemoveMemberAs(missing target): got %v, want ErrNotMember", err)
	}
}

// TestRemoveMemberAsCancelledCtx: отменённый ctx роняет pool.Begin.
func TestRemoveMemberAsCancelledCtx(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.RemoveMemberAs(ctx, 1, 1, 2); err == nil {
		t.Fatal("RemoveMemberAs on cancelled ctx: got nil, want error")
	}
}

// TestUpdateRegressionConfig покрывает успешный UPDATE, ветку ErrNotFound
// (несуществующий проект) и ошибку на отменённом ctx.
func TestUpdateRegressionConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()

	owner := newUser(t, pool, "urc-owner@example.com")
	o, err := svc.CreateOrg(ctx, "urc-org", "URC Org", owner)
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	p, err := svc.CreateProject(ctx, o.ID, "urc-proj", "URC Proj", "go")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	const cfgJSON = `{"threshold":1.5,"min_samples":10}`
	if err := svc.UpdateRegressionConfig(ctx, p.ID, cfgJSON); err != nil {
		t.Fatalf("UpdateRegressionConfig: %v", err)
	}
	// perf_regression_config — jsonb, PG может переформатировать текст, поэтому
	// сравниваем семантически, а не байт-в-байт.
	var stored map[string]any
	if err := pool.QueryRow(ctx,
		"SELECT perf_regression_config FROM projects WHERE id = $1", p.ID).Scan(&stored); err != nil {
		t.Fatalf("read back config: %v", err)
	}
	if stored["threshold"] != 1.5 || stored["min_samples"] != float64(10) {
		t.Errorf("stored config = %+v, want threshold=1.5 min_samples=10", stored)
	}

	// Несуществующий проект → ErrNotFound (RowsAffected==0).
	if err := svc.UpdateRegressionConfig(ctx, 999999, cfgJSON); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("UpdateRegressionConfig(missing): got %v, want ErrNotFound", err)
	}

	// Отменённый ctx → ошибка Exec.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.UpdateRegressionConfig(cctx, p.ID, cfgJSON); err == nil {
		t.Fatal("UpdateRegressionConfig on cancelled ctx: got nil, want error")
	}
}

// TestSSOByDomainEdgeCases: пустой домен → not found без ошибки; отменённый ctx
// на непустом домене → ошибка запроса.
func TestSSOByDomainEdgeCases(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)

	if cfg, ok, err := svc.SSOByDomain(context.Background(), "   "); err != nil || ok {
		t.Fatalf("SSOByDomain(blank) = (%+v,%v,%v), want (_,false,nil)", cfg, ok, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := svc.SSOByDomain(ctx, "corp.example.com"); err == nil {
		t.Fatal("SSOByDomain on cancelled ctx: got nil, want error")
	}
}

// TestDeleteSSOCancelledCtx: отменённый ctx → ошибка Exec (DeleteSSO иначе
// идемпотентен и не отдаёт ошибку на отсутствующем конфиге).
func TestDeleteSSOCancelledCtx(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)

	// Идемпотентность: удаление несуществующего конфига — не ошибка.
	if err := svc.DeleteSSO(context.Background(), 999999); err != nil {
		t.Fatalf("DeleteSSO(missing): %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.DeleteSSO(ctx, 1); err == nil {
		t.Fatal("DeleteSSO on cancelled ctx: got nil, want error")
	}
}

// TestGetOrgEdgeCases: несуществующая орга → ErrNotFound; отменённый ctx → ошибка.
func TestGetOrgEdgeCases(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)

	if _, err := svc.Get(context.Background(), 999999); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("Get(missing): got %v, want ErrNotFound", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.Get(ctx, 1); err == nil {
		t.Fatal("Get on cancelled ctx: got nil, want error")
	} else if errors.Is(err, org.ErrNotFound) {
		t.Fatalf("Get on cancelled ctx = %v, want DB error not ErrNotFound", err)
	}
}
