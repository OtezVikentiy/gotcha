package notify

import (
	"context"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// RedactToken replaces every occurrence of token in s with a placeholder.
// No-op when token is empty (never redact against an empty needle, which
// would otherwise match everywhere).
//
// Родилась в telegram.go как приём против эха bot-токена в non-2xx теле
// ответа Telegram API; здесь вынесена в общий хелпер и экспортирована, чтобы
// её же приёмом пользовались email.go/webhook.go (санация last_error у
// источника) и web.alertDeliveriesPage (второй эшелон — санация уже
// сохранённого last_error перед рендером не-admin'у).
func RedactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "<redacted>")
}

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
	// Если у нотифаера сам АДРЕС карточки несёт деталь (у хостов это имя
	// машины), он кладёт в payload "url_redacted" — укороченную ссылку, которой
	// RedactExternalPayload заменяет "url"; в белый список она не входит (см.
	// RedactExternalPayload).
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
//
// Необязательное поле payload "url_redacted" — сокращённая ссылка на случай,
// когда деталь несёт сам АДРЕС карточки. Есть — уходит наружу и в out["url"],
// и в тело вместо полной; нет — поведение прежнее. Так решается случай хостов:
// карточка адресуется именем машины (/projects/{id}/hosts/{имя}), id-адресации
// у хоста нет, и при выключенных деталях имя уезжало бы в Telegram внутри
// разрешённого "url" — теперь нотифаер кладёт рядом ссылку на список хостов.
// В externalSafeKeys "url_redacted" не входит намеренно: это директива для
// редакции, а не поле вывода, и отдельной строкой наружу оно не идёт.
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
	out["subject"] = i18n.Tf(ctx, "notify.redacted.subject", "kind", label)
	out["body"] = i18n.Tf(ctx, "notify.redacted.body", "kind", label, "url", url)
	return out
}
