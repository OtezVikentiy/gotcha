package auth_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestUserThemeSetGet(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uid, err := svc.Register(ctx, "theme@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// по умолчанию пусто
	code, err := svc.UserTheme(ctx, uid)
	if err != nil || code != "" {
		t.Fatalf("default theme = %q, err=%v", code, err)
	}
	if err := svc.SetTheme(ctx, uid, "light"); err != nil {
		t.Fatalf("SetTheme: %v", err)
	}
	code, err = svc.UserTheme(ctx, uid)
	if err != nil || code != "light" {
		t.Fatalf("after SetTheme = %q, err=%v", code, err)
	}
}
