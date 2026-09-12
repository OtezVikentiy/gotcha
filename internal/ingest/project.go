package ingest

import (
	"context"
	"sync"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

type ProjectResolver interface {
	GetProject(ctx context.Context, projectID int64) (org.Project, error)
}

// интерфейс, а не *ProjectCache: хендлер тестируется без БД.
type ProjectSettings interface {
	Resolve(ctx context.Context, projectID int64) (org.Project, error)
}

// размер карты ограничен maxKeyCacheEntries, как у KeyCache/OrgQuota — иначе на
// инсталляции с большим числом проектов карта росла бы без границ. Промахи не кешируются.
type ProjectCache struct {
	resolver ProjectResolver
	ttl      time.Duration
	now      func() time.Time

	mu      sync.Mutex
	entries map[int64]projectEntry
}

type projectEntry struct {
	project org.Project
	expires time.Time
}

func NewProjectCache(r ProjectResolver) *ProjectCache {
	return &ProjectCache{
		resolver: r,
		ttl:      30 * time.Second,
		now:      time.Now,
		entries:  map[int64]projectEntry{},
	}
}

func (c *ProjectCache) Resolve(ctx context.Context, projectID int64) (org.Project, error) {
	now := c.now()
	c.mu.Lock()
	if e, ok := c.entries[projectID]; ok && e.expires.After(now) {
		c.mu.Unlock()
		return e.project, nil
	}
	c.mu.Unlock()

	p, err := c.resolver.GetProject(ctx, projectID)
	if err != nil {
		return org.Project{}, err
	}
	c.mu.Lock()
	if len(c.entries) >= maxKeyCacheEntries {
		c.evict(now)
	}
	c.entries[projectID] = projectEntry{project: p, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	return p, nil
}

// сперва истёкшие записи, затем десятая часть произвольных, если не хватило.
// Вызывать под c.mu.
func (c *ProjectCache) evict(now time.Time) {
	for id, e := range c.entries {
		if !e.expires.After(now) {
			delete(c.entries, id)
		}
	}
	if len(c.entries) < maxKeyCacheEntries {
		return
	}
	drop := len(c.entries) / 10
	if drop == 0 {
		drop = 1
	}
	for id := range c.entries {
		if drop == 0 {
			break
		}
		delete(c.entries, id)
		drop--
	}
}
