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
// писатели — recordLogFailure/clearLogFailure, вызываемые ИЗ LogStepChannels
// (step.go) — а не порознь по вызывающим: у SendStepIfDue (лесенка, шаги 1+
// пяти источников) и uptime.Detector.notifyOpen/retryStepZeroLog (шаг 0
// аптайма, W3-E — тот же механизм, не вторая копия) один и тот же путь
// записи/очистки, и clearLogFailure зовётся только когда LogStepChannels
// СНОВА позвали для той же тройки (успех или принудительный прогресс).
// Инцидент, подтверждённый или закрытый ДО следующего такого вызова
// (например: лог упал на 2-й из 5 попыток, затем оператор подтвердил —
// инцидент выпал из OpenUnacked/из-под уведомлений навсегда), оставляет
// строку осиротевшей без этой чистки — источник (incident_source) в строке
// тот же, что у любого из шести, отдельного фильтра здесь не нужно.
func PurgeOldEscalations(ctx context.Context, pool *pgxpool.Pool, olderThan time.Duration) (int64, error) {
	// olderThan <= 0 сдвинул бы cutoff в настоящее или будущее и удалил бы
	// ПРАКТИЧЕСКИ ВСЕ строки обеих таблиц одним махом (sent_at/last_attempt_at
	// любой существующей строки < now()). Сегодня Janitor не тикает с таким
	// Retention (main.go запускает его только при cfg.IncidentRetentionDays >
	// 0, симметрично entityRetention.Any()), но olderThan приходит из
	// конфига, а не константа — молчаливое стирание всего лога эскалаций из-за
	// будущей ошибки конфигурации/рефакторинга main.go недопустимо: гвард
	// ставится здесь, а не только на вызывающей стороне.
	if olderThan <= 0 {
		return 0, fmt.Errorf("escalation: purge old: olderThan must be positive, got %s", olderThan)
	}
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

	// Первый проход — сразу, не дожидаясь тика (как telemetry.EntityJanitor):
	// иначе после каждого рестарта чаще Interval (час по умолчанию)
	// incident_escalations/escalation_step_log_failures не чистятся вовсе.
	j.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.tick(ctx)
		}
	}
}

func (j *Janitor) tick(ctx context.Context) {
	n, err := PurgeOldEscalations(ctx, j.Pool, j.Retention)
	if err != nil {
		slog.Error("escalation janitor: purge failed", "error", err)
		return
	}
	if n > 0 {
		slog.Info("escalation janitor: purged old rows", "deleted", n)
	}
}
