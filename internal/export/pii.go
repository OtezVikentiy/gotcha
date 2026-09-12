package export

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
)

const maskedValue = "[masked]"

// Пустая строка не превращается в маску — иначе нельзя отличить «скрыто» от «не собиралось».
// Для user_id — PseudonymizeUserID, не эта функция: user.id из SDK часто реальный PII, не суррогат.
func MaskUser(ip, email string) (string, string) {
	if ip != "" {
		ip = maskedValue
	}
	if email != "" {
		email = maskedValue
	}
	return ip, email
}

// 8 байт с запасом достаточно, чтобы не столкнуть user_id в пределах одной
// заявки (не для криптостойкости).
const userIDPseudonymHexLen = 16

// Псевдоним, не статичная маска — нужно считать уникальных пользователей внутри выгрузки.
// salt одноразовый на заявку: восстановить user_id нельзя, сопоставить два экспорта — тоже нельзя.
func PseudonymizeUserID(userID string, salt []byte) string {
	if userID == "" {
		return ""
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(userID))
	sum := mac.Sum(nil)
	return hex.EncodeToString(sum)[:userIDPseudonymHexLen]
}

// Один salt на один вызов eventSource.Stream, не на проект/инстанс — иначе
// одинаковый псевдоним user_id в разных выгрузках коррелировал бы файлы.
func NewExportSalt() []byte {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		// crypto/rand.Read практически никогда не ошибается на исправной ОС;
		// паника — честнее тихой деградации до предсказуемого salt.
		panic("export: crypto/rand недоступен: " + err.Error())
	}
	return salt
}

// Общий текст для Meta.PseudonymNote и письма — формулировка обязана совпадать дословно в обоих местах.
// На английском независимо от локали: Meta целиком машиночитаемая и не проходит через i18n.
const PseudonymUniquenessNote = "user_id pseudonyms are unique to this export and are not comparable with other exports"

// Общий на пакет, не новый на каждый вызов — денилист неизменен и не стоит его
// пересобирать на каждое событие. ScrubFreeText=true независимо от GOTCHA_SCRUB_FREETEXT.
var jsonScrubber = newJSONScrubber()

func newJSONScrubber() *ingest.Scrubber {
	s := ingest.NewScrubber(true, true, ingest.DefaultDenyKeys())
	s.ScrubFreeText = true
	return s
}

// Тот же денилист, что на приёме — не дублируется. Битый или пустой вход возвращается как есть.
func MaskJSON(raw string) string {
	return jsonScrubber.ScrubJSON(raw)
}

// Тот же скрабер, что на приёме (Scrubber.ScrubMessage) — поведение обязано совпадать дословно.
func MaskMessage(text string) string {
	return jsonScrubber.ScrubMessage(text)
}

// Копия обязательна: ScrubTags мутирует карту на месте, а входная map
// принадлежит вызывающему — маскирование не должно быть видно другим читателям тех же тегов.
func MaskTags(tags map[string]string) map[string]string {
	if len(tags) == 0 {
		return tags
	}
	cp := make(map[string]string, len(tags))
	for k, v := range tags {
		cp[k] = v
	}
	jsonScrubber.ScrubTags(cp)
	return cp
}
