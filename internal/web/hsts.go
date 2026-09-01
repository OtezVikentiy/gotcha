package web

import "strconv"

// HSTSHeaderValue собирает значение заголовка Strict-Transport-Security из
// настроек инстанса (GOTCHA_HSTS_*). Значение считается ОДИН раз на старте и
// кладётся в Handler.HSTSHeader — на каждый запрос его пересобирать незачем.
//
// includeSubDomains по умолчанию выключен и включается только явным решением
// оператора: инстанс часто живёт на поддомене, и флаг с нашей стороны
// принудил бы к HTTPS соседние сервисы того же домена, о которых приложение
// ничего не знает.
//
// Функция тотальна и ничего не валидирует: при enabled == false или
// отрицательном maxAgeSeconds возвращается пустая строка (заголовок не
// ставится). Валидация значений — дело конфига и отказ старта
// (cmd/gotcha/config.go), но битый заголовок не должен собраться ни на одном
// пути, включая тесты и будущих вызывающих.
//
// max-age=0 — ЗАКОННОЕ значение и не то же самое, что выключенный HSTS:
// браузер, однажды получивший max-age=31536000, держит пин год независимо от
// того, что сервер перестал слать заголовок. Единственный способ снять пин —
// реально отправленный max-age=0, поэтому enabled=true с нулевым max-age
// обязан давать непустую строку.
func HSTSHeaderValue(enabled bool, maxAgeSeconds int, includeSubDomains, preload bool) string {
	if !enabled || maxAgeSeconds < 0 {
		return ""
	}
	v := "max-age=" + strconv.Itoa(maxAgeSeconds)
	if includeSubDomains {
		v += "; includeSubDomains"
	}
	if preload {
		v += "; preload"
	}
	return v
}
