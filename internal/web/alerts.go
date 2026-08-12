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

// formBool — состояние HTML-чекбокса: присутствует в форме (обычно "on") —
// true, отсутствует — false. Невыбранный чекбокс браузер вообще не
// отправляет, поэтому пустая строка неотличима от отсутствия поля — этого
// достаточно.
func formBool(r *http.Request, name string) bool {
	return r.FormValue(name) != ""
}

// formBoolValue — то же состояние чекбокса, но строкой для FormState: "on" или
// пустая строка. Нужно потому, что FormState хранит значения полей как есть, а
// снятый пользователем флажок надо вернуть в форму снятым — иначе правка,
// упавшая на валидации адреса, молча включала бы канал обратно.
func formBoolValue(r *http.Request, name string) string {
	if formBool(r, name) {
		return "on"
	}
	return ""
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
// таблица каналов доставки с формой добавления. Доступ — оператор проекта
// (requireProjectOperator, спека 2026-08-08): работа с алертами —
// операционная задача, не настройка организации; канальный CRUD внутри
// страницы остаётся admin-only и скрыт/санирован для не-admin в renderAlerts.
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
	// h.Alerts может быть nil в стендах без подсистемы алертинга — тогда 404,
	// а не паника при разыменовании (тот же guard, что и h.Metrics в
	// metricsList). renderAlerts дереференсит h.Alerts.Rules/Channels.
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

// renderAlerts — общий рендер: GET-обработчик и все POST в этом файле на 422
// (то же сообщение на месте, без редиректа — тот же принцип, что и у
// renderProjectSettings/renderOrgSettings). Каналы тянутся через
// channelsForView (находка B1) — единственную дверь, которая для не-admin
// маскирует Target и зануляет Secret ДО передачи в шаблон, секрет не должен
// попасть в HTML оператора даже неотрендеренным. canManage приходит от
// вызывающего (гейт его уже посчитал — находка B4), а не резолвится здесь
// заново отдельным canManageProject.
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
	_ = templates.Alerts(projectID, rules, channels, h.EmailEnabled, canManage, form, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

// alertDeliveriesPage — GET /projects/{id}/alerts/deliveries: лог последних
// неудачных доставок уведомлений (spec §7), вынесен из основной страницы
// алертов на отдельную страницу (UI-фидбек: секция делала страницу алертов
// слишком длинной). Доступ — тот же guard, что и у alertsPage (оператор);
// цель доставки для не-admin маскируется тем же приёмом, что и в
// renderAlerts.
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
	// Outbox может быть не проставлен (например, в узких тестовых стендах,
	// не относящихся к алертам) — тогда просто не показываем ни одной
	// failed-записи, не роняя страницу.
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
			// A1: эшелон защиты сверх санации у источника (email.go/webhook.go) —
			// last_error может нести секрет прежним отправителем/старой записью
			// (миграция чистит только исторические строки на момент апгрейда), а
			// сюда попадают и записи, поставленные до фикса источника. Редакция
			// идёт по СЫРОМУ target — до его маскировки строкой ниже, иначе
			// адрес/URL внутри last_error не с чем будет сопоставить.
			failed[i].LastError = notify.RedactToken(failed[i].LastError, failed[i].Target)
			failed[i].Target = maskChannelTarget(failed[i].ChannelKind, failed[i].Target)
		}
	}
	_ = templates.AlertDeliveries(projectID, failed, authz.CanManage, h.currentEmail(r)).Render(r.Context(), w)
}

// alertsRulesSave — POST /projects/{id}/alerts/rules: одна форма сохраняет
// все три kind разом (new_issue, regression, spike), атомарно, одним вызовом
// h.Alerts.UpsertRules (см. комментарий рядом с вызовом ниже). Ошибка
// (ErrInvalidRule — в первую очередь ожидается от spike: Threshold/Window
// должны быть > 0) рендерит форму с 422, и НИ одно правило не сохраняется —
// раньше цикл писал их по очереди и обрывался на первой ошибке, оставляя уже
// применённые правила молча сохранёнными.
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
	// Атомарно: либо применяются все три правила, либо ни одно. Раньше цикл
	// писал их по очереди и обрывался на первой ошибке — уже записанные
	// оставались, а по перерисованной из БД форме понять, что сохранилось,
	// было нельзя.
	if err := h.Alerts.UpsertRules(r.Context(), rules); err != nil {
		h.renderAlerts(w, r, http.StatusUnprocessableEntity, projectID, authz.CanManage, nil, alertsErrorMessage(r.Context(), err))
		return
	}
	h.flashOK(w, "flash.rules_saved", 0)
	http.Redirect(w, r, alertsPath(projectID), http.StatusSeeOther)
}

// alertsChannelCreate — POST /projects/{id}/alerts/channels: kind, target,
// secret, enabled. ErrInvalidChannel (email — не-адрес; webhook — не
// http(s)-URL; telegram — пустые chat_id/bot token) → 422.
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
		Trusted:   formBool(r, "trusted"),
	}
	if _, err := h.Alerts.CreateChannel(r.Context(), c); err != nil {
		// requireProjectRole выше уже требует owner/admin — canManage тут
		// всегда true, но считаем явно (не хардкодим), тот же приём, что и в
		// остальных обработчиках канала: renderAlerts больше не резолвит его
		// сам (находка B4), значение просто переехало сюда с вызова.
		canManage, cmErr := h.canManageProject(r.Context(), projectID, uid)
		if cmErr != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		// Секрет намеренно НЕ возвращаем в форму: он и так вводится вслепую
		// (type=password), а класть bot-токен обратно в HTML на странице с
		// ошибкой — лишний повод ему оказаться в кеше или в скриншоте.
		h.renderAlerts(w, r, http.StatusUnprocessableEntity, projectID, canManage,
			templates.FormState{"kind": c.Kind, "target": c.Target,
				"trusted": formBoolValue(r, "trusted")}.Open("new-channel"),
			alertsErrorMessage(r.Context(), err))
		return
	}
	h.flashOK(w, "flash.channel_created", 0)
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

// alertsChannelUpdate — POST /projects/{id}/alerts/channels/update:
// channel_id, target, secret, enabled.
//
// До этого у канала был только жизненный цикл «создать/удалить»: выключенный
// канал нельзя было включить обратно, а опечатку в адресе — исправить, и
// единственным выходом было удалить канал и завести заново.
//
// Тип канала не меняется намеренно: у email, webhook и telegram разный смысл
// адреса и секрета, и «сменить тип» — это другой канал, а не правка этого.
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Тот же скоуп, что и у удаления: id канала приходит из формы, и без
	// проверки принадлежности владелец одного проекта правил бы чужой канал.
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
		// Отметка доверия берётся из формы, как и Enabled: она стоит в той же
		// форме рядом с адресом, поэтому смена получателя при сохранённой
		// галочке — видимое действие оператора, а не тихий перенос доверия.
		// Обратное — сбрасывать её при любой правке — так же тихо ЛОМАЛО бы
		// доставку деталей, и оператор узнавал бы об этом по обеднённому
		// уведомлению.
		Trusted: formBool(r, "trusted"),
	}
	if err := h.Alerts.UpdateChannel(r.Context(), c); err != nil {
		if errors.Is(err, alert.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return
		}
		// requireProjectRole выше уже требует owner/admin — canManage тут
		// всегда true, но считаем явно, тот же приём, что и в alertsChannelCreate.
		canManage, cmErr := h.canManageProject(r.Context(), projectID, uid)
		if cmErr != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		// Секрет в форму не возвращаем по той же причине, что и при создании.
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

// channelKind — тип канала, если он принадлежит проекту. Возвращает и признак
// принадлежности: тот же приём, что и channelBelongsToProject, но заодно даёт
// тип, который правка не меняет и потому берёт из базы, а не из формы.
func channelKind(channels []alert.Channel, channelID int64) (string, bool) {
	for _, c := range channels {
		if c.ID == channelID {
			return c.Kind, true
		}
	}
	return "", false
}

// alertsChannelDelete — POST /projects/{id}/alerts/channels/delete:
// channel_id. Канал должен принадлежать проекту из пути, иначе 404.
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if !channelBelongsToProject(channels, channelID) {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return
	}
	// Двухшаговое подтверждение (CSP default-src 'self' без unsafe-inline не
	// исполняет inline confirm() — см. renderConfirm): без confirmed=yes
	// показываем страницу подтверждения вместо необратимого действия.
	if r.FormValue("confirmed") != "yes" {
		h.renderConfirm(w, r, "confirm.title", "confirm.channel_delete.message", "confirm.delete",
			alertsPath(projectID), alertsChannelsDeletePath(projectID),
			[]templates.HiddenField{{Name: "channel_id", Value: strconv.FormatInt(channelID, 10)}})
		return
	}
	if err := h.Alerts.DeleteChannel(r.Context(), projectID, channelID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	h.flashOK(w, "flash.deleted", 0)
	http.Redirect(w, r, alertsPath(projectID), http.StatusSeeOther)
}

// alertsChannelTest — POST /projects/{id}/alerts/channels/test: channel_id.
// Тестовая отправка в канал (№69): без неё проверить Telegram/webhook/email
// можно было, только дождавшись реального алерта. Доставка СИНХРОННАЯ, мимо
// outbox (см. notify.Direct): результат нужен немедленно — успех уходит во
// flash, ошибка доставки показывается на странице алертов классом ошибки.
// Доступ — та же роль, что правит каналы; rate-limit не нужен: действие
// ручное, за формой CSRF-гейт sameOrigin и owner/admin.
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
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	// Тот же скоуп, что у правки/удаления: id канала приходит из формы, и без
	// проверки принадлежности владелец одного проекта дёргал бы чужой канал.
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

	// Тексты — на языке инстанса: получатель канала тот же, что у настоящих
	// алертов (№133–136), а не тот, кто нажал кнопку.
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
		// Класс ошибки, не сырой дамп: первая строка, с разумным потолком.
		reason := strings.SplitN(err.Error(), "\n", 2)[0]
		if len(reason) > 200 {
			reason = reason[:200] + "…"
		}
		// requireProjectRole выше уже требует owner/admin — canManage тут
		// всегда true, но считаем явно, тот же приём, что и в alertsChannelCreate.
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
