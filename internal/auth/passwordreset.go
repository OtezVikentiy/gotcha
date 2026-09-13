package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Короче инвайта (7 дней): токен даёт немедленный полный доступ к аккаунту, а не только
// членство после входа под тем же email — риск от утечки выше, окно держим коротким.
const PasswordResetTTL = time.Hour

var ErrResetTokenInvalid = errors.New("auth: password reset token is invalid, expired or already used")

func resetTokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// found=false для неизвестного email — вызывающий обязан ответить одинаково в обоих случаях,
// иначе перебор адресов раскрывает, какие из них зарегистрированы.
func (s *Service) RequestPasswordReset(ctx context.Context, email string) (token string, found bool, err error) {
	uid, err := s.UserByEmail(ctx, email)
	if errors.Is(err, ErrUserNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("auth: request password reset: %w", err)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", false, fmt.Errorf("auth: reset token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("auth: request password reset: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Один активный токен на пользователя: прошлые запросы гаснут сразу, не дожидаясь
	// собственного TTL — иначе письма из старых запросов остаются рабочими ссылками.
	if _, err := tx.Exec(ctx, "DELETE FROM password_resets WHERE user_id = $1", uid); err != nil {
		return "", false, fmt.Errorf("auth: request password reset: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO password_resets (user_id, token_hash, expires_at) VALUES ($1, $2, $3)",
		uid, resetTokenHash(token), time.Now().Add(PasswordResetTTL)); err != nil {
		return "", false, fmt.Errorf("auth: request password reset: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("auth: request password reset: %w", err)
	}
	return token, true, nil
}

// Не потребляет токен — используется страницей формы (GET), чтобы решить, показывать её
// или сразу отказ; само потребление делает только ResetPassword.
func (s *Service) ValidPasswordResetToken(ctx context.Context, token string) bool {
	var exists bool
	err := s.pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM password_resets WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now())",
		resetTokenHash(token)).Scan(&exists)
	return err == nil && exists
}

// Слабый пароль отклоняется до Commit — незакоммиченный UPDATE used_at откатывается вместе с
// остальной транзакцией, так что токен не сгорает и опечатку в форме можно повторить той же ссылкой.
func (s *Service) ResetPassword(ctx context.Context, token, newPassword string) error {
	if len(newPassword) < 8 || len(newPassword) > 512 {
		return ErrWeakPassword
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("auth: reset password: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID int64
	err = tx.QueryRow(ctx, `
		UPDATE password_resets SET used_at = now()
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
		RETURNING user_id`,
		resetTokenHash(token)).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrResetTokenInvalid
	}
	if err != nil {
		return fmt.Errorf("auth: reset password: %w", err)
	}

	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	if err := setPasswordAndKillSessions(ctx, tx, userID, hash); err != nil {
		return fmt.Errorf("auth: reset password: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("auth: reset password: %w", err)
	}
	return nil
}

func (s *Service) PurgeExpiredPasswordResets(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM password_resets WHERE expires_at < now() OR used_at IS NOT NULL")
	if err != nil {
		return 0, fmt.Errorf("auth: purge expired password resets: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Для CLI-подкоманды оператора: доступ к БД инстанса — само по себе право, старый пароль
// не нужен. Отдельна от SetPassword (та отказывает, если хеш уже есть) и её не ослабляет.
func (s *Service) AdminSetPassword(ctx context.Context, email, newPassword string) error {
	if len(newPassword) < 8 || len(newPassword) > 512 {
		return ErrWeakPassword
	}
	uid, err := s.UserByEmail(ctx, email)
	if err != nil {
		return err
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("auth: admin set password: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := setPasswordAndKillSessions(ctx, tx, uid, hash); err != nil {
		return fmt.Errorf("auth: admin set password: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("auth: admin set password: %w", err)
	}
	return nil
}

// Общий хвост ResetPassword/AdminSetPassword: любая принудительная замена пароля обязана
// убить все существующие сессии — тот же принцип, что у ChangePassword.
func setPasswordAndKillSessions(ctx context.Context, tx pgx.Tx, userID int64, hash string) error {
	if _, err := tx.Exec(ctx, "UPDATE users SET password_hash = $2 WHERE id = $1", userID, hash); err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM sessions WHERE user_id = $1", userID); err != nil {
		return fmt.Errorf("kill sessions: %w", err)
	}
	return nil
}
