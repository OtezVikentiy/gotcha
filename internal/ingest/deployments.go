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

func (h *Handler) deploymentsIngest(w http.ResponseWriter, r *http.Request) {
	key, ok := h.authenticate(w, r, SignalDeploy)
	if !ok {
		return
	}
	// Тот же rate-limit, что у envelope/store: без него владелец DSN мог бы лить
	// неограниченный поток INSERT'ов. Квоту деплои не расходуют, это не биллинговая телеметрия.
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
		// Отличаем MaxBytesError от битого JSON: иначе большой пейлоад тоже
		// считался бы malformed json.
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

// Допускает RFC3339, Unix-секунды или пусто/null; иначе — нулевое время,
// Record тогда подставит now().
func parseDeployTime(raw json.RawMessage) time.Time {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return time.Time{}
	}
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
	if sec, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(sec, 0).UTC()
	}
	return time.Time{}
}
