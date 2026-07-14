package ingest

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// KeyResolver — источник DSN-ключей; *org.Service ему удовлетворяет.
type KeyResolver interface {
	KeyByPublic(ctx context.Context, publicKey string) (org.Key, error)
}

// KeyCache кеширует позитивные ответы на keyTTL: ingest дёргает ключ на
// каждое событие. Латентность отзыва ключа = TTL кеша. Промахи не
// кешируются: невалидные ключи редки, а валидный новый ключ должен
// работать сразу.
type KeyCache struct {
	resolver KeyResolver
	ttl      time.Duration
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]keyEntry
}

type keyEntry struct {
	key     org.Key
	expires time.Time
}

func NewKeyCache(r KeyResolver) *KeyCache {
	return &KeyCache{
		resolver: r,
		ttl:      30 * time.Second,
		now:      time.Now,
		entries:  map[string]keyEntry{},
	}
}

// Resolve возвращает живой ключ по public key (из кеша или источника).
func (c *KeyCache) Resolve(ctx context.Context, publicKey string) (org.Key, error) {
	now := c.now()
	c.mu.Lock()
	if e, ok := c.entries[publicKey]; ok && e.expires.After(now) {
		c.mu.Unlock()
		return e.key, nil
	}
	c.mu.Unlock()

	k, err := c.resolver.KeyByPublic(ctx, publicKey)
	if err != nil {
		return org.Key{}, err
	}
	c.mu.Lock()
	c.entries[publicKey] = keyEntry{key: k, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	return k, nil
}

// PublicKeyFromRequest достаёт sentry_key из X-Sentry-Auth или query.
func PublicKeyFromRequest(r *http.Request) string {
	auth := r.Header.Get("X-Sentry-Auth")
	auth = strings.TrimPrefix(auth, "Sentry ")
	for _, part := range strings.Split(auth, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && k == "sentry_key" {
			return strings.Trim(v, `"`)
		}
	}
	return r.URL.Query().Get("sentry_key")
}
