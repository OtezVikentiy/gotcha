package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestChangePasswordUnknownUser(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := svc.ChangePassword(ctx, 987654321, "whatever12", "new-password-1"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("ChangePassword(несуществующий user) = %v, want ErrInvalidCredentials", err)
	}
}

func TestChangePasswordOAuthOnlyUser(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var uid int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email) VALUES ('cp-oauth@example.com') RETURNING id").Scan(&uid); err != nil {
		t.Fatalf("insert oauth-only user: %v", err)
	}

	err := changePasswordRecovering(t, svc, ctx, uid, "anything12", "new-password-1")
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("ChangePassword(oauth-only user) = %v, want ErrInvalidCredentials", err)
	}
}

// Перехватывает панику через recover, превращая её в обычный t.Fatalf.
func changePasswordRecovering(t *testing.T, svc *auth.Service, ctx context.Context, userID int64, oldPassword, newPassword string) error {
	t.Helper()
	type result struct {
		err      error
		panicVal any
	}
	done := make(chan result, 1)
	go func() {
		var r result
		defer func() {
			r.panicVal = recover()
			done <- r
		}()
		r.err = svc.ChangePassword(ctx, userID, oldPassword, newPassword)
	}()
	select {
	case r := <-done:
		if r.panicVal != nil {
			t.Fatalf("ChangePassword запаниковал: %v", r.panicVal)
		}
		return r.err
	case <-time.After(5 * time.Second):
		t.Fatal("ChangePassword не вернулся за 5с")
		return nil
	}
}

func TestChangePasswordMalformedStoredHash(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uid, err := svc.Register(ctx, "cp-malformed@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE users SET password_hash = 'not-a-valid-phc-string' WHERE id = $1", uid); err != nil {
		t.Fatalf("corrupt hash: %v", err)
	}

	err = svc.ChangePassword(ctx, uid, "hunter2hunter2", "new-password-1")
	if err == nil || errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("ChangePassword(повреждённый хеш) = %v, want обёрнутую ошибку VerifyPassword, а не nil/ErrInvalidCredentials", err)
	}
	if !strings.Contains(err.Error(), "auth: change password") {
		t.Fatalf("ChangePassword(повреждённый хеш): err = %v, want содержащую %q", err, "auth: change password")
	}
}
