package notify

import (
	"context"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// externalSafeKeys — «белый список» полей payload, которые разрешено
// раскрывать во внешние каналы (Telegram/webhook) при выключенном
// GOTCHA_EXTERNAL_CHANNEL_DETAILS. Только маршрут доставки, числовые
// идентификаторы/счётчики, вид алерта и ссылка на карточку — всё, что не
// несёт текста ошибки, имён транзакций/функций и потенциальных ПДн.
//
// Список — «default deny»: любое НЕ перечисленное здесь поле (title,
// culprit, body, subject, monitor_name, target_name, metric, function,
// service, cause, значения метрик и т.п.) вырезается. Так новое поле в
// payload любого нотифаера по умолчанию НЕ утечёт за пределы РФ, пока его
// осознанно не признают безопасным здесь.
var externalSafeKeys = map[string]struct{}{
	// Маршрут доставки — читает notify.Worker, чтобы собрать notify.Target
	// (channel_kind/target). Без них воркер не доставит сообщение. Секрета
	// здесь нет намеренно: воркер достаёт его по channel_id в момент отправки
	// (см. notify.SecretResolver), а в payload он не попадает вовсе.
	"channel_kind": {},
	"target":       {},
	// Вид алерта и ссылка на карточку — безопасный обезличенный минимум.
	"kind": {},
	"url":  {},
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

// redactedKindKeys — ключ каталога i18n с человекочитаемой подписью вида
// алерта для обезличенного сообщения. Enum закрыт: сюда обязан попасть
// каждый kind каждого нотифаера (см. TestRedactedKindLabelsCoverAllKinds).
// Где каноничная подпись уже есть у самого нотифаера — переиспользуется его
// ключ, а не заводится дубль.
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
	// регрессии профилей (profile.RegressionNotifier)
	"profile_regression_open":     "notify.redacted.kind.profile_regression_open",
	"profile_regression_resolved": "notify.redacted.kind.profile_regression_resolved",
	// аптайм (uptime.OutboxNotifier)
	"down":         "notify.redacted.kind.down",
	"up":           "notify.redacted.kind.up",
	"ssl_expiring": "notify.redacted.kind.ssl_expiring",
	"reminder":     "notify.redacted.kind.reminder",
}

// redactedKindLabel — подпись вида алерта для обезличенной темы/тела.
// Незнакомый вид уходит сырым enum'ом — это честнее, чем прятать его за
// пустой строкой (тот же принцип, что issueAlertKindLabel в internal/alert).
func redactedKindLabel(ctx context.Context, kind string) string {
	if key, ok := redactedKindKeys[kind]; ok {
		return i18n.T(ctx, key)
	}
	return kind
}

// RedactExternalPayload возвращает обезличенную копию payload для доставки во
// внешние каналы (Telegram/webhook), когда оператор выключил раскрытие
// деталей (GOTCHA_EXTERNAL_CHANNEL_DETAILS=false). Текст ошибки, имена
// транзакций/функций и тело уведомления могут нести ПДн, а Telegram/webhook
// уводят их за пределы РФ (152-ФЗ), поэтому наружу отдаётся только маршрут
// доставки, ссылка на карточку и вид алерта.
//
// Оставляются лишь поля из externalSafeKeys; subject/body перезаписываются
// маршрутным минимумом («[Gotcha] {вид алерта}» и «{вид алерта}\n\n{url}»),
// чтобы у Telegram (берёт текст из body) и webhook (сериализует весь payload)
// не осталось исходных деталей. Подпись вида — человекочитаемая, на языке
// инстанса: ctx обязан нести локаль уведомлений (i18n.WithLocale с
// GOTCHA_LOCALE — тот же ctx, которым нотифаер строил исходные тексты).
// Исходный payload не мутируется — возвращается новая map.
func RedactExternalPayload(ctx context.Context, payload map[string]any) map[string]any {
	out := make(map[string]any, len(externalSafeKeys))
	for k, v := range payload {
		if _, ok := externalSafeKeys[k]; ok {
			out[k] = v
		}
	}
	kind, _ := out["kind"].(string)
	url, _ := out["url"].(string)
	label := redactedKindLabel(ctx, kind)
	out["subject"] = i18n.Tf(ctx, "notify.redacted.subject", "kind", label)
	out["body"] = i18n.Tf(ctx, "notify.redacted.body", "kind", label, "url", url)
	return out
}
