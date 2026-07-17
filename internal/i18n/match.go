package i18n

import "golang.org/x/text/language"

// supported — поддерживаемые языки; ПЕРВЫЙ является дефолтом при отсутствии
// совпадения в Accept-Language.
var supported = []language.Tag{language.Russian, language.English}

var matcher = language.NewMatcher(supported)

var known = map[string]bool{"ru": true, "en": true}

// Parse принимает базовый код "ru"/"en"; неизвестный — ok=false.
func Parse(code string) (Locale, bool) {
	if known[code] {
		return Locale{Code: code}, true
	}
	return Locale{}, false
}

// Match выбирает поддерживаемую локаль по заголовку Accept-Language; нет
// совпадений (или пустой заголовок) → Default.
func Match(acceptLanguage string) Locale {
	tag, _ := language.MatchStrings(matcher, acceptLanguage)
	base, _ := tag.Base()
	if l, ok := Parse(base.String()); ok {
		return l
	}
	return Default
}
