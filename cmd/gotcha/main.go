package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("gotcha failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig(os.Getenv, os.Args[1:])
	if err != nil {
		return err
	}
	if cfg.SecretKey == "insecure-dev-secret" {
		slog.Warn("GOTCHA_SECRET_KEY is not set, using insecure dev default")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Выносная проба — отдельная ветка до всего остального: ей не нужны ни
	// Postgres, ни ClickHouse, ни HTTP-сервер (и входящих портов у неё в
	// чужом регионе может не быть вовсе). Только исходящие запросы к центру,
	// пока не придёт сигнал.
	if cfg.Mode == "probe" {
		probe := &uptime.ProbeClient{
			ServerURL:   cfg.ServerURL,
			Token:       cfg.ProbeToken,
			Concurrency: cfg.UptimeConcurrency,
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
		if err := db.MigratePG(cfg.PostgresDSN); err != nil {
			return err
		}
		if err := db.MigrateCH(cfg.ClickHouseDSN); err != nil {
			return err
		}
		if err := db.ApplyRetention(ctx, ch, cfg.RetentionDays); err != nil {
			return err
		}
		return db.ApplySpanRetention(ctx, ch, cfg.SpanRetentionDays)
	})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler(pg, ch))

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
		issueSvc = issue.NewService(pg)
		alertSvc = alert.NewService(pg)
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
			Svc:         uptimeSvc,
			Writer:      uptimeWriter,
			Region:      cfg.LocalRegion,
			Concurrency: cfg.UptimeConcurrency,
			OnResult:    uptimeDetector.OnResult,
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
				Alerts:       alertSvc,
				Outbox:       outbox,
				BaseURL:      cfg.BaseURL,
				EmailEnabled: emailSender.Configured(),
			},
		}
		go evaluator.Run(ctx)

		slog.Info("uptime enabled", "region", cfg.LocalRegion, "concurrency", cfg.UptimeConcurrency)
	}

	var pipeline *ingest.Pipeline
	var batcher *event.Batcher
	var spanWriter *trace.SpanWriter
	if cfg.Mode == "ingest" || cfg.Mode == "all" {
		batcher = event.NewBatcher(ch)
		go batcher.Run()

		// Трейсинг — часть ingest-контура: транзакции приезжают тем же
		// envelope-эндпойнтом, что и ошибки, и пишутся своим батчером
		// (transactions + spans), независимым от батчера событий.
		spanWriter = trace.NewSpanWriter(ch)
		go spanWriter.Run()

		// Алертинг (план 6) — часть ingest-контура: правила/каналы срабатывают
		// на события, которые проходят через этот же пайплайн.
		senders := map[string]notify.Sender{
			alert.ChannelWebhook:  &notify.WebhookSender{},
			alert.ChannelTelegram: &notify.TelegramSender{},
		}
		if emailSender.Configured() {
			senders[alert.ChannelEmail] = emailSender
		} else {
			slog.Warn("GOTCHA_SMTP_HOST is not set, email alert channels are disabled")
		}
		notifyWorker := &notify.Worker{Outbox: outbox, Senders: senders}
		go notifyWorker.Run(ctx)

		evaluator := &alert.Evaluator{
			Svc: alertSvc, Outbox: outbox, BaseURL: cfg.BaseURL, EmailEnabled: emailSender.Configured(),
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
		}

		pipeline = ingest.NewPipeline(issueSvc, batcher)
		pipeline.Alerts = evaluator
		pipeline.Spans = spanWriter
		pipeline.Perf = trace.NewIssueService(pg)
		pipeline.PerfAlerts = perfNotifier
		pipeline.Projects = projectCache
		pipeline.Start()
		ingestHandler := ingest.NewHandler(
			ingest.NewKeyCache(orgSvc), ingest.NewOrgQuota(orgSvc), pipeline, cfg.MaxEventBytes)
		// Квота транзакций — отдельный счётчик (organizations.transaction_quota
		// против org_usage.transactions_count): исчерпанный бюджет транзакций
		// не закрывает приём ошибок и наоборот.
		ingestHandler.TxQuota = ingest.NewOrgTransactionQuota(orgSvc)
		ingestHandler.Projects = projectCache
		ingestHandler.Register(mux)
		slog.Info("ingest enabled")
	}
	if cfg.Mode == "web" || cfg.Mode == "all" {
		authSvc := auth.NewService(pg)
		eventQuery := event.NewQuery(ch)
		webHandler := web.New(authSvc, orgSvc, issueSvc, eventQuery, cfg.BaseURL)
		webHandler.Alerts = alertSvc
		webHandler.Email = emailSender
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
		webHandler.OAuth = buildRegistry(cfg)
		webHandler.SecretKey = cfg.SecretKey
		webHandler.LocalRegion = cfg.LocalRegion
		webHandler.Register(mux)
		go (&auth.Janitor{Svc: authSvc}).Run(ctx)
		slog.Info("web enabled")
	}

	srv := &http.Server{Addr: cfg.Addr, Handler: mux}
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
