package auth_test

import (
	"context"
	"errors"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestIdentityRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx := context.Background()

	uidA, err := svc.Register(ctx, "a@example.com", "password12")
	if err != nil {
		t.Fatalf("register A: %v", err)
	}
	uidB, err := svc.Register(ctx, "b@example.com", "password12")
	if err != nil {
		t.Fatalf("register B: %v", err)
	}

	// Нет личности → ErrNoIdentity.
	if _, err := svc.IdentityUser(ctx, "oidc", "sub-a"); !errors.Is(err, auth.ErrNoIdentity) {
		t.Fatalf("IdentityUser missing = %v, want ErrNoIdentity", err)
	}
	// Привязали к A → IdentityUser возвращает A.
	if err := svc.LinkIdentity(ctx, uidA, "oidc", "sub-a", "a@example.com"); err != nil {
		t.Fatalf("LinkIdentity A: %v", err)
	}
	if got, err := svc.IdentityUser(ctx, "oidc", "sub-a"); err != nil || got != uidA {
		t.Fatalf("IdentityUser = (%d,%v), want (%d,nil)", got, err, uidA)
	}
	// Тот же субъект к B → ErrIdentityTaken.
	if err := svc.LinkIdentity(ctx, uidB, "oidc", "sub-a", "b@example.com"); !errors.Is(err, auth.ErrIdentityTaken) {
		t.Fatalf("LinkIdentity taken = %v, want ErrIdentityTaken", err)
	}
	// Второй oidc к тому же A → ErrAlreadyLinked.
	if err := svc.LinkIdentity(ctx, uidA, "oidc", "sub-a2", "a@example.com"); !errors.Is(err, auth.ErrAlreadyLinked) {
		t.Fatalf("LinkIdentity already = %v, want ErrAlreadyLinked", err)
	}
	// UpdateIdentityEmail меняет сохранённый email.
	if err := svc.UpdateIdentityEmail(ctx, "oidc", "sub-a", "a-new@example.com"); err != nil {
		t.Fatalf("UpdateIdentityEmail: %v", err)
	}
	ids, err := svc.ListIdentities(ctx, uidA)
	if err != nil || len(ids) != 1 || ids[0].Email != "a-new@example.com" {
		t.Fatalf("ListIdentities = (%+v,%v)", ids, err)
	}
	// UserByEmail.
	if got, err := svc.UserByEmail(ctx, "A@Example.com"); err != nil || got != uidA {
		t.Fatalf("UserByEmail = (%d,%v), want (%d,nil)", got, err, uidA)
	}
	if _, err := svc.UserByEmail(ctx, "nobody@example.com"); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("UserByEmail missing = %v, want ErrUserNotFound", err)
	}
	// CreateOAuthUser + повторный email → ErrEmailTaken.
	uidC, err := svc.CreateOAuthUser(ctx, "c@example.com")
	if err != nil {
		t.Fatalf("CreateOAuthUser: %v", err)
	}
	if _, err := svc.CreateOAuthUser(ctx, "c@example.com"); !errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("CreateOAuthUser dup = %v, want ErrEmailTaken", err)
	}
	// Unlink.
	if err := svc.UnlinkIdentity(ctx, uidC, "oidc"); !errors.Is(err, auth.ErrNoIdentity) {
		t.Fatalf("UnlinkIdentity none = %v, want ErrNoIdentity", err)
	}
	if err := svc.UnlinkIdentity(ctx, uidA, "oidc"); err != nil {
		t.Fatalf("UnlinkIdentity A: %v", err)
	}
	if ids, _ := svc.ListIdentities(ctx, uidA); len(ids) != 0 {
		t.Fatalf("ListIdentities after unlink = %+v, want empty", ids)
	}
}

func TestDeleteUser(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx := context.Background()
	uid, err := svc.CreateOAuthUser(ctx, "del@example.com")
	if err != nil {
		t.Fatalf("CreateOAuthUser: %v", err)
	}
	if err := svc.DeleteUser(ctx, uid); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := svc.UserByEmail(ctx, "del@example.com"); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("user still exists after delete")
	}
}
