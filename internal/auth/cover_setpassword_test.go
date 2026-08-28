package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestHasPasswordUnknownUser — несуществующий userID: SELECT возвращает
// pgx.ErrNoRows, HasPassword обязан отдать ErrInvalidCredentials, а не
// голую ошибку БД.
func TestHasPasswordUnknownUser(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.HasPassword(ctx, 987654321); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("HasPassword(несуществующий user) = %v, want ErrInvalidCredentials", err)
	}
}

// TestSetPasswordUnknownUser — условный UPDATE (WHERE id=$1 AND password_hash
// IS NULL) для несуществующего userID даёт RowsAffected==0; SetPassword
// добором различает «юзера нет» от «пароль уже задан» отдельным SELECT
// exists. Для отсутствующего юзера это должно дать ErrInvalidCredentials
// (а не ошибочно ErrPasswordAlreadySet).
func TestSetPasswordUnknownUser(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := svc.SetPassword(ctx, 987654321, "brandnewpass1")
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("SetPassword(несуществующий user) = %v, want ErrInvalidCredentials", err)
	}
	if errors.Is(err, auth.ErrPasswordAlreadySet) {
		t.Fatalf("SetPassword(несуществующий user) не должен маскироваться под ErrPasswordAlreadySet: %v", err)
	}
}
