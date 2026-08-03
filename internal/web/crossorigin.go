package web

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
)

// coThrottleWindow — не чаще одной строки лога об отказах same-origin.
// Без троттлинга кросс-доменный флуд (CSRF-скан, кривой прокси) забивает
// диск — ротация логов в compose заведена ровно из-за таких путей. Подавленные
// отказы не теряются: их число уходит полем suppressed следующей строки и
// счётчиком gotcha_web_cross_origin_rejected_total.
const coThrottleWindow = 10 * time.Second

type coThrottle struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int64
}

// denyCrossOrigin — единственный ответ на несовпадение Origin с BaseURL.
// До него 58 из 60 веток отвечали голым http.Error("forbidden") без единой
// строки в логе: оператор видел 403 на регистрации при зелёном /readyz и
// пустом журнале (находка №37). Полученный Origin пишется в ЛОГ, но не
// отражается в страницу: недоверенное значение в HTML незачем.
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

// CrossOriginRejected — счётчик для gotcha_web_cross_origin_rejected_total.
func (h *Handler) CrossOriginRejected() int64 { return h.crossOriginRejected.Load() }
