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

// reEmail — намеренно простая проверка формата (не полная RFC 5322): один @,
// непустые локальная часть и домен, в домене есть точка. Локальная часть и
// домен также не пускают control-байты (\x00-\x1F, \x7F) — без этого NUL в
// email проходит формат-валидацию и падает уже на INSERT в Postgres как
// голый 500 вместо аккуратного 422 (ErrInvalidEmail).
var reEmail = regexp.MustCompile(`^[^@\s\x00-\x1F\x7F]+@[^@\s\x00-\x1F\x7F]+\.[^@\s\x00-\x1F\x7F]+$`)

// ValidEmailFormat — тот же формат-чек, что используют Register/CreateOAuthUser
// ниже, экспортирован для переиспользования в web-слое (email в форме
// приглашения, смена email в настройках организации), чтобы не заводить там
// собственную копию regex, рискующую разойтись с этой.
func ValidEmailFormat(email string) bool {
	return len(email) <= 254 && reEmail.MatchString(email)
}

// Service — аутентификация: пользователи и сессии.
type Service struct {
	pool *pgxpool.Pool

	// Secure — работает ли инстанс под HTTPS (BaseURL начинается с https://).
	// RA-L1: на secure=true RequireUser читает сессию ТОЛЬКО из префиксной
	// __Host--cookie. Проставляется из main.go после NewService; дефолт false
	// (читать оба имени) сохраняет обратную совместимость.
	Secure bool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Register создаёт пользователя и возвращает его id.
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
	// PROD-B1: первый пользователь инстанса становится инстанс-админом.
	// Флаг вычисляется атомарно в том же операторе через NOT EXISTS; при
	// гоночной первой регистрации вторую вставку с true отсечёт частичный
	// уникальный индекс one_instance_admin.
	var id int64
	err = s.pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, is_instance_admin)
		 VALUES ($1, $2, NOT EXISTS (SELECT 1 FROM users))
		 RETURNING id`,
		email, hash).Scan(&id)
	// RA-L6: 23505 приходит от двух разных индексов. Различаем по имени
	// констрейнта: unique(email) → email действительно занят; one_instance_admin
	// → мы проиграли гонку за первого админа (NOT EXISTS увидел пустую таблицу,
	// но другой запрос уже вставил админа). Во втором случае email свободен —
	// повторяем вставку уже без претензии на админский флаг.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if pgErr.ConstraintName == "one_instance_admin" {
			err = s.pool.QueryRow(ctx,
				`INSERT INTO users (email, password_hash, is_instance_admin)
				 VALUES ($1, $2, false)
				 RETURNING id`,
				email, hash).Scan(&id)
			// После ретрая 23505 может быть уже только по email.
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return 0, ErrEmailTaken
			}
			if err != nil {
				return 0, fmt.Errorf("auth: register: %w", err)
			}
			return id, nil
		}
		return 0, ErrEmailTaken
	}
	if err != nil {
		return 0, fmt.Errorf("auth: register: %w", err)
	}
	return id, nil
}

// UserCount возвращает число пользователей инстанса. Используется гейтингом
// регистрации (PROD-B1) для bootstrap первого админа.
func (s *Service) UserCount(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&n); err != nil {
		return 0, fmt.Errorf("auth: user count: %w", err)
	}
	return n, nil
}

// UserIsInstanceAdmin сообщает, является ли пользователь админом инстанса.
func (s *Service) UserIsInstanceAdmin(ctx context.Context, userID int64) (bool, error) {
	var admin bool
	err := s.pool.QueryRow(ctx,
		"SELECT is_instance_admin FROM users WHERE id = $1", userID).Scan(&admin)
	if err != nil {
		return false, fmt.Errorf("auth: instance admin flag: %w", err)
	}
	return admin, nil
}

// TransferInstanceAdmin передаёт роль администратора инстанса от fromUID
// пользователю с email toEmail (K7-1: единственный админ инстанса без этого
// метода не мог передать роль — назначить второго было негде ни в Store, ни
// в CLI, ни в UI). Одна транзакция: снять флаг у текущего (RowsAffected 0 —
// он не админ, ErrNotInstanceAdmin), поставить получателю; частичный UNIQUE
// one_instance_admin (0017_instance_admin) — страховка от гонки двух
// одновременных передач: второй commit упадёт на этом индексе, транзакция
// откатится, ошибка вернётся как есть.
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
	if _, err := tx.Exec(ctx, "UPDATE users SET is_instance_admin = true WHERE id = $1", toUID); err != nil {
		return 0, fmt.Errorf("transfer instance admin: grant: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("transfer instance admin: commit: %w", err)
	}
	return toUID, nil
}

// Authenticate возвращает id пользователя по email+паролю.
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

// UserEmails — то же, что UserEmail, но батчем по нескольким id одним
// запросом (WHERE id = ANY($1)): для страниц-списков, где email нужен на
// каждую строку (напр. автор заявки на странице выгрузок), — иначе N строк
// дают N запросов в PG на один рендер (ревью веб-части E1, п.5). Не
// найденные id в возвращаемой карте просто отсутствуют — вызывающий решает
// сам, как показывать «неизвестного» автора (тот же принцип, что и
// UserEmail: ошибка/отсутствие строки не паникует и не роняет страницу).
// Пустой ids возвращает пустую карту без похода в БД.
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

// ChangePassword проверяет старый пароль, валидирует новый по тем же
// правилам, что и Register, и обновляет хеш. Удаляет ВСЕ сессии
// пользователя (включая ту, из которой пришёл запрос) — вызывающий хендлер
// обязан выпустить новую сессию и переустановить cookie.
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

// dummyHash — валидная PHC-строка для выравнивания времени ответа
// при несуществующем email (защита от user enumeration по таймингу).
var dummyHash = func() string {
	h, err := HashPassword("dummy-timing-equalizer")
	if err != nil {
		panic(err)
	}
	return h
}()
