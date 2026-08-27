package notify

import (
	"context"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// WithProjectSubject/WithProjectBody — единая обёртка темы/тела уведомления
// именем проекта (W3-E, кластер 4: «уведомления не называют проект» — один и
// тот же канал, подключённый к нескольким проектам, делает алерт
// неопознаваемым по одному числовому project_id). Общий контур доставки
// (escalation.Dispatch, полный payload) и RedactExternalPayload ниже
// (обезличенный payload для внешних каналов) вызывают ИМЕННО эти функции —
// единственная точка формата, чтобы оба пути не разъехались так же, как
// разъехались семь копий dispatch() до этой правки.
//
// project — Project.Name как он показан в UI (project.list.table.name),
// третьего формата (slug, "slug (name)") здесь намеренно нет.
func WithProjectSubject(ctx context.Context, subject, project string) string {
	return i18n.Tf(ctx, "notify.subject.with_project", "subject", subject, "project", project)
}

func WithProjectBody(ctx context.Context, body, project string) string {
	return i18n.Tf(ctx, "notify.body.with_project", "body", body, "project", project)
}
