package org

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrDomainTaken — домен уже привязан к другой организации (UNIQUE domain).
	ErrDomainTaken = errors.New("org: sso domain already used by another organization")
	// ErrInvalidSSO — обязательные поля SSO-конфига пусты.
	ErrInvalidSSO = errors.New("org: sso config requires issuer, client_id, client_secret and domain")
)

// SSOConfig — per-org OIDC-конфиг (этап 10). Хранится в org_sso.
type SSOConfig struct {
	OrgID        int64
	Issuer       string
	ClientID     string
	ClientSecret string
	Domain       string
	DefaultRole  string // 'admin' | 'member'
	Enforced     bool
}

func normalizeDomain(d string) string { return strings.ToLower(strings.TrimSpace(d)) }

// UpsertSSO создаёт/обновляет SSO-конфиг организации. Валидирует непустые
// issuer/client_id/client_secret/domain и default_role. Домен, занятый другой
// организацией → ErrDomainTaken.
func (s *Service) UpsertSSO(ctx context.Context, cfg SSOConfig) error {
	cfg.Domain = normalizeDomain(cfg.Domain)
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.Domain == "" {
		return ErrInvalidSSO
	}
	if cfg.DefaultRole == "" {
		cfg.DefaultRole = string(RoleMember)
	}
	if cfg.DefaultRole != string(RoleAdmin) && cfg.DefaultRole != string(RoleMember) {
		return ErrInvalidRole
	}
	// Шифруем client_secret at-rest, если задан мастер-ключ. Без ключа (dev)
	// пишем plaintext — читатель это распознаёт по отсутствию префикса "enc:".
	storedSecret := cfg.ClientSecret
	if s.secretKeySet {
		sealed, err := sealSecret(s.secretKey, cfg.ClientSecret)
		if err != nil {
			return fmt.Errorf("org: seal sso secret: %w", err)
		}
		storedSecret = sealed
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO org_sso (org_id, issuer, client_id, client_secret, domain, default_role, enforced)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (org_id) DO UPDATE SET
			issuer=$2, client_id=$3, client_secret=$4, domain=$5, default_role=$6, enforced=$7`,
		cfg.OrgID, cfg.Issuer, cfg.ClientID, storedSecret, cfg.Domain, cfg.DefaultRole, cfg.Enforced)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "domain") {
		return ErrDomainTaken
	}
	if err != nil {
		return fmt.Errorf("org: upsert sso: %w", err)
	}
	return nil
}

const ssoColumns = "org_id, issuer, client_id, client_secret, domain, default_role, enforced"

func scanSSO(row pgx.Row) (SSOConfig, error) {
	var c SSOConfig
	err := row.Scan(&c.OrgID, &c.Issuer, &c.ClientID, &c.ClientSecret, &c.Domain, &c.DefaultRole, &c.Enforced)
	return c, err
}

// decryptSSO расшифровывает client_secret прочитанного конфига, если задан
// мастер-ключ. openSecret на legacy-plaintext (без префикса "enc:") вернёт
// значение как есть, поэтому вызов безопасен и для старых записей.
func (s *Service) decryptSSO(c SSOConfig) (SSOConfig, error) {
	if !s.secretKeySet {
		return c, nil
	}
	secret, err := openSecret(s.secretKey, c.ClientSecret)
	if err != nil {
		return SSOConfig{}, fmt.Errorf("org: open sso secret: %w", err)
	}
	c.ClientSecret = secret
	return c, nil
}

// SSOByOrg возвращает SSO-конфиг организации, если он есть.
func (s *Service) SSOByOrg(ctx context.Context, orgID int64) (SSOConfig, bool, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+ssoColumns+" FROM org_sso WHERE org_id = $1", orgID)
	c, err := scanSSO(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return SSOConfig{}, false, nil
	}
	if err != nil {
		return SSOConfig{}, false, fmt.Errorf("org: sso by org: %w", err)
	}
	c, err = s.decryptSSO(c)
	if err != nil {
		return SSOConfig{}, false, err
	}
	return c, true, nil
}

// SSOByDomain возвращает SSO-конфиг по email-домену (identifier-first вход и
// принуждение). Пустой домен → не найдено.
func (s *Service) SSOByDomain(ctx context.Context, domain string) (SSOConfig, bool, error) {
	domain = normalizeDomain(domain)
	if domain == "" {
		return SSOConfig{}, false, nil
	}
	row := s.pool.QueryRow(ctx, "SELECT "+ssoColumns+" FROM org_sso WHERE domain = $1", domain)
	c, err := scanSSO(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return SSOConfig{}, false, nil
	}
	if err != nil {
		return SSOConfig{}, false, fmt.Errorf("org: sso by domain: %w", err)
	}
	c, err = s.decryptSSO(c)
	if err != nil {
		return SSOConfig{}, false, err
	}
	return c, true, nil
}

// DeleteSSO удаляет SSO-конфиг организации (идемпотентно).
func (s *Service) DeleteSSO(ctx context.Context, orgID int64) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM org_sso WHERE org_id = $1", orgID); err != nil {
		return fmt.Errorf("org: delete sso: %w", err)
	}
	return nil
}
