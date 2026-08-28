package auth_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestCreateSessionUnknownUser — sessions.user_id ссылается на users(id) с
// FK-констрейнтом; CreateSession для несуществующего userID обязан вернуть
// обёрнутую ошибку INSERT (нарушение внешнего ключа), а не запись-сироту.
func TestCreateSessionUnknownUser(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.CreateSession(ctx, 987654321); err == nil {
		t.Fatal("CreateSession(несуществующий userID) = nil, want ошибку нарушения внешнего ключа")
	}
}
