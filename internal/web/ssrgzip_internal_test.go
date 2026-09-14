package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGzipSSRCompressesLargeHTML(t *testing.T) {
	body := []byte("<!DOCTYPE html><html><body>" + strings.Repeat("x", 2000) + "</body></html>")
	h := gzipSSR(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
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
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("распакованное тело не совпадает с исходным (err=%v)", err)
	}
	if cl := rec.Header().Get("Content-Length"); cl == "" {
		t.Error("Content-Length не выставлен для сжатого ответа")
	}
}

func TestGzipSSRSkipsSmallBody(t *testing.T) {
	small := []byte("<!DOCTYPE html><html>ok</html>")
	h := gzipSSR(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(small)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("Content-Encoding = %q для тела ниже порога", enc)
	}
	if !bytes.Equal(rec.Body.Bytes(), small) {
		t.Error("тело ниже порога должно уйти как есть")
	}
}

func TestGzipSSRSkipsNonHTMLContentType(t *testing.T) {
	big := bytes.Repeat([]byte(`{"key":"value"}`), 200)
	h := gzipSSR(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(big)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("JSON-ответ не должен сжиматься, Content-Encoding = %q", enc)
	}
	if !bytes.Equal(rec.Body.Bytes(), big) {
		t.Error("тело JSON-ответа не должно меняться при проходе через gzipSSR")
	}
}

func TestGzipSSRSkipsSniffedNonHTML(t *testing.T) {
	big := bytes.Repeat([]byte("id,value\n1,ok\n"), 100)
	h := gzipSSR(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Content-Type намеренно не выставлен — решение принимается по сниффингу тела.
		_, _ = w.Write(big)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("несниффленное как html тело не должно сжиматься, Content-Encoding = %q", enc)
	}
	if !bytes.Equal(rec.Body.Bytes(), big) {
		t.Error("тело изменилось при проходе через gzipSSR")
	}
}

func TestGzipSSRRespectsClientWithoutGzip(t *testing.T) {
	body := []byte("<!DOCTYPE html>" + strings.Repeat("y", 2000))
	h := gzipSSR(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("клиент без Accept-Encoding не должен получить gzip, Content-Encoding = %q", enc)
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Error("тело для клиента без gzip должно остаться исходным")
	}
}

func TestGzipSSRPreservesStatusCode(t *testing.T) {
	body := []byte("<!DOCTYPE html>" + strings.Repeat("z", 2000))
	h := gzipSSR(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(body)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("код ответа = %d, want 404", rec.Code)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("тело не является валидным gzip: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("распакованное тело не совпадает с исходным (err=%v)", err)
	}
}

// Поток и «накопить одним куском» дают одинаковые байты, различает их момент доставки:
// после Write получатель уже видит их, буфер пуст — иначе крупный файл уйдёт в память.
func TestGzipCapturePassthroughStreamsWithoutBuffering(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := &gzipCapture{ResponseWriter: rec}
	cw.Header().Set("Content-Type", "application/octet-stream")

	part1 := bytes.Repeat([]byte("a"), 600)
	if _, err := cw.Write(part1); err != nil {
		t.Fatalf("Write(part1): %v", err)
	}
	if cw.buf.Len() != 0 {
		t.Fatalf("после Write внутренний буфер держит %d байт, want 0 — passthrough не должен накапливать", cw.buf.Len())
	}
	if rec.Body.Len() != len(part1) {
		t.Fatalf("получатель увидел %d байт сразу после Write, want %d — байты должны уходить немедленно, не по завершении хендлера", rec.Body.Len(), len(part1))
	}

	part2 := bytes.Repeat([]byte("b"), 600)
	if _, err := cw.Write(part2); err != nil {
		t.Fatalf("Write(part2): %v", err)
	}
	if cw.buf.Len() != 0 {
		t.Fatalf("после второго Write внутренний буфер держит %d байт, want 0", cw.buf.Len())
	}
	want := append(append([]byte{}, part1...), part2...)
	if !bytes.Equal(rec.Body.Bytes(), want) {
		t.Error("бинарное тело исказилось при потоковой передаче")
	}
}

func TestGzipSSRPassthroughBypassesBuffering(t *testing.T) {
	part1 := bytes.Repeat([]byte("a"), 600)
	part2 := bytes.Repeat([]byte("b"), 600)
	h := gzipSSR(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "1200")
		_, _ = w.Write(part1)
		_, _ = w.Write(part2)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("бинарный ответ не должен сжиматься, Content-Encoding = %q", enc)
	}
	want := append(append([]byte{}, part1...), part2...)
	if !bytes.Equal(rec.Body.Bytes(), want) {
		t.Error("бинарное тело исказилось при проходе через gzipSSR")
	}
}

func TestGzipSSRKeepsHTMLContentTypeOnCompressed(t *testing.T) {
	body := []byte("<!DOCTYPE html><html><body>" + strings.Repeat("x", 2000) + "</body></html>")
	h := gzipSSR(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	// Без явного заголовка тип додумывается по gzip-байтам и выходит application/x-gzip,
	// а nosniff превращает страницу в скачиваемый файл.
	if ct := rec.Result().Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html*", ct)
	}
}

func TestGzipSSRRealServerDoesNotSniffGzipType(t *testing.T) {
	body := []byte("<!DOCTYPE html><html><body>" + strings.Repeat("x", 2000) + "</body></html>")
	srv := httptest.NewServer(gzipSSR(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	})))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("запрос: %v", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	// Transport сам не должен распаковывать — иначе заголовки ответа переписываются.
	res, err := (&http.Transport{DisableCompression: true}).RoundTrip(req)
	if err != nil {
		t.Fatalf("запрос: %v", err)
	}
	defer res.Body.Close()

	if enc := res.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html* (браузер скачает страницу файлом)", ct)
	}
}
