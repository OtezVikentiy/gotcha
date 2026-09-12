package notify

import (
	"context"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// No-op when token is empty — an empty needle would otherwise match everywhere.
// Shared by email.go/webhook.go and web.alertDeliveriesPage (second redaction pass).
func RedactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "<redacted>")
}

// Default-deny whitelist for external channels when details are disabled:
// только маршрут/id/kind/url — не перечисленное (title, body, имена и т.п.) вырезается.
var externalSafeKeys = map[string]struct{}{
	// Секрета здесь нет намеренно — воркер достаёт его по channel_id в момент
	// отправки (см. SecretResolver), в payload он не попадает.
	"channel_kind": {},
	"target":       {},
	// Если адрес карточки сам несёт деталь (напр. имя хоста), нотифаер кладёт
	// "url_redacted" — им RedactExternalPayload заменяет "url" (не в этом списке).
	"kind": {},
	"url":  {},
	// ПРИНЯТЫЙ РИСК, не «не несёт ПДн»: оператор мог вписать чувствительное в имя
	// проекта, но без него канал на несколько проектов неотличим — риск меньше цены сокрытия.
	"project_name": {},
	// Числовые идентификаторы и счётчики: маршрутные, не несут текста ошибки.
	"project_id":       {},
	"issue_id":         {},
	"perf_issue_id":    {},
	"monitor_id":       {},
	"rule_id":          {},
	"times_seen":       {},
	"count":            {},
	"regression":       {},
	"duration_seconds": {},
	"days_left":        {},
}

// Enum закрыт — сюда обязан попасть каждый kind каждого нотифаера
// (см. TestRedactedKindLabelsCoverAllKinds).
var redactedKindKeys = map[string]string{
	// issue-алерты (alert.Evaluator)
	"new_issue":  "notify.issue.kind.new_issue",
	"regression": "notify.issue.kind.regression",
	"spike":      "notify.issue.kind.spike",
	// сводка подавленных уведомлений (alert.Digest)
	"suppressed_digest": "notify.redacted.kind.suppressed_digest",
	// метрические алерты (metric.Notifier)
	"metric_alert_open":     "notify.redacted.kind.metric_alert_open",
	"metric_alert_resolved": "notify.redacted.kind.metric_alert_resolved",
	// perf-находки (trace.Notifier)
	"n_plus_one":    "perf.issues.kind.n_plus_one",
	"slow_db_query": "perf.issues.kind.slow_db_query",
	"http_flood":    "perf.issues.kind.http_flood",
	// регрессии латентности (trace.RegressionNotifier)
	"regression_open":  "notify.redacted.kind.regression_open",
	"regression_close": "notify.redacted.kind.regression_close",

	"slo_burn_open":  "notify.redacted.kind.slo_burn_open",
	"slo_burn_close": "notify.redacted.kind.slo_burn_close",
	// регрессии профилей (profile.RegressionNotifier)
	"profile_regression_open":     "notify.redacted.kind.profile_regression_open",
	"profile_regression_resolved": "notify.redacted.kind.profile_regression_resolved",
	// аптайм (uptime.OutboxNotifier)
	"down":         "notify.redacted.kind.down",
	"up":           "notify.redacted.kind.up",
	"ssl_expiring": "notify.redacted.kind.ssl_expiring",
	"reminder":     "notify.redacted.kind.reminder",
	// встроенные инциденты хоста (host.HostNotifier)
	"host_alert_open":     "notify.redacted.kind.host_alert_open",
	"host_alert_resolved": "notify.redacted.kind.host_alert_resolved",
	// снятие хоста с наблюдения по ретенции (host.Retirer)
	"host_retired": "notify.redacted.kind.host_retired",
}

// Незнакомый вид уходит сырым enum'ом — честнее, чем прятать за пустой строкой.
func redactedKindLabel(ctx context.Context, kind string) string {
	if key, ok := redactedKindKeys[kind]; ok {
		return i18n.T(ctx, key)
	}
	return kind
}

// Оставляются только externalSafeKeys; subject/body переписываются маршрутным
// минимумом. payload["url_redacted"], если есть, подменяет "url" (для хостов).
func RedactExternalPayload(ctx context.Context, payload map[string]any) map[string]any {
	out := make(map[string]any, len(externalSafeKeys))
	for k, v := range payload {
		if _, ok := externalSafeKeys[k]; ok {
			out[k] = v
		}
	}
	kind, _ := out["kind"].(string)
	url, _ := out["url"].(string)
	if short, _ := payload["url_redacted"].(string); short != "" {
		url = short
		out["url"] = short
	}
	label := redactedKindLabel(ctx, kind)
	subject := i18n.Tf(ctx, "notify.redacted.subject", "kind", label)
	body := i18n.Tf(ctx, "notify.redacted.body", "kind", label, "url", url)
	// Переживает редакцию — иначе один канал на несколько проектов неотличим.
	if name, _ := out["project_name"].(string); name != "" {
		subject = WithProjectSubject(ctx, subject, name)
		body = WithProjectBody(ctx, body, name)
	}
	out["subject"] = subject
	out["body"] = body
	return out
}
