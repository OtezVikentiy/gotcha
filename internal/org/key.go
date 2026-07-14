package org

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Key — DSN-ключ проекта: по public_key ingest узнаёт проект.
type Key struct {
	ID        int64
	ProjectID int64
	PublicKey string
	Revoked   bool
}

// CreateKey выпускает новый DSN-ключ проекта.
func (s *Service) CreateKey(ctx context.Context, projectID int64) (Key, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return Key{}, fmt.Errorf("org: key: %w", err)
	}
	k := Key{ProjectID: projectID, PublicKey: hex.EncodeToString(raw)}
	err := s.pool.QueryRow(ctx,
		"INSERT INTO project_keys (project_id, public_key) VALUES ($1, $2) RETURNING id",
		projectID, k.PublicKey).Scan(&k.ID)
	if err != nil {
		return Key{}, fmt.Errorf("org: create key: %w", err)
	}
	return k, nil
}

// RevokeKey отзывает ключ. Не идемпотентно: повторный вызов на уже
// отозванном ключе вернёт ErrNotFound.
func (s *Service) RevokeKey(ctx context.Context, keyID int64) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE project_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL", keyID)
	if err != nil {
		return fmt.Errorf("org: revoke key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// KeyByPublic возвращает живой (неотозванный) ключ по public_key.
// Горячий путь ingest — по нему аутентифицируется каждое событие.
func (s *Service) KeyByPublic(ctx context.Context, publicKey string) (Key, error) {
	k := Key{PublicKey: publicKey}
	err := s.pool.QueryRow(ctx,
		"SELECT id, project_id FROM project_keys WHERE public_key = $1 AND revoked_at IS NULL",
		publicKey).Scan(&k.ID, &k.ProjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	if err != nil {
		return Key{}, fmt.Errorf("org: key by public: %w", err)
	}
	return k, nil
}

// KeysForProject возвращает все ключи проекта, включая отозванные.
func (s *Service) KeysForProject(ctx context.Context, projectID int64) ([]Key, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT id, project_id, public_key, revoked_at IS NOT NULL FROM project_keys WHERE project_id = $1 ORDER BY id",
		projectID)
	if err != nil {
		return nil, fmt.Errorf("org: keys for project: %w", err)
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.ID, &k.ProjectID, &k.PublicKey, &k.Revoked); err != nil {
			return nil, fmt.Errorf("org: keys for project: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
