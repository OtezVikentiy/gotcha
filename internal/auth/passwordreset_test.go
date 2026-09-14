package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestPasswordResetTTLIsOneHour(t *testing.T) {
	if auth.PasswordResetTTL != time.Hour {
		t.Errorf("PasswordResetTTL = %v, want 1h", auth.PasswordResetTTL)
	}
}

func TestRequestPasswordReset_UnknownEmailRespondsNotFound(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	token, found, err := svc.RequestPasswordReset(ctx, "nosuchuser@example.com")
	if err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	if found {
		t.Error("found = true for a nonexistent email, want false")
	}
	if token != "" {
		t.Errorf("token = %q for a nonexistent email, want empty", token)
	}
}

func TestRequestPasswordReset_ExistingEmailIssuesValidToken(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.Register(ctx, "reset1@example.com", "old-password-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	token, found, err := svc.RequestPasswordReset(ctx, "reset1@example.com")
	if err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	if !found {
		t.Fatal("found = false for a registered email, want true")
	}
	if token == "" {
		t.Fatal("token is empty for a registered email")
	}
	if !svc.ValidPasswordResetToken(ctx, token) {
		t.Error("ValidPasswordResetToken = false right after issuing, want true")
	}
	if svc.ValidPasswordResetToken(ctx, "not-the-token-at-all") {
		t.Error("ValidPasswordResetToken = true for an unrelated string, want false")
	}
}

// Второй запрос гасит ссылку из первого письма немедленно — иначе обе ссылки остаются
// рабочими одновременно, расширяя окно атаки без всякой пользы.
func TestRequestPasswordReset_SecondRequestInvalidatesFirst(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.Register(ctx, "reset2@example.com", "old-password-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	first, _, err := svc.RequestPasswordReset(ctx, "reset2@example.com")
	if err != nil {
		t.Fatalf("RequestPasswordReset (1st): %v", err)
	}
	second, _, err := svc.RequestPasswordReset(ctx, "reset2@example.com")
	if err != nil {
		t.Fatalf("RequestPasswordReset (2nd): %v", err)
	}
	if svc.ValidPasswordResetToken(ctx, first) {
		t.Error("first token still valid after a second request, want invalidated")
	}
	if !svc.ValidPasswordResetToken(ctx, second) {
		t.Error("second token invalid right after issuing, want valid")
	}
}

func TestResetPassword_UnknownTokenRejected(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := svc.ResetPassword(ctx, "no-such-token", "new-password-1"); !errors.Is(err, auth.ErrResetTokenInvalid) {
		t.Fatalf("ResetPassword(unknown token) = %v, want ErrResetTokenInvalid", err)
	}
}

func TestResetPassword_OneTimeUse(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.Register(ctx, "onetime@example.com", "old-password-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	token, _, err := svc.RequestPasswordReset(ctx, "onetime@example.com")
	if err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	if err := svc.ResetPassword(ctx, token, "new-password-1"); err != nil {
		t.Fatalf("ResetPassword (1st use): %v", err)
	}
	if err := svc.ResetPassword(ctx, token, "another-password-1"); !errors.Is(err, auth.ErrResetTokenInvalid) {
		t.Fatalf("ResetPassword (2nd use) = %v, want ErrResetTokenInvalid", err)
	}
	// Пароль от первого успешного использования обязан остаться в силе.
	if _, err := svc.Authenticate(ctx, "onetime@example.com", "new-password-1"); err != nil {
		t.Fatalf("Authenticate with the password set by the 1st use: %v", err)
	}
}

func TestResetPassword_ExpiredTokenRejected(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.Register(ctx, "expired@example.com", "old-password-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	token, _, err := svc.RequestPasswordReset(ctx, "expired@example.com")
	if err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE password_resets SET expires_at = now() - interval '1 minute'"); err != nil {
		t.Fatalf("expire token: %v", err)
	}
	if svc.ValidPasswordResetToken(ctx, token) {
		t.Error("ValidPasswordResetToken = true for an expired token, want false")
	}
	if err := svc.ResetPassword(ctx, token, "new-password-1"); !errors.Is(err, auth.ErrResetTokenInvalid) {
		t.Fatalf("ResetPassword(expired token) = %v, want ErrResetTokenInvalid", err)
	}
}

// Слабый новый пароль обязан провалиться ДО сжигания токена — иначе опечатка в форме
// требовала бы новой ссылки из письма для повторной попытки.
func TestResetPassword_WeakPasswordDoesNotConsumeToken(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.Register(ctx, "weak@example.com", "old-password-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	token, _, err := svc.RequestPasswordReset(ctx, "weak@example.com")
	if err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	if err := svc.ResetPassword(ctx, token, "short"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("ResetPassword(weak password) = %v, want ErrWeakPassword", err)
	}
	if !svc.ValidPasswordResetToken(ctx, token) {
		t.Error("token invalidated by a rejected weak password, want still valid")
	}
	if err := svc.ResetPassword(ctx, token, "good-password-1"); err != nil {
		t.Fatalf("ResetPassword after the weak attempt: %v", err)
	}
}

// Требование безопасности: успешный сброс убивает ВСЕ существующие сессии пользователя.
func TestResetPassword_KillsAllSessions(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uid, err := svc.Register(ctx, "killsessions@example.com", "old-password-1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok1, err := svc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatalf("CreateSession 1: %v", err)
	}
	tok2, err := svc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatalf("CreateSession 2: %v", err)
	}

	resetToken, _, err := svc.RequestPasswordReset(ctx, "killsessions@example.com")
	if err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	if err := svc.ResetPassword(ctx, resetToken, "new-password-1"); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}

	if _, err := svc.SessionUser(ctx, tok1); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("session 1 survived ResetPassword: err=%v, want ErrNoSession", err)
	}
	if _, err := svc.SessionUser(ctx, tok2); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("session 2 survived ResetPassword: err=%v, want ErrNoSession", err)
	}
	if _, err := svc.Authenticate(ctx, "killsessions@example.com", "old-password-1"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("old password still authenticates after reset: %v", err)
	}
}

// OAuth-only аккаунт (password_hash IS NULL) не отличается для восстановления по почте:
// ссылка подтверждает владение адресом, того же уровня доверия, что и OAuth-провайдер.
func TestResetPassword_WorksWhenNoPasswordExistedYet(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var uid int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email) VALUES ('oauthonly@example.com') RETURNING id").Scan(&uid); err != nil {
		t.Fatalf("insert oauth-only user: %v", err)
	}
	token, found, err := svc.RequestPasswordReset(ctx, "oauthonly@example.com")
	if err != nil || !found {
		t.Fatalf("RequestPasswordReset = (%q,%v,%v)", token, found, err)
	}
	if err := svc.ResetPassword(ctx, token, "brand-new-password-1"); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	if _, err := svc.Authenticate(ctx, "oauthonly@example.com", "brand-new-password-1"); err != nil {
		t.Fatalf("Authenticate after reset on a formerly passwordless account: %v", err)
	}
}

func TestPurgeExpiredPasswordResets(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.Register(ctx, "purge@example.com", "old-password-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, _, err := svc.RequestPasswordReset(ctx, "purge@example.com"); err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE password_resets SET expires_at = now() - interval '1 minute'"); err != nil {
		t.Fatalf("expire token: %v", err)
	}

	n, err := svc.PurgeExpiredPasswordResets(ctx)
	if err != nil {
		t.Fatalf("PurgeExpiredPasswordResets: %v", err)
	}
	if n != 1 {
		t.Errorf("purged = %d, want 1", n)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM password_resets").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("password_resets rows left = %d, want 0", count)
	}
}

func TestAdminSetPassword_UnknownEmail(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := svc.AdminSetPassword(ctx, "nosuchuser@example.com", "new-password-1"); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("AdminSetPassword(unknown email) = %v, want ErrUserNotFound", err)
	}
}

func TestAdminSetPassword_WeakPasswordRejected(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.Register(ctx, "adminweak@example.com", "old-password-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := svc.AdminSetPassword(ctx, "adminweak@example.com", "short"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("AdminSetPassword(weak) = %v, want ErrWeakPassword", err)
	}
}

// Требования подкоманды оператора: переиспользует общее хеширование (проверяется через
// успешный последующий Authenticate тем же VerifyPassword) и убивает существующие сессии.
func TestAdminSetPassword_SetsPasswordAndKillsSessions(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uid, err := svc.Register(ctx, "adminreset@example.com", "old-password-1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, err := svc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := svc.AdminSetPassword(ctx, "adminreset@example.com", "operator-set-1"); err != nil {
		t.Fatalf("AdminSetPassword: %v", err)
	}

	if _, err := svc.Authenticate(ctx, "adminreset@example.com", "old-password-1"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("old password still authenticates: %v", err)
	}
	if _, err := svc.Authenticate(ctx, "adminreset@example.com", "operator-set-1"); err != nil {
		t.Errorf("Authenticate with the operator-set password: %v", err)
	}
	if _, err := svc.SessionUser(ctx, tok); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("session survived AdminSetPassword: err=%v, want ErrNoSession", err)
	}
}

// AdminSetPassword не должен ослаблять SetPassword: после того как пароль установлен через
// подкоманду оператора, SetPassword (путь "хеша ещё нет") обязан по-прежнему отказывать.
func TestAdminSetPassword_DoesNotWeakenSetPassword(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var uid int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email) VALUES ('adminvsset@example.com') RETURNING id").Scan(&uid); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := svc.AdminSetPassword(ctx, "adminvsset@example.com", "operator-set-1"); err != nil {
		t.Fatalf("AdminSetPassword: %v", err)
	}
	if err := svc.SetPassword(ctx, uid, "should-not-apply-1"); !errors.Is(err, auth.ErrPasswordAlreadySet) {
		t.Fatalf("SetPassword after AdminSetPassword = %v, want ErrPasswordAlreadySet", err)
	}
	// Пароль, поставленный оператором, обязан остаться в силе.
	if _, err := svc.Authenticate(ctx, "adminvsset@example.com", "operator-set-1"); err != nil {
		t.Fatalf("Authenticate with the operator-set password: %v", err)
	}
}
