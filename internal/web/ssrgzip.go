package web

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

// Бинарные ответы (выгрузки, агент) узнаются по Content-Type, выставленному до первого
// Write, и уходят без буферизации — не задеть Range/большие файлы.
type gzipCapture struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	decided     bool
	passthrough bool
	buf         bytes.Buffer
}

// Unwrap пропускает http.NewResponseController (SetWriteDeadline и т.п.) к реальному
// ResponseWriter — agentdist продлевает таймаут записи для крупных бинарей.
func (c *gzipCapture) Unwrap() http.ResponseWriter {
	return c.ResponseWriter
}

func (c *gzipCapture) WriteHeader(status int) {
	if !c.wroteHeader {
		c.status = status
		c.wroteHeader = true
	}
}

func (c *gzipCapture) Write(p []byte) (int, error) {
	if !c.wroteHeader {
		c.status = http.StatusOK
		c.wroteHeader = true
	}
	if !c.decided {
		c.decided = true
		if ct := c.ResponseWriter.Header().Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "text/html") {
			c.passthrough = true
			c.ResponseWriter.WriteHeader(c.status)
		}
	}
	if c.passthrough {
		return c.ResponseWriter.Write(p)
	}
	return c.buf.Write(p)
}

// gzipSSR сжимает html-ответы страниц так же, как serveGzip — предсжатую статику.
func gzipSSR(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /static/ ставит свой Vary через serveGzip — здесь задвоился бы.
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			w.Header().Add("Vary", "Accept-Encoding")
		}
		cw := &gzipCapture{ResponseWriter: w}
		next.ServeHTTP(cw, r)
		if cw.passthrough {
			return
		}
		if !cw.wroteHeader {
			cw.status = http.StatusOK
		}
		body := cw.buf.Bytes()
		ct := w.Header().Get("Content-Type")
		if ct == "" {
			ct = http.DetectContentType(body)
		}
		if w.Header().Get("Content-Encoding") != "" || !strings.HasPrefix(ct, "text/html") ||
			len(body) < gzipMinBytes || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			w.WriteHeader(cw.status)
			_, _ = w.Write(body)
			return
		}
		var buf bytes.Buffer
		zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			w.WriteHeader(cw.status)
			_, _ = w.Write(body)
			return
		}
		_, _ = zw.Write(body)
		_ = zw.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
		w.WriteHeader(cw.status)
		_, _ = w.Write(buf.Bytes())
	})
}
