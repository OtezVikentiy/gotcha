package auth

import (
	"context"
	"fmt"
)

// UserTheme возвращает сохранённую тему оформления пользователя ("" — не задана).
func (s *Service) UserTheme(ctx context.Context, userID int64) (string, error) {
	var code string
	err := s.pool.QueryRow(ctx, "SELECT theme FROM users WHERE id = $1", userID).Scan(&code)
	if err != nil {
		return "", fmt.Errorf("auth: user theme: %w", err)
	}
	return code, nil
}

// SetTheme сохраняет тему оформления пользователя. Значение валидируется
// вызывающим (web-слой через theme.Parse) — здесь пишем как есть.
func (s *Service) SetTheme(ctx context.Context, userID int64, code string) error {
	if _, err := s.pool.Exec(ctx, "UPDATE users SET theme = $2 WHERE id = $1", userID, code); err != nil {
		return fmt.Errorf("auth: set theme: %w", err)
	}
	return nil
}
