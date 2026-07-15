package ingest

import (
	"context"
	"sync"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// ProjectResolver — источник настроек проекта; *org.Service ему удовлетворяет.
type ProjectResolver interface {
	GetProject(ctx context.Context, projectID int64) (org.Project, error)
}

// ProjectSettings — то, что нужно Handler'у от проекта на горячем пути
// (transaction_sample_rate). Интерфейс, а не *ProjectCache, чтобы хендлер
// тестировался без БД.
type ProjectSettings interface {
	Resolve(ctx context.Context, projectID int64) (org.Project, error)
}

// ProjectCache кеширует настройки проекта на projectTTL — ingest читает
// transaction_sample_rate на каждую транзакцию, и ходить за ним в PG каждый
// раз незачем. Латентность применения новой настройки = TTL кеша (как у
// KeyCache/OrgQuota). Промахи не кешируются.
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

// Resolve возвращает проект по id (из кеша или источника).
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
	c.entries[projectID] = projectEntry{project: p, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	return p, nil
}
