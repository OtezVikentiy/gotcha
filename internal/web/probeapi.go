package web

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

const (
	// probeMaxBodyBytes — лимит тела машинных ручек /probe/*: без сессии и
	// без sameOrigin единственная защита от заливки произвольного объёма —
	// жёсткий потолок (спека §4, Global Constraints плана).
	probeMaxBodyBytes = 1 << 20 // 1 MB
	// probeMaxResults — максимум результатов в одной пачке /probe/results.
	probeMaxResults = 100
	// probeDefaultLeaseLimit / probeMaxLeaseLimit — сколько заданий проба
	// получает за один lease, если не попросила иного, и потолок её запроса.
	probeDefaultLeaseLimit = 20
	probeMaxLeaseLimit     = 100
)

// probeAuth аутентифицирует пробу по Bearer-токену. Любая осечка (нет
// заголовка, не Bearer, неизвестный или отозванный токен) — одинаковый 401
// без подробностей: машинному клиенту незачем знать, чем именно его токен
// плох, а чужому — тем более. Сам токен не логируется никогда.
func (h *Handler) probeAuth(w http.ResponseWriter, r *http.Request) (uptime.Probe, bool) {
	auth := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || token == "" {
		writeProbeError(w, http.StatusUnauthorized, "unauthorized")
		return uptime.Probe{}, false
	}

	probe, err := h.Uptime.ProbeByToken(r.Context(), token)
	if errors.Is(err, uptime.ErrNotFound) {
		writeProbeError(w, http.StatusUnauthorized, "unauthorized")
		return uptime.Probe{}, false
	}
	if err != nil {
		slog.Error("probe api: token lookup failed", "error", err)
		writeProbeError(w, http.StatusInternalServerError, "internal error")
		return uptime.Probe{}, false
	}
	return probe, true
}

// probeLease — POST /probe/lease: выносная проба забирает задания своего
// региона. Публичный (без сессии и без sameOrigin) машинный эндпойнт — это
// не браузерная форма, а исходящий HTTPS-вызов пробы, для которого
// Bearer-токен — единственный и достаточный секрет (ср. heartbeat.go).
func (h *Handler) probeLease(w http.ResponseWriter, r *http.Request) {
	probe, ok := h.probeAuth(w, r)
	if !ok {
		return
	}

	var req uptime.LeaseRequest
	if !decodeProbeBody(w, r, &req) {
		return
	}

	limit := req.Limit
	if limit <= 0 {
		limit = probeDefaultLeaseLimit
	}
	if limit > probeMaxLeaseLimit {
		limit = probeMaxLeaseLimit
	}

	ctx := r.Context()
	if err := h.Uptime.TouchProbe(ctx, probe.ID); err != nil {
		slog.Error("probe api: touch probe failed", "probe_id", probe.ID, "error", err)
		writeProbeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	jobs, err := h.Uptime.LeaseForProbe(ctx, probe, limit)
	if err != nil {
		slog.Error("probe api: lease failed", "probe_id", probe.ID, "region", probe.Region, "error", err)
		writeProbeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	resp := uptime.LeaseResponse{
		ProbeID: probe.ID,
		Region:  probe.Region,
		Jobs:    make([]uptime.JobDTO, 0, len(jobs)),
	}
	for _, j := range jobs {
		resp.Jobs = append(resp.Jobs, uptime.NewJobDTO(j))
	}
	writeProbeJSON(w, http.StatusOK, resp)
}

// probeResults — POST /probe/results: приём пачки результатов от выносной
// пробы. Центр пробе не доверяет: время результата ставит он сам
// (time.Now().UTC()), а сам результат принимается, только если задание
// существует, выдано ИМЕННО ЭТОЙ пробе и её lease ещё жив (LeasedJob).
// Отвергнутый результат — не ошибка запроса: задание могло быть перевыдано
// после истечения lease, пока проба ходила по сети; такие просто считаются в
// rejected, а вся пачка остаётся 200.
//
// LeasedJob — только предварительная проверка (и источник Job.LeaseUntil):
// два одновременных запроса с одним queue_id оба её проходят. Право применить
// результат ровно один раз выдаёт claim внутри Ingestor.Accept — см. ClaimJob.
func (h *Handler) probeResults(w http.ResponseWriter, r *http.Request) {
	probe, ok := h.probeAuth(w, r)
	if !ok {
		return
	}
	if h.UptimeIngestor == nil {
		// Режим без Ingestor'а (собирается вызывающей стороной): принять
		// результат физически некому — обработать его должен центр целиком.
		slog.Error("probe api: results endpoint without an ingestor")
		writeProbeError(w, http.StatusServiceUnavailable, "results are not accepted by this node")
		return
	}

	var req uptime.ResultsRequest
	if !decodeProbeBody(w, r, &req) {
		return
	}
	if len(req.Results) > probeMaxResults {
		writeProbeError(w, http.StatusBadRequest, "too many results in one batch")
		return
	}

	ctx := r.Context()
	var resp uptime.ResultsResponse
	for _, res := range req.Results {
		job, err := h.Uptime.LeasedJob(ctx, res.QueueID, probe.ID)
		if errors.Is(err, uptime.ErrNotFound) {
			// Чужое, протухшее или уже выполненное задание — молча в rejected.
			resp.Rejected++
			continue
		}
		if err != nil {
			slog.Error("probe api: leased job lookup failed", "probe_id", probe.ID, "queue_id", res.QueueID, "error", err)
			writeProbeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if err := h.UptimeIngestor.Accept(ctx, job, time.Now().UTC(), res.Result()); err != nil {
			// БД споткнулась на этом результате. Если это случилось уже после
			// claim'а, результат потерян — монитор проверится заново, когда
			// планировщик поставит его в очередь по следующему сроку (см.
			// Ingestor.Accept). Остальную пачку это ронять не должно.
			slog.Error("probe api: accept result failed", "probe_id", probe.ID, "queue_id", res.QueueID, "error", err)
			resp.Rejected++
			continue
		}
		resp.Accepted++
	}

	writeProbeJSON(w, http.StatusOK, resp)
}

// decodeProbeBody читает тело запроса пробы под лимитом probeMaxBodyBytes.
// Пустое тело — не ошибка (v остаётся нулевым: /probe/lease без параметров
// — законный запрос «дай сколько дашь»).
func decodeProbeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, probeMaxBodyBytes)
	defer r.Body.Close()

	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeProbeError(w, http.StatusRequestEntityTooLarge, "body too large")
			return false
		}
		if errors.Is(err, io.EOF) {
			return true // пустое тело
		}
		writeProbeError(w, http.StatusBadRequest, "malformed json")
		return false
	}
	return true
}

func writeProbeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeProbeError(w http.ResponseWriter, status int, msg string) {
	writeProbeJSON(w, status, map[string]string{"error": msg})
}
