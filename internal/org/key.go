package org

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Неизменяем: сменить тип выпущенного ключа нельзя, только выпустить новый и
// отозвать старый — поэтому кеш ingest.KeyCache не нуждается в инвалидации по типу.
type KeyKind string

const (
	KindBrowser KeyKind = "browser"
	KindServer  KeyKind = "server"
	KindAgent   KeyKind = "agent"
	KindLegacy  KeyKind = "legacy"
)

var ErrInvalidKeyKind = errors.New("org: invalid key kind")

func (k KeyKind) Valid() bool {
	switch k {
	case KindBrowser, KindServer, KindAgent, KindLegacy:
		return true
	}
	return false
}

type Key struct {
	ID        int64
	ProjectID int64
	OrgID     int64
	PublicKey string
	Kind      KeyKind
	Revoked   bool
}

func (s *Service) CreateKeys(ctx context.Context, projectID int64, kinds ...KeyKind) ([]Key, error) {
	if len(kinds) == 0 {
		return nil, fmt.Errorf("%w: no kinds given", ErrInvalidKeyKind)
	}
	keys := make([]Key, 0, len(kinds))
	args := make([]any, 0, 1+len(kinds)*2)
	args = append(args, projectID)
	var sb strings.Builder
	sb.WriteString("INSERT INTO project_keys (project_id, public_key, kind) VALUES ")
	for i, kind := range kinds {
		if !kind.Valid() {
			return nil, fmt.Errorf("%w: %q", ErrInvalidKeyKind, kind)
		}
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("org: key: %w", err)
		}
		k := Key{ProjectID: projectID, PublicKey: hex.EncodeToString(raw), Kind: kind}
		keys = append(keys, k)
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "($1, $%d, $%d)", len(args)+1, len(args)+2)
		args = append(args, k.PublicKey, string(kind))
	}
	sb.WriteString(" RETURNING id")
	rows, err := s.pool.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("org: create keys: %w", err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		if i >= len(keys) {
			return nil, fmt.Errorf("org: create keys: got more rows than requested")
		}
		if err := rows.Scan(&keys[i].ID); err != nil {
			return nil, fmt.Errorf("org: create keys: %w", err)
		}
		i++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("org: create keys: %w", err)
	}
	if i != len(keys) {
		return nil, fmt.Errorf("org: create keys: got %d ids for %d keys", i, len(keys))
	}
	return keys, nil
}

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

func (s *Service) KeyByPublic(ctx context.Context, publicKey string) (Key, error) {
	k := Key{PublicKey: publicKey}
	err := s.pool.QueryRow(ctx,
		"SELECT pk.id, pk.project_id, p.org_id, pk.kind FROM project_keys pk "+
			"JOIN projects p ON p.id = pk.project_id "+
			"WHERE pk.public_key = $1 AND pk.revoked_at IS NULL",
		publicKey).Scan(&k.ID, &k.ProjectID, &k.OrgID, &k.Kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	if err != nil {
		return Key{}, fmt.Errorf("org: key by public: %w", err)
	}
	return k, nil
}

func (s *Service) KeysForProject(ctx context.Context, projectID int64) ([]Key, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT id, project_id, public_key, kind, revoked_at IS NOT NULL FROM project_keys WHERE project_id = $1 ORDER BY id",
		projectID)
	if err != nil {
		return nil, fmt.Errorf("org: keys for project: %w", err)
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.ID, &k.ProjectID, &k.PublicKey, &k.Kind, &k.Revoked); err != nil {
			return nil, fmt.Errorf("org: keys for project: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
