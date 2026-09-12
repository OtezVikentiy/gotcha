package auth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrEmailTaken         = errors.New("auth: email already registered")
	ErrWeakPassword       = errors.New("auth: password must be 8..512 characters")
	ErrInvalidCredentials = errors.New("auth: invalid email or password")
	ErrInvalidEmail       = errors.New("auth: invalid email")
	ErrNotInstanceAdmin   = errors.New("auth: user is not the instance admin")
	ErrSelfTransfer       = errors.New("auth: cannot transfer the instance admin role to yourself")
)

// Простой формат-чек (не RFC 5322): один @, непустые части, точка в домене, без control-байт —
// иначе NUL проходит валидацию и падает на INSERT в Postgres как голый 500 вместо 422.
var reEmail = regexp.MustCompile(`^[^@\s\x00-\x1F\x7F]+@[^@\s\x00-\x1F\x7F]+\.[^@\s\x00-\x1F\x7F]+$`)

// Экспортирован для переиспользования в web-слое — чтобы не заводить там свою копию regex.
func ValidEmailFormat(email string) bool {
	return len(email) <= 254 && reEmail.MatchString(email)
}

type Service struct {
	pool *pgxpool.Pool

	// Проставляется в main.go после NewService; дефолт false читает оба имени cookie для совместимости.
	Secure bool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

func (s *Service) Register(ctx context.Context, email, password string) (int64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !ValidEmailFormat(email) {
		return 0, ErrInvalidEmail
	}
	if len(password) < 8 || len(password) > 512 {
		return 0, ErrWeakPassword
	}
	hash, err := HashPassword(password)
	if err != nil {
		return 0, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("auth: register: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Тот же лок, что у DeleteSelfAccount — без него NOT EXISTS мог пропустить параллельное удаление
	// админа, и инстанс остался бы без администратора.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, 0)", instanceAdminBootstrapLockClass); err != nil {
		return 0, fmt.Errorf("auth: register: bootstrap lock: %w", err)
	}

	// Первый пользователь становится админом атомарно через NOT EXISTS; лок выше сериализует все
	// регистрации между собой, так что вторая честно видит уже закоммиченную первую.
	var id int64
	err = tx.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, is_instance_admin)
		 VALUES ($1, $2, NOT EXISTS (SELECT 1 FROM users))
		 RETURNING id`,
		email, hash).Scan(&id)
	// 23505 из двух разных индексов: email занят → ErrEmailTaken. Конфликт unique-индекса admin-флага
	// недостижим при исправной блокировке — это сигнал сломанного инварианта, а не штатная гонка.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if pgErr.ConstraintName == "one_instance_admin" {
			return 0, fmt.Errorf("auth: register: unexpected one_instance_admin conflict (bootstrap lock invariant broken): %w", err)
		}
		return 0, ErrEmailTaken
	}
	if err != nil {
		return 0, fmt.Errorf("auth: register: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("auth: register: commit: %w", err)
	}
	return id, nil
}

func (s *Service) UserCount(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&n); err != nil {
		return 0, fmt.Errorf("auth: user count: %w", err)
	}
	return n, nil
}

func (s *Service) UserIsInstanceAdmin(ctx context.Context, userID int64) (bool, error) {
	var admin bool
	err := s.pool.QueryRow(ctx,
		"SELECT is_instance_admin FROM users WHERE id = $1", userID).Scan(&admin)
	if err != nil {
		return false, fmt.Errorf("auth: instance admin flag: %w", err)
	}
	return admin, nil
}

// Одна транзакция: снимает флаг у текущего (иначе ErrNotInstanceAdmin), ставит получателю;
// UNIQUE one_instance_admin — страховка от гонки двух одновременных передач.
func (s *Service) TransferInstanceAdmin(ctx context.Context, fromUID int64, toEmail string) (int64, error) {
	toUID, err := s.UserByEmail(ctx, toEmail)
	if err != nil {
		return 0, err
	}
	if toUID == fromUID {
		return 0, ErrSelfTransfer
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("transfer instance admin: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx,
		"UPDATE users SET is_instance_admin = false WHERE id = $1 AND is_instance_admin", fromUID)
	if err != nil {
		return 0, fmt.Errorf("transfer instance admin: release: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, ErrNotInstanceAdmin
	}
	if err := grantInstanceAdmin(ctx, tx, toUID); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("transfer instance admin: commit: %w", err)
	}
	return toUID, nil
}

// RowsAffected 0 — получатель удалил аккаунт между UserByEmail и этим UPDATE: ошибка откатывает
// транзакцию (defer у вызывающего), прежний админ остаётся админом.
func grantInstanceAdmin(ctx context.Context, tx pgx.Tx, toUID int64) error {
	tag, err := tx.Exec(ctx, "UPDATE users SET is_instance_admin = true WHERE id = $1", toUID)
	if err != nil {
		return fmt.Errorf("transfer instance admin: grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// Неизвестный email и неверный пароль неразличимы для вызывающего.
func (s *Service) Authenticate(ctx context.Context, email, password string) (int64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var id int64
	var hash *string
	err := s.pool.QueryRow(ctx,
		"SELECT id, password_hash FROM users WHERE email = $1",
		email).Scan(&id, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		// Выравниваем время ответа: считаем хеш даже для несуществующего юзера.
		_, _ = VerifyPassword(password, dummyHash)
		return 0, ErrInvalidCredentials
	}
	if err != nil {
		return 0, fmt.Errorf("auth: authenticate: %w", err)
	}
	if hash == nil {
		// OAuth-only аккаунт: паролем войти нельзя. Выравниваем тайминг.
		_, _ = VerifyPassword(password, dummyHash)
		return 0, ErrInvalidCredentials
	}
	ok, err := VerifyPassword(password, *hash)
	if err != nil {
		return 0, fmt.Errorf("auth: authenticate: %w", err)
	}
	if !ok {
		return 0, ErrInvalidCredentials
	}
	return id, nil
}

func (s *Service) UserEmail(ctx context.Context, userID int64) (string, error) {
	var email string
	err := s.pool.QueryRow(ctx,
		"SELECT email FROM users WHERE id = $1", userID).Scan(&email)
	if err != nil {
		return "", fmt.Errorf("auth: user email: %w", err)
	}
	return email, nil
}

// Батчем по нескольким id (WHERE id = ANY($1)) — иначе N строк дают N отдельных запросов в PG.
// Не найденные id в карте просто отсутствуют — ошибка/пропуск не паникует и не роняет страницу.
func (s *Service) UserEmails(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		"SELECT id, email FROM users WHERE id = ANY($1)", ids)
	if err != nil {
		return nil, fmt.Errorf("auth: user emails: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var email string
		if err := rows.Scan(&id, &email); err != nil {
			return nil, fmt.Errorf("auth: user emails: scan: %w", err)
		}
		out[id] = email
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: user emails: %w", err)
	}
	return out, nil
}

// Удаляет ВСЕ сессии пользователя, включая текущую — вызывающий хендлер обязан выпустить новую
// сессию и переустановить cookie.
func (s *Service) ChangePassword(ctx context.Context, userID int64, oldPassword, newPassword string) error {
	var hash *string
	err := s.pool.QueryRow(ctx,
		"SELECT password_hash FROM users WHERE id = $1", userID).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidCredentials
	}
	if err != nil {
		return fmt.Errorf("auth: change password: %w", err)
	}
	if hash == nil {
		// Нет старого пароля — ChangePassword неприменим (нужен SetPassword).
		return ErrInvalidCredentials
	}
	ok, err := VerifyPassword(oldPassword, *hash)
	if err != nil {
		return fmt.Errorf("auth: change password: %w", err)
	}
	if !ok {
		return ErrInvalidCredentials
	}
	if len(newPassword) < 8 || len(newPassword) > 512 {
		return ErrWeakPassword
	}
	newHash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("auth: change password: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"UPDATE users SET password_hash = $2 WHERE id = $1", userID, newHash); err != nil {
		return fmt.Errorf("auth: change password: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"DELETE FROM sessions WHERE user_id = $1", userID); err != nil {
		return fmt.Errorf("auth: change password: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("auth: change password: %w", err)
	}
	return nil
}

// Выравнивает время ответа при несуществующем email — защита от user enumeration по таймингу.
var dummyHash = func() string {
	h, err := HashPassword("dummy-timing-equalizer")
	if err != nil {
		panic(err)
	}
	return h
}()
