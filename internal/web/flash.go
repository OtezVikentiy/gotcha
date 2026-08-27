package web

import (
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/web/flashctx"
)

// flashCookie — имя cookie, переносящей сообщение через редирект.
const flashCookie = "flash"

// Сообщение переносится cookie, а не query-параметром, ровно по одной причине:
// параметр остаётся в адресе, и сообщение залипает при F5 и уезжает в закладку.
// Cookie читается и гасится тем же запросом, поэтому показывается ровно один раз.
//
// Сам тип живёт в листовом пакете flashctx: его читает слой шаблонов, а тот
// импортируется из web — обратный импорт замкнул бы цикл.

// flashKeys — белый список ключей, которые разрешено показывать. Всё, чего тут
// нет, отбрасывается: значение cookie полностью подконтрольно клиенту.
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
	// Удаление проекта и организации сообщает про ОЧЕРЕДЬ, а не про
	// выполненную очистку: телеметрия в ClickHouse на момент ответа ещё жива,
	// её удаляет фоновый исполнитель. Сказать «удалено» здесь означало бы
	// повторить исходный дефект — страница успеха при невыполненной работе.
	"flash.project_delete_queued": true,
	"flash.org_delete_queued":     true,
	"flash.recipes_applied":       true,
}

// flashPairKeys — подмножество flashKeys с ДВУМЯ числами в сообщении
// («создано N, пропущено M»): плюральный Tn несёт только одно {n}, поэтому
// такие ключи рендерятся плоским переводом с подстановкой {n}/{m} через Tf
// (см. flashView). Ключ обязан состоять и в flashKeys: этот список только
// выбирает способ рендера, белый список — один.
var flashPairKeys = map[string]bool{
	"flash.recipes_applied": true,
}

// setFlash кладёт сообщение в cookie перед редиректом. Path=/ — сообщение может
// показаться на любой странице, куда ведёт редирект. MaxAge короткий: если
// показать не удалось (пользователь закрыл вкладку), оно не всплывёт через час.
//
// Ключ сюда приходит из кода, литералом — в отличие от parseFlash (строка 93),
// которая разбирает значение, целиком подконтрольное клиенту, и потому обязана
// отбрасывать подделку молча. Здесь молчание означало бы прятать опечатку или
// забытый в списке ключ: сообщение просто не появится, и отличить это от
// несработавшей формы будет нечем — ровно так годами жила находка про отзыв
// приглашения (flash.invite.revoked не было в списке).
func setFlash(w http.ResponseWriter, secure bool, kind, key string, n, m int) {
	if !flashKeys[key] {
		slog.Error("setFlash: ключ не найден в белом списке", "key", key)
		return
	}
	v := kind + "|" + key
	// Для парного ключа с m != 0 значение n пишется даже нулевым — позиция
	// в cookie важна (kind|key|n|m), parseFlash разбирает по индексам.
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

// clearFlash гасит cookie. Вызывается сразу при чтении, поэтому сообщение
// показывается ровно один раз и переживает F5 корректно (второй показ не
// случится).
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

// parseFlash разбирает значение cookie. Возвращает nil на всём, чего нет в
// белом списке.
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

// withFlash достаёт сообщение из cookie, гасит её и кладёт сообщение в контекст.
//
// Через контекст, а не параметром layout: layout зовут все страницы продукта, и
// протаскивать сообщение через каждую сигнатуру значило бы менять их все ради
// того, что к содержимому страницы отношения не имеет. Тем же приёмом сделаны
// локаль и тема.
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

// secureCookies — ставить ли Secure на служебные cookie. То же правило, что у
// сессионной: HTTPS-инстанс.
func (h *Handler) secureCookies() bool {
	return strings.HasPrefix(h.BaseURL, "https://")
}

// flashOK/flashWarn — сокращения для обработчиков.
func (h *Handler) flashOK(w http.ResponseWriter, key string, n int) {
	setFlash(w, h.secureCookies(), "ok", key, n, 0)
}

func (h *Handler) flashWarn(w http.ResponseWriter, key string, n int) {
	setFlash(w, h.secureCookies(), "warn", key, n, 0)
}

// flashOKPair — сообщение с двумя счётчиками («создано N, пропущено M»):
// key обязан состоять в flashPairKeys, иначе рендер уйдёт в плюральную
// ветку и второе число потеряется.
func (h *Handler) flashOKPair(w http.ResponseWriter, key string, n, m int) {
	setFlash(w, h.secureCookies(), "ok", key, n, m)
}
