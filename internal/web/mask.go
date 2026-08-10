package web

import (
	"net/url"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
)

// maskChannelTarget — цель канала для показа НЕ-админу: оператору нужно
// отличать каналы и отлаживать доставку, но адреса получателей — чужие
// персональные данные (спека 2026-08-08, решения владельца 2026-08-10).
// Email: первая руна + ***@домен; telegram: последние 2 цифры chat_id;
// webhook: только scheme://host — путь и query часто несут токены.
func maskChannelTarget(kind, target string) string {
	switch kind {
	case alert.ChannelEmail:
		at := strings.LastIndex(target, "@")
		if at <= 0 {
			return "****"
		}
		local := []rune(target[:at])
		if len(local) < 2 {
			return "***" + target[at:]
		}
		return string(local[0]) + "***" + target[at:]
	case alert.ChannelTelegram:
		r := []rune(target)
		if len(r) < 3 {
			return "****"
		}
		return "****" + string(r[len(r)-2:])
	case alert.ChannelWebhook:
		u, err := url.Parse(target)
		if err != nil || u.Host == "" {
			return "****"
		}
		return u.Scheme + "://" + u.Host + "/…"
	default:
		return "****"
	}
}
