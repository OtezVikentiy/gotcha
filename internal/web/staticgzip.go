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

// gzipMinBytes — ниже этого размера сжимать нет смысла: заголовок gzip и
// накладные расходы съедают выигрыш, а мелкие иконки и так укладываются в один
// сегмент TCP.
const gzipMinBytes = 1024

// compressibleExts — что сжимаем. Растровые форматы и всё, что уже сжато,
// в список не входят: повторное сжатие только тратит CPU.
var compressibleExts = map[string]bool{
	".css": true, ".js": true, ".svg": true, ".json": true,
	".map": true, ".txt": true, ".xml": true, ".ico": true,
}

// gzipAssets — предсжатые копии встроенной статики, путь → gzip-байты.
//
// Сжимаем ОДИН РАЗ при старте, а не на каждый запрос: содержимое встроено в
// бинарь через go:embed и не меняется, поэтому на запросе остаётся только
// отдать готовые байты. Измерено на app.css: 112 286 → 30 639 байт, минус 73%.
// Раньше статика уходила несжатой вообще — для SSR-приложения без бандлера это
// самая дешёвая крупная победа, и её отсутствие било по каждому первому
// открытию страницы.
type gzipAssets map[string][]byte

// buildGzipAssets предсжимает подходящие файлы. Ошибки чтения/сжатия не
// фатальны: такой файл просто отдаётся несжатым обычным FileServer.
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
			// Реального лика нет (writer оборачивает bytes.Buffer, не
			// файл/сокет), но закрываем на каждом пути — так же, как
			// успешный, а не только на нём: расходится с этой дисциплиной
			// проекта была бы единственным исключением в файле.
			_ = zw.Close()
			return nil
		}
		if err := zw.Close(); err != nil {
			return nil
		}
		// Если сжатие не дало выигрыша — не храним копию.
		if buf.Len() >= len(raw) {
			return nil
		}
		out[p] = buf.Bytes()
		return nil
	})
	return out
}

// serveGzip отдаёт предсжатую копию, когда клиент её принимает, и передаёт
// запрос дальше во всех остальных случаях.
//
// Vary: Accept-Encoding ставится ВСЕГДА, включая ответы без сжатия: без него
// промежуточный кэш мог бы отдать gzip-тело клиенту, который его не принимает.
func serveGzip(assets gzipAssets, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		body, ok := assets[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		// Тип обязателен: Content-Encoding уже задан, поэтому net/http не станет
		// досниффливать, а если бы стал — понюхал бы СЖАТЫЕ байты и выдал
		// application/x-gzip, который X-Content-Type-Options: nosniff заставит
		// браузер отвергнуть. Не разрешился тип — отдаём несжатую версию.
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

// acceptsGzip — принимает ли клиент gzip. Разбор простой, но с уважением к
// "gzip;q=0", которым клиент явно отказывается от кодирования.
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
