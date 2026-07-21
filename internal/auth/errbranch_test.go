package auth_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestAuthErrorBranches — ветки обработки ошибок БД. Отменённый контекст
// заставляет pool.Exec/QueryRow вернуть ошибку, не выполняя запрос, — так
// покрываются `if err != nil { return ... }`, недостижимые на живой БД.
func TestAuthErrorBranches(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)

	ok, cancelOK := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelOK()
	uid, err := svc.Register(ok, "err@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// отменённый контекст → любая работа с пулом падает
	dead, cancel := context.WithCancel(context.Background())
	cancel()

	if err := svc.SetLocale(dead, uid, "en"); err == nil {
		t.Error("SetLocale на отменённом ctx должна вернуть ошибку")
	}
	if err := svc.SetTheme(dead, uid, "dark"); err == nil {
		t.Error("SetTheme на отменённом ctx должна вернуть ошибку")
	}
	if err := svc.DestroySession(dead, "tok"); err == nil {
		t.Error("DestroySession на отменённом ctx должна вернуть ошибку")
	}
	if err := svc.DeleteUser(dead, uid); err == nil {
		t.Error("DeleteUser на отменённом ctx должна вернуть ошибку")
	}
	if _, err := svc.DestroyOtherSessions(dead, uid, "keep"); err == nil {
		t.Error("DestroyOtherSessions на отменённом ctx должна вернуть ошибку")
	}
	if _, err := svc.DeleteExpiredSessions(dead); err == nil {
		t.Error("DeleteExpiredSessions на отменённом ctx должна вернуть ошибку")
	}
	if _, err := svc.UserCount(dead); err == nil {
		t.Error("UserCount на отменённом ctx должна вернуть ошибку")
	}
	if _, err := svc.HasPassword(dead, uid); err == nil {
		t.Error("HasPassword на отменённом ctx должна вернуть ошибку")
	}
	if err := svc.SetPassword(dead, uid, "brandnewpass1"); err == nil {
		t.Error("SetPassword на отменённом ctx должна вернуть ошибку")
	}
}

// TestRegisterValidation — ранние отбраковки Register без похода в БД.
func TestRegisterValidation(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.Register(ctx, "not-an-email", "hunter2hunter2"); err != auth.ErrInvalidEmail {
		t.Errorf("невалидный email → %v, want ErrInvalidEmail", err)
	}
	if _, err := svc.Register(ctx, "ok@example.com", "short"); err != auth.ErrWeakPassword {
		t.Errorf("короткий пароль → %v, want ErrWeakPassword", err)
	}
}
