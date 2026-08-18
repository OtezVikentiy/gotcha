package log

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Query — чтение логов из ClickHouse. По образцу internal/trace/query.go:
// параметризованные запросы (значения только через ?, никогда не
// конкатенируются в текст), WHERE собирается конкатенацией строк-условий, а
// подставляемые значения идут отдельным срезом args в том же порядке.
type Query struct {
	conn driver.Conn
}

func NewQuery(conn driver.Conn) *Query {
	return &Query{conn: conn}
}

// LogRow — одна строка результата List. ProjectID не хранится: он известен
// из пути вызова (List принимает его отдельным параметром).
type LogRow struct {
	Timestamp      time.Time
	ObservedTS     time.Time
	Severity       string
	SeverityNumber uint8
	SeverityText   string
	Body           string
	TraceID        string
	SpanID         string
	LogAttributes  map[string]string
	ResourceAttrs  map[string]string
	Service        string
	Environment    string
}

// AttrFilter — фильтр по одному атрибуту записи: Resource=true проверяет
// resource_attrs, иначе — log_attributes (см. схему таблицы logs).
type AttrFilter struct {
	Resource bool
	Key      string
	Value    string
}

// ListFilter — параметры List.
type ListFilter struct {
	From, To time.Time

	Severity    []string
	Service     string
	Environment string
	Query       string // подстрока body, регистронезависимо
	Attrs       []AttrFilter

	// TraceID — жёсткий скоуп по trace_id (C3, logs in context): при непустом
	// добавляет "AND trace_id = ?" во ВСЕ запросы логов, включая подзапрос
	// AttrKeys (в отличие от прочих фильтров f, которые AttrKeys игнорирует, —
	// это осознанное отклонение: автокомплит ключей в контексте трейса обязан
	// быть по строкам этого трейса).
	TraceID string

	Limit int

	// Before/TieSkip — курсор пагинации keyset, см. List. TieSkip — сколько
	// строк с timestamp == Before ВСЕГО уже показано вызывающему. Если
	// хвостовой Before НЕ меняется между вызовами (тай тянется больше одной
	// страницы), TieSkip накапливается вызывающим (прибавляется), а не
	// пересчитывается заново по последней странице — иначе следующая
	// страница переспросит уже показанные строки этой тай-группы и вернёт
	// дубль. Референс правильной логики накопления — тестовый цикл
	// постраничного обхода в query_test.go.
	Before  time.Time
	TieSkip int
}

// FacetValue — значение фасета с числом вхождений. Тип объявлен здесь для
// будущего метода Facet (следующая задача); List его не использует.
type FacetValue struct {
	Value string
	Count int64
}

// defaultListLimit — сколько строк отдаёт List, если Limit не задан.
// maxListLimit — верхняя граница вне зависимости от запрошенного Limit:
// защита от случайно огромного значения из внешнего слоя (парсер запроса
// тоже клампит, но List не должен полагаться только на вызывающего).
const (
	defaultListLimit = 100
	maxListLimit     = 500
)

// List возвращает страницу логов проекта за [From, To), отфильтрованных по f,
// отсортированных newest-first, не более Limit штук (после клампа).
//
// Курсор пагинации (Before, TieSkip): timestamp в logs — DateTime64(3), на
// высоком rps у нескольких строк одна и та же миллисекунда. Строгое условие
// "timestamp < Before" на границе страницы теряет строки с тем же
// timestamp, что и последняя показанная (они физически могут идти ПОСЛЕ неё
// в результате следующего запроса и не соответствовать "<"); "timestamp <=
// Before" без поправки, наоборот, дублирует уже показанные. Вместо этого:
// условие "timestamp <= Before", лимит запроса увеличен на TieSkip
// (сколько строк с timestamp == Before уже показано предыдущей страницей),
// а после скана в Go среди строк с Timestamp == Before пропускаются первые
// TieSkip штук — это они и есть. Затем результат обрезается до Limit.
func (q *Query) List(ctx context.Context, projectID int64, f ListFilter) ([]LogRow, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	// toDateTime64(?, 3) + строковый аргумент вместо голого "timestamp >= ?"
	// с time.Time — обход бага биндинга clickhouse-go: позиционный "?"
	// (bindPositional в bind.go драйвера) форматирует ЛЮБОЙ time.Time
	// аргумент с жёстко зашитым TimeUnit=Seconds ("toDateTime('%d')" от
	// value.Unix()), то есть ВСЕГДА обрезает миллисекунды параметра до целой
	// секунды — независимо от реальной точности значения и от того, что
	// колонка timestamp объявлена DateTime64(3). Для полуоткрытого окна
	// [From, To) эта потеря безобидна (фуззи на <1с по краю окна, тот же
	// паттерн есть и в trace/metric/event query.go, там сравнение не
	// точечное). Но для Before — точечного курсора keyset-пагинации
	// ("timestamp <= Before" + постфильтр Timestamp.Equal(Before) в конце
	// функции) она РОНЯЕТ ровно ГРАНИЧНУЮ строку курсора всякий раз, когда её
	// миллисекунды не нулевые (то есть почти всегда на реальных данных):
	// секундное округление параметра делает Before МЕНЬШЕ фактического
	// значения строки, и "stored_ts <= truncated(Before)" ложно для самой
	// строки-границы — следующая страница «показать старее» теряет её молча.
	// chTimeArg форматирует время строкой с миллисекундами САМ (в обход
	// автоопределения типа драйвером — аргумент из time.Time становится
	// string), а toDateTime64(?, 3) в SQL кастует её обратно с нужной
	// точностью на стороне ClickHouse.
	where := "project_id = ? AND timestamp >= toDateTime64(?, 3) AND timestamp < toDateTime64(?, 3)"
	args := []any{uint64(projectID), chTimeArg(f.From), chTimeArg(f.To)}

	if len(f.Severity) > 0 {
		where += " AND severity IN (?)"
		args = append(args, f.Severity)
	}
	if f.Service != "" {
		where += " AND service = ?"
		args = append(args, f.Service)
	}
	if f.Environment != "" {
		where += " AND environment = ?"
		args = append(args, f.Environment)
	}
	if f.Query != "" {
		where += " AND positionCaseInsensitiveUTF8(body, ?) > 0"
		args = append(args, f.Query)
	}
	for _, a := range f.Attrs {
		col := "log_attributes"
		if a.Resource {
			col = "resource_attrs"
		}
		where += " AND " + col + "[?] = ?"
		args = append(args, a.Key, a.Value)
	}
	if f.TraceID != "" {
		where += " AND trace_id = ?"
		args = append(args, f.TraceID)
	}

	queryLimit := limit
	if !f.Before.IsZero() {
		// TieSkip приходит из URL (внешний слой) и ОБЯЗАН клампиться здесь, как
		// limit (List не полагается на вызывающего, см. maxListLimit выше): без
		// потолка queryLimit = limit + TieSkip снимает LIMIT и материализует всё
		// окно в память до обрезки out[:limit] — одиночный GET с гигантским
		// tskip кладёт мультитенантный процесс OOM. Легитимное накопление тай
		// крошечно (строки одной миллисекунды, при высоком rps единицы), так что
		// потолок maxListLimit с огромным запасом безопасен; отрицательный tskip
		// (дал бы отрицательный queryLimit → ошибка CH) обнуляется.
		if f.TieSkip < 0 {
			f.TieSkip = 0
		} else if f.TieSkip > maxListLimit {
			f.TieSkip = maxListLimit
		}
		where += " AND timestamp <= toDateTime64(?, 3)"
		args = append(args, chTimeArg(f.Before))
		queryLimit = limit + f.TieSkip
	}
	args = append(args, queryLimit)

	// Второй ключ сортировки обязателен для устойчивости курсора: "ORDER BY
	// timestamp DESC" без него не гарантирует ОДИНАКОВЫЙ относительный порядок
	// строк с равным timestamp между двумя разными запросами (страница 1 без
	// "AND timestamp <= ?" и страница 2 с ним — разные планы выполнения, CH
	// вправе перемешать тай по-своему). Без второго ключа TieSkip пропускает
	// не те строки и дублирует/теряет их на границе. cityHash64 — чистая
	// функция от значений самой строки, поэтому детерминирована между любыми
	// запросами по неизменным данным (строка в таблице не меняется между
	// вызовами); хэшируем ВСЕ 12 колонок результата, включая log_attributes/
	// resource_attrs через toString(Map) — иначе две строки, различающиеся
	// только атрибутами, схлопывались бы в один и тот же хэш. Коллизия
	// (а с ней риск дубля/потери одной строки на границе страницы) остаётся
	// только для строк, идентичных БУКВАЛЬНО по всем 12 колонкам в одну и ту
	// же миллисекунду — у logs нет уникального id, и такие строки неотличимы
	// друг от друга по содержимому, так что дубль/потеря одной из них не
	// заметны: контент на экране тот же.
	// SETTINGS max_execution_time = 20 (тот же литеральный приём, что у
	// Facet/AttrKeys/AttrValues ниже, только запас больше: список — единственный
	// из запросов логов, где полнотекст positionCaseInsensitiveUTF8 сочетается с
	// широким окном И это основной путь экрана, не вспомогательный фасет, так
	// что 20с вместо 5с). Без потолка тяжёлый запрос держит соединение до
	// дефолтных 60с общего CH-пула, которым делятся трейсы/метрики — долгий
	// список логов подвесил бы соседние разделы. При таймауте List вернёт
	// ошибку — logsList уже показывает её дружелюбно (loadFailed), а не 500-й.
	rows, err := q.conn.Query(ctx, `
		SELECT timestamp, observed_ts, severity, severity_number, severity_text,
			body, trace_id, span_id, log_attributes, resource_attrs, service, environment
		FROM logs
		WHERE `+where+`
		ORDER BY timestamp DESC,
			cityHash64(observed_ts, severity_number, severity_text, body, trace_id, span_id,
				toString(log_attributes), toString(resource_attrs), service, environment) DESC
		LIMIT ?
		SETTINGS max_execution_time = 20`, args...)
	if err != nil {
		return nil, fmt.Errorf("log: list: %w", err)
	}
	defer rows.Close()

	var out []LogRow
	for rows.Next() {
		var r LogRow
		if err := rows.Scan(
			&r.Timestamp, &r.ObservedTS, &r.Severity, &r.SeverityNumber, &r.SeverityText,
			&r.Body, &r.TraceID, &r.SpanID, &r.LogAttributes, &r.ResourceAttrs, &r.Service, &r.Environment,
		); err != nil {
			return nil, fmt.Errorf("log: list: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("log: list: %w", err)
	}

	if !f.Before.IsZero() && f.TieSkip > 0 {
		skip := 0
		for skip < len(out) && skip < f.TieSkip && out[skip].Timestamp.Equal(f.Before) {
			skip++
		}
		out = out[skip:]
	}
	if len(out) > limit {
		out = out[:limit]
	}

	return out, nil
}

// Histogram считает объём логов проекта на окне [f.From, f.To), разложенный
// по корзинам времени и severity (для stacked-графика T3). Корзин ровно
// buckets, ширина — размах окна / buckets, сетка выровнена по Unix epoch тем
// же приёмом, что EndpointLatency в trace/query.go (startUnix/endUnix через
// целочисленное деление на шаг). where — тот же набор условий, что у List
// (severity/service/environment/полнотекст/attrs), но БЕЗ курсора
// (Before/TieSkip) и БЕЗ LIMIT: гистограмма считает объём по ВСЕМУ окну, а не
// по одной странице.
//
// Результат уже пивотирован и добит нулями: series содержит ровно
// len(Severities) рядов (по одному на каждый канон severity, даже если в
// окне такого уровня вообще не было), каждый длиной len(times), пустая
// корзина — 0. GROUP BY t, severity в SQL отдаёт только НЕпустые пары
// (корзина, severity) — плоский пивот по всем 6 severity и добивка сеткой
// делаются здесь, в Go (fillSeries из trace/query.go заполняет один ряд, для
// stacked-графика по 6 severity нужен пивот, не переиспользуем его как есть).
func (q *Query) Histogram(ctx context.Context, projectID int64, f ListFilter, buckets int) ([]time.Time, map[string][]int64, error) {
	if buckets <= 0 {
		return nil, nil, fmt.Errorf("log: histogram: buckets must be positive, got %d", buckets)
	}

	stepSec := int64(f.To.Sub(f.From) / time.Duration(buckets) / time.Second)
	if stepSec < 1 {
		stepSec = 1
	}

	// where — 1:1 с List (см. её комментарий про chTimeArg/toDateTime64), но
	// без блока курсора и без LIMIT.
	where := "project_id = ? AND timestamp >= toDateTime64(?, 3) AND timestamp < toDateTime64(?, 3)"
	args := []any{stepSec, uint64(projectID), chTimeArg(f.From), chTimeArg(f.To)}

	if len(f.Severity) > 0 {
		where += " AND severity IN (?)"
		args = append(args, f.Severity)
	}
	if f.Service != "" {
		where += " AND service = ?"
		args = append(args, f.Service)
	}
	if f.Environment != "" {
		where += " AND environment = ?"
		args = append(args, f.Environment)
	}
	if f.Query != "" {
		where += " AND positionCaseInsensitiveUTF8(body, ?) > 0"
		args = append(args, f.Query)
	}
	for _, a := range f.Attrs {
		col := "log_attributes"
		if a.Resource {
			col = "resource_attrs"
		}
		where += " AND " + col + "[?] = ?"
		args = append(args, a.Key, a.Value)
	}
	if f.TraceID != "" {
		where += " AND trace_id = ?"
		args = append(args, f.TraceID)
	}

	// SETTINGS max_execution_time = 10 (тот же приём, что у List/Facet выше и
	// ниже) — Histogram считает по ВСЕМУ окну без LIMIT (см. докблок), поэтому
	// без потолка держит CH-соединение общего пула до дефолтных 60с. При
	// таймауте logsHistogram уже деградирует дружелюбно (Empty=true).
	rows, err := q.conn.Query(ctx, `
		SELECT toStartOfInterval(timestamp, INTERVAL ? second) AS t, severity, count() AS c
		FROM logs
		WHERE `+where+`
		GROUP BY t, severity
		ORDER BY t
		SETTINGS max_execution_time = 10`, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("log: histogram: %w", err)
	}
	defer rows.Close()

	byBucket := make(map[int64]map[string]int64)
	for rows.Next() {
		var t time.Time
		var sev string
		var c uint64
		if err := rows.Scan(&t, &sev, &c); err != nil {
			return nil, nil, fmt.Errorf("log: histogram: scan: %w", err)
		}
		bucketUnix := t.UTC().Unix()
		if byBucket[bucketUnix] == nil {
			byBucket[bucketUnix] = make(map[string]int64, len(Severities))
		}
		byBucket[bucketUnix][sev] += int64(c)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("log: histogram: %w", err)
	}

	// Сетка по Unix epoch — тот же приём, что EndpointLatency: последняя
	// корзина — та, что СОДЕРЖИТ момент f.To (не следующая за ним), иначе в
	// сетку попала бы корзина, для которой запрос физически не мог вернуть
	// данных (timestamp < f.To).
	fromUnix := f.From.UTC().Unix()
	toUnix := f.To.UTC().Unix()
	startUnix := (fromUnix / stepSec) * stepSec
	endUnix := ((toUnix - 1) / stepSec) * stepSec
	if endUnix < startUnix {
		endUnix = startUnix
	}

	n := int((endUnix-startUnix)/stepSec) + 1
	times := make([]time.Time, 0, n)
	series := make(map[string][]int64, len(Severities))
	for _, sev := range Severities {
		series[sev] = make([]int64, 0, n)
	}
	for cur := startUnix; cur <= endUnix; cur += stepSec {
		times = append(times, time.Unix(cur, 0).UTC())
		bucket := byBucket[cur] // nil-карта — все severity этой корзины нулевые
		for _, sev := range Severities {
			series[sev] = append(series[sev], bucket[sev])
		}
	}

	return times, series, nil
}

// facetColumns — whitelist имён колонок, допустимых в Facet: единственное
// место во всём файле, где текст SQL строится из параметра, а не из
// плейсхолдера ?. Значение параметра col сверяется с этой картой ДО того,
// как попасть в текст запроса (fmt.Errorf на отсутствии), поэтому подставить
// произвольную колонку (тем более что-то вроде "1=1 UNION ...") через него
// нельзя — конкатенации пользовательского ввода тут нет в принципе, только
// сверка с закрытым списком.
var facetColumns = map[string]bool{
	"severity":    true,
	"service":     true,
	"environment": true,
}

// facetLimit — сколько топ-значений отдаёт Facet (см. §4 спеки C2).
const facetLimit = 10

// Facet считает распределение значений колонки col (severity/service/
// environment — только они, facetColumns; иначе ошибка) в окне+фильтрах f,
// топ facetLimit по убыванию count. Курсор (Before/TieSkip) и Limit из f не
// применяются — фасет считает распределение по ВСЕМУ окну, а не по одной
// странице списка (тот же принцип, что и у Histogram).
//
// exclude-self: для col=="severity" собственное условие "severity IN (...)"
// в WHERE не добавляется — фасет обязан показывать распределение по ВСЕМ
// уровням, даже когда часть из них уже выбрана пользователем (иначе счётчик
// невыбранного уровня был бы всегда 0 — строки с этим уровнем уже отфильтрованы
// самим f.Severity — и клик по нему стал бы невозможен: он не сумел бы сузить
// список за пределы того, что уже видно). Для service/environment такой
// проблемы в MVP нет (одиночный select-фильтр, не мультивыбор, как у
// severity) — применяются ВСЕ фильтры, включая f.Severity, как в List.
//
// SETTINGS max_execution_time = 5 (литерал в тексте, НЕ плейсхолдер — тот же
// приём, что export.go) снижает пер-соединенческий дефолт 60с до 5с: тяжёлый
// фасет-запрос на большом окне обрывается быстро, а не вешает страницу —
// logsList при ошибке показывает конкретную секцию фасета пустой с пометкой,
// а не 500-т всю страницу.
func (q *Query) Facet(ctx context.Context, projectID int64, f ListFilter, col string) ([]FacetValue, error) {
	if !facetColumns[col] {
		return nil, fmt.Errorf("log: facet: column %q is not in the whitelist", col)
	}

	// where — тот же набор условий, что у List/Histogram (окно+прочие
	// фильтры), но БЕЗ курсора/LIMIT списка и без пустых значений самой
	// фасетной колонки (пустая строка — "атрибут не заполнен", отдельная
	// строка "" в топе только шумит).
	where := "project_id = ? AND timestamp >= toDateTime64(?, 3) AND timestamp < toDateTime64(?, 3) AND " + col + " != ''"
	args := []any{uint64(projectID), chTimeArg(f.From), chTimeArg(f.To)}

	if len(f.Severity) > 0 && col != "severity" {
		where += " AND severity IN (?)"
		args = append(args, f.Severity)
	}
	if f.Service != "" {
		where += " AND service = ?"
		args = append(args, f.Service)
	}
	if f.Environment != "" {
		where += " AND environment = ?"
		args = append(args, f.Environment)
	}
	if f.Query != "" {
		where += " AND positionCaseInsensitiveUTF8(body, ?) > 0"
		args = append(args, f.Query)
	}
	for _, a := range f.Attrs {
		attrCol := "log_attributes"
		if a.Resource {
			attrCol = "resource_attrs"
		}
		where += " AND " + attrCol + "[?] = ?"
		args = append(args, a.Key, a.Value)
	}
	if f.TraceID != "" {
		where += " AND trace_id = ?"
		args = append(args, f.TraceID)
	}
	args = append(args, facetLimit)

	rows, err := q.conn.Query(ctx, `
		SELECT `+col+`, count() AS c
		FROM logs
		WHERE `+where+`
		GROUP BY `+col+`
		ORDER BY c DESC
		LIMIT ?
		SETTINGS max_execution_time = 5`, args...)
	if err != nil {
		return nil, fmt.Errorf("log: facet: %w", err)
	}
	defer rows.Close()

	var out []FacetValue
	for rows.Next() {
		var v string
		var c uint64
		if err := rows.Scan(&v, &c); err != nil {
			return nil, fmt.Errorf("log: facet: scan: %w", err)
		}
		out = append(out, FacetValue{Value: v, Count: int64(c)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("log: facet: %w", err)
	}
	return out, nil
}

// attrKeysScanLimit — сколько последних (по timestamp) строк сканирует
// AttrKeys для обнаружения ключей (правка ревью IMPORTANT-1, §4 спеки C2):
// наивный "ARRAY JOIN mapKeys(log_attributes) GROUP BY key" по ВСЕМУ окну
// раскладывает каждую строку в N и на целевом трафике (150k rpm × 24ч ≈
// 200M+ строк) почти всегда обрывает SETTINGS max_execution_time=5 — тогда
// ядро-дифференциатор фичи (авто-фасеты по Map-атрибутам) молча пустует.
// Обнаружению ключей полная точность окна не нужна: ограниченная свежая
// выборка (50000 самых новых строк окна) даёт представительный набор ключей
// по предсказуемой и малой стоимости.
const attrKeysScanLimit = 50000

// AttrKeys возвращает топ ключей log_attributes по count() DESC в окне
// f.From/f.To — авто-обнаружение атрибут-фасетов (§4 спеки C2, ядро-
// дифференциатор: Grafana поверх Map-колонок так не умеет). Считается по
// ОГРАНИЧЕННОЙ свежей выборке (attrKeysScanLimit последних по времени
// строк), а не по всему окну — см. её комментарий. prefix, если не пустой,
// фильтрует ключи по префиксу (used автокомплитом T6); limit<=0 — facetLimit.
// Прочие фильтры f (severity/service/...) НЕ применяются: подзапрос сузил
// бы выборку ключей ещё сильнее, теряя редкие ключи ради точности, которая
// обнаружению ключей не нужна (ту же логику отражает spec §4 — только
// project_id+окно).
func (q *Query) AttrKeys(ctx context.Context, projectID int64, f ListFilter, prefix string, limit int) ([]FacetValue, error) {
	if limit <= 0 {
		limit = facetLimit
	}

	subWhere := "project_id = ? AND timestamp >= toDateTime64(?, 3) AND timestamp < toDateTime64(?, 3)"
	args := []any{uint64(projectID), chTimeArg(f.From), chTimeArg(f.To)}
	if f.TraceID != "" {
		// Единственный фильтр f, который AttrKeys чтит: trace_id — жёсткий скоуп
		// (контекст трейса), а не мягкий фасет (см. докблок метода).
		subWhere += " AND trace_id = ?"
		args = append(args, f.TraceID)
	}
	args = append(args, attrKeysScanLimit)

	query := `
		SELECT key, count() AS c
		FROM (
			SELECT log_attributes
			FROM logs
			WHERE ` + subWhere + `
			ORDER BY timestamp DESC
			LIMIT ?
		)
		ARRAY JOIN mapKeys(log_attributes) AS key`
	if prefix != "" {
		query += " WHERE key LIKE concat(?, '%')"
		args = append(args, prefix)
	}
	query += `
		GROUP BY key
		ORDER BY c DESC
		LIMIT ?
		SETTINGS max_execution_time = 5`
	args = append(args, limit)

	rows, err := q.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("log: attr keys: %w", err)
	}
	defer rows.Close()

	var out []FacetValue
	for rows.Next() {
		var v string
		var c uint64
		if err := rows.Scan(&v, &c); err != nil {
			return nil, fmt.Errorf("log: attr keys: scan: %w", err)
		}
		out = append(out, FacetValue{Value: v, Count: int64(c)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("log: attr keys: %w", err)
	}
	return out, nil
}

// AttrValues возвращает топ значений одного атрибута (log_attributes, либо
// resource_attrs при resource=true) по count() DESC в окне+фильтрах f — для
// РАСКРЫТОГО в UI ключа атрибут-фасета (§4 спеки C2), подгружается лениво
// (только для конкретного key по клику, не для всех ключей сразу). В
// отличие от AttrKeys считается по ВСЕМУ окну+фильтрам (тот же принцип, что
// Facet/Histogram) — раз ключ уже выбран, для него важна точность, не
// ограниченная выборка.
//
// mapContains(<map>, ?)-гард ОБЯЗАТЕЛЕН: `<map>[key]` в ClickHouse
// возвращает пустую строку для строки, где такого ключа вообще нет — без
// гарда такие строки молча склеились бы в один бакет с пустым значением
// вместе с реальными пустыми значениями атрибута, искажая counts. mapContains,
// а не has(mapKeys(...)) — не строит промежуточный массив ключей, дешевле.
func (q *Query) AttrValues(ctx context.Context, projectID int64, f ListFilter, resource bool, key string, limit int) ([]FacetValue, error) {
	if limit <= 0 {
		limit = facetLimit
	}
	col := "log_attributes"
	if resource {
		col = "resource_attrs"
	}

	// where — тот же набор условий, что у List/Facet (окно+ВСЕ фильтры,
	// включая f.Attrs — точечные фильтры по ДРУГИМ ключам продолжают сужать
	// выборку значений этого ключа).
	where := "project_id = ? AND timestamp >= toDateTime64(?, 3) AND timestamp < toDateTime64(?, 3)"
	whereArgs := []any{uint64(projectID), chTimeArg(f.From), chTimeArg(f.To)}

	if len(f.Severity) > 0 {
		where += " AND severity IN (?)"
		whereArgs = append(whereArgs, f.Severity)
	}
	if f.Service != "" {
		where += " AND service = ?"
		whereArgs = append(whereArgs, f.Service)
	}
	if f.Environment != "" {
		where += " AND environment = ?"
		whereArgs = append(whereArgs, f.Environment)
	}
	if f.Query != "" {
		where += " AND positionCaseInsensitiveUTF8(body, ?) > 0"
		whereArgs = append(whereArgs, f.Query)
	}
	for _, a := range f.Attrs {
		attrCol := "log_attributes"
		if a.Resource {
			attrCol = "resource_attrs"
		}
		where += " AND " + attrCol + "[?] = ?"
		whereArgs = append(whereArgs, a.Key, a.Value)
	}
	if f.TraceID != "" {
		where += " AND trace_id = ?"
		whereArgs = append(whereArgs, f.TraceID)
	}

	// Порядок args обязан идти 1:1 с порядком "?" в тексте запроса ниже:
	// сперва SELECT col[?] (key), затем where-условия, затем mapContains(col,
	// ?) (key ещё раз), затем LIMIT.
	args := make([]any, 0, len(whereArgs)+3)
	args = append(args, key)
	args = append(args, whereArgs...)
	args = append(args, key)
	args = append(args, limit)

	rows, err := q.conn.Query(ctx, `
		SELECT `+col+`[?] AS v, count() AS c
		FROM logs
		WHERE `+where+` AND mapContains(`+col+`, ?)
		GROUP BY v
		ORDER BY c DESC
		LIMIT ?
		SETTINGS max_execution_time = 5`, args...)
	if err != nil {
		return nil, fmt.Errorf("log: attr values: %w", err)
	}
	defer rows.Close()

	var out []FacetValue
	for rows.Next() {
		var v string
		var c uint64
		if err := rows.Scan(&v, &c); err != nil {
			return nil, fmt.Errorf("log: attr values: scan: %w", err)
		}
		out = append(out, FacetValue{Value: v, Count: int64(c)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("log: attr values: %w", err)
	}
	return out, nil
}

// chTimeArg форматирует t строкой с точностью до миллисекунды для
// toDateTime64(?, 3) в SQL — см. комментарий у сборки where в List: голый
// time.Time аргументом "?" драйвер clickhouse-go биндит с точностью только до
// секунды (bindPositional жёстко использует TimeUnit=Seconds), это теряет
// миллисекунды параметра молча. UTC — то же соглашение, что и у остальных
// временных сравнений продукта (сервер и хранилище работают в UTC).
func chTimeArg(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.000")
}
