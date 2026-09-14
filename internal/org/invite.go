package org

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const InviteTTL = 7 * 24 * time.Hour

// Потолок непринятых приглашений на организацию — без него накопление неотозванных
// приглашений не ограничено ничем, кроме частотного лимитера на вызывающей стороне.
const maxPendingInvitesPerOrg = 200

var (
	ErrInvalidRole           = errors.New("org: invite role must be admin or member")
	ErrInviteInvalid         = errors.New("org: invite is invalid, expired or already used")
	ErrInviteEmailMismatch   = errors.New("org: invite was issued for a different email")
	ErrTooManyPendingInvites = errors.New("org: too many pending invites")
)

func inviteTokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func (s *Service) Invite(ctx context.Context, orgID int64, email string, role Role) (string, error) {
	if role != RoleAdmin && role != RoleMember {
		return "", ErrInvalidRole
	}
	var pending int
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM org_invites WHERE org_id = $1 AND accepted_at IS NULL AND expires_at > now()",
		orgID).Scan(&pending); err != nil {
		return "", fmt.Errorf("org: count pending invites: %w", err)
	}
	if pending >= maxPendingInvitesPerOrg {
		return "", ErrTooManyPendingInvites
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

type PendingInvite struct {
	ID        int64
	Email     string
	Role      Role
	CreatedAt time.Time
	ExpiresAt time.Time
}

func (s *Service) PendingInvites(ctx context.Context, orgID int64) ([]PendingInvite, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, email, role, created_at, expires_at
		FROM org_invites
		WHERE org_id = $1 AND accepted_at IS NULL AND expires_at > now()
		ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, fmt.Errorf("org: pending invites: %w", err)
	}
	defer rows.Close()
	var out []PendingInvite
	for rows.Next() {
		var inv PendingInvite
		if err := rows.Scan(&inv.ID, &inv.Email, &inv.Role, &inv.CreatedAt, &inv.ExpiresAt); err != nil {
			return nil, fmt.Errorf("org: pending invites scan: %w", err)
		}
		out = append(out, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("org: pending invites: %w", err)
	}
	return out, nil
}

func (s *Service) RevokeInvite(ctx context.Context, orgID, inviteID int64) error {
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM org_invites WHERE id = $1 AND org_id = $2 AND accepted_at IS NULL",
		inviteID, orgID)
	if err != nil {
		return fmt.Errorf("org: revoke invite: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrInviteInvalid
	}
	return nil
}

func (s *Service) AcceptInvite(ctx context.Context, token string, userID int64, acceptingEmail string) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("org: accept invite: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var orgID int64
	var role Role
	var inviteEmail string
	err = tx.QueryRow(ctx, `
		UPDATE org_invites SET accepted_at = now()
		WHERE token_hash = $1 AND accepted_at IS NULL AND expires_at > now()
		RETURNING org_id, role, email`,
		inviteTokenHash(token)).Scan(&orgID, &role, &inviteEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrInviteInvalid
	}
	if err != nil {
		return 0, fmt.Errorf("org: accept invite: %w", err)
	}
	if !strings.EqualFold(inviteEmail, acceptingEmail) {
		return 0, ErrInviteEmailMismatch
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

type InviteInfo struct {
	OrgID   int64
	OrgName string
	Email   string
	Role    Role
}

func (s *Service) InviteByToken(ctx context.Context, token string) (InviteInfo, error) {
	var inv InviteInfo
	err := s.pool.QueryRow(ctx, `
		SELECT i.org_id, o.name, i.email, i.role
		FROM org_invites i
		JOIN organizations o ON o.id = i.org_id
		WHERE i.token_hash = $1 AND i.accepted_at IS NULL AND i.expires_at > now()`,
		inviteTokenHash(token)).Scan(&inv.OrgID, &inv.OrgName, &inv.Email, &inv.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return InviteInfo{}, ErrInviteInvalid
	}
	if err != nil {
		return InviteInfo{}, fmt.Errorf("org: invite by token: %w", err)
	}
	return inv, nil
}

func (s *Service) HasPendingInvite(ctx context.Context, email string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM org_invites
			WHERE email = $1 AND accepted_at IS NULL AND expires_at > now()
		)`, email).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("org: has pending invite: %w", err)
	}
	return exists, nil
}

func (s *Service) AcceptPendingInviteByEmail(ctx context.Context, email string, userID int64) (int64, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("org: accept invite by email: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var orgID int64
	var role Role
	err = tx.QueryRow(ctx, `
		UPDATE org_invites SET accepted_at = now()
		WHERE id = (
			SELECT id FROM org_invites
			WHERE email = $1 AND accepted_at IS NULL AND expires_at > now()
			ORDER BY created_at DESC
			LIMIT 1
		)
		AND accepted_at IS NULL
		RETURNING org_id, role`,
		email).Scan(&orgID, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("org: accept invite by email: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO org_members (org_id, user_id, role) VALUES ($1,$2,$3) ON CONFLICT (org_id, user_id) DO NOTHING",
		orgID, userID, role); err != nil {
		return 0, false, fmt.Errorf("org: accept invite by email: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("org: accept invite by email: %w", err)
	}
	return orgID, true, nil
}

func (s *Service) PurgeExpiredInvites(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM org_invites WHERE expires_at < now() OR accepted_at IS NOT NULL")
	if err != nil {
		return 0, fmt.Errorf("org: purge expired invites: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (s *Service) DeleteInvitesByEmail(ctx context.Context, email string) (int64, error) {
	tag, err := s.pool.Exec(ctx, "DELETE FROM org_invites WHERE email = $1", email)
	if err != nil {
		return 0, fmt.Errorf("org: delete invites by email: %w", err)
	}
	return tag.RowsAffected(), nil
}
