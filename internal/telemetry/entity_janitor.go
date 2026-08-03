package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultEntityJanitorInterval — период прохода чистильщика сущностей. Час, как
// у notify.OutboxJanitor: срок хранения измеряется днями, и заглядывать чаще
// незачем.
const defaultEntityJanitorInterval = time.Hour

// entityBatchSize — сколько строк удаляется одним оператором.
//
// Удаление идёт батчами, а не одним DELETE на всё накопленное: единственная
// транзакция на срок хранения строк держала бы блокировки на таблицах, по
// которым в это же время идёт приём событий.
const entityBatchSize = 1000

// entityJanitorLockID — идентификатор advisory-лока прохода.
//
// Лок не нужен для корректности: DELETE идемпотентен, и две реплики,
// удаляющие одно и то же, друг другу не мешают. Он нужен, чтобы они не
// выполняли один тяжёлый проход одновременно, утроив нагрузку на базу в момент,
// когда она и так занята приёмом.
const entityJanitorLockID = 0x676F7463 // "gotc"

// entityRule — что и по какому признаку удаляется.
//
// Правила лежат таблицей, а не набором функций, потому что все они — одна и та
// же операция с разными именами колонок: перечисление читается целиком, и в нём
// видно, что открытое не удаляется нигде.
type entityRule struct {
	table string
	// ageColumn — колонка, по которой измеряется возраст строки.
	ageColumn string
	// closedOnly — дополнительное условие «уже закрыто».
	//
	// Открытый инцидент описывает то, что происходит сейчас, и его возраст не
	// значит ничего: удалить его означало бы сделать активную проблему
	// невидимой. Для issues условия нет намеренно — unresolved без событий за
	// весь срок хранения это не активная проблема, а мусор в списке.
	closedOnly string
	// retention — класс данных, чьим сроком живёт правило. Задаётся ЯВНО у
	// каждого правила; без него срок равен нулю и правило не выполняется вовсе
	// (см. retentionUnset и TestEntityRulesDeclareRetention).
	retention retentionKind
}

// retentionKind — к какому классу данных привязан срок жизни правила.
//
// Нулевое значение — retentionUnset, и это существенно: будь нулём какой-то
// настоящий класс, правило, добавленное без указания срока, молча унаследовало
// бы его — ровно тот дефект, из-за которого находка и возникла. Сторож
// TestEntityRulesDeclareRetention опирается именно на это.
type retentionKind int

const (
	retentionUnset retentionKind = iota
	retentionEvents
	retentionMetrics
	retentionProfiles
	retentionIncidents
)

// Retentions — сроки хранения по классам данных: GOTCHA_RETENTION_DAYS,
// GOTCHA_METRIC_RETENTION_DAYS, GOTCHA_PROFILE_RETENTION_DAYS,
// GOTCHA_INCIDENT_RETENTION_DAYS. Нулевой срок класса выключает удаление в его
// правилах.
type Retentions struct {
	Events    time.Duration
	Metrics   time.Duration
	Profiles  time.Duration
	Incidents time.Duration
}

// Any — задан ли хоть один срок. Ни одного — чистильщика запускать незачем.
func (r Retentions) Any() bool {
	return r.Events > 0 || r.Metrics > 0 || r.Profiles > 0 || r.Incidents > 0
}

func (r Retentions) forKind(k retentionKind) time.Duration {
	switch k {
	case retentionEvents:
		return r.Events
	case retentionMetrics:
		return r.Metrics
	case retentionProfiles:
		return r.Profiles
	case retentionIncidents:
		return r.Incidents
	default:
		return 0
	}
}

// entityRules — что и по какому сроку удаляется.
//
// Раньше все шесть правил жили одним GOTCHA_RETENTION_DAYS, хотя сроков в
// продукте четыре. Следствие несимметрично в обе стороны: регрессия профиля
// переживала свои сэмплы на восемьдесят три дня (карточка открывалась, а
// флеймграфа за ней уже не было), инцидент метрики переживал точки метрик на
// шестьдесят, а инциденты аптайма, наоборот, уменьшались вместе со сроком
// событий — при том что публичная статус-страница обещает историю за
// девяносто дней.
var entityRules = []entityRule{
	// issues и perf_issues описывают события и транзакции — тот же срок.
	{table: "issues", ageColumn: "last_seen", retention: retentionEvents},
	{table: "perf_issues", ageColumn: "last_seen", retention: retentionEvents},
	// Инцидент аптайма — единственная сущность без своей телеметрии в
	// ClickHouse: результаты проверок живут общим сроком, а сам инцидент
	// показывает публичная статус-страница. Поэтому срок свой, а не заимствованный.
	{table: "incidents", ageColumn: "resolved_at", closedOnly: "resolved_at IS NOT NULL", retention: retentionIncidents},
	{table: "perf_regressions", ageColumn: "resolved_at", closedOnly: "status = 'resolved' AND resolved_at IS NOT NULL", retention: retentionEvents},
	// Регрессия профиля без своих сэмплов — пустая карточка: срок профилей.
	{table: "profile_regressions", ageColumn: "resolved_at", closedOnly: "status = 'resolved' AND resolved_at IS NOT NULL", retention: retentionProfiles},
	// Инцидент метрики без своих точек — то же самое: срок метрик.
	{table: "metric_incidents", ageColumn: "resolved_at", closedOnly: "status = 'resolved' AND resolved_at IS NOT NULL", retention: retentionMetrics},
}

// EntityJanitor удаляет из PostgreSQL сущности, переживших срок хранения
// телеметрии.
//
// Существует потому, что GOTCHA_RETENTION_DAYS вытеснял данные только из
// ClickHouse, а issues, perf_issues, инциденты и регрессии в PostgreSQL жили
// вечно. Следствий три. Список проблем показывал группы, событий которых уже
// нет, с пожизненным счётчиком times_seen. Заголовок issue — свободный текст,
// прошедший только опциональный free-text-скраб, — переживал объявленный срок
// хранения, что расходится с остальной 152-ФЗ-дисциплиной продукта. И рост был
// неограничен, причём управлялся публичным ключом приёма: уникальный
// fingerprint на событие даёт строку issues на событие.
type EntityJanitor struct {
	Pool *pgxpool.Pool
	// Retention — сроки по классам данных: каждое правило живёт сроком своей
	// сущности, а не одним общим (см. entityRules).
	Retention Retentions

	// Interval — период прохода; 0 означает defaultEntityJanitorInterval.
	Interval time.Duration

	purged atomic.Int64
}

// Purged — сколько строк удалено за время жизни процесса. Для самотелеметрии:
// у каждого исчезновения данных должно быть число, которое можно посмотреть.
func (j *EntityJanitor) Purged() int64 { return j.purged.Load() }

// Run чистит сущности каждые Interval, пока не отменят ctx.
func (j *EntityJanitor) Run(ctx context.Context) {
	interval := j.Interval
	if interval <= 0 {
		interval = defaultEntityJanitorInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Первый проход — сразу: иначе после каждого рестарта чаще раза в час
	// чистка не выполняется вообще. Тот же класс дефекта, что был у суточной
	// проверки сертификатов.
	j.tickLogged(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.tickLogged(ctx)
		}
	}
}

func (j *EntityJanitor) tickLogged(ctx context.Context) {
	n, err := j.Tick(ctx)
	if err != nil {
		slog.Error("telemetry: entity janitor: purge failed", "error", err)
		return
	}
	if n > 0 {
		slog.Info("telemetry: entity janitor: purged expired entities", "deleted", n)
	}
}

// Tick выполняет один проход и возвращает число удалённых строк.
//
// Возвращает 0 без ошибки, если проход пропущен: либо срок хранения не задан,
// либо advisory-лок держит другая реплика.
func (j *EntityJanitor) Tick(ctx context.Context) (int64, error) {
	if !j.Retention.Any() {
		return 0, nil
	}

	conn, err := j.Pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("telemetry: entity janitor: acquire: %w", err)
	}
	defer conn.Release()

	// Лок берётся на соединении и держится до его освобождения: advisory-лок
	// сессионный, и взять его через пул без явного соединения нельзя — он
	// достался бы случайной сессии и снялся бы в неизвестный момент.
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", int64(entityJanitorLockID)).Scan(&locked); err != nil {
		return 0, fmt.Errorf("telemetry: entity janitor: lock: %w", err)
	}
	if !locked {
		// Проход идёт на другой реплике. Это нормальная работа, не сбой.
		return 0, nil
	}
	defer func() {
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", int64(entityJanitorLockID)); err != nil {
			slog.Warn("telemetry: entity janitor: unlock failed", "error", err)
		}
	}()

	var total int64
	for _, rule := range entityRules {
		retention := j.Retention.forKind(rule.retention)
		if retention <= 0 {
			// Срок класса не задан или выключен нулём — правило пропускается.
			continue
		}
		n, err := j.purgeTable(ctx, conn, rule, int(retention/time.Second))
		total += n
		if err != nil {
			// Одна таблица не должна отменять остальные: причина отказа обычно
			// локальна (блокировка, нехватка места), а данные в других таблицах
			// точно так же пережили срок хранения.
			slog.Error("telemetry: entity janitor: table purge failed",
				"table", rule.table, "deleted", n, "error", err)
			continue
		}
		if n > 0 {
			slog.Info("telemetry: entity janitor: table purged", "table", rule.table, "deleted", n)
		}
	}
	j.purged.Add(total)
	return total, nil
}

// execer — то, чем purgeTable пользуется от соединения. Узкий интерфейс вместо
// *pgxpool.Conn: удаление батчами проверяется без базы, а сам проход — с ней.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// purgeTable удаляет из одной таблицы всё, что старше срока, батчами.
func (j *EntityJanitor) purgeTable(ctx context.Context, conn execer, rule entityRule, cutoffSecs int) (int64, error) {
	where := rule.ageColumn + " < now() - make_interval(secs => $1)"
	if rule.closedOnly != "" {
		where = rule.closedOnly + " AND " + where
	}
	// Имена таблиц и колонок берутся из entityRules — литералов в коде, а не из
	// пользовательского ввода; срок хранения передаётся параметром.
	stmt := fmt.Sprintf(
		"DELETE FROM %s WHERE id IN (SELECT id FROM %s WHERE %s LIMIT %d)",
		rule.table, rule.table, where, entityBatchSize)

	var total int64
	for {
		tag, err := conn.Exec(ctx, stmt, cutoffSecs)
		if err != nil {
			return total, fmt.Errorf("telemetry: purge %s: %w", rule.table, err)
		}
		n := tag.RowsAffected()
		total += n
		if n < entityBatchSize {
			return total, nil
		}
		// Между батчами уступаем: проход не должен монополизировать базу, по
		// которой идёт приём.
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
	}
}
