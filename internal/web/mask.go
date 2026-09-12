package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
)

// webhook показывает только scheme://host — путь и query часто несут токены.
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

// суффикс — 16 бит sha256(target): различает каналы со схлопнувшейся маской,
// исходное значение по нему не восстановить.
func maskDiscriminator(target string) string {
	sum := sha256.Sum256([]byte(target))
	return "·" + hex.EncodeToString(sum[:2])
}
