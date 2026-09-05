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

	// ErrInstanceAdminBlocked — единственный администратор инстанса не может
	// удалить свой аккаунт, ПОКА на инстансе есть другие пользователи:
	// передать роль некому (TransferInstanceAdmin требует существующего
	// другого пользователя), и настройка SSO организаций осталась бы
	// недоступна никому (K7-1). Когда других пользователей нет — гейт не
	// срабатывает: запирать некого, а первый следующий зарегистрировавшийся
	// сам станет администратором (NOT EXISTS в Register, user.go:74).
	ErrInstanceAdminBlocked = errors.New("auth: instance admin cannot self-delete while other users exist")
)

// instanceAdminBootstrapLockClass — classID двухаргументной формы
// pg_advisory_xact_lock(classid, objid), сериализующей Register (user.go,
// вычисление is_instance_admin через NOT EXISTS) с DeleteSelfAccount ниже:
// обе стороны держат этот xact-лок (objID фиксирован — 0, лок один на весь
// инстанс, не per-сущность) до COMMIT/ROLLBACK своей транзакции, поэтому
// NOT EXISTS в Register больше не может увидеть ещё не закоммиченную строку
// админа, которого в этот же момент удаляет DeleteSelfAccount (хвост
// волны 1, T8: до этого лока такая гонка оставляла инстанс вовсе без
// администратора — Register получал is_instance_admin=false по устаревшему
// снапшоту, а старый админ тем временем удалялся).
//
// classID=3 — отдельно от enqueueLockClassProject/enqueueLockClassUser
// (export/store.go, 1 и 2): двухаргументная форма структурно не пересекает
// разные классы даже при совпадении objID, поэтому пересечение исключено
// независимо от выбранных чисел (тот же принцип, что там же).
const instanceAdminBootstrapLockClass = 3

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

// DeleteUser удаляет аккаунт (каскадом — его личности/членства). Используется
// для отката висячего OAuth-юзера, если инвайт исчез в гонке между проверкой и
// принятием.
func (s *Service) DeleteUser(ctx context.Context, userID int64) error {
	if _, err := s.pool.Exec(ctx, "DELETE FROM users WHERE id = $1", userID); err != nil {
		return fmt.Errorf("auth: delete user: %w", err)
	}
	return nil
}

// DeleteSelfAccount удаляет аккаунт при самоудалении (/profile/delete) — в
// отличие от DeleteUser (откат висячего OAuth-юзера), гейт «я — единственный
// администратор инстанса, а на инстансе есть другие пользователи» (K7-1,
// ErrInstanceAdminBlocked) и сам DELETE выполняются в ОДНОЙ транзакции, а не
// отдельным запросом до неё: иначе между чтением флага в веб-слое и удалением
// остаётся окно гонки (устранение находки I1 волны 1). `FOR UPDATE` на
// собственной строке закрывает гонку с конкурентной передачей роли
// (TransferInstanceAdmin меняет ровно эту строку); с конкурентной
// РЕГИСТРАЦИЕЙ второго пользователя, случившейся строго между SELECT и
// COMMIT этой транзакции, закрывает instanceAdminBootstrapLockClass — общий
// с Register xact-лок (см. его докблок выше): без него Register мог
// вычислить NOT EXISTS по ещё не удалённой строке этого пользователя и не
// стать админом, хотя после COMMIT этой транзакции инстанс оказывался пуст
// (хвост волны 1, T8 — до этого лока инстанс оставался вовсе без
// администратора).
func (s *Service) DeleteSelfAccount(ctx context.Context, userID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("auth: delete self account: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Лок держится до конца транзакции (Commit/Rollback снимает его
	// автоматически) — Register не начнёт вычислять свой NOT EXISTS раньше,
	// чем эта транзакция определится с судьбой строки userID.
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
