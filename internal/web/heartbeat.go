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

// heartbeatMaxBodyBytes — тело heartbeat-пинга нам не нужно (успех = сам
// факт запроса), но лимит всё равно нужен: без него клиент мог бы залить
// сколько угодно байт в POST-тело незалогиненного публичного эндпойнта.
const heartbeatMaxBodyBytes = 1 << 10 // 1 KB

// HeartbeatIgnoreReason — почему heartbeat-пинг отклонён БЕЗ учёта монитора
// как живого. URL пинга — простая ссылка вида /uptime/hb/{token}, а такие
// ссылки регулярно дёргают не люди: unfurl-бот мессенджера разворачивает
// превью пересланной ссылки, антивирусный прокси сканирует URL из письма,
// браузер префетчит ссылку под курсором — и каждый такой запрос выглядит для
// наивного приёмника как «сервис жив», гася настоящую тревогу watchdog'а.
//
// Значения — закрытый контракт self-метрики
// gotcha_uptime_heartbeat_ignored_total{reason}: после 1.0 менять набор
// дорого (сторож internal/guards требует, чтобы каждое имя метрики было
// задокументировано в self-monitoring.md обеих локалей). Ровно две причины,
// сознательно не детальнее:
//   - HeartbeatIgnorePrefetchHeader — жёсткий протокольный сигнал: сам
//     запрос НЕСЁТ заголовок, которым клиент явно помечает себя как
//     предварительную/неинтерактивную выборку (Sec-Purpose, Purpose,
//     X-Purpose, X-Moz). Спецификация это гарантирует — ложных срабатываний
//     на обычном curl/wget/cron here не бывает.
//   - HeartbeatIgnoreBotUserAgent — эвристика: User-Agent совпадает с
//     известным ботом построения превью ссылок в мессенджере/соцсети, у
//     которого протокольного заголовка нет. Список это открытый, может расти
//     со временем — но REASON остаётся один, чтобы не плодить кардинальность
//     метрики под каждый добавленный бот.
type HeartbeatIgnoreReason string

const (
	// HeartbeatIgnorePrefetchHeader — см. HeartbeatIgnoreReason.
	HeartbeatIgnorePrefetchHeader HeartbeatIgnoreReason = "prefetch_header"
	// HeartbeatIgnoreBotUserAgent — см. HeartbeatIgnoreReason.
	HeartbeatIgnoreBotUserAgent HeartbeatIgnoreReason = "bot_user_agent"
)

// heartbeatIgnoreReasons — полный набор причин отклонения, в стабильном
// порядке. Существует по той же причине, что и keyRejectReasons в
// internal/ingest: счётчики создаются один раз при инициализации, поэтому
// подсчёт на горячем пути — атомарный инкремент без блокировки и без записи
// в map.
var heartbeatIgnoreReasons = []HeartbeatIgnoreReason{
	HeartbeatIgnorePrefetchHeader,
	HeartbeatIgnoreBotUserAgent,
}

// HeartbeatIgnoreReasons — все причины, по которым heartbeat умеет отклонять
// пинг без учёта монитора живым. main регистрирует self-метрику по каждой
// причине (см. ingest.KeyRejectReasons).
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

// heartbeatIgnoredCounts — счётчики отклонённых пингов по причине,
// процесс-локальные (как keyRejected у ingest.Handler): heartbeat — публичный
// эндпойнт без сессии, это просто self-телеметрия процесса, не per-org учёт.
var heartbeatIgnoredCounts = newHeartbeatIgnoreCounters()

func countHeartbeatIgnored(reason HeartbeatIgnoreReason) {
	heartbeatIgnoredCounts[reason].Add(1)
}

// HeartbeatIgnoredBy — снимок счётчика отклонённых пингов по конкретной
// причине с начала процесса. Потокобезопасно и дёшево — self-метрики main
// читают его как func() int64 при каждом снятии показаний.
func HeartbeatIgnoredBy(reason HeartbeatIgnoreReason) int64 {
	c, ok := heartbeatIgnoredCounts[reason]
	if !ok {
		return 0
	}
	return c.Load()
}

// heartbeatUnfurlBotUserAgents — известные боты построения превью ссылок в
// мессенджерах/соцсетях, сверяемые как case-insensitive подстрока
// User-Agent. Источник — публично документированные строки User-Agent
// каждой площадки (Slack link unfurling, Telegram Bot API webhook preview,
// WhatsApp/Facebook sharing debugger, Twitter Card validator, Discord embeds,
// LinkedIn Post Inspector, Viber/VK/Skype/Mattermost/Reddit превью ссылок).
// Список сознательно короткий и специфичный: каждый токен — уникальное имя
// бота, ни один не пересекается с User-Agent настоящего curl/wget/systemd.
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

// heartbeatIgnoreReason решает, обязан ли этот запрос быть отклонён без учёта
// монитора живым, и почему. Заголовки — сильный сигнал первыми (нулевой шанс
// ложного срабатывания на реальном клиенте): значение Sec-Purpose
// сравнивается ПРЕФИКСОМ ("prefetch;prerender" и т.п. — тоже prefetch), Purpose
// и X-Moz — точным значением "prefetch", X-Purpose — точным значением
// "preview" (см. спека). User-Agent проверяется, только если ни один
// заголовок не сработал.
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

// heartbeat — GET|POST /uptime/hb/{token}: приём внешнего пинга от
// сервиса клиента (см. спека §3 «Heartbeat не планируется»). Без
// авторизации и без sameOrigin — это не браузерная форма, а произвольный
// внешний вызов (cron, systemd timer и т.п.), для которого токен в самом
// URL — единственный и достаточный секрет. Неизвестный токен отдаёт
// голый JSON 404, а не стилизованную страницу ошибок — это машинный
// эндпойнт, у которого нет человеческого зрителя.
func (h *Handler) heartbeat(w http.ResponseWriter, r *http.Request) {
	// Кап и дренаж тела — ПЕРЕД отсевом префетча/превью, а не после: заголовки
	// Sec-Purpose/Purpose/X-Purpose/X-Moz и User-Agent — это то, что клиент
	// сам о себе заявляет, подделать их тривиально (echo -H 'Sec-Purpose:
	// prefetch'). Если бы кап стоял после ветвления на игнор, любой аноним
	// добавлял бы один заголовок и заливал неограниченное тело в этот
	// публичный неаутентифицированный POST без единого похода в БД — сам
	// отсев от этого не пострадал бы (ответ отклонённого запроса как был,
	// так и остаётся не завязан на БД), а вот кап переставал бы работать
	// именно для тех запросов, что легче всего подделать. Кап — до похода в
	// БД, поэтому его перестановка выше отсева не противоречит цели
	// «отклонённый запрос не трогает БД»: он её не трогает и здесь.
	r.Body = http.MaxBytesReader(w, r.Body, heartbeatMaxBodyBytes)
	defer r.Body.Close()

	// MaxBytesReader only enforces its cap on read — nothing reads the body
	// otherwise (the stdlib server doesn't drain unread bodies against a
	// MaxBytesReader's limit on its own), so without this the 1 KB cap above
	// is dead code and a client can upload an arbitrarily large body to this
	// public, unauthenticated endpoint.
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeHeartbeatJSON(w, http.StatusRequestEntityTooLarge, false)
			return
		}
		writeHeartbeatJSON(w, http.StatusBadRequest, false)
		return
	}

	// Отсев префетча/превью — раньше токена и БД: отклонённый запрос не
	// должен трогать вообще ничего в БД (ни счётчики приёма, ни состояние
	// монитора) — только лог и self-метрику. 204 (не 404/200) — чтобы бот,
	// развернувший ссылку, не счёл её мёртвой и не начал ретраить.
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

	// Детектор обязателен именно здесь. Инцидент по heartbeat открывает
	// watchdog (пропущенный удар), а закрыть его больше некому: у heartbeat
	// нет ни очереди заданий, ни пробы — единственный сигнал «жив» приходит
	// сюда. Без этого вызова ApplyResult вернёт статус в up, монитор в UI
	// позеленеет, а инцидент останется открытым навсегда: ни уведомления о
	// восстановлении, ни конца напоминаниям «всё ещё DOWN».
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
