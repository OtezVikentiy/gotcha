package web

import (
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/web/flashctx"
)

const flashCookie = "flash"

// Cookie, не query: параметр остался бы в адресе и всплывал при F5/из закладки.
// Тип живёт в flashctx — импорт из web сюда замкнул бы цикл.

// Белый список: то, чего тут нет, отбрасывается — значение cookie подконтрольно клиенту.
var flashKeys = map[string]bool{
	"flash.saved":             true,
	"flash.deleted":           true,
	"flash.invite_sent":       true,
	"flash.issues_resolved":   true,
	"flash.issues_ignored":    true,
	"flash.issues_reopened":   true,
	"flash.nothing_selected":  true,
	"flash.channel_created":   true,
	"flash.channel_updated":   true,
	"flash.channel_test_sent": true,
	"flash.team_deleted":      true,
	"flash.rules_saved":       true,
	"flash.escalations_saved": true,
	"flash.subject_purged":    true,
	"flash.invite_revoked":    true,
	"flash.export_requested":  true,
	// «Queued», не «удалено»: телеметрия ещё жива, чистит фоновый исполнитель — иначе
	// страница соврала бы об уже выполненной очистке.
	"flash.project_delete_queued": true,
	"flash.org_delete_queued":     true,
	"flash.recipes_applied":       true,
	// Раздельные ключи по состоянию: сообщение называет то, в чём монитор остался.
	"flash.monitor_paused":     true,
	"flash.monitor_resumed":    true,
	"flash.issue_status_saved": true,
	"flash.issue_assigned":     true,
	"flash.issue_unassigned":   true,
	// Раздельные ключи по действию — сообщение называет именно то, что произошло.
	"flash.log_filter_saved":       true,
	"flash.log_filter_updated":     true,
	"flash.log_filter_deleted":     true,
	"flash.log_filter_default_set": true,
	"flash.password_reset":         true,
}

// Ключи с ДВУМЯ числами в сообщении рендерятся через Tf с {n}/{m}, а не Tn с одним {n}.
// Ключ обязан быть и в flashKeys — этот список только выбирает способ рендера.
var flashPairKeys = map[string]bool{
	"flash.recipes_applied": true,
}

// Ключ — литерал из кода: непопадание в белый список логируется как ошибка, а не
// отбрасывается молча, как в parseFlash (тот разбирает подконтрольное клиенту).
func setFlash(w http.ResponseWriter, secure bool, kind, key string, n, m int) {
	if !flashKeys[key] {
		slog.Error("setFlash: ключ не найден в белом списке", "key", key)
		return
	}
	v := kind + "|" + key
	// n пишется даже нулевым, если m != 0 — позиция в cookie важна, parseFlash разбирает по индексам.
	if n != 0 || m != 0 {
		v += "|" + strconv.Itoa(n)
	}
	if m != 0 {
		v += "|" + strconv.Itoa(m)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    url.QueryEscape(v),
		Path:     "/",
		MaxAge:   60,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearFlash(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func parseFlash(raw string) *flashctx.Flash {
	v, err := url.QueryUnescape(raw)
	if err != nil {
		return nil
	}
	parts := strings.Split(v, "|")
	if len(parts) < 2 {
		return nil
	}
	kind, key := parts[0], parts[1]
	if kind != "ok" && kind != "warn" {
		return nil
	}
	if !flashKeys[key] {
		return nil
	}
	f := &flashctx.Flash{Kind: kind, Key: key, Pair: flashPairKeys[key]}
	if len(parts) > 2 {
		if n, err := strconv.Atoi(parts[2]); err == nil && n >= 0 {
			f.N = n
		}
	}
	if len(parts) > 3 {
		if m, err := strconv.Atoi(parts[3]); err == nil && m >= 0 {
			f.M = m
		}
	}
	return f
}

// Через контекст, не параметром layout: протаскивать через каждую сигнатуру страницы
// ради того, что к содержимому не относится, смысла нет — так же сделаны локаль и тема.
func (h *Handler) withFlash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(flashCookie)
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		clearFlash(w, h.secureCookies())
		f := parseFlash(c.Value)
		if f == nil {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(flashctx.With(r.Context(), f)))
	})
}

func (h *Handler) secureCookies() bool {
	return strings.HasPrefix(h.BaseURL, "https://")
}

func (h *Handler) flashOK(w http.ResponseWriter, key string, n int) {
	setFlash(w, h.secureCookies(), "ok", key, n, 0)
}

func (h *Handler) flashWarn(w http.ResponseWriter, key string, n int) {
	setFlash(w, h.secureCookies(), "warn", key, n, 0)
}

// key обязан быть в flashPairKeys — иначе рендер уйдёт в плюральную ветку, второе число потеряется.
func (h *Handler) flashOKPair(w http.ResponseWriter, key string, n, m int) {
	setFlash(w, h.secureCookies(), "ok", key, n, m)
}
