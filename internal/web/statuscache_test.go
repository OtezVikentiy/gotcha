package web

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// Роут /status/{key} публичный, без сессии и rate limit — если PG/CH подвиснут, анонимный
// запрос не должен парковать горутину, которую уже некому освободить.
func TestStatusCacheWaiterHonorsRequestContext(t *testing.T) {
	var c statusCache
	now := time.Now()

	leaderIn := make(chan struct{})
	release := make(chan struct{})
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = c.load(context.Background(), "slug", now, func() (templates.StatusPageView, error) {
			close(leaderIn)
			<-release
			return templates.StatusPageView{Title: "built"}, nil
		})
	}()
	<-leaderIn

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	returned := make(chan error, 1)
	go func() {
		_, err := c.load(ctx, "slug", now, func() (templates.StatusPageView, error) {
			t.Error("waiter must not start a second build")
			return templates.StatusPageView{}, nil
		})
		returned <- err
	}()

	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter with a cancelled request context is still blocked on the leader's build")
	}

	close(release)
	<-leaderDone

	view, err := c.load(context.Background(), "slug", now, func() (templates.StatusPageView, error) {
		t.Error("the built page must be served from the cache")
		return templates.StatusPageView{}, nil
	})
	if err != nil || view.Title != "built" {
		t.Fatalf("load() after the build = %+v, %v; want the cached page", view, err)
	}
}

// TTL должен отсчитываться от завершения сборки, не от входа в load — иначе долгая сборка
// съедала бы часть statusCacheTTL из времени жизни записи.
func TestStatusCacheTTLFromBuildEnd(t *testing.T) {
	var c statusCache
	enteredAt := time.Now()
	const buildDelay = 200 * time.Millisecond

	_, err := c.load(context.Background(), "slug", enteredAt, func() (templates.StatusPageView, error) {
		time.Sleep(buildDelay)
		return templates.StatusPageView{Title: "built"}, nil
	})
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}

	c.mu.Lock()
	entry, ok := c.entries["slug"]
	c.mu.Unlock()
	if !ok {
		t.Fatal("load() did not populate the cache")
	}

	wantExpiresAfterBuild := time.Now().Add(statusCacheTTL)
	if entry.expires.Before(enteredAt.Add(statusCacheTTL).Add(buildDelay / 2)) {
		t.Fatalf("expires = %v looks anchored to load()-entry time (%v) rather than build completion (~%v); TTL is being eaten by build duration",
			entry.expires, enteredAt, wantExpiresAfterBuild)
	}
}
