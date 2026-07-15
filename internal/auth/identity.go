package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrNoIdentity    = errors.New("auth: no such identity")
	ErrIdentityTaken = errors.New("auth: identity already linked to another account")
	ErrAlreadyLinked = errors.New("auth: account already has this provider linked")
	ErrUserNotFound  = errors.New("auth: no such user")
)

// Identity — привязка внешней личности к аккаунту (для страницы профиля).
type Identity struct {
	Provider  string
	Subject   string
	Email     string
	CreatedAt time.Time
}

// IdentityUser возвращает id аккаунта по (provider, subject); ErrNoIdentity —
// такой личности нет. Это горячий путь входа: матч по стабильному субъекту.
func (s *Service) IdentityUser(ctx context.Context, provider, subject string) (int64, error) {
	var uid int64
	err := s.pool.QueryRow(ctx,
		"SELECT user_id FROM user_identities WHERE provider = $1 AND subject = $2",
		provider, subject).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNoIdentity
	}
	if err != nil {
		return 0, fmt.Errorf("auth: identity user: %w", err)
	}
	return uid, nil
}

// LinkIdentity привязывает внешнюю личность к аккаунту. Конфликт по
// (provider,subject) — субъект уже за другим аккаунтом (ErrIdentityTaken);
// по (user_id,provider) — у аккаунта уже есть этот провайдер (ErrAlreadyLinked).
func (s *Service) LinkIdentity(ctx context.Context, userID int64, provider, subject, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	_, err := s.pool.Exec(ctx,
		"INSERT INTO user_identities (user_id, provider, subject, email) VALUES ($1,$2,$3,$4)",
		userID, provider, subject, email)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// Различаем PK (provider,subject) и UNIQUE (user_id,provider) по имени.
		if strings.Contains(pgErr.ConstraintName, "pkey") {
			return ErrIdentityTaken
		}
		return ErrAlreadyLinked
	}
	if err != nil {
		return fmt.Errorf("auth: link identity: %w", err)
	}
	return nil
}

// UpdateIdentityEmail обновляет сохранённый email личности (email у провайдера
// мог смениться со времени привязки). Best-effort: нет строки → без ошибки.
func (s *Service) UpdateIdentityEmail(ctx context.Context, provider, subject, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	_, err := s.pool.Exec(ctx,
		"UPDATE user_identities SET email = $3 WHERE provider = $1 AND subject = $2",
		provider, subject, email)
	if err != nil {
		return fmt.Errorf("auth: update identity email: %w", err)
	}
	return nil
}

// UnlinkIdentity удаляет привязку провайдера у аккаунта; нет строки → ErrNoIdentity.
func (s *Service) UnlinkIdentity(ctx context.Context, userID int64, provider string) error {
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM user_identities WHERE user_id = $1 AND provider = $2", userID, provider)
	if err != nil {
		return fmt.Errorf("auth: unlink identity: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoIdentity
	}
	return nil
}

// ListIdentities возвращает привязки аккаунта (для профиля), старейшие сверху.
func (s *Service) ListIdentities(ctx context.Context, userID int64) ([]Identity, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT provider, subject, COALESCE(email,''), created_at FROM user_identities WHERE user_id = $1 ORDER BY created_at",
		userID)
	if err != nil {
		return nil, fmt.Errorf("auth: list identities: %w", err)
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var id Identity
		if err := rows.Scan(&id.Provider, &id.Subject, &id.Email, &id.CreatedAt); err != nil {
			return nil, fmt.Errorf("auth: list identities scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UserByEmail ищет аккаунт по email (case-insensitive через citext);
// ErrUserNotFound — нет такого. Для неявной привязки по verified email.
func (s *Service) UserByEmail(ctx context.Context, email string) (int64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var uid int64
	err := s.pool.QueryRow(ctx,
		"SELECT id FROM users WHERE email = $1", email).Scan(&uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrUserNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("auth: user by email: %w", err)
	}
	return uid, nil
}

// CreateOAuthUser заводит аккаунт без пароля (OAuth-only); email занят →
// ErrEmailTaken. Провижининг разрешён только по инвайту (вызывающий проверяет).
func (s *Service) CreateOAuthUser(ctx context.Context, email string) (int64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if len(email) > 254 || !reEmail.MatchString(email) {
		return 0, ErrInvalidEmail
	}
	var id int64
	err := s.pool.QueryRow(ctx,
		"INSERT INTO users (email) VALUES ($1) RETURNING id", email).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return 0, ErrEmailTaken
	}
	if err != nil {
		return 0, fmt.Errorf("auth: create oauth user: %w", err)
	}
	return id, nil
}

// DeleteUser удаляет аккаунт (каскадом — его личности/членства). Используется
// для отката висячего OAuth-юзера, если инвайт исчез в гонке между проверкой и
// принятием.
func (s *Service) DeleteUser(ctx context.Context, userID int64) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM users WHERE id = $1", userID); err != nil {
		return fmt.Errorf("auth: delete user: %w", err)
	}
	return nil
}
