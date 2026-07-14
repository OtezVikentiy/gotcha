package ingest

import (
	"context"
	"errors"
	"net/http/httptest"
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

	// Промахи не кешируются.
	if _, err := kc.Resolve(ctx, "nope"); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("miss: %v", err)
	}
	if _, err := kc.Resolve(ctx, "nope"); !errors.Is(err, org.ErrNotFound) {
		t.Fatalf("miss again: %v", err)
	}
	if fr.calls != 4 {
		t.Fatalf("calls = %d, want 4 (misses not cached)", fr.calls)
	}
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
