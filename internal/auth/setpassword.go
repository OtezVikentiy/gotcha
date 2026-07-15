package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrPasswordAlreadySet — SetPassword вызван для аккаунта, у которого пароль
// уже есть (для смены пароля есть ChangePassword со старым паролем).
var ErrPasswordAlreadySet = errors.New("auth: password already set")

// HasPassword сообщает, задан ли у аккаунта пароль (не NULL). Используется
// профилем: OAuth-only юзеру показываем «задать пароль», а не «сменить».
func (s *Service) HasPassword(ctx context.Context, userID int64) (bool, error) {
	var hash *string
	err := s.pool.QueryRow(ctx,
		"SELECT password_hash FROM users WHERE id = $1", userID).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrInvalidCredentials
	}
	if err != nil {
		return false, fmt.Errorf("auth: has password: %w", err)
	}
	return hash != nil, nil
}

// SetPassword задаёт пароль аккаунту БЕЗ пароля (OAuth-only). Валидирует длину
// теми же правилами, что Register. Если пароль уже есть — ErrPasswordAlreadySet
// (менять существующий нужно через ChangePassword). Сессии не трогает: вызов
// идёт из активной сессии, которую незачем инвалидировать.
func (s *Service) SetPassword(ctx context.Context, userID int64, newPassword string) error {
	if len(newPassword) < 8 || len(newPassword) > 512 {
		return ErrWeakPassword
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	// Условный апдейт: пишем хеш только если он ещё NULL. RowsAffected==0
	// значит либо нет юзера, либо пароль уже задан — различаем добором.
	tag, err := s.pool.Exec(ctx,
		"UPDATE users SET password_hash = $2 WHERE id = $1 AND password_hash IS NULL",
		userID, hash)
	if err != nil {
		return fmt.Errorf("auth: set password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx,
			"SELECT true FROM users WHERE id = $1", userID).Scan(&exists); errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidCredentials
		} else if err != nil {
			return fmt.Errorf("auth: set password: %w", err)
		}
		return ErrPasswordAlreadySet
	}
	return nil
}
