package web

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// heartbeatMaxBodyBytes — тело heartbeat-пинга нам не нужно (успех = сам
// факт запроса), но лимит всё равно нужен: без него клиент мог бы залить
// сколько угодно байт в POST-тело незалогиненного публичного эндпойнта.
const heartbeatMaxBodyBytes = 1 << 10 // 1 KB

// heartbeat — GET|POST /uptime/hb/{token}: приём внешнего пинга от
// сервиса клиента (см. спека §3 «Heartbeat не планируется»). Без
// авторизации и без sameOrigin — это не браузерная форма, а произвольный
// внешний вызов (cron, systemd timer и т.п.), для которого токен в самом
// URL — единственный и достаточный секрет. Неизвестный токен отдаёт
// голый JSON 404, а не стилизованную страницу ошибок — это машинный
// эндпойнт, у которого нет человеческого зрителя.
func (h *Handler) heartbeat(w http.ResponseWriter, r *http.Request) {
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
	if _, err := h.Uptime.ApplyResult(ctx, m.ID, region, true, "", at); err != nil {
		slog.Error("heartbeat: apply result failed", "monitor_id", m.ID, "error", err)
		writeHeartbeatJSON(w, http.StatusInternalServerError, false)
		return
	}
	if h.UptimeWriter != nil {
		h.UptimeWriter.Add(m.ProjectID, m.ID, region, at, result)
	}

	writeHeartbeatJSON(w, http.StatusOK, true)
}

func writeHeartbeatJSON(w http.ResponseWriter, status int, ok bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": ok})
}
