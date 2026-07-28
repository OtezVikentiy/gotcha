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

// KeyResolver — источник DSN-ключей; *org.Service ему удовлетворяет.
type KeyResolver interface {
	KeyByPublic(ctx context.Context, publicKey string) (org.Key, error)
}

// negTTL — время жизни НЕГАТИВНОЙ записи (ключ не найден). Короче
// позитивного ttl (30s): валидный новый ключ должен заработать быстро, а
// негативная запись нужна лишь чтобы флуд неизвестными ключами не
// транслировался 1:1 в round-trip'ы к PostgreSQL (SEC-M1).
const negTTL = 10 * time.Second

// maxKeyCacheEntries — верхняя граница размера кеша. Негативные записи
// теперь тоже живут в entries, и поток из миллионов различных случайных
// ключей раздул бы map неограниченно. При переполнении map очищается
// целиком (простейшая корректная стратегия: негативные TTL короткие,
// позитивные восстановятся первым же событием проекта).
const maxKeyCacheEntries = 10000

// KeyCache кеширует ответы на TTL: ingest дёргает ключ на каждое событие.
// Латентность отзыва ключа = TTL кеша. Кешируются И позитивные (ttl), И
// негативные (negTTL, «ключ не найден») ответы: флуд неизвестными ключами
// иначе бьёт по общему pgx-пулу на каждом запросе (SEC-M1). Транзиентные
// ошибки (отмена ctx, таймаут пула) НЕ кешируются — иначе валидный ключ
// был бы ошибочно отвергнут на весь TTL.
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

// Resolve возвращает живой ключ по public key (из кеша или источника).
// Негативный кеш: если источник вернул org.ErrNotFound, запись живёт negTTL
// и повторные обращения к тому же неизвестному ключу обслуживаются из
// памяти, а не из PostgreSQL. Ошибка вызывающему возвращается прежняя
// (org.ErrNotFound → те же 404/auth-fail), поведение HTTP не меняется.
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
		// Только genuine «не найден» кешируем негативно; транзиентную
		// ошибку (ctx cancel, таймаут пула) — нет, иначе валидный ключ
		// оказался бы отвергнут на весь negTTL.
		if errors.Is(err, org.ErrNotFound) {
			c.store(publicKey, keyEntry{expires: now.Add(c.negTTL), notFound: true})
		}
		return org.Key{}, err
	}
	c.store(publicKey, keyEntry{key: k, expires: now.Add(c.ttl)})
	return k, nil
}

// store кладёт запись в кеш под mu, вытесняя записи при переполнении
// (см. maxKeyCacheEntries).
func (c *KeyCache) store(publicKey string, e keyEntry) {
	c.mu.Lock()
	if len(c.entries) >= maxKeyCacheEntries {
		c.evict()
	}
	c.entries[publicKey] = e
	c.mu.Unlock()
}

// size — число записей в кеше (для тестов вытеснения).
func (c *KeyCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// evict освобождает место в кеше. Порядок вытеснения важен: РАНЬШЕ кеш при
// переполнении стирался ЦЕЛИКОМ, поэтому поток запросов со случайными
// (несуществующими) ключами выбивал заодно и позитивные записи живых проектов —
// и легитимный трафик после каждого сброса снова бил в PostgreSQL на каждое
// событие. Теперь сначала уходит просроченное, затем НЕГАТИВНЫЕ записи (след
// перебора ключей), и лишь если и этого мало — произвольные, до отметки в 90%.
// Вызывать под c.mu.
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
	// Крайний случай: кеш забит живыми позитивными записями. Освобождаем 10%,
	// чтобы не сбрасывать всё и не расти без границы.
	target := maxKeyCacheEntries - maxKeyCacheEntries/10
	for k := range c.entries {
		if len(c.entries) < target {
			break
		}
		delete(c.entries, k)
	}
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
