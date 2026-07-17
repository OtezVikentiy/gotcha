package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestRegister_FirstUserIsInstanceAdmin: первый Register делает пользователя
// инстанс-админом (bootstrap), последующие — обычными.
func TestRegister_FirstUserIsInstanceAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// На пустом инстансе счётчик — ноль.
	if n, err := svc.UserCount(ctx); err != nil || n != 0 {
		t.Fatalf("UserCount empty = (%d,%v), want (0,nil)", n, err)
	}

	firstID, err := svc.Register(ctx, "first@example.com", "password12")
	if err != nil {
		t.Fatalf("Register first: %v", err)
	}
	if n, err := svc.UserCount(ctx); err != nil || n != 1 {
		t.Fatalf("UserCount after first = (%d,%v), want (1,nil)", n, err)
	}
	if admin, err := svc.UserIsInstanceAdmin(ctx, firstID); err != nil || !admin {
		t.Fatalf("first user IsInstanceAdmin = (%v,%v), want (true,nil)", admin, err)
	}

	secondID, err := svc.Register(ctx, "second@example.com", "password12")
	if err != nil {
		t.Fatalf("Register second: %v", err)
	}
	if admin, err := svc.UserIsInstanceAdmin(ctx, secondID); err != nil || admin {
		t.Fatalf("second user IsInstanceAdmin = (%v,%v), want (false,nil)", admin, err)
	}
	if n, err := svc.UserCount(ctx); err != nil || n != 2 {
		t.Fatalf("UserCount after second = (%d,%v), want (2,nil)", n, err)
	}
}

// RA-L6: обычная коллизия email по-прежнему даёт ErrEmailTaken. Проверяем,
// что дизамбигуация 23505 по имени констрейнта (email vs one_instance_admin)
// не сломала штатный путь «email уже занят».
func TestRegister_DuplicateEmailStillErrEmailTaken(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.Register(ctx, "dup@example.com", "password12"); err != nil {
		t.Fatalf("Register first: %v", err)
	}
	// Повторная регистрация того же email → ErrEmailTaken (не путаное admin-сообщение).
	if _, err := svc.Register(ctx, "dup@example.com", "password34"); !errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("duplicate email err = %v, want ErrEmailTaken", err)
	}
}
