package web

import (
	"errors"
	"net/http"
	"strconv"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

func projectSettingsPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/settings"
}

func projectSettingsRenamePath(projectID int64) string {
	return projectSettingsPath(projectID) + "/rename"
}

func projectSettingsKeysPath(projectID int64) string {
	return projectSettingsPath(projectID) + "/keys"
}

func projectSettingsKeysRevokePath(projectID int64) string {
	return projectSettingsKeysPath(projectID) + "/revoke"
}

// parsePathProjectID достаёт projectID из {id} пути /projects/{id}/settings*;
// на невалидный id — 404 (тот же принцип, что и у parsePathOrgID).
func parsePathProjectID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	projectID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return projectID, true
}

// requireProjectRole резолвит projectID -> orgID (org.ProjectOrg) и проверяет
// роль вызывающего в этой организации (requireOrgRole): несуществующий
// проект и недостаточная роль дают одну и ту же стилизованную 404.
func (h *Handler) requireProjectRole(w http.ResponseWriter, r *http.Request, projectID, userID int64) (int64, bool) {
	orgID, err := h.Org.ProjectOrg(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, "Страница не найдена")
			return 0, false
		}
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return 0, false
	}
	if _, ok := h.requireOrgRole(w, r, orgID, userID); !ok {
		return 0, false
	}
	return orgID, true
}

func projectSettingsErrorMessage(err error) string {
	switch {
	case errors.Is(err, org.ErrInvalidName):
		return "имя проекта не должно быть пустым"
	default:
		return "не удалось выполнить действие"
	}
}

// keyBelongsToProject проверяет принадлежность ключа проекту по уже
// загруженному списку KeysForProject — тот же приём, что и findProject: не
// даём отозвать чужой ключ по id (см. projectSettingsKeyRevoke).
func keyBelongsToProject(keys []org.Key, keyID int64) bool {
	for _, k := range keys {
		if k.ID == keyID {
			return true
		}
	}
	return false
}

// projectSettingsPage — GET /projects/{id}/settings: имя, платформа
// (readonly), таблица ключей, DSN текущего живого ключа. Доступ только
// owner/admin организации проекта (requireProjectRole).
func (h *Handler) projectSettingsPage(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projectID, ok := parsePathProjectID(w, r)
	if !ok {
		return
	}
	orgID, ok := h.requireProjectRole(w, r, projectID, uid)
	if !ok {
		return
	}
	h.renderProjectSettings(w, r, http.StatusOK, orgID, projectID, "")
}

// renderProjectSettings — общий рендер: GET-обработчик и все POST в этом
// файле на 422 (то же сообщение на месте, без редиректа — тот же принцип,
// что и renderOrgSettings/renderTeamsPage). orgID уже известен вызывающему
// (requireProjectRole его вернул) — не запрашиваем его заново.
func (h *Handler) renderProjectSettings(w http.ResponseWriter, r *http.Request, status int, orgID, projectID int64, errMsg string) {
	// Отдельного Get-по-id для проекта в org.Service нет — как и в
	// projectSetup, находим проект в списке всех проектов организации
	// (findProject определён в onboarding.go, тот же пакет).
	projects, err := h.Org.ProjectsOf(r.Context(), orgID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	project, ok := findProject(projects, projectID)
	if !ok {
		h.renderError(w, r, http.StatusNotFound, "Страница не найдена")
		return
	}
	keys, err := h.Org.KeysForProject(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	var dsn string
	if publicKey := firstLiveKey(keys); publicKey != "" {
		dsn = buildDSN(h.BaseURL, publicKey, projectID)
	}
	w.WriteHeader(status)
	_ = templates.ProjectSettings(project, keys, dsn, errMsg, h.currentEmail(r)).Render(r.Context(), w)
}

// projectSettingsRename — POST /projects/{id}/settings/rename: name.
// ErrInvalidName (пустое имя) → 422.
func (h *Handler) projectSettingsRename(w http.ResponseWriter, r *http.Request) {
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
	orgID, ok := h.requireProjectRole(w, r, projectID, uid)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")
	if err := h.Org.RenameProject(r.Context(), projectID, name); err != nil {
		h.renderProjectSettings(w, r, http.StatusUnprocessableEntity, orgID, projectID, projectSettingsErrorMessage(err))
		return
	}
	http.Redirect(w, r, projectSettingsPath(projectID), http.StatusSeeOther)
}

// projectSettingsKeyCreate — POST /projects/{id}/settings/keys: выпускает
// новый DSN-ключ проекта.
func (h *Handler) projectSettingsKeyCreate(w http.ResponseWriter, r *http.Request) {
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
	if _, err := h.Org.CreateKey(r.Context(), projectID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	http.Redirect(w, r, projectSettingsPath(projectID), http.StatusSeeOther)
}

// projectSettingsKeyRevoke — POST /projects/{id}/settings/keys/revoke:
// key_id. Ключ должен принадлежать проекту из пути (проверка через
// KeysForProject), иначе 404 — иначе можно было бы по id отозвать чужой ключ.
func (h *Handler) projectSettingsKeyRevoke(w http.ResponseWriter, r *http.Request) {
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
	keyID, err := strconv.ParseInt(r.FormValue("key_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad key_id", http.StatusBadRequest)
		return
	}
	keys, err := h.Org.KeysForProject(r.Context(), projectID)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if !keyBelongsToProject(keys, keyID) {
		h.renderError(w, r, http.StatusNotFound, "Страница не найдена")
		return
	}
	if err := h.Org.RevokeKey(r.Context(), keyID); err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	http.Redirect(w, r, projectSettingsPath(projectID), http.StatusSeeOther)
}
