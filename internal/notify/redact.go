package notify

import "fmt"

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
	// (channel_kind/target/secret). Без них воркер не доставит сообщение.
	"channel_kind": {},
	"target":       {},
	"secret":       {},
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

// RedactExternalPayload возвращает обезличенную копию payload для доставки во
// внешние каналы (Telegram/webhook), когда оператор выключил раскрытие
// деталей (GOTCHA_EXTERNAL_CHANNEL_DETAILS=false). Текст ошибки, имена
// транзакций/функций и тело уведомления могут нести ПДн, а Telegram/webhook
// уводят их за пределы РФ (152-ФЗ), поэтому наружу отдаётся только маршрут
// доставки, ссылка на карточку и вид алерта.
//
// Оставляются лишь поля из externalSafeKeys; subject/body перезаписываются
// маршрутным минимумом («[gotcha] {kind}» и «{kind}\n\n{url}»), чтобы у
// Telegram (берёт текст из body) и webhook (сериализует весь payload) не
// осталось исходных деталей. Исходный payload не мутируется — возвращается
// новая map.
func RedactExternalPayload(payload map[string]any) map[string]any {
	out := make(map[string]any, len(externalSafeKeys))
	for k, v := range payload {
		if _, ok := externalSafeKeys[k]; ok {
			out[k] = v
		}
	}
	kind, _ := out["kind"].(string)
	url, _ := out["url"].(string)
	out["subject"] = fmt.Sprintf("[gotcha] %s", kind)
	out["body"] = fmt.Sprintf("%s\n\n%s", kind, url)
	return out
}
