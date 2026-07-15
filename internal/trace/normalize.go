package trace

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// maxNormalizedDescription — кап нормализованного описания. Оно уезжает в
// фингерпринт проблемы и в её заголовок (то и другое хранится в PG), поэтому
// длина ограничена так же, как описание спана в ingest.
const maxNormalizedDescription = 2000

// capRunes обрезает s до n рун: описания приезжают из SDK и доверия им нет.
// Быстрый путь без аллокации — обычный случай: зовётся на КАЖДОМ спане каждой
// транзакции, а кап срабатывает на единицах из них, поэтому []rune строится
// только когда обрезать действительно нужно.
func capRunes(s string, n int) string {
	if len(s) <= n || utf8.RuneCountInString(s) <= n { // len в байтах >= числа рун
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// NormalizeSQL приводит запрос к виду, в котором два одинаковых по структуре
// запроса с разными значениями совпадают: литералы -> ?, списки IN (...) ->
// IN (?), числа -> ?, схлопывание пробелов, комментарии выброшены. Регистр
// ключевых слов НЕ трогаем (запрос показывается человеку), но сравнение идёт
// по нормализованной форме.
//
// Это НЕ парсер SQL, а нормализатор: он работает на каждом спане каждой
// транзакции, поэтому проходит по строке ровно один раз и ничего не
// валидирует. Запрос, который не удалось разобрать (незакрытый литерал,
// не-SQL мусор), возвращается разумным, а не изувеченным; паники нет ни на
// каком входе. Уже параметризованные плейсхолдеры ($1, ?, :name) остаются как
// есть — Doctrine/PDO присылают именно такие.
func NormalizeSQL(q string) string {
	var b strings.Builder
	b.Grow(len(q))

	// pendingSpace — между токенами был пробельный разговор (пробелы, перевод
	// строки, комментарий): вставим ровно один пробел, когда придёт токен.
	pendingSpace := false
	sep := func() {
		if pendingSpace && b.Len() > 0 {
			b.WriteByte(' ')
		}
		pendingSpace = false
	}

	for i := 0; i < len(q); {
		c := q[i]

		switch {
		case isSQLSpace(c):
			pendingSpace = true
			i++

		// -- строчный комментарий до конца строки. `#` комментарием НЕ считается:
		// в Postgres это оператор, а `#>` / `#>>` (доступ к JSON по пути) —
		// обычное дело, и трактовка `#` как комментария отрезала бы у таких
		// запросов весь хвост, слепив в одну проблему всё с общим префиксом.
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			for i < len(q) && q[i] != '\n' {
				i++
			}
			pendingSpace = true

		// /* блочный комментарий */ (незакрытый съедает хвост, но не зацикливается).
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			i += 2
			for i < len(q) && !(q[i] == '*' && i+1 < len(q) && q[i+1] == '/') {
				i++
			}
			if i < len(q) {
				i += 2
			}
			pendingSpace = true

		// 'строковый литерал' -> ?
		case c == '\'':
			i = skipSQLString(q, i)
			sep()
			b.WriteByte('?')

		// "идентификатор" (Postgres) и `идентификатор` (MySQL) — не значения,
		// копируем как есть.
		case c == '"' || c == '`':
			end := skipQuoted(q, i, c)
			sep()
			b.WriteString(q[i:end])
			i = end

		// $1 — плейсхолдер pgx/PDO; одиночный $ — просто символ.
		case c == '$' && i+1 < len(q) && isDigit(q[i+1]):
			j := i + 1
			for j < len(q) && isDigit(q[j]) {
				j++
			}
			sep()
			b.WriteString(q[i:j])
			i = j

		// :name / :1 — именованный плейсхолдер; :: (каст) распадётся на два
		// символьных токена и склеится обратно.
		case c == ':' && i+1 < len(q) && (isIdentStart(q[i+1]) || isDigit(q[i+1])):
			j := i + 1
			for j < len(q) && isIdentPart(q[j]) {
				j++
			}
			sep()
			b.WriteString(q[i:j])
			i = j

		// 42, 3.14, 1.2e-5, 0xff -> ?
		case isDigit(c) || (c == '.' && i+1 < len(q) && isDigit(q[i+1])):
			i = skipNumber(q, i)
			sep()
			b.WriteByte('?')

		// слово: ключевое слово, идентификатор или префикс строки (E'...', N'...').
		case isIdentStart(c):
			j := i + 1
			for j < len(q) && isIdentPart(q[j]) {
				j++
			}
			word := q[i:j]
			if j < len(q) && q[j] == '\'' && isStringPrefix(word) {
				i = skipSQLString(q, j)
				sep()
				b.WriteByte('?')
				continue
			}
			sep()
			b.WriteString(word)
			i = j

		// всё остальное (операторы, скобки, запятые, мусор) — как есть.
		default:
			sep()
			b.WriteByte(c)
			i++
		}
	}

	return collapseINList(b.String())
}

// inListRe — список плейсхолдеров внутри IN (...): именно он превращает
// IN (1,2,3) и IN (4,5) в одну форму. Подзапрос IN (SELECT ...) не подходит
// под шаблон и остаётся нетронутым. Группа 1 — само ключевое слово, чтобы не
// менять его регистр.
var inListRe = regexp.MustCompile(`(?i)\b(in)\s*\(\s*\?(?:\s*,\s*\?)*\s*\)`)

func collapseINList(q string) string {
	return inListRe.ReplaceAllString(q, "${1} (?)")
}

// skipSQLString возвращает индекс за закрывающей кавычкой литерала,
// начинающегося на q[i] == '\''.
//
// Основная семантика — стандартная (и дефолтная для Postgres при
// standard_conforming_strings=on): экранирование только удвоением кавычки (''),
// обратный слеш внутри литерала — обычный символ. Это НЕ вкусовщина: если
// считать `\` экранирующим, то валидный литерал `'C:\'` съедает свою же
// закрывающую кавычку, сканер пересинхронизируется на следующей, и содержимое
// СОСЕДНЕГО литерала (значение пользователя!) вываливается в вывод голыми
// токенами — а вывод уходит в title проблемы и в её фингерпринт.
//
// Экранирование слешем (MySQL, Postgres E'...') остаётся запасным вариантом и
// применяется, только когда стандартное чтение оставляет ОСТАТОК ЗАПРОСА с
// непарной кавычкой (то есть слеш действительно экранировал) и когда сам
// запасной разбор находит закрывающую кавычку. Оба ограничения не дают ему
// сожрать хвост запроса. Незакрытый литерал съедает остаток строки — это лучше,
// чем зациклиться.
func skipSQLString(q string, i int) int {
	end, closed := skipStandardString(q, i)
	if !closed || !closedByEscapedQuote(q, i, end) {
		return end
	}
	if !hasOddQuotes(q[end:]) {
		return end
	}
	if alt, altClosed := skipBackslashString(q, i); altClosed {
		return alt
	}
	return end
}

// skipStandardString — литерал по стандарту SQL: закрывается первой одиночной
// кавычкой, удвоенная кавычка ('') остаётся внутри.
func skipStandardString(q string, i int) (int, bool) {
	i++ // открывающая кавычка
	for i < len(q) {
		if q[i] != '\'' {
			i++
			continue
		}
		if i+1 < len(q) && q[i+1] == '\'' {
			i += 2
			continue
		}
		return i + 1, true
	}
	return len(q), false
}

// skipBackslashString — литерал с экранированием обратным слешем (MySQL, E'...').
func skipBackslashString(q string, i int) (int, bool) {
	i++
	for i < len(q) {
		switch q[i] {
		case '\\':
			i += 2
		case '\'':
			if i+1 < len(q) && q[i+1] == '\'' {
				i += 2
				continue
			}
			return i + 1, true
		default:
			i++
		}
	}
	return len(q), false
}

// closedByEscapedQuote — перед закрывающей кавычкой стоит НЕЧЁТНОЕ число слешей,
// то есть в диалекте со слешевым экранированием она была бы экранированной:
// разбор неоднозначен.
func closedByEscapedQuote(q string, start, end int) bool {
	j := end - 2 // символ перед закрывающей кавычкой
	n := 0
	for j >= start+1 && q[j] == '\\' {
		n++
		j--
	}
	return n%2 == 1
}

// hasOddQuotes — в остатке запроса непарное число кавычек: при стандартном
// чтении он остался бы с незакрытым литералом, а значит слеш всё-таки экранировал.
func hasOddQuotes(rest string) bool {
	return strings.Count(rest, "'")%2 == 1
}

// skipQuoted возвращает индекс за закрывающей кавычкой quote (" или `),
// учитывая удвоение как экранирование.
func skipQuoted(q string, i int, quote byte) int {
	i++
	for i < len(q) {
		if q[i] == quote {
			if i+1 < len(q) && q[i+1] == quote {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return len(q)
}

// skipNumber возвращает индекс за числовым литералом: целое, дробное,
// с экспонентой или шестнадцатеричное.
func skipNumber(q string, i int) int {
	for i < len(q) && (isDigit(q[i]) || q[i] == '.') {
		i++
	}
	if i < len(q) && (q[i] == 'e' || q[i] == 'E') {
		j := i + 1
		if j < len(q) && (q[j] == '+' || q[j] == '-') {
			j++
		}
		if j < len(q) && isDigit(q[j]) {
			for j < len(q) && isDigit(q[j]) {
				j++
			}
			i = j
		}
	}
	// 0xff / 0b1010: хвост из букв и цифр после числа — часть литерала.
	if i < len(q) && (q[i] == 'x' || q[i] == 'X' || q[i] == 'b' || q[i] == 'B') {
		j := i + 1
		for j < len(q) && isHexDigit(q[j]) {
			j++
		}
		if j > i+1 {
			i = j
		}
	}
	return i
}

// isStringPrefix — буквенный префикс строкового литерала: E'...' (Postgres),
// N'...' (SQL Server), B'...'/X'...' (битовые и hex-строки).
func isStringPrefix(word string) bool {
	if len(word) != 1 {
		return false
	}
	switch word[0] {
	case 'e', 'E', 'n', 'N', 'b', 'B', 'x', 'X':
		return true
	}
	return false
}

func isSQLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHexDigit(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// isIdentStart/isIdentPart: байты >= 0x80 — это части UTF-8-последовательностей
// (имена таблиц и колонок бывают не-ASCII), они всегда часть слова, так что
// многобайтные руны копируются целиком.
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }

// NormalizeCacheKey нормализует описание не-SQL операции с БД (Redis,
// Memcached): `GET user:42` -> `GET user:?`, `HGETALL session:abc123` ->
// `HGETALL session:?`. Через NormalizeSQL такие описания гнать нельзя: там
// `:name` — именованный плейсхолдер, и `user:1` с `user:42` остались бы РАЗНЫМИ
// формами, то есть ленивая подгрузка кеша в цикле (N разных ключей) не
// схлопывалась бы в одну находку — детектор молчал бы ровно там, где N+1 и есть.
//
// Команда (первый токен) сохраняется: GET и SET — разные операции. В остальных
// токенах маскируется каждый сегмент ключа, похожий на значение (цифры, hex,
// UUID); статические сегменты (`user`, `session`, `config:global`) остаются —
// без них описание превратилось бы в бесполезное `GET ?`.
func NormalizeCacheKey(desc string) string {
	fields := strings.Fields(desc)
	if len(fields) == 0 {
		return ""
	}
	for i := 1; i < len(fields); i++ {
		fields[i] = maskKeyToken(fields[i])
	}
	return strings.Join(fields, " ")
}

// maskKeyToken маскирует сегменты ключа, разделённые ':' (общепринятое
// соглашение об именовании ключей в Redis).
func maskKeyToken(tok string) string {
	segs := strings.Split(tok, ":")
	last := len(segs) - 1
	for i, seg := range segs {
		if isValueSegment(seg, i == last && last > 0) {
			segs[i] = "?"
		}
	}
	return strings.Join(segs, ":")
}

// isValueSegment — сегмент похож на значение, а не на имя.
//
// Правило первое: сегмент содержит цифру (42, abc123, 7c9e6679-...) — он
// меняется от вызова к вызову.
//
// Правило второе (позиционное): сегмент ПОСЛЕДНИЙ в составном ключе. Одних цифр
// мало: `user:jsmith` и `user:mbrown` цифр не содержат, оставались бы разными
// формами — и N+1 по буквенным идентификаторам (логины, slug'и) детектор просто
// не видел бы. Отличить `jsmith` от `global` структурно невозможно: то и другое
// — шесть букв, и никакая эвристика длины или энтропии их не разведёт.
//
// Цена решения: статический `config:global` тоже схлопывается в `config:?`. Это
// осознанный размен. Во-первых, описание остаётся читаемым — префикс (`user:`,
// `config:`) сохраняется, `GET ?` не получается. Во-вторых, ложной тревоги это
// не создаёт: N чтений ОДНОГО И ТОГО ЖЕ ключа и без маскирования попадали в одну
// группу, а N чтений РАЗНЫХ ключей с общим префиксом в цикле (тот самый
// feature-flag в Redis по всем эндпойнтам) — это и есть N+1, который мы ищем.
// Односегментные ключи (`PING`, `GET config`) не трогаются, если в них нет цифр.
func isValueSegment(seg string, lastInKey bool) bool {
	if seg == "" {
		return false
	}
	if lastInKey {
		return true
	}
	for i := 0; i < len(seg); i++ {
		if isDigit(seg[i]) {
			return true
		}
	}
	return false
}

// NormalizeURL заменяет id-подобные сегменты пути на {id}: /users/42/posts/7
// -> /users/{id}/posts/{id}; query-строка и фрагмент отбрасываются целиком (в
// них значения). Хост сохраняется: разные сервисы — разные проблемы.
// Описание http-спана у SDK часто выглядит как "GET https://...": метод
// сохраняется, нормализуется остаток.
//
// Что считается id — см. isIDSegment: одних чисел и UUID мало (ObjectID Mongo,
// хеши, slug'и с числовым хвостом давали НОВЫЙ фингерпринт на каждый запрос, а
// значит новую строку perf_issues — таблица росла без предела).
func NormalizeURL(u string) string {
	s := strings.TrimSpace(u)
	if s == "" {
		return ""
	}

	method := ""
	if i := strings.IndexByte(s, ' '); i > 0 && isHTTPMethod(s[:i]) {
		method = s[:i] + " "
		s = strings.TrimSpace(s[i+1:])
	}

	// query и фрагмент — целиком значения, выбрасываем.
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}

	// scheme://host[:port] отделяем от пути и не трогаем.
	head := ""
	if i := strings.Index(s, "://"); i >= 0 {
		rest := s[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			head, s = s[:i+3+j], rest[j:]
		} else {
			head, s = s, ""
		}
	}

	if s != "" {
		segs := strings.Split(s, "/")
		for i, seg := range segs {
			if isIDSegment(seg) {
				segs[i] = "{id}"
			}
		}
		s = strings.Join(segs, "/")
	}

	return method + head + s
}

// isIDSegment — сегмент пути похож на идентификатор, а не на имя маршрута.
// Правил четыре, от самого надёжного к самому вольному:
//
//  1. только цифры (`/users/42`);
//  2. канонический UUID (`/orders/7c9e6679-...`);
//  3. ≥ idHexSegmentLen шестнадцатеричных цифр подряд — ObjectID Mongo (24),
//     sha1/sha256, hex-токены сессий. Английских слов такой длины из одних лишь
//     букв a–f не бывает (`deadbeefdeadbeef` — не слово);
//  4. сегмент содержит цифру, длиной ≥ idDigitSegmentLen и без точки
//     (`/u/user1234567`, `/p/post-1234567`).
//
// Четвёртое правило — эвристика, и размен здесь сознательный. За: приложение,
// не шаблонизирующее маршруты, иначе плодит по строке perf_issues на КАЖДЫЙ
// запрос (при 50 rps — ~180 тысяч строк в час, и retention-задачи для этой
// таблицы нет). Против: длинное осмысленное слово с цифрой (`/download2024`)
// схлопнется в {id}, и две таких страницы станут одной проблемой. Цена ошибки
// несимметрична: склеенная проблема — это неточная группировка, а несклеенная —
// неограниченный рост таблицы, поэтому границы выбраны так, чтобы обычные слова
// (`/articles`, `/checkout`, `/v2`, слаг `/hello-world`) не трогать: без цифры
// сегмент не маскируется никогда, а короткие сегменты с цифрой (`/42abc`, `/v1`)
// — тоже. Точка исключает хосты, записанные без схемы (`api2.example.com`), и
// имена файлов (`app-2024.css`).
func isIDSegment(s string) bool {
	if s == "" {
		return false
	}
	if isNumericSegment(s) || isUUIDSegment(s) {
		return true
	}
	if len(s) >= idHexSegmentLen && isHexSegment(s) {
		return true
	}
	if len(s) < idDigitSegmentLen {
		return false
	}
	hasDigit := false
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return false
		}
		if isDigit(s[i]) {
			hasDigit = true
		}
	}
	return hasDigit
}

// Пороги длины для isIDSegment: 16 hex-символов — короче любого практического
// hex-идентификатора (ObjectID — 24, sha1 — 40) и длиннее любого слова из букв
// a–f; 8 символов — длина, ниже которой сегмент с цифрой чаще осмысленный
// (`v1`, `42abc`, `top10`), чем идентификатор.
const (
	idHexSegmentLen   = 16
	idDigitSegmentLen = 8
)

// isHexSegment — сегмент целиком из шестнадцатеричных цифр (в любом регистре).
func isHexSegment(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isHexDigit(s[i]) {
			return false
		}
	}
	return true
}

func isHTTPMethod(s string) bool {
	switch strings.ToUpper(s) {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "TRACE", "CONNECT":
		return true
	}
	return false
}

func isNumericSegment(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// isUUIDSegment — канонический UUID 8-4-4-4-12 в любом регистре.
func isUUIDSegment(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if !isHexDigit(s[i]) {
				return false
			}
		}
	}
	return true
}

// NormalizeDescription выбирает нормализатор по op спана (SQL-овые db -> SQL,
// прочие db (db.redis, db.memcached) -> маскирование ключа, http* -> URL,
// иначе — описание как есть) и каппит результат. Чистая детерминированная
// функция: одинаковый вход даёт одинаковый выход на любой реплике — иначе
// фингерпринты проблем разъедутся.
func NormalizeDescription(op, description string) string {
	lop := strings.ToLower(strings.TrimSpace(op))

	var out string
	switch {
	case isSQLOp(lop):
		out = NormalizeSQL(description)
	case strings.HasPrefix(lop, "db"):
		out = NormalizeCacheKey(description)
	case strings.HasPrefix(lop, "http"):
		out = NormalizeURL(description)
	default:
		out = strings.TrimSpace(description)
	}
	return capRunes(out, maxNormalizedDescription)
}

// isSQLOp — op спана, описание которого является SQL: `db` (так OTLP-маппинг
// называет реляционные БД — postgresql, mysql и прочие, см. otlpDBOp в
// internal/ingest/otlp.go) и `db.sql.*` / `db.query` от Sentry SDK. Всё
// остальное под `db.` (db.redis, db.memcached) — key-value хранилища, где
// описание это команда с ключом, а не запрос.
func isSQLOp(lop string) bool {
	return lop == "db" || strings.HasPrefix(lop, "db.sql") || strings.HasPrefix(lop, "db.query")
}
