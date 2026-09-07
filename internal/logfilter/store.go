package logfilter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
)

var (
	// ErrNotFound — фильтра с таким id нет, либо он не виден вызывающему
	// (SetDefault на чужой личный фильтр).
	ErrNotFound = errors.New("logfilter: фильтр не найден")
	// ErrLimitReached — потолок личных или общих фильтров исчерпан
	// (см. maxPersonalPerUser/maxSharedPerProject).
	ErrLimitReached = errors.New("logfilter: лимит фильтров исчерпан")
	// ErrNameTaken — имя занято в той же области видимости (личной у
	// пользователя либо общей в проекте) без учёта регистра.
	ErrNameTaken = errors.New("logfilter: имя фильтра уже занято")
)

// filterColumns — список столбцов log_saved_filters в порядке, которого
// ждёт scanFilter. Общий для Get/Visible/Create(RETURNING) — там нет join,
// квалификация таблицей не нужна; Default джойнит с log_default_filters
// и квалифицирует столбцы отдельно (там же единственная коллизия имён —
// project_id есть в обеих таблицах).
const filterColumns = "id, project_id, owner_user_id, author_user_id, name, payload, created_at, updated_at"

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// rowScanner — общий интерфейс pgx.Row и pgx.Rows, достаточный для scanFilter.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanFilter разбирает строку log_saved_filters в Filter. Payload
// неизвестной версии (или битый JSON) не роняет чтение: фильтр возвращается
// с Applicable=false и пустыми Predicates — он показывается в списке
// с пояснением, но не применяется.
func scanFilter(row rowScanner) (Filter, error) {
	var f Filter
	var raw []byte
	if err := row.Scan(&f.ID, &f.ProjectID, &f.OwnerUserID, &f.AuthorUserID, &f.Name, &raw, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return Filter{}, err
	}
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil || p.V != payloadVersion {
		return f, nil
	}
	f.Applicable = true
	f.Predicates = p.Predicates
	return f, nil
}

// isUniqueViolation — true, если ошибка pgx — нарушение уникального индекса
// (SQLSTATE 23505). По образцу internal/slo/store.go.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

// Create заводит новый фильтр — личный (ownerUserID != nil) либо общий
// проекта (ownerUserID == nil). authorUserID — всегда конкретный
// пользователь, создавший запись; для общего фильтра он переживает удаление
// автора (ON DELETE SET NULL), для личного равен владельцу.
//
// Проверка лимита — COUNT в той же транзакции, что и вставка. Без
// advisory-локов из export.Store.EnqueueLimited: там они защищают гонку
// очереди заданий, а превышение лимита фильтров на единицу при гонке
// безвредно.
func (s *Store) Create(ctx context.Context, projectID int64, ownerUserID *int64, authorUserID int64, name string, preds []log.Predicate) (Filter, error) {
	if err := validateName(name); err != nil {
		return Filter{}, err
	}
	if err := validatePredicates(preds); err != nil {
		return Filter{}, err
	}
	preds = log.NormalizePredicates(preds)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Filter{}, fmt.Errorf("logfilter: create: begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op после успешного Commit

	var count int
	if ownerUserID != nil {
		if err := tx.QueryRow(ctx,
			"SELECT count(*) FROM log_saved_filters WHERE project_id = $1 AND owner_user_id = $2",
			projectID, *ownerUserID).Scan(&count); err != nil {
			return Filter{}, fmt.Errorf("logfilter: create: подсчёт личных: %w", err)
		}
		if count >= maxPersonalPerUser {
			return Filter{}, ErrLimitReached
		}
	} else {
		if err := tx.QueryRow(ctx,
			"SELECT count(*) FROM log_saved_filters WHERE project_id = $1 AND owner_user_id IS NULL",
			projectID).Scan(&count); err != nil {
			return Filter{}, fmt.Errorf("logfilter: create: подсчёт общих: %w", err)
		}
		if count >= maxSharedPerProject {
			return Filter{}, ErrLimitReached
		}
	}

	raw, err := json.Marshal(payload{V: payloadVersion, Predicates: preds})
	if err != nil {
		return Filter{}, fmt.Errorf("logfilter: create: сериализация payload: %w", err)
	}

	f, err := scanFilter(tx.QueryRow(ctx, `
		INSERT INTO log_saved_filters (project_id, owner_user_id, author_user_id, name, payload)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING `+filterColumns,
		projectID, ownerUserID, authorUserID, name, raw))
	if err != nil {
		if isUniqueViolation(err) {
			return Filter{}, ErrNameTaken
		}
		return Filter{}, fmt.Errorf("logfilter: create: вставка: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Filter{}, fmt.Errorf("logfilter: create: commit: %w", err)
	}
	return f, nil
}

// Update переписывает имя, условия и владельца фильтра. ownerUserID == nil
// повышает фильтр до общего, ownerUserID != nil делает его личным
// (в том числе понижает общий).
//
// Понижение общего фильтра до личного обязано снимать умолчание у ВСЕХ
// ОСТАЛЬНЫХ пользователей в той же транзакции: каскад по filter_id этого не
// сделает — запись фильтра не удаляется, она меняет владельца, а те, для
// кого фильтр стал невидим, не должны продолжать ссылаться на него как на
// умолчание.
func (s *Store) Update(ctx context.Context, id int64, name string, preds []log.Predicate, ownerUserID *int64) error {
	if err := validateName(name); err != nil {
		return err
	}
	if err := validatePredicates(preds); err != nil {
		return err
	}
	preds = log.NormalizePredicates(preds)

	raw, err := json.Marshal(payload{V: payloadVersion, Predicates: preds})
	if err != nil {
		return fmt.Errorf("logfilter: update: сериализация payload: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("logfilter: update: begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op после успешного Commit

	tag, err := tx.Exec(ctx, `
		UPDATE log_saved_filters
		SET name = $2, payload = $3, owner_user_id = $4, updated_at = now()
		WHERE id = $1`,
		id, name, raw, ownerUserID)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrNameTaken
		}
		return fmt.Errorf("logfilter: update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	if ownerUserID != nil {
		if _, err := tx.Exec(ctx,
			"DELETE FROM log_default_filters WHERE filter_id = $1 AND user_id <> $2",
			id, *ownerUserID); err != nil {
			return fmt.Errorf("logfilter: update: очистка умолчаний: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("logfilter: update: commit: %w", err)
	}
	return nil
}

// Delete удаляет фильтр. Умолчания на него у всех пользователей уходят
// каскадом (log_default_filters.filter_id ON DELETE CASCADE).
func (s *Store) Delete(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM log_saved_filters WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("logfilter: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Visible возвращает общие фильтры проекта плюс личные фильтры userID
// в этом проекте.
func (s *Store) Visible(ctx context.Context, projectID, userID int64) ([]Filter, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+filterColumns+`
		FROM log_saved_filters
		WHERE project_id = $1 AND (owner_user_id IS NULL OR owner_user_id = $2)
		ORDER BY (owner_user_id IS NOT NULL), lower(name)`,
		projectID, userID)
	if err != nil {
		return nil, fmt.Errorf("logfilter: visible: %w", err)
	}
	defer rows.Close()

	var out []Filter
	for rows.Next() {
		f, err := scanFilter(rows)
		if err != nil {
			return nil, fmt.Errorf("logfilter: visible: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Get возвращает фильтр по id либо ErrNotFound.
func (s *Store) Get(ctx context.Context, id int64) (Filter, error) {
	f, err := scanFilter(s.pool.QueryRow(ctx, "SELECT "+filterColumns+" FROM log_saved_filters WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Filter{}, ErrNotFound
	}
	if err != nil {
		return Filter{}, fmt.Errorf("logfilter: get: %w", err)
	}
	return f, nil
}

// SetDefault делает filterID умолчанием пользователя userID в проекте
// projectID. Отказывает ErrNotFound, если фильтр не из этого проекта или
// не виден пользователю (чужой личный фильтр) — умолчание не может
// указывать на то, что владелец не увидит в списке.
func (s *Store) SetDefault(ctx context.Context, projectID, userID, filterID int64) error {
	f, err := s.Get(ctx, filterID)
	if err != nil {
		return err
	}
	if f.ProjectID != projectID || !(f.Shared() || *f.OwnerUserID == userID) {
		return ErrNotFound
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO log_default_filters (project_id, user_id, filter_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (project_id, user_id) DO UPDATE SET filter_id = excluded.filter_id`,
		projectID, userID, filterID); err != nil {
		return fmt.Errorf("logfilter: set default: %w", err)
	}
	return nil
}

// ClearDefault снимает умолчание пользователя в проекте. Идемпотентно —
// отсутствие умолчания не ошибка.
func (s *Store) ClearDefault(ctx context.Context, projectID, userID int64) error {
	if _, err := s.pool.Exec(ctx,
		"DELETE FROM log_default_filters WHERE project_id = $1 AND user_id = $2",
		projectID, userID); err != nil {
		return fmt.Errorf("logfilter: clear default: %w", err)
	}
	return nil
}

// Default возвращает умолчание пользователя в проекте. ok=false, если
// умолчание не выставлено.
func (s *Store) Default(ctx context.Context, projectID, userID int64) (Filter, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT f.id, f.project_id, f.owner_user_id, f.author_user_id, f.name, f.payload, f.created_at, f.updated_at
		FROM log_saved_filters f
		JOIN log_default_filters d ON d.filter_id = f.id
		WHERE d.project_id = $1 AND d.user_id = $2`,
		projectID, userID)
	f, err := scanFilter(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Filter{}, false, nil
	}
	if err != nil {
		return Filter{}, false, fmt.Errorf("logfilter: default: %w", err)
	}
	return f, true, nil
}
