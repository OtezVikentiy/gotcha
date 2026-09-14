package auth_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestCreateSessionUnknownUser(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if _, err := svc.CreateSession(ctx, 987654321); err == nil {
		t.Fatal("CreateSession(несуществующий userID) = nil, want ошибку нарушения внешнего ключа")
	}

	if !strings.Contains(buf.String(), "auth: create session failed") {
		t.Errorf("сбой CreateSession не залогирован — вход/регистрация на read-only базе "+
			"отдали бы голый 500 без единой строки в логе:\n%s", buf.String())
	}
}
