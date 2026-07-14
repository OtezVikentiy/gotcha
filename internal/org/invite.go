package org

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// InviteTTL — срок жизни приглашения.
const InviteTTL = 7 * 24 * time.Hour

var (
	ErrInvalidRole   = errors.New("org: invite role must be admin or member")
	ErrInviteInvalid = errors.New("org: invite is invalid, expired or already used")
)

func inviteTokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Invite выпускает приглашение в организацию. Возвращает сырой токен
// (для письма); в БД хранится только его sha256-хеш.
func (s *Service) Invite(ctx context.Context, orgID int64, email string, role Role) (string, error) {
	if role != RoleAdmin && role != RoleMember {
		return "", ErrInvalidRole
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("org: invite token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	_, err := s.pool.Exec(ctx,
		"INSERT INTO org_invites (org_id, email, role, token_hash, expires_at) VALUES ($1, $2, $3, $4, $5)",
		orgID, email, role, inviteTokenHash(token), time.Now().Add(InviteTTL))
	if err != nil {
		return "", fmt.Errorf("org: invite: %w", err)
	}
	return token, nil
}

// AcceptInvite принимает приглашение токен-носителем: приглашение
// одноразовое, вход по токену (email — адрес доставки письма).
// Уже участнику роль не меняем — только гасим приглашение.
func (s *Service) AcceptInvite(ctx context.Context, token string, userID int64) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("org: accept invite: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var orgID int64
	var role Role
	err = tx.QueryRow(ctx, `
		UPDATE org_invites SET accepted_at = now()
		WHERE token_hash = $1 AND accepted_at IS NULL AND expires_at > now()
		RETURNING org_id, role`,
		inviteTokenHash(token)).Scan(&orgID, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrInviteInvalid
	}
	if err != nil {
		return 0, fmt.Errorf("org: accept invite: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, $3) ON CONFLICT (org_id, user_id) DO NOTHING",
		orgID, userID, role); err != nil {
		return 0, fmt.Errorf("org: accept invite: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("org: accept invite: %w", err)
	}
	return orgID, nil
}
