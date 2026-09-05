package profile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Query читает агрегаты профилей из profile_samples (аналог metric.Query).
type Query struct {
	conn driver.Conn
}

func NewQuery(conn driver.Conn) *Query { return &Query{conn: conn} }

// ServiceInfo — группа профилей (сервис/тип/транзакция).
//
// Weight — суммарный вес выборок (sum(value)), Unit — его единица измерения
// из pprof SampleType.Unit ('nanoseconds', 'bytes', 'count'). Единица берётся
// из данных, а не угадывается по имени типа профиля: для нестандартных типов
// догадка не работает. У строк, записанных до миграции 0012, единицы нет —
// тогда UI возвращается к прежней догадке по типу.
//
// Samples — число выборок. Раньше поле с этим именем несло sum(value), и
// колонка «Замеры» показывала 284000000 там, где имелось в виду 284 мс
// процессорного времени.
type ServiceInfo struct {
	Service      string
	Type         string
	Transaction  string
	Weight       uint64
	Unit         string
	Samples      uint64
	Environments []string
}

// ListServices возвращает группы профилей проекта за период (для обзора/фильтров).
func (q *Query) ListServices(ctx context.Context, projectID int64, environment string, from, to time.Time) ([]ServiceInfo, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT service, profile_type, transaction,
			sum(value) AS weight,
			-- Единица одна на группу (профиль одного типа), поэтому берём
			-- любую непустую: max() пропускает пустые строки старых записей.
			max(unit) AS unit,
			count() AS samples,
			arraySort(groupUniqArray(environment)) AS envs
		FROM profile_samples
		WHERE project_id = ? AND ts >= ? AND ts < ? AND (? = '' OR environment = ?)
		GROUP BY service, profile_type, transaction
		ORDER BY weight DESC
		LIMIT 200`,
		projectID, from, to, environment, environment)
	if err != nil {
		return nil, fmt.Errorf("profile: list services: %w", err)
	}
	defer rows.Close()
	var out []ServiceInfo
	for rows.Next() {
		var s ServiceInfo
		if err := rows.Scan(&s.Service, &s.Type, &s.Transaction, &s.Weight, &s.Unit, &s.Samples, &s.Environments); err != nil {
			return nil, fmt.Errorf("profile: list services scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// FlameNode — узел flamegraph-дерева.
type FlameNode struct {
	Name     string
	Value    uint64
	Children []*FlameNode
}

// maxFlameStacks — потолок числа уникальных стеков, из которых собирается
// flamegraph (строк GROUP BY stack в Flame/FlameForTrace).
//
// Откуда число. Уникальных стеков в профиле не больше, чем выборок: pprof CPU
// на 100 Гц за 30-секундный профиль даёт ≤ 3 000 выборок и обычно 500–2 000
// разных стеков, PHP/Excimer на своих частотах — меньше. За часовое окно
// непрерывного профилирования стеки в основном повторяются, и у нагруженного
// сервиса с несколькими инстансами набирается порядка 5–20 тысяч уникальных.
// 50 000 — запас в 2,5–10 раз над этим: на реальных окнах флеймграф остаётся
// полным, усечение включается только на аномально широких.
//
// Зачем потолок. Без него дерево строилось в памяти хендлера на неограниченном
// числе стеков (единственные запросы файла без LIMIT). С потолком верхняя
// граница — 50 000 стеков × ~30 кадров = 1,5 млн узлов в худшем случае без
// общих префиксов; реальные стеки префиксы делят, так что фактически десятки
// мегабайт.
//
// Усечение идёт по убыванию веса: отрезаются самые лёгкие стеки, то есть те,
// что на флеймграфе и так тоньше пикселя.
const maxFlameStacks = 50_000

// Flame агрегирует стеки за период + фильтры и строит flamegraph-дерево. Корень
// синтетический («all») с суммарным value; каждый стек прибавляется проходом
// корень→лист. Стеков не больше maxFlameStacks, самые тяжёлые; таймаут
// SETTINGS max_execution_time — тот же литеральный приём, что у raw-запросов
// trace (Dependencies).
func (q *Query) Flame(ctx context.Context, projectID int64, service, environment, profileType, transaction string, from, to time.Time) (*FlameNode, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT stack, sum(value) AS total
		FROM profile_samples
		WHERE project_id = ? AND profile_type = ? AND service = ?
		  AND (? = '' OR environment = ?) AND (? = '' OR transaction = ?)
		  AND ts >= ? AND ts < ?
		GROUP BY stack
		ORDER BY total DESC
		LIMIT ?
		SETTINGS max_execution_time = 10`,
		projectID, profileType, service, environment, environment, transaction, transaction, from, to, maxFlameStacks)
	if err != nil {
		return nil, fmt.Errorf("profile: flame: %w", err)
	}
	defer rows.Close()
	return buildFlame(rows)
}

// HasProfileForTrace сообщает, есть ли профиль, привязанный к trace_id
// (profiling-in-context, этап 8). Пустой traceID → false без запроса.
func (q *Query) HasProfileForTrace(ctx context.Context, projectID int64, traceID string) (bool, error) {
	if traceID == "" {
		return false, nil
	}
	// LIMIT 1 вместо count(): нужен только факт наличия, а count() читает все
	// гранулы (project_id,trace_id) — LIMIT 1 короткозамыкает на первой (запускается
	// на каждом рендере waterfall). Индекс на trace_id — миграция 0017.
	var one uint8
	err := q.conn.QueryRow(ctx,
		"SELECT 1 FROM profile_samples WHERE project_id = ? AND trace_id = ? LIMIT 1",
		projectID, traceID).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("profile: has profile for trace: %w", err)
	}
	return true, nil
}

// FlameForTrace строит flamegraph по всем профилям, привязанным к trace_id
// (без окна/сервиса/типа — trace_id сам ограничивает выборку). Потолок стеков
// и таймаут — те же, что у Flame.
func (q *Query) FlameForTrace(ctx context.Context, projectID int64, traceID string) (*FlameNode, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT stack, sum(value) AS total
		FROM profile_samples
		WHERE project_id = ? AND trace_id = ?
		GROUP BY stack
		ORDER BY total DESC
		LIMIT ?
		SETTINGS max_execution_time = 10`,
		projectID, traceID, maxFlameStacks)
	if err != nil {
		return nil, fmt.Errorf("profile: flame for trace: %w", err)
	}
	defer rows.Close()
	return buildFlame(rows)
}

// buildFlame собирает дерево из строк (stack Array(String), sum(value)). Корень
// синтетический («all»); каждый стек прибавляется проходом корень→лист.
//
// Детей ищем через индекс по имени, живущий только на время сборки: линейный
// перебор Children делал сборку квадратичной по ширине узла, и maxFlameStacks
// стеков под одним кадром (широкий «плоский» профиль) собирались 15 секунд.
func buildFlame(rows driver.Rows) (*FlameNode, error) {
	root := &FlameNode{Name: "all"}
	index := map[*FlameNode]map[string]*FlameNode{}
	child := func(n *FlameNode, name string) *FlameNode {
		kids := index[n]
		if c, ok := kids[name]; ok {
			return c
		}
		if kids == nil {
			kids = map[string]*FlameNode{}
			index[n] = kids
		}
		c := &FlameNode{Name: name}
		n.Children = append(n.Children, c)
		kids[name] = c
		return c
	}
	for rows.Next() {
		var stack []string
		var total uint64
		if err := rows.Scan(&stack, &total); err != nil {
			return nil, fmt.Errorf("profile: flame scan: %w", err)
		}
		root.Value += total
		node := root
		for _, name := range stack {
			node = child(node, name)
			node.Value += total
		}
	}
	return root, rows.Err()
}

// ServiceType — пара (сервис, тип профиля) с данными (для оценщика регрессий).
type ServiceType struct {
	Service string
	Type    string
}

// ServicesWithProfiles — пары (service, profile_type) с профилями за окно.
func (q *Query) ServicesWithProfiles(ctx context.Context, projectID int64, from, to time.Time) ([]ServiceType, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT DISTINCT service, profile_type FROM profile_samples
		WHERE project_id = ? AND ts >= ? AND ts < ?`,
		projectID, from, to)
	if err != nil {
		return nil, fmt.Errorf("profile: services with profiles: %w", err)
	}
	defer rows.Close()
	var out []ServiceType
	for rows.Next() {
		var st ServiceType
		if err := rows.Scan(&st.Service, &st.Type); err != nil {
			return nil, fmt.Errorf("profile: services with profiles scan: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ProjectService — сервис с профилями и проект, которому он принадлежит.
type ProjectService struct {
	ProjectID int64
	Service   string
	Type      string
}

// ActiveServices — все пары (проект, сервис, тип) с профилями за окно, одним
// запросом по всем проектам.
//
// Раньше оценщик регрессий брал список проектов из PostgreSQL и спрашивал
// ClickHouse про каждый: проект без единого профиля стоил ровно столько же,
// сколько нагруженный. Здесь работа определяется данными — если профилей нет,
// нет и запросов.
func (q *Query) ActiveServices(ctx context.Context, from, to time.Time) ([]ProjectService, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT DISTINCT project_id, service, profile_type FROM profile_samples
		WHERE ts >= ? AND ts < ?`, from, to)
	if err != nil {
		return nil, fmt.Errorf("profile: active services: %w", err)
	}
	defer rows.Close()
	var out []ProjectService
	for rows.Next() {
		var ps ProjectService
		var projectID uint64
		if err := rows.Scan(&projectID, &ps.Service, &ps.Type); err != nil {
			return nil, fmt.Errorf("profile: active services scan: %w", err)
		}
		ps.ProjectID = int64(projectID)
		out = append(out, ps)
	}
	return out, rows.Err()
}

// FunctionShare — доля функции в self-CPU сервиса за окно.
type FunctionShare struct {
	Function string
	// Share — self-CPU функции, делённое на весь self-CPU окна.
	Share float64
	// Samples — число строк (сэмплов) окна; по нему проверяется MinSamples.
	// Не вес: единица value зависит от типа профиля (для CPU — наносекунды),
	// и сумма весов за любое непустое окно легко перескакивает и сто, и сто
	// миллионов — гейт «мало данных» с ней не срабатывал бы никогда.
	Samples uint64
}

// TopFunctionShares — топ-K функций окна сразу с их долями.
//
// Один запрос вместо «топ-K, а потом доля каждой по отдельности». Прежний путь
// стоил 1 + 2K запросов на сервис, причём каждый второй — скан за весь период
// базовой линии; при K=20 это 41 обращение к самой тяжёлой таблице продукта за
// один тик одного сервиса.
//
// Итог окна считается оконной функцией по тем же группам: сумма self по всем
// функциям равна сумме value по строкам окна, потому что arrayElement(stack, -1)
// на пустом стеке даёт пустую строку — такая строка попадает в группу «», а не
// исчезает. Число строк окна (total_samples) считается тем же способом, но по
// count(), а не по sum(value): доля — по весу, гейт MinSamples — по числу
// сэмплов, единицы разные и путать их нельзя.
func (q *Query) TopFunctionShares(ctx context.Context, projectID int64, service, profileType string, from, to time.Time, k int) ([]FunctionShare, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT fn, self, total, total_samples FROM (
			SELECT arrayElement(stack, -1) AS fn,
			       sum(value) AS self,
			       sum(sum(value)) OVER () AS total,
			       sum(count()) OVER () AS total_samples
			FROM profile_samples
			WHERE project_id = ? AND service = ? AND profile_type = ? AND ts >= ? AND ts < ?
			GROUP BY fn
		)
		WHERE fn != '' AND total > 0
		ORDER BY self DESC
		LIMIT ?`,
		projectID, service, profileType, from, to, k)
	if err != nil {
		return nil, fmt.Errorf("profile: top function shares: %w", err)
	}
	defer rows.Close()
	var out []FunctionShare
	for rows.Next() {
		var fn string
		var self, total, totalSamples uint64
		if err := rows.Scan(&fn, &self, &total, &totalSamples); err != nil {
			return nil, fmt.Errorf("profile: top function shares scan: %w", err)
		}
		if total == 0 {
			continue
		}
		out = append(out, FunctionShare{Function: fn, Share: float64(self) / float64(total), Samples: totalSamples})
	}
	return out, rows.Err()
}

// BaselineShare — базовая линия одной функции.
type BaselineShare struct {
	// Share — медиана дневной self-доли функции за базовое окно.
	Share float64
	// Samples — число строк (сэмплов) именно этой функции за базовое окно
	// (не сумма её веса — единица value зависит от типа профиля); по нему
	// Decide гейтит открытие по MinSamples. Оконный итог здесь не годится:
	// свежее окно вложено в базовое, и оконный объём базы всегда не меньше
	// свежего — такой гейт не срабатывал бы никогда.
	Samples uint64
}

// BaselineFunctionShares — базовые линии перечисленных функций одним запросом.
// Функции, не встречавшейся в базовом окне, в карте нет (нулевое значение).
//
// Дневной итог считается по всем функциям дня (оконная функция с PARTITION BY
// по дню), а отбор нужных функций идёт снаружи: иначе доля считалась бы от
// самой себя.
func (q *Query) BaselineFunctionShares(ctx context.Context, projectID int64, service, profileType string, functions []string, baselineDays int, now time.Time) (map[string]BaselineShare, error) {
	out := make(map[string]BaselineShare, len(functions))
	if len(functions) == 0 {
		return out, nil
	}
	from := now.AddDate(0, 0, -baselineDays)
	rows, err := q.conn.Query(ctx, `
		SELECT fn, quantileExact(0.5)(share) AS median, sum(cnt) AS samples FROM (
			SELECT d, fn, self, cnt, self / day_total AS share FROM (
				SELECT toDate(ts) AS d,
				       arrayElement(stack, -1) AS fn,
				       sum(value) AS self,
				       count() AS cnt,
				       sum(sum(value)) OVER (PARTITION BY toDate(ts)) AS day_total
				FROM profile_samples
				WHERE project_id = ? AND service = ? AND profile_type = ? AND ts >= ? AND ts < ?
				GROUP BY d, fn
			)
			WHERE day_total > 0
		)
		WHERE fn IN ?
		GROUP BY fn`,
		projectID, service, profileType, from, now, functions)
	if err != nil {
		return nil, fmt.Errorf("profile: baseline function shares: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var fn string
		var b BaselineShare
		if err := rows.Scan(&fn, &b.Share, &b.Samples); err != nil {
			return nil, fmt.Errorf("profile: baseline function shares scan: %w", err)
		}
		out[fn] = b
	}
	return out, rows.Err()
}

// TopFunctionsBySelfShare — топ-K функций по свежему self-CPU (лист стека) за
// окно; кандидаты на проверку регрессии.
func (q *Query) TopFunctionsBySelfShare(ctx context.Context, projectID int64, service, profileType string, from, to time.Time, k int) ([]string, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT arrayElement(stack, -1) AS fn, sum(value) AS self
		FROM profile_samples
		WHERE project_id = ? AND service = ? AND profile_type = ? AND ts >= ? AND ts < ? AND length(stack) > 0
		GROUP BY fn ORDER BY self DESC LIMIT ?`,
		projectID, service, profileType, from, to, k)
	if err != nil {
		return nil, fmt.Errorf("profile: top functions: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var fn string
		var self uint64
		if err := rows.Scan(&fn, &self); err != nil {
			return nil, fmt.Errorf("profile: top functions scan: %w", err)
		}
		out = append(out, fn)
	}
	return out, rows.Err()
}
