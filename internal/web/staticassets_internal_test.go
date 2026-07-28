package web

import (
	"testing"
	"testing/fstest"
)

// TestStaticAssetVersion: хэш детерминирован и чувствителен к содержимому и
// именам — иначе кэш-бастинг не сработал бы после деплоя (тот же URL на новый
// CSS). Порядок обхода FS не влияет (имена сортируются), длина — 12 hex.
func TestStaticAssetVersion(t *testing.T) {
	base := fstest.MapFS{
		"app.css":       {Data: []byte("body{color:red}")},
		"daterange.js":  {Data: []byte("console.log(1)")},
		"icons/one.svg": {Data: []byte("<svg/>")},
	}
	h1 := staticAssetVersion(base)
	if len(h1) != 12 {
		t.Fatalf("hash length = %d, want 12 (%q)", len(h1), h1)
	}
	for _, r := range h1 {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("hash not hex: %q", h1)
		}
	}

	// Тот же контент (другой литерал карты) → тот же хэш.
	same := fstest.MapFS{
		"app.css":       {Data: []byte("body{color:red}")},
		"daterange.js":  {Data: []byte("console.log(1)")},
		"icons/one.svg": {Data: []byte("<svg/>")},
	}
	if h2 := staticAssetVersion(same); h2 != h1 {
		t.Errorf("identical content gave different hash: %q != %q", h2, h1)
	}

	// Изменённый байт содержимого → другой хэш.
	changed := fstest.MapFS{
		"app.css":       {Data: []byte("body{color:blue}")},
		"daterange.js":  {Data: []byte("console.log(1)")},
		"icons/one.svg": {Data: []byte("<svg/>")},
	}
	if staticAssetVersion(changed) == h1 {
		t.Error("content change did not change the hash")
	}

	// Переименованный файл (тот же контент) → другой хэш (имя тоже хэшируется).
	renamed := fstest.MapFS{
		"app.css":       {Data: []byte("body{color:red}")},
		"daterange.js":  {Data: []byte("console.log(1)")},
		"icons/two.svg": {Data: []byte("<svg/>")}, // one.svg → two.svg
	}
	if staticAssetVersion(renamed) == h1 {
		t.Error("rename did not change the hash")
	}

	// Пустая FS всё равно даёт валидный 12-hex хэш (срез [:12] безопасен).
	if h := staticAssetVersion(fstest.MapFS{}); len(h) != 12 {
		t.Errorf("empty FS hash = %q, want 12 hex chars", h)
	}
}
