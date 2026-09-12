package auth

import (
	"context"
	"fmt"
)

// "" — язык не задан.
func (s *Service) UserLocale(ctx context.Context, userID int64) (string, error) {
	var code string
	err := s.pool.QueryRow(ctx, "SELECT locale FROM users WHERE id = $1", userID).Scan(&code)
	if err != nil {
		return "", fmt.Errorf("auth: user locale: %w", err)
	}
	return code, nil
}

// Валидация — на вызывающем (web через i18n.Parse), здесь пишем как есть.
func (s *Service) SetLocale(ctx context.Context, userID int64, code string) error {
	if _, err := s.pool.Exec(ctx, "UPDATE users SET locale = $2 WHERE id = $1", userID, code); err != nil {
		return fmt.Errorf("auth: set locale: %w", err)
	}
	return nil
}
