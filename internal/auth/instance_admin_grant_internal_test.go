package auth

import (
	"context"
	"errors"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestGrantInstanceAdminUnknownUser — хвост 1 волны 1: grant в передаче роли
// администратора инстанса обязан проверять RowsAffected. Получатель найден
// по email до транзакции; если его строка исчезла к моменту UPDATE (удалил
// аккаунт), UPDATE молча трогал 0 строк, commit проходил — и инстанс
// оставался вовсе без администратора. Теперь — ErrUserNotFound, транзакция
// откатывается вызывающим.
func TestGrantInstanceAdminUnknownUser(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	if err := grantInstanceAdmin(ctx, tx, -1); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("grantInstanceAdmin(unknown id) err = %v, want ErrUserNotFound", err)
	}
}

// TestGrantInstanceAdminExistingUser — положительная ветка того же хелпера:
// существующему пользователю флаг ставится, ошибки нет.
func TestGrantInstanceAdminExistingUser(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	svc := NewService(pool)
	uid, err := svc.Register(ctx, "grant-existing@example.com", "password-123")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	if err := grantInstanceAdmin(ctx, tx, uid); err != nil {
		t.Fatalf("grantInstanceAdmin(existing) err = %v, want nil", err)
	}
	var admin bool
	if err := tx.QueryRow(ctx, "SELECT is_instance_admin FROM users WHERE id = $1", uid).Scan(&admin); err != nil {
		t.Fatalf("select flag: %v", err)
	}
	if !admin {
		t.Fatal("is_instance_admin = false after grant, want true")
	}
}
