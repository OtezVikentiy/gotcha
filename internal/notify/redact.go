package notify

import (
	"context"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// No-op when token is empty — an empty needle would otherwise match everywhere.
// Shared by email.go and webhook.go to strip a channel's secret from its own send error.
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

// Enum закрыт — сюда обязан попасть каждый kind реестра (kind.go), сверяет
// internal/guards; ключи — те же константы реестра, не свои литералы.
var redactedKindKeys = map[string]string{
	// issue-алерты (alert.Evaluator)
	KindNewIssue:   "notify.issue.kind.new_issue",
	KindRegression: "notify.issue.kind.regression",
	KindSpike:      "notify.issue.kind.spike",
	// сводка подавленных уведомлений (alert.Digest)
	KindSuppressedDigest: "notify.redacted.kind.suppressed_digest",
	// метрические алерты (metric.Notifier)
	KindMetricAlertOpen:     "notify.redacted.kind.metric_alert_open",
	KindMetricAlertResolved: "notify.redacted.kind.metric_alert_resolved",
	// perf-находки (trace.Notifier)
	KindNPlusOne:    "perf.issues.kind.n_plus_one",
	KindSlowDBQuery: "perf.issues.kind.slow_db_query",
	KindHTTPFlood:   "perf.issues.kind.http_flood",
	// регрессии латентности (trace.RegressionNotifier)
	KindRegressionOpen:  "notify.redacted.kind.regression_open",
	KindRegressionClose: "notify.redacted.kind.regression_close",

	KindSLOBurnOpen:  "notify.redacted.kind.slo_burn_open",
	KindSLOBurnClose: "notify.redacted.kind.slo_burn_close",
	// регрессии профилей (profile.RegressionNotifier)
	KindProfileRegressionOpen:     "notify.redacted.kind.profile_regression_open",
	KindProfileRegressionResolved: "notify.redacted.kind.profile_regression_resolved",
	// аптайм (uptime.OutboxNotifier)
	KindDown:        "notify.redacted.kind.down",
	KindUp:          "notify.redacted.kind.up",
	KindSSLExpiring: "notify.redacted.kind.ssl_expiring",
	KindReminder:    "notify.redacted.kind.reminder",
	// встроенные инциденты хоста (host.HostNotifier)
	KindHostAlertOpen:     "notify.redacted.kind.host_alert_open",
	KindHostAlertResolved: "notify.redacted.kind.host_alert_resolved",
	// снятие хоста с наблюдения по ретенции (host.Retirer)
	KindHostRetired: "notify.redacted.kind.host_retired",
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
