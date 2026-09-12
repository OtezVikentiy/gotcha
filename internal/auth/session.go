package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// Без скользящего продления — TTL не сдвигается активностью.
const SessionTTL = 30 * 24 * time.Hour

var ErrNoSession = errors.New("auth: no such session")

func tokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// В БД хранится только sha256-хеш токена, не сам токен.
func (s *Service) CreateSession(ctx context.Context, userID int64) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	_, err := s.pool.Exec(ctx,
		"INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)",
		tokenHash(token), userID, time.Now().Add(SessionTTL))
	if err != nil {
		// Единственная точка для всех вызывающих (вход, регистрация, OAuth, смена
		// пароля): без неё сбой доходит до пользователя голым 500 без строки в логе.
		slog.Error("auth: create session failed", "user_id", userID, "error", err)
		return "", fmt.Errorf("auth: create session: %w", err)
	}
	return token, nil
}

func (s *Service) SessionUser(ctx context.Context, token string) (int64, error) {
	var userID int64
	err := s.pool.QueryRow(ctx,
		"SELECT user_id FROM sessions WHERE token_hash = $1 AND expires_at > now()",
		tokenHash(token)).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNoSession
	}
	if err != nil {
		return 0, fmt.Errorf("auth: session lookup: %w", err)
	}
	return userID, nil
}

func (s *Service) DestroySession(ctx context.Context, token string) error {
	if _, err := s.pool.Exec(ctx,
		"DELETE FROM sessions WHERE token_hash = $1", tokenHash(token)); err != nil {
		return fmt.Errorf("auth: destroy session: %w", err)
	}
	return nil
}

func (s *Service) DestroyOtherSessions(ctx context.Context, userID int64, keepToken string) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM sessions WHERE user_id = $1 AND token_hash <> $2",
		userID, tokenHash(keepToken))
	if err != nil {
		return 0, fmt.Errorf("auth: destroy other sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (s *Service) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE expires_at <= now()")
	if err != nil {
		return 0, fmt.Errorf("auth: delete expired sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}
