package i18n

import "golang.org/x/text/language"

// Первый язык — дефолт при отсутствии совпадения в Accept-Language.
var supported = []language.Tag{language.Russian, language.English}

var matcher = language.NewMatcher(supported)

var known = map[string]bool{"ru": true, "en": true}

func Parse(code string) (Locale, bool) {
	if known[code] {
		return Locale{Code: code}, true
	}
	return Locale{}, false
}

func Match(acceptLanguage string) Locale {
	tag, _ := language.MatchStrings(matcher, acceptLanguage)
	base, _ := tag.Base()
	if l, ok := Parse(base.String()); ok {
		return l
	}
	return Default
}
