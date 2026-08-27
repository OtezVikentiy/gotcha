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

// TestUserEmailsBatchMatchesIndividualLookups — UserEmails (батч-версия
// UserEmail для страниц-списков, напр. страницы выгрузок ошибок, ревью
// веб-части E1 п.5) обязана вернуть ровно те же email, что и по одному
// UserEmail на каждый id, плюс: несуществующий id молча отсутствует в
// карте (не ошибка, тот же контракт немолчания, что и у UserEmail), а
// пустой список id не ходит в БД и отдаёт пустую карту.
func TestUserEmailsBatchMatchesIndividualLookups(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id1, err := svc.Register(ctx, "batch-one@example.com", "password12")
	if err != nil {
		t.Fatalf("Register first: %v", err)
	}
	id2, err := svc.Register(ctx, "batch-two@example.com", "password12")
	if err != nil {
		t.Fatalf("Register second: %v", err)
	}
	const missingID = int64(999_999_999)

	got, err := svc.UserEmails(ctx, []int64{id1, id2, missingID})
	if err != nil {
		t.Fatalf("UserEmails: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("UserEmails вернул %d записей, want 2 (missingID не должен попасть в карту): %v", len(got), got)
	}
	if got[id1] != "batch-one@example.com" {
		t.Errorf("UserEmails[id1] = %q, want batch-one@example.com", got[id1])
	}
	if got[id2] != "batch-two@example.com" {
		t.Errorf("UserEmails[id2] = %q, want batch-two@example.com", got[id2])
	}
	if _, ok := got[missingID]; ok {
		t.Error("UserEmails содержит запись для несуществующего id")
	}

	empty, err := svc.UserEmails(ctx, nil)
	if err != nil {
		t.Fatalf("UserEmails(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("UserEmails(nil) = %v, want пустую карту", empty)
	}
}
