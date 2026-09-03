package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestWebEscalationsPage — owner (оператор проекта) видит редактор двух
// лесенок, member без командного доступа к проекту — 404 (тот же
// existence-oracle, что и alerts/slos: requireProjectOperator).
func TestWebEscalationsPage(t *testing.T) {
	s := newStack(t)
	s.h.EscalationPolicy = escalation.NewPolicyStore(s.pool)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "esc-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "esc-member@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "esc-co", "Esc Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "esc-proj", "Esc Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	// Канал проекта — без него ни один чекбокс step0_channels не рендерится
	// (только Deliverable-каналы, см. escalations.templ), а форма всё равно
	// обязана показать поля ступеней.
	if _, err := s.h.Alerts.CreateChannel(context.Background(), alert.Channel{
		ProjectID: proj.ID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/escalations"

	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	// Обе severity и фиксированные поля ступеней должны быть в форме.
	for _, want := range []string{"step0_delay", "step0_channels", "severity", "critical", "warning"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s missing %q: %s", path, want, body)
		}
	}

	resp = getWithCookie(t, s.srv, path, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (member, no team) status = %d, want 404", path, resp.StatusCode)
	}
}

// TestWebEscalationsSave — сохранение валидной лесенки (две ступени) →
// PolicyStore.Ladder возвращает её; лесенка с дырой в step_no → 422, ничего
// не сохраняется; dry-run-предпросмотр на странице после сохранения содержит
// цель канала (концерн: "содержит названия каналов ступеней").
func TestWebEscalationsSave(t *testing.T) {
	s := newStack(t)
	s.h.EscalationPolicy = escalation.NewPolicyStore(s.pool)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "esc-save-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "esc-save-co", "Esc Save Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "esc-save-proj", "Esc Save Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/escalations"

	c1, err := s.h.Alerts.CreateChannel(context.Background(), alert.Channel{
		ProjectID: proj.ID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hooks/a4d718d555cb0001",
	})
	if err != nil {
		t.Fatalf("CreateChannel c1: %v", err)
	}
	c2, err := s.h.Alerts.CreateChannel(context.Background(), alert.Channel{
		ProjectID: proj.ID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hooks/a4d718d555cb0002",
	})
	if err != nil {
		t.Fatalf("CreateChannel c2: %v", err)
	}

	// Валидная лесенка: ступень 0 сразу к c1, ступень 1 через 15 мин к c2.
	valid := url.Values{
		"severity":       {"critical"},
		"step0_delay":    {"0"},
		"step0_channels": {strconv.FormatInt(c1, 10)},
		"step1_delay":    {"15"},
		"step1_channels": {strconv.FormatInt(c2, 10)},
	}
	resp := postForm(t, s.srv, path, valid, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save valid ladder status = %d, want 303", resp.StatusCode)
	}

	ladder, err := s.h.EscalationPolicy.Ladder(context.Background(), proj.ID, escalation.SeverityCritical)
	if err != nil {
		t.Fatalf("Ladder: %v", err)
	}
	if len(ladder) != 2 || ladder[0].DelayMinutes != 0 || ladder[1].DelayMinutes != 15 ||
		len(ladder[0].ChannelIDs) != 1 || ladder[0].ChannelIDs[0] != c1 ||
		len(ladder[1].ChannelIDs) != 1 || ladder[1].ChannelIDs[0] != c2 {
		t.Fatalf("Ladder(critical) = %+v, want step0->c1, step1(15m)->c2", ladder)
	}

	// Dry-run на странице отражает то, что реально сохранено, но адрес
	// вебхука показан безопасным представлением (хост + путь с урезанным до
	// хвоста секретным сегментом), а не полным URL. Два разных вебхука при
	// этом обязаны оставаться различимы по хвосту.
	resp = getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	html := string(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s after save status = %d, want 200: %s", path, resp.StatusCode, html)
	}
	for _, want := range []string{"example.com/hooks/…0001", "example.com/hooks/…0002"} {
		if !strings.Contains(html, want) {
			t.Fatalf("GET %s after save missing masked channel target %q: %s", path, want, html)
		}
	}
	for _, leak := range []string{"a4d718d555cb0001", "a4d718d555cb0002"} {
		if strings.Contains(html, leak) {
			t.Fatalf("GET %s after save leaks full webhook path (%q found): %s", path, leak, html)
		}
	}

	// Дыра в step_no (ступень 0 занята, 1 пустая, 2 занята) → 422, старая
	// лесенка не тронута.
	gap := url.Values{
		"severity":       {"critical"},
		"step0_delay":    {"0"},
		"step0_channels": {strconv.FormatInt(c1, 10)},
		"step2_delay":    {"30"},
		"step2_channels": {strconv.FormatInt(c2, 10)},
	}
	resp = postForm(t, s.srv, path, gap, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("save ladder with gap status = %d, want 422", resp.StatusCode)
	}
	ladder2, err := s.h.EscalationPolicy.Ladder(context.Background(), proj.ID, escalation.SeverityCritical)
	if err != nil {
		t.Fatalf("Ladder after rejected save: %v", err)
	}
	if len(ladder2) != 2 {
		t.Fatalf("Ladder(critical) after rejected save = %+v, want unchanged 2-step ladder", ladder2)
	}
}

// TestWebEscalationsCrossTenant — concern T2: channel_id чужого проекта в
// форме отвергается ДО SetLadder, лесенка не сохраняется. Тот же сценарий,
// что и TestWebAlertsChannelUpdateForeign (edit_forms_test.go), для эскалаций.
func TestWebEscalationsCrossTenant(t *testing.T) {
	s := newStack(t)
	s.h.EscalationPolicy = escalation.NewPolicyStore(s.pool)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "esc-cross-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "esc-cross-co", "Esc Cross Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	mine, err := orgSvc.CreateProject(context.Background(), o.ID, "esc-cross-mine", "Mine", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	theirs, err := orgSvc.CreateProject(context.Background(), o.ID, "esc-cross-theirs", "Theirs", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	foreignChannel, err := s.h.Alerts.CreateChannel(context.Background(), alert.Channel{
		ProjectID: theirs.ID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/foreign-hook",
	})
	if err != nil {
		t.Fatalf("CreateChannel (theirs): %v", err)
	}

	path := "/projects/" + strconv.FormatInt(mine.ID, 10) + "/escalations"
	form := url.Values{
		"severity":       {"critical"},
		"step0_delay":    {"0"},
		"step0_channels": {strconv.FormatInt(foreignChannel, 10)},
	}
	resp := postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("save with foreign channel status = %d, want 422", resp.StatusCode)
	}
	ladder, err := s.h.EscalationPolicy.Ladder(context.Background(), mine.ID, escalation.SeverityCritical)
	if err != nil {
		t.Fatalf("Ladder: %v", err)
	}
	// Ничего не настроено -> дефолт-fallback (пустая лесенка проекта mine, у
	// которого нет собственных каналов), а не лесенка с чужим channel_id.
	if len(ladder) != 1 || len(ladder[0].ChannelIDs) != 0 {
		t.Fatalf("Ladder(mine) after rejected cross-tenant save = %+v, want default fallback with no channels", ladder)
	}
}

// TestWebEscalationsNilService — h.EscalationPolicy не проведён (узкий
// тестовый стенд) -> 404, тот же nil-guard, что у alertsPage/slosPage.
func TestWebEscalationsNilService(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "esc-nil-owner@example.com")
	o, _ := orgSvc.CreateOrg(context.Background(), "esc-nil-co", "Esc Nil Co", ownerID)
	proj, _ := orgSvc.CreateProject(context.Background(), o.ID, "esc-nil-proj", "Esc Nil Proj", "go")
	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/escalations"

	resp := getWithCookie(t, s.srv, path, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil EscalationPolicy status = %d, want 404", resp.StatusCode)
	}
}

// TestWebEscalationsUndeliverableChannelSurvivesResave — W2-C находка 5:
// канал, сохранённый в ступени, ломается ПОСЛЕ настройки (здесь — оператор
// его выключает; у сломанного секрета Deliverable() тот же false) — форма
// обязана продолжать его показывать, отмеченным и с пометкой причины, а
// повторное «Сохранить» (форма отправляется буквально как отрендерена,
// ничего руками не трогаем) не должно тихо потерять его из лесенки.
func TestWebEscalationsUndeliverableChannelSurvivesResave(t *testing.T) {
	s := newStack(t)
	s.h.EscalationPolicy = escalation.NewPolicyStore(s.pool)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "esc-undeliv-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "esc-undeliv-co", "Esc Undeliv Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "esc-undeliv-proj", "Esc Undeliv Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/escalations"

	chID, err := s.h.Alerts.CreateChannel(context.Background(), alert.Channel{
		ProjectID: proj.ID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/flaky-hook",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	form := url.Values{
		"severity":       {"critical"},
		"step0_delay":    {"0"},
		"step0_channels": {strconv.FormatInt(chID, 10)},
	}
	resp := postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save initial ladder status = %d, want 303", resp.StatusCode)
	}

	// Канал ломается ПОСЛЕ настройки ступени: оператор его выключил (тот же
	// эффект недоставляемости, что и у сломанного секрета — Deliverable()
	// смотрит на Enabled ИЛИ SecretBroken).
	if err := s.h.Alerts.UpdateChannel(context.Background(), alert.Channel{
		ID: chID, ProjectID: proj.ID, Kind: alert.ChannelWebhook, Target: "https://example.com/flaky-hook", Enabled: false,
	}); err != nil {
		t.Fatalf("UpdateChannel (disable): %v", err)
	}

	resp = getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET after disabling channel status = %d, want 200", resp.StatusCode)
	}
	html := string(body)
	chVal := `value="` + strconv.FormatInt(chID, 10) + `"`
	if !strings.Contains(html, chVal) {
		t.Fatalf("недоставляемый, но выбранный канал пропал из формы: %s", html)
	}
	// Чекбокс обязан остаться АКТИВНЫМ (не disabled) — иначе браузер не
	// пошлёт его value при отправке формы, и следующее "Сохранить" потеряет
	// канал так же тихо, как до фикса.
	checkboxStart := strings.Index(html, `<input type="checkbox" name="step0_channels" `+chVal)
	if checkboxStart == -1 {
		t.Fatalf("чекбокс канала не найден в ожидаемой форме: %s", html)
	}
	checkboxEnd := strings.Index(html[checkboxStart:], ">")
	if checkboxEnd == -1 {
		t.Fatalf("не удалось выделить тег чекбокса: %s", html)
	}
	checkboxTag := html[checkboxStart : checkboxStart+checkboxEnd]
	if strings.Contains(checkboxTag, "disabled") {
		t.Fatalf("чекбокс недоставляемого канала disabled — его value не уйдёт при отправке формы: %s", checkboxTag)
	}
	if !strings.Contains(checkboxTag, "checked") {
		t.Fatalf("чекбокс недоставляемого, но сохранённого канала не отмечен: %s", checkboxTag)
	}

	// Повторное "Сохранить" — форма отправляется буквально как отрендерена
	// (тот же набор полей, что и в исходном form выше): канал должен
	// пережить пересохранение, а не исчезнуть из лесенки.
	resp = postForm(t, s.srv, path, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("re-save ladder status = %d, want 303", resp.StatusCode)
	}
	ladder, err := s.h.EscalationPolicy.Ladder(context.Background(), proj.ID, escalation.SeverityCritical)
	if err != nil {
		t.Fatalf("Ladder: %v", err)
	}
	if len(ladder) != 1 || len(ladder[0].ChannelIDs) != 1 || ladder[0].ChannelIDs[0] != chID {
		t.Fatalf("Ladder(critical) после пересохранения = %+v, канал %d потерян", ladder, chID)
	}
}

// TestWebEscalationsLayout — UI-проход: вводная теория — в свёрнутой справке
// под <h1> со ссылкой на существующую доку; ступени лесенки — сеткой
// (класс-крючок на обёртке каждой из двух лесенок); правило непрерывности
// ступеней — заметной подсказкой ДО формы, а не серым хвостом под ней.
func TestWebEscalationsLayout(t *testing.T) {
	s := newStack(t)
	s.h.EscalationPolicy = escalation.NewPolicyStore(s.pool)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "esc-layout-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "esc-layout-co", "Esc Layout Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "esc-layout-proj", "Esc Layout Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := s.h.Alerts.CreateChannel(context.Background(), alert.Channel{
		ProjectID: proj.ID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/escalations"

	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	html := string(body)
	for _, want := range []string{`class="help-panel"`, `href="/docs/escalations"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("GET %s missing %q: %s", path, want, html)
		}
	}
	// Вводная теория живёт внутри тела свёрнутой справки, а не стеной
	// абзацев под <h1>: текст интро стоит между help-panel-body и </details>.
	bodyIdx := strings.Index(html, `class="help-panel-body"`)
	introIdx := strings.Index(html, "какая из них сработает")
	if bodyIdx == -1 || introIdx == -1 {
		t.Fatalf("GET %s: help-panel-body (%d) or intro text (%d) not found: %s", path, bodyIdx, introIdx, html)
	}
	detailsEnd := strings.Index(html[bodyIdx:], "</details>")
	if detailsEnd == -1 || introIdx < bodyIdx || introIdx > bodyIdx+detailsEnd {
		t.Fatalf("GET %s: intro text at %d is outside help-panel body [%d..%d]: %s", path, introIdx, bodyIdx, bodyIdx+detailsEnd, html)
	}
	// Ступени — сеткой: обёртка с классом-крючком в каждой из двух лесенок.
	if got := strings.Count(html, `class="escalation-steps"`); got != 2 {
		t.Fatalf("GET %s: %d escalation-steps wrappers, want 2 (critical + warning): %s", path, got, html)
	}
	// Правило непрерывности — заметной подсказкой (notice) до полей формы.
	noticeIdx := strings.Index(html, `<p class="notice">Чтобы убрать ступень`)
	formIdx := strings.Index(html, `class="escalation-ladder-form"`)
	if noticeIdx == -1 || formIdx == -1 {
		t.Fatalf("GET %s: continuity notice (%d) or ladder form (%d) not found: %s", path, noticeIdx, formIdx, html)
	}
	if noticeIdx > formIdx {
		t.Fatalf("GET %s: continuity notice at %d comes after the form at %d — must be readable before filling: %s", path, noticeIdx, formIdx, html)
	}
	if got := strings.Count(html, `<p class="notice">Чтобы убрать ступень`); got != 2 {
		t.Fatalf("GET %s: %d continuity notices, want 2 (one per ladder): %s", path, got, html)
	}
	// Подпись задержки — короткая у каждой ступени (иначе полная фраза
	// повторяется десять раз на экран), развёрнутое пояснение «минут от
	// открытия инцидента» — один раз на лесенку, в той же плашке-notice.
	if got := strings.Count(html, "Задержка, мин"); got != 10 {
		t.Fatalf("GET %s: %d short delay labels, want 10 (2 ladders × 5 steps): %s", path, got, html)
	}
	if got := strings.Count(html, "минутах от открытия инцидента"); got != 2 {
		t.Fatalf("GET %s: %d delay explanations, want 2 (one per ladder): %s", path, got, html)
	}
	// Зазор между карточками лесенок: класс-крючок .escalations-section на
	// обеих секциях (правило в app.css).
	if got := strings.Count(html, `class="escalations-section card"`); got != 2 {
		t.Fatalf("GET %s: %d escalations-section hooks, want 2: %s", path, got, html)
	}

	// Дока, на которую ведёт «подробнее», обязана существовать — иначе
	// свёрнутая справка кончается 404.
	resp = getWithCookie(t, s.srv, "/docs/escalations", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /docs/escalations status = %d, want 200", resp.StatusCode)
	}
}

// TestWebEscalationsChannelTargetMasked — адрес канала в подписях чекбоксов
// и dry-run: секретный сегмент пути вебхука в HTML отсутствует, но словарные
// сегменты пути видны целиком (посегментная маска maskedWebhookTarget) и два
// вебхука одного сервиса различимы по хвосту; email и telegram chat id — не
// секреты и показываются как есть. Страницу открывает owner (canManage), то
// есть channelsForView отдаёт СЫРЫЕ цели — маску обязан держать сам шаблон.
func TestWebEscalationsChannelTargetMasked(t *testing.T) {
	s := newStack(t)
	s.h.EscalationPolicy = escalation.NewPolicyStore(s.pool)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "esc-mask-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "esc-mask-co", "Esc Mask Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "esc-mask-proj", "Esc Mask Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	// Два вебхука одного сервиса: секрет — в пути, отличимы только хвостом.
	for _, target := range []string{
		"https://hooks.example.com/services/T000/B000/secretaaa111",
		"https://hooks.example.com/services/T000/B000/secretbbb222",
	} {
		if _, err := s.h.Alerts.CreateChannel(context.Background(), alert.Channel{
			ProjectID: proj.ID, Kind: alert.ChannelWebhook, Enabled: true, Target: target,
		}); err != nil {
			t.Fatalf("CreateChannel %s: %v", target, err)
		}
	}
	if _, err := s.h.Alerts.CreateChannel(context.Background(), alert.Channel{
		ProjectID: proj.ID, Kind: alert.ChannelEmail, Enabled: true, Target: "oncall@example.com",
	}); err != nil {
		t.Fatalf("CreateChannel email: %v", err)
	}
	if _, err := s.h.Alerts.CreateChannel(context.Background(), alert.Channel{
		ProjectID: proj.ID, Kind: alert.ChannelTelegram, Enabled: true, Target: "-100200300", Secret: "tok",
	}); err != nil {
		t.Fatalf("CreateChannel telegram: %v", err)
	}
	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/escalations"

	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	html := string(body)
	// Секретный сегмент вебхука не встречается в HTML нигде — ни в подписи
	// чекбокса ступени, ни в dry-run. Словарные сегменты (/services/T000/
	// B000) секретами не являются и по замыслу видны — в списке утечек
	// только настоящие секреты.
	for _, leak := range []string{"secretaaa111", "secretbbb222"} {
		if strings.Contains(html, leak) {
			t.Fatalf("GET %s leaks webhook secret path (%q found): %s", path, leak, html)
		}
	}
	// Хост, словарный путь и различающий хвост видны (вебхуки отличимы),
	// email и chat id — как есть.
	for _, want := range []string{
		"hooks.example.com/services/T000/B000/…a111",
		"hooks.example.com/services/T000/B000/…b222",
		"oncall@example.com", "-100200300",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("GET %s missing channel label %q: %s", path, want, html)
		}
	}
}
