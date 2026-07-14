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
)

// reEmail — намеренно простая проверка формата (не полная RFC 5322):
// один @, непустые локальная часть и домен, в домене есть точка.
var reEmail = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// Service — аутентификация: пользователи и сессии.
type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Register создаёт пользователя и возвращает его id.
func (s *Service) Register(ctx context.Context, email, password string) (int64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if len(email) > 254 || !reEmail.MatchString(email) {
		return 0, ErrInvalidEmail
	}
	if len(password) < 8 || len(password) > 512 {
		return 0, ErrWeakPassword
	}
	hash, err := HashPassword(password)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id",
		email, hash).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return 0, ErrEmailTaken
	}
	if err != nil {
		return 0, fmt.Errorf("auth: register: %w", err)
	}
	return id, nil
}

// Authenticate возвращает id пользователя по email+паролю.
// Неизвестный email и неверный пароль неразличимы для вызывающего.
func (s *Service) Authenticate(ctx context.Context, email, password string) (int64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var id int64
	var hash string
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
	ok, err := VerifyPassword(password, hash)
	if err != nil {
		return 0, fmt.Errorf("auth: authenticate: %w", err)
	}
	if !ok {
		return 0, ErrInvalidCredentials
	}
	return id, nil
}

// UserEmail возвращает email пользователя по id — используется шапкой
// SSR-страниц (web.Handler.currentEmail) для отрисовки формы logout.
func (s *Service) UserEmail(ctx context.Context, userID int64) (string, error) {
	var email string
	err := s.pool.QueryRow(ctx,
		"SELECT email FROM users WHERE id = $1", userID).Scan(&email)
	if err != nil {
		return "", fmt.Errorf("auth: user email: %w", err)
	}
	return email, nil
}

// ChangePassword проверяет старый пароль, валидирует новый по тем же
// правилам, что и Register, и обновляет хеш. Удаляет ВСЕ сессии
// пользователя (включая ту, из которой пришёл запрос) — вызывающий хендлер
// обязан выпустить новую сессию и переустановить cookie.
func (s *Service) ChangePassword(ctx context.Context, userID int64, oldPassword, newPassword string) error {
	var hash string
	err := s.pool.QueryRow(ctx,
		"SELECT password_hash FROM users WHERE id = $1", userID).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidCredentials
	}
	if err != nil {
		return fmt.Errorf("auth: change password: %w", err)
	}
	ok, err := VerifyPassword(oldPassword, hash)
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

// dummyHash — валидная PHC-строка для выравнивания времени ответа
// при несуществующем email (защита от user enumeration по таймингу).
var dummyHash = func() string {
	h, err := HashPassword("dummy-timing-equalizer")
	if err != nil {
		panic(err)
	}
	return h
}()
