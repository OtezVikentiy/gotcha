package escalation

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ContainsID сообщает, есть ли id в списке ids — общий фильтр «набор каналов»
// для dispatch пяти нотифаеров (B4, T6): channelIDs nil/пустой означает «все
// каналы» (проверяется до вызова этой функции вызывающим), непустой — фильтр
// по членству, применяемый ПОСЛЕ Deliverable-гейта.
func ContainsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// LogStep фиксирует отправку эскалационного уведомления в incident_escalations
// (миграция 0077) — общий хелпер для всех 6 нотифаеров (B4, T6, W2-C
// находка 2), одна строка на (источник, инцидент, канал, шаг). Пишется
// ПОСЛЕ успешного Enqueue: лог отмечает то, что реально встало в очередь, а
// не намерение туда поставить — иначе провал доставки выглядел бы как
// отправленный шаг.
//
// ON CONFLICT DO NOTHING на UNIQUE(source, incident, channel, step)
// (миграция 0085, W2-C находка 3): делает повторный вызов той же строки
// безопасным no-op'ом. Нужно для ретрая SendStepIfDue после краха процесса
// между логом и бампом — следующий тик заново шлёт notifyStep (см. её
// докблок, тот же осознанный trade-off, что и у тотального провала
// notifyStep) и заново логирует РЕАЛЬНО заенкенные каналы; без ON CONFLICT
// повтор упал бы на UNIQUE-нарушении там, где канал УЖЕ был залогирован
// предыдущей (прерванной) попыткой.
func LogStep(ctx context.Context, pool *pgxpool.Pool, source string, incidentID, channelID int64, step int) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (incident_source, incident_id, channel_id, step) DO NOTHING`, source, incidentID, channelID, step)
	if err != nil {
		return fmt.Errorf("escalation: log step: %w", err)
	}
	return nil
}

// recordLogFailure bumps the log-failure counter for (source, incidentID,
// step) in escalation_step_log_failures (миграция 0085, W2-C находка 3,
// условие 2 ревью) — the bound SendStepIfDue uses to stop a stuck LogStep
// from turning a blocked bump into a paging storm (see its docblock). One
// row per (source, incident, step): a channel-level failure still counts
// against the whole step, since a blocked bump holds back the WHOLE step,
// not just the one channel that failed to log.
func recordLogFailure(ctx context.Context, pool *pgxpool.Pool, source string, incidentID int64, step int) (attempts int, err error) {
	row := pool.QueryRow(ctx, `
		INSERT INTO escalation_step_log_failures (incident_source, incident_id, step, attempts, last_attempt_at)
		VALUES ($1, $2, $3, 1, now())
		ON CONFLICT (incident_source, incident_id, step)
		DO UPDATE SET attempts = escalation_step_log_failures.attempts + 1, last_attempt_at = now()
		RETURNING attempts`, source, incidentID, step)
	if err := row.Scan(&attempts); err != nil {
		return 0, fmt.Errorf("escalation: record log failure: %w", err)
	}
	return attempts, nil
}

// clearLogFailure сбрасывает счётчик провалов LogStep для (source,
// incidentID, step) — зовётся и после успешного логирования (счётчик больше
// не нужен), и после принудительного bump по границе попыток (см.
// SendStepIfDue): следующая ступень того же инцидента начинает с чистого
// счётчика. Best-effort: ошибка здесь не должна ронять основной путь —
// отсутствие сброса самое худшее приведёт к чуть более раннему
// принудительному бампу следующий раз, не к дыре или шторму.
func clearLogFailure(ctx context.Context, pool *pgxpool.Pool, source string, incidentID int64, step int) error {
	_, err := pool.Exec(ctx,
		"DELETE FROM escalation_step_log_failures WHERE incident_source = $1 AND incident_id = $2 AND step = $3",
		source, incidentID, step)
	if err != nil {
		return fmt.Errorf("escalation: clear log failure: %w", err)
	}
	return nil
}
