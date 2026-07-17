// Package web — серверный (SSR) UI поверх auth/org/issue/event: роутер,
// страницы аутентификации, статика. Каждая страница работает без JS
// (обычные формы и ссылки); htmx используется только как прогрессивное
// улучшение.
package web

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/telemetry"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

//go:embed static
var staticFiles embed.FS

// Handler — весь SSR UI.
type Handler struct {
	Auth    *auth.Service
	Org     *org.Service
	Issues  *issue.Service
	Events  *event.Query
	BaseURL string
	Secure  bool // Secure = strings.HasPrefix(BaseURL, "https://")
	// SecretKey — ключ HMAC-подписи короткоживущей oauth-cookie (этап 5,
	// oauthstate.go). Проставляется из cfg.SecretKey в main.go; в стендах может
	// быть пустым — тогда используется дефолт (см. secret()).
	SecretKey string

	// RegistrationMode — режим самостоятельной регистрации (PROD-B1):
	// open|invite|closed. Проставляется из cfg.RegistrationMode в main.go.
	// Пустая строка трактуется как «не open» (регистрация закрыта, кроме
	// bootstrap первого пользователя). См. registerSubmit/registerPage.
	RegistrationMode string

	// RetentionDays — срок хранения телеметрии в днях (PROD-P6).
	// Проставляется из cfg.RetentionDays в main.go. Показывается подписью
	// «События хранятся N дней» на странице настроек проекта; 0 — срок не
	// задан, подпись не рендерится.
	RetentionDays int

	// Alerts — CRUD правил/каналов алертинга (план 6, задача 5):
	// /projects/{id}/alerts и EnsureDefaultRules при создании проекта. Не
	// принимается конструктором New (как Auth/Org/Issues/Events), а
	// проставляется отдельным полем вызывающей стороной (cmd/gotcha/main.go,
	// тестовые стенды) — оставляет сигнатуру New нетронутой для всего
	// существующего кода, который его вызывает.
	Alerts *alert.Service
	// Email — синхронная отправка писем-приглашений (см. orgSettingsInvite).
	// Может быть nil (SMTP не настроен) — тогда приглашение только
	// показывается ссылкой в UI, письмо не шлётся.
	Email *notify.EmailSender
	// EmailEnabled — настроен ли SMTP-транспорт (PROD-P2). Проставляется из
	// main.go = emailSender.Configured(). Когда false, UI отражает
	// недоступность почты: опция Email в форме канала алертов дизейблена с
	// пояснением, а форма приглашений показывает предупреждение, что письмо не
	// уйдёт и ссылку нужно скопировать вручную. Поле дублирует смысл
	// h.Email!=nil && h.Email.Configured(), но вынесено отдельно, чтобы UI не
	// зависел от того, проставлен ли Email в конкретном стенде.
	EmailEnabled bool
	// Outbox — очередь доставки алертов (план 6, задача 5, spec §7): страница
	// /projects/{id}/alerts показывает таблицу failed-доставок
	// (FailedForProject), чтобы отказы каналов были видны в UI, а не только в
	// логах воркера. Как и Alerts, проставляется отдельным полем, а не
	// принимается конструктором New.
	Outbox *notify.Outbox

	// Uptime — CRUD и состояние мониторов (этап 2, план 2, задача 3):
	// heartbeat-эндпойнт (/uptime/hb/{token}) ищет монитор по токену и
	// обновляет last_beat_at/monitor_state. Как и Alerts/Outbox,
	// проставляется отдельным полем — nil (мод без "web") означает, что
	// heartbeat-роут вовсе не регистрируется (см. Register).
	Uptime *uptime.Service
	// UptimeWriter — запись результата heartbeat-пинга в ClickHouse
	// (check_results), тот же писатель, что использует uptime.Runner,
	// когда режим включает и "uptime": один процесс = одна очередь
	// вставок в CH. Может быть nil даже при непустом Uptime (в теории),
	// тогда heartbeat пропускает запись в CH, но всё равно отвечает 200 —
	// last_beat_at/monitor_state уже обновлены, это самое важное.
	UptimeWriter *uptime.ResultWriter
	// LocalRegion — регион, которым heartbeat помечает свои
	// ApplyResult/Writer.Add вызовы (тот же регион, которым локальный
	// uptime.Runner помечает свои проверки, cfg.LocalRegion в
	// cmd/gotcha/main.go). Пусто (значение по умолчанию для стендов,
	// которые не выставляют это поле явно) — используется
	// uptime.DefaultRegion.
	LocalRegion string

	// UptimeQuery — чтение агрегатов check_results из ClickHouse (план 4,
	// задача 2): список мониторов и страница монитора читают uptime%,
	// среднюю задержку, полоску доступности, график задержек и ленту
	// последних проверок отсюда, а не из Uptime (PG) — та часть состояния
	// живёт только в PG (monitors/monitor_state/incidents). Как и
	// Alerts/Outbox/Uptime, проставляется отдельным полем, а не
	// принимается конструктором New; может быть nil (например, в стендах
	// остальных web-тестов, которым мониторы не нужны) — тогда маршруты
	// /projects/{id}/monitors и /monitors/{id} не должны вызываться
	// (панику на nil-разыменование тестами эти стенды не бьют, так как они
	// его и не запрашивают).
	UptimeQuery *uptime.Query
	// UptimeIngestor — общий с локальной пробой хвост обработки результата
	// (claim → CH → ApplyResult → детекция): через него /probe/results
	// проводит результаты выносных проб, чтобы детекция инцидентов и запись
	// в ClickHouse были ровно в одном месте (см. uptime.Ingestor). Как и
	// Uptime/UptimeWriter/UptimeQuery, собирается вызывающей стороной
	// (cmd/gotcha/main.go, тестовые стенды); nil — приём результатов на этом
	// узле недоступен (/probe/results отвечает 503, см. probeapi.go).
	UptimeIngestor *uptime.Ingestor

	// Trace — чтение агрегатов производительности из ClickHouse (этап 3, план 4,
	// задача 2): список эндпойнтов и страница эндпойнта читают перцентили,
	// throughput, гистограммы и примеры трейсов отсюда. Как и
	// Alerts/Outbox/Uptime/UptimeQuery, проставляется отдельным полем, а не
	// принимается конструктором New; может быть nil (в стендах прочих
	// web-тестов, которым перформанс не нужен) — тогда маршруты
	// /projects/{id}/performance* не должны вызываться (эти стенды их и не
	// запрашивают).
	Trace *trace.Query
	// PerfIssues — perf_issues в PG (тот же trace.NewIssueService, что и в
	// пайплайне детекции): страница эндпойнта показывает связанные с ним
	// проблемы (фильтр по culprit). Как и Trace, отдельное необязательное поле;
	// nil → секция связанных проблем на странице эндпойнта просто пустая.
	PerfIssues *trace.IssueService
	// Regressions — perf_regressions в PG (тот же trace.NewRegressionService, что
	// и в оценщике этапа 4): страница /projects/{id}/regressions показывает
	// открытые/закрытые регрессии производительности. Как и PerfIssues, отдельное
	// необязательное поле; nil → маршрут регрессий отвечает 404 (nil-guard).
	Regressions *trace.RegressionService

	// OAuth — включённые провайдеры social login (этап 5). nil/empty →
	// кнопки входа скрыты, роуты /auth/oauth/* отвечают 404 на любой
	// провайдер. Проставляется отдельным полем (как Alerts/Trace), New не
	// трогаем.
	OAuth *oauth.Registry

	// Metrics — чтение агрегатов метрик из ClickHouse (этап 6): страницы
	// /projects/{id}/metrics[/{name}]. Как Trace/Regressions — отдельное
	// необязательное поле; nil → маршруты метрик отвечают 404 (nil-guard).
	Metrics *metric.Query
	// MetricRules/MetricIncidents — правила и инциденты пороговых алертов на
	// метрики (этап 6, план 5): страница /projects/{id}/metrics/alerts. nil →
	// маршруты алертов метрик отвечают 404.
	MetricRules     *metric.RuleService
	MetricIncidents *metric.IncidentService

	// Profiles — чтение профилей из ClickHouse (этап 7): страницы
	// /projects/{id}/profiles[/flame]. Необязательное поле; nil → 404.
	Profiles *profile.Query
	// ProfileRegressions — регрессии self-CPU функций (этап 9): страница
	// /projects/{id}/profile-regressions. Необязательное поле; nil → 404.
	ProfileRegressions *profile.RegressionService

	// Purger — best-effort очистка телеметрии проекта/субъекта из ClickHouse
	// (PRIV-H2): PG-каскад не трогает CH, поэтому удаление проекта/данных
	// субъекта в UI досылает удаление в CH через этот интерфейс.
	// Проставляется отдельным полем (main.go: telemetry.NewPurger(ch)); nil —
	// PG-удаление всё равно проходит, а CH-очистка пропускается с slog.Warn
	// (стенды прочих web-тестов Purger не задают). См. ProjectPurger.
	Purger ProjectPurger

	// ssoProviders — процесс-локальный кеш per-org OIDC-провайдеров (этап 10,
	// см. sso.go). Нулевое значение готово к работе.
	ssoProviders ssoCache

	loginLimiter *rateLimiter
	// ipLimiter — глобальный per-IP лимитер входа/регистрации (SEC-L2): в
	// дополнение к per-account (ip|email) ограничивает суммарный поток попыток с
	// одного IP по РАЗНЫМ email, закрывая обход per-account лимита перебором.
	ipLimiter *rateLimiter
	// statusCache — 30-секундный кеш публичных статус-страниц по slug'у
	// (см. statuspage.go). Нулевое значение готово к работе, поэтому поле не
	// требует инициализации в New.
	statusCache statusCache
}

// localRegion возвращает h.LocalRegion, а если оно не задано —
// uptime.DefaultRegion (см. комментарий к полю).
func (h *Handler) localRegion() string {
	if h.LocalRegion == "" {
		return uptime.DefaultRegion
	}
	return h.LocalRegion
}

// New собирает Handler. BaseURL используется для sameOrigin-проверки POST-ов
// и для выставления Secure-флага сессионной cookie.
//
// RegistrationMode по умолчанию "open" — это исторический контракт конструктора
// (регистрация открыта). Продовая безопасность PROD-B1 живёт на уровне конфига:
// main.go всегда проставляет webHandler.RegistrationMode = cfg.RegistrationMode
// (GOTCHA_REGISTRATION, дефолт "invite"), переопределяя это значение.
func New(authSvc *auth.Service, orgSvc *org.Service, issueSvc *issue.Service, events *event.Query, baseURL string) *Handler {
	return &Handler{
		Auth:             authSvc,
		Org:              orgSvc,
		Issues:           issueSvc,
		Events:           events,
		BaseURL:          baseURL,
		Secure:           strings.HasPrefix(baseURL, "https://"),
		RegistrationMode: "open",
		loginLimiter:     newRateLimiter(time.Now, 5, time.Minute),
		ipLimiter:        newRateLimiter(time.Now, 20, time.Minute),
	}
}

// Register навешивает маршруты задачи 4 на mux. Все они собираются на
// внутреннем ServeMux и монтируются на переданный mux одним catch-all "/" —
// это даёт единую точку для securityHeaders (весь ответ Handler'а несёт
// базовые security-заголовки) и для стилизованной 404-страницы на
// незарегистрированных путях (иначе достался бы голый "404 page not found"
// от stdlib ServeMux).
func (h *Handler) Register(mux *http.ServeMux) {
	inner := http.NewServeMux()

	inner.HandleFunc("GET /login", h.loginPage)
	inner.HandleFunc("POST /login", h.loginSubmit)
	inner.HandleFunc("GET /register", h.registerPage)
	inner.HandleFunc("POST /register", h.registerSubmit)
	inner.HandleFunc("POST /logout", h.logout)
	// Enterprise-SSO (этап 10): identifier-first вход по email-домену.
	inner.HandleFunc("GET /sso", h.ssoPage)
	inner.HandleFunc("POST /sso", h.ssoSubmit)

	// OAuth/social login (этап 5): открыты для анонимов (вход), сессию для
	// потока привязки проверяем внутри хендлера.
	inner.HandleFunc("GET /auth/oauth/{provider}/start", h.oauthStart)
	inner.HandleFunc("GET /auth/oauth/{provider}/callback", h.oauthCallback)

	// Переключатель языка (задача 6): доступен и анониму — например, на
	// странице логина, до создания сессии.
	inner.HandleFunc("POST /settings/locale", h.localeSwitch)

	// Переключатель темы оформления: доступен и анониму (см. локаль выше).
	inner.HandleFunc("POST /settings/theme", h.themeSwitch)

	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic("web: embedded static assets missing: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(staticSub))
	inner.Handle("GET /static/", http.StripPrefix("/static/", cacheControl(fileServer)))

	inner.Handle("GET /{$}", h.requireUser(http.HandlerFunc(h.index)))

	inner.Handle("GET /profile", h.requireUser(http.HandlerFunc(h.profilePage)))
	inner.Handle("POST /profile/password", h.requireUser(http.HandlerFunc(h.profilePasswordSubmit)))
	inner.Handle("POST /profile/password/set", h.requireUser(http.HandlerFunc(h.profilePasswordSet)))
	inner.Handle("POST /profile/sessions/revoke", h.requireUser(http.HandlerFunc(h.profileSessionsRevoke)))
	inner.Handle("POST /profile/identities/unlink", h.requireUser(http.HandlerFunc(h.profileIdentityUnlink)))

	inner.Handle("GET /onboarding", h.requireUser(http.HandlerFunc(h.onboardingPage)))
	inner.Handle("POST /onboarding", h.requireUser(http.HandlerFunc(h.onboardingSubmit)))
	inner.Handle("GET /docs", h.requireUser(http.HandlerFunc(h.docsIndex)))
	inner.Handle("GET /docs/{slug}", h.requireUser(http.HandlerFunc(h.docsPage)))
	inner.Handle("GET /projects", h.requireUser(http.HandlerFunc(h.projectsList)))
	inner.Handle("GET /projects/{id}/setup", h.requireUser(http.HandlerFunc(h.projectSetup)))
	inner.Handle("GET /projects/{id}/issues", h.requireUser(http.HandlerFunc(h.issuesList)))
	inner.Handle("POST /projects/{id}/issues/bulk", h.requireUser(http.HandlerFunc(h.issuesBulk)))
	inner.Handle("GET /issues/{id}", h.requireUser(http.HandlerFunc(h.issueDetail)))
	inner.Handle("POST /issues/{id}/status", h.requireUser(http.HandlerFunc(h.issueSetStatus)))
	inner.Handle("POST /issues/{id}/assign", h.requireUser(http.HandlerFunc(h.issueAssign)))

	inner.Handle("GET /orgs/{id}/settings", h.requireUser(http.HandlerFunc(h.orgSettingsPage)))
	inner.Handle("POST /orgs/{id}/settings/role", h.requireUser(http.HandlerFunc(h.orgSettingsRole)))
	inner.Handle("POST /orgs/{id}/settings/remove", h.requireUser(http.HandlerFunc(h.orgSettingsRemove)))
	inner.Handle("POST /orgs/{id}/settings/leave", h.requireUser(http.HandlerFunc(h.orgSettingsLeave)))
	inner.Handle("POST /orgs/{id}/settings/invite", h.requireUser(http.HandlerFunc(h.orgSettingsInvite)))
	inner.Handle("POST /orgs/{id}/settings/sso", h.requireUser(http.HandlerFunc(h.orgSettingsSSO)))
	inner.Handle("POST /orgs/{id}/settings/sso/delete", h.requireUser(http.HandlerFunc(h.orgSettingsSSODelete)))
	// Удаление организации и удаление ПДн субъекта (PRIV-H2) — owner-only.
	inner.Handle("POST /orgs/{id}/settings/delete", h.requireUser(http.HandlerFunc(h.orgSettingsDelete)))
	inner.Handle("POST /orgs/{id}/settings/purge-subject", h.requireUser(http.HandlerFunc(h.orgSettingsPurgeSubject)))
	// Выгрузка ПДн субъекта (право на доступ, 152-ФЗ ст. 14, RA-L11) — owner-only.
	inner.Handle("POST /orgs/{id}/settings/export-subject", h.requireUser(http.HandlerFunc(h.orgSettingsExportSubject)))
	inner.Handle("GET /invite/{token}", h.requireUser(http.HandlerFunc(h.inviteAcceptPage)))
	inner.Handle("POST /invite/{token}", h.requireUser(http.HandlerFunc(h.inviteAcceptSubmit)))

	// Выносные пробы организации (этап 2, план 5, задача 3): owner/admin
	// организации (requireOrgRole, как остальные org-настройки). Роуты
	// регистрируются безусловно — как и /projects/{id}/monitors выше: в режимах
	// "web"/"all" h.Uptime всегда собран (см. cmd/gotcha/main.go), а стенды
	// прочих web-тестов эти страницы не запрашивают.
	inner.Handle("GET /orgs/{id}/probes", h.requireUser(http.HandlerFunc(h.orgProbesPage)))
	inner.Handle("POST /orgs/{id}/probes", h.requireUser(http.HandlerFunc(h.orgProbesCreate)))
	inner.Handle("POST /orgs/{id}/probes/revoke", h.requireUser(http.HandlerFunc(h.orgProbesRevoke)))

	inner.Handle("GET /orgs/{id}/teams", h.requireUser(http.HandlerFunc(h.teamsPage)))
	inner.Handle("POST /orgs/{id}/teams", h.requireUser(http.HandlerFunc(h.teamsCreate)))
	inner.Handle("POST /teams/{id}/members", h.requireUser(http.HandlerFunc(h.teamMembersAdd)))
	inner.Handle("POST /teams/{id}/members/remove", h.requireUser(http.HandlerFunc(h.teamMembersRemove)))
	inner.Handle("POST /teams/{id}/projects", h.requireUser(http.HandlerFunc(h.teamProjectsAttach)))
	inner.Handle("POST /teams/{id}/projects/detach", h.requireUser(http.HandlerFunc(h.teamProjectsDetach)))

	inner.Handle("GET /projects/{id}/metrics", h.requireUser(http.HandlerFunc(h.metricsList)))
	inner.Handle("GET /projects/{id}/metrics/alerts", h.requireUser(http.HandlerFunc(h.metricAlertsPage)))
	inner.Handle("POST /projects/{id}/metrics/alerts", h.requireUser(http.HandlerFunc(h.metricAlertCreate)))
	inner.Handle("POST /projects/{id}/metrics/alerts/delete", h.requireUser(http.HandlerFunc(h.metricAlertDelete)))
	inner.Handle("GET /projects/{id}/metrics/{name}", h.requireUser(http.HandlerFunc(h.metricDetail)))

	inner.Handle("GET /projects/{id}/profiles", h.requireUser(http.HandlerFunc(h.profilesList)))
	inner.Handle("GET /projects/{id}/profiles/flame", h.requireUser(http.HandlerFunc(h.profileFlame)))
	inner.Handle("GET /projects/{id}/profile-regressions", h.requireUser(http.HandlerFunc(h.profileRegressionsList)))

	inner.Handle("GET /projects/{id}/settings", h.requireUser(http.HandlerFunc(h.projectSettingsPage)))
	inner.Handle("POST /projects/{id}/settings/rename", h.requireUser(http.HandlerFunc(h.projectSettingsRename)))
	inner.Handle("POST /projects/{id}/settings/keys", h.requireUser(http.HandlerFunc(h.projectSettingsKeyCreate)))
	inner.Handle("POST /projects/{id}/settings/keys/revoke", h.requireUser(http.HandlerFunc(h.projectSettingsKeyRevoke)))
	inner.Handle("POST /projects/{id}/settings/performance", h.requireUser(http.HandlerFunc(h.projectSettingsPerformance)))
	inner.Handle("POST /projects/{id}/settings/regressions", h.requireUser(http.HandlerFunc(h.projectSettingsRegressions)))
	// Удаление проекта (PRIV-H2) — owner-only; после PG-удаления досылает
	// CH-очистку через h.Purger (best-effort).
	inner.Handle("POST /projects/{id}/settings/delete", h.requireUser(http.HandlerFunc(h.projectSettingsDelete)))

	inner.Handle("GET /projects/{id}/alerts", h.requireUser(http.HandlerFunc(h.alertsPage)))
	inner.Handle("GET /projects/{id}/alerts/deliveries", h.requireUser(http.HandlerFunc(h.alertDeliveriesPage)))
	inner.Handle("POST /projects/{id}/alerts/rules", h.requireUser(http.HandlerFunc(h.alertsRulesSave)))
	inner.Handle("POST /projects/{id}/alerts/channels", h.requireUser(http.HandlerFunc(h.alertsChannelCreate)))
	inner.Handle("POST /projects/{id}/alerts/channels/delete", h.requireUser(http.HandlerFunc(h.alertsChannelDelete)))

	inner.Handle("POST /orgs/{id}/settings/quota", h.requireUser(http.HandlerFunc(h.orgSettingsQuota)))

	// Мониторы доступности (план 4, задача 2): список и страница монитора —
	// просмотр открыт любому, у кого есть доступ к проекту
	// (CanAccessProject), паузa/резюм/удаление — только owner/admin
	// (requireProjectRole), тот же принцип, что и у issues/alerts выше.
	inner.Handle("GET /projects/{id}/monitors", h.requireUser(http.HandlerFunc(h.monitorsList)))
	inner.Handle("GET /monitors/{id}", h.requireUser(http.HandlerFunc(h.monitorDetail)))
	inner.Handle("POST /monitors/{id}/pause", h.requireUser(http.HandlerFunc(h.monitorPause)))
	inner.Handle("POST /monitors/{id}/resume", h.requireUser(http.HandlerFunc(h.monitorResume)))
	inner.Handle("POST /monitors/{id}/delete", h.requireUser(http.HandlerFunc(h.monitorDelete)))

	// Формы создания/редактирования монитора, инциденты и окна обслуживания
	// (план 4, задача 3): создание/редактирование — только owner/admin
	// (requireProjectRole), инциденты — любой участник проекта
	// (CanAccessProject, тот же принцип, что и monitorsList/monitorDetail
	// выше).
	inner.Handle("GET /projects/{id}/monitors/new", h.requireUser(http.HandlerFunc(h.monitorNewPage)))
	inner.Handle("POST /projects/{id}/monitors", h.requireUser(http.HandlerFunc(h.monitorCreate)))
	inner.Handle("GET /monitors/{id}/edit", h.requireUser(http.HandlerFunc(h.monitorEditPage)))
	inner.Handle("POST /monitors/{id}", h.requireUser(http.HandlerFunc(h.monitorUpdate)))

	inner.Handle("GET /projects/{id}/incidents", h.requireUser(http.HandlerFunc(h.incidentsList)))

	// Производительность (этап 3, план 4, задача 2): список эндпойнтов и
	// страница эндпойнта — только чтение, доступ открыт любому участнику
	// проекта (CanAccessProject → 404, как monitorsList/issuesList; POST'ов и
	// sameOrigin здесь нет). Имя транзакции недоверенное и может содержать
	// слэши — берём весь остаток пути ({transaction...}) и декодируем в
	// обработчике. Роуты регистрируются безусловно, как /projects/{id}/monitors:
	// в режимах "web"/"all" h.Trace всегда собран (см. cmd/gotcha/main.go), а
	// стенды прочих web-тестов эти страницы не запрашивают.
	inner.Handle("GET /projects/{id}/performance", h.requireUser(http.HandlerFunc(h.performanceList)))
	inner.Handle("GET /projects/{id}/performance/{transaction...}", h.requireUser(http.HandlerFunc(h.endpointDetail)))

	// Web Vitals (этап 4, план 2, задача 2): обзорная страница страниц проекта с
	// p75 LCP/INP/CLS — только чтение, доступ по CanAccessProject → 404, как
	// performanceList. Панель Web Vitals на странице эндпойнта отдельного роута
	// не имеет (рендерится в endpointDetail).
	inner.Handle("GET /projects/{id}/web-vitals", h.requireUser(http.HandlerFunc(h.webVitalsList)))

	// Perf-проблемы (этап 3, план 5, задача 1): список проблем проекта и страница
	// проблемы — просмотр открыт любому участнику проекта (CanAccessProject → 404,
	// как performanceList), смена статуса — только owner/admin (requireProjectRole
	// + sameOrigin, как issueSetStatus). Страница проблемы несёт в пути только
	// {id}, проект резолвится из самой проблемы (PerfIssues.ProjectOf). Роуты
	// регистрируются безусловно, как /projects/{id}/performance: в режимах
	// "web"/"all" h.PerfIssues всегда собран, а стенды прочих web-тестов эти
	// страницы не запрашивают.
	inner.Handle("GET /projects/{id}/perf-issues", h.requireUser(http.HandlerFunc(h.perfIssuesList)))
	inner.Handle("GET /perf-issues/{id}", h.requireUser(http.HandlerFunc(h.perfIssueDetail)))
	inner.Handle("POST /perf-issues/{id}/status", h.requireUser(http.HandlerFunc(h.perfIssueSetStatus)))

	// Регрессии производительности (этап 4, план 5, задача 1): список
	// открытых/закрытых регрессий проекта — только чтение, доступ по
	// CanAccessProject → 404, как perfIssuesList; POST'ов и sameOrigin нет
	// (регрессии закрывает оценщик). Роут регистрируется безусловно, как
	// /performance; nil-guard на h.Regressions отвечает 404 в стендах без
	// детекции.
	inner.Handle("GET /projects/{id}/regressions", h.requireUser(http.HandlerFunc(h.regressionsList)))

	// Waterfall трейса (этап 3, план 4, задача 3): доступ — по проекту трейса
	// (ProjectForTrace → CanAccessProject → 404), не по {id} в пути. Только
	// чтение, POST'ов и sameOrigin здесь нет. Как и /performance*,
	// регистрируется безусловно — h.Trace собран в режимах "web"/"all".
	inner.Handle("GET /traces/{trace_id}", h.requireUser(http.HandlerFunc(h.traceWaterfall)))
	inner.Handle("GET /traces/{trace_id}/flame", h.requireUser(http.HandlerFunc(h.traceFlame)))

	// Настройки статус-страниц проекта (план 5, задача 4): только owner/admin
	// организации проекта (requireProjectRole), как окна обслуживания. У
	// /statuspages/{id} проект берётся из самой страницы (loadManagedStatusPage),
	// чужая страница по её id — 404.
	inner.Handle("GET /projects/{id}/statuspages", h.requireUser(http.HandlerFunc(h.statusPagesPage)))
	inner.Handle("POST /projects/{id}/statuspages", h.requireUser(http.HandlerFunc(h.statusPagesCreate)))
	inner.Handle("POST /statuspages/{id}", h.requireUser(http.HandlerFunc(h.statusPagesUpdate)))
	inner.Handle("POST /statuspages/{id}/delete", h.requireUser(http.HandlerFunc(h.statusPagesDelete)))

	inner.Handle("GET /projects/{id}/maintenance", h.requireUser(http.HandlerFunc(h.maintenancePage)))
	inner.Handle("POST /projects/{id}/maintenance", h.requireUser(http.HandlerFunc(h.maintenanceCreate)))
	inner.Handle("POST /projects/{id}/maintenance/delete", h.requireUser(http.HandlerFunc(h.maintenanceDelete)))

	// Публичный heartbeat-пинг (этап 2, план 2, задача 3): без requireUser
	// (внешний вызов, не браузер) и без sameOrigin (см. heartbeat.go).
	// Регистрируется только когда Uptime собран вызывающей стороной —
	// стенды остальных web-тестов его не задают и не должны получать этот
	// роут.
	// Lease-протокол выносных проб (план 5, задача 1): как и heartbeat —
	// машинный API без сессии и без sameOrigin, аутентификация
	// Bearer-токеном пробы (см. probeapi.go). Регистрируется по тому же
	// условию: только когда Uptime собран вызывающей стороной.
	// Публичная статус-страница (план 5, задача 4): единственный браузерный
	// роут без сессии — её и должен видеть аноним (см. statuspage.go). Как и
	// heartbeat, регистрируется только когда Uptime собран вызывающей
	// стороной; ей нужен ещё и UptimeQuery (uptime% и полоска за 90 дней из
	// ClickHouse) — в режимах "web"/"all" оба поля выставляются вместе.
	if h.Uptime != nil {
		inner.HandleFunc("GET /uptime/hb/{token}", h.heartbeat)
		inner.HandleFunc("POST /uptime/hb/{token}", h.heartbeat)

		inner.HandleFunc("POST /probe/lease", h.probeLease)
		inner.HandleFunc("POST /probe/results", h.probeResults)

		inner.HandleFunc("GET /status/{slug}", h.statusPage)
	}

	// Fallback: любой путь, не покрытый паттернами выше, — стилизованная 404.
	inner.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
	})

	mux.Handle("/", h.securityHeaders(h.withLocale(h.withTheme(h.withShell(inner)))))
}

// cacheControl проставляет Cache-Control на статику.
func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		next.ServeHTTP(w, r)
	})
}

// cspHeader — все страницы Gotcha загружают только собственные ресурсы:
// app.css и htmx.min.js отдаются с того же origin (см. layout.templ), и ни
// один шаблон не использует inline <script>/<style> или style="" — поэтому
// 'self' без 'unsafe-inline' ничего не ломает. base-uri 'none' и
// frame-ancestors 'none' закрывают base-tag injection и clickjacking (второе
// дублирует X-Frame-Options для браузеров без поддержки CSP).
const cspHeader = "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// securityHeaders проставляет базовые защитные заголовки на весь ответ:
// запрет MIME-sniffing, запрет встраивания в <iframe> (защита от
// clickjacking), урезанный Referrer при переходах на чужие origin'ы и CSP.
// Strict-Transport-Security добавляется только когда h.Secure (BaseURL
// начинается с https://) — на голом HTTP-деплое (например, за прокси без
// TLS) отправлять HSTS нельзя, браузер надолго заблокирует http:// доступ.
func (h *Handler) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("Referrer-Policy", "same-origin")
		hdr.Set("Content-Security-Policy", cspHeader)
		if h.Secure {
			hdr.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// renderError отдаёт стилизованную страницу ошибки (layout + сообщение) с
// заданным HTTP-статусом — замена голому http.Error, которое ломает вид
// сайта на ошибках.
func (h *Handler) renderError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	w.WriteHeader(status)
	_ = templates.ErrorPage(status, msg, h.currentEmail(r)).Render(r.Context(), w)
}

// notFound — стилизованная страница 404 вместо голого http.NotFound (которое
// отдаёт неоформленный текст «404 page not found» на белом фоне). Используется
// во всех обработчиках вместо http.NotFound и как catch-all для несуществующих
// маршрутов (см. inner.Handle("/", …) в New).
func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
}

// renderConfirm отдаёт общую страницу подтверждения деструктивного действия
// (templates.ConfirmPage) вместо самого действия — двухшаговый POST с
// confirmed=yes (см. её комментарий): под CSP default-src 'self' без
// unsafe-inline инлайновые onsubmit/onclick с confirm() не исполняются,
// поэтому подтверждение обязано быть server-side. titleKey/messageKey/
// confirmLabelKey — i18n-ключи (переводятся здесь, а не в шаблоне, чтобы
// вызывающий обработчик оставался единственным местом, знающим о действии);
// action — URL того же обработчика (форма подтверждения шлёт POST туда же);
// hidden — поля исходного POST, которые нужно сохранить до подтверждённого
// повтора (например key_id).
func (h *Handler) renderConfirm(w http.ResponseWriter, r *http.Request, titleKey, messageKey, confirmLabelKey, cancelHref, action string, hidden []templates.HiddenField) {
	title := i18n.T(r.Context(), titleKey)
	message := i18n.T(r.Context(), messageKey)
	confirmLabel := i18n.T(r.Context(), confirmLabelKey)
	w.WriteHeader(http.StatusOK)
	_ = templates.ConfirmPage(title, message, confirmLabel, cancelHref, templ.SafeURL(action), hidden, h.currentEmail(r)).Render(r.Context(), w)
}

// index — GET /{$}: без доступных проектов ведёт себя по-разному в
// зависимости от того, есть ли у юзера организация. Юзер вовсе без
// организаций уводится на /onboarding — ему ещё только предстоит завести
// первую организацию и проект. Юзер, который уже состоит в чужой или
// собственной организации, но не привязан ни к одному проекту (например,
// admin ещё не добавил его ни в одну команду), видит стилизованную страницу
// «нет доступных проектов» вместо повторного онбординга — заводить вторую
// организацию ему не нужно. При наличии доступных проектов — редирект на
// issues первого из них; окончательный роутинг появится в задаче 5.
func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	projects, err := h.Org.ProjectsForUser(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if len(projects) == 0 {
		orgs, err := h.Org.OrgsOf(r.Context(), uid)
		if err != nil {
			h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
			return
		}
		if len(orgs) > 0 {
			_ = templates.NoProjects(h.currentEmail(r)).Render(r.Context(), w)
			return
		}
		http.Redirect(w, r, "/onboarding", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, projectIssuesPath(projects[0].ID), http.StatusSeeOther)
}

// projectIssuesPath — предварительный путь до issue-листинга проекта;
// окончательный роутинг появится в задаче 5.
func projectIssuesPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/issues"
}

// currentEmail — email текущего юзера для шапки (форма logout). Пустая
// строка на любую ошибку (нет сессии в контексте, юзер удалён, сбой БД) —
// эта функция обслуживает только рендер шапки и не должна ронять страницу.
func (h *Handler) currentEmail(r *http.Request) string {
	uid, ok := auth.UserID(r.Context())
	if !ok {
		return ""
	}
	email, err := h.Auth.UserEmail(r.Context(), uid)
	if err != nil {
		return ""
	}
	return email
}

// requireUser оборачивает auth.Service.RequireUser: для htmx-запросов
// (HX-Request: true) вместо 303-редиректа на /login отдаёт 200 с заголовком
// HX-Redirect — htmx сам выполнит переход, а не покажет частичный HTML.
func (h *Handler) requireUser(next http.Handler) http.Handler {
	inner := h.Auth.RequireUser(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hx := r.Header.Get("HX-Request") == "true"
		if !hx {
			inner.ServeHTTP(w, r)
			return
		}
		inner.ServeHTTP(&hxRedirectWriter{ResponseWriter: w}, r)
	})
}

// ProjectPurger — узкий интерфейс CH-очистки, которым владеет web-слой
// (telemetry.Purger ему удовлетворяет). Вынесен в web, чтобы тесты могли
// подменять его фейком и считать вызовы, не поднимая ClickHouse. Оба метода
// best-effort: PG-удаление первично, ошибка CH-очистки логируется, но
// пользовательскую операцию не роняет.
type ProjectPurger interface {
	PurgeProject(ctx context.Context, projectID int64) error
	PurgeSubject(ctx context.Context, projectID int64, sub telemetry.Subject) error
	// ExportSubject — выгрузка всех ПДн субъекта в рамках проекта (право
	// субъекта на доступ, 152-ФЗ ст. 14). В отличие от Purge*-методов не
	// best-effort: результат отдаётся пользователю, ошибку нельзя проглотить.
	ExportSubject(ctx context.Context, projectID int64, sub telemetry.Subject) (telemetry.SubjectExport, error)
}

// requireOrgRole проверяет роль userID в организации orgID: доступ к
// настройкам организации (участники, роли, приглашения) есть только у
// owner/admin. Любая другая роль или отсутствие членства (org.ErrNotMember) —
// 404, тот же принцип, что и у CanAccessProject: не палим существование
// чужой организации.
func (h *Handler) requireOrgRole(w http.ResponseWriter, r *http.Request, orgID, userID int64) (org.Role, bool) {
	role, err := h.Org.Role(r.Context(), orgID, userID)
	if err != nil {
		if errors.Is(err, org.ErrNotMember) {
			h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
			return "", false
		}
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return "", false
	}
	if role != org.RoleOwner && role != org.RoleAdmin {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
		return "", false
	}
	return role, true
}

// hxRedirectWriter перехватывает 303-редирект и превращает его в 200 +
// HX-Redirect, если запрос пришёл от htmx.
type hxRedirectWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *hxRedirectWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if code == http.StatusSeeOther {
		loc := w.Header().Get("Location")
		w.Header().Del("Location")
		w.Header().Set("HX-Redirect", loc)
		w.ResponseWriter.WriteHeader(http.StatusOK)
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *hxRedirectWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

// sameOrigin — защита POST-ов от CSRF без токенов: Origin (либо, если его
// нет, Referer) обязан совпадать с BaseURL по scheme+host. Обычные формы
// браузер всегда снабжает Origin, поэтому пустые Origin и Referer тоже
// считаются нарушением.
func sameOrigin(r *http.Request, baseURL string) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return false
	}
	return isSameOriginURL(src, baseURL)
}

// isSameOriginURL — совпадает ли raw (Origin/Referer) с baseURL по
// scheme+host. Вынесено из sameOrigin, чтобы им же сверять Referer при
// редиректах после POST (см. bulkRedirectTarget в issues.go).
func isSameOriginURL(raw, baseURL string) bool {
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Scheme == base.Scheme && u.Host == base.Host
}
