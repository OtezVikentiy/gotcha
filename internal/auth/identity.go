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

	// Единственный админ не может самоудалиться, пока есть другие пользователи: передать роль некому.
	// Если он единственный — гейт не срабатывает, следующий зарегистрировавшийся станет админом.
	ErrInstanceAdminBlocked = errors.New("auth: instance admin cannot self-delete while other users exist")
)

// Сериализует Register с DeleteSelfAccount через pg_advisory_xact_lock(classid, 0):
// без него гонка регистрации/самоудаления может оставить инстанс совсем без администратора.
const instanceAdminBootstrapLockClass = 3

type Identity struct {
	Provider  string
	Subject   string
	Email     string
	CreatedAt time.Time
}

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

// Best-effort: если такой личности нет, ошибки не возвращается.
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

// email — citext на стороне БД, поиск case-insensitive.
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

// Провижининг разрешён только по инвайту — эту проверку делает вызывающий.
func (s *Service) CreateOAuthUser(ctx context.Context, email string) (int64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !ValidEmailFormat(email) {
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

// Каскадно удаляет и личности/членства (FK) — используется для отката висячего OAuth-юзера.
func (s *Service) DeleteUser(ctx context.Context, userID int64) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM users WHERE id = $1", userID); err != nil {
		return fmt.Errorf("auth: delete user: %w", err)
	}
	return nil
}

// Гейт и удаление — в одной транзакции с FOR UPDATE и общим с Register advisory-локом:
// иначе гонка удаления/регистрации может оставить инстанс без администратора.
func (s *Service) DeleteSelfAccount(ctx context.Context, userID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("auth: delete self account: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Лок держится до конца транзакции (снимается на COMMIT/ROLLBACK) — Register дождётся её итога.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, 0)", instanceAdminBootstrapLockClass); err != nil {
		return fmt.Errorf("auth: delete self account: bootstrap lock: %w", err)
	}

	var admin bool
	if err := tx.QueryRow(ctx,
		"SELECT is_instance_admin FROM users WHERE id = $1 FOR UPDATE", userID).Scan(&admin); err != nil {
		return fmt.Errorf("auth: delete self account: instance admin flag: %w", err)
	}
	if admin {
		var othersExist bool
		if err := tx.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM users WHERE id <> $1)", userID).Scan(&othersExist); err != nil {
			return fmt.Errorf("auth: delete self account: other users: %w", err)
		}
		if othersExist {
			return ErrInstanceAdminBlocked
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM users WHERE id = $1", userID); err != nil {
		return fmt.Errorf("auth: delete self account: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("auth: delete self account: commit: %w", err)
	}
	return nil
}
