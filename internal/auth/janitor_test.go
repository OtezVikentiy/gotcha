package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestJanitorDeletesExpiredSessions(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uid, err := svc.Register(ctx, "janitor@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	token, err := svc.CreateSession(ctx, uid)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET expires_at = now() - interval '1 minute'"); err != nil {
		t.Fatalf("expire: %v", err)
	}

	j := &auth.Janitor{Svc: svc, Interval: 50 * time.Millisecond}
	jCtx, jCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		j.Run(jCtx)
		close(done)
	}()
	t.Cleanup(func() {
		jCancel()
		<-done
	})

	// Не отличить через SessionUser удалённую сессию от просроченной — считаем строки напрямую.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM sessions").Scan(&n); err != nil {
			t.Fatalf("count sessions: %v", err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expired session row was not deleted within 5s (remaining=%d)", n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := svc.SessionUser(ctx, token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("expired session should be gone: %v", err)
	}
}

func TestJanitorRunFirstPassIsImmediate(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := auth.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	uid, err := svc.Register(ctx, "janitor-firstpass@example.com", "hunter2hunter2")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := svc.CreateSession(ctx, uid); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET expires_at = now() - interval '1 minute'"); err != nil {
		t.Fatalf("expire: %v", err)
	}

	// Без первого прохода сессия осталась бы нетронутой — Interval нарочно больше времени всего теста.
	j := &auth.Janitor{Svc: svc, Interval: time.Hour}
	jCtx, jCancel := context.WithCancel(context.Background())
	defer jCancel()
	go j.Run(jCtx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM sessions").Scan(&n); err != nil {
			t.Fatalf("count sessions: %v", err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("просроченная сессия не убрана за 5с — первого прохода нет, чистка ждёт целый Interval")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
