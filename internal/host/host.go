// Package host — реестр имён хостов, видевших события/транзакции проекта
// (таблица hosts), и троттлер их регистрации на приёме (Toucher).
// Используется оценщиком по-хостовых метрик (задачи 5+) и UI-выбором хоста
// в фильтрах.
package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Host — строка таблицы hosts: имя хоста в рамках проекта и окно его
// видимости (первое и последнее событие с этим именем).
type Host struct {
	ID        int64
	ProjectID int64
	Name      string
	FirstSeen time.Time
	LastSeen  time.Time
}

// MaxHostsPerProject — потолок числа хостов ОДНОГО проекта. Единственной
// границей до него был гард кардинальности приёма (10 000 значений host.name в
// час со сбросом окна, то есть до сотен тысяч строк в сутки), а хост — не просто
// строка в PG: каждый умерший становится silent-инцидентом с уведомлением в
// каждый канал проекта и висит открытым до истечения ретенции. Эфемерные имена
// (поды k8s, машины автоскейла) превращали это в лавину писем и захлёб оценщика,
// который на каждый хост ходит в ClickHouse.
//
// 1000 — с запасом больше любого парка, который имеет смысл вести без
// группировки, и вдвое больше потолка строк на странице списка (hostsListLimit).
// Упор в потолок НЕ тихий: Upsert возвращает число отброшенных имён, Toucher
// пишет предупреждение в журнал и растит счётчик self-метрик — «часть хостов
// пропала» обязано быть отличимо от «их и не было» (тот же принцип, что у
// gotcha_cardinality_collapsed_total).
const MaxHostsPerProject = 1000

// MaxActiveHostsPerTick — потолок строк, которые оценщик забирает за один тик
// (ListActiveWithProject). Инсталляция на сотню проектов с полным
// MaxHostsPerProject у каждого укладывается сюда целиком; число нужно ровно
// затем, чтобы отказ дисциплины на приёме не превращался в неограниченную
// выборку в памяти узла оценки.
const MaxActiveHostsPerTick = 20_000

const hostColumns = `id, project_id, name, first_seen, last_seen`

// validName — годится ли имя к регистрации. Пустое отсекается защитно (см.
// Upsert), "." и ".." — потому что имя хоста едет в путь карточки
// /projects/{id}/hosts/{name}: url.PathEscape точки не экранирует, и браузер
// нормализует /hosts/.. в /projects/{id} ещё до запроса — хост с таким именем
// зарегистрировался бы, но открыть или удалить его через интерфейс было бы
// нечем.
func validName(name string) bool {
	return name != "" && name != "." && name != ".."
}

func scanHost(row pgx.Row) (Host, error) {
	var h Host
	err := row.Scan(&h.ID, &h.ProjectID, &h.Name, &h.FirstSeen, &h.LastSeen)
	return h, err
}

// Store — CRUD поверх таблицы hosts.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Upsert батчем регистрирует имена хостов проекта: новым — first_seen=now(),
// известным — last_seen=now(). Один запрос через unnest. Возвращает число
// НОВЫХ имён, отброшенных потолком MaxHostsPerProject (уже известные хосты
// продолжают обновлять last_seen и после упора в потолок — иначе парк,
// доросший до границы, целиком провалился бы в ложную тишину).
//
// names дедуплицируются ЗДЕСЬ (не полагаемся на вызывающих): ON CONFLICT DO
// UPDATE падает с «cannot affect row a second time», если в одном INSERT
// конфликтуют две строки с одинаковым (project_id, name) — а Toucher и
// ingest-парсер вполне могут прислать дубли в одном батче событий.
//
// Потолок применяется ОДНИМ оператором вместе со вставкой (CTE room считает
// свободные места, ветка новых имён режется LIMIT'ом) — это СУЖАЕТ окно гонки,
// но не закрывает его: count в CTE читается в снимке транзакции, поэтому
// параллельные батчи приёма видят одно и то же «свободно» и вместе способны
// перелить границу. Перебор ограничен числом одновременных батчей на их размер
// и разово: следующий оператор уже увидит заполненную таблицу.
//
// Жёсткая граница потребовала бы сериализуемой изоляции или advisory-lock на
// проект в горячем пути приёма — это цена, несоразмерная последствию (десяток
// лишних строк сверх тысячи, ни одна из которых ничего не ломает). Осознанный
// размен, а не недосмотр.
func (s *Store) Upsert(ctx context.Context, projectID int64, names []string) (int, error) {
	seen := make(map[string]struct{}, len(names))
	dedup := make([]string, 0, len(names))
	for _, n := range names {
		if !validName(n) {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		dedup = append(dedup, n)
	}
	if len(dedup) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `
		WITH input AS (
			SELECT DISTINCT n AS name FROM unnest($2::text[]) AS n
		),
		room AS (
			SELECT GREATEST($3::bigint - count(*), 0) AS free FROM hosts WHERE project_id = $1
		),
		allowed AS (
			SELECT i.name FROM input i
			 WHERE EXISTS (SELECT 1 FROM hosts h WHERE h.project_id = $1 AND h.name = i.name)
			UNION ALL
			(SELECT i.name FROM input i
			  WHERE NOT EXISTS (SELECT 1 FROM hosts h WHERE h.project_id = $1 AND h.name = i.name)
			  ORDER BY i.name
			  LIMIT (SELECT free FROM room))
		)
		INSERT INTO hosts (project_id, name)
		SELECT $1, name FROM allowed
		ON CONFLICT (project_id, name) DO UPDATE SET last_seen = now()`,
		projectID, dedup, MaxHostsPerProject)
	if err != nil {
		return 0, fmt.Errorf("host: upsert: %w", err)
	}
	// Каждое пропущенное потолком имя — ровно одна незатронутая строка: имена
	// уже дедуплицированы, а разрешённые дают либо INSERT, либо UPDATE.
	rejected := len(dedup) - int(tag.RowsAffected())
	if rejected < 0 {
		rejected = 0
	}
	return rejected, nil
}

// List возвращает хосты проекта, отсортированные по имени, не больше limit
// строк (limit <= 0 → MaxHostsPerProject). Потолок именно в SQL, а не срезом
// после выборки: страница списка всё равно показывает первые hostsListLimit
// строк, и вычитывать ради них весь реестр проекта незачем.
func (s *Store) List(ctx context.Context, projectID int64, limit int) ([]Host, error) {
	if limit <= 0 {
		limit = MaxHostsPerProject
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+hostColumns+" FROM hosts WHERE project_id = $1 ORDER BY name LIMIT $2", projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("host: list: %w", err)
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, fmt.Errorf("host: list scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Get возвращает хост проекта по имени. ok=false, если такого имени нет.
func (s *Store) Get(ctx context.Context, projectID int64, name string) (Host, bool, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+hostColumns+" FROM hosts WHERE project_id = $1 AND name = $2", projectID, name)
	h, err := scanHost(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Host{}, false, nil
	}
	if err != nil {
		return Host{}, false, fmt.Errorf("host: get: %w", err)
	}
	return h, true, nil
}

// ListByIDs возвращает хосты по идентификаторам, в произвольном порядке.
// Отсутствующие id молча пропускаются — вызывающий (Retirer) работает с
// батчем, который между выборкой и чтением мог поредеть.
func (s *Store) ListByIDs(ctx context.Context, ids []int64) ([]Host, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+hostColumns+" FROM hosts WHERE id = ANY($1)", ids)
	if err != nil {
		return nil, fmt.Errorf("host: list by ids: %w", err)
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, fmt.Errorf("host: list by ids scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Delete удаляет хост проекта по имени. ok=false, если такого имени уже нет
// (идемпотентно — не ошибка).
func (s *Store) Delete(ctx context.Context, projectID int64, name string) (bool, error) {
	row := s.pool.QueryRow(ctx,
		"DELETE FROM hosts WHERE project_id = $1 AND name = $2 RETURNING id", projectID, name)
	var id int64
	err := row.Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("host: delete: %w", err)
	}
	return true, nil
}

// ListActiveWithProject возвращает хосты ВСЕХ проектов, чей last_seen свежее
// freshWithin от текущего момента — вход для оценщика по-хостовых метрик
// (Task 11+), которому не важна конкретная страница/проект, а важно, какие
// хосты сейчас живы. Время окна вычисляет Go, не SQL (свой now() относительно
// caller-а, а не сервера) — как и в брифе.
//
// limit (<= 0 → MaxActiveHostsPerTick) — предохранитель на памяти оценщика:
// выборка едет в срез целиком. Порядок (project_id, name) детерминирован, то
// есть при упоре в потолок из оценки systematically выпадают одни и те же
// хвостовые проекты — поэтому вызывающий обязан жаловаться в журнал (см.
// Evaluator.Tick), а настоящая защита от разрастания — MaxHostsPerProject на
// приёме.
func (s *Store) ListActiveWithProject(ctx context.Context, freshWithin time.Duration, limit int) ([]Host, error) {
	if limit <= 0 {
		limit = MaxActiveHostsPerTick
	}
	since := time.Now().Add(-freshWithin)
	rows, err := s.pool.Query(ctx,
		"SELECT "+hostColumns+" FROM hosts WHERE last_seen > $1 ORDER BY project_id, name LIMIT $2", since, limit)
	if err != nil {
		return nil, fmt.Errorf("host: list active: %w", err)
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, fmt.Errorf("host: list active scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
