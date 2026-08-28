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

// TestChangePasswordUnknownUser — несуществующий userID: начальный SELECT
// password_hash возвращает pgx.ErrNoRows, ChangePassword обязан отдать
// ErrInvalidCredentials, а не голую ошибку БД (иначе хендлер утечёт
// внутренности через 500 вместо аккуратного отказа).
func TestChangePasswordUnknownUser(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := svc.ChangePassword(ctx, 987654321, "whatever12", "new-password-1"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("ChangePassword(несуществующий user) = %v, want ErrInvalidCredentials", err)
	}
}

// TestChangePasswordOAuthOnlyUser — у аккаунта без пароля (OAuth-only,
// password_hash IS NULL) ChangePassword неприменим: нужен SetPassword.
// Должен вернуться ErrInvalidCredentials, а не паника на разыменовании
// nil-хеша и не попытка сверить пароль с пустой строкой. Вызов ChangePassword
// обёрнут в горутину с recover (тот же приём, что и в
// TestJanitorRunDefaultsIntervalWhenZero): если защиту hash==nil вырезать,
// ChangePassword паникует на *hash, а необработанная паника убивает весь
// тестовый бинарь целиком — соседние тесты того же прогона вообще не
// отрапортуют. С обёрткой падение остаётся обычным t.Fatalf.
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

// changePasswordRecovering вызывает ChangePassword в отдельной горутине и
// перехватывает панику через recover, превращая её в обычный t.Fatalf —
// см. комментарий у TestChangePasswordOAuthOnlyUser.
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

// TestChangePasswordMalformedStoredHash — если сохранённый password_hash
// повреждён (не валидная PHC-строка argon2id — например, порча данных или
// ручное вмешательство в БД), VerifyPassword возвращает ErrMalformedHash, и
// ChangePassword обязан обернуть эту ошибку, а не притвориться, что старый
// пароль просто неверен, и не запаниковать.
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
