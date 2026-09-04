package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/baseurl"
	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/envcontract"
	"gitflic.ru/otezvikentiy/gotcha/internal/export"
	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
)

// exportFieldEnvNames переводит текст ошибки export.Config.Validate()
// (называет поля структуры — MaxRows, MaxBytes, DiskBudget, TTL) в имена
// переменных окружения, которыми эти поля наполняются в loadConfig ниже:
// оператору имя поля Go ни о чём не говорит, а имя переменной — то, что он
// правит в .env.
var exportFieldEnvNames = map[string]string{
	"MaxRows":    "GOTCHA_EXPORT_MAX_ROWS",
	"MaxBytes":   "GOTCHA_EXPORT_MAX_BYTES",
	"DiskBudget": "GOTCHA_EXPORT_DISK_BUDGET_BYTES",
	"TTL":        "GOTCHA_EXPORT_RETENTION_HOURS",
}

// exportFieldNameRe находит имена полей exportFieldEnvNames в тексте ошибки
// ТОЛЬКО целым словом (\b...\b), а не любой подстрокой — минорная находка
// задачи 11: internal/export/worker.go несёт собственный пакетный термин
// leaseTTL (внутренняя константа-таймаут переклейма заявки, 20 минут,
// никогда не читается из env — не поле export.Config вовсе), который
// заканчивается тем же токеном "TTL". Прежняя strings.NewReplacer билась по
// голой подстроке и увечила "...строго меньше leaseTTL (%s)" в
// "...строго меньше leaseGOTCHA_EXPORT_RETENTION_HOURS (%s)" — то же имя,
// но не то понятие, а сообщение оператору переставало читаться. \b слева от
// "TTL" внутри "leaseTTL" не находит границы слова (оба символа — часть
// одного идентификатора), поэтому целословный разбор оставляет его нетронутым.
var exportFieldNameRe = regexp.MustCompile(`\b(` + strings.Join(exportFieldNames(), "|") + `)\b`)

// exportFieldNames — ключи exportFieldEnvNames для регэкспа выше, в
// детерминированном порядке (карта иначе даёт недетерминированный порядок
// альтернатив — сам regexp/syntax от этого не ломается, но воспроизводимость
// скомпилированного паттерна между прогонами дороже одной сортировки).
func exportFieldNames() []string {
	names := make([]string, 0, len(exportFieldEnvNames))
	for name := range exportFieldEnvNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// translateExportEnvNames — обёртка над exportFieldNameRe/exportFieldEnvNames,
// вызываемая на err.Error() из export.Config.Validate() перед показом
// оператору.
func translateExportEnvNames(msg string) string {
	return exportFieldNameRe.ReplaceAllStringFunc(msg, func(field string) string {
		return exportFieldEnvNames[field]
	})
}

// devSecretKey — публично известный дефолт GOTCHA_SECRET_KEY для localhost-стендов.
// Вынесен в константу, т.к. по нему принимаются решения в нескольких местах
// (валидация запуска, отказ от бессмысленного at-rest-шифрования SSO — Info21).
const devSecretKey = "insecure-dev-secret"

// Config собирается из env (префикс GOTCHA_) и флагов командной строки.
type Config struct {
	Mode          string // ingest | web | uptime | probe | all
	Addr          string
	BaseURL       string
	PostgresDSN   string
	ClickHouseDSN string
	SMTPHost      string
	SMTPPort      int
	SMTPUser      string
	SMTPPassword  string
	SMTPFrom      string
	// TelegramAPIBase — базовый адрес Bot API (GOTCHA_TELEGRAM_API_BASE).
	// Пусто — https://api.telegram.org, дефолт живёт в пакете notify.
	//
	// Нужен там, где до api.telegram.org не достучаться: фильтрация трафика
	// у оператора связи, закрытый исход из периметра, собственный
	// telegram-bot-api рядом с инстансом. Альтернатива — пиннинг имени на
	// живой IP через extra_hosts — держится ровно до следующей смены адреса
	// или настроек фильтра, то есть чинит экземпляр, а не класс.
	TelegramAPIBase string
	// RetentionDays и родственные *RetentionDays ниже: 0 = хранить вечно
	// (№34, TTL в ClickHouse снимается). Исключение — OutboxRetentionDays,
	// у которой пол >= 1 сохранён: outbox — рабочая очередь, не архив.
	RetentionDays        int
	SpanRetentionDays    int
	MetricRetentionDays  int
	ProfileRetentionDays int
	// LogRetentionDays — срок хранения структурированных логов
	// (GOTCHA_LOG_RETENTION_DAYS). Логи объёмны, поэтому дефолт короче спанов.
	LogRetentionDays int
	// IncidentRetentionDays — срок хранения ЗАКРЫТЫХ инцидентов аптайма
	// (GOTCHA_INCIDENT_RETENTION_DAYS). Свой, а не общий с событиями: у
	// инцидента нет собственной телеметрии в ClickHouse, зато его показывает
	// публичная статус-страница, обещающая историю за девяносто дней.
	IncidentRetentionDays int
	// DeployRetentionDays — срок хранения маркеров выкладок
	// (GOTCHA_DEPLOY_RETENTION_DAYS). Свой, а не общий с событиями: история
	// выкладок — отдельная ось, не привязанная к телеметрии в ClickHouse, а
	// таблицу пишет публичный ключ приёма вне квоты, поэтому граница обязательна.
	DeployRetentionDays int
	// Edition — редакция сборки (oss | saas). Влияет на дефолты квот:
	// в oss все дефолты = 0 (безлимит), в saas = 1_000_000. См. loadConfig.
	Edition string
	// Default*Quota — дефолтные месячные квоты приёма при создании
	// организации (0 = безлимит). Читаются из GOTCHA_DEFAULT_*_QUOTA;
	// дефолт зависит от Edition.
	DefaultEventQuota       int64
	DefaultTransactionQuota int64
	DefaultMetricQuota      int64
	DefaultProfileQuota     int64
	DefaultLogQuota         int64
	MaxEventBytes           int64
	// MaxBufferBytes — байтовый потолок КАЖДОГО буфера писателя. Незаданная
	// переменная — 0 внутри Config — значение по умолчанию из пакета писателя
	// (256 МиБ) либо авто-вывод от потолка кучи (см. effectiveMaxBufferBytes,
	// autoMaxBufferBytes в main.go). Явный 0 или отрицательное значение —
	// ошибка конфигурации, отказ старта (loadConfig отличает «не задано» от
	// «задано нулём»), а не тихое приравнивание к дефолту. Нужен для
	// стеснённых профилей: писателей пять (event, SpanWriter, metric, profile,
	// log), но SpanWriter применяет ОДИН потолок к ДВУМ независимым буферам
	// (txBuf и spanBuf) — итого шесть «единиц потолка» по 256 МиБ, худший
	// случай 1.5 ГиБ (не 1.25, как было бы при пяти писателях по одному
	// буферу), тогда как docker-compose.small.yml ставит контейнеру mem_limit
	// 256m — там потолок физически не может сработать раньше OOM-killer'а.
	MaxBufferBytes int64
	// IngestRateLimit — per-DSN токен-бакет приёма, запросов/с на project id
	// (GOTCHA_INGEST_RATE_PER_SEC). Burst = 2×лимит. 0 выключает лимит.
	// Срабатывает после аутентификации ключа и ДО квоты; ответ 429.
	IngestRateLimit int
	// MaxQueueBytes — байтовый потолок очереди приёма (в дополнение к её
	// ёмкости в задачах). Незаданная переменная — 0 внутри Config — значение
	// по умолчанию (64 МиБ). Явный 0 или отрицательное значение — ошибка
	// конфигурации, отказ старта (см. MaxBufferBytes — тот же контракт).
	//
	// Счётный потолок сам по себе ничего не гарантировал: событие несёт до
	// четырёх сырых JSON-блоков по 256 КиБ, то есть до мегабайта на задачу, а
	// очередь держит тысячу задач. Гигабайт резидентной памяти на пути, куда
	// пишет кто угодно с публичным ключом приёма.
	MaxQueueBytes int64
	// AlertBudgetWindowSeconds/AlertBudgetLimit — пер-проектный потолок
	// уведомлений. Троттлинг alert_throttle ключуется парой (issue_id, rule_id),
	// и у НОВОГО issue строки там нет по определению — он проходит всегда, а
	// значит уникальный fingerprint на событие давал уведомление на событие.
	// 0 у лимита ВЫКЛЮЧАЕТ ограничение — осознанный выбор для инсталляции с
	// доверенными отправителями, а не аварийный режим.
	AlertBudgetWindowSeconds int
	AlertBudgetLimit         int
	// CardinalityLimit/CardinalityWindowSeconds — потолок РАЗЛИЧНЫХ значений
	// открытых полей (имя транзакции, окружение, имя метрики, сервис, операция)
	// на проект за окно. Эти значения стоят в ключах сортировки ClickHouse и в
	// GROUP BY материализованных представлений: идентификатор, случайно
	// попавший в имя, превращает десяток эндпойнтов в сотни тысяч строк
	// агрегатов, а ClickHouse общий на всех тенантов. 0 выключает ограничение.
	CardinalityLimit         int
	CardinalityWindowSeconds int
	// RunEvaluators — запускать ли оценщики регрессий производительности,
	// правил по метрикам и регрессий профилей. По умолчанию они идут вместе с
	// режимом uptime (исторически), хотя к аптайму отношения не имеют: при
	// раздельном развёртывании web+ingest — а оно документировано как
	// поддерживаемое — правило по метрике выглядело включённым и не
	// вычислялось НИКОГДА, без единой строки в логе.
	RunEvaluators       *bool
	MetricEvalInterval  int
	ProfileEvalInterval int
	HostEvalInterval    int
	SLOEvalInterval     int
	// EscalationInterval — период тика централизованного планировщика
	// эскалаций (B4, T8): как часто он проверяет, не настала ли задержка
	// очередной ступени лесенки для открытых неподтверждённых инцидентов
	// всех 5 источников.
	EscalationInterval int
	// DependencySettleSeconds — задержка первого уведомления узла с
	// задекларированным родителем (B5, T8): схлопывание гонки детекции, пока
	// не ясно, упал ли родитель следом. Должна быть >= самого медленного
	// порога тишины хостов-родителей проекта, иначе окно не успевает накрыть
	// реальную гонку (GOTCHA_DEPENDENCY_SETTLE_SECONDS).
	DependencySettleSeconds int
	OutboxRetentionDays     int
	// PurgeReconcileHours — период сверки телеметрии удалённых проектов
	// (GOTCHA_PROJECT_PURGE_RECONCILE_HOURS); 0 выключает сверку. Ноль здесь не
	// ошибка, а выключатель: установка, где в ClickHouse пишет что-то помимо
	// продукта, обязана иметь возможность отключить сверку, не отключая
	// очередь удаления.
	PurgeReconcileHours int
	// NotifyConcurrency — сколько уведомлений доставляется одновременно
	// (GOTCHA_NOTIFY_CONCURRENCY). Последовательная доставка означала, что один
	// мёртвый вебхук с 30-секундным таймаутом задерживает все остальные.
	NotifyConcurrency int
	SecretKey         string
	// SecretKeyPrev — предыдущий мастер-ключ на время ротации
	// (GOTCHA_SECRET_KEY_PREV). Пусто — ротация не идёт, кольцо из одного
	// ключа. Валидация — ниже, рядом с проверками SecretKey.
	SecretKeyPrev string
	// AllowInsecureSecret — аварийный обход обеих проверок SecretKey выше
	// (GOTCHA_SECRET_KEY_ALLOW_INSECURE). Только для нестандартных
	// dev-стендов, разбирается безусловно на каждом старте — см. врезку
	// рядом с использованием ниже.
	AllowInsecureSecret bool
	// TrustedProxies — CIDR/IP доверенных reverse-proxy (GOTCHA_TRUSTED_PROXIES).
	// Пусто — X-Forwarded-For не доверяется, per-IP лимитер ключуется по
	// RemoteAddr (см. web.clientIP, SEC-L2).
	TrustedProxies []*net.IPNet
	// RegistrationMode — режим самостоятельной регистрации (PROD-B1):
	// open (открыта всем), invite (по приглашению, кроме bootstrap первого
	// админа), closed (только bootstrap первого админа). Дефолт — invite.
	RegistrationMode string
	// HSTSEnabled/HSTSMaxAgeSeconds/HSTSIncludeSubDomains/HSTSPreload —
	// заголовок Strict-Transport-Security веб-слоя (GOTCHA_HSTS_*). Четыре
	// переменные, а не одна строка: включённость и max-age — РАЗНЫЕ состояния.
	// Выключенный HSTS означает «заголовок не ставится» (его ставит прокси) и
	// пин у браузера НЕ снимает; снять пин можно только реально отправленным
	// max-age=0, поэтому ноль — законное значение, а не «выключено».
	// includeSubDomains по умолчанию false: инстанс часто живёт на поддомене,
	// и флаг с нашей стороны потребовал бы HTTPS от соседних сервисов домена.
	HSTSEnabled           bool
	HSTSMaxAgeSeconds     int
	HSTSIncludeSubDomains bool
	HSTSPreload           bool
	// Locale — язык инстанса для ВНЕШНИХ уведомлений (email/Telegram/webhook):
	// у получателя вне HTTP-запроса нет своей локали, поэтому язык выбирает
	// оператор инстанса (№133–136). UI это не трогает — там локаль зрителя.
	// ru|en, дефолт ru.
	Locale string
	// MigrateOnly — применить миграции и выйти, не поднимая ни одного
	// компонента (флаг --migrate-only).
	//
	// Нужен для развёртываний с GOTCHA_AUTO_MIGRATE_ENABLED=false, где миграции
	// выносят в отдельный init-job. Документация предлагала для этого
	// `docker compose run ... gotcha /bin/sh -c "true"`, но ENTRYPOINT — сам
	// бинарь, а flag.Parse останавливается на первом не-флаге и ошибки не
	// возвращает: команда молча поднимала ПОЛНОЦЕННЫЙ инстанс в режиме all и
	// не завершалась никогда.
	MigrateOnly bool
	// MigrateForcePG/MigrateForceCH — снять флаг dirty со схемы на версии N
	// и выйти (флаги --migrate-force / --migrate-force-ch). −1 — не
	// запрошено. Force не доделывает миграцию — он снимает признак
	// незавершённости; допустимые N проверяет db.ForcePG/ForceCH.
	MigrateForcePG int
	MigrateForceCH int

	// ScrubIP/ScrubEmail/ScrubKeys — серверный PII-scrubbing (PRIV-H1),
	// включён по умолчанию. ScrubIP/ScrubEmail зануляют ip/email субъекта;
	// ScrubKeys — denylist ключей, значения которых редактируются в
	// tags/contexts/stacktrace/span.data.
	ScrubIP    bool
	ScrubEmail bool
	ScrubKeys  []string
	// ScrubAllowKeys — точные имена-исключения из ScrubKeys. Матч denylist
	// намеренно подстрочный и fail-closed (под-scrub = утечка ПДн, over-scrub =
	// потерянное отладочное поле), поэтому author (⊃auth) или tokenizer (⊃token)
	// маскируются по умолчанию. Оператор возвращает нужные ему поля явно —
	// GOTCHA_SCRUB_KEEP_KEYS=author,tokenizer.
	ScrubAllowKeys []string

	// LogLevel/LogFormat — управление логами (GOTCHA_LOGGING_LEVEL: debug|info|warn|
	// error, GOTCHA_LOGGING_FORMAT: text|json). Без них уровень был жёстко зашит в
	// Info, а формат — текстовый: поднять детализацию во время инцидента было
	// нельзя вообще, а текстовый вывод плохо ложится в Loki/ELK.
	LogLevel  string
	LogFormat string
	// ScrubFreeText (RA-L10) — опционально маскировать email-адреса в свободном
	// тексте (message/exception_value/span.description). По умолчанию выключено
	// (консервативно, чтобы не портить SQL/URL); только email, не номера.
	//
	// it-sec P2-2 (2026-08-12): осознанный остаточный риск — при выключенном
	// (дефолтном) флаге email внутри свободного текста исключения оседает в
	// ClickHouse plaintext (структурные email-поля и URL-токены скрабятся
	// независимо от этого флага, см. internal/ingest/scrub.go:ScrubUser/
	// ScrubMessage). Задокументировано для операторов, работающих под 152-ФЗ,
	// в internal/docs/{ru,en}/privacy.md («Обезличивание свободного текста» /
	// «Free-text scrubbing») и configuration.md.
	ScrubFreeText bool

	// SSRFAllowPrivate* (SEC-M1) — разрешить исходящим запросам ходить на
	// приватные/loopback/link-local адреса. По умолчанию всё false
	// (мультитенантная защита от SSRF к метадате/внутренним сервисам).
	//
	// Флаги РАЗДЕЛЬНЫЕ, потому что риск у трёх контуров разный:
	//   Uptime  — самый безобидный: мониторить свой внутренний сервис нужно
	//             постоянно, цель задаёт админ организации.
	//   Webhook — цель задаёт админ проекта, а ответ цели (до 1 КБ) виден в UI
	//             на странице доставок: это полноценный внутренний ридер.
	//   OIDC    — туда уходит client_secret на token_endpoint из discovery.
	//   Telegram — цель СЕГОДНЯ тоже задаёт только оператор (BaseURL —
	//             GOTCHA_TELEGRAM_API_BASE, не арендатор), поэтому риск
	//             ближе к Uptime; проведён через netguard не потому, что
	//             сейчас есть дыра, а для единообразия defense-in-depth на
	//             случай, если base URL когда-нибудь станет пер-канальным
	//             (it-sec P2-1, 2026-08-12). Оператор, у которого свой
	//             telegram-bot-api/прокси на приватном адресе (docker-сеть
	//             и т.п.), включает этот флаг явно.
	// Раньше один GOTCHA_SSRF_ALLOW_PRIVATE расслаблял все три сразу, поэтому
	// «разрешить мониторить внутренний сервис» незаметно открывало и остальные два.
	//
	// Старая переменная сохранена как ДЕФОЛТ для всех — существующие
	// инсталляции не ломаются, но могут сузить разрешение точечно.
	SSRFAllowPrivateUptime   bool
	SSRFAllowPrivateWebhook  bool
	SSRFAllowPrivateOIDC     bool
	SSRFAllowPrivateTelegram bool
	// AutoMigrate (ARCH-M3) — применять миграции схемы на старте. По
	// умолчанию true; false выносит миграции в отдельный init-job, чтобы
	// app-реплики не клинили все разом на dirty-состоянии.
	AutoMigrate bool
	// ExternalChannelDetails — разрешить текст ошибки (title/culprit/body)
	// ЛЮБОМУ получателю уведомления, включая заведомо внешних. По умолчанию
	// false (privacy-by-default): недоверенным получателям уходит только
	// обезличенная ссылка. Оператор включает это, когда у него есть законное
	// основание для трансграничной передачи (152-ФЗ ст. 12).
	ExternalChannelDetails bool
	// TrustedRecipients — домены и хосты, считающиеся своим контуром: почта на
	// этих доменах и вебхуки на этих хостах получают детали события даже при
	// ExternalChannelDetails=false. Совпадение суффиксом, поэтому запись
	// «corp.example» покрывает и «mail.corp.example».
	//
	// Домен из BaseURL доверенный всегда и без настройки — см.
	// alert.NewDetailPolicy. Эта настройка нужна там, где почта и вебхуки
	// живут на другом домене той же организации.
	TrustedRecipients []string

	// UptimeConcurrency — сколько проверок uptime.Runner выполняет
	// одновременно (режимы uptime|all).
	UptimeConcurrency int
	// LocalRegion — имя встроенного региона локальной пробы (см.
	// uptime.DefaultRegion), используется uptime.Runner.
	LocalRegion string
	// ProbeToken/ServerURL — учётные данные выносной пробы (--mode=probe):
	// база центра и Bearer-токен пробы. В этом режиме обязательны — больше
	// пробе знать нечего (ни PG, ни CH она не открывает).
	ProbeToken string
	ServerURL  string

	// AgentDistDir — каталог с install.sh и бинарями gotcha-agent (GOTCHA_DIST_DIR,
	// план A2, задача 10): раскладывается в образ на этапе сборки Docker (Task 12/13),
	// сервер отдаёт их из web-хендлера (internal/web/agentdist.go). Дефолт совпадает с
	// `ENV GOTCHA_DIST_DIR` в Dockerfile — на штатном Docker-проде оператору не
	// нужно ничего задавать явно, а env_file с пустым значением переменной (или её
	// отсутствием) не гасит раздачу (rem-A ops-H1: `str()` трактует пустую строку как
	// «не задано» и берёт этот дефолт). В dev-режиме (go run без docker) каталога по
	// этому пути физически нет — agentDistAvailable() честно отдаёт 404 с подсказкой.
	AgentDistDir string
	// AgentDistRatePerMin — порог per-IP лимитера раздачи бинарей агента
	// (GOTCHA_DIST_RATE_PER_MIN, ops-H4): install.sh качает бинарь и SHA256SUMS
	// (2 запроса на установку/обновление), а ключ лимитера — клиентский IP, поэтому
	// парк за одним egress-адресом (NAT, Ansible-раскатка) делит один бюджет. Дефолт
	// 120/мин — это 60 хостов в минуту с одного IP, с запасом для обычной раскатки;
	// оператор с ещё большим парком за одним IP может поднять порог на время операции.
	AgentDistRatePerMin int

	// ExportDir — каталог файлов выгрузки ошибок/событий (E1, GOTCHA_EXPORT_DIR).
	// Каталог должен пережить пересоздание контейнера — в docker-compose.yml это
	// именованный том exportdata. Если каталог не удаётся создать на старте,
	// выгрузки не фатальны для процесса: раздел выключается (main.go), продукт
	// работает дальше без него.
	ExportDir string
	// ExportTTLHours — срок хранения готового файла выгрузки от завершения
	// заявки, в часах (GOTCHA_EXPORT_RETENTION_HOURS). Дефолт 168 — семь суток.
	ExportTTLHours int
	// ExportMaxRows — потолок строк одной выгрузки (GOTCHA_EXPORT_MAX_ROWS):
	// при достижении заявка помечается Truncated, а не растёт неограниченно.
	// Обязан быть строго меньше защитного предела потока событий
	// (export.eventStreamSafetyLimit, 1 000 000) — это проверяет
	// export.Config.Validate() при построении воркера, не здесь.
	ExportMaxRows int64
	// ExportMaxBytes — потолок размера файла одной выгрузки в байтах
	// (GOTCHA_EXPORT_MAX_BYTES).
	ExportMaxBytes int64
	// ExportDiskBudgetBytes — суммарный бюджет каталога ExportDir
	// (GOTCHA_EXPORT_DISK_BUDGET_BYTES): переполнение — ВРЕМЕННЫЙ отказ новой
	// заявки (до трёх попыток, пока джанитор не освободит место истёкшими
	// файлами), а не частично записанный файл.
	ExportDiskBudgetBytes int64

	// OAuth/social login (этап 5). Каждый провайдер включается независимо;
	// включённый без обязательных секретов → отказ на старте. Секреты живут
	// только в памяти процесса.
	OIDCEnabled        bool
	OIDCIssuer         string
	OIDCClientID       string
	OIDCClientSecret   string
	OIDCScopes         string
	OIDCName           string
	YandexEnabled      bool
	YandexClientID     string
	YandexClientSecret string
	VKEnabled          bool
	VKClientID         string
	VKClientSecret     string
}

var validModes = map[string]bool{
	"ingest": true, "web": true, "uptime": true, "probe": true, "all": true,
}

// defaultScrubKeys — denylist ключей для PII-scrubbing по умолчанию (PRIV-H1).
// Список живёт в internal/ingest (ingest.DefaultDenyKeys), а не здесь: та же
// маска применяется в internal/export к выгрузкам, и два независимых списка
// разъехались бы при первой же правке одного из них.
func defaultScrubKeys() []string {
	return ingest.DefaultDenyKeys()
}

// isLocalBaseURL — BaseURL указывает на локальную разработку (localhost/loopback).
// Для таких стендов дефолтный SecretKey допустим (см. валидацию ниже).
func isLocalBaseURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// secretKeyMattersFor — режимы, для которых мастер-ключ обязателен.
//
// Раньше проверялись только web и all — по логике «ключ подписывает cookie, а
// cookie бывают только там». Но тем же ключом шифруются секреты каналов
// доставки (alert_channels.secret) и SSO client_secret, а РАСШИФРОВЫВАЮТ их
// теперь ingest и uptime тоже: секрет резолвится в момент отправки, а очередь
// уведомлений наполняют оценщики из ingest и детектор аптайма.
//
// Реплика --mode=ingest с дефолтным ключом стартовала молча, а её
// alert.Service шёл без ключа — и отдавал в доставку СЫРОЙ шифротекст
// "enc:…" в качестве bot-токена. Telegram отвечал 401, задача уходила в
// ретраи и в failed, а по симптому это не диагностируется никак.
//
// probe исключён намеренно: он не ходит ни в PG, ни в CH и секретов не видит.
func secretKeyMattersFor(mode string) bool {
	switch mode {
	case "web", "all", "ingest", "uptime":
		return true
	default:
		return false
	}
}

// hstsHeaderMattersFor — режимы, в которых вообще существует web.Handler
// (см. main.go: `webHandler = web.New(...)` — только под
// `cfg.Mode == "web" || cfg.Mode == "all"`), а значит и заголовок
// Strict-Transport-Security физически возможен. Уже, чем secretKeyMattersFor
// выше: ingest и uptime мастер-ключ используют (шифруют/расшифровывают
// секреты каналов), но веб-хендлера не поднимают — предупреждение про
// GOTCHA_BASE_URL не https в этих режимах не про что предупреждать, только
// шумит на каждом старте приёмного узла и dev-стенда uptime.
func hstsHeaderMattersFor(mode string) bool {
	switch mode {
	case "web", "all":
		return true
	default:
		return false
	}
}

// parseInt64Env читает key через getenv и разбирает его как int64. Незаданная
// (пустая) переменная — не ошибка, возвращается def. На ошибке разбора тоже
// возвращается def, а НЕ частичный результат strconv.ParseInt: на «8MiB» тот
// вернул бы 0, а на переполнении — значение, зажатое до края int64, — то есть
// молча превратил бы опечатку оператора в правдоподобное на вид число.
// Ошибка при этом называет переменную по имени. Значение триммится по краям
// той же строкой, что и str()/strGuarded()/parseBool() — иначе голое число
// было бы единственным типом env-значения, для которого случайный пробел
// (перенос из .env.example с отступом, копирование из таблицы) ронял бы
// старт вместо ожидаемого разбора.
func parseInt64Env(getenv func(string) string, key string, def int64) (int64, error) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

// parseIntEnv — как parseInt64Env, но для полей типа int. Парсит с
// bitSize = разрядность int (strconv.IntSize), поэтому итоговое int(n)
// заведомо без усечения на любой платформе — это закрывает CodeQL
// go/incorrect-integer-conversion для доверенного env-конфига (значения
// задаёт оператор, не атакующий).
func parseIntEnv(getenv func(string) string, key string, def int) (int, error) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, strconv.IntSize)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return int(n), nil
}

// logLevels — единственная таблица допустимых значений GOTCHA_LOGGING_LEVEL,
// с готовым slog.Level для каждого. validateLogging проверяет по ней
// принадлежность, setupLogging (cmd/gotcha/main.go) читает по ней же
// slog.Level для уже провалидированного значения — вторую копию этого же
// списка (которая могла бы разойтись с первой при добавлении уровня) заводить
// незачем.
var logLevels = map[string]slog.Level{
	"":        slog.LevelInfo,
	"info":    slog.LevelInfo,
	"debug":   slog.LevelDebug,
	"warn":    slog.LevelWarn,
	"warning": slog.LevelWarn,
	"error":   slog.LevelError,
}

// logFormats — таблица допустимых значений GOTCHA_LOGGING_FORMAT (см.
// докблок logLevels — тот же принцип «одна таблица»).
var logFormats = map[string]bool{
	"":     true,
	"text": true,
	"json": true,
}

// validateLogging проверяет GOTCHA_LOGGING_LEVEL/GOTCHA_LOGGING_FORMAT по
// logLevels/logFormats. Вызывается дважды одним и тем же текстом ошибки:
// из loadConfig (копится в errs вместе с остальной конфигурацией, K5-1) и из
// setupLogging первой строкой (тот же контракт «невалидное значение — отказ
// старта» должен сработать и там, если когда-нибудь вызовется в обход
// loadConfig). level/format здесь ожидаются уже триммленными и в нижнем
// регистре — как cfg.LogLevel/cfg.LogFormat из loadConfig.
func validateLogging(level, format string) error {
	var errs []error
	if _, ok := logLevels[level]; !ok {
		errs = append(errs, fmt.Errorf("GOTCHA_LOGGING_LEVEL must be debug, info, warn (alias warning) or error, got %q", level))
	}
	if _, ok := logFormats[format]; !ok {
		errs = append(errs, fmt.Errorf("GOTCHA_LOGGING_FORMAT must be text or json, got %q", format))
	}
	// Обе проверки независимы (разные переменные) — копятся обе разом, а не
	// только первая по порядку: та же логика, что и у остального блока
	// накопления ошибок в loadConfig (K5-1), но здесь она нужна ВНУТРИ самой
	// validateLogging, потому что она вызывается один раз с обоими значениями
	// сразу и возвращает единственную error.
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func loadConfig(getenv func(string) string, args []string) (Config, error) {
	// envcontract-P0: старые имена переменных окружения проверяются самой
	// первой операцией в loadConfig — до разбора флагов и до
	// security-проверки GOTCHA_SECRET_KEY ниже (см. её докблок «ops P2-1»,
	// где объяснено, почему эта проверка встала ВЫШЕ security-проверки).
	// Смысл порядка: старое имя означает, что часть конфига оператора не
	// прочиталась ВООБЩЕ — обсуждать корректность прочитанных значений
	// (в том числе критичного секретного ключа) раньше, чем оператор узнал,
	// что его .env устарел, — бессмысленный цикл: он мог бы поправить
	// секрет, перезапуститься и невольно продолжить работать с молча
	// применёнными дефолтами вместо остальных своих значений.
	if err := envcontract.CheckRenamedAll(getenv); err != nil {
		return Config{}, err
	}

	fs := flag.NewFlagSet("gotcha", flag.ContinueOnError)
	mode := fs.String("mode", "all", "process role: ingest | web | uptime | probe | all")
	migrateOnly := fs.Bool("migrate-only", false,
		"apply schema migrations and exit (init-job for deployments with GOTCHA_AUTO_MIGRATE_ENABLED=false)")
	migrateForce := fs.Int("migrate-force", -1,
		"clear the dirty flag on the PostgreSQL schema at version N and exit (see upgrade docs)")
	migrateForceCH := fs.Int("migrate-force-ch", -1,
		"clear the dirty flag on the ClickHouse schema at version N and exit (see upgrade docs)")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if !validModes[*mode] {
		return Config{}, fmt.Errorf("invalid --mode %q: want ingest, web, uptime, probe or all", *mode)
	}
	// probe вообще не открывает соединения с базой (см. main), поэтому
	// мигрировать ему нечего — молча выйти нулём было бы обманом: оператор
	// решил бы, что схема применена.
	if *migrateOnly && *mode == "probe" {
		return Config{}, fmt.Errorf("--migrate-only is not valid with --mode=probe: a probe never opens the database")
	}
	// Те же соображения для force: probe базу не открывает; с --migrate-only
	// это разные намерения (применить ↔ снять флаг); друг с другом — миграции
	// идут последовательно, застрять могла только одна база.
	if (*migrateForce >= 0 || *migrateForceCH >= 0) && *mode == "probe" {
		return Config{}, fmt.Errorf("--migrate-force is not valid with --mode=probe: a probe never opens the database")
	}
	if (*migrateForce >= 0 || *migrateForceCH >= 0) && *migrateOnly {
		return Config{}, fmt.Errorf("--migrate-force and --migrate-only are different intents (clear the dirty flag vs apply migrations): pass one")
	}
	if *migrateForce >= 0 && *migrateForceCH >= 0 {
		return Config{}, fmt.Errorf("--migrate-force and --migrate-force-ch cannot be combined: migrations run sequentially, only one database can be stuck dirty")
	}

	// str — обрезает пробелы по краям; строка из одних пробелов — то же самое,
	// что переменная не задана вовсе (контракт envcontract: пустая строка =
	// «не задано»), поэтому в обоих случаях отдаётся def. rem-A ops-H1 уже
	// проверял пустой env против отсутствующего — пробельный добавляется к
	// той же трактовке, не заводит третью.
	str := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}

	var errs []error

	// strGuarded — как str, но для переменных, где тихий откат на def при
	// НЕПУСТОМ-но-пробельном значении опаснее отказа старта: GOTCHA_SECRET_KEY
	// откатился бы на публично известный insecure-dev-secret (SEC-C1 ниже его
	// пропустит на localhost, и оператор годами хранил бы секреты «под
	// пробелом»), GOTCHA_PG_DSN/GOTCHA_CH_DSN — на localhost вместо прод-БД,
	// которую оператор явно указал. Отличие от str(): там, где переменная
	// НЕ задана вовсе (raw == ""), поведение то же самое — def безопасен,
	// это штатный путь дев-стенда без .env. Опасен только случай «оператор
	// что-то туда написал, но там оказались только пробелы».
	strGuarded := func(key, def string) string {
		raw := getenv(key)
		if raw == "" {
			return def
		}
		if v := strings.TrimSpace(raw); v != "" {
			return v
		}
		errs = append(errs, fmt.Errorf(
			"%s must not be blank (got only whitespace); unset the variable entirely to use the default",
			key))
		return def
	}

	// parseBool — распознаёт булево значение env; для непустого нераспознанного
	// значения копит ошибку (RA-L4: `SCRUB_IP=ture` не должен молча выключать
	// privacy-дефолт). Возвращает (значение, задано-ли-непустое).
	parseBool := func(key string) (bool, bool) {
		v := strings.ToLower(strings.TrimSpace(getenv(key)))
		switch v {
		case "":
			return false, false
		case "1", "true", "yes", "on":
			return true, true
		case "0", "false", "no", "off":
			return false, true
		default:
			errs = append(errs, fmt.Errorf("%s: invalid boolean %q (want 1/0/true/false/yes/no/on/off)", key, getenv(key)))
			return false, true
		}
	}

	boolEnv := func(key string) bool {
		v, _ := parseBool(key)
		return v
	}

	// boolEnvDef — как boolEnv, но unset → def (для флагов, включённых по
	// умолчанию: явные 0/false/no/off → false).
	boolEnvDef := func(key string, def bool) bool {
		v, set := parseBool(key)
		if !set {
			return def
		}
		return v
	}

	// num/intNum — тонкие обёртки над parseInt64Env/parseIntEnv (package-level,
	// см. ниже), которые только копят ошибку в errs; сам разбор и решение
	// «что вернуть на ошибке» живут в проверяемых напрямую функциях, а не в
	// замыкании, — иначе мутация в теле разбора не наблюдаема снаружи
	// loadConfig ни одним тестом (errors.Join(errs...) уходит раньше любых
	// cfg.X-валидаций, так что разница между def и частичным результатом
	// strconv никогда не долетает до Config).
	num := func(key string, def int64) int64 {
		n, err := parseInt64Env(getenv, key, def)
		if err != nil {
			errs = append(errs, err)
		}
		return n
	}

	intNum := func(key string, def int) int {
		n, err := parseIntEnv(getenv, key, def)
		if err != nil {
			errs = append(errs, err)
		}
		return n
	}

	// PROD-B2: редакция определяет дефолт квот. В oss безлимит (0),
	// в saas — прежний 1_000_000. Явный GOTCHA_DEFAULT_*_QUOTA перекрывает.
	//
	// lower() здесь и у RegistrationMode/Locale ниже — расширение
	// принимаемого, не сужение: раньше "EDITION=OSS" ронял старт с
	// "must be oss or saas", хотя оператор написал ровно то значение, что
	// документация приводит в примерах (переменные окружения принято писать
	// капсом), только с заглавной буквы у значения. str() уже триммит —
	// lower() той же строкой убирает и регистр перед сравнением enum'ов.
	edition := strings.ToLower(str("GOTCHA_EDITION", "oss"))
	defQuota := int64(0)
	if edition == "saas" {
		defQuota = 1_000_000
	}

	// RunEvaluators — тристабильный: nil, если GOTCHA_EVALUATORS_ENABLED не задан
	// (см. докблок поля). Разбор — тот же parseBool, что у всех остальных
	// булевых полей, поэтому «ture» копит ошибку в errs, а не тихо становится
	// false.
	var runEvaluators *bool
	if v, set := parseBool("GOTCHA_EVALUATORS_ENABLED"); set {
		runEvaluators = &v
	}

	// GOTCHA_MAX_WRITER_BUFFER_BYTES/GOTCHA_MAX_INGEST_QUEUE_BYTES: 0 внутри Config остаётся
	// сигналом «нет явного потолка, посчитай/возьми дефолт пакета» для
	// downstream (effectiveMaxBufferBytes, autoMaxBufferBytes в main.go) — это
	// НЕ меняется. Но 0 не заданной переменной и 0, написанный оператором
	// руками, обязаны различаться: num() возвращает одно и то же значение
	// (def=0) что при отсутствии переменной, что после её фикса на ошибке
	// разбора, поэтому решение «задано ли явно и разобралось ли» берём ДО
	// вызова num() — по сырому значению env и по факту добавления ошибки
	// именно для этого ключа. Так «мусор» и «явный 0» не дублируют друг
	// друга вторым сообщением об ошибке.
	maxBufferBytesSet := strings.TrimSpace(getenv("GOTCHA_MAX_WRITER_BUFFER_BYTES")) != ""
	maxBufferBytesErrsBefore := len(errs)
	maxBufferBytes := num("GOTCHA_MAX_WRITER_BUFFER_BYTES", 0)
	if maxBufferBytesSet && len(errs) == maxBufferBytesErrsBefore && maxBufferBytes < 1 {
		errs = append(errs, fmt.Errorf(
			"GOTCHA_MAX_WRITER_BUFFER_BYTES must be >= 1 (0 or negative refuses startup here, unlike "+
				"the other retention/quota variables where 0 means unlimited/forever; unset the "+
				"variable entirely to use the writer package default), got %d", maxBufferBytes))
	}

	maxQueueBytesSet := strings.TrimSpace(getenv("GOTCHA_MAX_INGEST_QUEUE_BYTES")) != ""
	maxQueueBytesErrsBefore := len(errs)
	maxQueueBytes := num("GOTCHA_MAX_INGEST_QUEUE_BYTES", 0)
	if maxQueueBytesSet && len(errs) == maxQueueBytesErrsBefore && maxQueueBytes < 1 {
		errs = append(errs, fmt.Errorf(
			"GOTCHA_MAX_INGEST_QUEUE_BYTES must be >= 1 (0 or negative refuses startup here, unlike "+
				"the other retention/quota variables where 0 means unlimited/forever; unset the "+
				"variable entirely to use the ingest queue package default), got %d", maxQueueBytes))
	}

	cfg := Config{
		Mode:                     *mode,
		MigrateOnly:              *migrateOnly,
		MigrateForcePG:           *migrateForce,
		MigrateForceCH:           *migrateForceCH,
		Addr:                     str("GOTCHA_LISTEN_ADDR", ":8080"),
		BaseURL:                  str("GOTCHA_BASE_URL", "http://localhost:8080"),
		PostgresDSN:              strGuarded("GOTCHA_PG_DSN", "postgres://gotcha:gotcha@localhost:5432/gotcha?sslmode=disable"),
		ClickHouseDSN:            strGuarded("GOTCHA_CH_DSN", "clickhouse://localhost:9000/gotcha"),
		SMTPHost:                 str("GOTCHA_SMTP_HOST", ""),
		SMTPPort:                 intNum("GOTCHA_SMTP_PORT", 587),
		SMTPUser:                 str("GOTCHA_SMTP_USER", ""),
		SMTPPassword:             str("GOTCHA_SMTP_PASSWORD", ""),
		SMTPFrom:                 str("GOTCHA_SMTP_FROM", ""),
		TelegramAPIBase:          str("GOTCHA_TELEGRAM_API_BASE", ""),
		RetentionDays:            intNum("GOTCHA_EVENT_RETENTION_DAYS", 90),
		SpanRetentionDays:        intNum("GOTCHA_SPAN_RETENTION_DAYS", 30),
		MetricRetentionDays:      intNum("GOTCHA_METRIC_RETENTION_DAYS", 30),
		ProfileRetentionDays:     intNum("GOTCHA_PROFILE_RETENTION_DAYS", 7),
		LogRetentionDays:         intNum("GOTCHA_LOG_RETENTION_DAYS", 14),
		IncidentRetentionDays:    intNum("GOTCHA_INCIDENT_RETENTION_DAYS", 90),
		DeployRetentionDays:      intNum("GOTCHA_DEPLOY_RETENTION_DAYS", 90),
		Edition:                  edition,
		DefaultEventQuota:        num("GOTCHA_DEFAULT_EVENT_QUOTA", defQuota),
		DefaultTransactionQuota:  num("GOTCHA_DEFAULT_TRANSACTION_QUOTA", defQuota),
		DefaultMetricQuota:       num("GOTCHA_DEFAULT_METRIC_QUOTA", defQuota),
		DefaultProfileQuota:      num("GOTCHA_DEFAULT_PROFILE_QUOTA", defQuota),
		DefaultLogQuota:          num("GOTCHA_DEFAULT_LOG_QUOTA", defQuota),
		MaxEventBytes:            num("GOTCHA_MAX_EVENT_BYTES", 1<<20),
		IngestRateLimit:          intNum("GOTCHA_INGEST_RATE_PER_SEC", 500),
		MaxBufferBytes:           maxBufferBytes,
		MaxQueueBytes:            maxQueueBytes,
		AlertBudgetWindowSeconds: intNum("GOTCHA_ALERT_BUDGET_WINDOW_SECONDS", 3600),
		AlertBudgetLimit:         intNum("GOTCHA_ALERT_BUDGET_LIMIT", 50),
		CardinalityLimit:         intNum("GOTCHA_CARDINALITY_LIMIT", 10000),
		CardinalityWindowSeconds: intNum("GOTCHA_CARDINALITY_WINDOW_SECONDS", 3600),
		RunEvaluators:            runEvaluators,
		MetricEvalInterval:       intNum("GOTCHA_METRIC_EVAL_INTERVAL_SECONDS", 60),
		ProfileEvalInterval:      intNum("GOTCHA_PROFILE_EVAL_INTERVAL_SECONDS", 300),
		HostEvalInterval:         intNum("GOTCHA_HOST_EVAL_INTERVAL_SECONDS", 60),
		SLOEvalInterval:          intNum("GOTCHA_SLO_EVAL_INTERVAL_SECONDS", 120),
		EscalationInterval:       intNum("GOTCHA_ESCALATION_INTERVAL_SECONDS", 60),
		DependencySettleSeconds:  intNum("GOTCHA_DEPENDENCY_SETTLE_SECONDS", 300),
		OutboxRetentionDays:      intNum("GOTCHA_OUTBOX_RETENTION_DAYS", 7),
		PurgeReconcileHours:      intNum("GOTCHA_PROJECT_PURGE_RECONCILE_HOURS", 24),
		NotifyConcurrency:        intNum("GOTCHA_NOTIFY_CONCURRENCY", 4),
		SecretKey:                strGuarded("GOTCHA_SECRET_KEY", "insecure-dev-secret"),
		// GOTCHA_SECRET_KEY_PREV — читается ДОСЛОВНО, через getenv напрямую,
		// а не через str()/strGuarded(): назначение PREV — держать прежний
		// мастер-ключ байт в байт, каким бы он ни был, чтобы им ещё можно
		// было расшифровать секреты, зашифрованные ДО ротации. Если старый
		// GOTCHA_SECRET_KEY имел хвостовой пробел (str() его бы обрезал, но
		// шифровал он именно "ключ+пробел"), тримминг PREV сделал бы
		// единственный штатный путь восстановления нерабочим: расшифровка
		// пошла бы уже другим ключом. GOTCHA_SECRET_KEY по-прежнему триммится
		// strGuarded() — это текущий ключ, у него нет обратной совместимости
		// с историческими байтами.
		SecretKeyPrev:         getenv("GOTCHA_SECRET_KEY_PREV"),
		RegistrationMode:      strings.ToLower(str("GOTCHA_REGISTRATION_MODE", "invite")),
		HSTSEnabled:           boolEnvDef("GOTCHA_HSTS_ENABLED", true),
		HSTSMaxAgeSeconds:     intNum("GOTCHA_HSTS_MAX_AGE_SECONDS", 31536000),
		HSTSIncludeSubDomains: boolEnv("GOTCHA_HSTS_INCLUDE_SUBDOMAINS"),
		HSTSPreload:           boolEnv("GOTCHA_HSTS_PRELOAD"),
		Locale:                strings.ToLower(str("GOTCHA_LOCALE", "ru")),
		UptimeConcurrency:     intNum("GOTCHA_UPTIME_CONCURRENCY", 50),
		LocalRegion:           str("GOTCHA_UPTIME_LOCAL_REGION", "local"),
		ProbeToken:            str("GOTCHA_PROBE_KEY", ""),
		ServerURL:             str("GOTCHA_PROBE_SERVER_URL", ""),
		AgentDistDir:          str("GOTCHA_DIST_DIR", "/opt/gotcha/agent-dist"),
		AgentDistRatePerMin:   intNum("GOTCHA_DIST_RATE_PER_MIN", 120),
		ExportDir:             str("GOTCHA_EXPORT_DIR", "/var/lib/gotcha/exports"),
		ExportTTLHours:        intNum("GOTCHA_EXPORT_RETENTION_HOURS", 168),
		ExportMaxRows:         num("GOTCHA_EXPORT_MAX_ROWS", 200_000),
		ExportMaxBytes:        num("GOTCHA_EXPORT_MAX_BYTES", 268_435_456),
		ExportDiskBudgetBytes: num("GOTCHA_EXPORT_DISK_BUDGET_BYTES", 5_368_709_120),
	}
	// GOTCHA_SECRET_KEY_PREV читается дословно (см. её докблок у поля выше),
	// а значит не проходит через собственную blank-проверку strGuarded() —
	// тот же контракт повторён здесь явно: пробельное (но непустое)
	// значение почти наверняка случайный пробел, а не осознанный
	// «prev-ключ из одних пробелов», и молчаливая трактовка его как
	// настоящего ключа собрала бы кольцо шифрования с ключом, которым
	// НИЧЕГО не шифровалось, — тот же класс ошибки, что и у
	// GOTCHA_SECRET_KEY/GOTCHA_PG_DSN/GOTCHA_CH_DSN в strGuarded(). Пустая
	// строка по-прежнему означает «не задано» (тримминг тут ни при чём —
	// пустая строка не становится пробельной никаким тримингом), поэтому
	// проверяется именно "непусто, но триммится в пусто", а не просто
	// TrimSpace(raw) == "". Не под secretKeyMattersFor(cfg.Mode): та же
	// гигиена значения, что и у strGuarded(), которая тоже безусловна.
	if raw := cfg.SecretKeyPrev; raw != "" && strings.TrimSpace(raw) == "" {
		errs = append(errs, fmt.Errorf(
			"GOTCHA_SECRET_KEY_PREV must not be blank (got only whitespace); "+
				"unset the variable entirely if no rotation is in progress"))
	}
	// GOTCHA_PG_DSN/GOTCHA_CH_DSN — единственные обязательные адреса, которых
	// до сих пор старт не разбирал вовсе: опечатка всплывала только на первом
	// db.NewPostgres/db.NewClickHouse, то есть после того, как остальная
	// конфигурация уже принята. Проверяем разбираемость БЕЗ соединения —
	// тем же парсером, что реально потребляет клиент (pgxpool.ParseConfig,
	// clickhouse.ParseDSN), а не общим baseurl.Normalize: DSN — не «базовый
	// адрес» с обязательными схемой/хостом и запретом на query, а форма,
	// которую сам клиент понимает как в URL-виде (postgres://…), так и в
	// keyword/value-виде (host=… user=… dbname=…) — обе легальны для pgx,
	// baseurl.Normalize отверг бы вторую как «не URL».
	//
	// Обе ошибки копятся в errs, а не возвращаются немедленно: это два
	// НОВЫХ старта-проверки, вставленные в функцию, которая уже копит
	// числовые/булевы опечатки и отдаёт их разом (errors.Join ниже) — цикл
	// «оператор чинит один DSN, перезапускается, узнаёт про следующую
	// опечатку» — ровно то, что накопление errs существует, чтобы убрать.
	// Немедленный return сохранился бы, если бы результат разбора DSN тёк
	// куда-то дальше по функции, но он никуда не течёт: PostgresDSN/
	// ClickHouseDSN используются позже только как есть (сырой строкой),
	// не через возврат этой проверки.
	if err := db.ValidatePostgresDSN(cfg.PostgresDSN); err != nil {
		errs = append(errs, fmt.Errorf("GOTCHA_PG_DSN: %w", err))
	}
	if err := db.ValidateClickHouseDSN(cfg.ClickHouseDSN); err != nil {
		errs = append(errs, fmt.Errorf("GOTCHA_CH_DSN: %w", err))
	}
	// GOTCHA_BASE_URL — база всех ссылок, которые продукт строит сам: heartbeat
	// cron-команда (см. её докблок в web/monitorform.go — curl без -L, редиректа
	// не будет), OAuth RedirectURI, приглашения (orgsettings.go). Проверяем и
	// нормализуем на старте тем же хелпером baseurl.Normalize, что и
	// GOTCHA_TELEGRAM_API_BASE/GOTCHA_PROBE_SERVER_URL ниже и GOTCHA_AGENT_ENDPOINT
	// (internal/agent/config.go) — один хелпер на все четыре базовых адреса,
	// а не четыре расходящиеся политики: без схемы/хоста каждая построенная
	// ссылка вела бы в никуда, а хвостовая косая («…app/») даёт «…app//dashboard»
	// в КАЖДОЙ из них — молча, до первого репорта пользователя о битой ссылке в
	// письме или упавшем cron.
	//
	// Ошибка тоже копится в errs, а не возвращается немедленно (та же
	// причина, что у DSN выше) — с сырым, ненормализованным cfg.BaseURL,
	// который продолжает жить в конфиге до самого возврата. SEC-C1 ниже
	// (слабый/дефолтный секрет на не-local адресе) читает и isLocalBaseURL
	// (cfg.BaseURL), и baseURLErr САМ: на сыром невалидном значении
	// isLocalBaseURL честно возвращает false (url.Parse либо падает, либо
	// отдаёт хост, не совпадающий ни с localhost, ни с 127.0.0.1, ни с ::1),
	// но SEC-C1 намеренно НЕ стреляет, пока baseURLErr != nil (см. её
	// докблок) — «не-локальный» на невалидном значении означает «неизвестно»,
	// а не «точно публичный», и решение по сомнительному входу не должно
	// самостоятельным немедленным return топить саму причину сомнения —
	// ошибку разбора GOTCHA_BASE_URL, уже накопленную в errs строкой ниже.
	normalizedBaseURL, baseURLErr := baseurl.Normalize("GOTCHA_BASE_URL", cfg.BaseURL)
	if baseURLErr != nil {
		errs = append(errs, baseURLErr)
	} else {
		cfg.BaseURL = normalizedBaseURL
	}
	cfg.OIDCEnabled = boolEnv("GOTCHA_OIDC_ENABLED")
	cfg.OIDCIssuer = str("GOTCHA_OIDC_ISSUER", "")
	cfg.OIDCClientID = str("GOTCHA_OIDC_CLIENT_ID", "")
	cfg.OIDCClientSecret = str("GOTCHA_OIDC_CLIENT_SECRET", "")
	cfg.OIDCScopes = str("GOTCHA_OIDC_SCOPES", "")
	cfg.OIDCName = str("GOTCHA_OIDC_DISPLAY_NAME", "")
	cfg.YandexEnabled = boolEnv("GOTCHA_YANDEX_ENABLED")
	cfg.YandexClientID = str("GOTCHA_YANDEX_CLIENT_ID", "")
	cfg.YandexClientSecret = str("GOTCHA_YANDEX_CLIENT_SECRET", "")
	cfg.VKEnabled = boolEnv("GOTCHA_VK_ENABLED")
	cfg.VKClientID = str("GOTCHA_VK_CLIENT_ID", "")
	cfg.VKClientSecret = str("GOTCHA_VK_CLIENT_SECRET", "")

	// PRIV-H1: PII-scrubbing включён по умолчанию.
	cfg.ScrubIP = boolEnvDef("GOTCHA_SCRUB_IP", true)
	cfg.ScrubEmail = boolEnvDef("GOTCHA_SCRUB_EMAIL", true)
	cfg.ScrubFreeText = boolEnv("GOTCHA_SCRUB_FREETEXT")
	// Общий флаг — дефолт для трёх раздельных (обратная совместимость).
	ssrfAll := boolEnv("GOTCHA_SSRF_ALLOW_PRIVATE")
	cfg.SSRFAllowPrivateUptime = boolEnvDef("GOTCHA_SSRF_ALLOW_PRIVATE_UPTIME", ssrfAll)
	cfg.SSRFAllowPrivateWebhook = boolEnvDef("GOTCHA_SSRF_ALLOW_PRIVATE_WEBHOOK", ssrfAll)
	cfg.SSRFAllowPrivateOIDC = boolEnvDef("GOTCHA_SSRF_ALLOW_PRIVATE_OIDC", ssrfAll)
	cfg.SSRFAllowPrivateTelegram = boolEnvDef("GOTCHA_SSRF_ALLOW_PRIVATE_TELEGRAM", ssrfAll)
	cfg.AutoMigrate = boolEnvDef("GOTCHA_AUTO_MIGRATE_ENABLED", true)
	// --migrate-only подразумевает применение миграций: этот запуск ради них и
	// существует. Иначе флаг вместе с GOTCHA_AUTO_MIGRATE_ENABLED=false — а это ровно
	// та конфигурация, для которой он и нужен, — только проверил бы схему и
	// вышел, ничего не применив.
	if cfg.MigrateOnly {
		cfg.AutoMigrate = true
	}
	// Privacy-by-default: полный текст ошибок/стектрейсов/имён транзакций может
	// нести ПДн, а внешние каналы (Telegram — серверы за пределами РФ, webhook)
	// уводят его наружу, потенциально трансгранично (152-ФЗ ст.12). По умолчанию
	// шлём обезличенный payload (только ссылка/заголовок); оператор осознанно
	// включает детали через GOTCHA_EXTERNAL_CHANNEL_DETAILS_ENABLED=true.
	cfg.ExternalChannelDetails = boolEnvDef("GOTCHA_EXTERNAL_CHANNEL_DETAILS_ENABLED", false)
	// GOTCHA_SCRUB_DENY_KEYS ДОПОЛНЯЕТ дефолтный denylist, а не заменяет его.
	//
	// Раньше заменял, и это был тихий откат защиты: оператор дописывал одно своё
	// поле — и терял скрубинг password/token/secret/cookie/cvv целиком, ничего об
	// этом не узнав. Хуже того, значение вида ",," проходило проверку на
	// непустоту, все элементы отсеивались как пустые, ветка с дефолтами
	// пропускалась, и denylist оставался ПУСТЫМ.
	//
	// Убрать конкретный дефолт по-прежнему можно — точным именем через
	// GOTCHA_SCRUB_KEEP_KEYS. Это осознанное действие оператора, а не побочный
	// эффект добавления своего ключа.
	cfg.ScrubKeys = defaultScrubKeys()
	for _, k := range strings.Split(getenv("GOTCHA_SCRUB_DENY_KEYS"), ",") {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			cfg.ScrubKeys = append(cfg.ScrubKeys, k)
		}
	}
	cfg.LogLevel = strings.ToLower(strings.TrimSpace(getenv("GOTCHA_LOGGING_LEVEL")))
	cfg.LogFormat = strings.ToLower(strings.TrimSpace(getenv("GOTCHA_LOGGING_FORMAT")))

	// Исключения из denylist — точными именами (см. Config.ScrubAllowKeys).
	for _, k := range strings.Split(getenv("GOTCHA_SCRUB_KEEP_KEYS"), ",") {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			cfg.ScrubAllowKeys = append(cfg.ScrubAllowKeys, k)
		}
	}
	// GOTCHA_TRUSTED_RECIPIENTS — домены/хосты своего контура (см.
	// Config.TrustedRecipients). Разбираем как список имён: невалидное имя тут
	// не опасно (просто ни с чем не совпадёт), поэтому — в отличие от
	// TRUSTED_PROXIES — падать на нём не за что.
	for _, h := range strings.Split(getenv("GOTCHA_TRUSTED_RECIPIENTS"), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			cfg.TrustedRecipients = append(cfg.TrustedRecipients, h)
		}
	}
	// GOTCHA_TRUSTED_PROXIES — список CIDR («10.0.0.0/8») и/или голых IP
	// («192.168.1.5», трактуется как /32 или /128) доверенных прокси.
	// Невалидные записи — ошибка конфигурации, а не тихий пропуск: молча
	// проигнорированный прокси означал бы, что XFF не доверяется и лимитер
	// снова ключуется по IP прокси (тихая деградация защиты).
	if tp := strings.TrimSpace(getenv("GOTCHA_TRUSTED_PROXIES")); tp != "" {
		for _, item := range strings.Split(tp, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if !strings.Contains(item, "/") {
				if ip := net.ParseIP(item); ip != nil {
					if ip.To4() != nil {
						item += "/32"
					} else {
						item += "/128"
					}
				}
			}
			_, n, err := net.ParseCIDR(item)
			if err != nil {
				errs = append(errs, fmt.Errorf("GOTCHA_TRUSTED_PROXIES: invalid entry %q: %w", item, err))
				continue
			}
			cfg.TrustedProxies = append(cfg.TrustedProxies, n)
		}
	}
	// ops P2-1: проверка GOTCHA_SECRET_KEY стоит ПЕРЕД возвратом накопленных
	// errs ниже (а не среди остальных валидаций хвостом функции) сознательно —
	// это единственная security-critical проверка конфига (слабый/дефолтный
	// ключ на не-local BaseURL = угон аккаунта через OAuth-link, SEC-C1). Она
	// возвращается СВОИМ отдельным `return`, а не через накопление в errs, —
	// поэтому даже теперь, когда loadConfig отдаёт ВСЕ накопленные числовые/
	// булевы опечатки разом (errors.Join ниже), слабый секрет при этом не
	// тонет между ними одной строкой из многих: если у оператора ОДНОВРЕМЕННО
	// опечатка в каком-то числовом поле (например GOTCHA_EVENT_RETENTION_DAYS=abc)
	// И слабый секрет, порядок ниже гарантирует, что он увидит СНАЧАЛА и
	// ТОЛЬКО предупреждение про секрет — иначе он мог бы исправить опечатку,
	// перезапуститься и невольно продолжить стартовать со слабым ключом ещё
	// один цикл деплоя, пока не увидит следующую ошибку.
	//
	// Тем не менее envcontract.CheckRenamedAll в самом начале loadConfig стоит ВЫШЕ
	// даже этой проверки: старое имя означает, что часть .env оператора не
	// прочиталась ВООБЩЕ, тихо заменившись дефолтом, — это более базовая
	// поломка конфига, чем состояние конкретно секретного ключа, и о ней
	// нужно узнать раньше любой другой диагностики, иначе оператор потратит
	// цикл деплоя на секрет, а после рестарта увидит, что у него вдобавок
	// не применились ещё какие-то из его настроек.
	//
	// GOTCHA_SECRET_KEY_ALLOW_INSECURE разбирается ЗДЕСЬ, ДО обеих проверок ниже,
	// а не inline через boolEnv() в их && условиях: у обеих проверок первый
	// операнд короткого замыкания после режима — состояние самого ключа
	// (== devSecretKey / длина < 32), и при СИЛЬНОМ кастомном ключе оба
	// условия останавливаются на этом операнде раньше, чем дошли бы до
	// boolEnv("GOTCHA_SECRET_KEY_ALLOW_INSECURE") — мусорное значение переменной
	// («ture» и т.п.) в этом случае вообще не разбиралось бы и не копилось
	// в errs, хотя оператор его туда явно написал. Разбор здесь исполняется
	// безусловно на каждом старте, до вычисления любого из условий ниже.
	cfg.AllowInsecureSecret = boolEnv("GOTCHA_SECRET_KEY_ALLOW_INSECURE")

	// SEC-C1: дефолтный ключ подписи oauth-cookie публично известен из
	// исходников. В серверных режимах на не-localhost BaseURL это дыра
	// (угон аккаунта через OAuth-link) — отказываемся стартовать.
	// Escape-hatch для нестандартного dev-окружения —
	// GOTCHA_SECRET_KEY_ALLOW_INSECURE=1.
	//
	// baseURLErr == nil в условии: если GOTCHA_BASE_URL сам не разобрался,
	// isLocalBaseURL на сыром значении не врёт (не путает мусор с localhost),
	// но «не-локальный» тут означает «неизвестно», а не «точно публичный» —
	// решение по сомнительному входу подменило бы собой САМУ ПРИЧИНУ
	// (невалидный BASE_URL, уже накопленный в errs строкой выше) отдельным
	// немедленным return, который её топит. Разобраться с URL — первый шаг;
	// SEC-C1 либо сработает при следующем старте на уже валидном значении,
	// либо нет.
	if secretKeyMattersFor(cfg.Mode) &&
		cfg.SecretKey == devSecretKey &&
		baseURLErr == nil &&
		!isLocalBaseURL(cfg.BaseURL) &&
		!cfg.AllowInsecureSecret {
		return Config{}, fmt.Errorf(
			"GOTCHA_SECRET_KEY must be set to a strong random value for a non-local %s instance "+
				"(default key is public and enables OAuth account takeover); "+
				"set GOTCHA_SECRET_KEY_ALLOW_INSECURE=1 to override for development", cfg.Mode)
	}

	// SEC: слишком короткий кастомный ключ — слабый ключ подписи oauth-cookie и
	// мастер шифрования SSO client_secret. В серверных режимах на не-local
	// требуем >= 32 байт (стандартный минимум для ключа). Тот же escape-hatch
	// и та же оговорка про baseURLErr, что и у проверки дефолтного ключа выше.
	if secretKeyMattersFor(cfg.Mode) &&
		cfg.SecretKey != devSecretKey &&
		len(cfg.SecretKey) < 32 &&
		baseURLErr == nil &&
		!isLocalBaseURL(cfg.BaseURL) &&
		!cfg.AllowInsecureSecret {
		return Config{}, fmt.Errorf(
			"GOTCHA_SECRET_KEY is too short (%d bytes) for a non-local %s instance; "+
				"use at least 32 random bytes (e.g. `openssl rand -hex 32`); "+
				"set GOTCHA_SECRET_KEY_ALLOW_INSECURE=1 to override for development",
			len(cfg.SecretKey), cfg.Mode)
	}

	// GOTCHA_SECRET_KEY_PREV — предыдущий мастер-ключ на время ротации.
	// Три сочетания с текущим ключом не про стойкость (GOTCHA_SECRET_KEY_ALLOW_INSECURE
	// тут ни при чём — эскейп-хэтч снимает требования к силе ключа, а не чинит
	// конфиг, физически не способный сделать то, что от него ждут), а про то,
	// что молча проигнорированная переменная убедила бы оператора в ротации,
	// которой на самом деле нет:
	if secretKeyMattersFor(cfg.Mode) && cfg.SecretKeyPrev != "" {
		switch {
		case cfg.SecretKeyPrev == devSecretKey:
			// Дев-ключом никогда ничего не шифровалось — предыдущим ключом
			// ротации он быть не может по определению.
			return Config{}, fmt.Errorf(
				"GOTCHA_SECRET_KEY_PREV must not be the public dev default " +
					"(nothing was ever encrypted with it, so it cannot be a rotation source); " +
					"unset GOTCHA_SECRET_KEY_PREV if no rotation is in progress")
		case cfg.SecretKeyPrev == cfg.SecretKey:
			// PREV равен текущему — ротации нет, а конфиг выглядит так, будто
			// она идёт.
			return Config{}, fmt.Errorf(
				"GOTCHA_SECRET_KEY_PREV must differ from GOTCHA_SECRET_KEY " +
					"(a rotation source equal to the current key means no rotation " +
					"is actually happening); unset GOTCHA_SECRET_KEY_PREV if none is")
		case cfg.SecretKey == devSecretKey:
			// Текущий ключ дев → шифрование at-rest выключено целиком, PREV
			// молча ничего не расшифровывал бы.
			return Config{}, fmt.Errorf(
				"GOTCHA_SECRET_KEY_PREV is set but GOTCHA_SECRET_KEY is still the dev " +
					"default (encryption at rest is off entirely on the dev key, so " +
					"GOTCHA_SECRET_KEY_PREV would silently do nothing); " +
					"set a real GOTCHA_SECRET_KEY or unset GOTCHA_SECRET_KEY_PREV")
		}
	}

	// Возвращаем ВСЕ накопленные ошибки, а не только первую: оператор с
	// пятью опечатками правит .env за один проход, а не за пять деплоев.
	// errors.Join кладёт каждую на отдельную строку, и каждая из них уже
	// начинается с имени переменной (см. num/intNum/parseBool выше и
	// GOTCHA_TRUSTED_PROXIES/GOTCHA_MAX_WRITER_BUFFER_BYTES/GOTCHA_MAX_INGEST_QUEUE_BYTES
	// рядом) — этим текстом оператор чинит конфиг.
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}

	// K5-1: с этой точки и до конца функции ошибки КОПЯТСЯ в errs, а не
	// возвращаются немедленно — тот же принцип, что и в первом накопительном
	// блоке выше (числовые/булевы опечатки): оператор с семью независимыми
	// ошибками конфига (enum, range, экспортная четвёрка, HSTS, oauth-провайдер)
	// правит .env за один проход, а не за семь деплоев. Остаются fail-fast
	// только проверки ВЫШЕ первого errors.Join (разбор флагов и то, что от
	// него зависит) — они сделали бы накопление бессмысленным (без --mode
	// дальнейшие проверки не про что вести). validateLogging — тем же путём,
	// что и остальные: setupLogging (cmd/gotcha/main.go) вызывает ту же
	// функцию первой строкой, так что текст ошибки один и тот же что при
	// старте, что при использовании до сборки логгера.
	if err := validateLogging(cfg.LogLevel, cfg.LogFormat); err != nil {
		errs = append(errs, err)
	}

	// max-age проверяется независимо от HSTSEnabled: отрицательное значение —
	// всегда опечатка оператора, а не режим.
	if cfg.HSTSMaxAgeSeconds < 0 {
		errs = append(errs, fmt.Errorf(
			"GOTCHA_HSTS_MAX_AGE_SECONDS must be >= 0 (0 sends max-age=0, which un-pins browsers), got %d",
			cfg.HSTSMaxAgeSeconds))
	}
	// Проверки preload — ТОЛЬКО при включённом HSTS: иначе аварийный откат
	// («выключить HSTS, флаги оставить как были») упирался бы в отказ старта
	// ровно тогда, когда сервис и так лежит.
	if cfg.HSTSEnabled && cfg.HSTSPreload {
		// Заголовок без includeSubDomains или с коротким max-age в preload-список
		// всё равно не примут, а владелец будет считать, что подал заявку.
		// Обе проверки независимы (разные переменные) — копятся по отдельности,
		// оператор увидит сразу обе, если ошибся в обеих.
		if !cfg.HSTSIncludeSubDomains {
			errs = append(errs, fmt.Errorf(
				"GOTCHA_HSTS_PRELOAD requires GOTCHA_HSTS_INCLUDE_SUBDOMAINS=true: "+
					"the preload list rejects a header without includeSubDomains"))
		}
		if cfg.HSTSMaxAgeSeconds < 31536000 {
			errs = append(errs, fmt.Errorf(
				"GOTCHA_HSTS_PRELOAD requires GOTCHA_HSTS_MAX_AGE_SECONDS >= 31536000 (one year), got %d",
				cfg.HSTSMaxAgeSeconds))
		}
	}
	if cfg.HSTSEnabled {
		if hstsHeaderMattersFor(cfg.Mode) && !strings.HasPrefix(cfg.BaseURL, "https://") {
			slog.Warn("GOTCHA_HSTS_ENABLED is on but GOTCHA_BASE_URL is not https:// — " +
				"Strict-Transport-Security is never sent on a plain HTTP deploy")
		}
	} else {
		// Факт ПРИСУТСТВИЯ переменной, а не её значение: boolEnvDef/boolEnv
		// возвращают уже готовое значение и «выставлено в дефолт» от «не
		// выставлено» через них не отличить.
		for _, name := range []string{
			"GOTCHA_HSTS_MAX_AGE_SECONDS",
			"GOTCHA_HSTS_INCLUDE_SUBDOMAINS",
			"GOTCHA_HSTS_PRELOAD",
		} {
			if strings.TrimSpace(getenv(name)) != "" {
				slog.Warn(fmt.Sprintf(
					"HSTS is off (GOTCHA_HSTS_ENABLED=false) — %s is ignored", name),
					"var", name)
			}
		}
	}

	switch cfg.RegistrationMode {
	case "open", "invite", "closed":
	default:
		errs = append(errs, fmt.Errorf("GOTCHA_REGISTRATION_MODE must be open, invite or closed, got %q", cfg.RegistrationMode))
	}

	switch cfg.Locale {
	case "ru", "en":
	default:
		errs = append(errs, fmt.Errorf("GOTCHA_LOCALE must be ru or en, got %q", cfg.Locale))
	}

	switch cfg.Edition {
	case "oss", "saas":
	default:
		errs = append(errs, fmt.Errorf("GOTCHA_EDITION must be oss or saas, got %q", cfg.Edition))
	}

	if cfg.RetentionDays < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_EVENT_RETENTION_DAYS must be >= 0 (0 keeps data forever), got %d", cfg.RetentionDays))
	}
	if cfg.SpanRetentionDays < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_SPAN_RETENTION_DAYS must be >= 0 (0 keeps data forever), got %d", cfg.SpanRetentionDays))
	}
	if cfg.MetricRetentionDays < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_METRIC_RETENTION_DAYS must be >= 0 (0 keeps data forever), got %d", cfg.MetricRetentionDays))
	}
	if cfg.AlertBudgetWindowSeconds < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_ALERT_BUDGET_WINDOW_SECONDS must be >= 1, got %d", cfg.AlertBudgetWindowSeconds))
	}
	if cfg.AlertBudgetLimit < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_ALERT_BUDGET_LIMIT must be >= 0 (0 disables the ceiling), got %d", cfg.AlertBudgetLimit))
	}
	if cfg.IngestRateLimit < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_INGEST_RATE_PER_SEC must be >= 0 (0 disables the limit), got %d", cfg.IngestRateLimit))
	}
	if cfg.CardinalityLimit < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_CARDINALITY_LIMIT must be >= 0 (0 disables the limit), got %d", cfg.CardinalityLimit))
	}
	if cfg.CardinalityWindowSeconds < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_CARDINALITY_WINDOW_SECONDS must be >= 1, got %d", cfg.CardinalityWindowSeconds))
	}
	if cfg.MetricEvalInterval < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_METRIC_EVAL_INTERVAL_SECONDS must be >= 1, got %d", cfg.MetricEvalInterval))
	}
	if cfg.ProfileRetentionDays < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_PROFILE_RETENTION_DAYS must be >= 0 (0 keeps data forever), got %d", cfg.ProfileRetentionDays))
	}
	if cfg.LogRetentionDays < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_LOG_RETENTION_DAYS must be >= 0 (0 keeps data forever), got %d", cfg.LogRetentionDays))
	}
	if cfg.IncidentRetentionDays < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_INCIDENT_RETENTION_DAYS must be >= 0 (0 keeps data forever), got %d", cfg.IncidentRetentionDays))
	}
	if cfg.DeployRetentionDays < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_DEPLOY_RETENTION_DAYS must be >= 0 (0 keeps data forever), got %d", cfg.DeployRetentionDays))
	}
	if cfg.OutboxRetentionDays < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_OUTBOX_RETENTION_DAYS must be >= 1, got %d", cfg.OutboxRetentionDays))
	}
	if cfg.PurgeReconcileHours < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_PROJECT_PURGE_RECONCILE_HOURS must be >= 0, got %d", cfg.PurgeReconcileHours))
	}
	if cfg.NotifyConcurrency < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_NOTIFY_CONCURRENCY must be >= 1, got %d", cfg.NotifyConcurrency))
	}
	if cfg.ProfileEvalInterval < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_PROFILE_EVAL_INTERVAL_SECONDS must be >= 1, got %d", cfg.ProfileEvalInterval))
	}
	if cfg.HostEvalInterval < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_HOST_EVAL_INTERVAL_SECONDS must be >= 1, got %d", cfg.HostEvalInterval))
	}
	if cfg.SLOEvalInterval < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_SLO_EVAL_INTERVAL_SECONDS must be >= 1, got %d", cfg.SLOEvalInterval))
	}
	if cfg.EscalationInterval < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_ESCALATION_INTERVAL_SECONDS must be >= 1, got %d", cfg.EscalationInterval))
	}
	if cfg.DependencySettleSeconds < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_DEPENDENCY_SETTLE_SECONDS must be >= 0, got %d", cfg.DependencySettleSeconds))
	}
	// Квоты: 0 = безлимит (легитимно в любой редакции), отрицательные — ошибка.
	if cfg.DefaultEventQuota < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_DEFAULT_EVENT_QUOTA must be >= 0, got %d", cfg.DefaultEventQuota))
	}
	if cfg.DefaultTransactionQuota < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_DEFAULT_TRANSACTION_QUOTA must be >= 0, got %d", cfg.DefaultTransactionQuota))
	}
	if cfg.DefaultMetricQuota < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_DEFAULT_METRIC_QUOTA must be >= 0, got %d", cfg.DefaultMetricQuota))
	}
	if cfg.DefaultProfileQuota < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_DEFAULT_PROFILE_QUOTA must be >= 0, got %d", cfg.DefaultProfileQuota))
	}
	if cfg.DefaultLogQuota < 0 {
		errs = append(errs, fmt.Errorf("GOTCHA_DEFAULT_LOG_QUOTA must be >= 0, got %d", cfg.DefaultLogQuota))
	}
	if cfg.MaxEventBytes < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_MAX_EVENT_BYTES must be >= 1, got %d", cfg.MaxEventBytes))
	}
	if cfg.UptimeConcurrency < 1 {
		errs = append(errs, fmt.Errorf("GOTCHA_UPTIME_CONCURRENCY must be >= 1, got %d", cfg.UptimeConcurrency))
	}
	// GOTCHA_SMTP_PORT — TCP-порт, диапазон 1..65535. Раньше -1/0/99999
	// проходили старт молча (SMTPHost=="" вообще не трогает почту), а отказ
	// всплывал только при первой попытке отправки — то есть на первом же
	// алерте, письме-приглашении или уведомлении о выгрузке, месяцами
	// спустя после деплоя. Диапазон проверяется независимо от того, задан
	// ли GOTCHA_SMTP_HOST: мусор в порту — опечатка оператора уже сейчас,
	// а не когда-нибудь потом при первой отправке.
	if cfg.SMTPPort < 1 || cfg.SMTPPort > 65535 {
		errs = append(errs, fmt.Errorf("GOTCHA_SMTP_PORT must be between 1 and 65535, got %d", cfg.SMTPPort))
	}
	// Экспортная четвёрка (GOTCHA_EXPORT_RETENTION_HOURS/_MAX_ROWS/_MAX_BYTES/
	// _DISK_BUDGET_BYTES) валидируется той же export.Config.Validate(), что
	// worker.Tick вызывает на каждом тике (internal/export/worker.go) — но
	// раньше ТОЛЬКО там: Run() глотал её ошибку как slog.Warn на каждом
	// тике, процесс стартовал с виду здоровым, раздел «Выгрузки» был виден
	// в UI, а заявки копились в очереди навсегда, потому что ни один тик не
	// проходил дальше Validate(). Вызов из Tick НЕ убирается — лимиты можно
	// поправить на ходу без рестарта, и вторая линия защиты остаётся; эта
	// проверка на старте её дублирует, а не заменяет.
	if err := (export.Config{
		TTL:        time.Duration(cfg.ExportTTLHours) * time.Hour,
		MaxRows:    cfg.ExportMaxRows,
		MaxBytes:   cfg.ExportMaxBytes,
		DiskBudget: cfg.ExportDiskBudgetBytes,
	}).Validate(); err != nil {
		errs = append(errs, errors.New(translateExportEnvNames(err.Error())))
	}
	// GOTCHA_TELEGRAM_API_BASE — свой Bot API вместо api.telegram.org.
	// Отправитель дописывает к адресу «/bot{token}/sendMessage», поэтому
	// проверяем и нормализуем тем же baseurl.Normalize, что GOTCHA_BASE_URL
	// выше: без схемы и хоста каждая доставка падала бы с "unsupported
	// protocol scheme", а запрос или фрагмент оказались бы посреди пути —
	// адрес, по которому никто не отвечает, вместо внятного отказа при
	// запуске. Хвостовые косые снимаем сами: «…org/» дало бы «…org//bot…»,
	// и Bot API ответил бы 404 на каждое уведомление.
	normalizedTelegramAPIBase, err := baseurl.Normalize("GOTCHA_TELEGRAM_API_BASE", cfg.TelegramAPIBase)
	if err != nil {
		errs = append(errs, err)
	} else {
		cfg.TelegramAPIBase = normalizedTelegramAPIBase
	}

	// GOTCHA_PROBE_SERVER_URL — формат-проверку (тем же baseurl.Normalize)
	// гейтим режимом пробы: вне --mode=probe переменную никто не читает, и
	// ронять старт по невалидному-но-неиспользуемому значению — ровно тот
	// класс тихих breaking-change, ради предотвращения которого делается E3
	// (round 1 ревью задачи 6: до этой правки "not-a-url" вне --mode=probe
	// стало отказом старта, хотя на BASE и по контракту это должно быть
	// молчаливо-игнорируемое значение с предупреждением). Внутри режима
	// пробы — вся строгость Normalize, как раньше: схему/хост проверяем и
	// хвостовую «/» срезаем сразу — без этого «https://host/» давало бы
	// «//probe/lease» в каждом запросе (внутри internal/uptime/probeclient.go
	// от этого есть отдельная защита на точке использования, TrimSuffix — но
	// контракт замораживается на уровне конфигурации, а не только там).
	if cfg.Mode == "probe" {
		// ServerURL и ProbeToken — независимые переменные: ошибка нормализации
		// первой не мешает проверить вторую (иначе оператор с обеими
		// незаданными видит только одну ошибку за деплой).
		normalizedServerURL, err := baseurl.Normalize("GOTCHA_PROBE_SERVER_URL", cfg.ServerURL)
		if err != nil {
			errs = append(errs, err)
		} else {
			cfg.ServerURL = normalizedServerURL
			if cfg.ServerURL == "" {
				errs = append(errs, fmt.Errorf("GOTCHA_PROBE_SERVER_URL is required with --mode=probe"))
			}
		}
		if cfg.ProbeToken == "" {
			errs = append(errs, fmt.Errorf("GOTCHA_PROBE_KEY is required with --mode=probe"))
		}
	} else if cfg.ServerURL != "" {
		// Заданный, но бесполезный вне --mode=probe: ничего его не читает,
		// поэтому оператор, скорее всего, либо забыл переключить режим, либо
		// перенёс переменную из .env пробы по ошибке — тот же паттерн, что
		// предупреждение о GOTCHA_HSTS_* при выключенном GOTCHA_HSTS_ENABLED
		// выше. Безусловно, НЕЗАВИСИМО от того, валидно значение или нет —
		// формат вне режима пробы не проверяется вовсе (см. выше).
		slog.Warn("GOTCHA_PROBE_SERVER_URL is set but --mode is not probe — the value is ignored",
			"mode", cfg.Mode)
	}

	if cfg.OIDCEnabled && (cfg.OIDCIssuer == "" || cfg.OIDCClientID == "" || cfg.OIDCClientSecret == "") {
		errs = append(errs, fmt.Errorf("GOTCHA_OIDC_ENABLED requires GOTCHA_OIDC_ISSUER, _CLIENT_ID and _CLIENT_SECRET"))
	}
	if cfg.YandexEnabled && (cfg.YandexClientID == "" || cfg.YandexClientSecret == "") {
		errs = append(errs, fmt.Errorf("GOTCHA_YANDEX_ENABLED requires GOTCHA_YANDEX_CLIENT_ID and _CLIENT_SECRET"))
	}
	if cfg.VKEnabled && (cfg.VKClientID == "" || cfg.VKClientSecret == "") {
		errs = append(errs, fmt.Errorf("GOTCHA_VK_ENABLED requires GOTCHA_VK_CLIENT_ID and _CLIENT_SECRET"))
	}

	// Финальная точка возврата всего этого блока: если что-то из выше
	// накопленного не прошло, отдаём всё разом (см. докблок errors.Join
	// первого накопительного блока выше — тот же принцип).
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// loadConfigChecked оборачивает loadConfig проверкой неизвестных имён,
// которую loadConfig сам выполнить не может (см. докблок checkUnknownEnvVars
// ниже), в ЕДИНСТВЕННОМ порядке, который держит диагностику точной: сначала
// loadConfig (а внутри него, самой первой операцией, envcontract.CheckRenamedAll),
// и только если он прошёл — checkUnknownEnvVars. Устаревшее имя из
// envcontract.Renamed не входит в envcontract.Known (оно больше не читается
// ни одним конфигом), так что если поменять эти два вызова местами,
// устаревшее имя получит не точный ответ «renamed to NEW_NAME», а куда менее
// полезную догадку по Левенштейну (или вовсе никакой, если расстояние до
// ближайшего известного имени больше порога) — оператор увидит не то
// сообщение, которое разработчики предусмотрели именно для его случая.
//
// main.go зовёт эту функцию вместо loadConfig напрямую; internal/agent эту
// проверку не выполняет вовсе (свой процесс, свой куда более узкий набор
// переменных, чужая GOTCHA_* переменная в общем `.env` для него — не повод
// отказывать в старте, см. envcontract.CheckRenamedScoped).
func loadConfigChecked(getenv func(string) string, environ func() []string, args []string) (Config, error) {
	cfg, err := loadConfig(getenv, args)
	if err != nil {
		return Config{}, err
	}
	if err := checkUnknownEnvVars(environ); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// checkUnknownEnvVars отказывает старту, если в окружении процесса
// присутствует переменная с префиксом GOTCHA_, которую не читает ни
// cmd/gotcha (envcontract.Known), ни internal/agent (тоже входит в
// envcontract.Known — сервер и агент штатно делят один хост и один `.env`,
// см. докблок envcontract.Known). Опечатка в текущем имени (GOTCHA_HSTS_ENABLE
// вместо GOTCHA_HSTS_ENABLED) раньше проходила молча с дефолтом; теперь она
// такой же fail-fast, как устаревшее имя (envcontract.CheckRenamedAll).
//
// GOTCHA_COMPOSE_* и GOTCHA_BUILD_* исключены целиком по префиксу, а не
// перечислением: их читает сам Docker Compose (подстановка ${...}) или
// Makefile (build-args образа), ни один Go-процесс их не читает и знать их
// поимённо не обязан — `env_file: .env` в docker-compose.yml легитимно
// кладёт их в окружение процесса gotcha наравне со всем остальным файлом.
// Обратная сторона: опечатка ВНУТРИ этих двух префиксов (GOTCHA_COMPOSE_PG_PASWORD)
// проходит молча — исключение защищает диапазон имён целиком, не каждое имя
// в нём по отдельности; страховка на этот случай — не эта проверка, а то,
// что сам Compose откажет в подстановке для `${GOTCHA_COMPOSE_PG_PASSWORD:?}`
// без дефолта, если оператор реально забудет пароль, а не опечатается в
// имени переменной, которую Compose и так не потребует.
//
// Принимает environ func() []string, а не читает os.Environ() напрямую:
// loadConfig выше принимает только getenv func(string) string и
// принципиально не может перечислить всё окружение процесса (интерфейс «дай
// значение по ключу», а не «покажи, что вообще задано») — эта проверка
// требует именно перечисления, поэтому она отдельная функция со своей
// точкой инъекции (в проде — os.Environ, в тестах — срез-фикстура), а не
// ветка внутри loadConfig.
//
// internal/agent.LoadConfig несёт СВОЮ, более узкую версию той же идеи
// (checkUnknownAgentEnvVars) — она смотрит только на переменные с префиксом
// GOTCHA_AGENT_, а не на весь GOTCHA_* целиком, как эта функция: агент
// штатно ставится на тот же хост, что и сервер, с общим `.env`, и обязан
// пропускать чужие (серверные) имена молча — отказ по ним был бы
// самоуправством, не защитой. Но опечатка ВНУТРИ своего же префикса — тот
// же класс дыры, что и здесь: на удалённом хосте, где агент стоит один,
// этой (серверной) проверки нет вовсе, и без агентской install.sh --check
// подтверждал бы битый конфиг агента как «config OK».
//
// Имя из envcontract.Renamed — ОТДЕЛЬНАЯ ветка, не «unknown», и это НЕ
// дублирует CheckRenamedAll (loadConfig выше, самая первая операция): та
// проверяет только НЕПУСТОЕ значение (declared-but-unset для по-настоящему
// живого имени — легитимный случай, докblock checkRenamed в
// internal/envcontract/check.go), так что переименованное имя с ПУСТЫМ
// значением долетает досюда, минуя CheckRenamedAll. Но declared-but-unset
// имеет смысл только для имени, которое что-то ЕЩЁ читает по старому
// написанию, — переименованное имя не читает уже никто, так что пустое
// значение его не спасает: тот же контракт, что у internal/agent.
// checkUnknownAgentEnvVars (после её собственной правки на тот же случай),
// тем же текстом — envcontract.RenamedError, а не заново набранным «unknown».
func checkUnknownEnvVars(environ func() []string) error {
	var renamed, unknown []string
	for _, kv := range environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "GOTCHA_") {
			continue
		}
		if envcontract.Known[name] {
			continue
		}
		if strings.HasPrefix(name, "GOTCHA_COMPOSE_") || strings.HasPrefix(name, "GOTCHA_BUILD_") {
			continue
		}
		if _, wasRenamed := envcontract.Renamed[name]; wasRenamed {
			renamed = append(renamed, name)
			continue
		}
		unknown = append(unknown, name)
	}
	var errs []error
	if err := envcontract.RenamedError(renamed); err != nil {
		errs = append(errs, err)
	}
	if len(unknown) == 0 {
		return errors.Join(errs...)
	}
	// Отсортировано — иначе порядок вхождений в тексте ошибки менялся бы
	// между запусками вместе с порядком os.Environ() (тот не гарантирован).
	sort.Strings(unknown)
	parts := make([]string, len(unknown))
	for i, name := range unknown {
		if suggestions := suggestKnownNames(name); len(suggestions) > 0 {
			parts[i] = fmt.Sprintf("%s (unknown; did you mean %s?)", name, strings.Join(suggestions, " or "))
		} else {
			parts[i] = fmt.Sprintf("%s (unknown)", name)
		}
	}
	errs = append(errs, fmt.Errorf("unknown environment variable(s), check for typos: %s", strings.Join(parts, ", ")))
	return errors.Join(errs...)
}

// maxSuggestDistance — порог расстояния Левенштейна для подсказки похожего
// известного имени: 2 ловит характерные опечатки одной буквой или коротким
// суффиксом (GOTCHA_HSTS_ENABLE → GOTCHA_HSTS_ENABLED, вставка одной буквы;
// GOTCHA_INGEST_RATE_PER_SEK → GOTCHA_INGEST_RATE_PER_SEC, замена одной), но
// не расползается на имена из другой подсистемы, которые случайно похожи
// длиной и общими буквами, а не опечаткой одного и того же имени.
const maxSuggestDistance = 2

// suggestKnownNames возвращает известные имена в пределах maxSuggestDistance
// редактирований от name, отсортированные ДЕТЕРМИНИРОВАННО: сначала по
// расстоянию, затем по алфавиту. envcontract.Known — map, обход которой не
// гарантирует порядок сам по себе (как и находки CheckRenamedAll выше по
// файлу) — без сортировки текст ошибки менялся бы между запусками при
// нескольких кандидатах на одном расстоянии.
func suggestKnownNames(name string) []string {
	type candidate struct {
		name string
		dist int
	}
	var candidates []candidate
	for known := range envcontract.Known {
		if d := levenshteinDistance(name, known); d <= maxSuggestDistance {
			candidates = append(candidates, candidate{known, d})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].dist != candidates[j].dist {
			return candidates[i].dist < candidates[j].dist
		}
		return candidates[i].name < candidates[j].name
	})
	out := make([]string, len(candidates))
	for i, c := range candidates {
		out[i] = c.name
	}
	return out
}

// levenshteinDistance — расстояние редактирования (вставка/удаление/замена
// одного символа) между двумя строками, классическое динамическое
// программирование в две строки-буфера (а не полная матрица) — длины имён
// переменных окружения малы (десятки символов), но нет причины заводить
// O(n*m) память там, где хватает O(m).
func levenshteinDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			deletion := prev[j] + 1
			insertion := curr[j-1] + 1
			substitution := prev[j-1] + cost
			min := deletion
			if insertion < min {
				min = insertion
			}
			if substitution < min {
				min = substitution
			}
			curr[j] = min
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}
