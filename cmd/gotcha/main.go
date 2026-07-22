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
	"gitflic.ru/otezvikentiy/gotcha/internal/telemetry"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/version"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
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

func run() error {
	if versionRequested(os.Args[1:]) {
		fmt.Println("gotcha", version.String())
		return nil
	}
	cfg, err := loadConfig(os.Getenv, os.Args[1:])
	if err != nil {
		return err
	}
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
	oauth.SetAllowPrivateHosts(cfg.SSRFAllowPrivate)

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
			AllowPrivateTargets: cfg.SSRFAllowPrivate,
		}
		probe.Run(ctx)
		slog.Info("probe stopped")
		return nil
	}

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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler(pg, ch))
	mux.HandleFunc("GET /version", versionHandler())

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
			Alerts:          alertSvc,
			Uptime:          uptimeSvc,
			Outbox:          outbox,
			BaseURL:         cfg.BaseURL,
			EmailEnabled:    emailSender.Configured(),
			ExternalDetails: cfg.ExternalChannelDetails,
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

	var runner *uptime.Runner
	if cfg.Mode == "uptime" || cfg.Mode == "all" {
		runner = &uptime.Runner{
			Svc:                 uptimeSvc,
			Writer:              uptimeWriter,
			Region:              cfg.LocalRegion,
			Concurrency:         cfg.UptimeConcurrency,
			OnResult:            uptimeDetector.OnResult,
			AllowPrivateTargets: cfg.SSRFAllowPrivate,
		}
		go runner.Run(ctx)

		watchdog := &uptime.Watchdog{
			Svc:      uptimeSvc,
			Detector: uptimeDetector,
			Notifier: uptimeNotifier,
			Region:   cfg.LocalRegion,
		}
		go watchdog.Run(ctx)

		// Оценщик регрессий производительности (этап 4, план 4) живёт в том же
		// процессе, что и uptime-watchdog: оба — периодические джобы, которым
		// нужны PG (инциденты/конфиг) и общий outbox/каналы. Он обходит топ-K
		// целей каждого проекта и шлёт алерт об открытии/закрытии регрессии через
		// тот же outbox. alertSvc/outbox/emailSender гарантированно построены
		// выше в этом же блоке (uptime|all), даже если процесс не крутит web.
		evaluator := &trace.Evaluator{
			Pool:        pg,
			Query:       trace.NewQuery(ch),
			Regressions: trace.NewRegressionService(pg),
			Notifier: &trace.RegressionNotifier{
				Alerts:          alertSvc,
				Outbox:          outbox,
				BaseURL:         cfg.BaseURL,
				EmailEnabled:    emailSender.Configured(),
				ExternalDetails: cfg.ExternalChannelDetails,
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
				Alerts:          alertSvc,
				Outbox:          outbox,
				BaseURL:         cfg.BaseURL,
				EmailEnabled:    emailSender.Configured(),
				ExternalDetails: cfg.ExternalChannelDetails,
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
				Alerts:          alertSvc,
				Outbox:          outbox,
				BaseURL:         cfg.BaseURL,
				EmailEnabled:    emailSender.Configured(),
				ExternalDetails: cfg.ExternalChannelDetails,
			},
			Interval: time.Duration(cfg.ProfileEvalInterval) * time.Second,
			Config:   profile.DefaultProfileRegressionConfig(),
		}
		go profileRegEval.Run(ctx)

		slog.Info("uptime enabled", "region", cfg.LocalRegion, "concurrency", cfg.UptimeConcurrency)
	}

	var pipeline *ingest.Pipeline
	var batcher *event.Batcher
	var spanWriter *trace.SpanWriter
	var metricWriter *metric.Writer
	var profileWriter *profile.Writer
	if cfg.Mode == "ingest" || cfg.Mode == "all" {
		batcher = event.NewBatcher(ch)
		go batcher.Run()

		// Трейсинг — часть ingest-контура: транзакции приезжают тем же
		// envelope-эндпойнтом, что и ошибки, и пишутся своим батчером
		// (transactions + spans), независимым от батчера событий.
		spanWriter = trace.NewSpanWriter(ch)
		go spanWriter.Run()

		// Метрики (этап 6) — третий приёмник ingest-контура: OTLP /v1/metrics
		// пишет точки в metric_points своим батчером.
		metricWriter = metric.NewWriter(ch)
		go metricWriter.Run()

		// Профили (этап 7) — четвёртый приёмник: Sentry-профили из envelope и
		// pprof из /profiles/pprof пишутся в profile_samples своим батчером.
		profileWriter = profile.NewWriter(ch)
		go profileWriter.Run()

		// Алертинг (план 6) — часть ingest-контура: правила/каналы срабатывают
		// на события, которые проходят через этот же пайплайн.
		senders := map[string]notify.Sender{
			alert.ChannelWebhook:  &notify.WebhookSender{AllowPrivate: cfg.SSRFAllowPrivate},
			alert.ChannelTelegram: &notify.TelegramSender{},
		}
		if emailSender.Configured() {
			senders[alert.ChannelEmail] = emailSender
		} else {
			slog.Warn("GOTCHA_SMTP_HOST is not set, email alert channels are disabled")
		}
		notifyWorker := &notify.Worker{Outbox: outbox, Senders: senders}
		go notifyWorker.Run(ctx)

		// Чистка notification_outbox (техдолг): доставленные/проваленные строки
		// несут секреты каналов в payload и без ретенции копятся бесконечно.
		outboxJanitor := &notify.OutboxJanitor{
			Outbox:    outbox,
			Retention: time.Duration(cfg.OutboxRetentionDays) * 24 * time.Hour,
		}
		go outboxJanitor.Run(ctx)

		evaluator := &alert.Evaluator{
			Svc: alertSvc, Outbox: outbox, BaseURL: cfg.BaseURL, EmailEnabled: emailSender.Configured(),
			ExternalDetails: cfg.ExternalChannelDetails,
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
			Alerts:          alertSvc,
			Outbox:          outbox,
			Pool:            pg, // perf_alert_throttle: рассылка ограничена по проекту
			BaseURL:         cfg.BaseURL,
			EmailEnabled:    emailSender.Configured(),
			ExternalDetails: cfg.ExternalChannelDetails,
		}

		pipeline = ingest.NewPipeline(issueSvc, batcher)
		pipeline.Alerts = evaluator
		pipeline.Spans = spanWriter
		pipeline.Perf = trace.NewIssueService(pg)
		pipeline.PerfAlerts = perfNotifier
		pipeline.Projects = projectCache
		scrubber := ingest.NewScrubber(cfg.ScrubIP, cfg.ScrubEmail, cfg.ScrubKeys)
		scrubber.ScrubFreeText = cfg.ScrubFreeText // RA-L10: opt-in маскирование email в свободном тексте
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
			pipeline.Close()
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
