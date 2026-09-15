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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/db"
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
	"gitflic.ru/otezvikentiy/gotcha/internal/memlimit"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/oauth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/scrub"
	"gitflic.ru/otezvikentiy/gotcha/internal/secretbox"
	"gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/telemetry"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
	"gitflic.ru/otezvikentiy/gotcha/internal/version"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	// Пакет, не apk add: базовый образ (alpine) tzdata не несёт, без него
	// time.LoadLocation падает на любом поясе, кроме UTC. Цена — ~450 КБ.
	_ "time/tzdata"
)

func main() {
	// До всего остального: не поднимает ни соединений, ни сервера, только
	// спрашивает уже работающий процесс.
	if url, ok := healthcheckRequested(os.Args[1:], os.Getenv); ok {
		os.Exit(runHealthcheck(url))
	}
	if code, ok := dispatchSubcommand(os.Args[1:], os.Getenv); ok {
		os.Exit(code)
	}
	if err := run(); err != nil {
		slog.Error("gotcha failed", "error", err)
		os.Exit(1)
	}
}

func versionRequested(args []string) bool {
	for _, a := range args {
		if a == "--version" || a == "version" {
			return true
		}
	}
	return false
}

// HMAC-SHA256(master, label) — доменное разделение, не совпадает с ключом
// at-rest-шифрования SSO (sha256(master) в org). Пустой master → пустой ключ.
func deriveCookieKey(master string) string {
	if master == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte("gotcha:oauth-cookie-mac:v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

// Нераспознанное непустое значение — отказ старта, не тихий откат к дефолту:
// иначе оператор тратит цикл на отладку логгера вместо инцидента.
func setupLogging(level, format string) error {
	if err := validateLogging(level, format); err != nil {
		return err
	}
	opts := &slog.HandlerOptions{Level: logLevels[level]}
	var h slog.Handler
	switch format {
	case "", "text":
		h = slog.NewTextHandler(os.Stderr, opts)
	case "json":
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
	return nil
}

type writerStats interface {
	Buffered() int64
	Dropped() int64
	InsertFailures() int64
}

func registerWriterMetrics(r *selfmetrics.Registry, name string, w writerStats) {
	lbl := map[string]string{"writer": name}
	r.AddInt(selfmetrics.Gauge, "gotcha_writer_buffered_rows",
		"Rows buffered in memory, waiting to be written to ClickHouse.", lbl, w.Buffered)
	r.AddInt(selfmetrics.Counter, "gotcha_writer_dropped_rows_total",
		"Rows dropped because the buffer overflowed; these are lost for good.", lbl, w.Dropped)
	r.AddInt(selfmetrics.Counter, "gotcha_writer_insert_failures_total",
		"Failed batch inserts. The batch is retried, so this is not data loss by itself.", lbl, w.InsertFailures)
}

// Единственный источник истины для gotcha_secret_key_insecure и Handler.SecretKeyInsecure:
// повторять сравнение на своей стороне нельзя, метрика и предупреждение разойдутся.
func secretKeyInsecure(secretKey string) bool {
	return secretKey == devSecretKey
}

func registerSecretKeyMetric(r *selfmetrics.Registry, secretKey string) {
	r.AddInt(selfmetrics.Gauge, "gotcha_secret_key_insecure",
		"1 when GOTCHA_SECRET_KEY is unset (the public dev default): channel secrets, "+
			"SSO client_secret and monitor headers are stored in PostgreSQL as plaintext "+
			"in modes that touch them.",
		nil, func() int64 {
			if secretKeyInsecure(secretKey) {
				return 1
			}
			return 0
		})
}

// Доля потолка кучи, отдаваемая СУММЕ писательских буферов; остаток — на
// HTTP-приём, разбор JSON, клиент PostgreSQL, сам рантайм поверх GC-паузы.
const autoBufferSafeShare = 0.6

// SpanWriter применяет ОДИН потолок к ДВУМ независимым буферам (txBuf и
// spanBuf) — считается за 2 единицы из 6 (остальные писатели по 1).
const autoBufferCapUnits = 6

// 0, если heapLimitBytes <= 0 — вызывающий оставляет flat-дефолт пакета
// (256 МиБ): шесть таких буферов дали бы больше heap-потолка docker-compose.yml.
func autoMaxBufferBytes(heapLimitBytes int64) int64 {
	if heapLimitBytes <= 0 {
		return 0
	}
	return int64(float64(heapLimitBytes) * autoBufferSafeShare / autoBufferCapUnits)
}

// Доля ОСТАТКА кучи сверх писательских буферов (1-autoBufferSafeShare), отданная
// суммарному весу одновременных разборов профиля (см. ingest.Handler.
// SetProfileDecodeBudgetBytes) — тот же порядок допущения, что и у самих
// буферов, не отдельная система координат. Остаток остатка — HTTP-приём,
// разбор JSON, клиент PostgreSQL, рантайм.
const profileDecodeBudgetShareOfResidual = 0.3

// 0, если heapLimitBytes <= 0 — бюджет остаётся неограниченным, как и у
// autoMaxBufferBytes в этом случае.
func autoProfileDecodeBudgetBytes(heapLimitBytes int64) int64 {
	if heapLimitBytes <= 0 {
		return 0
	}
	residual := float64(heapLimitBytes) * (1 - autoBufferSafeShare)
	return int64(residual * profileDecodeBudgetShareOfResidual)
}

// Явный GOTCHA_MAX_WRITER_BUFFER_BYTES всегда побеждает автодефолт от потолка кучи.
func effectiveMaxBufferBytes(cfgMaxBufferBytes, heapLimitBytes int64) int64 {
	if cfgMaxBufferBytes != 0 {
		return cfgMaxBufferBytes
	}
	return autoMaxBufferBytes(heapLimitBytes)
}

// Не завязан на GOTCHA_EXPORT_RETENTION_HOURS: статус "истекла" должен остаться
// видимым на странице, а не исчезнуть в тот же тик, что и файл.
const exportMinRowRetention = 30 * 24 * time.Hour

// Не короче ttl файла: иначе при GOTCHA_EXPORT_RETENTION_HOURS больше 30 суток
// PurgeRows снёс бы строку ЖИВОЙ заявки раньше её собственного срока.
func exportRowRetention(ttl time.Duration) time.Duration {
	if ttl > exportMinRowRetention {
		return ttl
	}
	return exportMinRowRetention
}

func waitGroupWithTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	stopped := make(chan struct{})
	go func() {
		wg.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Строго больше ingestsignal.FinalFlushTimeout: Recorder.Run возвращается
// только после финального Flush, иначе ожидание проигрывает гонку флашу.
const ingestSignalsDrainWindow = 5 * time.Second

// Без ожидания Run гонится с закрытием пула, и накопленное с последнего тика
// теряется молча.
func drainIngestSignals(wg *sync.WaitGroup) {
	if !waitGroupWithTimeout(wg, ingestSignalsDrainWindow) {
		slog.Warn("ingest signals recorder did not stop within the shutdown window")
	}
}

// По таймауту closeFn продолжает работать в горутине-сироте (утечка до её
// завершения) — не терять процесс по SIGKILL дороже, чем не дождаться его.
func closeBounded(closeFn func(), timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		closeFn()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func drainParallel(branches ...func()) {
	var dwg sync.WaitGroup
	dwg.Add(len(branches))
	for _, b := range branches {
		go func() {
			defer dwg.Done()
			b()
		}()
	}
	dwg.Wait()
}

// Единственный источник истины для этой тройки режимов: дублирование списком
// в разных местах уже породило панику на --mode=uptime.
func commonServicesEnabled(mode string) bool {
	return mode == "ingest" || mode == "web" || mode == "all"
}

func exportModeServesFiles(mode string) bool {
	return mode == "web" || mode == "all"
}

// dirOK И режим, отдающий файл — оба обязательны: --mode=ingest не строит
// webHandler, --mode=uptime оставляет issueSvc nil (воркер упал бы паникой).
func exportsWiringEnabled(mode string, dirOK bool) bool {
	return dirOK && exportModeServesFiles(mode)
}

// 0700, не 0755: каталог выгрузок — единственное место, где ПДн ложатся на
// диск целым каталогом, не только файлом (файлы внутри уже 0600).
const exportDirMode = 0o700

func ensureExportDir(dir string) error {
	return os.MkdirAll(dir, exportDirMode)
}

// os.MkdirAll на существующем каталоге возвращает nil независимо от прав —
// без этой пробы запись падала бы на каждой заявке молча.
func exportDirWritable(dir string) error {
	// Имя пробы уникально на каждый вызов: несколько реплик на общем томе не
	// должны удалить чужой временный файл.
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	probe := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(probe)
		return err
	}
	return os.Remove(probe)
}

// Отсутствие лимита — не ошибка: продукт не выдумывает потолок за оператора,
// но и не молчит об этом.
func applyMemoryLimit() int64 {
	limit, err := memlimit.Apply()
	switch {
	case errors.Is(err, memlimit.ErrNoLimit):
		slog.Info("no container memory limit found; heap ceiling not set " +
			"(buffers grow until the host runs out — set mem_limit or GOMEMLIMIT)")
		return 0
	case err != nil:
		slog.Warn("cannot derive heap ceiling from container limit", "error", err)
		return 0
	}
	slog.Info("heap ceiling set", "bytes", limit)
	return limit
}

// Лучшим усилием — невалидное значение здесь не отказ старта, ту же
// переменную позже провалит validateLogging внутри loadConfigChecked.
func earlyLogEnv(getenv func(string) string) (level, format string) {
	return strings.ToLower(strings.TrimSpace(getenv("GOTCHA_LOGGING_LEVEL"))),
		strings.ToLower(strings.TrimSpace(getenv("GOTCHA_LOGGING_FORMAT")))
}

// Применяет логирование ДО loadConfigChecked — она сама зовёт slog.Warn,
// и эти предупреждения обязаны идти уже в выбранном формате/уровне.
func loadConfigWithLogging(getenv func(string) string, environ func() []string, args []string) (Config, error) {
	earlyLevel, earlyFormat := earlyLogEnv(getenv)
	_ = setupLogging(earlyLevel, earlyFormat)
	cfg, err := loadConfigChecked(getenv, environ, args)
	if err != nil {
		return Config{}, err
	}
	if err := setupLogging(cfg.LogLevel, cfg.LogFormat); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func run() error {
	if versionRequested(os.Args[1:]) {
		fmt.Println("gotcha", version.String())
		return nil
	}
	cfg, err := loadConfigWithLogging(os.Getenv, os.Environ, os.Args[1:])
	if err != nil {
		return err
	}
	if cfg.MigrateForcePG >= 0 || cfg.MigrateForceCH >= 0 {
		if cfg.MigrateForcePG >= 0 {
			err = db.ForcePG(cfg.PostgresDSN, uint(cfg.MigrateForcePG))
		} else {
			err = db.ForceCH(cfg.ClickHouseDSN, uint(cfg.MigrateForceCH))
		}
		if err != nil {
			return err
		}
		slog.Info("dirty flag cleared; force does not finish the migration — "+
			"verify the schema before restarting (see /docs/upgrade)",
			"pg_version", cfg.MigrateForcePG, "ch_version", cfg.MigrateForceCH)
		return nil
	}
	// Go-рантайм лимит cgroup не читает, а буферы растут по замыслу, дожидаясь
	// возвращения хранилища: без потолка OOM-killer ядра теряет всё буферизованное.
	memLimitBytes := applyMemoryLimit()
	if cfg.SecretKey == devSecretKey {
		slog.Warn("GOTCHA_SECRET_KEY is not set — using insecure dev default (fine for localhost only)")
	}
	if !isLocalBaseURL(cfg.BaseURL) && !strings.HasPrefix(cfg.BaseURL, "https://") {
		slog.Warn("GOTCHA_BASE_URL is non-local plain HTTP — session cookies ride unencrypted; enable TLS (https)")
	}
	// SSRF-safe по тому же флагу, что webhook/uptime — приватные адреса режутся,
	// если оператор не разрешил их явно (внутренний IdP). До любого OAuth-обмена.
	oauth.SetAllowPrivateHosts(cfg.SSRFAllowPrivateOIDC)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	return runServer(ctx, cfg, memLimitBytes)
}

// Отделена от run(): построение процесса по cfg.Mode и его штатное завершение
// проверяются тестами напрямую, без сигналов ОС и разбора os.Args/os.Environ.
func runServer(ctx context.Context, cfg Config, memLimitBytes int64) error {
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

	chWrite, err := db.NewClickHouseWriter(ctx, cfg.ClickHouseDSN)
	if err != nil {
		return err
	}
	defer chWrite.Close()

	retention, err := applyMigrations(ctx, cfg, pg, ch)
	if err != nil {
		return err
	}

	if cfg.MigrateOnly {
		slog.Info("migrations applied, exiting (--migrate-only)")
		return nil
	}

	// Реестр ленив — хранит функции и опрашивает их на каждом скрапе, поэтому
	// порядок регистрации значения не имеет.
	var selfMetrics selfmetrics.Registry
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_memory_limit_bytes",
		"Heap ceiling in effect (GOMEMLIMIT), derived from the container limit; 0 when unlimited.",
		nil, func() int64 { return memLimitBytes })
	selfMetrics.Add(selfmetrics.Gauge, "gotcha_build_info",
		"Build metadata; the value is always 1, the version lives in the label.",
		map[string]string{
			"version": version.String(),
			"mode":    cfg.Mode,
			// stamped=false: сборка мимо make, версия из исходников — не
			// сверить с задеплоенным git-тегом.
			"stamped": strconv.FormatBool(version.Stamped()),
		},
		func() float64 { return 1 })
	registerSecretKeyMetric(&selfMetrics, cfg.SecretKey)
	registerRetentionMetrics(&selfMetrics, retention)
	// Не зависит от cfg.Mode: i18n.T зовётся и из web, и из notify независимо
	// от режима процесса.
	for _, locale := range i18n.SupportedLocales() {
		for _, stage := range i18n.MissingKeyStages() {
			selfMetrics.AddInt(selfmetrics.Counter, "gotcha_i18n_missing_key_total",
				"Translation key lookups that missed: stage=\"fallback\" found the key in the default locale, stage=\"missing\" found it nowhere and the raw key was shown. Rendering never fails on a miss — see internal/i18n/catalog.go.",
				map[string]string{"locale": locale, "stage": string(stage)},
				func() int64 { return i18n.MissingKeyTotal(locale, stage) })
		}
	}
	storagePollers := registerStorageMetrics(&selfMetrics, chDiskSource{conn: ch})
	go storagePollers.Run(ctx)
	pgUsedBytes := registerUsedBytesMetric(&selfMetrics, "postgres", pgUsedBytesSource{pool: pg})
	go pgUsedBytes.Run(ctx)

	var orgSvc *org.Service
	var issueSvc *issue.Service
	var alertSvc *alert.Service
	var emailSender *notify.EmailSender
	var outbox *notify.Outbox
	if commonServicesEnabled(cfg.Mode) {
		orgSvc = org.NewService(pg, cfg.DefaultEventQuota)
		orgSvc.SetQuotaDefaults(cfg.DefaultTransactionQuota, cfg.DefaultMetricQuota, cfg.DefaultProfileQuota, cfg.DefaultLogQuota)
		// Кольцо at-rest ключей раздаётся не здесь, а централизованно в
		// wireSecretRing ниже, после того как построены все сервисы.
		issueSvc = issue.NewService(pg)
		alertSvc = alert.NewService(pg)
		alertSvc.SetBudget(time.Duration(cfg.AlertBudgetWindowSeconds)*time.Second, cfg.AlertBudgetLimit)
		emailSender = notify.NewEmailSender(notify.EmailConfig{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort,
			User: cfg.SMTPUser, Password: cfg.SMTPPassword, From: cfg.SMTPFrom,
			RequireTLS: cfg.SMTPRequireTLS,
		})
		outbox = notify.NewOutbox(pg)
	}

	// Один инстанс на процесс: кеш снапшота графа зависимостей общий на тик,
	// а не пересобирается отдельно на каждого потребителя.
	depSuppressor := depsuppress.NewSuppressor(pg)
	settleGrace := time.Duration(cfg.DependencySettleSeconds) * time.Second

	// Один Grouper на процесс поверх того же depSuppressor.
	groupStore := incidentgroup.NewStore(pg)
	grouper := &incidentgroup.Grouper{Pool: pg, Store: groupStore, Roots: depSuppressor}

	// web монтирует heartbeat-роут даже без --mode=uptime; в --mode=all оба
	// контура делят один ResultWriter — одна очередь вставок в ClickHouse.
	var uptimeSvc *uptime.Service
	var uptimeWriter *uptime.ResultWriter
	var uptimeDetector *uptime.Detector
	var uptimeNotifier *uptime.OutboxNotifier
	var uptimeIngestor *uptime.Ingestor
	if cfg.Mode == "web" || cfg.Mode == "uptime" || cfg.Mode == "all" {
		uptimeSvc = uptime.NewService(pg)
		// Обязано совпадать с регионом, который лизит Runner ниже, иначе монитор
		// попал бы в регион, который не проверяет никто.
		uptimeSvc.LocalRegion = cfg.LocalRegion
		uptimeWriter = uptime.NewResultWriter(chWrite)
		registerWriterMetrics(&selfMetrics, "uptime_results", uptimeWriter)
		go uptimeWriter.Run()

		// Уведомления детектора идут через тот же Outbox — строим здесь тоже,
		// если uptime работает без ingest/web.
		if alertSvc == nil {
			alertSvc = alert.NewService(pg)
			alertSvc.SetBudget(time.Duration(cfg.AlertBudgetWindowSeconds)*time.Second, cfg.AlertBudgetLimit)
		}
		if outbox == nil {
			outbox = notify.NewOutbox(pg)
		}
		if emailSender == nil {
			emailSender = notify.NewEmailSender(notify.EmailConfig{
				Host: cfg.SMTPHost, Port: cfg.SMTPPort,
				User: cfg.SMTPUser, Password: cfg.SMTPPassword, From: cfg.SMTPFrom,
				RequireTLS: cfg.SMTPRequireTLS,
			})
		}
		uptimeNotifier = &uptime.OutboxNotifier{
			Alerts:       alertSvc,
			Uptime:       uptimeSvc,
			Outbox:       outbox,
			BaseURL:      cfg.BaseURL,
			EmailEnabled: emailSender.Configured(),
			Details:      detailPolicy(cfg),
			Locale:       i18n.Locale{Code: cfg.Locale},
			DepCounts:    depSuppressor,
			Projects:     escalation.OrgProjectNamer{Svc: orgSvc},
		}
		uptimeDetector = &uptime.Detector{
			Svc: uptimeSvc, Notifier: uptimeNotifier,
			Dep: depSuppressor, SettleGrace: settleGrace,
			IncidentGroups: grouper,
			Pool:           pg,
		}
		// Общий и для web (проводит результаты выносных проб через
		// /probe/results), и для uptime (тот же хвост у локальной пробы).
		uptimeIngestor = &uptime.Ingestor{
			Svc:      uptimeSvc,
			Writer:   uptimeWriter,
			OnResult: uptimeDetector.OnResult,
		}
	}

	// Обязательно ДО подъёма HTTP-слушателя и ДО бэкфилла ниже: итог ротации
	// обязан появиться в логе того же рестарта, а не следующего.
	if cfg.SecretKey != devSecretKey {
		if _, err := wireSecretRing(cfg.SecretKey, cfg.SecretKeyPrev, orgSvc, alertSvc, uptimeSvc); err != nil {
			return err
		}
		rewrapAllSecrets(ctx, orgSvc, alertSvc, uptimeSvc)
	}

	// В любом процессе с uptimeSvc, не только там, где крутится Runner — иначе
	// в раздельном развёртывании web+ingest очередь проверок не наполняется.
	if uptimeSvc != nil {
		uptimeScheduler := &uptime.Scheduler{Svc: uptimeSvc}
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_uptime_scheduler_last_tick_timestamp_seconds",
			"Unix time of the last completed uptime check scheduling pass. Stale value means due checks are not being enqueued.",
			nil, uptimeScheduler.LastTickUnix)
		selfMetrics.Add(selfmetrics.Gauge, "gotcha_uptime_scheduler_tick_duration_seconds",
			"Duration of the last uptime check scheduling call. Approaching scheduleBudget means PostgreSQL is not keeping up.",
			nil, uptimeScheduler.LastTickSeconds)
		go uptimeScheduler.Run(ctx)
		if cfg.Mode == "web" {
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
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_uptime_runner_last_tick_timestamp_seconds",
			"Unix time of the last completed uptime check lease pass. Stale value means active checks are not being leased/executed.",
			nil, runner.LastTickUnix)
		selfMetrics.Add(selfmetrics.Gauge, "gotcha_uptime_runner_tick_duration_seconds",
			"Duration of the last uptime check lease call. Approaching leaseBudget means PostgreSQL is not keeping up.",
			nil, runner.LastTickSeconds)
		go runner.Run(ctx)

		watchdog := &uptime.Watchdog{
			Svc:      uptimeSvc,
			Detector: uptimeDetector,
			Notifier: uptimeNotifier,
			Writer:   uptimeWriter,
			Region:   cfg.LocalRegion,
			Maint:    uptimeSvc,
		}
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_uptime_watchdog_last_tick_timestamp_seconds",
			"Unix time of the last completed heartbeat/reminder pass. Stale value means missed heartbeats and incident reminders are not being evaluated.",
			nil, watchdog.LastTickUnix)
		selfMetrics.Add(selfmetrics.Gauge, "gotcha_uptime_watchdog_tick_duration_seconds",
			"Duration of the last heartbeat/reminder pass. Approaching the interval means the watchdog stops keeping up.",
			nil, watchdog.LastTickSeconds)
		go watchdog.Run(ctx)

		// Дефолт не включает их автоматически везде — в связке web+uptime это
		// гоняло бы двойную оценку.
		if runEvaluators(cfg) {
			startEvaluators(ctx, cfg, pg, ch, alertSvc, outbox, emailSender, &selfMetrics, depSuppressor, grouper, settleGrace, orgSvc)
		}

		slog.Info("uptime enabled", "region", cfg.LocalRegion, "concurrency", cfg.UptimeConcurrency)
	}

	// Оценщики вне режима uptime — только по явному включению.
	if cfg.Mode != "uptime" && cfg.Mode != "all" && cfg.Mode != "probe" {
		switch {
		case runEvaluatorsExplicit(cfg):
			startEvaluators(ctx, cfg, pg, ch, alertSvc, outbox, emailSender, &selfMetrics, depSuppressor, grouper, settleGrace, orgSvc)
			slog.Info("evaluators enabled by GOTCHA_EVALUATORS_ENABLED", "mode", cfg.Mode)
		default:
			// Гейтит ШЕСТЬ циклов: startEvaluators поднимает и host.Evaluator, и
			// slo.Evaluator, и escalation.Scheduler, не только правила по метрикам.
			slog.Warn(evaluatorsDisabledWarning, "mode", cfg.Mode)
		}
	}

	var pipeline *ingest.Pipeline
	var batcher *event.Batcher
	var spanWriter *trace.SpanWriter
	var metricWriter *metric.Writer
	var profileWriter *profile.Writer
	var logWriter *log.Writer
	var writerDrops *ingest.WriterDropAttributor
	// Объявлены здесь, а не через := в if-блоках ниже: newRootMux собирается
	// один раз, после того как оба хендлера построены (или остались nil).
	var ingestHandler *ingest.Handler
	var webHandler *web.Handler
	// nil остаётся, если каталог выгрузок не удалось создать — тогда раздел
	// выключен, а не фатален для процесса.
	var exportStore *export.Store
	// drain() ждёт эту группу с ограниченным окном, чтобы заявка, прерванная
	// посреди сборки, успела дописать release() в БД до убийства процесса.
	var exportWorkersWG sync.WaitGroup
	// То же ожидание, что exportWorkersWG выше, но для накопителя сигналов
	// приёма: финальный Flush обязан успеть дописать в PG до pg.Close().
	var ingestSignalsWG sync.WaitGroup
	// Нужен и приёму (схлопывание), и веб-слою (диагностика: что схлопнуто).
	var cardinality *ingest.CardinalityGuard
	// Объявлен здесь, не := внутри ingest-блока, чтобы веб-слой мог взять его
	// для Forget при удалении хоста.
	var hostToucher *host.Toucher
	// Гейт — НЕ режим, а сам факт наличия outbox: в очередь пишут контуры из
	// разных режимов (uptime-нотифаер в web|uptime|all, оценщики в ingest|all).
	var notifyDirect *notify.Direct
	if outbox != nil {
		senders := map[string]notify.Sender{
			alert.ChannelWebhook:  &notify.WebhookSender{AllowPrivate: cfg.SSRFAllowPrivateWebhook},
			alert.ChannelTelegram: &notify.TelegramSender{BaseURL: cfg.TelegramAPIBase, AllowPrivate: cfg.SSRFAllowPrivateTelegram},
		}
		if emailSender != nil && emailSender.Configured() {
			senders[alert.ChannelEmail] = emailSender
		} else {
			slog.Warn("GOTCHA_SMTP_HOST is not set, email alert channels are disabled")
		}
		notifyDirect = &notify.Direct{Senders: senders}
		if alertSvc != nil {
			notifyDirect.Secrets = alertSvc
		}
		// Наблюдение за доставкой. До него «алерт не пришёл» диагностировался
		// грепом логов, причём janitor через семь дней удалял улику.
		notifyStats := &notify.Stats{}
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_notify_sent_total",
			"Notifications delivered successfully.", nil, notifyStats.Sent)
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_notify_failed_total",
			"Notifications given up on after exhausting retries.", nil, notifyStats.Failed)
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_notify_retried_total",
			"Delivery attempts that failed and were rescheduled.", nil, notifyStats.Retried)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_notify_queue_depth",
			"Notifications waiting to be delivered.", nil, notifyStats.Pending)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_notify_queue_failed",
			"Notifications in the queue that will not be retried again.", nil, notifyStats.FailedJobs)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_notify_queue_oldest_seconds",
			"Age of the oldest notification still waiting; the number that tells a quiet queue from a stuck one.",
			nil, notifyStats.OldestPendingAgeSeconds)
		go notifyStats.RunSnapshots(ctx, outbox)

		notifyWorker := &notify.Worker{
			Outbox:      outbox,
			Senders:     senders,
			Concurrency: cfg.NotifyConcurrency,
			Stats:       notifyStats,
		}
		// Проверка на nil обязательна: присваивание nil-указателя *alert.Service
		// в интерфейс дало бы НЕ-nil интерфейс и панику на первой же доставке.
		if alertSvc != nil {
			notifyWorker.Secrets = alertSvc
		}
		go notifyWorker.Run(ctx)

		// Гейт тот же, что у воркера доставки (наличие outbox, не режим
		// процесса): потолок без сводки — молчаливая потеря.
		if alertSvc != nil {
			digester := &alert.Digester{
				Svc:          alertSvc,
				Outbox:       outbox,
				BaseURL:      cfg.BaseURL,
				EmailEnabled: emailSender != nil && emailSender.Configured(),
				Details:      detailPolicy(cfg),
				Locale:       i18n.Locale{Code: cfg.Locale},
			}
			go digester.Run(ctx)
			selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_alert_digest_last_tick_timestamp_seconds",
				"Unix time of the last completed suppressed-alert digest pass. Stale value means digest summaries are not being sent.",
				nil, digester.LastTickUnix)
			selfMetrics.Add(selfmetrics.Gauge, "gotcha_alert_digest_tick_duration_seconds",
				"Duration of the last suppressed-alert digest pass. Approaching the interval means PostgreSQL or notification channels are not keeping up.",
				nil, digester.LastTickSeconds)
			selfMetrics.AddInt(selfmetrics.Counter, "gotcha_alert_digest_suppressed_lost_total",
				"Suppressed alerts whose digest summary could neither be delivered nor requeued for retry — permanently lost.",
				nil, digester.LostSuppressed)
			selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_alert_digest_skipped_batches",
				"Number of suppressed-alert batches skipped in the last tick because the tick budget ran out. Non-zero means the digester cannot keep up with the batch count.",
				nil, digester.LastTickSkippedBatches)
		}

		// Доставленные/проваленные строки без ретенции копятся бесконечно.
		outboxJanitor := &notify.OutboxJanitor{
			Outbox:    outbox,
			Retention: time.Duration(cfg.OutboxRetentionDays) * 24 * time.Hour,
		}
		go outboxJanitor.Run(ctx)

		// Каталог трогаем только когда режим им может воспользоваться — иначе на
		// ingest/uptime/probe-реплике это давало бы бесполезный warn при старте.
		exportDirOK := false
		if exportModeServesFiles(cfg.Mode) {
			if err := ensureExportDir(cfg.ExportDir); err != nil {
				// Не фатально: продукт работает дальше без раздела выгрузок,
				// страница отвечает 404 (webHandler.Exports остаётся nil ниже).
				slog.Warn("export: export directory is unavailable, the exports section is disabled", "dir", cfg.ExportDir, "err", err)
			} else if err := exportDirWritable(cfg.ExportDir); err != nil {
				slog.Warn("export: export directory is not writable by this process, the exports section is disabled", "dir", cfg.ExportDir, "err", err)
			} else {
				exportDirOK = true
			}
		}
		if exportsWiringEnabled(cfg.Mode, exportDirOK) {
			exportStore = export.NewStore(pg)
			exportCfg := export.Config{
				Dir:        cfg.ExportDir,
				TTL:        time.Duration(cfg.ExportTTLHours) * time.Hour,
				MaxRows:    cfg.ExportMaxRows,
				MaxBytes:   cfg.ExportMaxBytes,
				DiskBudget: cfg.ExportDiskBudgetBytes,
			}
			// В отличие от каталога (недоступность диска — условие среды, не
			// ошибка оператора), кривая конфигурация — отказ старта.
			if err := exportCfg.Validate(); err != nil {
				return fmt.Errorf("export: configuration: %w", err)
			}
			// nil вместо отправителя, если почта не настроена — иначе
			// EmailSender.Send диалит пустой host:port на каждой заявке.
			var exportMailer export.Mailer
			if emailSender != nil && emailSender.Configured() {
				exportMailer = emailSender
			}
			exportWorker := &export.Worker{
				Store:  exportStore,
				Pool:   pg,
				Issues: export.NewIssueSource(issueSvc, cfg.BaseURL),
				Events: export.NewEventSource(event.NewQuery(ch), issueSvc),
				Cfg:    exportCfg,
				Notify: export.NewMailNotifier(exportMailer, exportStore, cfg.BaseURL, i18n.Locale{Code: cfg.Locale}),
			}
			exportWorkersWG.Add(1)
			go func() {
				defer exportWorkersWG.Done()
				exportWorker.Run(ctx)
			}()

			exportJanitor := &export.Janitor{
				Store:        exportStore,
				Pool:         pg,
				Dir:          cfg.ExportDir,
				RowRetention: exportRowRetention(exportCfg.TTL),
			}
			exportWorkersWG.Add(1)
			go func() {
				defer exportWorkersWG.Done()
				exportJanitor.Run(ctx)
			}()
			selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_export_janitor_last_tick_timestamp_seconds",
				"Unix time of the last completed export janitor pass. Stale value means expired exports, purge queue history, and orphan files are not being cleaned up.",
				nil, exportJanitor.LastTickUnix)
			selfMetrics.Add(selfmetrics.Gauge, "gotcha_export_janitor_tick_duration_seconds",
				"Duration of the last export janitor pass. Approaching the interval means PostgreSQL or the export directory is falling behind.",
				nil, exportJanitor.LastTickSeconds)

			exportStats := &export.Stats{}
			selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_export_queue_depth",
				"Export requests queued or being built.", nil, exportStats.Pending)
			selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_export_queue_failed",
				"Export requests in the queue that exhausted retries and will not be retried again.", nil, exportStats.FailedJobs)
			selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_export_queue_oldest_seconds",
				"Age of the oldest export request still queued or being built; the number that tells a quiet queue from a stuck one.",
				nil, exportStats.OldestPendingAgeSeconds)
			go exportStats.RunSnapshots(ctx, exportStore)

			exportUsedBytes := registerUsedBytesMetric(&selfMetrics, "exports", exportDirUsedBytesSource{dir: cfg.ExportDir})
			go exportUsedBytes.Run(ctx)
		}
	}

	// Каждое правило живёт сроком СВОЕЙ сущности (telemetry.entityRules), не
	// общим GOTCHA_EVENT_RETENTION_DAYS. Проход берёт advisory-лок, на занятом молча уступает.
	entityRetention := telemetry.Retentions{
		Events:      time.Duration(cfg.RetentionDays) * 24 * time.Hour,
		Metrics:     time.Duration(cfg.MetricRetentionDays) * 24 * time.Hour,
		Profiles:    time.Duration(cfg.ProfileRetentionDays) * 24 * time.Hour,
		Incidents:   time.Duration(cfg.IncidentRetentionDays) * 24 * time.Hour,
		Deployments: time.Duration(cfg.DeployRetentionDays) * 24 * time.Hour,
	}
	if pg != nil && entityRetention.Any() {
		entityJanitor := &telemetry.EntityJanitor{
			Pool:      pg,
			Retention: entityRetention,
			// Удаление хоста каскадит его host_incidents, включая открытые —
			// Retirer перед удалением батча закрывает их и уведомляет о снятии.
			PreDelete: map[string]telemetry.PreDeleteHook{
				"hosts": (&host.Retirer{
					Hosts:     host.NewStore(pg),
					Incidents: host.NewIncidentService(pg),
					Notifier: &host.HostNotifier{
						Alerts:       alertSvc,
						Outbox:       outbox,
						BaseURL:      cfg.BaseURL,
						EmailEnabled: emailSender.Configured(),
						Details:      detailPolicy(cfg),
						Locale:       i18n.Locale{Code: cfg.Locale},
						// StepNotifier перезагружает инцидент по ID.
						Incidents: host.NewIncidentService(pg),
						Hosts:     host.NewStore(pg),
						Settings:  host.NewSettingsService(pg),
						Pool:      pg,
						Projects:  escalation.OrgProjectNamer{Svc: orgSvc},
					},
				}).Retire,
			},
		}
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_entities_purged_total",
			"Rows deleted from PostgreSQL because they outlived the retention of the data they describe.",
			nil, entityJanitor.Purged)
		go entityJanitor.Run(ctx)
	}

	// Заявку ставит та же транзакция, что удаляет проект; здесь — исполнитель и
	// суточная сверка сирот на случай, когда заявки не появилось вообще.
	if pg != nil && ch != nil {
		purgeWorker := &telemetry.PurgeWorker{
			Queue:             telemetry.NewPurgeQueue(pg),
			Purger:            telemetry.NewPurger(ch),
			Conn:              ch,
			ReconcileInterval: time.Duration(cfg.PurgeReconcileHours) * time.Hour,
		}
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_purge_queue_depth",
			"Projects whose ClickHouse telemetry is still waiting to be deleted.",
			nil, purgeWorker.Depth)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_purge_queue_oldest_seconds",
			"Age of the oldest pending project purge request, in seconds.",
			nil, purgeWorker.OldestSeconds)
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_projects_purged_total",
			"Projects whose ClickHouse telemetry has been deleted after the project was removed.",
			nil, purgeWorker.Purged)
		go purgeWorker.Run(ctx)
	}

	if cfg.Mode == "ingest" || cfg.Mode == "all" {
		maxBufBytes := effectiveMaxBufferBytes(cfg.MaxBufferBytes, memLimitBytes)
		if cfg.MaxBufferBytes == 0 && maxBufBytes > 0 {
			slog.Info("writer buffer cap auto-derived from detected heap ceiling",
				"bytes", maxBufBytes, "heap_ceiling_bytes", memLimitBytes)
		}

		batcher = event.NewBatcher(chWrite)
		batcher.SetMaxBufferBytes(maxBufBytes)
		registerWriterMetrics(&selfMetrics, "events", batcher)
		go batcher.Run()

		spanWriter = trace.NewSpanWriter(chWrite)
		spanWriter.SetMaxBufferBytes(maxBufBytes)
		registerWriterMetrics(&selfMetrics, "spans", spanWriter)
		go spanWriter.Run()

		metricWriter = metric.NewWriter(chWrite)
		metricWriter.SetMaxBufferBytes(maxBufBytes)
		registerWriterMetrics(&selfMetrics, "metrics", metricWriter)
		// Имя хоста — в логе, не в метке (кардинальность).
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_metric_points_clock_skew_total",
			"Metric points that arrived with a timestamp from the future and were clamped to the receive time. Growth means a sender's clock runs ahead of the server's.",
			nil, metric.ClockSkewPoints)
		go metricWriter.Run()

		profileWriter = profile.NewWriter(chWrite)
		profileWriter.SetMaxBufferBytes(maxBufBytes)
		registerWriterMetrics(&selfMetrics, "profiles", profileWriter)
		go profileWriter.Run()

		logWriter = log.NewWriter(chWrite)
		logWriter.SetMaxBufferBytes(maxBufBytes)
		registerWriterMetrics(&selfMetrics, "logs", logWriter)
		go logWriter.Run()

		evaluator := &alert.Evaluator{
			Svc: alertSvc, Outbox: outbox, BaseURL: cfg.BaseURL, EmailEnabled: emailSender.Configured(),
			Details: detailPolicy(cfg),
			Locale:  i18n.Locale{Code: cfg.Locale},
			// Подавляет issue-алерты ДО claimThrottle/claimBudget в OnIssue.
			Maint:    uptime.NewService(pg),
			Projects: escalation.OrgProjectNamer{Svc: orgSvc},
		}
		spikeWorker := &alert.Spike{
			Svc: alertSvc, Outbox: outbox, Issues: issueSvc, Events: event.NewQuery(ch), Evaluator: evaluator,
		}
		go spikeWorker.Run(ctx)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_alert_spike_last_tick_timestamp_seconds",
			"Unix time of the last completed spike-rule evaluation pass. Stale value means spike alerts are not being evaluated.",
			nil, spikeWorker.LastTickUnix)
		selfMetrics.Add(selfmetrics.Gauge, "gotcha_alert_spike_tick_duration_seconds",
			"Duration of the last spike-rule evaluation pass. Approaching the interval means ClickHouse is not keeping up.",
			nil, spikeWorker.LastTickSeconds)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_alert_spike_skipped_rules",
			"Number of enabled spike rules skipped in the last tick because the tick budget ran out. Non-zero means the worker cannot keep up with the rule count.",
			nil, spikeWorker.LastTickSkippedRules)

		// Один инстанс на процесс, тот же кеш, что читает transaction_sample_rate.
		projectCache := ingest.NewProjectCache(orgSvc)
		perfNotifier := &trace.OutboxNotifier{
			Alerts:       alertSvc,
			Outbox:       outbox,
			Pool:         pg, // perf_alert_throttle: рассылка ограничена по проекту
			BaseURL:      cfg.BaseURL,
			EmailEnabled: emailSender.Configured(),
			Details:      detailPolicy(cfg),
			Locale:       i18n.Locale{Code: cfg.Locale},
		}

		pipeline = ingest.NewPipeline(issueSvc, batcher)
		pipeline.SetMaxQueueBytes(cfg.MaxQueueBytes)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_pipeline_queue_depth",
			"Tasks waiting in the ingest pipeline queue.", nil, pipeline.Queued)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_pipeline_queue_bytes",
			"Bytes held by tasks waiting in the ingest pipeline queue.", nil, pipeline.QueuedBytes)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_pipeline_queue_capacity",
			"Ingest pipeline queue capacity.", nil, pipeline.QueueCap)
		// По метрике на причину: переполнение очереди лечится размером очереди
		// и числом воркеров, отказ хранилища — не лечится ничем из этого.
		for _, reason := range ingest.DropReasons() {
			selfMetrics.AddInt(selfmetrics.Counter, "gotcha_pipeline_dropped_tasks_total",
				"Tasks dropped by the ingest pipeline. These are lost for good; the reason label says why.",
				map[string]string{"reason": string(reason)},
				func() int64 { return pipeline.DroppedBy(reason) })
		}
		// Без этой пары "503 потому что воркеры ждут" и "воркеры просто отстают"
		// выглядят одинаково по одной только gotcha_pipeline_queue_depth.
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_pipeline_backpressure_waits_total",
			"How many times an ingest worker waited for room in a saturated write buffer before writing.",
			nil, pipeline.BackpressureWaits)
		selfMetrics.Add(selfmetrics.Counter, "gotcha_pipeline_backpressure_wait_seconds_total",
			"Total time ingest workers spent waiting for room in a saturated write buffer.",
			nil, pipeline.BackpressureWaitSeconds)
		pipeline.Alerts = evaluator
		pipeline.Spans = spanWriter
		pipeline.Perf = trace.NewIssueService(pg)
		pipeline.PerfAlerts = perfNotifier
		// Окно обслуживания подавляет только notify в recordFinding, детекция в
		// perf_issues продолжает работать как обычно.
		pipeline.Maint = uptime.NewService(pg)
		pipeline.Projects = projectCache
		scrubber := scrub.NewScrubber(cfg.ScrubIP, cfg.ScrubEmail, cfg.ScrubKeys)
		scrubber.ScrubFreeText = cfg.ScrubFreeText // opt-in маскирование email в свободном тексте
		scrubber.SetAllowKeys(cfg.ScrubAllowKeys)  // явные исключения из fail-closed denylist
		pipeline.Scrub = scrubber
		// Pipeline агрегирует дропы per-org и сливает пачкой по тику, не пишет
		// в БД на каждый дроп — покрывает дропы после того, как grant списал квоту.
		pipeline.DropCounter = orgSvc
		// Дропы буфера писателя — тот же класс потери, другой слой; сливаются
		// тем же 60с-флашем в org_usage.dropped_*.
		batcher.SetDropSink(pipeline.CountDroppedEvents)
		spanWriter.SetDropSink(pipeline.CountDroppedTransactions)
		// Спан не квота — задваивать dropped_transactions нельзя (см. SetDropSink
		// выше); видимость только через журнал, своей колонки в org_usage у спанов нет.
		spanWriter.SetSpanDropSink(func(orgID, n int64) {
			slog.Warn("trace: spans dropped from buffer under load", "org_id", orgID, "n", n)
		})
		// Метрики/профили/логи несут только project_id — резолв в org_id и запись
		// в org_usage откладываются на собственный тикер, вне писательского mu.
		writerDrops = ingest.NewWriterDropAttributor(projectCache, orgSvc, orgSvc)
		go writerDrops.Run()
		metricWriter.SetDropSink(writerDrops.CountDroppedMetrics)
		profileWriter.SetDropSink(writerDrops.CountDroppedProfiles)
		logWriter.SetDropSink(writerDrops.CountDroppedLogs)
		pipeline.Start()
		ingestHandler = ingest.NewHandler(
			ingest.NewKeyCache(orgSvc), ingest.NewOrgQuota(orgSvc), pipeline, cfg.MaxEventBytes)
		// burst = 2×лимит — та же пропорция, что у прежней захардкоженной пары
		// 500/1000. 0 выключает.
		ingestHandler.SetRateLimit(time.Now, float64(cfg.IngestRateLimit), 2*float64(cfg.IngestRateLimit))
		// По клиентскому IP, до аутентификации DSN; burst = 2×лимит, как у дефолтной пары 2000/4000.
		ingestHandler.SetPreAuthRateLimit(time.Now, float64(cfg.PreAuthRateLimit), 2*float64(cfg.PreAuthRateLimit))
		// Только touchUnverifiedSignal; burst = 5×лимит, как у дефолтной пары 2/10.
		ingestHandler.SetSignalTouchRateLimit(time.Now, float64(cfg.SignalTouchRateLimit), 5*float64(cfg.SignalTouchRateLimit))
		ingestHandler.SetProfileDecodeBudgetBytes(autoProfileDecodeBudgetBytes(memLimitBytes))
		// Отдельный счётчик от org_usage.transactions_count: исчерпанный бюджет
		// транзакций не закрывает приём ошибок и наоборот.
		ingestHandler.TxQuota = ingest.NewOrgTransactionQuota(orgSvc)
		ingestHandler.Projects = projectCache
		ingestHandler.Metrics = metricWriter
		ingestHandler.MetricQuota = ingest.NewOrgMetricQuota(orgSvc)
		// Троттлинг 1/мин на (project, host): приём шлёт точки чаще, чем имеет
		// смысл писать в PG; потолок карты — защита от кардинального мусора.
		hostToucher = host.NewToucher(host.NewStore(pg), time.Minute, 65536)
		ingestHandler.Hosts = hostToucher
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_host_registration_failures_total",
			"Failed background upserts of the host registry. While this grows, host last_seen is stale and silence alerts may be false.",
			nil, hostToucher.UpsertFailures)
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_host_registrations_rejected_total",
			"New host names dropped because a project hit the per-project host ceiling.",
			nil, hostToucher.RejectedNames)
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_host_registrations_scope_skipped_total",
			"Metric exports carrying host.* attributes from a key type that may not register hosts. The export is accepted; only host registration is skipped.",
			nil, ingestHandler.HostScopeSkipped)
		ingestHandler.Profiles = profileWriter
		ingestHandler.ProfileQuota = ingest.NewOrgProfileQuota(orgSvc)
		ingestHandler.Logs = logWriter
		ingestHandler.LogQuota = ingest.NewOrgLogQuota(orgSvc)
		ingestHandler.Deploy = deploy.NewStore(pg)
		ingestHandler.DropCounter = orgSvc
		// По метрике на причину: "ключа нет вовсе" и "ключ чужого проекта"
		// требуют разных действий оператора, общий счётчик их не различал бы.
		for _, reason := range ingest.KeyRejectReasons() {
			selfMetrics.AddInt(selfmetrics.Counter, "gotcha_ingest_key_rejections_total",
				"Ingest requests rejected during key authentication, before quotas. The reason label says why.",
				map[string]string{"reason": string(reason)},
				func() int64 { return ingestHandler.KeyRejectedBy(reason) })
		}
		// Обе метрики намеренно сосуществуют: gotcha_ingest_key_rejections_total
		// детализирует отказ по ключу, эта — причину со вторым измерением (сигналом).
		for _, p := range ingest.IngestRejectionPairs() {
			selfMetrics.AddInt(selfmetrics.Counter, "gotcha_ingest_rejected_total",
				"Ingest requests rejected, by broad reason and telemetry signal. reason=\"key_revoked\" is reserved for future use (see ingest.IngestRejectReason) and never appears here today.",
				map[string]string{"reason": string(p.Reason), "signal": string(p.Signal)},
				func() int64 { return ingestHandler.RejectedBy(p.Reason, p.Signal) })
		}
		// Профиль принят (200/202), не отвергнут — parser говорит, какой из двух
		// декодеров срезал часть сэмплов/кадров по капу.
		for _, p := range ingest.ProfileParsers() {
			selfMetrics.AddInt(selfmetrics.Counter, "gotcha_ingest_profile_truncated_total",
				"Profiles accepted but truncated by a decode-time cap; parser says which path. Reason lives only in the warn-level log line, not in this metric.",
				map[string]string{"parser": string(p)},
				func() int64 { return ingestHandler.ProfileTruncatedBy(p) })
		}
		// ВРЕМЕННАЯ: исчезает вместе с алиасами в 2.0 — задокументировано в
		// self-monitoring и CHANGELOG, удалять без релиза-предупреждения нельзя.
		for _, p := range ingest.DeprecatedPaths() {
			selfMetrics.AddInt(selfmetrics.Counter, "gotcha_ingest_deprecated_path_total",
				"Requests that arrived on a deprecated ingest path, kept working as an alias until 2.0. TEMPORARY: this metric is removed together with the aliases.",
				map[string]string{"path": string(p)},
				func() int64 { return ingestHandler.DeprecatedPathHits(p) })
		}
		ingestHandler.Scrub = scrubber
		// Один экземпляр на процесс, общий для всех путей приёма — иначе один и
		// тот же проект набирал бы отдельные наборы значений на разных входах.
		cardinality = ingest.NewCardinalityGuard(
			cfg.CardinalityLimit, time.Duration(cfg.CardinalityWindowSeconds)*time.Second)
		ingestHandler.Cardinality = cardinality
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_cardinality_collapsed_total",
			"Field values collapsed into the overflow bucket because a project hit its cardinality limit.",
			nil, cardinality.CollapsedTotal)
		selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_cardinality_tracked_values",
			"Distinct field values the cardinality guard is remembering right now.",
			nil, cardinality.TrackedValues)
		// Аккумулятор, не запись на каждый Touch: путь неаутентифицированный,
		// запись в PG на каждый отказ была бы усилителем нагрузки.
		ingestSignals := ingestsignal.NewRecorder(ingestsignal.NewStore(pg))
		ingestHandler.Signals = ingestSignals
		ingestSignalsWG.Add(1)
		go func() {
			defer ingestSignalsWG.Done()
			ingestSignals.Run(ctx)
		}()
		slog.Info("ingest enabled")
	}
	if cfg.Mode == "web" || cfg.Mode == "all" {
		authSvc := auth.NewService(pg)
		authSvc.Secure = strings.HasPrefix(cfg.BaseURL, "https://") // на HTTPS читать только __Host- cookie
		eventQuery := event.NewQuery(ch)
		webHandler = web.New(authSvc, orgSvc, issueSvc, eventQuery, cfg.BaseURL)
		webHandler.Alerts = alertSvc
		webHandler.Email = emailSender
		webHandler.EmailEnabled = emailSender.Configured()
		// Форма удаления ПДн должна знать, обезличиваются ли email/IP на приёме:
		// при включённом скрубинге поиск субъекта по ним не найдёт ничего.
		webHandler.ScrubIP = cfg.ScrubIP
		webHandler.ScrubEmail = cfg.ScrubEmail
		// exportStore остаётся nil, если каталог выгрузок недоступен — тогда
		// webHandler.Exports тоже nil, и страница отвечает 404, а не падает.
		webHandler.Exports = exportStore
		webHandler.ExportDir = cfg.ExportDir
		// В режиме all — тот же экземпляр, что у приёма. В раздельном
		// развёртывании веб-узел его не видит: предупреждения — через ingest-узел.
		webHandler.Cardinality = cardinality
		webHandler.Outbox = outbox
		webHandler.NotifyDirect = notifyDirect
		webHandler.NotifyLocale = i18n.Locale{Code: cfg.Locale}
		webHandler.Uptime = uptimeSvc
		webHandler.UptimeWriter = uptimeWriter
		webHandler.UptimeQuery = uptime.NewQuery(ch)
		webHandler.UptimeIngestor = uptimeIngestor
		webHandler.Trace = trace.NewQuery(ch)
		webHandler.PerfIssues = trace.NewIssueService(pg)
		webHandler.Regressions = trace.NewRegressionService(pg)
		webHandler.Metrics = metric.NewQuery(ch)
		webHandler.MetricRules = metric.NewRuleService(pg)
		webHandler.MetricIncidents = metric.NewIncidentService(pg)
		webHandler.Hosts = host.NewStore(pg)
		webHandler.HostIncidents = host.NewIncidentService(pg)
		webHandler.HostSettings = host.NewSettingsService(pg)
		webHandler.HostOverrides = host.NewHostOverrideService(pg)
		webHandler.GroupThresholds = host.NewGroupThresholdService(pg)
		// В чистом web-режиме HostForget остаётся настоящим nil-интерфейсом,
		// а не typed-nil, на котором Forget запаниковал бы.
		if hostToucher != nil {
			webHandler.HostForget = hostToucher
		}
		webHandler.LogQuery = log.NewQuery(ch)
		webHandler.LogRetentionDays = cfg.LogRetentionDays
		webHandler.LogFilters = logfilter.NewStore(pg)
		webHandler.Deploy = deploy.NewStore(pg)
		webHandler.Signals = ingestsignal.NewStore(pg)
		webHandler.SLO = slo.NewStore(pg)
		webHandler.SLOProviders = slo.Providers(trace.NewQuery(ch), uptime.NewQuery(ch), uptime.NewService(pg), cfg.RetentionDays)
		webHandler.EscalationPolicy = escalation.NewPolicyStore(pg)
		webHandler.AlertDeps = depsuppress.NewStore(pg)
		webHandler.IncidentGroups = groupStore
		// Та же величина, что settleGrace для depSuppressor/Detector/Scheduler —
		// экран показывает фактически действующее значение, не догадку.
		webHandler.SuppressionGrace = settleGrace
		webHandler.Profiles = profile.NewQuery(ch)
		webHandler.ProfileRegressions = profile.NewRegressionService(pg)
		webHandler.OAuth = buildRegistry(cfg)
		webHandler.SecretKey = deriveCookieKey(cfg.SecretKey)
		webHandler.SecretKeyInsecure = secretKeyInsecure(cfg.SecretKey)
		webHandler.TrustedProxies = cfg.TrustedProxies
		webHandler.RegistrationMode = cfg.RegistrationMode
		webHandler.HSTSHeader = web.HSTSHeaderValue(
			cfg.HSTSEnabled, cfg.HSTSMaxAgeSeconds, cfg.HSTSIncludeSubDomains, cfg.HSTSPreload)
		webHandler.RetentionDays = cfg.RetentionDays
		webHandler.SpanRetentionDays = cfg.SpanRetentionDays
		webHandler.LocalRegion = cfg.LocalRegion
		webHandler.Purger = telemetry.NewPurger(ch)
		// Порог лимитера — отдельно от New(), чтобы массовое обновление парка за
		// одним IP не упиралось в дефолт, рассчитанный на штучные установки.
		webHandler.AgentDistDir = cfg.AgentDistDir
		webHandler.SetAgentDistRateLimit(cfg.AgentDistRatePerMin)
		selfMetrics.AddInt(selfmetrics.Counter, "gotcha_web_cross_origin_rejected_total",
			"POST requests rejected because Origin/Referer did not match GOTCHA_BASE_URL.",
			nil, webHandler.CrossOriginRejected)
		// /uptime/hb/{token} — публичная ссылка, её регулярно дёргают не люди
		// (unfurl-боты, антивирусные прокси). Не в счёт живости, но и не невидимо.
		for _, reason := range web.HeartbeatIgnoreReasons() {
			selfMetrics.AddInt(selfmetrics.Counter, "gotcha_uptime_heartbeat_ignored_total",
				"Heartbeat pings received but NOT counted as monitor liveness. The reason label says why (prefetch/preview header or a known link-preview bot User-Agent).",
				map[string]string{"reason": string(reason)},
				func() int64 { return web.HeartbeatIgnoredBy(reason) })
		}
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

	mux := newRootMux(rootDeps{
		pg:            pg,
		ch:            ch,
		selfMetrics:   &selfMetrics,
		ingestHandler: ingestHandler,
		webHandler:    webHandler,
	})
	srv := newServer(&cfg, mux)
	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Addr, "mode", cfg.Mode)
		errCh <- srv.ListenAndServe()
	}()

	// Независимые цепочки ждутся ПАРАЛЛЕЛЬНО: бюджет самой длинной (~60с) должен
	// оставаться с запасом до 90с stop_grace_period в docker-compose.yml.
	drain := func() {
		var branches []func()

		// Порядок внутри цепочки не декоративный: pipeline форвардит в batcher/
		// spanWriter — закрыть их раньше значило бы писать в закрытый writer.
		if pipeline != nil || batcher != nil || spanWriter != nil {
			branches = append(branches, func() {
				if pipeline != nil {
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
			})
		}
		if metricWriter != nil || profileWriter != nil {
			branches = append(branches, func() {
				if metricWriter != nil {
					cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					if err := metricWriter.Close(cctx); err != nil {
						slog.Error("metric writer drain failed", "error", err)
					}
					cancel()
				}
				if profileWriter != nil {
					cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					if err := profileWriter.Close(cctx); err != nil {
						slog.Error("profile writer drain failed", "error", err)
					}
					cancel()
				}
				// После Close писателей: последний emitDrops уже добавил в агрегат,
				// закрытие сливает его в PG, а не роняет на выходе из процесса.
				if writerDrops != nil {
					writerDrops.Close()
				}
			})
		}
		if logWriter != nil {
			branches = append(branches, func() {
				cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := logWriter.Close(cctx); err != nil {
					slog.Error("log writer drain failed", "error", err)
				}
			})
		}
		branches = append(branches, func() {
			drainIngestSignals(&ingestSignalsWG)
		})
		if runner != nil || uptimeWriter != nil {
			branches = append(branches, func() {
				if runner != nil {
					// Runner.Close() без ctx — ограничиваем на месте вызова.
					if !closeBounded(runner.Close, 10*time.Second) {
						slog.Warn("uptime runner did not stop within the shutdown window")
					}
				}
				if uptimeWriter != nil {
					cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if err := uptimeWriter.Close(cctx); err != nil {
						slog.Error("uptime result writer drain failed", "error", err)
					}
				}
			})
		}
		// Ждём именно ЗАВЕРШЕНИЯ горутин, не только отмены ctx: без этого
		// release() мог бы не успеть дописать строку в PG до убийства процесса.
		branches = append(branches, func() {
			if !waitGroupWithTimeout(&exportWorkersWG, 5*time.Second) {
				slog.Warn("export worker/janitor did not stop within the shutdown window")
			}
		})

		drainParallel(branches...)
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

// Вызывающий обязан звать только при cfg.SecretKey != devSecretKey — здесь
// это не перепроверяется. nil-сервис пропускается молча (не построен в режиме).
func wireSecretRing(current, previous string, orgSvc *org.Service, alertSvc *alert.Service, uptimeSvc *uptime.Service) (secretbox.Keyring, error) {
	ring, err := secretbox.NewKeyring(current, previous)
	if err != nil {
		return secretbox.Keyring{}, fmt.Errorf("build secretbox keyring: %w", err)
	}
	// previous_key_id логируется только пока ротация идёт, current_key_id — всегда.
	slog.Info("secretbox keyring ready", "current_key_id", ring.CurrentID(),
		"previous_key_id", ring.PreviousID(),
		"rotation_in_progress", previous != "")
	if orgSvc != nil {
		orgSvc.SetKeyring(ring)
	}
	if alertSvc != nil {
		alertSvc.SetKeyring(ring)
	}
	if uptimeSvc != nil {
		uptimeSvc.SetKeyring(ring)
	}
	return ring, nil
}

// Сигнатура без возврата ошибки намеренно: отказ прохода не должен уметь
// ронять старт, и эту границу нельзя случайно нарушить последующей правкой.
func rewrapAllSecrets(ctx context.Context, orgSvc *org.Service, alertSvc *alert.Service, uptimeSvc *uptime.Service) {
	if orgSvc != nil {
		if n, err := orgSvc.RewrapSecrets(ctx); err != nil {
			slog.Warn("rewrap org sso secrets failed", "err", err)
		} else if n > 0 {
			slog.Info("rewrapped org sso secrets", "rows", n)
		}
	}
	if alertSvc != nil {
		if n, err := alertSvc.RewrapSecrets(ctx); err != nil {
			slog.Warn("rewrap alert channel secrets failed", "err", err)
		} else if n > 0 {
			slog.Info("rewrapped alert channel secrets", "rows", n)
		}
	}
	if uptimeSvc != nil {
		if n, err := uptimeSvc.RewrapSecrets(ctx); err != nil {
			slog.Warn("rewrap monitor header secrets failed", "err", err)
		} else if n > 0 {
			slog.Info("rewrapped monitor header secrets", "rows", n)
		}
	}
}

func startEvaluators(ctx context.Context, cfg Config, pg *pgxpool.Pool, ch driver.Conn,
	alertSvc *alert.Service, outbox *notify.Outbox, emailSender *notify.EmailSender,
	selfMetrics *selfmetrics.Registry, dep *depsuppress.Suppressor, grouper *incidentgroup.Grouper,
	settleGrace time.Duration, orgSvc *org.Service) {
	// uptimeSvc из run() не переиспользуем — startEvaluators зовётся и в режимах
	// без него; uptime.NewService(pg) для окон обслуживания требует только пул.
	maint := uptime.NewService(pg)

	policyStore := escalation.NewPolicyStore(pg)

	evaluator := &trace.Evaluator{
		Pool:        pg,
		Query:       trace.NewQuery(ch),
		Regressions: trace.NewRegressionService(pg),
		Maint:       maint,
		Policy:      policyStore,
		Notifier: &trace.RegressionNotifier{
			Alerts:       alertSvc,
			Outbox:       outbox,
			BaseURL:      cfg.BaseURL,
			EmailEnabled: emailSender.Configured(),
			Details:      detailPolicy(cfg),
			Locale:       i18n.Locale{Code: cfg.Locale},
			// StepNotifier перезагружает регрессию по ID.
			Regressions: trace.NewRegressionService(pg),
			Pool:        pg,
			Projects:    escalation.OrgProjectNamer{Svc: orgSvc},
		},
	}
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_trace_evaluator_last_tick_timestamp_seconds",
		"Unix time of the last completed performance regression evaluation pass. Stale value means regression alerts are not being evaluated.",
		nil, evaluator.LastTickUnix)
	selfMetrics.Add(selfmetrics.Gauge, "gotcha_trace_evaluator_tick_duration_seconds",
		"Duration of the last performance regression evaluation pass. Approaching the interval means the evaluator stops keeping up.",
		nil, evaluator.LastTickSeconds)
	go evaluator.Run(ctx)

	metricEval := &metric.Evaluator{
		Rules:     metric.NewRuleService(pg),
		Query:     metric.NewQuery(ch),
		Incidents: metric.NewIncidentService(pg),
		Maint:     maint,
		Policy:    policyStore,
		Pool:      pg,
		// Членство metric-инцидента правила label_key='host' в группе
		// down-корня его хоста.
		IncidentGroups: grouper,
		Notifier: &metric.MetricNotifier{
			Alerts:       alertSvc,
			Outbox:       outbox,
			BaseURL:      cfg.BaseURL,
			EmailEnabled: emailSender.Configured(),
			Details:      detailPolicy(cfg),
			Locale:       i18n.Locale{Code: cfg.Locale},
			// StepNotifier перезагружает инцидент+правило по ID.
			Incidents: metric.NewIncidentService(pg),
			Rules:     metric.NewRuleService(pg),
			Pool:      pg,
			Projects:  escalation.OrgProjectNamer{Svc: orgSvc},
		},
		Interval: time.Duration(cfg.MetricEvalInterval) * time.Second,
	}
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_metric_evaluator_last_tick_timestamp_seconds",
		"Unix time of the last completed metric threshold evaluation pass. Stale value means metric rule alerts are not being evaluated.",
		nil, metricEval.LastTickUnix)
	selfMetrics.Add(selfmetrics.Gauge, "gotcha_metric_evaluator_tick_duration_seconds",
		"Duration of the last metric threshold evaluation pass. Approaching the interval means the evaluator stops keeping up.",
		nil, metricEval.LastTickSeconds)
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_metric_evaluator_skipped_rules",
		"Number of enabled rules skipped in the last tick because the tick budget ran out. Non-zero means the evaluator cannot keep up with the rule count.",
		nil, metricEval.LastTickSkippedRules)
	go metricEval.Run(ctx)

	profileRegEval := &profile.RegressionEvaluator{
		Query:       profile.NewQuery(ch),
		Regressions: profile.NewRegressionService(pg),
		Maint:       maint,
		Policy:      policyStore,
		Pool:        pg,
		Notifier: &profile.RegressionNotifier{
			Alerts:       alertSvc,
			Outbox:       outbox,
			BaseURL:      cfg.BaseURL,
			EmailEnabled: emailSender.Configured(),
			Details:      detailPolicy(cfg),
			Locale:       i18n.Locale{Code: cfg.Locale},
			// StepNotifier перезагружает регрессию по ID.
			Regressions: profile.NewRegressionService(pg),
			Pool:        pg,
			Projects:    escalation.OrgProjectNamer{Svc: orgSvc},
		},
		Interval: time.Duration(cfg.ProfileEvalInterval) * time.Second,
		Config:   profile.DefaultProfileRegressionConfig(),
	}
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_profile_evaluator_last_tick_timestamp_seconds",
		"Unix time of the last completed profile regression evaluation pass. Stale value means profile regression alerts are not being evaluated.",
		nil, profileRegEval.LastTickUnix)
	selfMetrics.Add(selfmetrics.Gauge, "gotcha_profile_evaluator_tick_duration_seconds",
		"Duration of the last profile regression evaluation pass. Approaching the interval means the evaluator stops keeping up.",
		nil, profileRegEval.LastTickSeconds)
	go profileRegEval.Run(ctx)

	hostEval := &host.Evaluator{
		Store:          host.NewStore(pg),
		Settings:       host.NewSettingsService(pg),
		Incidents:      host.NewIncidentService(pg),
		Metrics:        metric.NewQuery(ch),
		Overrides:      host.NewHostOverrideService(pg),
		Groups:         host.NewGroupThresholdService(pg),
		Maint:          maint,
		Policy:         policyStore,
		Pool:           pg,
		Dep:            dep,
		IncidentGroups: grouper,
		Notifier: &host.HostNotifier{
			Alerts:       alertSvc,
			Outbox:       outbox,
			BaseURL:      cfg.BaseURL,
			EmailEnabled: emailSender.Configured(),
			Details:      detailPolicy(cfg),
			Locale:       i18n.Locale{Code: cfg.Locale},
			// StepNotifier перезагружает инцидент/хост/настройки по ID.
			Incidents: host.NewIncidentService(pg),
			Hosts:     host.NewStore(pg),
			Settings:  host.NewSettingsService(pg),
			Overrides: host.NewHostOverrideService(pg),
			Groups:    host.NewGroupThresholdService(pg),
			Pool:      pg,
			// У Retirer-экземпляра (entityJanitor) поле остаётся nil намеренно —
			// он шлёт только retire/close.
			DepCounts: dep,
			Projects:  escalation.OrgProjectNamer{Svc: orgSvc},
		},
		Interval: time.Duration(cfg.HostEvalInterval) * time.Second,
	}
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_host_evaluator_last_tick_timestamp_seconds",
		"Unix time of the last completed host threshold evaluation pass. Stale value means host alerts are not being evaluated.",
		nil, hostEval.LastTickUnix)
	selfMetrics.Add(selfmetrics.Gauge, "gotcha_host_evaluator_tick_duration_seconds",
		"Duration of the last host threshold evaluation pass. Approaching the interval means the evaluator stops keeping up.",
		nil, hostEval.LastTickSeconds)
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_host_evaluator_skipped_hosts",
		"Number of active hosts skipped in the last tick because the tick budget ran out. Non-zero means the evaluator cannot keep up with the host count.",
		nil, hostEval.LastTickSkippedHosts)
	go hostEval.Run(ctx)

	sloNotifier := &slo.SLOBurnNotifier{
		Alerts:       alertSvc,
		Outbox:       outbox,
		BaseURL:      cfg.BaseURL,
		EmailEnabled: emailSender.Configured(),
		Details:      detailPolicy(cfg),
		Locale:       i18n.Locale{Code: cfg.Locale},
		// StepNotifier перезагружает SLO+инцидент по ID.
		Store:    slo.NewStore(pg),
		Pool:     pg,
		Projects: escalation.OrgProjectNamer{Svc: orgSvc},
	}
	sloEval := &slo.Evaluator{
		Pool:           pg,
		Store:          slo.NewStore(pg),
		Providers:      slo.Providers(trace.NewQuery(ch), uptime.NewQuery(ch), uptime.NewService(pg), cfg.RetentionDays),
		Notifier:       sloNotifier,
		Interval:       time.Duration(cfg.SLOEvalInterval) * time.Second,
		Maint:          maint,
		Policy:         policyStore,
		IncidentGroups: grouper,
	}
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_slo_evaluator_last_tick_timestamp_seconds",
		"Unix time of the last completed SLO burn-rate evaluation pass. Stale value means SLO error-budget alerts are not being evaluated.",
		nil, sloEval.LastTickUnix)
	selfMetrics.Add(selfmetrics.Gauge, "gotcha_slo_evaluator_tick_duration_seconds",
		"Duration of the last SLO burn-rate evaluation pass. Approaching the interval means the evaluator stops keeping up.",
		nil, sloEval.LastTickSeconds)
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_slo_evaluator_skipped_slos",
		"Number of enabled SLOs skipped in the last tick because the tick budget ran out. Non-zero means the evaluator cannot keep up with the SLO count.",
		nil, sloEval.LastTickSkippedSLOs)
	go sloEval.Run(ctx)

	// uptime.Service.OpenUnacked не отдаёт планировщику инциденты на
	// escalation_level=0: первую доставку "down" шлёт Detector, иначе она уйдёт дважды.
	uptimeEscNotifier := &uptime.OutboxNotifier{
		Alerts:       alertSvc,
		Uptime:       maint,
		Outbox:       outbox,
		BaseURL:      cfg.BaseURL,
		EmailEnabled: emailSender.Configured(),
		Details:      detailPolicy(cfg),
		Locale:       i18n.Locale{Code: cfg.Locale},
		DepCounts:    dep,
		Projects:     escalation.OrgProjectNamer{Svc: orgSvc},
	}
	bindings := []escalation.Binding{
		{Src: trace.NewRegressionService(pg), Notifier: evaluator.Notifier},
		{Src: metric.NewIncidentService(pg), Notifier: metricEval.Notifier},
		{Src: profile.NewRegressionService(pg), Notifier: profileRegEval.Notifier},
		{Src: host.NewIncidentService(pg), Notifier: hostEval.Notifier},
		{Src: slo.NewStore(pg), Notifier: sloNotifier},
		{Src: maint, Notifier: uptimeEscNotifier},
	}
	sched := &escalation.Scheduler{
		Bindings: bindings,
		Policy:   policyStore,
		Maint:    maint,
		Pool:     pg,
		Interval: time.Duration(cfg.EscalationInterval) * time.Second,
		Now:      time.Now,
		// Host-инцидент, подавленный планировщиком, тем же вызовом попадает в
		// состав группы.
		Dep:         &incidentgroup.DepGate{Dep: dep, Grouper: grouper},
		SettleGrace: settleGrace,
	}
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_escalation_scheduler_last_tick_timestamp_seconds",
		"Unix time of the last completed escalation scheduler pass. Stale value means escalation ladders are not advancing for any of the six incident sources.",
		nil, sched.LastTickUnix)
	selfMetrics.Add(selfmetrics.Gauge, "gotcha_escalation_scheduler_tick_duration_seconds",
		"Duration of the last escalation scheduler pass. Approaching the interval means the scheduler stops keeping up.",
		nil, sched.LastTickSeconds)
	selfMetrics.AddInt(selfmetrics.Gauge, "gotcha_escalation_scheduler_skipped_incidents",
		"Number of open unacknowledged incidents skipped in the last tick because the tick budget ran out. Non-zero means the scheduler cannot keep up with the incident count.",
		nil, sched.LastTickSkippedIncidents)
	go sched.Run(ctx)

	// 0 означает «хранить вечно» — janitor тогда не запускаем.
	if cfg.IncidentRetentionDays > 0 {
		escalationJanitor := &escalation.Janitor{
			Pool:      pg,
			Retention: time.Duration(cfg.IncidentRetentionDays) * 24 * time.Hour,
		}
		go escalationJanitor.Run(ctx)
	}

	// Sweep вечно-открытых групп работает независимо от ретеншена; ретеншен
	// resolved-групп — тем же сроком, что инциденты (0 — хранить вечно).
	groupJanitor := &incidentgroup.Janitor{
		Pool:      pg,
		Retention: time.Duration(cfg.IncidentRetentionDays) * 24 * time.Hour,
	}
	go groupJanitor.Run(ctx)
}

// Решение живёт в одном типе, поле обязано быть заполнено — компилятор не
// даст завести восьмой нотифаер, забыв про гейт.
func detailPolicy(cfg Config) alert.DetailPolicy {
	return alert.NewDetailPolicy(cfg.BaseURL, cfg.TrustedRecipients, cfg.ExternalChannelDetails)
}

// GOTCHA_TRUSTED_RECIPIENTS печатается в ОБЕИХ ветках: разбор списка не
// отказывает старт, так что этот лог — единственная диагностика опечатки.
func logDetailPolicy(cfg Config) {
	switch {
	case cfg.ExternalChannelDetails:
		slog.Info("alert details: sent to every recipient (GOTCHA_EXTERNAL_CHANNEL_DETAILS_ENABLED=true)",
			"trusted_recipients", cfg.TrustedRecipients)
	default:
		slog.Info("alert details: sent only to trusted recipients",
			"instance_host", cfg.BaseURL,
			"trusted_recipients", cfg.TrustedRecipients,
			"hint", "add organization mail/webhook domains via GOTCHA_TRUSTED_RECIPIENTS")
	}
}

// Называет ВСЕ ШЕСТЬ циклов, которые гейтит GOTCHA_EVALUATORS_ENABLED: помимо
// metric/trace/profile/host — ещё slo.Evaluator и escalation.Scheduler.
const evaluatorsDisabledWarning = "metric/performance/profile/host/slo evaluators and the escalation scheduler are NOT running in this mode; " +
	"metric alert rules, regression detection, host thresholds (disk/memory/load/silence), " +
	"SLO error-budget burn alerts and escalation ladders (for ALL incident sources, including uptime) " +
	"will never fire — " +
	"run a --mode=uptime (or --mode=all) replica, or set GOTCHA_EVALUATORS_ENABLED=true here"

// Дефолт — true: runEvaluatorsExplicit требует явного true, чтобы web+uptime
// не гоняли двойную оценку по умолчанию.
func runEvaluators(cfg Config) bool {
	if cfg.RunEvaluators != nil {
		return *cfg.RunEvaluators
	}
	return true
}

func runEvaluatorsExplicit(cfg Config) bool {
	return cfg.RunEvaluators != nil && *cfg.RunEvaluators
}
