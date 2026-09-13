package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a-h/templ"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/deploy"
	"gitflic.ru/otezvikentiy/gotcha/internal/depsuppress"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/export"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
	"gitflic.ru/otezvikentiy/gotcha/internal/ingestsignal"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/logfilter"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/telemetry"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web/templates"
)

//go:embed static
var staticFiles embed.FS

type Handler struct {
	Auth    *auth.Service
	Org     *org.Service
	Issues  *issue.Service
	Events  *event.Query
	BaseURL string
	Secure  bool
	// пустое значение — дефолт из secret().
	SecretKey string

	// true, когда шифрование at-rest выключено (dev-ключ) — секреты форм ниже
	// уходят в PG открытым текстом; тот же признак, что у gotcha_secret_key_insecure.
	SecretKeyInsecure bool

	// XFF доверяем только от пиров отсюда — иначе игнорируется, ключ лимитера RemoteAddr.
	TrustedProxies []*net.IPNet

	// пустая строка — заголовок не ставится (HSTS отдан обратному прокси).
	HSTSHeader string

	// пустая строка — «не open» (закрыто, кроме bootstrap первого пользователя).
	RegistrationMode string

	// 0 — срок не задан, подпись «хранятся N дней» не рендерится.
	RetentionDays int

	// источник истины для traceWaterfall/performanceList; trace.SpanRetentionDays —
	// лишь дефолт первой установки, не читать его напрямую. 0 — TTL не задан.
	SpanRetentionDays int

	pages *http.ServeMux

	routes []string

	Alerts *alert.Service
	Email  *notify.EmailSender
	// дублирует смысл h.Email!=nil && h.Email.Configured(), вынесено отдельно,
	// чтобы UI не зависел от того, проставлен ли Email в конкретном стенде.
	EmailEnabled bool

	// при включённом скрубинге user_email/user_ip зануляются на приёме — поиск
	// субъекта по ним не найдёт ничего никогда.
	ScrubIP    bool
	ScrubEmail bool

	// веб-слою нужен не для ограничения, а для диагностики — nil-safe.
	Cardinality *ingest.CardinalityGuard
	Outbox      *notify.Outbox

	NotifyDirect *notify.Direct

	NotifyLocale i18n.Locale

	Uptime *uptime.Service
	// может быть nil даже при непустом Uptime — тогда heartbeat пропускает
	// запись в CH, но всё равно отвечает 200 (last_beat_at уже обновлён).
	UptimeWriter *uptime.ResultWriter
	// пусто — используется uptime.DefaultRegion.
	LocalRegion string

	UptimeQuery    *uptime.Query
	UptimeIngestor *uptime.Ingestor

	Trace       *trace.Query
	PerfIssues  *trace.IssueService
	Regressions *trace.RegressionService

	OAuth *oauth.Registry

	Metrics         *metric.Query
	MetricRules     *metric.RuleService
	MetricIncidents *metric.IncidentService

	SLO          *slo.Store
	SLOProviders map[slo.SLIKind]slo.Provider

	EscalationPolicy *escalation.PolicyStore

	AlertDeps *depsuppress.Store

	// nil (стенд/инстанс без подсистемы) не отдаёт 404 — «Обзор» рендерится
	// с пустыми секциями вместо ошибки.
	IncidentGroups *incidentgroup.Store

	Exports   *export.Store
	ExportDir string

	SuppressionGrace time.Duration

	// гейт всех хендлеров хостов — только h.Metrics == nil (см. hosts.go).
	Hosts         *host.Store
	HostIncidents *host.IncidentService
	HostSettings  *host.SettingsService
	HostOverrides *host.HostOverrideService
	// nil даже когда HostOverrides уже проставлен — резолвер трактует nil так
	// же, как «групповых порогов нет», каскад идёт дальше к project/default.
	GroupThresholds *host.GroupThresholdService
	// интерфейс, не конкретный *host.Toucher: в web-only режиме остаётся nil,
	// и Forget просто пропускается.
	HostForget HostForgetter

	Deploy *deploy.Store

	Signals *ingestsignal.Store

	LogQuery   *log.Query
	LogFilters *logfilter.Store
	// не экспортируется: внешние вызывающие не настраивают его напрямую.
	attrKeysCache *attrKeysCache
	// обрезает From окна списка логов снизу — без этого запрос шире TTL
	// сканирует партиции, где данных гарантированно уже нет. 0 — не применяется.
	LogRetentionDays int

	Profiles           *profile.Query
	ProfileRegressions *profile.RegressionService

	// PG-каскад не трогает CH — удаление проекта/данных субъекта в UI досылает
	// удаление в CH через этот интерфейс; nil — CH-очистка пропускается с Warn.
	Purger ProjectPurger

	// пусто бывает в dev-режиме (go run без docker) или при неверном каталоге —
	// GET /install.sh и /agent/{file} отвечают 404 с подсказкой, не паникуют.
	AgentDistDir string
	// поле per-Handler, не package-level: разные инстансы (например, в тестах
	// на разных временных каталогах) не делят один кеш.
	agentETags sync.Map

	ssoProviders ssoCache

	loginLimiter *rateLimiter
	// сдерживает перебор ОДНОГО email с пула IP — per-account/per-IP лимиты
	// порознь это пропускают; порог щедрый, чтобы не задеть легитимного.
	emailLimiter *rateLimiter
	// доп. к per-account: держит суммарный поток с одного IP по разным email.
	ipLimiter *rateLimiter
	// без капа аноним без единого ключа выбирает пул PostgreSQL и роняет UI,
	// алерты и квоты; порог щедрый (600/мин ≈ 10/с на IP).
	publicLimiter *rateLimiter
	// отдельный от publicLimiter: бинарь ~9.3 МиБ, общий лимит с этим весом
	// дал бы DoS-профиль через тысячи долгих соединений с одного IP.
	agentLimiter *rateLimiter
	// перебор slug'ов (успех vs 422 «занято») раскрывает, что slug занят —
	// лимит на создание делает перебор дорогим, не мешая легитимному.
	statusPageLimiter *rateLimiter
	// лимит активных заявок не ловит того, кто ставит заявку и сразу удаляет —
	// здесь ограничена частота тяжёлой выборки по ClickHouse.
	exportLimiter *rateLimiter
	// СВОИ, не emailLimiter/ipLimiter логина — общий ключ запирал бы жертву на её же входе.
	// IP-лимитер общий для /forgot-password и /reset-password/{token}.
	passwordResetEmailLimiter *rateLimiter
	passwordResetIPLimiter    *rateLimiter
	statusCache               statusCache

	crossOriginRejected atomic.Int64
	coThrottle          coThrottle
}

func (h *Handler) localRegion() string {
	if h.LocalRegion == "" {
		return uptime.DefaultRegion
	}
	return h.LocalRegion
}

// потолок разных ключей на лимитер (rl.maxKeys), не бюджет памяти — у каждого
// лимитера своя ожидаемая кардинальность и модель ключа.
const (
	loginLimiterMaxKeys      = 20000
	ipLimiterMaxKeys         = 20000
	emailLimiterMaxKeys      = 20000
	publicLimiterMaxKeys     = 20000
	agentLimiterMaxKeys      = 5000
	statusPageLimiterMaxKeys = 5000
	exportLimiterMaxKeys     = 5000
	passwordResetMaxKeys     = 20000
)

func New(authSvc *auth.Service, orgSvc *org.Service, issueSvc *issue.Service, events *event.Query, baseURL string) *Handler {
	return &Handler{
		Auth:                      authSvc,
		Org:                       orgSvc,
		Issues:                    issueSvc,
		Events:                    events,
		BaseURL:                   baseURL,
		Secure:                    strings.HasPrefix(baseURL, "https://"),
		HSTSHeader:                "max-age=31536000",
		RegistrationMode:          "open",
		loginLimiter:              newRateLimiter(time.Now, 5, time.Minute, loginLimiterMaxKeys, "loginLimiter"),
		ipLimiter:                 newRateLimiter(time.Now, 20, time.Minute, ipLimiterMaxKeys, "ipLimiter"),
		emailLimiter:              newRateLimiter(time.Now, 50, 15*time.Minute, emailLimiterMaxKeys, "emailLimiter"),
		publicLimiter:             newRateLimiter(time.Now, 600, time.Minute, publicLimiterMaxKeys, "publicLimiter"),
		agentLimiter:              newRateLimiter(time.Now, 10, time.Minute, agentLimiterMaxKeys, "agentLimiter"),
		statusPageLimiter:         newRateLimiter(time.Now, 12, time.Minute, statusPageLimiterMaxKeys, "statusPageLimiter"),
		exportLimiter:             newRateLimiter(time.Now, createRateLimit, createRateWindow, exportLimiterMaxKeys, "exportLimiter"),
		passwordResetEmailLimiter: newRateLimiter(time.Now, 5, 15*time.Minute, passwordResetMaxKeys, "passwordResetEmailLimiter"),
		passwordResetIPLimiter:    newRateLimiter(time.Now, 20, time.Minute, passwordResetMaxKeys, "passwordResetIPLimiter"),
		attrKeysCache:             newAttrKeysCache(),
	}
}

// стандартный ServeMux свои шаблоны наружу не отдаёт — без перехвата тут
// ручной список маршрутов отставал бы от факта регистрации.
type recordingMux struct {
	*http.ServeMux
	patterns []string
}

func (m *recordingMux) HandleFunc(pattern string, fn http.HandlerFunc) {
	m.patterns = append(m.patterns, pattern)
	m.ServeMux.HandleFunc(pattern, fn)
}

func (m *recordingMux) Handle(pattern string, handler http.Handler) {
	m.patterns = append(m.patterns, pattern)
	m.ServeMux.Handle(pattern, handler)
}

// единый catch-all "/" — общая точка для securityHeaders и стилизованной
// 404 вместо голого "404 page not found" от stdlib ServeMux.
func (h *Handler) Register(mux *http.ServeMux) {
	inner := &recordingMux{ServeMux: http.NewServeMux()}

	inner.HandleFunc("GET /login", h.loginPage)
	inner.HandleFunc("POST /login", h.loginSubmit)
	inner.HandleFunc("GET /register", h.registerPage)
	inner.HandleFunc("POST /register", h.registerSubmit)
	inner.HandleFunc("POST /logout", h.logout)
	inner.HandleFunc("GET /sso", h.ssoPage)
	inner.HandleFunc("POST /sso", h.ssoSubmit)
	inner.HandleFunc("GET /forgot-password", h.forgotPasswordPage)
	inner.HandleFunc("POST /forgot-password", h.forgotPasswordSubmit)
	inner.HandleFunc("GET /reset-password/{token}", h.resetPasswordPage)
	inner.HandleFunc("POST /reset-password/{token}", h.resetPasswordSubmit)

	// публичный: аноним по ссылке-приглашению должен видеть, куда его зовут,
	// не теряя токен под requireUser. Само чтение (InviteByToken) его не гасит.
	inner.HandleFunc("GET /invite/{token}", h.inviteAcceptPage)

	// открыты для анонимов; сессию для потока привязки проверяем внутри хендлера.
	inner.HandleFunc("GET /auth/oauth/{provider}/start", h.publicRateLimited(h.oauthStart))
	inner.HandleFunc("GET /auth/oauth/{provider}/callback", h.oauthCallback)

	// анонимный POST — limitFormBody здесь навешан явно, а не только через requireUser.
	inner.Handle("POST /settings/locale", h.limitFormBody(http.HandlerFunc(h.localeSwitch)))

	inner.Handle("POST /settings/theme", h.limitFormBody(http.HandlerFunc(h.themeSwitch)))

	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic("web: embedded static assets missing: " + err.Error())
	}
	assetVer := staticAssetVersion(staticSub)
	templates.SetAssetVersion(assetVer)
	fileServer := http.FileServer(http.FS(staticSub))
	// статика встроена и неизменна — гзип считаем один раз при старте, не на запросе.
	gz := buildGzipAssets(staticSub)
	inner.Handle("GET /static/", http.StripPrefix("/static/", cacheControl(assetVer, noDirListing(serveGzip(gz, fileServer)))))

	inner.Handle("GET /{$}", h.requireUser(http.HandlerFunc(h.index)))

	inner.Handle("GET /profile", h.requireUser(http.HandlerFunc(h.profilePage)))
	inner.Handle("POST /profile/password", h.requireUser(http.HandlerFunc(h.profilePasswordSubmit)))
	inner.Handle("POST /profile/password/set", h.requireUser(http.HandlerFunc(h.profilePasswordSet)))
	inner.Handle("POST /profile/delete", h.requireUser(http.HandlerFunc(h.profileDelete)))
	inner.Handle("POST /profile/sessions/revoke", h.requireUser(http.HandlerFunc(h.profileSessionsRevoke)))
	inner.Handle("POST /profile/identities/unlink", h.requireUser(http.HandlerFunc(h.profileIdentityUnlink)))
	inner.Handle("POST /profile/instance-admin/transfer", h.requireUser(http.HandlerFunc(h.profileInstanceAdminTransfer)))

	inner.Handle("GET /onboarding", h.requireUser(http.HandlerFunc(h.onboardingPage)))
	inner.Handle("POST /onboarding", h.requireUser(http.HandlerFunc(h.onboardingSubmit)))
	inner.Handle("GET /docs", h.requireUser(http.HandlerFunc(h.docsIndex)))
	inner.Handle("GET /docs/{slug}", h.requireUser(http.HandlerFunc(h.docsPage)))
	inner.Handle("GET /about", h.requireUser(http.HandlerFunc(h.aboutPage)))
	inner.Handle("GET /projects", h.requireUser(http.HandlerFunc(h.projectsRedirect)))
	inner.Handle("POST /projects/new", h.requireUser(http.HandlerFunc(h.projectCreate)))
	inner.Handle("GET /projects/{id}/setup", h.requireUser(http.HandlerFunc(h.projectSetup)))
	inner.Handle("GET /projects/{id}/issues", h.requireUser(http.HandlerFunc(h.issuesList)))
	inner.Handle("POST /projects/{id}/issues/bulk", h.requireUser(http.HandlerFunc(h.issuesBulk)))
	inner.Handle("GET /issues/{id}", h.requireUser(http.HandlerFunc(h.issueDetail)))
	inner.Handle("POST /issues/{id}/status", h.requireUser(http.HandlerFunc(h.issueSetStatus)))
	inner.Handle("POST /issues/{id}/assign", h.requireUser(http.HandlerFunc(h.issueAssign)))

	inner.Handle("GET /orgs/{id}/projects", h.requireUser(http.HandlerFunc(h.orgProjectsPage)))
	inner.Handle("GET /orgs/{id}/settings", h.requireUser(http.HandlerFunc(h.orgSettingsPage)))
	inner.Handle("POST /orgs/{id}/settings/role", h.requireUser(http.HandlerFunc(h.orgSettingsRole)))
	inner.Handle("POST /orgs/{id}/settings/remove", h.requireUser(http.HandlerFunc(h.orgSettingsRemove)))
	inner.Handle("POST /orgs/{id}/settings/leave", h.requireUser(http.HandlerFunc(h.orgSettingsLeave)))
	inner.Handle("POST /orgs/{id}/settings/invite", h.requireUser(http.HandlerFunc(h.orgSettingsInvite)))
	inner.Handle("POST /orgs/{id}/settings/invite/revoke", h.requireUser(http.HandlerFunc(h.orgSettingsInviteRevoke)))
	inner.Handle("POST /orgs/{id}/settings/sso", h.requireUser(http.HandlerFunc(h.orgSettingsSSO)))
	inner.Handle("POST /orgs/{id}/settings/sso/delete", h.requireUser(http.HandlerFunc(h.orgSettingsSSODelete)))
	inner.Handle("POST /orgs/{id}/settings/delete", h.requireUser(http.HandlerFunc(h.orgSettingsDelete)))
	inner.Handle("POST /orgs/{id}/settings/purge-subject", h.requireUser(http.HandlerFunc(h.orgSettingsPurgeSubject)))
	inner.Handle("POST /orgs/{id}/settings/export-subject", h.requireUser(http.HandlerFunc(h.orgSettingsExportSubject)))
	// требует сессию: принять приглашение может только вошедший.
	inner.Handle("POST /invite/{token}", h.requireUser(http.HandlerFunc(h.inviteAcceptSubmit)))

	inner.Handle("GET /orgs/{id}/probes", h.requireUser(http.HandlerFunc(h.orgProbesPage)))
	inner.Handle("POST /orgs/{id}/probes", h.requireUser(http.HandlerFunc(h.orgProbesCreate)))
	inner.Handle("POST /orgs/{id}/probes/revoke", h.requireUser(http.HandlerFunc(h.orgProbesRevoke)))

	inner.Handle("GET /orgs/{id}/teams", h.requireUser(http.HandlerFunc(h.teamsPage)))
	inner.Handle("POST /orgs/{id}/teams", h.requireUser(http.HandlerFunc(h.teamsCreate)))
	inner.Handle("POST /teams/{id}/rename", h.requireUser(http.HandlerFunc(h.teamRename)))
	inner.Handle("POST /teams/{id}/members", h.requireUser(http.HandlerFunc(h.teamMembersAdd)))
	inner.Handle("POST /teams/{id}/members/remove", h.requireUser(http.HandlerFunc(h.teamMembersRemove)))
	inner.Handle("POST /teams/{id}/projects", h.requireUser(http.HandlerFunc(h.teamProjectsAttach)))
	inner.Handle("POST /teams/{id}/projects/detach", h.requireUser(http.HandlerFunc(h.teamProjectsDetach)))
	inner.Handle("POST /teams/{id}/delete", h.requireUser(http.HandlerFunc(h.teamDelete)))
	inner.Handle("POST /profile/getting-started/hide", h.requireUser(http.HandlerFunc(h.gettingStartedHide)))

	inner.Handle("GET /projects/{id}/metrics", h.requireUser(http.HandlerFunc(h.metricsList)))
	inner.Handle("GET /projects/{id}/metrics/alerts", h.requireUser(http.HandlerFunc(h.metricAlertsPage)))
	inner.Handle("POST /projects/{id}/metrics/alerts", h.requireUser(http.HandlerFunc(h.metricAlertCreate)))
	inner.Handle("POST /projects/{id}/metrics/alerts/delete", h.requireUser(http.HandlerFunc(h.metricAlertDelete)))
	// литерал "delete" специфичнее {ruleID} — ServeMux разводит их независимо
	// от порядка регистрации.
	inner.Handle("POST /projects/{id}/metrics/alerts/{ruleID}", h.requireUser(http.HandlerFunc(h.metricAlertUpdate)))
	inner.Handle("GET /projects/{id}/metrics/{name}", h.requireUser(http.HandlerFunc(h.metricDetail)))

	inner.Handle("GET /projects/{id}/recipes", h.requireUser(http.HandlerFunc(h.recipesListPage)))
	inner.Handle("GET /projects/{id}/recipes/{slug}", h.requireUser(http.HandlerFunc(h.recipeDetailPage)))
	inner.Handle("POST /projects/{id}/recipes/{slug}/thresholds", h.requireUser(http.HandlerFunc(h.recipeThresholdsCreate)))

	inner.Handle("GET /projects/{id}/slos", h.requireUser(http.HandlerFunc(h.slosPage)))
	inner.Handle("GET /projects/{id}/slos/{sloID}", h.requireUser(http.HandlerFunc(h.sloDetail)))
	inner.Handle("POST /projects/{id}/slos", h.requireUser(http.HandlerFunc(h.sloCreate)))
	inner.Handle("POST /projects/{id}/slos/{sloID}/delete", h.requireUser(http.HandlerFunc(h.sloDelete)))

	// литерал "settings" перед {name} — ServeMux (Go 1.22) отдаёт приоритет
	// специфичному сегменту, поэтому хост с именем "settings" по карточке недоступен.
	inner.Handle("GET /projects/{id}/hosts", h.requireUser(http.HandlerFunc(h.hostsList)))
	inner.Handle("GET /projects/{id}/hosts/settings", h.requireUser(http.HandlerFunc(h.hostSettingsPage)))
	inner.Handle("POST /projects/{id}/hosts/settings", h.requireUser(http.HandlerFunc(h.hostSettingsSave)))
	inner.Handle("POST /projects/{id}/hosts/settings/groups", h.requireUser(http.HandlerFunc(h.hostGroupThresholdSave)))
	inner.Handle("POST /projects/{id}/hosts/settings/groups/delete", h.requireUser(http.HandlerFunc(h.hostGroupThresholdDelete)))
	inner.Handle("GET /projects/{id}/hosts/{name}", h.requireUser(http.HandlerFunc(h.hostDetail)))
	inner.Handle("POST /projects/{id}/hosts/{name}/thresholds", h.requireUser(http.HandlerFunc(h.hostThresholdsSave)))
	inner.Handle("POST /projects/{id}/hosts/{name}/delete", h.requireUser(http.HandlerFunc(h.hostDelete)))

	inner.Handle("GET /projects/{id}/logs", h.requireUser(http.HandlerFunc(h.logsList)))
	inner.Handle("GET /projects/{id}/logs/attr-keys", h.requireUser(http.HandlerFunc(h.logsAttrKeys)))

	// право зависит от вида фильтра (личный/общий), не от маршрута — второй
	// гейт requireLogFilterOperator в logfilters.go.
	inner.Handle("POST /projects/{id}/logs/filters", h.requireUser(http.HandlerFunc(h.logFiltersCreate)))
	inner.Handle("POST /projects/{id}/logs/filters/{filterID}/update", h.requireUser(http.HandlerFunc(h.logFiltersUpdate)))
	inner.Handle("POST /projects/{id}/logs/filters/{filterID}/delete", h.requireUser(http.HandlerFunc(h.logFiltersDelete)))
	inner.Handle("POST /projects/{id}/logs/filters/{filterID}/default", h.requireUser(http.HandlerFunc(h.logFiltersSetDefault)))

	inner.Handle("GET /projects/{id}/profiles", h.requireUser(http.HandlerFunc(h.profilesList)))
	inner.Handle("GET /projects/{id}/profiles/flame", h.requireUser(http.HandlerFunc(h.profileFlame)))
	inner.Handle("GET /projects/{id}/profile-regressions", h.requireUser(http.HandlerFunc(h.profileRegressionsList)))

	inner.Handle("GET /projects/{id}/settings", h.requireUser(http.HandlerFunc(h.projectSettingsPage)))
	inner.Handle("POST /projects/{id}/settings/rename", h.requireUser(http.HandlerFunc(h.projectSettingsRename)))
	inner.Handle("POST /projects/{id}/settings/keys", h.requireUser(http.HandlerFunc(h.projectSettingsKeyCreate)))
	inner.Handle("POST /projects/{id}/settings/keys/revoke", h.requireUser(http.HandlerFunc(h.projectSettingsKeyRevoke)))
	inner.Handle("POST /projects/{id}/settings/performance", h.requireUser(http.HandlerFunc(h.projectSettingsPerformance)))
	inner.Handle("POST /projects/{id}/settings/regressions", h.requireUser(http.HandlerFunc(h.projectSettingsRegressions)))
	// owner-only; после PG-удаления досылает best-effort CH-очистку через h.Purger.
	inner.Handle("POST /projects/{id}/settings/delete", h.requireUser(http.HandlerFunc(h.projectSettingsDelete)))

	inner.Handle("GET /projects/{id}/alerts", h.requireUser(http.HandlerFunc(h.alertsPage)))
	inner.Handle("GET /projects/{id}/alerts/deliveries", h.requireUser(http.HandlerFunc(h.alertDeliveriesPage)))
	inner.Handle("POST /projects/{id}/alerts/rules", h.requireUser(http.HandlerFunc(h.alertsRulesSave)))
	inner.Handle("POST /projects/{id}/alerts/channels", h.requireUser(http.HandlerFunc(h.alertsChannelCreate)))
	inner.Handle("POST /projects/{id}/alerts/channels/update", h.requireUser(http.HandlerFunc(h.alertsChannelUpdate)))
	inner.Handle("POST /projects/{id}/alerts/channels/delete", h.requireUser(http.HandlerFunc(h.alertsChannelDelete)))
	inner.Handle("POST /projects/{id}/alerts/channels/test", h.requireUser(http.HandlerFunc(h.alertsChannelTest)))

	inner.Handle("GET /projects/{id}/escalations", h.requireUser(http.HandlerFunc(h.escalationsPage)))
	inner.Handle("POST /projects/{id}/escalations", h.requireUser(http.HandlerFunc(h.escalationsSave)))

	inner.Handle("GET /projects/{id}/alert-suppression", h.requireUser(http.HandlerFunc(h.alertSuppressionPage)))
	inner.Handle("POST /projects/{id}/alert-suppression", h.requireUser(http.HandlerFunc(h.alertSuppressionSave)))
	inner.Handle("POST /projects/{id}/alert-suppression/{depID}", h.requireUser(http.HandlerFunc(h.alertSuppressionUpdate)))
	inner.Handle("POST /projects/{id}/alert-suppression/{depID}/delete", h.requireUser(http.HandlerFunc(h.alertSuppressionDelete)))

	// доп. проверка авторства/CanManage внутри download/delete/списка (exports.go).
	inner.Handle("GET /projects/{id}/exports", h.requireUser(http.HandlerFunc(h.exportsPage)))
	inner.Handle("POST /projects/{id}/exports", h.requireUser(http.HandlerFunc(h.exportsCreate)))
	inner.Handle("GET /projects/{id}/exports/{jobID}/download", h.requireUser(http.HandlerFunc(h.exportsDownload)))
	inner.Handle("POST /projects/{id}/exports/{jobID}/delete", h.requireUser(http.HandlerFunc(h.exportsDelete)))

	inner.Handle("POST /projects/{id}/incidents/{source}/{incident_id}/ack", h.requireUser(http.HandlerFunc(h.incidentAck)))

	inner.Handle("POST /orgs/{id}/settings/quota", h.requireUser(http.HandlerFunc(h.orgSettingsQuota)))

	inner.Handle("GET /projects/{id}/monitors", h.requireUser(http.HandlerFunc(h.monitorsList)))
	inner.Handle("GET /monitors/{id}", h.requireUser(http.HandlerFunc(h.monitorDetail)))
	inner.Handle("POST /monitors/{id}/pause", h.requireUser(http.HandlerFunc(h.monitorPause)))
	inner.Handle("POST /monitors/{id}/resume", h.requireUser(http.HandlerFunc(h.monitorResume)))
	inner.Handle("POST /monitors/{id}/delete", h.requireUser(http.HandlerFunc(h.monitorDelete)))
	inner.Handle("POST /monitors/{id}/heartbeat/regenerate", h.requireUser(http.HandlerFunc(h.monitorHeartbeatRegenerate)))

	inner.Handle("GET /projects/{id}/monitors/new", h.requireUser(http.HandlerFunc(h.monitorNewPage)))
	inner.Handle("POST /projects/{id}/monitors", h.requireUser(http.HandlerFunc(h.monitorCreate)))
	inner.Handle("GET /monitors/{id}/edit", h.requireUser(http.HandlerFunc(h.monitorEditPage)))
	inner.Handle("POST /monitors/{id}", h.requireUser(http.HandlerFunc(h.monitorUpdate)))

	inner.Handle("GET /projects/{id}/incidents", h.requireUser(http.HandlerFunc(h.incidentsList)))
	inner.Handle("GET /projects/{id}/overview", h.requireUser(http.HandlerFunc(h.overview)))
	inner.Handle("GET /projects/{id}/incident-feed", h.requireUser(http.HandlerFunc(h.incidentFeedRedirect)))

	// имя транзакции недоверенное и может содержать слэши — берём весь остаток
	// пути ({transaction...}) и декодируем в обработчике.
	inner.Handle("GET /projects/{id}/performance", h.requireUser(http.HandlerFunc(h.performanceList)))
	inner.Handle("GET /projects/{id}/performance/{transaction...}", h.requireUser(http.HandlerFunc(h.endpointDetail)))

	inner.Handle("GET /projects/{id}/dependencies", h.requireUser(http.HandlerFunc(h.dependencies)))

	inner.Handle("GET /projects/{id}/web-vitals", h.requireUser(http.HandlerFunc(h.webVitalsList)))

	inner.Handle("GET /projects/{id}/perf-issues", h.requireUser(http.HandlerFunc(h.perfIssuesList)))
	inner.Handle("GET /perf-issues/{id}", h.requireUser(http.HandlerFunc(h.perfIssueDetail)))
	inner.Handle("POST /perf-issues/{id}/status", h.requireUser(http.HandlerFunc(h.perfIssueSetStatus)))

	inner.Handle("GET /projects/{id}/regressions", h.requireUser(http.HandlerFunc(h.regressionsList)))

	inner.Handle("GET /projects/{id}/deployments", h.requireUser(http.HandlerFunc(h.deployments)))

	inner.Handle("GET /traces/{trace_id}", h.requireUser(http.HandlerFunc(h.traceWaterfall)))
	inner.Handle("GET /traces/{trace_id}/flame", h.requireUser(http.HandlerFunc(h.traceFlame)))

	inner.Handle("GET /projects/{id}/statuspages", h.requireUser(http.HandlerFunc(h.statusPagesPage)))
	inner.Handle("POST /projects/{id}/statuspages", h.requireUser(http.HandlerFunc(h.statusPagesCreate)))
	inner.Handle("POST /statuspages/{id}", h.requireUser(http.HandlerFunc(h.statusPagesUpdate)))
	inner.Handle("POST /statuspages/{id}/delete", h.requireUser(http.HandlerFunc(h.statusPagesDelete)))

	inner.Handle("GET /projects/{id}/maintenance", h.requireUser(http.HandlerFunc(h.maintenancePage)))
	inner.Handle("POST /projects/{id}/maintenance", h.requireUser(http.HandlerFunc(h.maintenanceCreate)))
	inner.Handle("POST /projects/{id}/maintenance/update", h.requireUser(http.HandlerFunc(h.maintenanceUpdate)))
	inner.Handle("POST /projects/{id}/maintenance/delete", h.requireUser(http.HandlerFunc(h.maintenanceDelete)))

	if h.Uptime != nil {
		inner.HandleFunc("GET /uptime/hb/{token}", h.publicRateLimited(h.heartbeat))
		inner.HandleFunc("POST /uptime/hb/{token}", h.publicRateLimited(h.heartbeat))

		inner.HandleFunc("POST /probe/lease", h.publicRateLimited(h.probeLease))
		inner.HandleFunc("POST /probe/results", h.publicRateLimited(h.probeResults))

		inner.HandleFunc("GET /status/{key}", h.publicRateLimited(h.statusPage))
	}

	inner.HandleFunc("GET /install.sh", h.publicRateLimited(h.installSh))
	// свой agentLimiter, не publicLimiter: раздача тяжелее по трафику/времени
	// соединения, режется отдельно.
	inner.HandleFunc("GET /agent/{file}", h.agentDistRateLimited(h.agentFile))

	inner.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
	})

	// h.pages остаётся *http.ServeMux (не recordingMux) — RoutePattern работает
	// через h.pages.Handler и не должен зависеть от обёртки.
	h.pages = inner.ServeMux
	h.routes = inner.patterns
	mux.Handle("/", h.securityHeaders(h.withLocale(h.withTheme(h.withFlash(h.withShell(inner))))))
}

// хэш меняется при любом изменении ассета — браузеры не отдают старую версию
// CSS/JS из кэша после деплоя.
func staticAssetVersion(fsys fs.FS) string {
	names := make([]string, 0, 8)
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return nil
	})
	sort.Strings(names) // детерминированный порядок независимо от обхода FS
	h := sha256.New()
	for _, n := range names {
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			continue
		}
		_, _ = h.Write([]byte(n))
		_, _ = h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// иммутабельный кэш только при текущем ?v; чужой/устаревший — короткий, иначе
// кэш прокси/CDN «прибивает» их к неизменяемому URL на год.
func cacheControl(version string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.URL.Query().Get("v"); v != "" && v == version {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "max-age=3600")
		}
		next.ServeHTTP(w, r)
	})
}

// без guard http.FileServer печатает листинг каталога (/static/, /static/icons/).
func noDirListing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// 'self' без 'unsafe-inline' работает: ни один шаблон не использует inline-скрипты/стили.
// base-uri/frame-ancestors 'none' — от base-tag injection и clickjacking.
const cspHeader = "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// HSTS только когда h.Secure (https) — на голом HTTP-деплое отправлять его
// нельзя, браузер надолго заблокирует http:// доступ.
func (h *Handler) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("Referrer-Policy", "same-origin")
		hdr.Set("Content-Security-Policy", cspHeader)
		// SSR-страницы несут ПДн — не должны оседать в дисковом кэше/bfcache
		// после логаута; /static перекрывает своим Cache-Control ниже по цепочке.
		hdr.Set("Cache-Control", "no-store")
		if h.Secure && h.HSTSHeader != "" {
			hdr.Set("Strict-Transport-Security", h.HSTSHeader)
		}
		next.ServeHTTP(w, r)
	})
}

// нужен, чтобы отличить «обработчик отверг id» от «маршрута нет» — оба дают 404.
func (h *Handler) RoutePattern(method, path string) string {
	if h.pages == nil {
		return ""
	}
	req, err := http.NewRequest(method, path, nil)
	if err != nil {
		return ""
	}
	_, pattern := h.pages.Handler(req)
	return pattern
}

// нужен сторожу на Origin: перебор всех маршрутов, а не литеральный список,
// который отстаёт от регистрации.
func (h *Handler) RegisteredRoutes() []string {
	return h.routes
}

func (h *Handler) renderError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	// Content-Type ставим до WriteHeader — иначе автоопределение не сработает
	// и заголовка не будет вовсе.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = templates.ErrorPage(status, msg, h.currentEmail(r)).Render(r.Context(), w)
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
}

// двухшаговый POST — под CSP без unsafe-inline onclick="confirm()" не
// исполняется, подтверждение обязано быть server-side.
func (h *Handler) renderConfirm(w http.ResponseWriter, r *http.Request, titleKey, messageKey, confirmLabelKey, cancelHref, action string, hidden []templates.HiddenField) {
	h.renderConfirmf(w, r, titleKey, messageKey, confirmLabelKey, cancelHref, action, hidden)
}

func (h *Handler) renderConfirmf(w http.ResponseWriter, r *http.Request, titleKey, messageKey, confirmLabelKey, cancelHref, action string, hidden []templates.HiddenField, kv ...string) {
	title := i18n.T(r.Context(), titleKey)
	message := i18n.Tf(r.Context(), messageKey, kv...)
	confirmLabel := i18n.T(r.Context(), confirmLabelKey)
	w.WriteHeader(http.StatusOK)
	_ = templates.ConfirmPage(title, message, confirmLabel, cancelHref, templ.SafeURL(action), hidden, h.currentEmail(r)).Render(r.Context(), w)
}

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
			canCreate := false
			for _, o := range orgs {
				role, err := h.Org.Role(r.Context(), o.ID, uid)
				if err == nil && (role == org.RoleOwner || role == org.RoleAdmin) {
					canCreate = true
					break
				}
			}
			_ = templates.NoProjects(canCreate, h.currentEmail(r)).Render(r.Context(), w)
			return
		}
		http.Redirect(w, r, "/onboarding", http.StatusSeeOther)
		return
	}
	// без запомненного проекта — НЕ первый проект списка (не подменять явный
	// выбор организации), а список первой по порядку организации.
	if id := projCookieID(r); id != 0 {
		for _, p := range projects {
			if p.ID == id {
				http.Redirect(w, r, overviewPath(id), http.StatusSeeOther)
				return
			}
		}
	}
	orgs, err := h.Org.OrgsOf(r.Context(), uid)
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, i18n.T(r.Context(), "error.internal"))
		return
	}
	if len(orgs) == 0 {
		// не должно случаться (проект без организации не существует) — тот же
		// тупиковый выход про запас.
		http.Redirect(w, r, "/onboarding", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, orgProjectsPath(orgs[0].ID), http.StatusSeeOther)
}

func projectIssuesPath(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/issues"
}

// любая ошибка — «», не паника: обслуживает только шапку, важнее не уронить страницу.
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

// requireUser не оборачивает /invite/{token} — currentEmail тут всегда вернула
// бы «», даже вошедшему; ищем сессию напрямую.
func (h *Handler) currentEmailPublic(r *http.Request) string {
	tok, ok := auth.ReadSessionToken(r, h.Secure)
	if !ok {
		return ""
	}
	uid, err := h.Auth.SessionUser(r.Context(), tok)
	if err != nil {
		return ""
	}
	email, err := h.Auth.UserEmail(r.Context(), uid)
	if err != nil {
		return ""
	}
	return email
}

// вложенные http.MaxBytesReader считают одни и те же байты — срабатывает
// наименьший предел, не последний применённый (auth/heartbeat/probe ставят свой).
const formBodyMaxBytes = 64 << 10 // 64 KiB

func (h *Handler) limitFormBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, formBodyMaxBytes)
		next.ServeHTTP(w, r)
	})
}

// превышение лимита тела — 413, не общий 400: клиент должен отличить сломанную
// форму от тела сверх лимита.
func (h *Handler) parseForm(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.renderError(w, r, http.StatusRequestEntityTooLarge, i18n.T(r.Context(), "error.body_too_large"))
			return false
		}
		h.renderError(w, r, http.StatusBadRequest, i18n.T(r.Context(), "error.bad_request"))
		return false
	}
	return true
}

// htmx-запросам (HX-Request) вместо 303 отдаём 200 + HX-Redirect — иначе htmx
// покажет частичный HTML редиректной страницы.
func (h *Handler) requireUser(next http.Handler) http.Handler {
	inner := h.Auth.RequireUser(h.limitFormBody(next))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hx := r.Header.Get("HX-Request") == "true"
		if !hx {
			inner.ServeHTTP(w, r)
			return
		}
		inner.ServeHTTP(&hxRedirectWriter{ResponseWriter: w}, r)
	})
}

type ProjectPurger interface {
	PurgeProject(ctx context.Context, projectID int64) error
	// разница между «удалено N» и «не найдено» важна для 152-ФЗ — скрубинг мог
	// занулить email/IP на приёме.
	PurgeSubject(ctx context.Context, projectID int64, sub telemetry.Subject) (telemetry.PurgeResult, error)
	// не best-effort, в отличие от Purge*: результат отдаётся пользователю,
	// ошибку нельзя проглотить.
	ExportSubject(ctx context.Context, projectID int64, sub telemetry.Subject) (telemetry.SubjectExport, error)
}

// нечлену — 404, не 403: не палим существование чужой организации (тот же
// принцип, что у CanAccessProject).
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
	// роли не хватает — 403: участник и так знает про организацию, «не найдено»
	// на знакомой странице читалось бы как поломка.
	if role != org.RoleOwner && role != org.RoleAdmin {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.403.body"))
		return "", false
	}
	return role, true
}

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

// пустые Origin и Referer тоже считаются нарушением — обычная форма браузера
// всегда шлёт Origin.
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
