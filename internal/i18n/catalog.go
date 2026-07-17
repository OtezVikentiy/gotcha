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
func lookup(code, key string) string {
	if c, ok := catalogs[code]; ok {
		if v, ok := c.Messages[key]; ok {
			return v
		}
	}
	if code != Default.Code {
		if c, ok := catalogs[Default.Code]; ok {
			if v, ok := c.Messages[key]; ok {
				return v
			}
		}
	}
	return key
}
