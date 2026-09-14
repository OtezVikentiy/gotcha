package trace

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxNormalizedDescription = 2000

// []rune строится только когда обрезать действительно нужно — зовётся на каждом спане.
func capRunes(s string, n int) string {
	if len(s) <= n || utf8.RuneCountInString(s) <= n { // len в байтах >= числа рун
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// Это НЕ парсер SQL: проходит по строке ровно один раз, ничего не валидирует, и на
// неразобираемом входе (незакрытый литерал, не-SQL мусор) не паникует.
func NormalizeSQL(q string) string {
	var b strings.Builder
	b.Grow(len(q))
	quotePfx := buildQuotePrefixFn(q)

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

		// `#` комментарием НЕ считается: в Postgres это оператор (`#>`, `#>>` — JSON path).
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
			i = skipSQLString(q, i, quotePfx)
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
				i = skipSQLString(q, j, quotePfx)
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

// Подзапрос IN (SELECT ...) не подходит под шаблон и остаётся нетронутым.
var inListRe = regexp.MustCompile(`(?i)\b(in)\s*\(\s*\?(?:\s*,\s*\?)*\s*\)`)

func collapseINList(q string) string {
	return inListRe.ReplaceAllString(q, "${1} (?)")
}

// Стандартная семантика (слеш — обычный символ) пробуется первой: считать `\` экранирующим
// значило бы, что `'C:\'` съедает свою кавычку и в вывод вываливается СОСЕДНИЙ литерал.
// quotePfx — см. buildQuotePrefix.
func skipSQLString(q string, i int, quotePfx []int32) int {
	end, closed := skipStandardString(q, i)
	if !closed || !closedByEscapedQuote(q, i, end) {
		return end
	}
	if !oddQuotesFrom(quotePfx, end) {
		return end
	}
	if alt, altClosed := skipBackslashString(q, i); altClosed {
		return alt
	}
	return end
}

// Закрывается первой одиночной кавычкой, удвоенная кавычка (”) остаётся внутри.
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

// экранирование обратным слешем — синтаксис MySQL и Postgres E'...'.
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

// НЕЧЁТНОЕ число слешей перед кавычкой — в диалекте со слешевым экранированием она
// была бы экранированной, разбор неоднозначен.
func closedByEscapedQuote(q string, start, end int) bool {
	j := end - 2 // символ перед закрывающей кавычкой
	n := 0
	for j >= start+1 && q[j] == '\\' {
		n++
		j--
	}
	return n%2 == 1
}

// Через переменную, не напрямую: тест на TestNormalizeSQLBuildsQuotePrefixOnce
// подменяет её счётчиком, чтобы проверить кратность вызова без замера времени.
var buildQuotePrefixFn = buildQuotePrefix

// quotePfx[k] = число ' в q[:k]; строится один раз в NormalizeSQL (O(len(q))),
// а не на каждый литерал — иначе транзакция с O(n) литералами сканировалась бы O(n²).
func buildQuotePrefix(q string) []int32 {
	pfx := make([]int32, len(q)+1)
	var n int32
	for i := 0; i < len(q); i++ {
		if q[i] == '\'' {
			n++
		}
		pfx[i+1] = n
	}
	return pfx
}

// Нечётность числа кавычек в q[from:] — O(1) по префиксным суммам вместо
// strings.Count по остатку строки.
func oddQuotesFrom(quotePfx []int32, from int) bool {
	total := quotePfx[len(quotePfx)-1]
	return (total-quotePfx[from])%2 == 1
}

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

// E'...' (Postgres), N'...' (SQL Server), B'...'/X'...' (битовые и hex-строки).
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

// Байты >= 0x80 — части UTF-8-последовательностей (имена бывают не-ASCII), всегда часть слова.
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }

// Через NormalizeSQL такие описания гнать нельзя: там `:name` — плейсхолдер, и `user:1` с
// `user:42` остались бы РАЗНЫМИ формами — детектор молчал бы ровно там, где N+1 и есть.
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

// Последний сегмент составного ключа маскируется всегда, даже без цифр (`user:jsmith`):
// структурно неотличим от статического (`config:global`), но иначе N+1 не увидеть.
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

// query-строка и фрагмент отбрасываются целиком — в них значения. Хост сохраняется:
// разные сервисы — разные проблемы.
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

// Правило 4 (цифра + длина, без точки) — эвристика: длинное осмысленное слово с цифрой
// (`/download2024`) тоже схлопнется в {id}. Точка исключает хосты и имена файлов.
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

// 16 hex-символов короче любого практического hex-идентификатора и длиннее слова из a–f;
// 8 — ниже этого сегмент с цифрой чаще осмысленный (`v1`, `top10`), чем идентификатор.
const (
	idHexSegmentLen   = 16
	idDigitSegmentLen = 8
)

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

// Канонический UUID 8-4-4-4-12 в любом регистре.
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

// Чистая детерминированная функция: одинаковый вход даёт одинаковый выход на любой
// реплике — иначе фингерпринты проблем разъедутся.
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

// Всё остальное под `db.` (db.redis, db.memcached) — key-value хранилища, где
// описание это команда с ключом, а не запрос.
func isSQLOp(lop string) bool {
	return lop == "db" || strings.HasPrefix(lop, "db.sql") || strings.HasPrefix(lop, "db.query")
}
