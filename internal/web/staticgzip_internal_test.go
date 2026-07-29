package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestAcceptsGzip(t *testing.T) {
	cases := map[string]bool{
		"":                            false,
		"deflate":                     false,
		"gzip":                        true,
		"gzip, deflate, br":           true,
		"br;q=1.0, gzip;q=0.8":        true,
		"gzip;q=0":                    false, // клиент явно отказался
		"identity;q=1, gzip;q=0":      false,
		"GZIP":                        true, // регистр не важен
		" gzip ":                      true,
		"deflate;q=1.0, gzip;q=0.001": true,
	}
	for header, want := range cases {
		if got := acceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}

// TestServeGzipCompressesStatic — статика уходила несжатой вообще: измерено
// 112 286 байт на app.css там, где gzip даёт 30 639 (−73%). Для SSR-приложения
// без бандлера это самая дешёвая крупная победа.
func TestServeGzipCompressesStatic(t *testing.T) {
	big := bytes.Repeat([]byte("body { color: red; }\n"), 500) // хорошо сжимается
	fsys := fstest.MapFS{
		"app.css":  {Data: big},
		"tiny.css": {Data: []byte("a{}")}, // ниже порога — не сжимаем
		"logo.png": {Data: bytes.Repeat([]byte{0x89, 0x50}, 2000)},
	}
	assets := buildGzipAssets(fsys)
	if _, ok := assets["app.css"]; !ok {
		t.Fatal("app.css не предсжат")
	}
	if _, ok := assets["tiny.css"]; ok {
		t.Error("файл ниже порога не должен сжиматься")
	}
	if _, ok := assets["logo.png"]; ok {
		t.Error("растровый формат не должен сжиматься повторно")
	}
	if len(assets["app.css"]) >= len(big) {
		t.Fatalf("сжатие не дало выигрыша: %d >= %d", len(assets["app.css"]), len(big))
	}

	plain := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(big)
	})
	h := serveGzip(assets, plain)

	// Клиент принимает gzip → сжатое тело, корректно распаковывается.
	req := httptest.NewRequest(http.MethodGet, "/app.css", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	if v := rec.Header().Get("Vary"); v != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", v)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("тело не является валидным gzip: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("распакованное тело не совпадает с исходным (err=%v)", err)
	}

	// Клиент НЕ принимает gzip → исходное тело, без Content-Encoding.
	req2 := httptest.NewRequest(http.MethodGet, "/app.css", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if enc := rec2.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("Content-Encoding = %q для клиента без gzip", enc)
	}
	if !bytes.Equal(rec2.Body.Bytes(), big) {
		t.Error("клиент без gzip должен получить исходное тело")
	}
	// Vary обязателен и здесь: иначе промежуточный кэш отдаст сжатое тело
	// клиенту, который его не принимает.
	if v := rec2.Header().Get("Vary"); v != "Accept-Encoding" {
		t.Errorf("Vary = %q в несжатом ответе, want Accept-Encoding", v)
	}
}
