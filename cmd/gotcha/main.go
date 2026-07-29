package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"
	"gitflic.ru/otezvikentiy/gotcha/internal/telemetry"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/version"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	// Часовые пояса — в самом бинаре. Базовый образ (alpine) tzdata не несёт, и
	// без этого time.LoadLocation падает на любом поясе, кроме UTC: форма окон
	// обслуживания предлагает Europe/Moscow и соседей, а сохранить их было
	// нельзя. Хуже того, окно с непонятным поясом роняло проверку обслуживания
	// в детекторе, и инцидент не открывался вовсе.
	//
	// Пакет, а не apk add: так продукт не зависит от базового образа — бинарь,
	// собранный и запущенный где угодно, ведёт себя одинаково. Цена — около
	// 450 КБ.
	_ "time/tzdata"
)

func main() {
	if err := run(); err != nil {
		slog.Error("gotcha failed", "error", err)
		os.Exit(1)
	}
}

// versionRequested — true, если среди аргументов есть флаг версии.
func versionRequested(args []string) bool {
	for _, a := range args {
		if a == "--version" || a == "version" {
			return true
		}
	}
	return false
}

// deriveCookieKey выводит из мастер-секрета отдельный подключ для HMAC-подписи
// oauth-flow cookie (доменное разделение, Info20): HMAC-SHA256(master, label).
// Детерминирован (переживает рестарт), не совпадает с ключом at-rest-шифрования
// SSO (sha256(master) в org). Пустой мастер → пустой ключ: web-слой сам
// подставит дефолт для стендов (см. oauthflow.go secret()).
func deriveCookieKey(master string) string {
	if master == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte("gotcha:oauth-cookie-mac:v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

// setupLogging настраивает глобальный slog по GOTCHA_LOG_LEVEL/GOTCHA_LOG_FORMAT.
// Пустые/нераспознанные значения дают прежнее поведение (текст, уровень Info),
// поэтому апгрейд ничего не меняет молча. json — для Loki/ELK, debug — чтобы
// поднять детализацию во время инцидента без пересборки.
func setupLogging(level, format string) {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}

// writerStats — то, что каждый буферизованный писатель рассказывает о себе.
// Один интерфейс на пятерых (события, спаны, метрики, профили, аптайм) —
// добавление шестого не потребует трогать регистрацию.
type writerStats interface {
	Buffered() int64
	Dropped() int64
	InsertFailures() int64
}

// registerWriterMetrics публикует три счётчика писателя под меткой writer=.
// Именно этих трёх не хватало при разборе «часть событий не доезжает»: глубина
// буфера показывает, принимает ли хранилище, отказы вставки — почему, а потери
// говорят, что данные уже НЕ вернуть.
func registerWriterMetrics(r *selfmetrics.Registry, name string, w writerStats) {
	lbl := map[string]string{"writer": name}
	r.AddInt(selfmetrics.Gauge, "gotcha_writer_buffered_rows",
		"Rows buffered in memory, waiting to be written to ClickHouse.", lbl, w.Buffered)
	r.AddInt(selfmetrics.Counter, "gotcha_writer_dropped_rows_total",
		"Rows dropped because the buffer overflowed; these are lost for good.", lbl, w.Dropped)
	r.AddInt(selfmetrics.Counter, "gotcha_writer_insert_failures_total",
		"Failed batch inserts. The batch is retried, so this is not data loss by itself.", lbl, w.InsertFailures)
}

func run() error {
	if versionRequested(os.Args[1:]) {
		fmt.Println("gotcha", version.String())
		return nil
	}
	cfg, err := loadConfig(os.Getenv, os.Args[1:])
	if err != nil {
		return err
	}
	setupLogging(cfg.LogLevel, cfg.LogFormat)
	if cfg.SecretKey == devSecretKey {
		slog.Warn("GOTCHA_SECRET_KEY is not set — using insecure dev default (fine for localhost only)")
	}
	// SEC-M3: сессионная cookie без Secure на не-loopback HTTP уходит открытым
	// текстом (сниффинг/replay). Для продукта мониторинга дефолт должен толкать к TLS.
	if !isLocalBaseURL(cfg.BaseURL) && !strings.HasPrefix(cfg.BaseURL, "https://") {
		slog.Warn("GOTCHA_BASE_URL is non-local plain HTTP — session cookies ride unencrypted; enable TLS (https)")
	}
	// Исходящие OIDC-вызовы (discovery/JWKS/token/userinfo) — SSRF-safe по тому же
	// флагу, что webhook/uptime: приватные адреса режутся, если оператор не разрешил
	// их явно (внутренний IdP). Ставим до любого OAuth-обмена.
	oauth.SetAllowPrivateHosts(cfg.SSRFAllowPrivateOIDC)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Выносная проба — отдельная ветка до всего остального: ей не нужны ни
	// Postgres, ни ClickHouse, ни HTTP-сервер (и входящих портов у неё в
	// чужом регионе может не быть вовсе). Только исходящие запросы к центру,
	// пока не придёт сигнал.
	if cfg.Mode == "probe" {
		probe := &uptime.ProbeClient{
			ServerURL:           cfg.ServerURL,
			Token:               cfg.ProbeToken,
			Concurrency:         cfg.UptimeConcurrency,
			AllowPrivateTargets: cfg.SSRFAllowPrivateUptime,
		}
		probe.Run(ctx)
		slog.Info("probe stopped")
		return nil
	}

	// Кому уйдут детали события — печатаем один раз на старте, до того как
	// заработает первый нотифаер (в режиме probe нотифаеров нет вовсе).
	logDetailPolicy(cfg)

	pg, err := db.NewPostgres(ctx, cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer pg.Close()

	ch, err := db.NewClickHouse(ctx, cfg.ClickHouseDSN)
	if err != nil {
		return err
	}
	defer ch.Close()

	// Сигнал во время миграций не прерывает их (golang-migrate не берёт
	// context) — процесс завершится после текущего шага.
	slog.Info("applying migrations")
	err = db.WithMigrationLock(ctx, pg, func() error {
		// ARCH-M3: авто-миграцию можно отключить (GOTCHA_AUTO_MIGRATE=false) и
		// выносить в отдельный init-job, чтобы app-реплики не клинили все разом.
		if cfg.AutoMigrate {
			if err := db.MigratePG(cfg.PostgresDSN); err != nil {
				return err
			}
			if err := db.MigrateCH(cfg.ClickHouseDSN); err != nil {
				return err
			}
			// Проверка схемы нужна И ЗДЕСЬ, после успешной миграции. Гейт ловит
			// не только отставание, но и ОПЕРЕЖЕНИЕ: схема новее встроенной —
			// значит бинарь откатили после неудачного релиза. Для m.Up() такая
			// схема выглядит как ErrNoChange, то есть старый бинарь стартовал
			// молча и падал уже на вставках. А upgrade.md обещает оператору
			// ровно обратное — внятную ошибку при старте. AUTO_MIGRATE включён
			// по умолчанию, так что до этой правки обещание не выполнялось в
			// самой частой конфигурации.
			if err := db.CheckSchemaCurrent(cfg.PostgresDSN); err != nil {
				return err
			}
			if err := db.CheckSchemaCurrentCH(cfg.ClickHouseDSN); err != nil {
				return err
			}
		} else {
			// RA-8: без авто-миграции app не должен стартовать на отставшей схеме
			// (иначе insert падает на каждой вставке → тихий дроп телеметрии).
			// Проверяем и PG, и CH (audit-3: CH-схема тоже нуждается в гейте).
			if err := db.CheckSchemaCurrent(cfg.PostgresDSN); err != nil {
				return err
			}
			if err := db.CheckSchemaCurrentCH(cfg.ClickHouseDSN); err != nil {
				return err
			}
		}
		if err := db.ApplyRetention(ctx, ch, cfg.RetentionDays); err != nil {
			return err
		}
		if err := db.ApplySpanRetention(ctx, ch, cfg.SpanRetentionDays); err != nil {
			return err
		}
		if err := db.ApplyMetricRetention(ctx, ch, cfg.MetricRetentionDays); err != nil {
			return err
		}
		if err := db.ApplyProfileRetention(ctx, ch, cfg.ProfileRetentionDays); err != nil {
			return err
		}
		if err := db.ApplyTransactionRetention(ctx, ch, cfg.RetentionDays); err != nil {
			return err
		}
		// RA-L3 (audit-3): web_vitals_5m тоже должен получать TTL, иначе inner-таблица
		// MV растёт вечно (имя транзакции может нести URL — 152-ФЗ).
		return db.ApplyWebVitalsRetention(ctx, ch, cfg.RetentionDays)
	})
	if err != nil {
		return err
	}

	// --migrate-only: схема применена, больше делать нечего. Отдельный
	// init-job для развёртываний с GOTCHA_AUTO_MIGRATE=false, где миграции
	// прогоняют один раз перед стартом реплик.
	if cfg.MigrateOnly {
		slog.Info("migrations applied, exiting (--migrate-only)")
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler(pg, ch))
	mux.HandleFunc("GET /version", versionHandler())

	// Самотелеметрия. Роут регистрируется здесь, а метрики добавляются ниже по
	// мере создания писателей: реестр ленив — он хранит функции и опрашивает их
	// на каждом скрапе, поэтому порядок регистрации значения не имеет.
	//
	// Без аутентификации, как /healthz и /version: тут нет ни ПДн, ни секретов —
	// только счётчики буферов и потерь. И, в отличие от /healthz, ни одного
	// обращения к БД: /metrics обязан отвечать именно тогда, когда БД лежит.
	var selfMetrics selfmetrics.Registry
	selfMetrics.Add(selfmetrics.Gauge, "gotcha_build_info",
		"Build metadata; the value is always 1, the version lives in the label.",
		map[string]string{"version": version.String(), "mode": cfg.Mode},
		func() float64 { return 1 })
	mux.HandleFunc("GET /metrics", selfMetrics.Handler())

	// Общие сервисы нужны и ingest-у, и web-у — строим один раз на любой
	// активный режим, а не дублируем на каждый. alertSvc/emailSender/outbox
	// тоже общие: ingest использует их для срабатывания правил
	// (Evaluator/Spike) и доставки, а web — для страницы
	// /projects/{id}/alerts (правила/каналы/failed-доставки, см.
	// web.Handler.Alerts/Outbox) и синхронной отправки писем-приглашений (см.
	// web.Handler.Email в orgsettings.go).
	var orgSvc *org.Service
	var issueSvc *issue.Service
	var alertSvc *alert.Service
	var emailSender *notify.EmailSender
	var outbox *notify.Outbox
	if cfg.Mode == "ingest" || cfg.Mode == "web" || cfg.Mode == "all" {
		orgSvc = org.NewService(pg, cfg.DefaultEventQuota)
		orgSvc.SetQuotaDefaults(cfg.DefaultTransactionQuota, cfg.DefaultMetricQuota, cfg.DefaultProfileQuota)
		// SSO client_secret шифруется этим мастер-ключом at-rest. С публично
		// известным dev-дефолтом шифровать бессмысленно — ключ виден в исходниках,
		// а «enc:»-значение давало бы ложное чувство защиты (Info21). Тогда
		// оставляем plaintext, как при пустом ключе. На не-localhost web/all
		// дефолтный ключ и так отбивается валидацией конфига, поэтому в реальном
		// проде сюда приходит настоящий ключ и шифрование включается.
		if cfg.SecretKey != devSecretKey {
			orgSvc.SetSecretKey(cfg.SecretKey)
		}
		issueSvc = issue.NewService(pg)
		alertSvc = alert.NewService(pg)
		if cfg.SecretKey != devSecretKey {
			alertSvc.SetSecretKey(cfg.SecretKey)
		}
		alertSvc.SetBudget(time.Duration(cfg.AlertBudgetWindowSeconds)*time.Second, cfg.AlertBudgetLimit)
		emailSender = notify.NewEmailSender(notify.EmailConfig{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort,
			User: cfg.SMTPUser, Password: cfg.SMTPPassword, From: cfg.SMTPFrom,
		})
		outbox = notify.NewOutbox(pg)
	}

	// uptimeSvc/uptimeWriter — как и orgSvc/issueSvc выше, общие для любого
	// активного режима, которому они нужны: web монтирует героя этой задачи,
	// публичный heartbeat-роут (webHandler.Uptime/UptimeWriter), даже когда
	// сам процесс не крутит Runner (--mode=web без --mode=uptime, например
	// отдельная реплика для входящих HTTP-запросов); uptime собирает поверх
	// них Runner. В --mode=all оба контура делят один ResultWriter — одна
	// очередь вставок в ClickHouse на процесс, а не по одной на контур.
	var uptimeSvc *uptime.Service
	var uptimeWriter *uptime.ResultWriter
	var uptimeDetector *uptime.Detector
	var uptimeNotifier *uptime.OutboxNotifier
	var uptimeIngestor *uptime.Ingestor
	if cfg.Mode == "web" || cfg.Mode == "uptime" || cfg.Mode == "all" {
		uptimeSvc = uptime.NewService(pg)
		// Имя встроенного региона — конфигурируемое: Service.Regions предлагает
		// его в форме монитора, и оно обязано совпадать с тем, которое лизит
		// Runner ниже, иначе монитор попал бы в регион, который не проверяет
		// никто.
		uptimeSvc.LocalRegion = cfg.LocalRegion
		uptimeWriter = uptime.NewResultWriter(ch)
		registerWriterMetrics(&selfMetrics, "uptime_results", uptimeWriter)
		go uptimeWriter.Run()

		// alertSvc/outbox/emailSender are only built for ingest/web/all above —
		// uptime alone doesn't need alerting/outbox for anything else, but the
		// detector's notifier (down/up/ssl_expiring/reminder) is delivered
		// through the very same Outbox, so build them here too when uptime
		// runs on its own.
		if alertSvc == nil {
			alertSvc = alert.NewService(pg)
			if cfg.SecretKey != devSecretKey {
				alertSvc.SetSecretKey(cfg.SecretKey)
			}
			alertSvc.SetBudget(time.Duration(cfg.AlertBudgetWindowSeconds)*time.Second, cfg.AlertBudgetLimit)
		}
		if outbox == nil {
			outbox = notify.NewOutbox(pg)
		}
		if emailSender == nil {
			emailSender = notify.NewEmailSender(notify.EmailConfig{
				Host: cfg.SMTPHost, Port: cfg.SMTPPort,
				User: cfg.SMTPUser, Password: cfg.SMTPPassword, From: cfg.SMTPFrom,
			})
		}
		uptimeNotifier = &uptime.OutboxNotifier{
			Alerts:       alertSvc,
			Uptime:       uptimeSvc,
			Outbox:       outbox,
			BaseURL:      cfg.BaseURL,
			EmailEnabled: emailSender.Configured(),
			Details:      detailPolicy(cfg),
		}
		uptimeDetector = &uptime.Detector{Svc: uptimeSvc, Notifier: uptimeNotifier}
		// Ingestor нужен и режиму web (через него /probe/results проводит
		// результаты выносных проб), и режиму uptime (тот же хвост у
		// локальной пробы, Runner собирает его из своих полей) — детекция
		// инцидентов и запись в CH одинаковы для обоих источников.
		uptimeIngestor = &uptime.Ingestor{
			Svc:      uptimeSvc,
			Writer:   uptimeWriter,
			OnResult: uptimeDetector.OnResult,
		}
	}

	// Планировщик — в любом процессе, у которого есть uptimeSvc, а не только
	// там, где крутится Runner.
	//
	// Постановка заданий раньше жила вторым тикером внутри Runner, а Runner
	// собирается только в uptime и all. В документированном раздельном
	// развёртывании web+ingest это значило, что очередь проверок не
	// наполнялась никогда: монитор показан включённым, состояние остаётся
	// unknown, пропуски heartbeat и истечение сертификатов не считаются, и ни
	// одной строки в логе. Выносные пробы там же простаивали — они честно
	// опрашивали пустую очередь.
	//
	// Постановка идемпотентна (ON CONFLICT DO NOTHING по (monitor_id, region))
	// и двигает last_scheduled_at только у мониторов, чьё задание реально
	// вставилось, поэтому несколько реплик с планировщиком расписание не
	// растягивают.
	if uptimeSvc != nil {
		go (&uptime.Scheduler{Svc: uptimeSvc}).Run(ctx)
		if cfg.Mode == "web" {
			// Симметрично предупреждению про оценщики: в этом процессе проверки
			// только ставятся в очередь, а исполнять их некому, кроме реплики
			// --mode=uptime или выносной пробы. Молчание тут читалось бы как
			// «аптайм работает».
			slog.Warn("uptime checks are scheduled here but NOT executed in this mode; "+
				"run a --mode=uptime (or --mode=all) replica, or register a remote probe",
				"mode", cfg.Mode)
		}
	}

	var runner *uptime.Runner
	if cfg.Mode == "uptime" || cfg.Mode == "all" {
		runner = &uptime.Runner{
			Svc:                 uptimeSvc,
			Writer:              uptimeWriter,
			Region:              cfg.LocalRegion,
			Concurrency:         cfg.UptimeConcurrency,
			OnResult:            uptimeDetector.OnResult,
			AllowPrivateTargets: cfg.SSRFAllowPrivateUptime,
		}
		go runner.Run(ctx)

		watchdog := &uptime.Watchdog{
			Svc:      uptimeSvc,
			Detector: uptimeDetector,
			Notifier: uptimeNotifier,
			Writer:   uptimeWriter,
			Region:   cfg.LocalRegion,
		}
		go watchdog.Run(ctx)

		// Оценщики регрессий производительности, правил по метрикам и регрессий
		// профилей — периодические джобы, которым нужны PG (инциденты/конфиг) и
		// общий outbox/каналы. К аптайму они отношения не имеют: они живут здесь
		// исторически, потому что тут уже собраны alertSvc, outbox и emailSender.
		//
		// Из-за этого при документированном раздельном развёртывании web+ingest
		// (оператор, которому аптайм не нужен, никогда не запустит --mode=uptime)
		// правило по метрике выглядело включённым и не вычислялось НИКОГДА —
		// ни строки в логе, ни признака в интерфейсе. Ровно тот же класс дефекта
		// уже находили у воркера доставки.
		//
		// GOTCHA_RUN_EVALUATORS позволяет включить их в любом режиме с БД. Дефолт
		// оставлен прежним намеренно: включать их автоматически везде значило бы
		// в связке web+uptime гонять двойную оценку.
		if runEvaluators(cfg) {
			startEvaluators(ctx, cfg, pg, ch, alertSvc, outbox, emailSender)
		}

		slog.Info("uptime enabled", "region", cfg.LocalRegion, "concurrency", cfg.UptimeConcurrency)
	}

	// Оценщики вне режима uptime — только по явному включению.
	if cfg.Mode != "uptime" && cfg.Mode != "all" && cfg.Mode != "probe" {
		switch {
		case runEvaluatorsExplicit(cfg):
			startEvaluators(ctx, cfg, pg, ch, alertSvc, outbox, emailSender)
			slog.Info("evaluators enabled by GOTCHA_RUN_EVALUATORS", "mode", cfg.Mode)
		default:
			// Молчать здесь нельзя: правило по метрике в интерфейсе выглядит
			// включённым, а не вычисляется. Оператор должен узнать об этом при
			// старте, а не во время инцидента.
			slog.Warn("metric/performance/profile evaluators are NOT running in this mode; "+
				"metric alert rules and regression detection will never fire — "+
				"run a --mode=uptime (or --mode=all) replica, or set GOTCHA_RUN_EVALUATORS=true here",
				"mode", cfg.Mode)
		}
	}

	var pipeline *ingest.Pipeline
	var batcher *event.Batcher
	var spanWriter *trace.SpanWriter
	var metricWriter *metric.Writer
	var profileWriter *profile.Writer
	// cardinality — ограничитель кардинальности; нужен и приёму (схлопывание),
	// и веб-слою (диагностика: что схлопнуто и примеры значений).
	var cardinality *ingest.CardinalityGuard
	// Доставка уведомлений и чистка очереди. Гейт — НЕ режим, а сам факт наличия
	// outbox: в очередь пишут контуры из разных режимов (uptime.OutboxNotifier в
	// web|uptime|all, оценщики трейсов/метрик/профилей в ingest|all), и когда
	// воркер жил только в ingest|all, инсталляция `--mode=uptime` открывала
	// инциденты, складывала задания в notification_outbox и НЕ ДОСТАВЛЯЛА ИХ
	// НИКОГДА — без единой строки в логе. Заодно не работал и janitor, поэтому
	// таблица с секретами каналов в payload росла безгранично.
	if outbox != nil {
		senders := map[string]notify.Sender{
			alert.ChannelWebhook:  &notify.WebhookSender{AllowPrivate: cfg.SSRFAllowPrivateWebhook},
			alert.ChannelTelegram: &notify.TelegramSender{},
		}
		if emailSender != nil && emailSender.Configured() {
			senders[alert.ChannelEmail] = emailSender
		} else {
			slog.Warn("GOTCHA_SMTP_HOST is not set, email alert channels are disabled")
		}
		notifyWorker := &notify.Worker{Outbox: outbox, Senders: senders}
		// Секрет канала воркер достаёт по channel_id в момент отправки: в
		// payload задачи его больше нет (см. notify.SecretResolver). Проверка
		// на nil обязательна — присваивание nil-указателя *alert.Service в
		// интерфейс дало бы НЕ-nil интерфейс и панику на первой же доставке.
		if alertSvc != nil {
			notifyWorker.Secrets = alertSvc
		}
		go notifyWorker.Run(ctx)

		// Сводки о подавленных уведомлениях. Гейт тот же, что у воркера доставки
		// (наличие outbox, а не режим процесса): потолок без сводки — молчаливая
		// потеря, а «тишина в Telegram» неотличима от «всё спокойно».
		if alertSvc != nil {
			digester := &alert.Digester{
				Svc:          alertSvc,
				Outbox:       outbox,
				BaseURL:      cfg.BaseURL,
				EmailEnabled: emailSender != nil && emailSender.Configured(),
				Details:      detailPolicy(cfg),
			}
			go digester.Run(ctx)
		}

		// Чистка notification_outbox (техдолг): доставленные/проваленные строки
		// без ретенции копятся бесконечно.
		outboxJanitor := &notify.OutboxJanitor{
			Outbox:    outbox,
			Retention: time.Duration(cfg.OutboxRetentionDays) * 24 * time.Hour,
		}
		go outboxJanitor.Run(ctx)
	}

	if cfg.Mode == "ingest" || cfg.Mode == "all" {
		batcher = event.NewBatcher(ch)
		batcher.SetMaxBufferBytes(cfg.MaxBufferBytes)
		registerWriterMetrics(&selfMetrics, "events", batcher)
		go batcher.Run()

		// Трейсинг — часть ingest-контура: транзакции приезжают тем же
		// envelope-эндпойнтом, что и ошибки, и пишутся своим батчером
		// (transactions + spans), независимым от батчера событий.
		spanWriter = trace.NewSpanWriter(ch)
		spanWriter.SetMaxBufferBytes(cfg.MaxBufferBytes)
		registerWriterMetrics(&selfMetrics, "spans", spanWriter)
		go spanWriter.Run()

		// Метрики (этап 6) — третий приёмник ingest-контура: OTLP /v1/metrics
		// пишет точки в metric_points своим батчером.
		metricWriter = metric.NewWriter(ch)
		metricWriter.SetMaxBufferBytes(cfg.MaxBufferBytes)
		registerWriterMetrics(&selfMetrics, "metrics", metricWriter)
		go metricWriter.Run()

		// Профили (этап 7) — четвёртый приёмник: Sentry-профили из envelope и
		// pprof из /profiles/pprof пишутся в profile_samples своим батчером.
		profileWriter = profile.NewWriter(ch)
		profileWriter.SetMaxBufferBytes(cfg.MaxBufferBytes)
		registerWriterMetrics(&selfMetrics, "profiles", profileWriter)
		go profileWriter.Run()

		evaluator := &alert.Evaluator{
			Svc: alertSvc, Outbox: outbox, BaseURL: cfg.BaseURL, EmailEnabled: emailSender.Configured(),
			Details: detailPolicy(cfg),
		}
		spikeWorker := &alert.Spike{
			Svc: alertSvc, Outbox: outbox, Issues: issueSvc, Events: event.NewQuery(ch), Evaluator: evaluator,
		}
		go spikeWorker.Run(ctx)

		// Детекторы производительности (план 3): находки уезжают в perf_issues
		// (PG) и алертят при первом обнаружении через тот же outbox, что и
		// алерты об ошибках. Пороги берутся из projects.perf_detector_config
		// через тот же кеш проектов, что читает transaction_sample_rate —
		// один инстанс на процесс, чтобы не держать два кеша одного и того же.
		projectCache := ingest.NewProjectCache(orgSvc)
		perfNotifier := &trace.OutboxNotifier{
			Alerts:       alertSvc,
			Outbox:       outbox,
			Pool:         pg, // perf_alert_throttle: рассылка ограничена по проекту
			BaseURL:      cfg.BaseURL,
			EmailEnabled: emailSender.Configured(),
			Details:      detailPolicy(cfg),
		}

		pipeline = ingest.NewPipeline(issueSvc, batcher)
		pipeline.SetMaxQueueBytes(cfg.MaxQueueBytes)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_pipeline_queued_tasks",
			"Tasks waiting in the ingest pipeline queue.", nil, pipeline.Queued)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_pipeline_queued_bytes",
			"Bytes held by tasks waiting in the ingest pipeline queue.", nil, pipeline.QueuedBytes)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_pipeline_queue_capacity",
			"Ingest pipeline queue capacity.", nil, pipeline.QueueCap)
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_pipeline_dropped_tasks_total",
			"Tasks dropped by the ingest pipeline: queue full, or the handler panicked.",
			nil, pipeline.Dropped)
		pipeline.Alerts = evaluator
		pipeline.Spans = spanWriter
		pipeline.Perf = trace.NewIssueService(pg)
		pipeline.PerfAlerts = perfNotifier
		pipeline.Projects = projectCache
		scrubber := ingest.NewScrubber(cfg.ScrubIP, cfg.ScrubEmail, cfg.ScrubKeys)
		scrubber.ScrubFreeText = cfg.ScrubFreeText // RA-L10: opt-in маскирование email в свободном тексте
		scrubber.SetAllowKeys(cfg.ScrubAllowKeys)  // явные исключения из fail-closed denylist
		pipeline.Scrub = scrubber
		pipeline.Start()
		ingestHandler := ingest.NewHandler(
			ingest.NewKeyCache(orgSvc), ingest.NewOrgQuota(orgSvc), pipeline, cfg.MaxEventBytes)
		// Квота транзакций — отдельный счётчик (organizations.transaction_quota
		// против org_usage.transactions_count): исчерпанный бюджет транзакций
		// не закрывает приём ошибок и наоборот.
		ingestHandler.TxQuota = ingest.NewOrgTransactionQuota(orgSvc)
		ingestHandler.Projects = projectCache
		// Метрики (этап 6): приёмник + отдельная квота метрик.
		ingestHandler.Metrics = metricWriter
		ingestHandler.MetricQuota = ingest.NewOrgMetricQuota(orgSvc)
		// Профили (этап 7): приёмник + отдельная квота профилей.
		ingestHandler.Profiles = profileWriter
		ingestHandler.ProfileQuota = ingest.NewOrgProfileQuota(orgSvc)
		ingestHandler.DropCounter = orgSvc
		ingestHandler.Scrub = scrubber // RA-5: тем же скрабером чистим атрибуты метрик
		// Ограничитель кардинальности: один экземпляр на процесс, общий для всех
		// путей приёма — иначе один и тот же проект набирал бы отдельные наборы
		// значений на Sentry-входе и на OTLP-входе.
		cardinality = ingest.NewCardinalityGuard(
			cfg.CardinalityLimit, time.Duration(cfg.CardinalityWindowSeconds)*time.Second)
		ingestHandler.Cardinality = cardinality
		// Схлопнутое обязано быть видно оператору: без счётчика «часть имён
		// пропала» неотличимо от «их и не было».
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_cardinality_collapsed_total",
			"Field values collapsed into the overflow bucket because a project hit its cardinality limit.",
			nil, cardinality.CollapsedTotal)
		ingestHandler.Register(mux)
		slog.Info("ingest enabled")
	}
	if cfg.Mode == "web" || cfg.Mode == "all" {
		authSvc := auth.NewService(pg)
		authSvc.Secure = strings.HasPrefix(cfg.BaseURL, "https://") // RA-L1: на HTTPS читать только __Host- cookie
		eventQuery := event.NewQuery(ch)
		webHandler := web.New(authSvc, orgSvc, issueSvc, eventQuery, cfg.BaseURL)
		webHandler.Alerts = alertSvc
		webHandler.Email = emailSender
		webHandler.EmailEnabled = emailSender.Configured()
		// Форма удаления ПДн должна знать, обезличиваются ли email/IP на приёме:
		// при включённом скрубинге поиск субъекта по ним не найдёт ничего.
		webHandler.ScrubIP = cfg.ScrubIP
		webHandler.ScrubEmail = cfg.ScrubEmail
		// Диагностика кардинальности: в режиме all это тот же экземпляр, что у
		// приёма. В раздельном развёртывании веб-узел его не видит — тогда
		// предупреждения доступны через /metrics и логи ingest-узла.
		webHandler.Cardinality = cardinality
		webHandler.Outbox = outbox
		webHandler.Uptime = uptimeSvc
		webHandler.UptimeWriter = uptimeWriter
		// uptimeSvc is always built above whenever cfg.Mode is "web" or
		// "all" (see the uptimeSvc/uptimeWriter block), so UptimeQuery is
		// unconditional here too — same ClickHouse handle as uptimeWriter,
		// just for reads instead of writes.
		webHandler.UptimeQuery = uptime.NewQuery(ch)
		webHandler.UptimeIngestor = uptimeIngestor
		// Perf-страницы (этап 3, план 4): агрегаты транзакций из того же CH
		// (Trace) и связанные perf-проблемы из PG (PerfIssues).
		webHandler.Trace = trace.NewQuery(ch)
		webHandler.PerfIssues = trace.NewIssueService(pg)
		// Регрессии (этап 4, план 5): список /projects/{id}/regressions читает
		// perf_regressions из PG (тот же сервис, что и оценщик выше).
		webHandler.Regressions = trace.NewRegressionService(pg)
		webHandler.Metrics = metric.NewQuery(ch)
		webHandler.MetricRules = metric.NewRuleService(pg)
		webHandler.MetricIncidents = metric.NewIncidentService(pg)
		webHandler.Profiles = profile.NewQuery(ch)
		webHandler.ProfileRegressions = profile.NewRegressionService(pg)
		webHandler.OAuth = buildRegistry(cfg)
		// Ключ подписи oauth-flow cookie выводим отдельным подключом от мастера,
		// а не берём мастер напрямую: тогда контекст HMAC-подписи cookie и
		// контекст at-rest-шифрования SSO client_secret (sha256(master) в org)
		// не делят один и тот же ключевой материал — доменное разделение (Info20).
		// Ephemeral oauth-cookie при апгрейде просто инвалидируются один раз.
		webHandler.SecretKey = deriveCookieKey(cfg.SecretKey)
		webHandler.TrustedProxies = cfg.TrustedProxies
		webHandler.RegistrationMode = cfg.RegistrationMode
		webHandler.RetentionDays = cfg.RetentionDays
		webHandler.LocalRegion = cfg.LocalRegion
		webHandler.Purger = telemetry.NewPurger(ch)
		webHandler.Register(mux)
		janitor := &auth.Janitor{Svc: authSvc}
		if orgSvc != nil {
			// Просроченные/принятые инвайты копят email приглашённых бессрочно —
			// чистим на том же тике (минимизация ПДн, 152-ФЗ ст.5 ч.7).
			janitor.Extra = append(janitor.Extra, auth.Cleanup{
				Name: "expired invites", Fn: orgSvc.PurgeExpiredInvites,
			})
		}
		go janitor.Run(ctx)
		slog.Info("web enabled")
	}

	// Таймауты обязательны: Go по умолчанию их НЕ ставит, а на этом же mux
	// висят публичные приёмные эндпойнты (DSN публичен по замыслу). Без них
	// Slowloris — медленная посылка заголовков/тела по байту — держит горутину
	// и файловый дескриптор на каждое соединение бесконечно, и тысячи таких
	// коннектов кладут приём для всех тенантов. MaxBytesReader от этого не
	// спасает (тело просто не дочитывается, соединение живёт). ReadHeaderTimeout
	// режет slow-header, ReadTimeout — slow-body, WriteTimeout — медленного
	// читателя, IdleTimeout закрывает простаивающие keep-alive.
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB заголовков — с запасом, но не безлимит
	}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Addr, "mode", cfg.Mode)
		errCh <- srv.ListenAndServe()
	}()

	drain := func() {
		if pipeline != nil {
			// Тот же бюджет, что у остальных писателей ниже: без дедлайна деградация
			// PostgreSQL держала бы shutdown до ~20 минут, и внешний
			// stop_grace_period успел бы убить процесс раньше их дренажа.
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := pipeline.Close(cctx); err != nil {
				slog.Warn("ingest pipeline drain incomplete", "error", err)
			}
			cancel()
		}
		if batcher != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := batcher.Close(cctx); err != nil {
				slog.Error("event batcher drain failed", "error", err)
			}
		}
		if spanWriter != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := spanWriter.Close(cctx); err != nil {
				slog.Error("span writer drain failed", "error", err)
			}
		}
		if metricWriter != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := metricWriter.Close(cctx); err != nil {
				slog.Error("metric writer drain failed", "error", err)
			}
		}
		if profileWriter != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := profileWriter.Close(cctx); err != nil {
				slog.Error("profile writer drain failed", "error", err)
			}
		}
		if runner != nil {
			runner.Close()
		}
		if uptimeWriter != nil {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := uptimeWriter.Close(cctx); err != nil {
				slog.Error("uptime result writer drain failed", "error", err)
			}
		}
	}

	select {
	case err := <-errCh:
		drain()
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			drain()
			return err
		}
		if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
			drain()
			return err
		}
		drain()
		return nil
	}
}

// startEvaluators запускает периодические оценщики: регрессии
// производительности, правила по метрикам и регрессии профилей. Вынесено в
// функцию, потому что запускать их надо из двух мест — режима uptime|all и
// явного GOTCHA_RUN_EVALUATORS в прочих режимах с БД.
func startEvaluators(ctx context.Context, cfg Config, pg *pgxpool.Pool, ch driver.Conn,
	alertSvc *alert.Service, outbox *notify.Outbox, emailSender *notify.EmailSender) {
	evaluator := &trace.Evaluator{
		Pool:        pg,
		Query:       trace.NewQuery(ch),
		Regressions: trace.NewRegressionService(pg),
		Notifier: &trace.RegressionNotifier{
			Alerts:       alertSvc,
			Outbox:       outbox,
			BaseURL:      cfg.BaseURL,
			EmailEnabled: emailSender.Configured(),
			Details:      detailPolicy(cfg),
		},
	}
	go evaluator.Run(ctx)

	// Оценщик пороговых алертов на метрики (этап 6, план 4) — та же ниша, что
	// и regression-evaluator: периодическая джоба на PG (правила/инциденты) +
	// CH (агрегаты метрик), алертит через общий outbox. alertSvc/outbox/
	// emailSender построены выше в этом же блоке (uptime|all).
	metricEval := &metric.Evaluator{
		Rules:     metric.NewRuleService(pg),
		Query:     metric.NewQuery(ch),
		Incidents: metric.NewIncidentService(pg),
		Notifier: &metric.MetricNotifier{
			Alerts:       alertSvc,
			Outbox:       outbox,
			BaseURL:      cfg.BaseURL,
			EmailEnabled: emailSender.Configured(),
			Details:      detailPolicy(cfg),
		},
		Interval: time.Duration(cfg.MetricEvalInterval) * time.Second,
	}
	go metricEval.Run(ctx)

	// Оценщик регрессий профилей (этап 9): рост self-CPU доли функции над
	// скользящей базой → инцидент + алерт через общий outbox. Та же ниша,
	// что regression/metric-оценщики; alertSvc/outbox/emailSender/pg/ch в scope.
	profileRegEval := &profile.RegressionEvaluator{
		Pool:        pg,
		Query:       profile.NewQuery(ch),
		Regressions: profile.NewRegressionService(pg),
		Notifier: &profile.RegressionNotifier{
			Alerts:       alertSvc,
			Outbox:       outbox,
			BaseURL:      cfg.BaseURL,
			EmailEnabled: emailSender.Configured(),
			Details:      detailPolicy(cfg),
		},
		Interval: time.Duration(cfg.ProfileEvalInterval) * time.Second,
		Config:   profile.DefaultProfileRegressionConfig(),
	}
	go profileRegEval.Run(ctx)
}

// runEvaluators — запускать ли оценщики в режиме uptime|all. Явное значение
// переменной перекрывает дефолт (например, чтобы выключить их на реплике,
// которая крутит только проверки аптайма).
// detailPolicy — политика раскрытия деталей события получателю уведомления.
// Одна на процесс и собирается из конфига: раньше каждый нотифаер получал
// голый bool и сам решал по типу канала, и правило разъезжалось от нотифаера к
// нотифаеру (их семь). Теперь решение живёт в одном типе, а поле обязано быть
// заполнено — компилятор не даст завести восьмой нотифаер, забыв про гейт.
func detailPolicy(cfg Config) alert.DetailPolicy {
	return alert.NewDetailPolicy(cfg.BaseURL, cfg.TrustedRecipients, cfg.ExternalChannelDetails)
}

// logDetailPolicy — один раз при старте пишет, кому уйдут детали события.
//
// Правило перестало быть очевидным из одного флага: раньше «email — всегда,
// остальные — по GOTCHA_EXTERNAL_CHANNEL_DETAILS», теперь решает домен
// получателя. Оператор, у которого почта на другом домене, обнаружил бы
// пропажу деталей только по отсутствию текста в письме — а из лога видно
// сразу, какой список действует и чего в нём не хватает.
func logDetailPolicy(cfg Config) {
	switch {
	case cfg.ExternalChannelDetails:
		slog.Info("alert details: sent to every recipient (GOTCHA_EXTERNAL_CHANNEL_DETAILS=true)")
	default:
		slog.Info("alert details: sent only to trusted recipients",
			"instance_host", cfg.BaseURL,
			"trusted_recipients", cfg.TrustedRecipients,
			"hint", "add organization mail/webhook domains via GOTCHA_TRUSTED_RECIPIENTS")
	}
}

func runEvaluators(cfg Config) bool {
	if cfg.RunEvaluators != nil {
		return *cfg.RunEvaluators
	}
	return true
}

// runEvaluatorsExplicit — включены ли оценщики ЯВНО. В режимах без аптайма
// дефолта нет: включать их самим значило бы в связке web+uptime гонять двойную
// оценку, а молча не включать — то, что уже привело к «правило включено и не
// срабатывает никогда».
func runEvaluatorsExplicit(cfg Config) bool {
	return cfg.RunEvaluators != nil && *cfg.RunEvaluators
}
