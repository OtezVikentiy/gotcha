package org

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
)

var (
	ErrNotFound     = errors.New("org: not found")
	ErrSlugTaken    = errors.New("org: slug already taken")
	ErrInvalidSlug  = errors.New("org: invalid slug")
	ErrInvalidName  = errors.New("org: name must not be empty")
	ErrInvalidQuota = errors.New("org: invalid quota")
)

var reSlug = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

func validSlug(slug string) bool {
	return reSlug.MatchString(slug)
}

func ValidSlug(slug string) bool {
	return validSlug(slug)
}

const (
	QuotaKindEvents       = "events"
	QuotaKindTransactions = "transactions"
	QuotaKindMetrics      = "metrics"
	QuotaKindProfiles     = "profiles"
	QuotaKindLogs         = "logs"
)

var QuotaKinds = []string{QuotaKindEvents, QuotaKindTransactions, QuotaKindMetrics, QuotaKindProfiles, QuotaKindLogs}

type Org struct {
	ID               int64
	Slug             string
	Name             string
	EventQuota       int64
	TransactionQuota int64
	MetricQuota      int64
	ProfileQuota     int64
	LogQuota         int64
}

type Service struct {
	pool                *pgxpool.Pool
	defaultQuota        int64
	defaultTxQuota      int64
	defaultMetricQuota  int64
	defaultProfileQuota int64
	defaultLogQuota     int64
	ring                secretbox.Keyring
	secretKeySet        bool
}

func NewService(pool *pgxpool.Pool, defaultQuota int64) *Service {
	return &Service{pool: pool, defaultQuota: defaultQuota}
}

func (s *Service) SetQuotaDefaults(transaction, metric, profile, log int64) {
	s.defaultTxQuota = transaction
	s.defaultMetricQuota = metric
	s.defaultProfileQuota = profile
	s.defaultLogQuota = log
}

func (s *Service) SetKeyring(ring secretbox.Keyring) {
	s.ring = ring
	s.secretKeySet = true
}

func (s *Service) CreateOrg(ctx context.Context, slug, name string, ownerID int64) (Org, error) {
	if !validSlug(slug) {
		return Org{}, ErrInvalidSlug
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Org{}, fmt.Errorf("org: create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	o := Org{
		Slug:             slug,
		Name:             name,
		EventQuota:       s.defaultQuota,
		TransactionQuota: s.defaultTxQuota,
		MetricQuota:      s.defaultMetricQuota,
		ProfileQuota:     s.defaultProfileQuota,
		LogQuota:         s.defaultLogQuota,
	}
	err = tx.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota, transaction_quota, metric_quota, profile_quota, log_quota) "+
			"VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id",
		slug, name, s.defaultQuota, s.defaultTxQuota, s.defaultMetricQuota, s.defaultProfileQuota, s.defaultLogQuota).Scan(&o.ID)
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

func (s *Service) DeleteOrg(ctx context.Context, orgID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("org: delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO project_purge_queue (project_id)
		 SELECT id FROM projects WHERE org_id = $1
		 ON CONFLICT (project_id) DO NOTHING`, orgID); err != nil {
		return fmt.Errorf("org: delete: enqueue purge: %w", err)
	}
	tag, err := tx.Exec(ctx, "DELETE FROM organizations WHERE id = $1", orgID)
	if err != nil {
		return fmt.Errorf("org: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("org: delete: %w", err)
	}
	return nil
}

func (s *Service) OrgsOf(ctx context.Context, userID int64) ([]Org, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT o.id, o.slug, o.name, o.event_quota, o.transaction_quota, o.metric_quota, o.profile_quota, o.log_quota FROM organizations o "+
			"JOIN org_members m ON m.org_id = o.id WHERE m.user_id = $1 ORDER BY o.name",
		userID)
	if err != nil {
		return nil, fmt.Errorf("org: orgs of: %w", err)
	}
	defer rows.Close()
	var out []Org
	for rows.Next() {
		var o Org
		if err := rows.Scan(&o.ID, &o.Slug, &o.Name, &o.EventQuota, &o.TransactionQuota, &o.MetricQuota, &o.ProfileQuota, &o.LogQuota); err != nil {
			return nil, fmt.Errorf("org: orgs of: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Service) Get(ctx context.Context, orgID int64) (Org, error) {
	o := Org{ID: orgID}
	err := s.pool.QueryRow(ctx,
		"SELECT slug, name, event_quota, transaction_quota, metric_quota, profile_quota, log_quota FROM organizations WHERE id = $1",
		orgID).Scan(&o.Slug, &o.Name, &o.EventQuota, &o.TransactionQuota, &o.MetricQuota, &o.ProfileQuota, &o.LogQuota)
	if errors.Is(err, pgx.ErrNoRows) {
		return Org{}, ErrNotFound
	}
	if err != nil {
		return Org{}, fmt.Errorf("org: get: %w", err)
	}
	return o, nil
}
