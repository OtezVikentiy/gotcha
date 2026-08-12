package web

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// TestStatusCacheWaiterHonorsRequestContext — ждущий чужой сборки запрос обязан
// отпустить свою горутину, как только клиент отвалился. Роут /status/{key}
// публичный, без сессии и без rate limit'а: если PG/CH подвиснут, каждый
// анонимный запрос по этому slug'у парковал бы горутину, которую уже некому
// освободить (до фикса ждущие висели на голом <-b.done).
func TestStatusCacheWaiterHonorsRequestContext(t *testing.T) {
	var c statusCache
	now := time.Now()

	// Ведущий заходит в build и застревает там (как на подвисшей БД).
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

	// Ждущий, чей запрос отменён (клиент закрыл соединение), возвращается сразу,
	// а не висит до конца чужой сборки.
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

	// Ведущий при этом свою сборку не бросает — её результат ждут другие.
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

// TestStatusCacheTTLFromBuildEnd — P2-11: TTL должен отсчитываться от момента,
// когда сборка ЗАВЕРШИЛАСЬ, а не от момента входа в load. До фикса putLocked
// брал заранее захваченный `now` (заход в load), так что запись, положенная в
// кеш после долгой сборки, жила заметно меньше заявленных statusCacheTTL —
// эффективный TTL = statusCacheTTL - длительность сборки.
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

	// От момента входа в load TTL пришлось бы отсчитывать так, что expires
	// оказался бы существенно раньше enteredAt+statusCacheTTL, если бы putLocked
	// всё ещё использовал старый `now`. Проверяем, что expires близок к
	// "сейчас + TTL" (после сборки), а не "enteredAt + TTL" (до неё).
	wantExpiresAfterBuild := time.Now().Add(statusCacheTTL)
	if entry.expires.Before(enteredAt.Add(statusCacheTTL).Add(buildDelay / 2)) {
		t.Fatalf("expires = %v looks anchored to load()-entry time (%v) rather than build completion (~%v); TTL is being eaten by build duration",
			entry.expires, enteredAt, wantExpiresAfterBuild)
	}
}
