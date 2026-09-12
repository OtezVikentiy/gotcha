package web

import (
	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// Ограничитель живёт в памяти процесса приёма: в раздельном развёртывании
// веб-узел его не видит и вернёт пусто.
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
