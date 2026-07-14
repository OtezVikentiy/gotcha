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

func TestRegisterAuthenticateSession(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uid, err := svc.Register(ctx, "dev@example.com", "hunter2hunter2")
	if err != nil || uid == 0 {
		t.Fatalf("Register: uid=%d err=%v", uid, err)
	}

	if _, err := svc.Register(ctx, "DEV@example.com", "otherpassword"); !errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("duplicate email (case-insensitive): got %v, want ErrEmailTaken", err)
	}
	if _, err := svc.Register(ctx, "short@example.com", "1234567"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("weak password: got %v, want ErrWeakPassword", err)
	}

	got, err := svc.Authenticate(ctx, "dev@example.com", "hunter2hunter2")
	if err != nil || got != uid {
		t.Fatalf("Authenticate: got=%d err=%v", got, err)
	}
	if _, err := svc.Authenticate(ctx, "dev@example.com", "wrong"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("wrong password: got %v, want ErrInvalidCredentials", err)
	}
	if _, err := svc.Authenticate(ctx, "ghost@example.com", "whatever"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("unknown email: got %v, want ErrInvalidCredentials (no user enumeration)", err)
	}

	token, err := svc.CreateSession(ctx, uid)
	if err != nil || token == "" {
		t.Fatalf("CreateSession: %v", err)
	}
	if got, err := svc.SessionUser(ctx, token); err != nil || got != uid {
		t.Fatalf("SessionUser: got=%d err=%v", got, err)
	}
	if _, err := svc.SessionUser(ctx, "forged-token"); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("forged token: got %v, want ErrNoSession", err)
	}
	if err := svc.DestroySession(ctx, token); err != nil {
		t.Fatalf("DestroySession: %v", err)
	}
	if _, err := svc.SessionUser(ctx, token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("destroyed session still valid: %v", err)
	}
}

func TestRegisterInvalidEmail(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, email := range []string{
		"no-at-sign",
		"",
		strings.Repeat("a", 251) + "@b.c", // 255 байт целиком, >254
	} {
		if _, err := svc.Register(ctx, email, "hunter2hunter2"); !errors.Is(err, auth.ErrInvalidEmail) {
			t.Errorf("Register(%q, ...): got %v, want ErrInvalidEmail", email, err)
		}
	}
}

func TestRegisterPasswordTooLong(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.Register(ctx, "toolong@example.com", strings.Repeat("a", 513)); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("513-byte password: got %v, want ErrWeakPassword", err)
	}
}

func TestChangePassword(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uid, err := svc.Register(ctx, "changepw@example.com", "old-password-1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	token, err := svc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Неверный старый пароль → ErrInvalidCredentials, ничего не меняется.
	if err := svc.ChangePassword(ctx, uid, "wrong-old-password", "new-password-1"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("ChangePassword (wrong old): got %v, want ErrInvalidCredentials", err)
	}
	if _, err := svc.SessionUser(ctx, token); err != nil {
		t.Fatalf("session should survive failed ChangePassword: %v", err)
	}

	// Слабый новый пароль → ErrWeakPassword, ничего не меняется.
	if err := svc.ChangePassword(ctx, uid, "old-password-1", "short"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("ChangePassword (weak new): got %v, want ErrWeakPassword", err)
	}
	if _, err := svc.SessionUser(ctx, token); err != nil {
		t.Fatalf("session should survive rejected ChangePassword: %v", err)
	}

	// Успешная смена пароля: старый хеш больше не подходит, новый — работает,
	// все сессии юзера (включая текущую) уничтожены.
	if err := svc.ChangePassword(ctx, uid, "old-password-1", "new-password-1"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if _, err := svc.Authenticate(ctx, "changepw@example.com", "old-password-1"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("old password still works: %v", err)
	}
	if got, err := svc.Authenticate(ctx, "changepw@example.com", "new-password-1"); err != nil || got != uid {
		t.Fatalf("new password auth: got=%d err=%v", got, err)
	}
	if _, err := svc.SessionUser(ctx, token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("session should be destroyed after ChangePassword: %v", err)
	}
}

func TestDestroyOtherSessions(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uid, err := svc.Register(ctx, "destroyothers@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	keep, err := svc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatalf("CreateSession (keep): %v", err)
	}
	other1, err := svc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatalf("CreateSession (other1): %v", err)
	}
	other2, err := svc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatalf("CreateSession (other2): %v", err)
	}

	n, err := svc.DestroyOtherSessions(ctx, uid, keep)
	if err != nil {
		t.Fatalf("DestroyOtherSessions: %v", err)
	}
	if n != 2 {
		t.Fatalf("DestroyOtherSessions count = %d, want 2", n)
	}
	if _, err := svc.SessionUser(ctx, keep); err != nil {
		t.Fatalf("kept session destroyed: %v", err)
	}
	if _, err := svc.SessionUser(ctx, other1); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("other1 session survived: %v", err)
	}
	if _, err := svc.SessionUser(ctx, other2); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("other2 session survived: %v", err)
	}
}

func TestExpiredSession(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uid, err := svc.Register(ctx, "exp@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	token, err := svc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Просрочиваем напрямую в БД — быстрее, чем ждать 30 дней.
	if _, err := pool.Exec(ctx, "UPDATE sessions SET expires_at = now() - interval '1 minute'"); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if _, err := svc.SessionUser(ctx, token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("expired session accepted: %v", err)
	}
	n, err := svc.DeleteExpiredSessions(ctx)
	if err != nil || n != 1 {
		t.Fatalf("DeleteExpiredSessions: n=%d err=%v", n, err)
	}
}
