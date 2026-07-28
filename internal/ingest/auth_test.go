package ingest

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

type fakeResolver struct {
	calls int
	keys  map[string]org.Key
}

func (f *fakeResolver) KeyByPublic(_ context.Context, pub string) (org.Key, error) {
	f.calls++
	k, ok := f.keys[pub]
	if !ok {
		return org.Key{}, org.ErrNotFound
	}
	return k, nil
}

func TestKeyCache(t *testing.T) {
	fr := &fakeResolver{keys: map[string]org.Key{"abc": {ID: 1, ProjectID: 7, OrgID: 3, PublicKey: "abc"}}}
	kc := NewKeyCache(fr)
	now := time.Now()
	kc.now = func() time.Time { return now }
	ctx := context.Background()

	k, err := kc.Resolve(ctx, "abc")
	if err != nil || k.ProjectID != 7 {
		t.Fatalf("first resolve: %+v err=%v", k, err)
	}
	if _, err := kc.Resolve(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	if fr.calls != 1 {
		t.Fatalf("calls = %d, want 1 (cached)", fr.calls)
	}

	// TTL истёк — ходим в источник снова.
	now = now.Add(31 * time.Second)
	if _, err := kc.Resolve(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	if fr.calls != 2 {
		t.Fatalf("calls = %d, want 2 (expired)", fr.calls)
	}

	// Промахи негативно кешируются на negTTL: два подряд Resolve одного
	// неизвестного ключа бьют в источник лишь ОДИН раз (SEC-M1).
	if _, err := kc.Resolve(ctx, "nope"); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("miss: %v", err)
	}
	if _, err := kc.Resolve(ctx, "nope"); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("miss again: %v", err)
	}
	if fr.calls != 3 {
		t.Fatalf("calls = %d, want 3 (miss negative-cached)", fr.calls)
	}

	// По истечении negTTL негативная запись протухает — снова идём в источник.
	now = now.Add(negTTL + time.Second)
	if _, err := kc.Resolve(ctx, "nope"); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("miss after neg TTL: %v", err)
	}
	if fr.calls != 4 {
		t.Fatalf("calls = %d, want 4 (neg entry expired)", fr.calls)
	}
}

// TestKeyCacheTransientNotCached: транзиентная ошибка (не ErrNotFound) НЕ
// кешируется — иначе валидный ключ был бы отвергнут на весь negTTL.
func TestKeyCacheTransientNotCached(t *testing.T) {
	fr := &flakyResolver{err: context.DeadlineExceeded}
	kc := NewKeyCache(fr)
	ctx := context.Background()
	if _, err := kc.Resolve(ctx, "k"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first: %v", err)
	}
	if _, err := kc.Resolve(ctx, "k"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second: %v", err)
	}
	if fr.calls != 2 {
		t.Fatalf("calls = %d, want 2 (transient error not cached)", fr.calls)
	}
}

type flakyResolver struct {
	calls int
	err   error
}

func (f *flakyResolver) KeyByPublic(_ context.Context, _ string) (org.Key, error) {
	f.calls++
	return org.Key{}, f.err
}

func TestPublicKeyFromRequest(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/1/envelope/", nil)
	r.Header.Set("X-Sentry-Auth",
		"Sentry sentry_version=7, sentry_client=sentry.php/4.10, sentry_key=deadbeef")
	if got := PublicKeyFromRequest(r); got != "deadbeef" {
		t.Errorf("header: %q", got)
	}

	r = httptest.NewRequest("POST", "/api/1/envelope/?sentry_key=cafebabe", nil)
	if got := PublicKeyFromRequest(r); got != "cafebabe" {
		t.Errorf("query: %q", got)
	}

	r = httptest.NewRequest("POST", "/api/1/envelope/", nil)
	if got := PublicKeyFromRequest(r); got != "" {
		t.Errorf("absent: %q", got)
	}
}

// TestKeyCacheFloodKeepsLiveProjects фиксирует правку вытеснения: поток запросов
// со случайными несуществующими ключами не должен выбивать из кеша позитивные
// записи живых проектов. Раньше кеш при переполнении стирался ЦЕЛИКОМ, поэтому
// перебор ключей заставлял и легитимный трафик ходить в PostgreSQL на каждое
// событие — усиление нагрузки на общий пул.
func TestKeyCacheFloodKeepsLiveProjects(t *testing.T) {
	fr := &fakeResolver{keys: map[string]org.Key{"live": {ID: 1, ProjectID: 7, OrgID: 3, PublicKey: "live"}}}
	kc := NewKeyCache(fr)
	now := time.Now()
	kc.now = func() time.Time { return now }
	ctx := context.Background()

	// Живой проект попадает в кеш.
	if _, err := kc.Resolve(ctx, "live"); err != nil {
		t.Fatalf("resolve live: %v", err)
	}
	callsAfterWarmup := fr.calls

	// Перебор: заведомо больше ёмкости кеша несуществующих ключей.
	for i := 0; i < maxKeyCacheEntries+5000; i++ {
		_, _ = kc.Resolve(ctx, "miss-"+strconv.Itoa(i))
	}

	// Живой ключ обязан по-прежнему обслуживаться из кеша.
	if _, err := kc.Resolve(ctx, "live"); err != nil {
		t.Fatalf("resolve live after flood: %v", err)
	}
	if got := fr.calls - callsAfterWarmup; got != maxKeyCacheEntries+5000 {
		t.Errorf("живой ключ вытеснен перебором: лишних обращений к источнику %d",
			got-(maxKeyCacheEntries+5000))
	}
	if kc.size() > maxKeyCacheEntries {
		t.Errorf("кеш вырос за границу: %d > %d", kc.size(), maxKeyCacheEntries)
	}
}
