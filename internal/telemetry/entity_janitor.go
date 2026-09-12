package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultEntityJanitorInterval = time.Hour

const entityBatchSize = 1000

// Не нужен для корректности (DELETE идемпотентен) — только чтобы реплики не дублировали проход.
const entityJanitorLockID = 0x676F7463 // "gotc"

type entityRule struct {
	table     string
	ageColumn string
	// Пусто, если сущность отражает открытую проблему — иначе удаление скроет активный инцидент.
	closedOnly string
	retention  retentionKind
}

// Нулевое значение — retentionUnset, а не настоящий класс: иначе забытый retention сработал бы молча.
type retentionKind int

const (
	retentionUnset retentionKind = iota
	retentionEvents
	retentionMetrics
	retentionProfiles
	retentionIncidents
	retentionDeployments
)

type Retentions struct {
	Events      time.Duration
	Metrics     time.Duration
	Profiles    time.Duration
	Incidents   time.Duration
	Deployments time.Duration
}

func (r Retentions) Any() bool {
	return r.Events > 0 || r.Metrics > 0 || r.Profiles > 0 || r.Incidents > 0 || r.Deployments > 0
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
	case retentionDeployments:
		return r.Deployments
	default:
		return 0
	}
}

var entityRules = []entityRule{
	{table: "issues", ageColumn: "last_seen", retention: retentionEvents},
	{table: "perf_issues", ageColumn: "last_seen", retention: retentionEvents},
	{table: "incidents", ageColumn: "resolved_at", closedOnly: "resolved_at IS NOT NULL", retention: retentionIncidents},
	{table: "perf_regressions", ageColumn: "resolved_at", closedOnly: "status = 'resolved' AND resolved_at IS NOT NULL", retention: retentionEvents},
	{table: "profile_regressions", ageColumn: "resolved_at", closedOnly: "status = 'resolved' AND resolved_at IS NOT NULL", retention: retentionProfiles},
	{table: "metric_incidents", ageColumn: "resolved_at", closedOnly: "status = 'resolved' AND resolved_at IS NOT NULL", retention: retentionMetrics},
	{table: "slo_incidents", ageColumn: "resolved_at", closedOnly: "status = 'resolved' AND resolved_at IS NOT NULL", retention: retentionMetrics},
	// closedOnly пуст намеренно: перед удалением батча хук preDeleteHosts закрывает открытые
	// инциденты хоста и уведомляет о снятии с наблюдения (см. PreDelete, host.Retirer).
	{table: "hosts", ageColumn: "last_seen", retention: retentionMetrics},
	{table: "host_incidents", ageColumn: "resolved_at", closedOnly: "status = 'resolved' AND resolved_at IS NOT NULL", retention: retentionMetrics},
	// TTL обязателен: таблицу пишет публичный ключ приёма, без границы она растёт вне квоты.
	{table: "deployments", ageColumn: "deployed_at", retention: retentionDeployments},
}

type EntityJanitor struct {
	Pool      *pgxpool.Pool
	Retention Retentions

	// На экземпляре, не в entityRule: entityRules — package-level var, общий для всех janitor'ов и тестов.
	PreDelete map[string]PreDeleteHook

	Interval time.Duration

	purged atomic.Int64
}

// Ошибка отменяет удаление батча до следующего прохода — хук обязан быть безопасен к повтору.
type PreDeleteHook func(ctx context.Context, ids []int64) error

func (j *EntityJanitor) Purged() int64 { return j.purged.Load() }

func (j *EntityJanitor) Run(ctx context.Context) {
	interval := j.Interval
	if interval <= 0 {
		interval = defaultEntityJanitorInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

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

func (j *EntityJanitor) Tick(ctx context.Context) (int64, error) {
	if !j.Retention.Any() {
		return 0, nil
	}

	conn, err := j.Pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("telemetry: entity janitor: acquire: %w", err)
	}
	defer conn.Release()

	// Advisory-лок сессионный: берётся на одном соединении и держится до его освобождения.
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
			continue
		}
		n, err := j.purgeTable(ctx, conn, rule, int(retention/time.Second))
		total += n
		if err != nil {
			// Ошибка одной таблицы не должна прерывать остальные: причины обычно локальны.
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

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func (j *EntityJanitor) purgeTable(ctx context.Context, conn execer, rule entityRule, cutoffSecs int) (int64, error) {
	where := rule.ageColumn + " < now() - make_interval(secs => $1)"
	if rule.closedOnly != "" {
		where = rule.closedOnly + " AND " + where
	}
	if hook := j.PreDelete[rule.table]; hook != nil {
		return j.purgeTableHooked(ctx, conn, rule, where, cutoffSecs, hook)
	}
	// %s тут — имена таблиц/колонок из литералов entityRules, не пользовательский ввод.
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
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
	}
}

// DELETE повторяет условие целиком, а не только id — строка могла перестать подходить, пока шёл хук.
func (j *EntityJanitor) purgeTableHooked(ctx context.Context, conn execer, rule entityRule, where string, cutoffSecs int, hook PreDeleteHook) (int64, error) {
	selectStmt := fmt.Sprintf("SELECT id FROM %s WHERE %s LIMIT %d", rule.table, where, entityBatchSize)
	deleteStmt := fmt.Sprintf("DELETE FROM %s WHERE id = ANY($2) AND %s", rule.table, where)

	var total int64
	for {
		rows, err := conn.Query(ctx, selectStmt, cutoffSecs)
		if err != nil {
			return total, fmt.Errorf("telemetry: purge %s: select batch: %w", rule.table, err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return total, fmt.Errorf("telemetry: purge %s: collect batch: %w", rule.table, err)
		}
		if len(ids) == 0 {
			return total, nil
		}
		if err := hook(ctx, ids); err != nil {
			return total, fmt.Errorf("telemetry: purge %s: pre-delete hook: %w", rule.table, err)
		}
		tag, err := conn.Exec(ctx, deleteStmt, cutoffSecs, ids)
		if err != nil {
			return total, fmt.Errorf("telemetry: purge %s: %w", rule.table, err)
		}
		total += tag.RowsAffected()
		// Выход по размеру выборки, не удалённых — строка могла перестать подходить и не удалиться.
		if len(ids) < entityBatchSize {
			return total, nil
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
	}
}
