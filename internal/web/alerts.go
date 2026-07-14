package web

import (
	"errors"
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

// maxFailedDeliveries — сколько последних failed-записей показывать на
// странице алертов (spec §7). Ограничиваем, чтобы страница не разрослась на
// проектах с долгой историей отказов — это обзорная таблица, не полный лог.
const maxFailedDeliveries = 50

func alertsPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/alerts"
}

func alertsRulesPath(projectID int64) string {
	return alertsPath(projectID) + "/rules"
}

func alertsChannelsPath(projectID int64) string {
	return alertsPath(projectID) + "/channels"
}

func alertsChannelsDeletePath(projectID int64) string {
	return alertsChannelsPath(projectID) + "/delete"
}

// alertsErrorMessage переводит доменные ошибки alert.Service в
// человекочитаемое сообщение для 422-страницы алертов.
func alertsErrorMessage(err error) string {
	switch {
	case errors.Is(err, alert.ErrInvalidRule):
		return "недопустимое правило: у spike обязательны threshold и window (> 0), throttle не может быть отрицательным"
	case errors.Is(err, alert.ErrInvalidChannel):
		return "недопустимый канал: проверьте адрес/URL и обязательные поля выбранного типа"
	default:
		return "не удалось выполнить действие"
	}
}

// formBool — состояние HTML-чекбокса: присутствует в форме (обычно "on") —
// true, отсутствует — false. Невыбранный чекбокс браузер вообще не
// отправляет, поэтому пустая строка неотличима от отсутствия поля — этого
// достаточно.
func formBool(r *http.Request, name string) bool {
	return r.FormValue(name) != ""
}

// formInt — числовое поле формы; пустое значение или не-число трактуются как
// 0 (а не как ошибка запроса) — итоговую валидность решает уже
// alert.Service.UpsertRule/CreateChannel (ErrInvalidRule/ErrInvalidChannel).
func formInt(r *http.Request, name string) int {
	n, err := strconv.Atoi(r.FormValue(name))
	if err != nil {
		return 0
	}
	return n
}

// alertsPage — GET /projects/{id}/alerts: правила (new_issue/regression:
// enabled+throttle; spike: enabled+threshold+window+throttle) одной формой и
// таблица каналов доставки с формой добавления. Доступ только owner/admin
// организации проекта (requireProjectRole).
func (h *Handler) alertsPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := parsePathProjectID(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	h.renderAlerts(w, r, http.StatusOK, projectID, "")
}

// renderAlerts — общий рендер: GET-обработчик и все POST в этом файле на 422
// (то же сообщение на месте, без редиректа — тот же принцип, что и у
// renderProjectSettings/renderOrgSettings).
func (h *Handler) renderAlerts(w http.ResponseWriter, r *http.Request, status int, projectID int64, errMsg string) {
	rules, err := h.Alerts.Rules(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	channels, err := h.Alerts.Channels(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	// Outbox может быть не проставлен (например, в узких тестовых стендах,
	// не относящихся к алертам) — тогда просто не показываем секцию
	// failed-доставок, не роняя страницу.
	var failed []notify.FailedJob
	if h.Outbox != nil {
		failed, err = h.Outbox.FailedForProject(r.Context(), projectID, maxFailedDeliveries)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
	}
	w.WriteHeader(status)
	_ = templates.Alerts(projectID, rules, channels, failed, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

// alertsRulesSave — POST /projects/{id}/alerts/rules: одна форма сохраняет
// все три kind разом (UpsertRule по каждому). Правила применяются в
// фиксированном порядке (new_issue, regression, spike); первая же ошибка
// (ErrInvalidRule — в первую очередь ожидается от spike: Threshold/Window
// должны быть > 0) прерывает сохранение остальных ещё не применённых правил
// и рендерит форму с 422 — но alert.Service не даёт кросс-правильной
// транзакции, поэтому уже применённые до ошибки правила в этом вызове
// остаются сохранёнными (тот же trade-off, что и у остальных
// multi-step-форм этого пакета — здесь неатомарность безопасна, так как
// каждое правило само по себе валидно на момент своего UpsertRule).
func (h *Handler) alertsRulesSave(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := parsePathProjectID(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	rules := []alert.Rule{
		{
			ProjectID:       projectID,
			Kind:            alert.KindNewIssue,
			Enabled:         formBool(r, "new_issue_enabled"),
			ThrottleMinutes: formInt(r, "new_issue_throttle"),
		},
		{
			ProjectID:       projectID,
			Kind:            alert.KindRegression,
			Enabled:         formBool(r, "regression_enabled"),
			ThrottleMinutes: formInt(r, "regression_throttle"),
		},
		{
			ProjectID:       projectID,
			Kind:            alert.KindSpike,
			Enabled:         formBool(r, "spike_enabled"),
			Threshold:       formInt(r, "spike_threshold"),
			WindowMinutes:   formInt(r, "spike_window"),
			ThrottleMinutes: formInt(r, "spike_throttle"),
		},
	}
	for _, rule := range rules {
		if _, err := h.Alerts.UpsertRule(r.Context(), rule); err != nil {
			h.renderAlerts(w, r, http.StatusUnprocessableEntity, projectID, alertsErrorMessage(err))
			return
		}
	}
	http.Redirect(w, r, alertsPath(projectID), http.StatusSeeOther)
}

// alertsChannelCreate — POST /projects/{id}/alerts/channels: kind, target,
// secret, enabled. ErrInvalidChannel (email — не-адрес; webhook — не
// http(s)-URL; telegram — пустые chat_id/bot token) → 422.
func (h *Handler) alertsChannelCreate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := parsePathProjectID(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	c := alert.Channel{
		ProjectID: projectID,
		Kind:      r.FormValue("kind"),
		Enabled:   formBool(r, "enabled"),
		Target:    r.FormValue("target"),
		Secret:    r.FormValue("secret"),
	}
	if _, err := h.Alerts.CreateChannel(r.Context(), c); err != nil {
		h.renderAlerts(w, r, http.StatusUnprocessableEntity, projectID, alertsErrorMessage(err))
		return
	}
	http.Redirect(w, r, alertsPath(projectID), http.StatusSeeOther)
}

// channelBelongsToProject — тот же приём, что и keyBelongsToProject
// (projsettings.go): не даём удалить канал, принадлежащий чужому проекту, по
// подобранному id.
func channelBelongsToProject(channels []alert.Channel, channelID int64) bool {
	for _, c := range channels {
		if c.ID == channelID {
			return true
		}
	}
	return false
}

// alertsChannelDelete — POST /projects/{id}/alerts/channels/delete:
// channel_id. Канал должен принадлежать проекту из пути, иначе 404.
func (h *Handler) alertsChannelDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := parsePathProjectID(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	channelID, err := strconv.ParseInt(r.FormValue("channel_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad channel_id", http.StatusBadRequest)
		return
	}
	channels, err := h.Alerts.Channels(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if !channelBelongsToProject(channels, channelID) {
		h.renderError(w, r, http.StatusNotFound, "Страница не найдена")
		return
	}
	if err := h.Alerts.DeleteChannel(r.Context(), channelID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	http.Redirect(w, r, alertsPath(projectID), http.StatusSeeOther)
}
