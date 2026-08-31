package ingest

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
)

// deploymentsIngest принимает событие выкладки из CI: POST
// /api/v1/{project}/deployments с sentry_key-авторизацией и телом-JSON одного
// деплоя. Записывает в реестр деплоев (h.Deploy) и отвечает {id}. Аутентификация
// та же, что у envelope/store (project id из пути + public key), но вход
// server-to-server, поэтому без CORS.
func (h *Handler) deploymentsIngest(w http.ResponseWriter, r *http.Request) {
	key, ok := h.authenticate(w, r, SignalDeploy)
	if !ok {
		return
	}
	// Тот же per-DSN троттлинг, что у envelope/store: ключ — публичный sentry_key,
	// поэтому без rate-limit любой владелец DSN мог бы лить неограниченный поток
	// INSERT'ов в общую таблицу деплоев. Квоту деплои не расходуют (не биллинговая
	// телеметрия), достаточно rate-limit.
	if h.rateLimited(w, key.OrgID, key.ProjectID, SignalDeploy) {
		return
	}
	if h.Deploy == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "deployments disabled")
		return
	}
	body, closeBody, err := h.body(w, r)
	if err != nil {
		h.countRejected(RejectMalformed, SignalDeploy)
		writeJSONError(w, http.StatusBadRequest, "bad body")
		return
	}
	defer closeBody()

	var in struct {
		Version     string          `json:"version"`
		Environment string          `json:"environment"`
		DeployedAt  json.RawMessage `json:"deployed_at"`
		URL         string          `json:"url"`
		Changelog   string          `json:"changelog"`
	}
	if err := json.NewDecoder(body).Decode(&in); err != nil {
		// Тело в этой ветке приходит из того же http.MaxBytesReader, что у
		// остальных пяти входов (h.body), но раньше decode-ошибка отсюда всегда
		// отвечала 400 "malformed json" — даже когда тело превысило лимит и
		// декодер упёрся в *http.MaxBytesError. Клиент получал "битый JSON" за
		// собственный слишком большой пейлоад, а метрика не могла отличить одно
		// от другого. Проверка та же, что у остальных ReadAll/Parse-веток.
		var maxErr *http.MaxBytesError
		if errors.Is(err, ErrTooLarge) || errors.As(err, &maxErr) {
			h.countRejected(RejectTooLarge, SignalDeploy)
			writeJSONError(w, http.StatusRequestEntityTooLarge, "deployment too large")
			return
		}
		h.countRejected(RejectMalformed, SignalDeploy)
		writeJSONError(w, http.StatusBadRequest, "malformed json")
		return
	}
	if strings.TrimSpace(in.Version) == "" {
		writeJSONError(w, http.StatusBadRequest, "version required")
		return
	}

	d := deploy.Deployment{
		Version:     in.Version,
		Environment: in.Environment,
		URL:         in.URL,
		Changelog:   in.Changelog,
		DeployedAt:  parseDeployTime(in.DeployedAt),
	}
	saved, err := h.Deploy.Record(r.Context(), key.ProjectID, d)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "record failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"id": saved.ID})
}

// parseDeployTime разбирает поле deployed_at тела деплоя, допуская три формы от
// разных CI: RFC3339-строку ("2026-01-02T03:04:05Z"), Unix-секунды числом и
// отсутствие/null. При пустом или неразобранном значении возвращает нулевое
// время — Record подставит now(). Формально не звать parseNDJSONTimestampNs: он
// приватный и живёт в другом пакете (package log).
func parseDeployTime(raw json.RawMessage) time.Time {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return time.Time{}
	}
	// JSON-строка: снять кавычки честным декодом и разобрать как RFC3339.
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return time.Time{}
		}
		t, err := time.Parse(time.RFC3339, str)
		if err != nil {
			return time.Time{}
		}
		return t.UTC()
	}
	// JSON-число: Unix-секунды.
	if sec, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(sec, 0).UTC()
	}
	return time.Time{}
}
