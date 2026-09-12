package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// Ниже этого размера накладные расходы gzip съедают выигрыш — мелкие иконки и так
// укладываются в один сегмент TCP.
const gzipMinBytes = 1024

// Растровые форматы и всё, что уже сжато, не входят: повторное сжатие только тратит CPU.
var compressibleExts = map[string]bool{
	".css": true, ".js": true, ".svg": true, ".json": true,
	".map": true, ".txt": true, ".xml": true, ".ico": true,
}

// Сжимаем один раз при старте — embed-контент не меняется, на запросе остаётся отдать готовые
// байты. Измерено на app.css: 112 286 → 30 639 байт (-73%).
type gzipAssets map[string][]byte

// Ошибки чтения/сжатия не фатальны: файл просто отдаётся несжатым обычным FileServer.
func buildGzipAssets(fsys fs.FS) gzipAssets {
	out := gzipAssets{}
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !compressibleExts[strings.ToLower(path.Ext(p))] {
			return nil
		}
		raw, err := fs.ReadFile(fsys, p)
		if err != nil || len(raw) < gzipMinBytes {
			return nil
		}
		var buf bytes.Buffer
		zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return nil
		}
		if _, err := zw.Write(raw); err != nil {
			// Реального лика нет (buffer, не файл/сокет), но закрываем на каждом пути — тот же
			// порядок, что и на успехе.
			_ = zw.Close()
			return nil
		}
		if err := zw.Close(); err != nil {
			return nil
		}
		if buf.Len() >= len(raw) {
			return nil
		}
		out[p] = buf.Bytes()
		return nil
	})
	return out
}

// Vary ставится ВСЕГДА, включая ответы без сжатия — иначе промежуточный кэш мог бы отдать
// gzip-тело клиенту, который его не принимает.
func serveGzip(assets gzipAssets, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		body, ok := assets[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		// Content-Encoding уже задан, net/http не досниффит — а досниффив, понюхал бы сжатые
		// байты и выдал application/x-gzip, который nosniff заставит браузер отвергнуть.
		ct := mime.TypeByExtension(path.Ext(r.URL.Path))
		if ct == "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, bytes.NewReader(body))
	})
}

// Разбор простой, но с уважением к "gzip;q=0" — им клиент явно отказывается от кодирования.
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(fields[0]), "gzip") {
			continue
		}
		for _, f := range fields[1:] {
			f = strings.TrimSpace(f)
			if strings.HasPrefix(f, "q=") {
				if q, err := strconv.ParseFloat(strings.TrimPrefix(f, "q="), 64); err == nil && q == 0 {
					return false
				}
			}
		}
		return true
	}
	return false
}
