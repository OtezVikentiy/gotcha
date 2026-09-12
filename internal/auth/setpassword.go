package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Смена существующего пароля — через ChangePassword со старым паролем.
var ErrPasswordAlreadySet = errors.New("auth: password already set")

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

// Сессии не трогает — вызов идёт из уже активной сессии, инвалидировать нечего.
func (s *Service) SetPassword(ctx context.Context, userID int64, newPassword string) error {
	if len(newPassword) < 8 || len(newPassword) > 512 {
		return ErrWeakPassword
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	// RowsAffected==0 значит либо юзера нет, либо пароль уже задан — различаем добором ниже.
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
