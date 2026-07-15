package auth_test

import (
	"context"
	"errors"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestSetPasswordOnOAuthOnlyUser(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx := context.Background()

	var uid int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email) VALUES ('sp@example.com') RETURNING id").Scan(&uid); err != nil {
		t.Fatalf("insert: %v", err)
	}

	has, err := svc.HasPassword(ctx, uid)
	if err != nil || has {
		t.Fatalf("HasPassword before = (%v,%v), want (false,nil)", has, err)
	}
	// Паролем войти нельзя, пока не задан.
	if _, err := svc.Authenticate(ctx, "sp@example.com", "whatever12"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("Authenticate NULL hash = %v, want ErrInvalidCredentials", err)
	}
	// Слишком короткий пароль — ErrWeakPassword.
	if err := svc.SetPassword(ctx, uid, "short"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("SetPassword weak = %v, want ErrWeakPassword", err)
	}
	// Валидный пароль устанавливается, после чего логин проходит.
	if err := svc.SetPassword(ctx, uid, "goodpassword12"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	has, err = svc.HasPassword(ctx, uid)
	if err != nil || !has {
		t.Fatalf("HasPassword after = (%v,%v), want (true,nil)", has, err)
	}
	if _, err := svc.Authenticate(ctx, "sp@example.com", "goodpassword12"); err != nil {
		t.Fatalf("Authenticate after SetPassword: %v", err)
	}
	// Повторный SetPassword запрещён.
	if err := svc.SetPassword(ctx, uid, "anotherpass12"); !errors.Is(err, auth.ErrPasswordAlreadySet) {
		t.Fatalf("SetPassword twice = %v, want ErrPasswordAlreadySet", err)
	}
}
