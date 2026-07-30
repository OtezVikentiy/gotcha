package docs

import (
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/util"
)

// translitIDs выдаёт заголовкам осмысленные и устойчивые якоря.
//
// Штатный генератор goldmark оставляет от кириллического заголовка пустую
// строку и подставляет «heading», «heading-1», «heading-2»: ссылка на раздел
// оказывается привязанной к ПОРЯДКУ разделов, и вставка абзаца в середину
// страницы протухает все ссылки ниже. Транслитерация даёт якорь, зависящий от
// текста заголовка, а не от его номера.
type translitIDs struct {
	used map[string]bool
}

func newTranslitIDs() parser.IDs { return &translitIDs{used: map[string]bool{}} }

func (t *translitIDs) Generate(value []byte, kind ast.NodeKind) []byte {
	slug := slugify(string(value))
	if slug == "" {
		slug = "section"
	}
	candidate := slug
	for i := 1; t.used[candidate]; i++ {
		candidate = slug + "-" + itoa(i)
	}
	t.used[candidate] = true
	return []byte(candidate)
}

func (t *translitIDs) Put(value []byte) { t.used[string(value)] = true }

// cyr — таблица транслитерации. Практическая, а не стандарт: якорь должен быть
// узнаваем в адресной строке, а не соответствовать ГОСТу.
var cyr = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e",
	'ж': "zh", 'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m",
	'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",
	'ф': "f", 'х': "h", 'ц': "c", 'ч': "ch", 'ш': "sh", 'щ': "sch",
	'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
}

// slugify превращает текст заголовка в якорь: латиница нижним регистром,
// кириллица транслитерируется, всё прочее становится дефисом.
func slugify(s string) string {
	var b strings.Builder
	prevDash := true // подавляет ведущий дефис
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case cyr[r] != "":
			b.WriteString(cyr[r])
			prevDash = false
		case r == 'ъ' || r == 'ь':
			// Мягкий и твёрдый знаки просто исчезают, дефиса не дают.
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

var _ = util.Prioritized // goldmark util удерживается в зависимостях явно
