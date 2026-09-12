package web

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// Тело не используется (успех = сам факт запроса), но лимит нужен: без него анонимный
// клиент мог бы залить произвольный объём в POST этого публичного эндпойнта.
const heartbeatMaxBodyBytes = 1 << 10 // 1 KB

// Ровно два значения — закрытый контракт self-метрики (после 1.0 менять дорого): не
// плодить кардинальность под каждый способ распознавания префетч-бота.
type HeartbeatIgnoreReason string

const (
	HeartbeatIgnorePrefetchHeader HeartbeatIgnoreReason = "prefetch_header"
	HeartbeatIgnoreBotUserAgent   HeartbeatIgnoreReason = "bot_user_agent"
)

// Причины перечислены заранее — счётчики создаются один раз, подсчёт на горячем пути
// атомарный, без блокировки и без записи в map.
var heartbeatIgnoreReasons = []HeartbeatIgnoreReason{
	HeartbeatIgnorePrefetchHeader,
	HeartbeatIgnoreBotUserAgent,
}

func HeartbeatIgnoreReasons() []HeartbeatIgnoreReason {
	return append([]HeartbeatIgnoreReason(nil), heartbeatIgnoreReasons...)
}

func newHeartbeatIgnoreCounters() map[HeartbeatIgnoreReason]*atomic.Int64 {
	m := make(map[HeartbeatIgnoreReason]*atomic.Int64, len(heartbeatIgnoreReasons))
	for _, r := range heartbeatIgnoreReasons {
		m[r] = new(atomic.Int64)
	}
	return m
}

// Процесс-локальные счётчики: публичный эндпойнт без сессии, self-телеметрия, не per-org учёт.
var heartbeatIgnoredCounts = newHeartbeatIgnoreCounters()

func countHeartbeatIgnored(reason HeartbeatIgnoreReason) {
	heartbeatIgnoredCounts[reason].Add(1)
}

func HeartbeatIgnoredBy(reason HeartbeatIgnoreReason) int64 {
	c, ok := heartbeatIgnoredCounts[reason]
	if !ok {
		return 0
	}
	return c.Load()
}

// Публично документированные User-Agent подстроки известных ботов превью ссылок — короткий
// список, ни один токен не пересекается с UA настоящего curl/wget/systemd.
var heartbeatUnfurlBotUserAgents = []string{
	"slackbot-linkexpanding",
	"telegrambot",
	"whatsapp",
	"facebookexternalhit",
	"twitterbot",
	"discordbot",
	"linkedinbot",
	"skypeuripreview",
	"redditbot",
	"viber",
	"vkshare",
	"mattermost",
}

// Sec-Purpose сравнивается ПРЕФИКСОМ ("prefetch;prerender" тоже считается), Purpose/X-Moz —
// точным "prefetch", X-Purpose — точным "preview". User-Agent — только если заголовки не сработали.
func heartbeatIgnoreReason(r *http.Request) (HeartbeatIgnoreReason, bool) {
	if v := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Purpose"))); strings.HasPrefix(v, "prefetch") {
		return HeartbeatIgnorePrefetchHeader, true
	}
	if v := strings.ToLower(strings.TrimSpace(r.Header.Get("Purpose"))); v == "prefetch" {
		return HeartbeatIgnorePrefetchHeader, true
	}
	if v := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Purpose"))); v == "preview" {
		return HeartbeatIgnorePrefetchHeader, true
	}
	if v := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Moz"))); v == "prefetch" {
		return HeartbeatIgnorePrefetchHeader, true
	}
	if ua := strings.ToLower(r.Header.Get("User-Agent")); ua != "" {
		for _, tok := range heartbeatUnfurlBotUserAgents {
			if strings.Contains(ua, tok) {
				return HeartbeatIgnoreBotUserAgent, true
			}
		}
	}
	return "", false
}

// Без авторизации и sameOrigin: внешний вызов (cron/systemd), токен в URL — единственный
// секрет. 404 — голый JSON, не стилизованная страница: эндпойнт машинный, зрителя нет.
func (h *Handler) heartbeat(w http.ResponseWriter, r *http.Request) {
	// Кап и дренаж — ДО отсева по заголовкам: те подделываются тривиально, и если бы кап
	// стоял после, подделка заголовка сняла бы ограничение размера тела.
	r.Body = http.MaxBytesReader(w, r.Body, heartbeatMaxBodyBytes)
	defer r.Body.Close()

	// MaxBytesReader ограничивает только чтение — сервер не дренирует тело сам; без явного
	// чтения здесь кап выше мёртв, и клиент зальёт неограниченное тело.
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeHeartbeatJSON(w, http.StatusRequestEntityTooLarge, false)
			return
		}
		writeHeartbeatJSON(w, http.StatusBadRequest, false)
		return
	}

	// Отсев — раньше токена и БД: отклонённый запрос не должен коснуться ничего в БД, только
	// лог и self-метрику. 204, не 404/200 — чтобы бот не счёл ссылку мёртвой и не ретраил.
	if reason, ignored := heartbeatIgnoreReason(r); ignored {
		countHeartbeatIgnored(reason)
		slog.Info("heartbeat: ignored prefetch/preview request", "reason", string(reason), "method", r.Method)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	ctx := r.Context()
	token := r.PathValue("token")

	m, err := h.Uptime.ByHeartbeatToken(ctx, token)
	if errors.Is(err, uptime.ErrNotFound) {
		writeHeartbeatJSON(w, http.StatusNotFound, false)
		return
	}
	if err != nil {
		slog.Error("heartbeat: lookup failed", "error", err)
		writeHeartbeatJSON(w, http.StatusInternalServerError, false)
		return
	}

	if err := h.Uptime.TouchHeartbeat(ctx, m.ID); err != nil {
		slog.Error("heartbeat: touch failed", "monitor_id", m.ID, "error", err)
		writeHeartbeatJSON(w, http.StatusInternalServerError, false)
		return
	}

	region := h.localRegion()
	at := time.Now().UTC()
	result := uptime.Result{OK: true}
	st, err := h.Uptime.ApplyResult(ctx, m.ID, region, true, "", at)
	if err != nil {
		slog.Error("heartbeat: apply result failed", "monitor_id", m.ID, "error", err)
		writeHeartbeatJSON(w, http.StatusInternalServerError, false)
		return
	}
	if h.UptimeWriter != nil {
		h.UptimeWriter.Add(m.ProjectID, m.ID, region, at, result)
	}

	// Без вызова OnResult монитор позеленеет, но открытый watchdog-инцидент останется висеть
	// навсегда: у heartbeat нет других сигналов, которые могли бы его закрыть.
	if h.UptimeIngestor != nil && h.UptimeIngestor.OnResult != nil {
		h.UptimeIngestor.OnResult(ctx, m, region, result, st)
	}

	writeHeartbeatJSON(w, http.StatusOK, true)
}

func writeHeartbeatJSON(w http.ResponseWriter, status int, ok bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": ok})
}
