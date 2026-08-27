package escalation

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PurgeOldEscalations удаляет строки incident_escalations старше olderThan
// (по sent_at) и escalation_step_log_failures старше olderThan (по
// last_attempt_at), возвращает суммарное число удалённых строк. Гигиена
// (M-6): лог эскалаций растёт бесконечно, ретенция привязана к сроку
// хранения инцидентов (cfg.IncidentRetentionDays) — та же сущность, тот же
// срок жизни. Образец — notify.Outbox.PurgeOld (outbox.go:238).
//
// escalation_step_log_failures (миграция 0085, W2-C находка 3) заведена в
// эту же чистку, а не отдельным ретеншеном (ревью аудита 2026-08-27): без
// FK на incident_id (тот же приём, что у incident_escalations самой — 6
// разных источников в разных таблицах, общий FK невозможен) строка живёт
// сама по себе и НЕ исчезает вместе с инцидентом/проектом. Единственные её
// писатели — recordLogFailure/clearLogFailure внутри SendStepIfDue
// (step.go) — а clearLogFailure зовётся только когда SendStepIfDue СНОВА
// позвали для той же тройки (успех или принудительный бамп). Инцидент,
// подтверждённый или закрытый ДО следующего вызова (например: лог упал на
// 2-й из 5 попыток, затем оператор подтвердил — инцидент выпал из
// OpenUnacked навсегда), оставляет строку осиротевшей без этой чистки.
func PurgeOldEscalations(ctx context.Context, pool *pgxpool.Pool, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	tag, err := pool.Exec(ctx, "DELETE FROM incident_escalations WHERE sent_at < $1", cutoff)
	if err != nil {
		return 0, fmt.Errorf("escalation: purge old: %w", err)
	}
	failTag, err := pool.Exec(ctx, "DELETE FROM escalation_step_log_failures WHERE last_attempt_at < $1", cutoff)
	if err != nil {
		return 0, fmt.Errorf("escalation: purge old log failures: %w", err)
	}
	return tag.RowsAffected() + failTag.RowsAffected(), nil
}

// defaultJanitorInterval — период тика Janitor по умолчанию.
const defaultJanitorInterval = time.Hour

// Janitor периодически чистит incident_escalations и
// escalation_step_log_failures (см. PurgeOldEscalations).
// Образец — notify.OutboxJanitor (internal/notify/janitor.go).
type Janitor struct {
	Pool      *pgxpool.Pool
	Retention time.Duration // старше — удаляется; <= 0 выключает чистку
	Interval  time.Duration // период тика, дефолт 1 час
}

// Run тикает с Interval, на каждом тике зовёт PurgeOldEscalations(Retention).
// Ошибка логируется и не роняет цикл. Retention <= 0 — janitor не запускать
// (см. вызывающий код в main.go, симметрично entityRetention.Any()).
// Запускать как "go j.Run(ctx)".
func (j *Janitor) Run(ctx context.Context) {
	interval := j.Interval
	if interval <= 0 {
		interval = defaultJanitorInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := PurgeOldEscalations(ctx, j.Pool, j.Retention)
			if err != nil {
				slog.Error("escalation janitor: purge failed", "error", err)
				continue
			}
			if n > 0 {
				slog.Info("escalation janitor: purged old rows", "deleted", n)
			}
		}
	}
}
