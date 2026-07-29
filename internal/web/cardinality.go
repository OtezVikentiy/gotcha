package web

import (
	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// cardinalityNotices собирает предупреждения о полях, по которым проект упёрся
// в потолок различных значений.
//
// Показывать их обязательно там, где человек смотрит на данные: увидев в списке
// «<cardinality-limit>» без объяснения, он не поймёт ни что произошло, ни что
// делать. Причина почти всегда не злонамеренная — в имя попал идентификатор, —
// и распознаётся она по ПРИМЕРАМ схлопнутых значений.
//
// Ограничитель живёт в памяти процесса приёма. В раздельном развёртывании
// (web и ingest на разных узлах) веб-узел его не видит и вернёт пусто —
// предупреждения тогда доступны только через /metrics и логи ingest-узла.
func (h *Handler) cardinalityNotices(projectID int64) []templates.CardinalityNotice {
	if h.Cardinality == nil {
		return nil
	}
	reports := h.Cardinality.Report(projectID)
	if len(reports) == 0 {
		return nil
	}
	out := make([]templates.CardinalityNotice, 0, len(reports))
	for _, r := range reports {
		out = append(out, templates.CardinalityNotice{
			Field:     ingest.FieldLabel(r.Field),
			Limit:     r.Limit,
			Collapsed: r.Collapsed,
			Samples:   r.Samples,
		})
	}
	return out
}
