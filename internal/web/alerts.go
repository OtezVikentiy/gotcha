package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

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

func alertsErrorMessage(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, alert.ErrInvalidRule):
		return i18n.T(ctx, "error.alerts.invalid_rule")
	case errors.Is(err, alert.ErrInvalidChannel):
		return i18n.T(ctx, "error.alerts.invalid_channel")
	default:
		return i18n.T(ctx, "error.action_failed")
	}
}

func formBool(r *http.Request, name string) bool {
	return r.FormValue(name) != ""
}

func formBoolValue(r *http.Request, name string) string {
	if formBool(r, name) {
		return "on"
	}
	return ""
}

func formInt(r *http.Request, name string) int {
	n, err := strconv.Atoi(r.FormValue(name))
	if err != nil {
		return 0
	}
	return n
}

func (h *Handler) alertsPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	if h.Alerts == nil {
		h.notFound(w, r)
		return
	}
	authz, ok := h.requireProjectOperator(w, r, projectID, uid)
	if !ok {
		return
	}
	h.renderAlerts(w, r, http.StatusOK, projectID, authz.CanManage, nil, "")
}

func (h *Handler) renderAlerts(w http.ResponseWriter, r *http.Request, status int, projectID int64, canManage bool, form templates.FormState, errMsg string) {
	rules, err := h.Alerts.Rules(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	channels, err := h.channelsForView(r.Context(), projectID, canManage)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	w.WriteHeader(status)
	_ = templates.Alerts(projectID, rules, channels, h.EmailEnabled, canManage, h.SecretKeyInsecure, form, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

func (h *Handler) alertDeliveriesPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := h.parsePathProjectID(w, r)
	if !ok {
		return
	}
	authz, ok := h.requireProjectOperator(w, r, projectID, uid)
	if !ok {
		return
	}
	var failed []notify.FailedJob
	var err error
	if h.Outbox != nil {
		failed, err = h.Outbox.FailedForProject(r.Context(), projectID, maxFailedDeliveries)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
	}
	if !authz.CanManage {
		for i := range failed {
			// Порядок важен: редактируем токен по сырому Target, следующая строка перезатирает его маской.
			failed[i].LastError = notify.RedactToken(failed[i].LastError, failed[i].Target)
			failed[i].Target = maskChannelTarget(failed[i].ChannelKind, failed[i].Target)
		}
	}
	_ = templates.AlertDeliveries(projectID, failed, authz.CanManage, h.currentEmail(r)).Render(r.Context(), w)
}

func (h *Handler) alertsRulesSave(w http.ResponseWriter, r *http.Request) {
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
	if h.Alerts == nil {
		h.notFound(w, r)
		return
	}
	authz, ok := h.requireProjectOperator(w, r, projectID, uid)
	if !ok {
		return
	}
	if !h.parseForm(w, r) {
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
	if err := h.Alerts.UpsertRules(r.Context(), rules); err != nil {
		h.renderAlerts(w, r, http.StatusUnprocessableEntity, projectID, authz.CanManage, nil, alertsErrorMessage(r.Context(), err))
		return
	}
	h.flashOK(w, "flash.rules_saved", 0)
	http.Redirect(w, r, alertsPath(projectID), http.StatusSeeOther)
}

func (h *Handler) alertsChannelCreate(w http.ResponseWriter, r *http.Request) {
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
	if h.Alerts == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	c := alert.Channel{
		ProjectID: projectID,
		Kind:      r.FormValue("kind"),
		Enabled:   formBool(r, "enabled"),
		Target:    r.FormValue("target"),
		Secret:    r.FormValue("secret"),
		Trusted:   formBool(r, "trusted"),
	}
	if _, err := h.Alerts.CreateChannel(r.Context(), c); err != nil {
		canManage, cmErr := h.canManageProject(r.Context(), projectID, uid)
		if cmErr != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		h.renderAlerts(w, r, http.StatusUnprocessableEntity, projectID, canManage,
			templates.FormState{"kind": c.Kind, "target": c.Target,
				"trusted": formBoolValue(r, "trusted")}.Open("new-channel"),
			alertsErrorMessage(r.Context(), err))
		return
	}
	h.flashOK(w, "flash.channel_created", 0)
	http.Redirect(w, r, alertsPath(projectID), http.StatusSeeOther)
}

func (h *Handler) alertsChannelUpdate(w http.ResponseWriter, r *http.Request) {
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
	if h.Alerts == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	channelID, err := strconv.ParseInt(r.FormValue("channel_id"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}
	channels, err := h.Alerts.Channels(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	kind, ok := channelKind(channels, channelID)
	if !ok {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return
	}
	c := alert.Channel{
		ID:        channelID,
		ProjectID: projectID,
		Kind:      kind,
		Enabled:   formBool(r, "enabled"),
		Target:    r.FormValue("target"),
		Secret:    r.FormValue("secret"),
		Trusted:   formBool(r, "trusted"),
	}
	if err := h.Alerts.UpdateChannel(r.Context(), c); err != nil {
		if errors.Is(err, alert.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return
		}
		canManage, cmErr := h.canManageProject(r.Context(), projectID, uid)
		if cmErr != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		h.renderAlerts(w, r, http.StatusUnprocessableEntity, projectID, canManage,
			templates.FormState{"target": c.Target, "enabled": formBoolValue(r, "enabled"),
				"trusted": formBoolValue(r, "trusted")}.
				Open(templates.EditChannelModalID(channelID)),
			alertsErrorMessage(r.Context(), err))
		return
	}
	h.flashOK(w, "flash.channel_updated", 0)
	http.Redirect(w, r, alertsPath(projectID), http.StatusSeeOther)
}

func channelKind(channels []alert.Channel, channelID int64) (string, bool) {
	for _, c := range channels {
		if c.ID == channelID {
			return c.Kind, true
		}
	}
	return "", false
}

func findChannel(channels []alert.Channel, channelID int64) (alert.Channel, bool) {
	for _, c := range channels {
		if c.ID == channelID {
			return c, true
		}
	}
	return alert.Channel{}, false
}

func (h *Handler) alertsChannelDelete(w http.ResponseWriter, r *http.Request) {
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
	if h.Alerts == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	channelID, err := strconv.ParseInt(r.FormValue("channel_id"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}
	channels, err := h.Alerts.Channels(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	c, ok := findChannel(channels, channelID)
	if !ok {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return
	}
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirmf(w, r, "confirm.title", "confirm.channel_delete.message", "confirm.delete",
			alertsPath(projectID), alertsChannelsDeletePath(projectID),
			[]templates.HiddenField{{Name: "channel_id", Value: strconv.FormatInt(channelID, 10)}},
			"kind", i18n.T(r.Context(), "alerts.channels.kind."+c.Kind), "target", c.Target)
		return
	}
	if err := h.Alerts.DeleteChannel(r.Context(), projectID, channelID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.flashOK(w, "flash.deleted", 0)
	http.Redirect(w, r, alertsPath(projectID), http.StatusSeeOther)
}

func (h *Handler) alertsChannelTest(w http.ResponseWriter, r *http.Request) {
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
	if h.Alerts == nil || h.NotifyDirect == nil {
		h.notFound(w, r)
		return
	}
	if _, ok := h.requireProjectRole(w, r, projectID, uid); !ok {
		return
	}
	if !h.parseForm(w, r) {
		return
	}
	channelID, err := strconv.ParseInt(r.FormValue("channel_id"), 10, 64)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return
	}
	channels, err := h.Alerts.Channels(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	var ch alert.Channel
	found := false
	for _, c := range channels {
		if c.ID == channelID {
			ch, found = c, true
			break
		}
	}
	if !found {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return
	}

	lctx := i18n.WithLocale(context.Background(), h.NotifyLocale)
	url := h.BaseURL + alertsPath(projectID)
	subject := i18n.T(lctx, "notify.test.subject")
	body := i18n.Tf(lctx, "notify.test.body", "name", ch.Target, "url", url)
	payload := map[string]any{
		"kind":         "channel_test",
		"project_id":   projectID,
		"url":          url,
		"subject":      subject,
		"body":         body,
		"channel_kind": ch.Kind,
		"target":       ch.Target,
	}
	if err := h.NotifyDirect.Send(r.Context(), ch.ID, ch.Kind, ch.Target, payload); err != nil {
		reason := strings.SplitN(err.Error(), "\n", 2)[0]
		if len(reason) > 200 {
			reason = reason[:200] + "…"
		}
		canManage, cmErr := h.canManageProject(r.Context(), projectID, uid)
		if cmErr != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		h.renderAlerts(w, r, http.StatusUnprocessableEntity, projectID, canManage, nil,
			i18n.Tf(r.Context(), "err.channel_test_failed", "reason", reason))
		return
	}
	h.flashOK(w, "flash.channel_test_sent", 0)
	http.Redirect(w, r, alertsPath(projectID), http.StatusSeeOther)
}
