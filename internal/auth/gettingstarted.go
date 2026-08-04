package auth

import (
	"context"
	"fmt"
)

// HideGettingStarted сообщает, скрыл ли пользователь чек-лист «Первые шаги»
// (№71). Флаг живёт в профиле, а не в cookie: скрытое не должно воскресать
// на другом устройстве.
func (s *Service) HideGettingStarted(ctx context.Context, userID int64) (bool, error) {
	var hidden bool
	err := s.pool.QueryRow(ctx, "SELECT hide_getting_started FROM users WHERE id = $1", userID).Scan(&hidden)
	if err != nil {
		return false, fmt.Errorf("auth: hide getting started: %w", err)
	}
	return hidden, nil
}

// SetHideGettingStarted скрывает чек-лист навсегда. Обратной ручки нет
// намеренно: чек-лист и сам исчезает по мере закрытия шагов, «Скрыть» — это
// решение «я знаю, что делаю», а не переключатель.
func (s *Service) SetHideGettingStarted(ctx context.Context, userID int64) error {
	if _, err := s.pool.Exec(ctx, "UPDATE users SET hide_getting_started = true WHERE id = $1", userID); err != nil {
		return fmt.Errorf("auth: set hide getting started: %w", err)
	}
	return nil
}
