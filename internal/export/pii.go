package export

import "gitflic.ru/otezvikentiy/gotcha/internal/ingest"

// maskedValue — заменитель прямых идентификаторов пользователя в выгрузке.
// Одна константа на весь пакет: одинаковая маска в CSV-колонках и в JSON —
// признак, по которому получатель выгрузки узнаёт «данные скрыты», а не
// повод гадать, что означает конкретное значение.
const maskedValue = "[masked]"

// MaskUser прячет прямые идентификаторы пользователя. user_id остаётся: он
// внутренний и нужен, чтобы посчитать «сколько пользователей задело».
//
// Пустая строка не превращается в маску: пустая колонка честнее ложного
// «данные скрыты» там, где данных не было — иначе по выгрузке нельзя было бы
// отличить «email скрыт» от «email не собирался вовсе» (важно, когда на
// приёме уже включён скрабинг и user_email в хранилище и так пуст).
func MaskUser(ip, email string) (string, string) {
	if ip != "" {
		ip = maskedValue
	}
	if email != "" {
		email = maskedValue
	}
	return ip, email
}

// MaskJSON проходит по объекту и маскирует значения ключей из денилиста приёма
// (Authorization, Cookie, X-Api-Key и прочее — internal/ingest.DefaultDenyKeys).
// Тот же денилист, что и на приёме: список не дублируется, поведение не
// расходится. Битый или пустой вход возвращается как есть — выгрузка не должна
// молча терять то, что не смогла разобрать.
func MaskJSON(raw string) string {
	scrub := ingest.NewScrubber(true, true, ingest.DefaultDenyKeys())
	return scrub.ScrubJSON(raw)
}
