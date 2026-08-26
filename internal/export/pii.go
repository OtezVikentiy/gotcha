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

// jsonScrubber — один общий *ingest.Scrubber на весь пакет, а не новый на
// каждый вызов MaskJSON: денилист константен (ingest.DefaultDenyKeys), и
// eventSource.toRecord зовёт MaskJSON дважды на КАЖДОЕ событие выгрузки
// (request и contexts, source_events.go) — до 400 000 конструирований
// Scrubber на заявку с потолком в 200k строк, каждое заново компилирует
// нормализованный денилист.
//
// Безопасен для конкурентного использования: ScrubJSON (и весь путь
// walk/scrubValue под ней) читает поля Scrubber (ScrubIP/ScrubEmail/
// denyNorm/allowNorm), но ни разу их не пишет — единственные мутации полей
// во всём internal/ingest/scrub.go происходят в NewScrubber (конструктор,
// однократно, до публикации переменной) и в SetAllowKeys, которую MaskJSON
// не зовёт. Мутирует ScrubJSON только ЛОКАЛЬНУЮ декодированную копию JSON
// каждого вызова (v, распакованный из raw), а не сам Scrubber, — конкурентные
// вызовы не делят это состояние между собой.
var jsonScrubber = ingest.NewScrubber(true, true, ingest.DefaultDenyKeys())

// MaskJSON проходит по объекту и маскирует значения ключей из денилиста приёма
// (Authorization, Cookie, X-Api-Key и прочее — internal/ingest.DefaultDenyKeys).
// Тот же денилист, что и на приёме: список не дублируется, поведение не
// расходится. Битый или пустой вход возвращается как есть — выгрузка не должна
// молча терять то, что не смогла разобрать.
func MaskJSON(raw string) string {
	return jsonScrubber.ScrubJSON(raw)
}
