package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestCreateOAuthUserInvalidEmail(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := svc.CreateOAuthUser(ctx, "not-an-email"); !errors.Is(err, auth.ErrInvalidEmail) {
		t.Fatalf("CreateOAuthUser(невалидный email) = %v, want ErrInvalidEmail", err)
	}
}
