package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func maintenancePath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/maintenance"
}

func maintenanceDeletePath(projectID int64) string {
	return maintenancePath(projectID) + "/delete"
}

// maintenanceDateTimeLayouts — форматы, которые отдаёт нативный
// <input type="datetime-local">: обычно без секунд, но некоторые браузеры
// добавляют ":00" — принимаем оба.
var maintenanceDateTimeLayouts = []string{"2006-01-02T15:04", "2006-01-02T15:04:05"}

// parseLocalDateTime разбирает значение datetime-local как настенное время в
// loc (не UTC) — так «начало 10:00» в форме с выбранным Europe/Moscow
// действительно означает 10:00 по Москве, а не 10:00 UTC.
func parseLocalDateTime(raw string, loc *time.Location) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range maintenanceDateTimeLayouts {
		if t, err := time.ParseInLocation(layout, raw, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// maintenanceTimezone — выбранный TZ из фиксированного select'а либо (если
// выбран пункт "другой", отправляющий пустое значение) — свободный текст
// IANA-имени из соседнего поля.
func maintenanceTimezone(r *http.Request) string {
	tz := r.FormValue("timezone")
	if tz != "" {
		return tz
	}
	return strings.TrimSpace(r.FormValue("timezone_custom"))
}

// parseMaintenanceForm собирает uptime.Window из уже распарсенной формы
// (r.ParseForm должен быть вызван вызывающей стороной). Невалидный
// datetime-local (или его отсутствие) оставляет StartsAt/EndsAt = nil —
// validateWindow на стороне uptime.Service отклонит такое окно как
// ErrInvalidWindow, а не запаникует.
func parseMaintenanceForm(r *http.Request, projectID int64) uptime.Window {
	// «kind» — radio: oneoff|weekly. Тип окна взаимоисключающий, поэтому это
	// выбор одного из двух, а не флаг (раньше был чекбокс «weekly»).
	weekly := r.FormValue("kind") == "weekly"
	tz := maintenanceTimezone(r)

	w := uptime.Window{
		ProjectID: projectID,
		Name:      strings.TrimSpace(r.FormValue("name")),
		Weekly:    weekly,
		Timezone:  tz,
	}
	if weekly {
		w.Weekday = formInt(r, "weekday")
		w.StartTime = r.FormValue("start_time")
		w.EndTime = r.FormValue("end_time")
		return w
	}

	loc, err := time.LoadLocation(tz)
	if err != nil {
		// Заведомо невалидный TZ всё равно приведёт к ErrInvalidWindow
		// (validateWindow сам зовёт time.LoadLocation) — здесь UTC нужен
		// только чтобы распарсить сами даты, не уронив обработчик.
		loc = time.UTC
	}
	if starts, ok := parseLocalDateTime(r.FormValue("starts_at"), loc); ok {
		w.StartsAt = &starts
	}
	if ends, ok := parseLocalDateTime(r.FormValue("ends_at"), loc); ok {
		w.EndsAt = &ends
	}
	return w
}

func maintenanceErrorMessage(ctx context.Context, err error) string {
	if errors.Is(err, uptime.ErrInvalidWindow) {
		return i18n.Tf(ctx, "error.maintenance.invalid_window", "detail", err.Error())
	}
	return i18n.T(ctx, "error.action_failed")
}

// windowBelongsToProject — тот же приём, что и keyBelongsToProject/
// channelBelongsToProject: не даём удалить окно чужого проекта по
// подобранному id.
func windowBelongsToProject(windows []uptime.Window, windowID int64) bool {
	for _, w := range windows {
		if w.ID == windowID {
			return true
		}
	}
	return false
}

// maintenancePage — GET /projects/{id}/maintenance: список окон + форма
// создания. Доступ только owner/admin организации проекта
// (requireProjectRole) — управление окнами обслуживания меняет то, что
// детектор считает даунтаймом, это не read-only страница вроде инцидентов.
func (h *Handler) maintenancePage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	// renderMaintenance дереференсит h.Uptime (окна обслуживания) — в стендах
	// без подсистемы мониторинга 404, а не паника (тот же guard, что и в
	// metricsList).
	if h.Uptime == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	h.renderMaintenance(w, r, http.StatusOK, projectID, "")
}

// renderMaintenance — общий рендер: GET-обработчик и оба POST в этом файле
// на 422 (то же сообщение на месте, без редиректа — тот же принцип, что и у
// renderAlerts/renderProjectSettings).
func (h *Handler) renderMaintenance(w http.ResponseWriter, r *http.Request, status int, projectID int64, errMsg string) {
	windows, err := h.Uptime.Windows(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	w.WriteHeader(status)
	_ = templates.Maintenance(projectID, windows, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

// maintenanceCreate — POST /projects/{id}/maintenance: sameOrigin +
// requireProjectRole, разовое либо еженедельное окно, ErrInvalidWindow ->
// 422 с сообщением (список уже сохранённых окон + форма создания
// перерисовываются, как у renderAlerts — конкретные введённые значения формы
// не переносятся, это не требуется спекой задачи).
func (h *Handler) maintenanceCreate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Uptime == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	win := parseMaintenanceForm(r, projectID)
	if _, err := h.Uptime.CreateWindow(r.Context(), win); err != nil {
		if errors.Is(err, uptime.ErrInvalidWindow) {
			h.renderMaintenance(w, r, http.StatusUnprocessableEntity, projectID, maintenanceErrorMessage(r.Context(), err))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, maintenancePath(projectID), http.StatusSeeOther)
}

// maintenanceDelete — POST /projects/{id}/maintenance/delete: window_id.
// Окно должно принадлежать проекту из пути, иначе 404 — тот же принцип, что
// и у alertsChannelDelete/projectSettingsKeyRevoke.
func (h *Handler) maintenanceDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Uptime == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	windowID, err := strconv.ParseInt(r.FormValue("window_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad window_id", http.StatusBadRequest)
		return
	}

	windows, err := h.Uptime.Windows(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !windowBelongsToProject(windows, windowID) {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return
	}
	if err := h.Uptime.DeleteWindow(r.Context(), windowID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, maintenancePath(projectID), http.StatusSeeOther)
}
