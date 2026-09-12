package guards

import (
	"regexp"
	"strings"
	"testing"
)

// Видит только литеральный class="...": динамические {expr} в templ не различает, а
// конкатенация raw-строк в .go-эмиттерах может дать ложное совпадение (см. validClassTokenRe).
var classAttrRe = regexp.MustCompile(`class="([^"]*)"`)

// Первый символ — буква: цифра в начале не имя класса в этом дереве, а
// дефис/подчёркивание в начале среди используемых классов не встречаются.
var validClassTokenRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// Без этого шага упоминание класса в прозе комментария CSS засчиталось бы за определение.
var cssCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)

// Жадный `[A-Za-z0-9_-]*` берёт имя ЦЕЛИКОМ (".chip-large", не ".chip") — \b
// здесь не годится: дефис не словообразующий, граница легла бы посреди имени.
var cssClassNameRe = regexp.MustCompile(`\.([A-Za-z_-][A-Za-z0-9_-]*)`)

func cssDefinedClasses(css string) map[string]bool {
	css = cssCommentRe.ReplaceAllString(css, "")
	out := map[string]bool{}
	for _, m := range cssClassNameRe.FindAllStringSubmatch(css, -1) {
		out[m[1]] = true
	}
	return out
}

// //-комментарии вычищаются до разбора: без этого правило находило бы
// class="..." примеры в собственных doc-комментариях этого файла.
func scanClassAttrs(path, body string, report func(path string, line int, name string)) {
	for i, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		checked := stripTrailingComment(line)
		for _, m := range classAttrRe.FindAllStringSubmatch(checked, -1) {
			for _, name := range strings.Fields(m[1]) {
				if !validClassTokenRe.MatchString(name) {
					continue
				}
				report(path, i+1, name)
			}
		}
	}
}

// Классы без расчёта на стиль (маркер формы *-form или страницы/раздела) —
// визуал приходит от вложенных .card/.btn-*/etc. Пополняется только маркером целой семьи.
var permanentCSSClassExemptions = []Exemption{
	{Value: "alert-rules-form", Why: "семантический маркер формы, стиля нет по замыслу — оформление от .field/.select/.btn* внутри", Finding: "по замыслу"},
	{Value: "channel-create-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "channel-delete-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "channel-test-form", Why: "семантический маркер формы (тест-отправка в канал, №69), стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "channel-edit-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "confirm-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "invite-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "key-revoke-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "member-remove-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "metric-alert-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "monitor-form-form", Why: "семантический маркер формы (form внутри страницы monitor-form — совпадение имён страницы и формы, не опечатка), стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "org-delete-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "org-leave-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "probe-create-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "probe-revoke-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "project-create-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "project-delete-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "project-rename-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "sso-delete-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "status-page-delete-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "slo-form", Why: "семантический маркер формы (создание SLO, план D1), стиля нет по замыслу — оформление от .field/.select/.input/.btn* внутри", Finding: "по замыслу"},
	{Value: "escalation-ladder-form", Why: "семантический маркер формы (лесенка эскалации, B4 T9), стиля нет по замыслу — оформление от .field/.checkbox-field/.btn* внутри", Finding: "по замыслу"},
	{Value: "status-page-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "subject-export-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "subject-purge-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "gs-hide-form", Why: "семантический маркер формы (скрыть чек-лист, №71), стиля нет по замыслу — кнопка стилизована .btn, позиция — .card-header-action", Finding: "по замыслу"},
	{Value: "chromeless-logout", Why: "семантический маркер формы выхода в chromeless-шапке (№21), стиля нет по замыслу — кнопка стилизована .btn", Finding: "по замыслу"},
	{Value: "team-create-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "team-delete-form", Why: "семантический маркер формы (удаление команды, №26), стиля нет по замыслу — раскладку даёт обёртка .row-actions", Finding: "по замыслу"},
	{Value: "team-member-remove-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "team-project-detach-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "team-rename-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "window-delete-form", Why: "семантический маркер формы, стиля нет по замыслу", Finding: "по замыслу"},

	{Value: "about", Why: "маркер страницы на корневом <div>, стиля нет по замыслу — визуал от @layout и вложенных элементов", Finding: "по замыслу"},
	{Value: "alert-deliveries", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "alerts", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "chromeless", Why: "маркер страницы на <body>, стиля нет по замыслу — визуал несут дочерние .chromeless-top/.chromeless-main", Finding: "по замыслу"},
	{Value: "confirm-page", Why: "маркер страницы на корневом <div>, стиля нет по замыслу — внутри уже стилизованная .card", Finding: "по замыслу"},
	{Value: "deps", Why: "маркер страницы карты зависимостей (C4) на корневом <div>, стиля нет по замыслу — визуал от @layout и вложенных .card/.data-table", Finding: "по замыслу"},
	{Value: "deployments", Why: "маркер страницы на корневом <div> (список деплоев, C5), стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "docs-page", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "escalations", Why: "маркер страницы на корневом <div> (лесенки эскалации, B4 T9), стиля нет по замыслу — визуал от @layout и вложенных .card/.field/.checkbox-field", Finding: "по замыслу"},
	{Value: "hosts", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "host-settings", Why: "маркер под-страницы (настройки порогов хоста, T16) на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "incidents", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "issues", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "logs", Why: "маркер страницы на корневом <div> (задача 2, C2), стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "maintenance", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "metric-detail", Why: "маркер под-страницы (метрика) на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "metrics", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "org-probes", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "org-settings", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "performance", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "profile", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "profile-flame", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "profile-regressions", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "profiles", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "projects-list", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "regressions", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "slos", Why: "маркер страницы на корневом <div> (список SLO, план D1), стиля нет по замыслу — визуал от @layout и вложенных .card/.data-table", Finding: "по замыслу"},
	{Value: "teams", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "web-vitals", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},

	{Value: "alerts-section", Why: "семантический маркер секции внутри страницы alerts, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "gdpr-block", Why: "семантический маркер секции (152-ФЗ экспорт/удаление ПДн), стиля нет по замыслу — внутри уже стилизованные .field/.hint/.btn-ghost", Finding: "по замыслу"},
	{Value: "invite-accept", Why: "семантический маркер, стоит в паре с .card (class=\"invite-accept card\") — визуал от .card", Finding: "по замыслу"},
	{Value: "invite-link-block", Why: "семантический маркер, стоит в паре с .card (class=\"card invite-link-block\")", Finding: "по замыслу"},
	{Value: "metric-alerts", Why: "маркер под-раздела на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "monitor-kind-fields", Why: "семантический маркер на <fieldset> (HTTP/TCP/DNS/Heartbeat) — группировка полей формы по виду монитора, у <fieldset> уже есть браузерный вид, своего правила не требуется", Finding: "по замыслу"},
	{Value: "pending-invites", Why: "семантический маркер секции внутри org-settings, стиля нет по замыслу — внутри уже стилизованная .data-table", Finding: "по замыслу"},
	{Value: "perf-issue-detail", Why: "семантический маркер, стоит в паре с issue-detail, которая и несёт раскладку страницы (перенесено из старого ручного списка csscontract_test.go)", Finding: "по замыслу"},
	{Value: "perf-issues", Why: "маркер страницы на корневом <div>, стиля нет по замыслу", Finding: "по замыслу"},
	{Value: "probe-run", Why: "семантический маркер на <code>, визуал от базового селектора \"code, pre\" в app.css", Finding: "по замыслу"},
	{Value: "probe-token-block", Why: "семантический маркер, стоит в паре с .card (class=\"probe-token-block card\")", Finding: "по замыслу"},
	{Value: "profiles-actions", Why: "семантический маркер-обёртка вокруг уже стилизованной ссылки .btn.btn-ghost", Finding: "по замыслу"},
	{Value: "rate-guard-card", Why: "семантический маркер, стоит в паре с .card.card--wide (class=\"card card--wide rate-guard-card\")", Finding: "по замыслу"},
	{Value: "sso-redirect", Why: "семантический маркер-обёртка вокруг текста и <code> (визуал code — от базового селектора)", Finding: "по замыслу"},
	{Value: "sso-status", Why: "семантический маркер; в одном из трёх вхождений идёт вместе с уже стилизованным .hint (class=\"sso-status hint\")", Finding: "по замыслу"},
	{Value: "team", Why: "семантический маркер, стоит в паре с .card (class=\"team card\")", Finding: "по замыслу"},
}

// Потолок — фактическое число записей выше; менять только вместе со списком.
const maxPermanentCSSClassExemptions = 78

// Классы без семьи и без стилизованного соседа: похоже, автор разметки
// рассчитывал на оформление, но забыл завести его в app.css.
var debtCSSClassExemptions = []Exemption{
	{Value: "request-url", Why: "стоит рядом с уже стилизованным .request-method в той же строке разметки (issuedetail.templ:357), но своего стиля не имеет", Finding: "№TBD (подпроект G)"},
	{Value: "metric-meta", Why: "СОМНИТЕЛЬНО: похоже на пропущенный стиль подписи под заголовком (по роли похож на .chart-caption, который стилизован — уменьшенный тусклый шрифт под графиком), но ни семьи, ни стилизованного соседа нет; решение владельца — оставить как сомнительное для триажа, не решать здесь молча", Finding: "№TBD (подпроект G; сомнительный случай)"},
	{Value: "empty", Why: "<p class=\"empty\"> — пустое состояние flame-профиля (internal/web/svg.go:61); в app.css есть только семья .empty-state (другое имя), самого .empty нет нигде", Finding: "№TBD (подпроект G; найдено раундом правок 1 — расширение на .go-эмиттеры)"},
	{Value: "probe-token", Why: "переклассифицирован из permanentCSSClassExemptions по указанию владельца: <code class=\"probe-token\"> показывает 64-символьный сырой секрет один раз, базовый селектор \"code, pre\" не даёт переноса/переполнения — похоже, что стиль заводился, но не был дописан (probe-run рядом оставлен как маркер по замыслу — переоценка его не касалась)", Finding: "№TBD (подпроект G; найдено раундом правок 1 — расширение на .go-эмиттеры)"},
}

const maxDebtCSSClassExemptions = 4

// _test.go не исключён (в отличие от i18n_keys_test.go) — литеральный
// class="..." там совпадает с реальной разметкой, а не намеренно бьёт по дыре.
func TestNoUndefinedCSSClasses(t *testing.T) {
	tree := Load(t)
	defined := cssDefinedClasses(tree.CSS.Body)

	exempt := ExemptedValues(permanentCSSClassExemptions)
	for v := range ExemptedValues(debtCSSClassExemptions) {
		exempt[v] = true
	}

	// seen собирает только неопределённые классы — иначе почин записи не
	// отличить от неё же нетронутой, и храповик CheckExemptions не заметит устаревание.
	seen := map[string]bool{}
	reported := map[string]bool{}

	handle := func(path string, line int, name string) {
		if defined[name] {
			return
		}
		seen[name] = true
		if exempt[name] || reported[name] {
			return
		}
		reported[name] = true
		t.Errorf("%s:%d: класс %q используется в разметке, но не определён в app.css", path, line, name)
	}

	for _, f := range tree.Templates {
		scanClassAttrs(f.Path, f.Body, handle)
	}
	for _, f := range tree.GoFiles {
		// _templ.go дублирует уже просканированный .templ.
		if f.Generated {
			continue
		}
		scanClassAttrs(f.Path, f.Body, handle)
	}

	CheckExemptions(t, "TestNoUndefinedCSSClasses (по замыслу)", permanentCSSClassExemptions, maxPermanentCSSClassExemptions, seen)
	CheckExemptions(t, "TestNoUndefinedCSSClasses (долг подпроекта G)", debtCSSClassExemptions, maxDebtCSSClassExemptions, seen)
}
