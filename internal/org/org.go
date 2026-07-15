// Package org — тенантность: организации, команды, проекты, роли,
// DSN-ключи и приглашения. Всё внутри принадлежит организации.
package org

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound     = errors.New("org: not found")
	ErrSlugTaken    = errors.New("org: slug already taken")
	ErrInvalidSlug  = errors.New("org: invalid slug")
	ErrInvalidName  = errors.New("org: name must not be empty")
	ErrInvalidQuota = errors.New("org: invalid quota")
)

// reSlug — lower-case буквенно-цифровой slug с дефисами, без дефисов по краям,
// 1..64 символа.
var reSlug = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

func validSlug(slug string) bool {
	return reSlug.MatchString(slug)
}

// ValidSlug — экспортированная проверка синтаксиса slug'а (та же, что
// используют CreateOrg/CreateProject). Нужна вызывающему коду (например,
// онбордингу), чтобы провалидировать оба slug'а до похода в БД и не
// заводить частично созданные записи при ошибке на втором шаге.
func ValidSlug(slug string) bool {
	return validSlug(slug)
}

type Org struct {
	ID         int64
	Slug       string
	Name       string
	EventQuota int64
	// TransactionQuota — месячная квота транзакций, счётчик у неё свой
	// (org_usage.transactions_count): транзакции не тратят бюджет ошибок.
	TransactionQuota int64
}

// Service — доменная логика тенантности поверх PostgreSQL.
type Service struct {
	pool         *pgxpool.Pool
	defaultQuota int64
}

func NewService(pool *pgxpool.Pool, defaultQuota int64) *Service {
	return &Service{pool: pool, defaultQuota: defaultQuota}
}

// CreateOrg создаёт организацию и делает ownerID её owner'ом (одна транзакция).
func (s *Service) CreateOrg(ctx context.Context, slug, name string, ownerID int64) (Org, error) {
	if !validSlug(slug) {
		return Org{}, ErrInvalidSlug
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Org{}, fmt.Errorf("org: create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	o := Org{Slug: slug, Name: name, EventQuota: s.defaultQuota}
	err = tx.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1, $2, $3) RETURNING id",
		slug, name, s.defaultQuota).Scan(&o.ID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Org{}, ErrSlugTaken
	}
	if err != nil {
		return Org{}, fmt.Errorf("org: create: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner')",
		o.ID, ownerID); err != nil {
		return Org{}, fmt.Errorf("org: create owner: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Org{}, fmt.Errorf("org: create: %w", err)
	}
	return o, nil
}

// DeleteOrg удаляет организацию (FK-и на неё — org_members, projects и т.д. —
// каскадные). Используется онбордингом для компенсации, когда организация
// успела создаться, а последующий шаг (проект, ключ) провалился, и в будущем —
// настройками организации (ручное удаление).
func (s *Service) DeleteOrg(ctx context.Context, orgID int64) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM organizations WHERE id = $1", orgID)
	if err != nil {
		return fmt.Errorf("org: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// OrgsOf возвращает все организации, в которых состоит userID (в любой
// роли), отсортированные по name. Используется страницей "/" (задача 5,
// задача 4): различить юзера без единой организации (нужен /onboarding) от
// юзера-члена организации(й), которому просто не назначен ни один проект
// (нужна страница «нет доступных проектов»).
func (s *Service) OrgsOf(ctx context.Context, userID int64) ([]Org, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT o.id, o.slug, o.name, o.event_quota, o.transaction_quota FROM organizations o "+
			"JOIN org_members m ON m.org_id = o.id WHERE m.user_id = $1 ORDER BY o.name",
		userID)
	if err != nil {
		return nil, fmt.Errorf("org: orgs of: %w", err)
	}
	defer rows.Close()
	var out []Org
	for rows.Next() {
		var o Org
		if err := rows.Scan(&o.ID, &o.Slug, &o.Name, &o.EventQuota, &o.TransactionQuota); err != nil {
			return nil, fmt.Errorf("org: orgs of: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Get возвращает организацию по id.
func (s *Service) Get(ctx context.Context, orgID int64) (Org, error) {
	o := Org{ID: orgID}
	err := s.pool.QueryRow(ctx,
		"SELECT slug, name, event_quota, transaction_quota FROM organizations WHERE id = $1",
		orgID).Scan(&o.Slug, &o.Name, &o.EventQuota, &o.TransactionQuota)
	if errors.Is(err, pgx.ErrNoRows) {
		return Org{}, ErrNotFound
	}
	if err != nil {
		return Org{}, fmt.Errorf("org: get: %w", err)
	}
	return o, nil
}
