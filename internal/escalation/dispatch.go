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
// (миграция 0077) — общий хелпер для всех 5 нотифаеров (B4, T6), одна строка
// на (источник, инцидент, канал, шаг). Пишется ПОСЛЕ успешного Enqueue: лог
// отмечает то, что реально встало в очередь, а не намерение туда поставить —
// иначе провал доставки выглядел бы как отправленный шаг.
func LogStep(ctx context.Context, pool *pgxpool.Pool, source string, incidentID, channelID int64, step int) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step)
		VALUES ($1, $2, $3, $4)`, source, incidentID, channelID, step)
	if err != nil {
		return fmt.Errorf("escalation: log step: %w", err)
	}
	return nil
}
