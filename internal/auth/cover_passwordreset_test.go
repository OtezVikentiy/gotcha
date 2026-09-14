package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Существующая строка password_resets блокируется FOR UPDATE в отдельной транзакции: DELETE
// внутри RequestPasswordReset упирается в лок и падает по дедлайну ctx, не касаясь INSERT.
func TestRequestPasswordReset_DeleteExecFails(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	bg := context.Background()

	uid, err := svc.Register(bg, "reset-delete-fail@example.com", "old-password-1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	first, _, err := svc.RequestPasswordReset(bg, "reset-delete-fail@example.com")
	if err != nil {
		t.Fatalf("RequestPasswordReset (1st): %v", err)
	}

	lockTx, err := pool.Begin(bg)
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	defer lockTx.Rollback(bg)
	if _, err := lockTx.Exec(bg, "SELECT id FROM password_resets WHERE user_id = $1 FOR UPDATE", uid); err != nil {
		t.Fatalf("lock existing row: %v", err)
	}

	shortCtx, cancel := context.WithTimeout(bg, 2*time.Second)
	defer cancel()
	if _, _, err := svc.RequestPasswordReset(shortCtx, "reset-delete-fail@example.com"); err == nil {
		t.Fatal("RequestPasswordReset с заблокированной строкой = nil, want ошибку дедлайна на DELETE")
	} else if !strings.Contains(err.Error(), "auth: request password reset: clear previous") {
		t.Errorf("err = %v, want содержащую %q", err, "auth: request password reset: clear previous")
	} else if strings.Contains(err.Error(), "insert") || strings.Contains(err.Error(), "commit") {
		t.Errorf("err = %v содержит метку следующего шага — DELETE не должен был дойти до INSERT/Commit", err)
	}

	if err := lockTx.Rollback(bg); err != nil {
		t.Fatalf("rollback lock tx: %v", err)
	}

	if !svc.ValidPasswordResetToken(bg, first) {
		t.Error("исходный токен инвалидирован при провале DELETE — транзакция обязана откатиться целиком")
	}
}

// users-строка блокируется FOR UPDATE: у свежего пользователя без прежних заявок DELETE не
// задевает ни одной строки и проходит мгновенно, а INSERT упирается в проверку внешнего ключа
// на заблокированную родительскую строку и падает по дедлайну.
func TestRequestPasswordReset_InsertExecFails(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	bg := context.Background()

	uid, err := svc.Register(bg, "reset-insert-fail@example.com", "old-password-1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	lockTx, err := pool.Begin(bg)
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	defer lockTx.Rollback(bg)
	if _, err := lockTx.Exec(bg, "SELECT id FROM users WHERE id = $1 FOR UPDATE", uid); err != nil {
		t.Fatalf("lock user row: %v", err)
	}

	shortCtx, cancel := context.WithTimeout(bg, 2*time.Second)
	defer cancel()
	if _, _, err := svc.RequestPasswordReset(shortCtx, "reset-insert-fail@example.com"); err == nil {
		t.Fatal("RequestPasswordReset с заблокированным users = nil, want ошибку дедлайна на INSERT")
	} else if !strings.Contains(err.Error(), "auth: request password reset: insert") {
		t.Errorf("err = %v, want содержащую %q", err, "auth: request password reset: insert")
	} else if strings.Contains(err.Error(), "clear previous") || strings.Contains(err.Error(), "commit") {
		t.Errorf("err = %v содержит метку другого шага — DELETE прошёл, упасть должен был INSERT", err)
	}

	if err := lockTx.Rollback(bg); err != nil {
		t.Fatalf("rollback lock tx: %v", err)
	}

	var count int
	if err := pool.QueryRow(bg, "SELECT count(*) FROM password_resets WHERE user_id = $1", uid).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("после провала INSERT осталось %d строк password_resets — транзакция обязана откатиться целиком", count)
	}
}

// Ни одна DB-операция не предшествует Begin в ResetPassword — отменённый ctx бьёт ровно в него.
func TestResetPassword_BeginFails(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	bg := context.Background()

	if _, err := svc.Register(bg, "reset-begin-fail@example.com", "old-password-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	token, _, err := svc.RequestPasswordReset(bg, "reset-begin-fail@example.com")
	if err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}

	dead, cancel := context.WithCancel(bg)
	cancel()

	err = svc.ResetPassword(dead, token, "new-password-1")
	if err == nil || errors.Is(err, ErrResetTokenInvalid) {
		t.Fatalf("ResetPassword на отменённом ctx = %v, want обёрнутую ошибку Begin, не nil/ErrResetTokenInvalid", err)
	}
	if !strings.Contains(err.Error(), "auth: reset password: begin") {
		t.Errorf("err = %v, want содержащую %q", err, "auth: reset password: begin")
	}
	if !svc.ValidPasswordResetToken(bg, token) {
		t.Error("токен тронут при провале Begin — до него дело дойти не должно")
	}
}

// Строка password_resets с активным токеном блокируется FOR UPDATE: UPDATE ... RETURNING внутри
// ResetPassword упирается в лок и падает по дедлайну — ошибка контекста, не pgx.ErrNoRows.
func TestResetPassword_QueryRowScanNonNoRowsError(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	bg := context.Background()

	uid, err := svc.Register(bg, "reset-queryrow-fail@example.com", "old-password-1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	token, _, err := svc.RequestPasswordReset(bg, "reset-queryrow-fail@example.com")
	if err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}

	lockTx, err := pool.Begin(bg)
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	defer lockTx.Rollback(bg)
	if _, err := lockTx.Exec(bg, "SELECT id FROM password_resets WHERE user_id = $1 FOR UPDATE", uid); err != nil {
		t.Fatalf("lock reset row: %v", err)
	}

	shortCtx, cancel := context.WithTimeout(bg, 2*time.Second)
	defer cancel()
	err = svc.ResetPassword(shortCtx, token, "new-password-1")
	if err == nil || errors.Is(err, ErrResetTokenInvalid) {
		t.Fatalf("ResetPassword с заблокированной строкой = %v, want ошибку дедлайна, не nil/ErrResetTokenInvalid", err)
	}
	if !strings.Contains(err.Error(), "auth: reset password: consume token") {
		t.Errorf("err = %v, want содержащую %q", err, "auth: reset password: consume token")
	} else if strings.Contains(err.Error(), "begin") || strings.Contains(err.Error(), "apply password") || strings.Contains(err.Error(), "commit") {
		t.Errorf("err = %v содержит метку другого шага — упасть должен был именно QueryRow.Scan", err)
	}

	if err := lockTx.Rollback(bg); err != nil {
		t.Fatalf("rollback lock tx: %v", err)
	}

	if !svc.ValidPasswordResetToken(bg, token) {
		t.Error("токен потреблён несмотря на провал UPDATE ... RETURNING — транзакция обязана откатиться целиком")
	}
}

// users-строка получателя блокируется FOR UPDATE: UPDATE ... RETURNING по password_resets
// проходит (другая таблица), а UPDATE users внутри setPasswordAndKillSessions упирается в лок.
func TestResetPassword_SetPasswordAndKillSessionsFails(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	bg := context.Background()

	uid, err := svc.Register(bg, "reset-setpass-fail@example.com", "old-password-1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, err := svc.CreateSession(bg, uid)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	token, _, err := svc.RequestPasswordReset(bg, "reset-setpass-fail@example.com")
	if err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}

	lockTx, err := pool.Begin(bg)
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	defer lockTx.Rollback(bg)
	if _, err := lockTx.Exec(bg, "SELECT id FROM users WHERE id = $1 FOR UPDATE", uid); err != nil {
		t.Fatalf("lock user row: %v", err)
	}

	shortCtx, cancel := context.WithTimeout(bg, 2*time.Second)
	defer cancel()
	err = svc.ResetPassword(shortCtx, token, "new-password-1")
	if err == nil || errors.Is(err, ErrResetTokenInvalid) {
		t.Fatalf("ResetPassword с заблокированным users = %v, want ошибку дедлайна, не nil/ErrResetTokenInvalid", err)
	}
	if !strings.Contains(err.Error(), "auth: reset password: apply password") {
		t.Errorf("err = %v, want содержащую %q", err, "auth: reset password: apply password")
	} else if strings.Contains(err.Error(), "begin") || strings.Contains(err.Error(), "consume token") || strings.Contains(err.Error(), "commit") {
		t.Errorf("err = %v содержит метку другого шага — упасть должен был именно setPasswordAndKillSessions", err)
	}

	if err := lockTx.Rollback(bg); err != nil {
		t.Fatalf("rollback lock tx: %v", err)
	}

	if !svc.ValidPasswordResetToken(bg, token) {
		t.Error("токен потреблён несмотря на провал смены пароля — транзакция обязана откатиться целиком")
	}
	if _, err := svc.SessionUser(bg, tok); err != nil {
		t.Errorf("сессия убита несмотря на откат всей транзакции: %v", err)
	}
}

// Тот же приём FOR UPDATE, что и для ResetPassword: UserByEmail и HashPassword не трогают
// заблокированную строку и успевают до дедлайна, UPDATE users внутри
// setPasswordAndKillSessions — упирается в лок.
func TestAdminSetPassword_SetPasswordAndKillSessionsFails(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	bg := context.Background()

	uid, err := svc.Register(bg, "admin-setpass-fail@example.com", "old-password-1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, err := svc.CreateSession(bg, uid)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	lockTx, err := pool.Begin(bg)
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	defer lockTx.Rollback(bg)
	if _, err := lockTx.Exec(bg, "SELECT id FROM users WHERE id = $1 FOR UPDATE", uid); err != nil {
		t.Fatalf("lock user row: %v", err)
	}

	shortCtx, cancel := context.WithTimeout(bg, 2*time.Second)
	defer cancel()
	if err := svc.AdminSetPassword(shortCtx, "admin-setpass-fail@example.com", "operator-set-1"); err == nil {
		t.Fatal("AdminSetPassword с заблокированным users = nil, want ошибку дедлайна")
	} else if !strings.Contains(err.Error(), "auth: admin set password: apply password") {
		t.Errorf("err = %v, want содержащую %q", err, "auth: admin set password: apply password")
	} else if strings.Contains(err.Error(), "begin") || strings.Contains(err.Error(), "commit") {
		t.Errorf("err = %v содержит метку другого шага — упасть должен был именно setPasswordAndKillSessions", err)
	}

	if err := lockTx.Rollback(bg); err != nil {
		t.Fatalf("rollback lock tx: %v", err)
	}

	if _, err := svc.Authenticate(bg, "admin-setpass-fail@example.com", "old-password-1"); err != nil {
		t.Errorf("старый пароль перестал работать несмотря на откат: %v", err)
	}
	if _, err := svc.SessionUser(bg, tok); err != nil {
		t.Errorf("сессия убита несмотря на откат всей транзакции: %v", err)
	}
}

// Транзакция закрывается (Rollback) до вызова — первый же Exec (UPDATE users) обязан упасть,
// до DELETE FROM sessions дело дойти не должно.
func TestSetPasswordAndKillSessionsUpdateUsersFails(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	bg := context.Background()

	uid, err := svc.Register(bg, "spaks-update-fail@example.com", "old-password-1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, err := svc.CreateSession(bg, uid)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	tx, err := pool.Begin(bg)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := tx.Rollback(bg); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	hash, err := HashPassword("new-password-1")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	err = setPasswordAndKillSessions(bg, tx, uid, hash)
	if err == nil {
		t.Fatal("setPasswordAndKillSessions на закрытой транзакции = nil, want ошибку")
	}
	if !strings.Contains(err.Error(), "set password") {
		t.Errorf("err = %v, want содержащую %q (провал UPDATE users, не DELETE sessions)", err, "set password")
	}

	if _, err := svc.Authenticate(bg, "spaks-update-fail@example.com", "old-password-1"); err != nil {
		t.Errorf("старый пароль перестал работать: %v", err)
	}
	if _, err := svc.SessionUser(bg, tok); err != nil {
		t.Errorf("сессия убита несмотря на провал UPDATE users: %v", err)
	}
}

// Декоратор pgx.Tx: пропускает Exec к реальной транзакции, кроме SQL с заданной подстрокой —
// на ней возвращает синтетическую ошибку, не касаясь БД.
type failOnMatchTx struct {
	pgx.Tx
	match string
}

func (f failOnMatchTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, f.match) {
		return pgconn.CommandTag{}, errors.New("injected: exec blocked")
	}
	return f.Tx.Exec(ctx, sql, args...)
}

// UPDATE users проходит на реальной (незакоммиченной) транзакции, DELETE FROM sessions
// перехватывается декоратором — проверяем, что после отката ни то ни другое не осталось в силе.
func TestSetPasswordAndKillSessionsDeleteSessionsFails(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)
	bg := context.Background()

	uid, err := svc.Register(bg, "spaks-delete-fail@example.com", "old-password-1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	tok, err := svc.CreateSession(bg, uid)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	tx, err := pool.Begin(bg)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(bg) }()
	wrapped := failOnMatchTx{Tx: tx, match: "DELETE FROM sessions"}

	hash, err := HashPassword("new-password-1")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	err = setPasswordAndKillSessions(bg, wrapped, uid, hash)
	if err == nil {
		t.Fatal("setPasswordAndKillSessions с заблокированным DELETE sessions = nil, want ошибку")
	}
	if !strings.Contains(err.Error(), "kill sessions") {
		t.Errorf("err = %v, want содержащую %q (провал DELETE sessions, не UPDATE users)", err, "kill sessions")
	}

	if err := tx.Rollback(bg); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var storedHash *string
	if err := pool.QueryRow(bg, "SELECT password_hash FROM users WHERE id = $1", uid).Scan(&storedHash); err != nil {
		t.Fatalf("select password_hash: %v", err)
	}
	if storedHash == nil {
		t.Fatal("password_hash = NULL после отката — Register должен был его установить")
	}
	if ok, _ := VerifyPassword("new-password-1", *storedHash); ok {
		t.Error("новый пароль применился несмотря на откат — UPDATE users не должен был закоммититься")
	}
	if _, err := svc.SessionUser(bg, tok); err != nil {
		t.Errorf("сессия убита несмотря на провал DELETE (и последующий откат): %v", err)
	}
}

func TestPurgeExpiredPasswordResetsExecFails(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := NewService(pool)

	dead, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := svc.PurgeExpiredPasswordResets(dead); err == nil {
		t.Fatal("PurgeExpiredPasswordResets на отменённом ctx = nil, want ошибку")
	} else if !strings.Contains(err.Error(), "auth: purge expired password resets") {
		t.Errorf("err = %v, want содержащую %q", err, "auth: purge expired password resets")
	}
}
