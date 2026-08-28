package i18n

import (
	"embed"
	"encoding/json"
)

//go:embed locales/*.json
var localeFS embed.FS

type catalog struct {
	Messages map[string]string            `json:"messages"`
	Plurals  map[string]map[string]string `json:"plurals"`
}

// catalogs — загруженные при инициализации каталоги по коду локали.
var catalogs = loadCatalogs()

func loadCatalogs() map[string]catalog {
	out := map[string]catalog{}
	for _, code := range []string{"ru", "en"} {
		b, err := localeFS.ReadFile("locales/" + code + ".json")
		if err != nil {
			panic("i18n: missing catalog " + code + ": " + err.Error())
		}
		var c catalog
		if err := json.Unmarshal(b, &c); err != nil {
			panic("i18n: bad catalog " + code + ": " + err.Error())
		}
		out[code] = c
	}
	return out
}

// lookup — сообщение по (code,key) c fallback на Default и на сам ключ.
//
// Контракт (осознанный, не пересматривать без спеки): рендер НИКОГДА не
// падает и не паникует на отсутствующем ключе. Промах — это либо тихий
// fallback на локаль по умолчанию (страница показывает чужой язык), либо
// возврат самого ключа как строки (страница показывает сырой идентификатор
// вида "nav.issues"). Это защищает сторонних переводчиков, форкающих
// локаль под третий язык: незаконченный перевод не должен ронять страницу
// или отдавать 500. Оба случая промаха наблюдаемы — не молчаливы: см.
// recordMissingKey (missingkey.go) — log/slog.Warn с полями key/locale/stage
// (дедуп раз в минуту на одну и ту же тройку) и self-метрика
// gotcha_i18n_missing_key_total{locale,stage}, которая считает КАЖДЫЙ промах
// независимо от дедупликации лога (снимок — MissingKeyTotal).
//
// Тот же промах случается и для ключа, который есть только в секции
// "plurals" каталога, но вызван через T()/lookup вместо Tn()/pluralLookup —
// lookup смотрит только в Messages, поэтому такой ключ всегда учитывается
// как MissingKeyMissing (или fallback, если нашёлся в Messages дефолтной
// локали — на практике этого не бывает, раз ключ живёт в plurals).
func lookup(code, key string) string {
	if c, ok := catalogs[code]; ok {
		if v, ok := c.Messages[key]; ok {
			return v
		}
	}
	if code != Default.Code {
		if c, ok := catalogs[Default.Code]; ok {
			if v, ok := c.Messages[key]; ok {
				recordMissingKey(code, key, MissingKeyFallback)
				return v
			}
		}
	}
	recordMissingKey(code, key, MissingKeyMissing)
	return key
}
