package ingest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

type KeyResolver interface {
	KeyByPublic(ctx context.Context, publicKey string) (org.Key, error)
}

// Короче позитивного ttl: новый валидный ключ должен быстро заработать.
const negTTL = 10 * time.Second

const maxKeyCacheEntries = 10000

// Латентность отзыва ключа равна TTL кеша.
type KeyCache struct {
	resolver KeyResolver
	ttl      time.Duration
	negTTL   time.Duration
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]keyEntry
}

type keyEntry struct {
	key      org.Key
	expires  time.Time
	notFound bool // негативная запись: ключ отсутствует/отозван
}

func NewKeyCache(r KeyResolver) *KeyCache {
	return &KeyCache{
		resolver: r,
		ttl:      30 * time.Second,
		negTTL:   negTTL,
		now:      time.Now,
		entries:  map[string]keyEntry{},
	}
}

func (c *KeyCache) Resolve(ctx context.Context, publicKey string) (org.Key, error) {
	now := c.now()
	c.mu.Lock()
	if e, ok := c.entries[publicKey]; ok && e.expires.After(now) {
		c.mu.Unlock()
		if e.notFound {
			return org.Key{}, org.ErrNotFound
		}
		return e.key, nil
	}
	c.mu.Unlock()

	k, err := c.resolver.KeyByPublic(ctx, publicKey)
	if err != nil {
		// Только genuine «не найден» кешируем негативно; транзиентную ошибку — нет,
		// иначе валидный ключ оказался бы отвергнут на весь negTTL.
		if errors.Is(err, org.ErrNotFound) {
			c.store(publicKey, keyEntry{expires: now.Add(c.negTTL), notFound: true})
		}
		return org.Key{}, err
	}
	c.store(publicKey, keyEntry{key: k, expires: now.Add(c.ttl)})
	return k, nil
}

func (c *KeyCache) store(publicKey string, e keyEntry) {
	c.mu.Lock()
	if len(c.entries) >= maxKeyCacheEntries {
		c.evict()
	}
	c.entries[publicKey] = e
	c.mu.Unlock()
}

func (c *KeyCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// Порядок важен: сначала просроченное, затем негативные записи — иначе флуд
// случайными ключами вымывает и позитивные записи живых проектов. Вызывать под c.mu.
func (c *KeyCache) evict() {
	now := c.now()
	for k, e := range c.entries {
		if !e.expires.After(now) {
			delete(c.entries, k)
		}
	}
	if len(c.entries) < maxKeyCacheEntries {
		return
	}
	for k, e := range c.entries {
		if e.notFound {
			delete(c.entries, k)
		}
	}
	// Кеш забит живыми ключами: сбрасываем 10%, а не всё, чтобы не проседать разом.
	target := maxKeyCacheEntries - maxKeyCacheEntries/10
	for k := range c.entries {
		if len(c.entries) < target {
			break
		}
		delete(c.entries, k)
	}
}

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
