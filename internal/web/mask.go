package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
)

// maskChannelTarget — цель канала для показа НЕ-админу: оператору нужно
// отличать каналы и отлаживать доставку, но адреса получателей — чужие
// персональные данные (спека 2026-08-08, решения владельца 2026-08-10).
// Email: первая руна + ***@домен; telegram: последние 2 цифры chat_id;
// webhook: только scheme://host — путь и query часто несут токены.
//
// C3: одного scheme://host мало, если в проекте несколько webhook-каналов
// на общем хосте (Slack/Discord — секрет в пути, а не в хосте): маска
// схлопывала их в одинаковую строку, и оператор не мог отличить каналы
// друг от друга ни в таблице каналов, ни в пикере формы монитора. К каждой
// маске добавлен дискриминатор — см. maskDiscriminator.
func maskChannelTarget(kind, target string) string {
	masked := maskChannelTargetCore(kind, target)
	if target == "" {
		return masked
	}
	return masked + " " + maskDiscriminator(target)
}

func maskChannelTargetCore(kind, target string) string {
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

// maskDiscriminator — короткий (4 hex-символа = 16 бит) суффикс от
// sha256(target) для маски канала (находка C3): два разных target,
// схлопнувшихся в одинаковую маску (типичный случай — два webhook на одном
// хосте: путь спрятан, а секрет живёт в пути), получают разные метки.
// Однонаправленный хеш полного target, включая скрытые часть/query — по
// суффиксу нельзя восстановить исходное значение, только сравнить с другой
// меткой на глаз. 16 бит — это осознанный компромисс: коллизия на пару
// каналов в проекте не страшна (метки просто совпадут, это косметика, не
// брешь), а на десяток символов в UI не тратится.
func maskDiscriminator(target string) string {
	sum := sha256.Sum256([]byte(target))
	return "·" + hex.EncodeToString(sum[:2])
}
