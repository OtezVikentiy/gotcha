package auth_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestUserLocaleSetGet(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uid, err := svc.Register(ctx, "loc@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// по умолчанию пусто
	code, err := svc.UserLocale(ctx, uid)
	if err != nil || code != "" {
		t.Fatalf("default locale = %q, err=%v", code, err)
	}
	if err := svc.SetLocale(ctx, uid, "en"); err != nil {
		t.Fatalf("SetLocale: %v", err)
	}
	code, err = svc.UserLocale(ctx, uid)
	if err != nil || code != "en" {
		t.Fatalf("after SetLocale = %q, err=%v", code, err)
	}
}
