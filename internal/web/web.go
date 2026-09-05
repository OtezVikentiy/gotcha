// Package web — серверный (SSR) UI поверх auth/org/issue/event: роутер,
// страницы аутентификации, статика. Каждая страница работает без JS
// (обычные формы и ссылки): клиентского JS в продукте минимум — только
// прогрессивное улучшение пикера дат (static/daterange.js).
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

	// TrustedProxies — сети доверенных reverse-proxy (GOTCHA_TRUSTED_PROXIES).
	// Только когда непосредственный пир входит сюда, clientIP доверяет
	// X-Forwarded-For и извлекает настоящий IP клиента (SEC-L2, см. clientIP).
	// Пусто — XFF игнорируется, ключом лимитера служит RemoteAddr.
	TrustedProxies []*net.IPNet

	// HSTSHeader — готовое значение Strict-Transport-Security, собранное на
	// старте из GOTCHA_HSTS_* (см. HSTSHeaderValue). Пустая строка — заголовок
	// не ставится (HSTS отдан обратному прокси). Дефолт конструктора —
	// исторический "max-age=31536000"; main.go всегда перезаписывает его
	// значением из конфига, тем же приёмом, что и RegistrationMode.
	HSTSHeader string

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

	// SpanRetentionDays — срок хранения spans (детали waterfall трейса) в днях
	// (GOTCHA_SPAN_RETENTION_DAYS). Проставляется из cfg.SpanRetentionDays в
	// main.go, применяется на каждом старте к TTL таблицы spans
	// (db.ApplySpanRetention) — настраивается независимо от RetentionDays
	// (события) и от TTL таблицы transactions, которая живёт дольше. Источник
	// истины для traceWaterfall/performanceList при решении, истекли ли spans
	// у конкретного трейса; trace.SpanRetentionDays — лишь справочный дефолт
	// первой установки (для доков/тестов), не читать его напрямую в хендлерах.
	// 0 — TTL не задан (спаны хранятся вечно); тогда истёкшими spans не
	// считаются вовсе, а «пустой Trace()» трактуется как ручное удаление.
	SpanRetentionDays int

	// pages — роутер страниц (внутренний mux). Заполняется в Register;
	// читается только RoutePattern.
	pages *http.ServeMux

	// routes — шаблоны всех маршрутов, зарегистрированных в Register (находка
	// №18 аудита, задача 10 плана сторожей): собирает recordingMux при
	// регистрации, наружу отдаётся RegisteredRoutes(). Нужен сторожу на
	// Origin — стандартный ServeMux свои шаблоны не отдаёт, а ручной список
	// маршрутов отстаёт от факта регистрации (см. RegisteredRoutes).
	routes []string

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

	// ScrubIP/ScrubEmail — зеркало серверного скрубинга приёма
	// (GOTCHA_SCRUB_IP/GOTCHA_SCRUB_EMAIL). Веб-слою они нужны не для того,
	// чтобы что-то маскировать, а чтобы не врать в форме удаления ПДн: при
	// включённом скрубинге колонки user_email/user_ip зануляются на приёме, и
	// поиск субъекта по ним не найдёт ничего никогда.
	ScrubIP    bool
	ScrubEmail bool

	// Cardinality — ограничитель кардинальности приёма. Веб-слою нужен не для
	// ограничения, а для ДИАГНОСТИКИ: показать, по какому полю проект упёрся в
	// потолок и какие значения схлопнулись. nil — методы nil-safe.
	Cardinality *ingest.CardinalityGuard
	// Outbox — очередь доставки алертов (план 6, задача 5, spec §7): страница
	// /projects/{id}/alerts показывает таблицу failed-доставок
	// (FailedForProject), чтобы отказы каналов были видны в UI, а не только в
	// логах воркера. Как и Alerts, проставляется отдельным полем, а не
	// принимается конструктором New.
	Outbox *notify.Outbox

	// NotifyDirect — синхронная отправка тестового сообщения в канал (№69),
	// мимо outbox: результат нужен немедленно, а не «когда-нибудь доставим».
	// Те же Senders/Secrets, что у notify.Worker. nil — кнопка теста отвечает
	// 404 (стенды без доставки).
	NotifyDirect *notify.Direct

	// NotifyLocale — локаль ИНСТАНСА (GOTCHA_LOCALE) для текстов тестового
	// сообщения: получатель канала тот же, что у настоящих алертов.
	NotifyLocale i18n.Locale

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

	// SLO/SLOProviders — определения SLO (PG) и SLI-провайдеры (CH) для
	// раздела /projects/{id}/slos (план D1): список с достижением/остатком
	// бюджета и форма создания. Как Metrics — необязательные поля; SLO nil →
	// маршруты SLO отвечают 404 (nil-guard). SLOProviders nil → список
	// рендерится без процентов (HasData=false), расчёт достижения пропускается.
	SLO          *slo.Store
	SLOProviders map[slo.SLIKind]slo.Provider

	// EscalationPolicy — политика эскалации проекта (лесенки critical/warning,
	// B4 T7): раздел /projects/{id}/escalations читает и правит её тем же
	// стором, что резолвят все пять оценщиков на открытии инцидента (T2). nil →
	// маршруты эскалаций отвечают 404, тот же nil-guard, что и у SLO/Alerts.
	EscalationPolicy *escalation.PolicyStore

	// AlertDeps — рёбра зависимостей между узлами проекта для подавления
	// шторма алертов (B5, задача 9): раздел /projects/{id}/alert-suppression
	// читает/правит их тем же Store, что резолвят оценщики при подавлении
	// дочернего инцидента. nil → маршруты отвечают 404, тот же nil-guard, что
	// и у EscalationPolicy/SLO выше.
	AlertDeps *depsuppress.Store

	// IncidentGroups — группы инцидентов (D3): секции «Обзора»
	// (/projects/{id}/overview, задача 6 nav-ia — бывшая сводная лента
	// /incident-feed, теперь редиректящая туда). nil (стенд/инстанс без
	// подсистемы) НЕ отдаёт 404 — «Обзор» является дверью по умолчанию
	// (index()), страница рендерится с пустыми секциями вместо ошибки, см.
	// докблок overview.go.
	IncidentGroups *incidentgroup.Store

	// Exports — очередь заявок на выгрузку ошибок/событий (E1): раздел
	// /projects/{id}/exports читает/ставит заявки тем же Store, что и
	// воркер/джанитор фичи. nil → маршруты отвечают 404, тот же nil-guard,
	// что и у AlertDeps/IncidentGroups выше (фича выключена — каталог
	// выгрузок недоступен на этом инстансе).
	Exports *export.Store
	// ExportDir — каталог файлов выгрузки на диске (GOTCHA_EXPORT_DIR),
	// нужен отдельно от Exports: скачивание отдаёт файл по id заявки, не
	// проходя через воркер/конфиг сборки.
	ExportDir string

	// SuppressionGrace — задержка первого уведомления узла с
	// задекларированным родителем (GOTCHA_DEPENDENCY_SETTLE_SECONDS, та же
	// величина, что settleGrace у depSuppressor/uptime.Detector/
	// escalation.Scheduler в cmd/gotcha/main.go): экран подавления шторма
	// показывает её оператору, а не оставляет догадываться (устранение
	// аудита B5, P2-1). Нулевое значение (main.go не проведён / узкий
	// тестовый стенд) — подсказка про грейс молча не показывается, тот же
	// nil-safe принцип, что и у Hosts/Uptime в suppressionNodes.
	SuppressionGrace time.Duration

	// Hosts/HostIncidents/HostSettings — реестр хостов, их встроенные
	// инциденты (диск/память/нагрузка/тишина) и пороги (план A1): страницы
	// /projects/{id}/hosts[...]. Как Metrics — отдельные необязательные поля;
	// nil → маршруты хостов отвечают 404. Гейт всех хендлеров хостов — только
	// h.Metrics == nil (см. hosts.go): main.go всегда проставляет эту тройку
	// вместе с Metrics, поэтому по факту они nil одновременно.
	Hosts         *host.Store
	HostIncidents *host.IncidentService
	HostSettings  *host.SettingsService
	// HostOverrides — per-host переопределения порогов (план B2, T6):
	// карточка хоста читает/пишет их через ThresholdResolver.Effective (см.
	// hosts.go renderHostDetail/hostThresholdsSave). nil-guard в hostDetail —
	// такой же, как у Hosts/HostIncidents/HostSettings (main.go всегда
	// проставляет их вместе).
	HostOverrides *host.HostOverrideService
	// GroupThresholds — групповые пороги по role/env (план B2, T7). Отдельно
	// от HostOverrides по nil-безопасности: пока T7 не отгружен, поле может
	// оставаться nil даже когда HostOverrides уже проставлен — резолвер
	// (ThresholdResolver.Groups) трактует nil/пустой список так же, как
	// «групповых порогов нет», каскад просто идёт дальше к project/default.
	GroupThresholds *host.GroupThresholdService
	// HostForget — инвалидация троттлера регистрации хостов (host.Toucher)
	// при удалении хоста (Task 15): следующий Touch того же имени не должен
	// молча проглатываться троттлингом, будто хост уже зарегистрирован.
	// Интерфейс, а не конкретный *host.Toucher — nil-safe поле: в web-only
	// режиме (без ingest, см. main.go) остаётся nil, и Forget просто
	// пропускается (задокументированное ограничение однопроцессного
	// дефолта — при разнесённых web/ingest-репликах троттлер живёт в чужом
	// процессе и недостижим отсюда).
	HostForget HostForgetter

	// Deploy — Postgres-store деплоев (C5): вертикальные маркеры на графиках
	// проекта, экран-список деплоев и привязка регрессий к ближайшему
	// предшествующему деплою. Как Hosts/Metrics — отдельное необязательное
	// поле; nil → маркеры не рисуются, экран отвечает 404 (nil-guard).
	Deploy *deploy.Store

	// Signals — per-project сигналы приёма (аудит перед 1.0, K7-5/K7-6):
	// попадания на устаревшие пути приёма и отказы по ключу, которые раньше
	// были видны только process-local self-метриками без метки проекта.
	// Читается страницей настроек проекта (устаревшие пути) и страницей
	// issues (отказы по ключу на пустом списке/в чек-листе онбординга).
	// nil-safe, как Deploy/Trace выше: без него секции просто не рендерятся
	// — ни то, ни другое не критично для работы этих страниц.
	Signals *ingestsignal.Store

	// LogQuery — чтение структурированных логов из ClickHouse (C2, задача 2):
	// страница /projects/{id}/logs. Как Trace/Metrics — отдельное
	// необязательное поле; nil → маршрут логов отвечает 404 (nil-guard).
	LogQuery *log.Query
	// attrKeysCache — кеш ответов logsAttrKeys (задача 6, C2, §6 спеки:
	// «кеш per-project ~60с»). Заполняется в New() всегда (нужен вне
	// зависимости от того, проведён ли LogQuery — сам эндпоинт проверяет
	// h.LogQuery==nil раньше, чем заглянуть в кеш), не экспортируется:
	// внешние вызывающие (main.go, тестовые стенды) не настраивают его
	// напрямую, в отличие от LogQuery.
	attrKeysCache *attrKeysCache
	// LogRetentionDays — срок хранения логов в днях (GOTCHA_LOG_RETENTION_DAYS,
	// cfg.LogRetentionDays). Обрезает From окна списка логов снизу независимо
	// от выбранного пресета: без этого запрос за окно шире фактического TTL
	// сканирует партиции, в которых данных гарантированно уже нет. 0 — срок не
	// задан (логи хранятся вечно), обрезка не применяется. Тот же принцип, что
	// у SpanRetentionDays.
	LogRetentionDays int

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

	// AgentDistDir — каталог с install.sh-скриптом (встроен через //go:embed,
	// см. agentdist.go) и бинарями gotcha-agent (план A2, задача 10):
	// GOTCHA_DIST_DIR, раскладывается в образ сборкой Docker. Дефолт
	// (cmd/gotcha/config.go) совпадает с путём из Dockerfile — пусто здесь
	// бывает только в dev-режиме (go run без docker) или если оператор явно
	// указал каталог, которого физически нет; в обоих случаях GET /install.sh
	// и GET /agent/{file} отвечают 404 с подсказкой, не паникуют.
	AgentDistDir string
	// agentETags — ленивый sha256-ETag раздаваемых бинарей агента, посчитанный
	// один раз на имя файла (agentdist.go): файлы в образе неизменяемы, поэтому
	// пересчитывать хэш на каждый запрос незачем. Поле per-Handler (не
	// package-level), чтобы разные инстансы Handler (например, в тестах на
	// разных временных каталогах) не делили один кеш. Нулевое значение готово
	// к работе.
	agentETags sync.Map

	// ssoProviders — процесс-локальный кеш per-org OIDC-провайдеров (этап 10,
	// см. sso.go). Нулевое значение готово к работе.
	ssoProviders ssoCache

	loginLimiter *rateLimiter
	// emailLimiter — глобальный per-EMAIL лимитер входа (без IP): per-account
	// (ip|email) и per-IP лимиты не сдерживают распределённый перебор ОДНОГО
	// аккаунта с пула IP (каждый IP получает свежий бакет). Этот кап (щедрый,
	// 50/15мин) ограничивает суммарный поток попыток на email со всех адресов,
	// не задевая легитимного пользователя; порог намеренно высокий, чтобы
	// сдержать брутфорс, но не дать тривиального account-lockout DoS.
	emailLimiter *rateLimiter
	// ipLimiter — глобальный per-IP лимитер входа/регистрации (SEC-L2): в
	// дополнение к per-account (ip|email) ограничивает суммарный поток попыток с
	// одного IP по РАЗНЫМ email, закрывая обход per-account лимита перебором.
	ipLimiter *rateLimiter
	// publicLimiter — per-IP лимитер НЕаутентифицированных машинных и публичных
	// роутов: /uptime/hb/{token}, /probe/*, /status/{key}, /auth/oauth/*/start.
	// Каждый такой запрос от анонима стоит похода в PostgreSQL (резолв токена
	// пробы/heartbeat-токена/слага), а пул общий с веб-частью, поэтому без капа
	// аноним без единого ключа выбирает пул и роняет UI, алерты и квоты.
	// Порог щедрый (600/мин ≈ 10/с на IP): один хост с десятками heartbeat-
	// мониторов и пробой должен укладываться с запасом, а перебор — нет.
	publicLimiter *rateLimiter
	// agentLimiter — отдельный узкий per-IP лимитер ТОЛЬКО для раздачи бинарей
	// агента (GET /agent/{file}, agentdist.go): бинарь ~9.3 МиБ, а общий
	// publicLimiter (600/мин/IP) с этим весом даёт DoS-профиль — до ~9000
	// одновременных соединений с одного IP по 15-минутному дедлайну записи
	// (см. Handler.agentFile). install.sh мал и остаётся под publicLimiter.
	// Дефолт New() (10/мин) — только для стендов/тестов без конфига; в
	// проде main.go сразу перекрывает его через SetAgentDistRateLimit
	// (GOTCHA_DIST_RATE_PER_MIN, ops-H4) — щедрее, чтобы одна установка
	// (2 запроса: бинарь+SHA) не сериализовала раскатку парка за одним IP.
	agentLimiter *rateLimiter
	// statusPageLimiter — per-USER лимитер создания статус-страниц (security
	// P2-2): slug глобально уникален на инстанс, а создание доступно любому
	// оператору любой организации, поэтому перебором slug'ов (успех vs 422
	// «занято») можно узнать, что slug кем-то занят на инстансе. Полное
	// устранение — скоуп-миграция UNIQUE (project_id, slug), ломающая контракт
	// публичных URL, отложена. Здесь — дешёвая мера: лимит на попытки создания
	// (12/мин на пользователя) делает перебор дорогим, не мешая легитимному
	// оператору (страницы создают штучно). Ключ — uid: создатель всегда
	// аутентифицирован.
	statusPageLimiter *rateLimiter
	// exportLimiter — per-«uid|projectID» лимитер частоты постановки заявок
	// на выгрузку (E1, спека §8): лимит активных заявок (maxActivePerUser/
	// maxActivePerProject, exports.go) не ловит того, кто ставит заявку и
	// тут же удаляет её — от этого защищает именно ограничение частоты
	// тяжёлой выборки по ClickHouse.
	exportLimiter *rateLimiter
	// statusCache — 30-секундный кеш публичных статус-страниц по slug'у
	// (см. statuspage.go). Нулевое значение готово к работе, поэтому поле не
	// требует инициализации в New.
	statusCache statusCache

	// crossOriginRejected/coThrottle — счётчик и троттлинг лога отказов
	// same-origin (см. crossorigin.go). Нулевые значения готовы к работе.
	crossOriginRejected atomic.Int64
	coThrottle          coThrottle
}

// localRegion возвращает h.LocalRegion, а если оно не задано —
// uptime.DefaultRegion (см. комментарий к полю).
func (h *Handler) localRegion() string {
	if h.LocalRegion == "" {
		return uptime.DefaultRegion
	}
	return h.LocalRegion
}

// Потолки числа ключей (rl.maxKeys) для лимитеров ниже — находка W2-B: один
// общий потолок на все лимитеры пакета неверен, у каждого своя ожидаемая
// кардинальность и модель ключа (per-IP vs per-account vs per-аутентифицированный
// пользователь). Числа — не тесный бюджет памяти (лишний порядок здесь
// тривиален: maxKeys*limit временных меток по 24 байта — единицы МиБ даже на
// 20000), а верхняя граница РАЗНЫХ ключей, которую лимитер готов принять,
// прежде чем начать отказывать невиданным ранее ключам (см. ratelimit.go,
// Allow).
const (
	// loginLimiterMaxKeys — ключ ip|email (5/мин). После находки W2-B в
	// auth.go per-IP ipLimiter проверяется ПЕРВЫМ операндом || — один IP не
	// заводит в loginLimiter больше ipLimiter'ного лимита (20) ключей в
	// минуту, так что заполнение карты требует ботнета, а не одной машины.
	// Потолок здесь — backstop поверх этого: щедрый запас на тысячи
	// одновременно атакующих IP.
	loginLimiterMaxKeys = 20000
	// ipLimiterMaxKeys — ключ clientIP (20/мин): тот же щедрый запас, что и
	// у loginLimiterMaxKeys — это и есть тот самый дешёвый по кардинальности
	// лимитер, что теперь в auth.go сдерживает рост карты loginLimiter.
	ipLimiterMaxKeys = 20000
	// emailLimiterMaxKeys — ключ email без IP (50/15мин): окно длиннее, но
	// пространство ключей (реальные адреса) естественно меньше пространства
	// IP-адресов; тот же порядок запаса, не тесный бюджет.
	emailLimiterMaxKeys = 20000
	// publicLimiterMaxKeys — ключ clientIP на самой широкой по охвату группе
	// роутов (heartbeat/пробы/публичные статус-страницы, см. поле
	// Handler.publicLimiter) — тот же запас, что и у остальных per-IP
	// лимитеров этого блока.
	publicLimiterMaxKeys = 20000
	// agentLimiterMaxKeys — ключ clientIP, но узкий сценарий (раздача
	// бинаря агента, ~9.3 МиБ за запрос, см. поле Handler.agentLimiter):
	// установок агента на порядки меньше общего публичного трафика, потолок
	// ниже.
	agentLimiterMaxKeys = 5000
	// statusPageLimiterMaxKeys / exportLimiterMaxKeys — ключ аутентифицирован
	// (uid или uid|projectID): кардинальность ограничена числом
	// пользователей/проектов self-hosted инстанса, а не открытым интернетом,
	// потолок соразмерно ниже, чем у анонимных per-IP лимитеров выше.
	statusPageLimiterMaxKeys = 5000
	exportLimiterMaxKeys     = 5000
)

// New собирает Handler. BaseURL используется для sameOrigin-проверки POST-ов
// и для выставления Secure-флага сессионной cookie.
//
// RegistrationMode по умолчанию "open" — это исторический контракт конструктора
// (регистрация открыта). Продовая безопасность PROD-B1 живёт на уровне конфига:
// main.go всегда проставляет webHandler.RegistrationMode = cfg.RegistrationMode
// (GOTCHA_REGISTRATION_MODE, дефолт "invite"), переопределяя это значение.
//
// HSTSHeader по умолчанию "max-age=31536000" — исторический контракт
// конструктора (ровно тот заголовок, который securityHeaders слал до
// появления настройки). main.go всегда проставляет
// webHandler.HSTSHeader = web.HSTSHeaderValue(...) из GOTCHA_HSTS_*.
func New(authSvc *auth.Service, orgSvc *org.Service, issueSvc *issue.Service, events *event.Query, baseURL string) *Handler {
	return &Handler{
		Auth:              authSvc,
		Org:               orgSvc,
		Issues:            issueSvc,
		Events:            events,
		BaseURL:           baseURL,
		Secure:            strings.HasPrefix(baseURL, "https://"),
		HSTSHeader:        "max-age=31536000",
		RegistrationMode:  "open",
		loginLimiter:      newRateLimiter(time.Now, 5, time.Minute, loginLimiterMaxKeys, "loginLimiter"),
		ipLimiter:         newRateLimiter(time.Now, 20, time.Minute, ipLimiterMaxKeys, "ipLimiter"),
		emailLimiter:      newRateLimiter(time.Now, 50, 15*time.Minute, emailLimiterMaxKeys, "emailLimiter"),
		publicLimiter:     newRateLimiter(time.Now, 600, time.Minute, publicLimiterMaxKeys, "publicLimiter"),
		agentLimiter:      newRateLimiter(time.Now, 10, time.Minute, agentLimiterMaxKeys, "agentLimiter"),
		statusPageLimiter: newRateLimiter(time.Now, 12, time.Minute, statusPageLimiterMaxKeys, "statusPageLimiter"),
		exportLimiter:     newRateLimiter(time.Now, createRateLimit, createRateWindow, exportLimiterMaxKeys, "exportLimiter"),
		attrKeysCache:     newAttrKeysCache(),
	}
}

// recordingMux — ServeMux, запоминающий зарегистрированные шаблоны.
//
// Нужен сторожу маршрутов на Origin (находка №18): стандартный ServeMux свои
// шаблоны наружу не отдаёт, а список, который вели руками в cover_cheap_test.go,
// отставал от регистрации — ровно это и оставило восемь мутирующих маршрутов
// без проверки Origin незамеченными. Перехватывает и HandleFunc, и Handle:
// маршруты за requireUser регистрируются вторым, и без перехвата именно они —
// самые интересные для CSRF — выпали бы из перебора.
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

// Register навешивает маршруты задачи 4 на mux. Все они собираются на
// внутреннем ServeMux и монтируются на переданный mux одним catch-all "/" —
// это даёт единую точку для securityHeaders (весь ответ Handler'а несёт
// базовые security-заголовки) и для стилизованной 404-страницы на
// незарегистрированных путях (иначе достался бы голый "404 page not found"
// от stdlib ServeMux).
func (h *Handler) Register(mux *http.ServeMux) {
	inner := &recordingMux{ServeMux: http.NewServeMux()}

	inner.HandleFunc("GET /login", h.loginPage)
	inner.HandleFunc("POST /login", h.loginSubmit)
	inner.HandleFunc("GET /register", h.registerPage)
	inner.HandleFunc("POST /register", h.registerSubmit)
	inner.HandleFunc("POST /logout", h.logout)
	// Enterprise-SSO (этап 10): identifier-first вход по email-домену.
	inner.HandleFunc("GET /sso", h.ssoPage)
	inner.HandleFunc("POST /sso", h.ssoSubmit)

	// GET /invite/{token} — публичный: аноним по ссылке-приглашению должен
	// увидеть, куда его зовут, и получить пути входа/регистрации, не теряя
	// токен. Под requireUser он раньше молча улетал на /login, теряя смысл
	// страницы для того самого адресата, у которого ещё нет аккаунта. Само
	// чтение приглашения (InviteByToken) токен не гасит — accepted_at
	// выставляет только POST ниже, за requireUser.
	inner.HandleFunc("GET /invite/{token}", h.inviteAcceptPage)

	// OAuth/social login (этап 5): открыты для анонимов (вход), сессию для
	// потока привязки проверяем внутри хендлера.
	inner.HandleFunc("GET /auth/oauth/{provider}/start", h.publicRateLimited(h.oauthStart))
	inner.HandleFunc("GET /auth/oauth/{provider}/callback", h.oauthCallback)

	// Переключатель языка (задача 6): доступен и анониму — например, на
	// странице логина, до создания сессии. Не под requireUser — limitFormBody
	// (K7-4) навешан здесь явно, а не только внутри requireUser.
	inner.Handle("POST /settings/locale", h.limitFormBody(http.HandlerFunc(h.localeSwitch)))

	// Переключатель темы оформления: доступен и анониму (см. локаль выше).
	inner.Handle("POST /settings/theme", h.limitFormBody(http.HandlerFunc(h.themeSwitch)))

	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic("web: embedded static assets missing: " + err.Error())
	}
	// Версия статики (хэш содержимого встроенных ассетов) — шаблоны ссылаются
	// на app.css/js как /static/<файл>?v=<хэш>, чтобы после деплоя браузеры не
	// отдавали старый закэшированный CSS (иначе новый daterange.js уже подтянут,
	// а правил скрытия в CSS ещё нет — контрол разъезжается).
	assetVer := staticAssetVersion(staticSub)
	templates.SetAssetVersion(assetVer)
	fileServer := http.FileServer(http.FS(staticSub))
	// Предсжатая статика: содержимое встроено и неизменно, поэтому жмём один раз
	// при старте (см. buildGzipAssets) — на запросе остаётся отдать готовые байты.
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
	// Удаление организации и удаление ПДн субъекта (PRIV-H2) — owner-only.
	inner.Handle("POST /orgs/{id}/settings/delete", h.requireUser(http.HandlerFunc(h.orgSettingsDelete)))
	inner.Handle("POST /orgs/{id}/settings/purge-subject", h.requireUser(http.HandlerFunc(h.orgSettingsPurgeSubject)))
	// Выгрузка ПДн субъекта (право на доступ, 152-ФЗ ст. 14, RA-L11) — owner-only.
	inner.Handle("POST /orgs/{id}/settings/export-subject", h.requireUser(http.HandlerFunc(h.orgSettingsExportSubject)))
	// POST (принятие) остаётся за requireUser — принять приглашение может
	// только вошедший.
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
	// Литерал "delete" специфичнее {ruleID} — ServeMux разводит их независимо
	// от порядка регистрации (прецедент hosts/settings ниже).
	inner.Handle("POST /projects/{id}/metrics/alerts/{ruleID}", h.requireUser(http.HandlerFunc(h.metricAlertUpdate)))
	inner.Handle("GET /projects/{id}/metrics/{name}", h.requireUser(http.HandlerFunc(h.metricDetail)))

	// Рецепты мониторинга (B6): страницы подключения типовых сервисов
	// (postgres/nginx/redis/docker). Просмотр — любой с доступом к проекту
	// (как /metrics); создание рекомендованных порогов — оператор проекта
	// (см. recipes.go, тот же гейт, что у мутаций metric alerts).
	inner.Handle("GET /projects/{id}/recipes", h.requireUser(http.HandlerFunc(h.recipesListPage)))
	inner.Handle("GET /projects/{id}/recipes/{slug}", h.requireUser(http.HandlerFunc(h.recipeDetailPage)))
	inner.Handle("POST /projects/{id}/recipes/{slug}/thresholds", h.requireUser(http.HandlerFunc(h.recipeThresholdsCreate)))

	// SLO (план D1): список определений + форма создания + удаление. Гейт всех
	// трёх — оператор проекта (см. slos.go, зеркало metric-alerts).
	inner.Handle("GET /projects/{id}/slos", h.requireUser(http.HandlerFunc(h.slosPage)))
	inner.Handle("GET /projects/{id}/slos/{sloID}", h.requireUser(http.HandlerFunc(h.sloDetail)))
	inner.Handle("POST /projects/{id}/slos", h.requireUser(http.HandlerFunc(h.sloCreate)))
	inner.Handle("POST /projects/{id}/slos/{sloID}/delete", h.requireUser(http.HandlerFunc(h.sloDelete)))

	// Хосты (план A1, задача 14): литерал "settings" перед {name} — ServeMux
	// (Go 1.22) отдаёт приоритет более специфичному сегменту независимо от
	// порядка регистрации, поэтому хост с именем "settings" по карточке
	// недоступен (тот же прецедент, что у /metrics/{name} с метрикой
	// "alerts"). detail/settings/delete в этой задаче — заглушки; полная
	// реализация — Task 15 (карточка/удаление) и Task 16 (форма порогов).
	inner.Handle("GET /projects/{id}/hosts", h.requireUser(http.HandlerFunc(h.hostsList)))
	inner.Handle("GET /projects/{id}/hosts/settings", h.requireUser(http.HandlerFunc(h.hostSettingsPage)))
	inner.Handle("POST /projects/{id}/hosts/settings", h.requireUser(http.HandlerFunc(h.hostSettingsSave)))
	// Групповые пороги по окружению/роли (B2, T7): литеральные сегменты
	// "settings"/"groups"/"delete" — тем же приоритетом ServeMux, что у
	// "settings" перед {name} в комментарии выше.
	inner.Handle("POST /projects/{id}/hosts/settings/groups", h.requireUser(http.HandlerFunc(h.hostGroupThresholdSave)))
	inner.Handle("POST /projects/{id}/hosts/settings/groups/delete", h.requireUser(http.HandlerFunc(h.hostGroupThresholdDelete)))
	inner.Handle("GET /projects/{id}/hosts/{name}", h.requireUser(http.HandlerFunc(h.hostDetail)))
	inner.Handle("POST /projects/{id}/hosts/{name}/thresholds", h.requireUser(http.HandlerFunc(h.hostThresholdsSave)))
	inner.Handle("POST /projects/{id}/hosts/{name}/delete", h.requireUser(http.HandlerFunc(h.hostDelete)))

	// Логи (C2, задача 2): базовый просмотрщик — фильтры/список/раскрытие/
	// курсорная пагинация. Гистограмма/фасеты — задачи T3-T5.
	inner.Handle("GET /projects/{id}/logs", h.requireUser(http.HandlerFunc(h.logsList)))
	// Автокомплит ключей атрибутов (задача 6, C2, §6 спеки): отдельный
	// GET-роут ПЕРЕД /logs/{...} нет конфликтов, так как под /logs других
	// сегментов не зарегистрировано — ServeMux (Go 1.22) сам разберёт более
	// специфичный литеральный сегмент "attr-keys" впереди возможных будущих
	// {name}-шаблонов.
	inner.Handle("GET /projects/{id}/logs/attr-keys", h.requireUser(http.HandlerFunc(h.logsAttrKeys)))

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
	inner.Handle("POST /projects/{id}/alerts/channels/update", h.requireUser(http.HandlerFunc(h.alertsChannelUpdate)))
	inner.Handle("POST /projects/{id}/alerts/channels/delete", h.requireUser(http.HandlerFunc(h.alertsChannelDelete)))
	inner.Handle("POST /projects/{id}/alerts/channels/test", h.requireUser(http.HandlerFunc(h.alertsChannelTest)))

	// Эскалации (B4, задача 9): редактор лесенок critical/warning + dry-run-
	// предпросмотр. Доступ — оператор проекта (requireProjectOperator), как
	// alerts/slos/metric-alerts выше.
	inner.Handle("GET /projects/{id}/escalations", h.requireUser(http.HandlerFunc(h.escalationsPage)))
	inner.Handle("POST /projects/{id}/escalations", h.requireUser(http.HandlerFunc(h.escalationsSave)))

	// Подавление шторма (B5, задача 9): редактор рёбер зависимостей между
	// узлами проекта. Доступ — оператор проекта (requireProjectOperator), как
	// у escalations выше.
	inner.Handle("GET /projects/{id}/alert-suppression", h.requireUser(http.HandlerFunc(h.alertSuppressionPage)))
	inner.Handle("POST /projects/{id}/alert-suppression", h.requireUser(http.HandlerFunc(h.alertSuppressionSave)))
	inner.Handle("POST /projects/{id}/alert-suppression/{depID}", h.requireUser(http.HandlerFunc(h.alertSuppressionUpdate)))
	inner.Handle("POST /projects/{id}/alert-suppression/{depID}/delete", h.requireUser(http.HandlerFunc(h.alertSuppressionDelete)))

	// Выгрузки ошибок/событий (E1, задачи 10/11): список заявок + постановка,
	// скачивание готового файла, удаление терминальной заявки — тот же
	// оператор-гейт, что у alert-suppression выше, с дополнительной проверкой
	// авторства/CanManage внутри download/delete/списка (exports.go).
	inner.Handle("GET /projects/{id}/exports", h.requireUser(http.HandlerFunc(h.exportsPage)))
	inner.Handle("POST /projects/{id}/exports", h.requireUser(http.HandlerFunc(h.exportsCreate)))
	inner.Handle("GET /projects/{id}/exports/{jobID}/download", h.requireUser(http.HandlerFunc(h.exportsDownload)))
	inner.Handle("POST /projects/{id}/exports/{jobID}/delete", h.requireUser(http.HandlerFunc(h.exportsDelete)))

	// Ack инцидентов (B4, задача 10): один эндпоинт на все 5 источников
	// (host/metric/trace/profile/slo), диспатч по {source} — см.
	// incidents_ack.go. Доступ — оператор проекта, как у escalations выше.
	inner.Handle("POST /projects/{id}/incidents/{source}/{incident_id}/ack", h.requireUser(http.HandlerFunc(h.incidentAck)))

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
	inner.Handle("POST /monitors/{id}/heartbeat/regenerate", h.requireUser(http.HandlerFunc(h.monitorHeartbeatRegenerate)))

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
	inner.Handle("GET /projects/{id}/overview", h.requireUser(http.HandlerFunc(h.overview)))
	inner.Handle("GET /projects/{id}/incident-feed", h.requireUser(http.HandlerFunc(h.incidentFeedRedirect)))

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

	// Карта зависимостей (C4): таблица внешних вызовов (БД/кеш/HTTP) сервиса,
	// агрегированная из client-op спанов — доступ по CanAccessProject → 404, как
	// у performanceList.
	inner.Handle("GET /projects/{id}/dependencies", h.requireUser(http.HandlerFunc(h.dependencies)))

	// Web Vitals (этап 4, план 2, задача 2): обзорная страница страниц проекта с
	// p75 LCP/INP/CLS — только чтение, доступ по CanAccessProject → 404, как
	// performanceList. Панель Web Vitals на странице эндпойнта отдельного роута
	// не имеет (рендерится в endpointDetail).
	inner.Handle("GET /projects/{id}/web-vitals", h.requireUser(http.HandlerFunc(h.webVitalsList)))

	// Perf-проблемы (этап 3, план 5, задача 1): список проблем проекта и страница
	// проблемы — просмотр открыт любому участнику проекта (CanAccessProject → 404,
	// как performanceList), смена статуса — та же граница CanAccessProject
	// + sameOrigin (спека 2026-08-08: выровнено с issueSetStatus, доступ, не роль).
	// Страница проблемы несёт в пути только {id}, проект резолвится из самой
	// проблемы (PerfIssues.ProjectOf). Роуты
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

	// Список деплоев проекта (C5): версия/окружение/когда/изменения/ссылка на
	// прогон CI. Доступ — CanAccessProject; nil-guard на h.Deploy отвечает 404
	// в стендах без приёма деплоев.
	inner.Handle("GET /projects/{id}/deployments", h.requireUser(http.HandlerFunc(h.deployments)))

	// Waterfall трейса (этап 3, план 4, задача 3): доступ — по проекту трейса
	// (ProjectForTrace → CanAccessProject → 404), не по {id} в пути. Только
	// чтение, POST'ов и sameOrigin здесь нет. Как и /performance*,
	// регистрируется безусловно — h.Trace собран в режимах "web"/"all".
	inner.Handle("GET /traces/{trace_id}", h.requireUser(http.HandlerFunc(h.traceWaterfall)))
	inner.Handle("GET /traces/{trace_id}/flame", h.requireUser(http.HandlerFunc(h.traceFlame)))

	// Настройки статус-страниц проекта (план 5, задача 4): оператор проекта
	// (requireProjectOperator), как окна обслуживания. У
	// /statuspages/{id} проект берётся из самой страницы (loadManagedStatusPage),
	// чужая страница по её id — 404.
	inner.Handle("GET /projects/{id}/statuspages", h.requireUser(http.HandlerFunc(h.statusPagesPage)))
	inner.Handle("POST /projects/{id}/statuspages", h.requireUser(http.HandlerFunc(h.statusPagesCreate)))
	inner.Handle("POST /statuspages/{id}", h.requireUser(http.HandlerFunc(h.statusPagesUpdate)))
	inner.Handle("POST /statuspages/{id}/delete", h.requireUser(http.HandlerFunc(h.statusPagesDelete)))

	inner.Handle("GET /projects/{id}/maintenance", h.requireUser(http.HandlerFunc(h.maintenancePage)))
	inner.Handle("POST /projects/{id}/maintenance", h.requireUser(http.HandlerFunc(h.maintenanceCreate)))
	inner.Handle("POST /projects/{id}/maintenance/update", h.requireUser(http.HandlerFunc(h.maintenanceUpdate)))
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
		inner.HandleFunc("GET /uptime/hb/{token}", h.publicRateLimited(h.heartbeat))
		inner.HandleFunc("POST /uptime/hb/{token}", h.publicRateLimited(h.heartbeat))

		inner.HandleFunc("POST /probe/lease", h.publicRateLimited(h.probeLease))
		inner.HandleFunc("POST /probe/results", h.publicRateLimited(h.probeResults))

		inner.HandleFunc("GET /status/{key}", h.publicRateLimited(h.statusPage))
	}

	// Раздача install.sh и бинарей агента (план A2, задача 10): публичный
	// машинный доступ без сессии (curl | sh на голом сервере, где логиниться
	// ещё некому) и без sameOrigin — тот же принцип, что у heartbeat/probe/
	// status выше. Регистрируются безусловно (не под h.Uptime != nil), потому
	// что AgentDistDir не зависит от режима uptime — свой nil-guard на пустой/
	// отсутствующий каталог даёт agentdist.go.
	inner.HandleFunc("GET /install.sh", h.publicRateLimited(h.installSh))
	// /agent/{file} — под своим agentLimiter, не publicLimiter (см. поле
	// Handler.agentLimiter): раздача бинарей тяжелее по трафику и времени
	// соединения, чем остальные публичные роуты, и должна резаться отдельно.
	inner.HandleFunc("GET /agent/{file}", h.agentDistRateLimited(h.agentFile))

	// Fallback: любой путь, не покрытый паттернами выше, — стилизованная 404.
	inner.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.renderError(w, r, http.StatusNotFound, i18n.T(r.Context(), "error.not_found"))
	})

	// Роутер страниц запоминается: снаружи виден только catch-all «/», и по
	// коду ответа нельзя отличить «обработчик отверг битый id» от «маршрута
	// нет». Проверки регистрации спрашивают RoutePattern. Поле pages остаётся
	// *http.ServeMux (не recordingMux) — RoutePattern работает через
	// h.pages.Handler(req) и не должен меняться из-за обёртки.
	h.pages = inner.ServeMux
	h.routes = inner.patterns
	mux.Handle("/", h.securityHeaders(h.withLocale(h.withTheme(h.withFlash(h.withShell(inner))))))
}

// staticAssetVersion возвращает короткий хэш содержимого встроенных статических
// ассетов. Шаблоны подставляют его в URL как ?v=<хэш>, поэтому любое изменение
// CSS/JS меняет ссылку — браузеры не отдают старую версию из кэша после деплоя.
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

// cacheControl проставляет Cache-Control на статику. Неизменяемый кэш ставится
// только когда ?v совпадает с ТЕКУЩИМ хэшем содержимого (version): хэш меняется
// при любом изменении ассета, поэтому под текущей версией контент вечен. Чужой/
// устаревший ?v (или его отсутствие) — короткий кэш: так клиент не «прибивает»
// на год произвольное значение под неизменяемым URL (churn кэша прокси/CDN).
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

// noDirListing отдаёт 404 на запрос каталога (пустой путь после StripPrefix или
// путь, заканчивающийся "/"): иначе http.FileServer печатает листинг каталога
// (/static/, /static/icons/) с именами файлов. Раскрытие имён — мелочь, но не
// нужно; файлы по прямому пути отдаются как раньше.
func noDirListing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cspHeader — все страницы Gotcha загружают только собственные ресурсы:
// app.css и daterange.js отдаются с того же origin (см. layout.templ), и ни
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
// Значение заголовка берётся из HSTSHeader (собрано на старте из
// GOTCHA_HSTS_*), а не зашито; HSTS остаётся здесь и НЕ переезжает в
// baseSecurityHeaders (cmd/gotcha/server.go) — тот ставит только то, что
// верно для любого ответа, а HSTS зависит от TLS-режима инстанса, про
// который знает лишь веб-хендлер; в режиме ingest веб-хендлера нет вовсе.
func (h *Handler) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("Referrer-Policy", "same-origin")
		hdr.Set("Content-Security-Policy", cspHeader)
		// no-store по умолчанию: аутентифицированные SSR-страницы несут ПДн
		// (email участников, ключи, телеметрия) — они не должны оседать в
		// дисковом кэше браузера/bfcache и показываться Назад после логаута, ни
		// кэшироваться прокси. Статика ставит свой Cache-Control ниже по цепочке
		// (cacheControl для /static) и это значение перекрывает.
		hdr.Set("Cache-Control", "no-store")
		// Проверка h.Secure сохраняется рядом с настройкой: на http-инстансе
		// заголовка нет ни при каких значениях GOTCHA_HSTS_* — защита в
		// глубину, не зависящая от того, что подставил main.go.
		if h.Secure && h.HSTSHeader != "" {
			hdr.Set("Strict-Transport-Security", h.HSTSHeader)
		}
		next.ServeHTTP(w, r)
	})
}

// RoutePattern возвращает шаблон, по которому роутер страниц выберет
// обработчик для метода и пути; пустая строка означает, что своей регистрации у
// пути нет.
//
// Существует ради проверок маршрутизации: 404 одинаково возвращают и
// обработчик, отвергнувший битый идентификатор, и отсутствующий маршрут — а
// тест, не различающий эти случаи, остаётся зелёным после удаления регистрации.
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

// RegisteredRoutes возвращает шаблоны всех маршрутов, зарегистрированных в
// Register (метод и путь в одной строке, как в http.ServeMux, например
// "POST /teams/{id}/rename") — в порядке регистрации, дубликаты возможны, если
// Register вызвать дважды на одном Handler.
//
// Существует ради сторожа на Origin (находка №18): перебор ВСЕХ мутирующих
// маршрутов вместо литерального списка, который отстаёт от регистрации.
func (h *Handler) RegisteredRoutes() []string {
	return h.routes
}

// renderError отдаёт стилизованную страницу ошибки (layout + сообщение) с
// заданным HTTP-статусом — замена голому http.Error, которое ломает вид
// сайта на ошибках.
func (h *Handler) renderError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	// WriteHeader отправляет заголовки до первой записи тела, поэтому
	// автоопределение Content-Type не срабатывает — без явной установки
	// страница ошибки уходит вовсе без Content-Type.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
	h.renderConfirmf(w, r, titleKey, messageKey, confirmLabelKey, cancelHref, action, hidden)
}

// renderConfirmf — renderConfirm с подстановками в текст сообщения (пары
// ключ-значение для i18n.Tf).
//
// Нужен там, где вопрос без деталей бессмыслен. Пример — удаление данных
// субъекта: страница спрашивала «Удалить данные субъекта из телеметрии
// проекта?», не показывая НИ проекта, НИ критериев, потому что те лежат в
// hidden-полях. Защищать она была должна ровно от опечатки в номере проекта
// (25 вместо 26), но заметить эту опечатку на экране было не по чему —
// подтверждение существовало, а страховки не давало.
func (h *Handler) renderConfirmf(w http.ResponseWriter, r *http.Request, titleKey, messageKey, confirmLabelKey, cancelHref, action string, hidden []templates.HiddenField, kv ...string) {
	title := i18n.T(r.Context(), titleKey)
	message := i18n.Tf(r.Context(), messageKey, kv...)
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
			// №21: владельцу/админу любой из его организаций «попросите
			// администратора» — тупик: он сам администратор. Ему — дверь в
			// создание проекта (/projects, модалка там).
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
	// Кука голая "/" — единственное место, где она вправе решать (§5 спеки
	// nav-ia): запомненный проект (если юзер всё ещё в списке своих
	// проектов) ведёт прямиком на его «Обзор». Без запомненного проекта —
	// НЕ первый проект списка (это подменяло бы явный выбор организации
	// молчаливым решением за юзера), а список проектов первой по порядку
	// организации (та же дверь, что и у /projects, projectsRedirect в
	// orgprojects.go) — юзер выбирает проект сам.
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
		// Не должно случаться, раз projects непуст (проект без организации
		// не существует) — тот же тупиковый выход, что и у orgs==0 выше,
		// если инвариант всё же нарушится.
		http.Redirect(w, r, "/onboarding", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, orgProjectsPath(orgs[0].ID), http.StatusSeeOther)
}

// projectIssuesPath — путь до issue-листинга проекта: цель ссылок «Первые
// шаги»/ошибок онбординга (issues.go), больше не цель голого index() —
// задача 6 nav-ia увела вход в приложение на overviewPath.
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

// currentEmailPublic — email текущего пользователя на маршрутах БЕЗ
// requireUser (GET /invite/{token}: страница обязана быть видна и анониму,
// поэтому requireUser её не оборачивает и auth.UserID в контекст не кладёт —
// currentEmail на такой странице всегда вернула бы "", даже вошедшему).
//
// Ищет сессию напрямую, тем же приёмом, что withShell/localeSwitch/themeSwitch
// для best-effort резолвинга анонимных маршрутов: любая ошибка или отсутствие
// cookie — "" (аноним), запрос не падает.
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

// formBodyMaxBytes — общий предел тела POST для обычных html-форм продукта
// (настройки, правила, пороги, метки): раньше (K7-4) на ~50 форм-хендлерах
// предел не был поставлен явно и держался на неявных 10 МиБ
// ParseForm/ParseMultipartForm стандартной библиотеки — решение о размере
// нигде не было записано. Формы этого продукта — это поля настроек и
// правил, а не файлы; 64 КиБ — щедрый запас над самой тяжёлой легитимной
// формой (SSO-конфиг, правило алерта с несколькими каналами, http_body
// синтетического монитора) и на три порядка меньше implicit-дефолта stdlib.
//
// Компонуется с более строгими частными пределами (auth 8 КиБ,
// heartbeatMaxBodyBytes 1 КБ, probeMaxBodyBytes 1 МиБ): вложенные
// http.MaxBytesReader считают одни и те же байты независимо, поэтому
// срабатывает наименьший из пределов, а не последний применённый — общий
// предел не может затереть более строгий частный.
const formBodyMaxBytes = 64 << 10 // 64 KiB

// limitFormBody ограничивает тело запроса до formBodyMaxBytes прежде, чем
// next дойдёт до r.ParseForm(). Единая точка применения (K7-4) — здесь, а не
// копипастой в каждом из ~50 обработчиков: requireUser ниже оборачивает им
// все аутентифицированные POST-формы, а два публичных form-POST без сессии
// (settings/locale, settings/theme) оборачиваются им отдельно в Register.
func (h *Handler) limitFormBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, formBodyMaxBytes)
		next.ServeHTTP(w, r)
	})
}

// parseForm разбирает тело формы и переводит ошибку ParseForm в ответ:
// превышение предела тела (общего выше или частного — auth/heartbeat/probe
// ставят свой явно сами) отвечает 413, а не общим 400, иначе клиент не
// отличит сломанную форму от тела сверх лимита. Возвращает true, если разбор
// успешен и обработчик может продолжать.
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

// requireUser оборачивает auth.Service.RequireUser: для htmx-запросов
// (HX-Request: true) вместо 303-редиректа на /login отдаёт 200 с заголовком
// HX-Redirect — htmx сам выполнит переход, а не покажет частичный HTML.
// limitFormBody (K7-4) навешан здесь же: requireUser оборачивает все
// аутентифицированные POST-формы продукта, так что это единственное место,
// где нужно поставить общий предел тела разом на все ~50 форм-хендлеров.
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

// ProjectPurger — узкий интерфейс CH-очистки, которым владеет web-слой
// (telemetry.Purger ему удовлетворяет). Вынесен в web, чтобы тесты могли
// подменять его фейком и считать вызовы, не поднимая ClickHouse. Оба метода
// best-effort: PG-удаление первично, ошибка CH-очистки логируется, но
// пользовательскую операцию не роняет.
type ProjectPurger interface {
	PurgeProject(ctx context.Context, projectID int64) error
	// PurgeSubject возвращает, сколько строк было отнесено к субъекту: удаление
	// умеет завершаться «успешно», не тронув ничего (при включённом по умолчанию
	// скрубинге email/IP колонки зануляются на приёме, и поиск по ним не
	// совпадает ни с чем), а исполняющий требование по 152-ФЗ обязан видеть
	// разницу между «удалено N записей» и «не найдено ничего».
	PurgeSubject(ctx context.Context, projectID int64, sub telemetry.Subject) (telemetry.PurgeResult, error)
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
	// Членство есть, роли не хватает — честный 403 (№72): участник и так
	// знает, что организация существует, а «не найдено» на знакомой странице
	// читается как поломка. Не-члену выше отвечает 404 — существование чужой
	// организации не раскрывается.
	if role != org.RoleOwner && role != org.RoleAdmin {
		h.renderError(w, r, http.StatusForbidden, i18n.T(r.Context(), "error.403.body"))
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
