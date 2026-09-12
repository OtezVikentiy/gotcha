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

// некоторые браузеры добавляют ":00" к datetime-local — принимаем оба формата.
var maintenanceDateTimeLayouts = []string{"2006-01-02T15:04", "2006-01-02T15:04:05"}

// время трактуется как настенное в loc, не UTC — иначе «начало 10:00» с выбранным
// Europe/Moscow стало бы 10:00 UTC.
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

func maintenanceTimezone(r *http.Request) string {
	tz := r.FormValue("timezone")
	if tz != "" {
		return tz
	}
	return strings.TrimSpace(r.FormValue("timezone_custom"))
}

// второй результат — отмечен ли чекбокс «без даты окончания»: вызывающий обязан
// проверить end_required им до CreateWindow/UpdateWindow.
func parseMaintenanceForm(r *http.Request, projectID int64) (uptime.Window, bool) {
	weekly := r.FormValue("kind") == "weekly"
	tz := maintenanceTimezone(r)
	indefinite := r.FormValue("indefinite") != ""

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
		return w, indefinite
	}

	loc, err := time.LoadLocation(tz)
	if err != nil {
		// невалидный TZ всё равно даст ErrInvalidWindow дальше — UTC нужен только
		// распарсить сами даты, не уронив обработчик.
		loc = time.UTC
	}
	if starts, ok := parseLocalDateTime(r.FormValue("starts_at"), loc); ok {
		w.StartsAt = &starts
	}
	if !indefinite {
		if ends, ok := parseLocalDateTime(r.FormValue("ends_at"), loc); ok {
			w.EndsAt = &ends
		}
	}
	return w, indefinite
}

func maintenanceErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, uptime.ErrInvalidWindowName):
		return i18n.T(ctx, "error.maintenance.invalid_name")
	case errors.Is(err, uptime.ErrInvalidWindowTimezone):
		return i18n.T(ctx, "error.maintenance.invalid_timezone")
	case errors.Is(err, uptime.ErrInvalidWindowWeekday):
		return i18n.T(ctx, "error.maintenance.invalid_weekday")
	case errors.Is(err, uptime.ErrInvalidWindowStartTime):
		return i18n.T(ctx, "error.maintenance.invalid_start_time")
	case errors.Is(err, uptime.ErrInvalidWindowEndTime):
		return i18n.T(ctx, "error.maintenance.invalid_end_time")
	case errors.Is(err, uptime.ErrInvalidWindowSameTime):
		return i18n.T(ctx, "error.maintenance.same_start_end")
	case errors.Is(err, uptime.ErrInvalidWindowRange):
		return i18n.T(ctx, "error.maintenance.invalid_range")
	case errors.Is(err, uptime.ErrInvalidWindow):
		return i18n.T(ctx, "error.maintenance.invalid_window_generic")
	}
	return i18n.T(ctx, "error.action_failed")
}

// nil EndsAt значит «бессрочно» для validateWindow — этот смысл обязан быть явным
// выбором (чекбокс indefinite), а не тем, что дату конца забыли ввести.
func oneOffEndRequired(win uptime.Window, indefinite bool) bool {
	return !win.Weekly && !indefinite && win.EndsAt == nil
}

func windowBelongsToProject(windows []uptime.Window, windowID int64) bool {
	for _, w := range windows {
		if w.ID == windowID {
			return true
		}
	}
	return false
}

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
	// h.Uptime == nil в стендах без подсистемы мониторинга — 404, а не паника.
	if h.Uptime == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	h.renderMaintenance(w, r, http.StatusOK, projectID, nil, "")
}

func maintenanceFormState(r *http.Request) templates.FormState {
	f := templates.FormState{}
	for _, name := range []string{
		"name", "kind", "starts_at", "ends_at", "indefinite",
		"weekday", "start_time", "end_time", "timezone_custom",
	} {
		if v := r.FormValue(name); v != "" {
			f[name] = v
		}
	}
	// пустое значение select'а — это выбор «Другой»; пропустив его, форма после
	// ошибки вернулась бы на UTC, спрятав введённый вручную пояс.
	f["timezone"] = r.FormValue("timezone")
	return f
}

func (h *Handler) renderMaintenance(w http.ResponseWriter, r *http.Request, status int, projectID int64, form templates.FormState, errMsg string) {
	windows, err := h.Uptime.Windows(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	w.WriteHeader(status)
	_ = templates.Maintenance(projectID, windows, form, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

func (h *Handler) maintenanceCreate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
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
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}

	win, indefinite := parseMaintenanceForm(r, projectID)
	if oneOffEndRequired(win, indefinite) {
		h.renderMaintenance(w, r, http.StatusUnprocessableEntity, projectID,
			maintenanceFormState(r).Open("new-maintenance-window"),
			i18n.T(r.Context(), "error.maintenance.end_required"))
		return
	}
	if _, err := h.Uptime.CreateWindow(r.Context(), win); err != nil {
		if errors.Is(err, uptime.ErrInvalidWindow) {
			h.renderMaintenance(w, r, http.StatusUnprocessableEntity, projectID,
				maintenanceFormState(r).Open("new-maintenance-window"),
				maintenanceErrorMessage(r.Context(), err))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, maintenancePath(projectID), http.StatusSeeOther)
}

func (h *Handler) maintenanceUpdate(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
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
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	windowID, err := strconv.ParseInt(r.FormValue("window_id"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
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

	win, indefinite := parseMaintenanceForm(r, projectID)
	win.ID = windowID
	if oneOffEndRequired(win, indefinite) {
		h.renderMaintenance(w, r, http.StatusUnprocessableEntity, projectID,
			maintenanceFormState(r).Open(templates.EditWindowModalID(windowID)),
			i18n.T(r.Context(), "error.maintenance.end_required"))
		return
	}
	if err := h.Uptime.UpdateWindow(r.Context(), win); err != nil {
		if errors.Is(err, uptime.ErrInvalidWindow) {
			h.renderMaintenance(w, r, http.StatusUnprocessableEntity, projectID,
				maintenanceFormState(r).Open(templates.EditWindowModalID(windowID)),
				maintenanceErrorMessage(r.Context(), err))
			return
		}
		if errors.Is(err, uptime.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.flashOK(w, "flash.saved", 0)
	http.Redirect(w, r, maintenancePath(projectID), http.StatusSeeOther)
}

func (h *Handler) maintenanceDelete(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r, h.BaseURL) {
		h.denyCrossOrigin(w, r)
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
	if _, ok := h.requireProjectOperator(w, r, projectID, uid); !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	windowID, err := strconv.ParseInt(r.FormValue("window_id"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
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
	// CSP без unsafe-inline не исполняет inline confirm() — подтверждение отдельной страницей.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.maintenance_delete.message", "confirm.delete",
			maintenancePath(projectID), maintenanceDeletePath(projectID),
			[]templates.HiddenField{{Name: "window_id", Value: strconv.FormatInt(windowID, 10)}})
		return
	}
	if err := h.Uptime.DeleteWindow(r.Context(), windowID, projectID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	http.Redirect(w, r, maintenancePath(projectID), http.StatusSeeOther)
}
