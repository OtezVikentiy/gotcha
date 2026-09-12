package notify

import (
	"context"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// Единая точка формата для escalation.Dispatch и RedactExternalPayload —
// иначе оба пути разойдутся. project — Project.Name, не slug/"slug (name)".
func WithProjectSubject(ctx context.Context, subject, project string) string {
	return i18n.Tf(ctx, "notify.subject.with_project", "subject", subject, "project", project)
}

func WithProjectBody(ctx context.Context, body, project string) string {
	return i18n.Tf(ctx, "notify.body.with_project", "body", body, "project", project)
}
