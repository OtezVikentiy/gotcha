package web

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

const coThrottleWindow = 10 * time.Second

type coThrottle struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int64
}

// Origin пишется в лог, но не в страницу: недоверенное значение в HTML незачем.
func (h *Handler) denyCrossOrigin(w http.ResponseWriter, r *http.Request) {
	h.crossOriginRejected.Add(1)
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	h.coThrottle.mu.Lock()
	now := time.Now()
	if now.Sub(h.coThrottle.last) >= coThrottleWindow {
		suppressed := h.coThrottle.suppressed
		h.coThrottle.suppressed = 0
		h.coThrottle.last = now
		h.coThrottle.mu.Unlock()
		slog.Warn("web: cross-origin request rejected",
			"origin", src, "base_url", h.BaseURL,
			"method", r.Method, "path", r.URL.Path,
			"suppressed", suppressed)
	} else {
		h.coThrottle.suppressed++
		h.coThrottle.mu.Unlock()
	}
	h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.cross_origin"))
}

func (h *Handler) CrossOriginRejected() int64 { return h.crossOriginRejected.Load() }
